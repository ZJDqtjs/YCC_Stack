package hub

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"starstack/internal/ffx"
	"starstack/internal/store"
	"starstack/internal/vm"
)

const (
	vmPlatformFee      = 0.10
	vmCommandRetry     = 15 * time.Second
	vmHeartbeatGrace   = 90 * time.Second
	vmOfflineGrace     = 5 * time.Minute
	vmProvisionLimit   = 25 * time.Minute
	vmStopGrace        = 10 * time.Minute
	maxVMOffersPerNode = 4
)

// ---- restore / persistence ----

func (s *Server) restoreVM() {
	offers, err := s.st.VMOffers()
	if err != nil {
		log.Printf("restore vm offers: %v", err)
	}
	for _, r := range offers {
		o := &vm.Offer{
			ID: r.ID, NodeID: r.NodeID, OwnerID: r.OwnerID, Name: r.Name,
			ImageURL: r.ImageURL, ImageSHA256: r.ImageSHA256, ImageFormat: r.ImageFormat,
			OS: r.OS, CPUs: r.CPUs, MemMB: r.MemMB, DiskGB: r.DiskGB, NetMode: r.NetMode,
			IPv6Prefix: r.IPv6Prefix, IPv6Gateway: r.IPv6Gateway, HostIPv6: r.HostIPv6,
			PriceHour: r.PriceHour, MaxHours: r.MaxHours, Enabled: r.Enabled, CreatedAt: r.CreatedAt,
		}
		json.Unmarshal([]byte(r.Ports), &o.Ports)
		s.voffers[o.ID] = o
	}
	insts, err := s.st.VMInstances()
	if err != nil {
		log.Printf("restore vm instances: %v", err)
	}
	for _, r := range insts {
		i := &vm.Instance{
			ID: r.ID, OfferID: r.OfferID, NodeID: r.NodeID, BuyerID: r.BuyerID,
			SellerID: r.SellerID, Name: r.Name, Status: r.Status, CPUs: r.CPUs,
			MemMB: r.MemMB, DiskGB: r.DiskGB, ImageURL: r.ImageURL,
			ImageSHA256: r.ImageSHA256, ImageFormat: r.ImageFormat, OS: r.OS,
			NetMode: r.NetMode, GuestIPv6: r.GuestIPv6, PrefixLen: r.PrefixLen,
			Gateway: r.Gateway, HostIPv6: r.HostIPv6, SSHUser: r.SSHUser,
			SSHPort: r.SSHPort, ConsolePort: r.ConsolePort, SSHPublicKey: r.SSHPublicKey,
			RootPasswordEnc: r.RootPasswordEnc, VNCPasswordEnc: r.VNCPasswordEnc,
			PriceHour: r.PriceHour, Hold: r.Hold, Spent: r.Spent, MaxHours: r.MaxHours,
			Progress: r.Progress, Note: r.Note, Err: r.Err, Cleanup: r.Cleanup,
			CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, StoppedAt: r.StoppedAt,
			LastSeen: r.LastSeen, ExpiresAt: r.ExpiresAt, StoppingAt: r.StoppingAt,
		}
		if i.Status == vm.StatusStopping && i.StoppingAt == 0 {
			i.StoppingAt = i.LastSeen
			if i.StoppingAt == 0 {
				i.StoppingAt = i.CreatedAt
			}
		}
		json.Unmarshal([]byte(r.Ports), &i.Ports)
		if r.Endpoints != "" {
			var ep vm.Endpoints
			if json.Unmarshal([]byte(r.Endpoints), &ep) == nil {
				i.Endpoints = &ep
			}
		}
		s.vms[i.ID] = i
	}
}

func (s *Server) saveVMOffer(o *vm.Offer) {
	if err := s.st.InsertVMOffer(&store.VMOfferRow{
		ID: o.ID, NodeID: o.NodeID, OwnerID: o.OwnerID, Name: o.Name,
		ImageURL: o.ImageURL, ImageSHA256: o.ImageSHA256, ImageFormat: o.ImageFormat,
		OS: o.OS, CPUs: o.CPUs, MemMB: o.MemMB, DiskGB: o.DiskGB, NetMode: o.NetMode,
		IPv6Prefix: o.IPv6Prefix, IPv6Gateway: o.IPv6Gateway, HostIPv6: o.HostIPv6,
		Ports: encJSON(o.Ports), PriceHour: o.PriceHour, MaxHours: o.MaxHours,
		Enabled: o.Enabled, CreatedAt: o.CreatedAt,
	}); err != nil {
		log.Printf("save vm offer %s: %v", o.ID, err)
	}
}

func (s *Server) saveVM(i *vm.Instance) {
	if err := s.st.UpdateVMInstance(vmInstanceRow(i)); err != nil {
		log.Printf("save vm instance %s: %v", i.ID, err)
	}
}

func vmCapsJSON(c *ffx.VMInventory) string {
	if c == nil {
		return ""
	}
	return encJSON(c)
}

func encJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// vmWakeNodeLocked re-delivers the commands a node missed while it was away.
// Called on hello, after the agent has proved who it is.
func (s *Server) vmWakeNodeLocked(nodeID string, now time.Time) {
	for _, inst := range s.vms {
		if inst.NodeID != nodeID {
			continue
		}
		switch {
		case inst.Status == vm.StatusQueued || inst.Status == vm.StatusProvisioning:
			s.enqueueVMLocked(inst, vm.OpStart, now)
		case inst.Status == vm.StatusStopping || (inst.StoppedAt != 0 && !inst.Cleanup):
			s.enqueueVMLocked(inst, vm.OpStop, now)
		}
	}
}

func (s *Server) enqueueVMLocked(inst *vm.Instance, op string, now time.Time) {
	wd := s.workers[inst.NodeID]
	if wd == nil || !wd.online() || wd.vminbox == nil {
		return
	}
	if inst.DispatchedAt > 0 && now.Unix()-inst.DispatchedAt < int64(vmCommandRetry/time.Second) {
		return
	}
	cmd := &vm.Command{Op: op, Instance: inst.ID}
	if op == vm.OpStart {
		plan, err := s.vmPlanLocked(inst)
		if err != nil {
			// No VM was ever started, so there is nothing for the agent to clean up.
			s.vmFinalizeLocked(inst, vm.StatusFailed, now.Unix(), "无法生成启动指令，预扣已退还", err.Error(), true)
			return
		}
		cmd.Plan = plan
	}
	select {
	case wd.vminbox <- cmd:
		inst.DispatchedAt = now.Unix()
	default:
		// The agent is not draining; leave DispatchedAt alone so the next tick
		// tries again instead of dropping the command on the floor.
	}
}

func (s *Server) vmPlanLocked(inst *vm.Instance) (*vm.Plan, error) {
	root, err := s.secrets.open(inst.RootPasswordEnc)
	if err != nil {
		return nil, err
	}
	vnc, err := s.secrets.open(inst.VNCPasswordEnc)
	if err != nil {
		return nil, err
	}
	dns := []string{"2606:4700:4700::1111", "2001:4860:4860::8888"}
	if inst.NetMode != vm.NetRouted {
		// slirp hands DNS to the guest through DHCP; this list is only a fallback.
		dns = []string{"1.1.1.1", "2606:4700:4700::1111"}
	}
	return &vm.Plan{
		InstanceID: inst.ID, Name: inst.Name, CPUs: inst.CPUs, MemMB: inst.MemMB,
		DiskGB: inst.DiskGB, ImageURL: inst.ImageURL, ImageSHA256: inst.ImageSHA256,
		ImageFormat: inst.ImageFormat, OS: inst.OS, SSHUser: inst.SSHUser,
		SSHAuthorizedKey: inst.SSHPublicKey, RootPassword: root,
		Hostname: vmHostname(inst.Name, inst.ID), DNS: dns,
		Network: vm.Network{
			Mode: inst.NetMode, GuestIPv6: inst.GuestIPv6, PrefixLen: inst.PrefixLen,
			Gateway: inst.Gateway, HostIPv6: inst.HostIPv6, Forwards: inst.Ports,
			ConsolePort: inst.ConsolePort, VNCPassword: vnc,
		},
	}, nil
}

func vmHostname(name, id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	h := strings.Trim(b.String(), "-")
	if h == "" || h[0] < 'a' || h[0] > 'z' {
		h = "vm-" + h
	}
	if len(h) > 24 {
		h = h[:24]
	}
	suffix := "0000"
	if len(id) >= 4 {
		suffix = id[len(id)-4:]
	}
	return strings.Trim(h, "-") + "-" + suffix
}

// ---- lifecycle / billing ----

func (s *Server) vmTickLocked(now time.Time) {
	for _, inst := range s.vms {
		wd := s.workers[inst.NodeID]
		online := wd != nil && wd.online()

		if inst.StoppedAt != 0 {
			// A broker-side termination still owes the machine a stop command if
			// the agent was offline when it happened.
			if !inst.Cleanup && online {
				s.enqueueVMLocked(inst, vm.OpStop, now)
			}
			continue
		}
		switch inst.Status {
		case vm.StatusQueued, vm.StatusProvisioning:
			if now.Unix()-inst.CreatedAt > int64(vmProvisionLimit/time.Second) {
				s.vmFinalizeLocked(inst, vm.StatusFailed, now.Unix(),
					"启动超时，预扣已全额退还", "卖方机器未在时限内完成启动", false)
				continue
			}
			if online {
				s.enqueueVMLocked(inst, vm.OpStart, now)
			}
		case vm.StatusRunning, vm.StatusUnreachable:
			if inst.ExpiresAt > 0 && now.Unix() >= inst.ExpiresAt {
				inst.Status = vm.StatusStopping
				inst.StoppingAt = now.Unix()
				inst.Note = "预扣时长已用尽，正在自动停机"
				inst.DispatchedAt = 0
				s.saveVM(inst)
				s.enqueueVMLocked(inst, vm.OpStop, now)
				continue
			}
			if online {
				if now.Unix()-inst.LastSeen > int64(vmHeartbeatGrace/time.Second) {
					if inst.Status != vm.StatusUnreachable {
						inst.Status = vm.StatusUnreachable
						inst.Note = "超过 90 秒没有收到卖方机器心跳，计费暂按最后心跳冻结"
						s.saveVM(inst)
					}
					// The node itself can stay online (its transcode poll keeps
					// the node alive) while the VM agent is wedged. Without a
					// second deadline the buyer's hold would stay locked forever.
					if now.Unix()-inst.LastSeen > int64(vmOfflineGrace/time.Second) {
						s.vmFinalizeLocked(inst, vm.StatusFailed, inst.LastSeen,
							"虚拟机心跳中断超过 5 分钟，按最后心跳结算并退还剩余预扣",
							"VM 心跳中断", false)
					}
				} else if inst.Status == vm.StatusUnreachable {
					inst.Status = vm.StatusRunning
					inst.Note = ""
					s.saveVM(inst)
				}
				continue
			}
			if inst.Status != vm.StatusUnreachable {
				inst.Status = vm.StatusUnreachable
				inst.Note = "卖方机器已离线，计费暂按最后心跳冻结"
				s.saveVM(inst)
			}
			if now.Unix()-inst.LastSeen > int64(vmOfflineGrace/time.Second) {
				end := inst.LastSeen
				if end <= 0 {
					end = inst.CreatedAt
				}
				s.vmFinalizeLocked(inst, vm.StatusFailed, end,
					"卖方机器失联，按最后心跳结算并退还剩余预扣", "节点失联超过 5 分钟", false)
			}
		case vm.StatusStopping:
			if online {
				s.enqueueVMLocked(inst, vm.OpStop, now)
			}
			since := inst.StoppingAt
			if since == 0 {
				since = inst.LastSeen
			}
			if since == 0 {
				since = inst.CreatedAt
			}
			if now.Unix()-since > int64(vmStopGrace/time.Second) {
				end := inst.ExpiresAt
				if end <= 0 || end > now.Unix() {
					end = now.Unix()
				}
				s.vmFinalizeLocked(inst, vm.StatusStopped, end,
					"停机确认超时，已按到期时间结算", "", false)
			}
		}
	}
}

// vmFinalizeLocked settles a rental exactly once. The buyer's hold was already
// deducted when the order was placed, so this only refunds what the VM did not
// consume and pays the seller from the consumed part.
func (s *Server) vmFinalizeLocked(inst *vm.Instance, status string, endAt int64, note, errMsg string, cleaned bool) {
	if inst.StoppedAt != 0 {
		return
	}
	if endAt <= 0 {
		endAt = time.Now().Unix()
	}
	if inst.ExpiresAt > 0 && endAt > inst.ExpiresAt {
		endAt = inst.ExpiresAt
	}
	if inst.StartedAt > 0 && endAt < inst.StartedAt {
		endAt = inst.StartedAt
	}
	secs, spent := vm.Billable(inst.PriceHour, inst.StartedAt, endAt, inst.Hold)
	inst.Spent = spent
	inst.Status = status
	inst.StoppedAt = endAt
	inst.Cleanup = cleaned
	inst.Progress = 100
	if note != "" {
		inst.Note = note
	}
	inst.Err = clip(errMsg, 300)

	now := time.Now().Unix()
	if back := round4(inst.Hold - spent); back > 0 && inst.BuyerID != "" {
		if bal, err := s.st.AddCredits(inst.BuyerID, back); err == nil {
			s.st.Ledger(&store.Entry{TS: now, Kind: store.Refund, UserID: inst.BuyerID,
				JobID: inst.ID, Amount: back, Balance: bal,
				Note: "云主机结算退还：" + note})
		} else {
			log.Printf("vm %s refund: %v", inst.ID, err)
		}
	}
	if spent > 0 {
		earn := round4(spent * (1 - vmPlatformFee))
		if owner, err := s.st.NodeOwner(inst.NodeID); err == nil && owner != "" {
			if bal, err := s.st.AddCredits(owner, earn); err == nil {
				s.st.Ledger(&store.Entry{TS: now, Kind: store.Earn, UserID: owner,
					NodeID: inst.NodeID, JobID: inst.ID, Amount: earn, Balance: bal,
					Note: fmt.Sprintf("云主机 %s 提供 %.0f 秒算力（平台费 %.0f%%）",
						inst.Name, secs, vmPlatformFee*100)})
			}
			s.st.AddNodeEarnings(inst.NodeID, secs, earn)
			if wd := s.workers[inst.NodeID]; wd != nil {
				wd.Earned = round4(wd.Earned + earn)
				wd.Billed += secs
			}
		}
	}
	s.saveVM(inst)
	log.Printf("vm %s %s: %.0fs, billed %.3f points (hold %.3f, refund %.3f)",
		inst.ID, status, secs, spent, inst.Hold, round4(inst.Hold-spent))
}

// ---- views ----

func (s *Server) vmPreview(inst *vm.Instance) *vm.Endpoints {
	if inst.Endpoints != nil {
		return inst.Endpoints
	}
	ep := &vm.Endpoints{SSHUser: inst.SSHUser}
	if inst.NetMode == vm.NetRouted {
		ep.IPv6 = inst.GuestIPv6
		ep.SSH = bracketAddr(inst.GuestIPv6, 22)
		ep.Direct = true
		if inst.ConsolePort > 0 {
			ep.VNC = "vnc://" + bracketAddr(inst.HostIPv6, inst.ConsolePort)
		}
		return ep
	}
	ep.IPv6 = inst.HostIPv6
	for _, p := range inst.Ports {
		listen := bracketAddr(inst.HostIPv6, p.HostPort)
		ep.Forwards = append(ep.Forwards, vm.Forward{Proto: p.Proto, Listen: listen, GuestPort: p.GuestPort})
		if p.Proto == "tcp" && p.GuestPort == 22 {
			ep.SSH = listen
		}
	}
	if inst.ConsolePort > 0 {
		ep.VNC = "vnc://" + bracketAddr(inst.HostIPv6, inst.ConsolePort)
	}
	return ep
}

func bracketAddr(host string, port int) string {
	return "[" + strings.Trim(host, "[]") + "]:" + strconv.Itoa(port)
}

func (s *Server) vmViewLocked(inst *vm.Instance, u *store.User) map[string]any {
	v := inst.Snapshot()
	// The hold is billed at the end, but a running VM must not show "0 used"
	// while it is consuming paid time. This is the same formula settlement uses.
	if inst.StartedAt > 0 && inst.StoppedAt == 0 {
		if _, est := vm.Billable(inst.PriceHour, inst.StartedAt, time.Now().Unix(), inst.Hold); est > 0 {
			v["spent"] = est
		}
	}
	nodeName := inst.NodeID
	if wd := s.workers[inst.NodeID]; wd != nil && wd.Name != "" {
		nodeName = wd.Name
	}
	v["node_name"] = nodeName
	v["mine"] = inst.BuyerID == u.ID
	v["seller_mine"] = inst.SellerID == u.ID
	v["endpoints"] = s.vmPreview(inst)
	if inst.ExpiresAt > 0 && inst.Status != vm.StatusStopped && inst.Status != vm.StatusFailed {
		v["remaining_sec"] = inst.ExpiresAt - time.Now().Unix()
	}
	return v
}

func (s *Server) vmOfferViewLocked(o *vm.Offer, u *store.User) map[string]any {
	wd := s.workers[o.NodeID]
	online := wd != nil && wd.online()
	caps := (*ffx.VMInventory)(nil)
	if wd != nil {
		caps = wd.VM
	}
	v := map[string]any{
		"id": o.ID, "node_id": o.NodeID, "name": o.Name, "node_name": o.NodeID,
		"image_url": o.ImageURL, "image_format": o.ImageFormat, "os": o.OS,
		"cpus": o.CPUs, "mem_mb": o.MemMB, "disk_gb": o.DiskGB,
		"net_mode": o.NetMode, "ipv6_prefix": o.IPv6Prefix, "ipv6_gateway": o.IPv6Gateway,
		"host_ipv6": o.HostIPv6, "ports": o.Ports, "price_hour": o.PriceHour,
		"max_hours": o.MaxHours, "enabled": o.Enabled, "created_at": o.CreatedAt,
		"online": online, "available": o.Enabled && online && caps != nil,
		"mine": o.OwnerID == u.ID,
	}
	if caps != nil {
		v["max_vms"] = caps.MaxVMs
		v["accel"] = caps.Accel
	}
	if wd != nil {
		v["node_name"] = wd.Name
		v["node_cpus"] = wd.CPUs
		v["node_mem_mb"] = wd.MemMB
	}
	return v
}

// ---- seller: offers ----

func (s *Server) handleVMOffers(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		s.handleVMCreateOffer(w, r, u)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, jsonErr("只支持 GET / POST"), 405)
		return
	}
	s.mu.Lock()
	out := make([]map[string]any, 0, len(s.voffers))
	for _, o := range s.voffers {
		if !o.Enabled && o.OwnerID != u.ID {
			continue
		}
		out = append(out, s.vmOfferViewLocked(o, u))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		ai, _ := out[i]["available"].(bool)
		aj, _ := out[j]["available"].(bool)
		if ai != aj {
			return ai
		}
		pi, _ := out[i]["price_hour"].(float64)
		pj, _ := out[j]["price_hour"].(float64)
		if pi != pj {
			return pi < pj
		}
		return out[i]["created_at"].(int64) < out[j]["created_at"].(int64)
	})
	writeJSON(w, map[string]any{"offers": out, "now": time.Now().Unix()})
}

func (s *Server) handleVMCreateOffer(w http.ResponseWriter, r *http.Request, u *store.User) {
	var o vm.Offer
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&o); err != nil {
		http.Error(w, jsonErr("请求格式错误"), 400)
		return
	}
	if strings.TrimSpace(o.NodeID) == "" {
		http.Error(w, jsonErr("请选择用于出租的节点"), 400)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wd := s.workers[o.NodeID]
	if wd == nil {
		http.Error(w, jsonErr("找不到这台节点"), 404)
		return
	}
	if wd.OwnerID != u.ID {
		http.Error(w, jsonErr("这台机器不属于你"), 403)
		return
	}
	if wd.VM == nil {
		http.Error(w, jsonErr("这台机器没有启用云主机：请用 --vm 启动 Agent，并确认已安装 qemu"), 409)
		return
	}
	if !wd.online() {
		http.Error(w, jsonErr("请先让这台机器上线，再发布配置（需要当前 IPv6 地址）"), 409)
		return
	}
	if err := o.Normalize(); err != nil {
		http.Error(w, jsonErr(err.Error()), 400)
		return
	}
	if !containsIPv6(wd.IPv6, o.HostIPv6) {
		http.Error(w, jsonErr("卖方 IPv6 必须是这台机器当前上报的地址之一"), 400)
		return
	}
	if o.NetMode == vm.NetRouted && wd.VM.Bridge == "" {
		http.Error(w, jsonErr("routed 模式需要 Agent 以 --vm-bridge <网桥> 启动，这台机器没有配置"), 400)
		return
	}
	if o.CPUs > wd.CPUs {
		http.Error(w, jsonErr(fmt.Sprintf("vCPU 超过机器的 %d 核", wd.CPUs)), 400)
		return
	}
	if o.MemMB > int(wd.MemMB) {
		http.Error(w, jsonErr(fmt.Sprintf("内存超过机器的 %d MiB", wd.MemMB)), 400)
		return
	}
	if wd.DiskFree > 0 && int64(o.DiskGB)<<10 > int64(wd.DiskFree) {
		http.Error(w, jsonErr(fmt.Sprintf("磁盘超过机器当前空闲的 %d MiB", wd.DiskFree)), 400)
		return
	}
	n := 0
	for _, existing := range s.voffers {
		if existing.NodeID == o.NodeID {
			n++
		}
	}
	if n >= maxVMOffersPerNode {
		http.Error(w, jsonErr(fmt.Sprintf("一台机器最多发布 %d 个云主机配置", maxVMOffersPerNode)), 409)
		return
	}
	o.ID = "vmo" + newID()
	o.OwnerID = u.ID
	o.Enabled = true
	o.CreatedAt = time.Now().Unix()
	s.voffers[o.ID] = &o
	s.saveVMOffer(&o)
	view := s.vmOfferViewLocked(&o, u)
	log.Printf("vm offer %s published by %s on node %s: %dC/%dMB/%dGB %.2f pts/h %s",
		o.ID, u.Name, o.NodeID, o.CPUs, o.MemMB, o.DiskGB, o.PriceHour, o.NetMode)
	writeJSON(w, map[string]any{"ok": true, "offer": view})
}

func (s *Server) handleVMOfferOp(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/vm/offers/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, jsonErr("请求格式错误"), 400)
			return
		}
		found, err := s.st.SetVMOfferEnabled(id, u.ID, req.Enabled)
		if err != nil {
			http.Error(w, jsonErr("更新失败"), 500)
			return
		}
		if !found {
			http.Error(w, jsonErr("配置不存在或不属于你"), 404)
			return
		}
		s.mu.Lock()
		if o := s.voffers[id]; o != nil && o.OwnerID == u.ID {
			o.Enabled = req.Enabled
		}
		s.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "id": id, "enabled": req.Enabled})
	case http.MethodDelete:
		found, err := s.st.DeleteVMOffer(id, u.ID)
		if err != nil {
			http.Error(w, jsonErr("删除失败"), 500)
			return
		}
		if !found {
			http.Error(w, jsonErr("配置不存在或不属于你"), 404)
			return
		}
		s.mu.Lock()
		delete(s.voffers, id)
		s.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "id": id,
			"note": "已下架；已经租出的云主机按订单快照继续运行"})
	default:
		http.Error(w, jsonErr("只支持 PUT / DELETE"), 405)
	}
}

// ---- buyer: orders ----

func (s *Server) handleVMOrder(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, jsonErr("只支持 POST"), 405)
		return
	}
	// An order generates an RSA key before it can know whether the offer is
	// valid, so cap attempts per account rather than let a logged-in loop burn
	// the broker's CPU on 2048-bit keygen.
	if !s.limiter.allow("vm-order:"+u.ID, 12) {
		http.Error(w, jsonErr("下单过于频繁，请一分钟后再试"), http.StatusTooManyRequests)
		return
	}
	var req struct {
		OfferID      string `json:"offer_id"`
		Name         string `json:"name"`
		Hours        int    `json:"hours"`
		SSHPublicKey string `json:"ssh_public_key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, jsonErr("请求格式错误"), 400)
		return
	}
	if err := vm.ValidateSSHKey(req.SSHPublicKey); err != nil {
		http.Error(w, jsonErr(err.Error()), 400)
		return
	}
	sshPub, sshPriv := strings.TrimSpace(req.SSHPublicKey), ""
	if sshPub == "" {
		var err error
		sshPub, sshPriv, err = newSSHKeypair()
		if err != nil {
			http.Error(w, jsonErr("生成 SSH 密钥失败"), 500)
			return
		}
	}
	// VNC authentication is DES-based and only uses the first 8 characters;
	// issuing 12 would just invite a mismatch between what we show and what works.
	rootPW, vncPW := vm.NewSecret(20), vm.NewSecret(8)

	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.voffers[req.OfferID]
	if o == nil {
		http.Error(w, jsonErr("配置不存在"), 404)
		return
	}
	if !o.Enabled {
		http.Error(w, jsonErr("这个配置已下架"), 409)
		return
	}
	wd := s.workers[o.NodeID]
	if wd == nil || !wd.online() {
		http.Error(w, jsonErr("卖方机器当前不在线"), 409)
		return
	}
	if wd.VM == nil {
		http.Error(w, jsonErr("卖方机器当前不能启动虚拟机"), 409)
		return
	}
	if !containsIPv6(wd.IPv6, o.HostIPv6) {
		http.Error(w, jsonErr("卖方公网 IPv6 已经变化，请让卖方重新发布这个配置"), 409)
		return
	}
	if o.OwnerID == u.ID {
		http.Error(w, jsonErr("不能租用自己发布的云主机"), 403)
		return
	}
	activeMine := 0
	for _, inst := range s.vms {
		if inst.BuyerID == u.ID && inst.Active() {
			activeMine++
		}
	}
	if activeMine >= s.policy.MaxActiveVMs {
		http.Error(w, jsonErr(fmt.Sprintf("你已有 %d 台云主机在运行，单账户上限 %d 台",
			activeMine, s.policy.MaxActiveVMs)), http.StatusTooManyRequests)
		return
	}
	hours := req.Hours
	if hours <= 0 {
		hours = 1
	}
	if hours > o.MaxHours {
		http.Error(w, jsonErr(fmt.Sprintf("最长可租 %d 小时", o.MaxHours)), 400)
		return
	}
	hold := round4(o.PriceHour * float64(hours))
	if hold < 0.01 {
		http.Error(w, jsonErr("租期金额过小"), 400)
		return
	}
	if bal, err := s.st.Balance(u.ID); err != nil || bal < hold {
		http.Error(w, jsonErr(fmt.Sprintf("算力点不足：本次预扣 %.2f 点", hold)), http.StatusPaymentRequired)
		return
	}

	// Capacity is checked on live instances, not on published offers: a seller
	// may advertise four shapes the machine cannot run at the same time.
	maxVMs := wd.VM.MaxVMs
	if maxVMs <= 0 {
		maxVMs = 2
	}
	count, usedCPU, usedMem, usedDisk, usedGuest, usedHost := s.vmNodeUsageLocked(o.NodeID)
	if count >= maxVMs {
		http.Error(w, jsonErr("这台机器上的虚拟机已满"), 409)
		return
	}
	if usedCPU+o.CPUs > wd.CPUs {
		http.Error(w, jsonErr("这台机器的 vCPU 已被其它租用占满"), 409)
		return
	}
	if usedMem+o.MemMB > int(wd.MemMB) {
		http.Error(w, jsonErr("这台机器的内存已被其它租用占满"), 409)
		return
	}
	if wd.DiskFree > 0 && int64(usedDisk+o.DiskGB)<<10 > int64(wd.DiskFree) {
		http.Error(w, jsonErr("这台机器的磁盘已被其它租用占满"), 409)
		return
	}

	inst := &vm.Instance{
		ID: "vmi" + newID() + newID(), OfferID: o.ID, NodeID: o.NodeID,
		BuyerID: u.ID, SellerID: o.OwnerID, Status: vm.StatusQueued,
		CPUs: o.CPUs, MemMB: o.MemMB, DiskGB: o.DiskGB,
		ImageURL: o.ImageURL, ImageSHA256: o.ImageSHA256, ImageFormat: o.ImageFormat,
		OS: o.OS, NetMode: o.NetMode, HostIPv6: o.HostIPv6, SSHUser: "starstack",
		SSHPublicKey: sshPub, PriceHour: o.PriceHour,
		Hold: hold, MaxHours: o.MaxHours, Note: "已下单，等待卖方机器启动虚拟机",
		CreatedAt: time.Now().Unix(), LastSeen: time.Now().Unix(),
	}
	inst.Name = vm.CleanLabel(req.Name)
	if inst.Name == "" {
		inst.Name = o.Name
	}
	if n := []rune(inst.Name); len(n) > 40 {
		inst.Name = string(n[:40])
	}
	switch o.NetMode {
	case vm.NetRouted:
		guest, plen, err := vm.AllocGuestIPv6(o.IPv6Prefix, inst.ID, o.IPv6Gateway, usedGuest)
		if err != nil {
			http.Error(w, jsonErr("分配 IPv6 地址失败："+err.Error()), 409)
			return
		}
		inst.GuestIPv6, inst.PrefixLen, inst.Gateway = guest, plen, o.IPv6Gateway
		inst.SSHPort = 22
		cp, err := vm.AllocConsolePort(usedHost)
		if err != nil {
			http.Error(w, jsonErr(err.Error()), 409)
			return
		}
		inst.ConsolePort = cp
	default:
		ports, err := vm.AllocHostPorts(o.Ports, usedHost)
		if err != nil {
			http.Error(w, jsonErr(err.Error()), 409)
			return
		}
		inst.Ports = ports
		for _, p := range ports {
			if p.Proto == "tcp" && p.GuestPort == 22 {
				inst.SSHPort = p.HostPort
			}
		}
		// Console port is allocated after the forwards so a seller's explicit
		// host port is never stolen by the VNC listener.
		cp, err := vm.AllocConsolePort(usedHost)
		if err != nil {
			http.Error(w, jsonErr(err.Error()), 409)
			return
		}
		inst.ConsolePort = cp
	}
	rootEnc, err := s.secrets.seal(rootPW)
	if err != nil {
		http.Error(w, jsonErr("保存口令失败"), 500)
		return
	}
	vncEnc, err := s.secrets.seal(vncPW)
	if err != nil {
		http.Error(w, jsonErr("保存控制台口令失败"), 500)
		return
	}
	inst.RootPasswordEnc, inst.VNCPasswordEnc = rootEnc, vncEnc

	if bal, err := s.st.AddCredits(u.ID, -hold); err != nil {
		http.Error(w, jsonErr("预扣失败"), 500)
		return
	} else {
		s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: store.Hold, UserID: u.ID,
			JobID: inst.ID, Amount: -hold, Balance: bal,
			Note: fmt.Sprintf("云主机 %s 预扣 %d 小时（按实际运行秒数结算）", inst.Name, hours)})
	}
	if err := s.st.InsertVMInstance(vmInstanceRow(inst)); err != nil {
		if bal, e := s.st.AddCredits(u.ID, hold); e == nil {
			s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: store.Refund, UserID: u.ID,
				JobID: inst.ID, Amount: hold, Balance: bal, Note: "云主机登记失败，预扣退回"})
		}
		http.Error(w, jsonErr("云主机登记失败"), 500)
		return
	}
	s.vms[inst.ID] = inst
	s.enqueueVMLocked(inst, vm.OpStart, time.Now())
	view := s.vmViewLocked(inst, u)
	log.Printf("vm %s ordered by %s from offer %s on %s: %dC/%dMB/%dGB %.2f pts/h × %dh, hold %.3f",
		inst.ID, u.Name, o.ID, o.NodeID, inst.CPUs, inst.MemMB, inst.DiskGB,
		inst.PriceHour, hours, hold)
	writeJSON(w, map[string]any{
		"ok": true, "instance": view, "ssh_user": inst.SSHUser,
		"ssh_public_key": sshPub, "ssh_private_key": sshPriv,
		"root_password": rootPW, "vnc_password": vncPW,
		"warning": "SSH 私钥、root 口令和控制台口令只显示这一次，请立即保存。",
	})
}

// vmNodeUsageLocked returns what live instances already hold on one machine.
// Ports and guest addresses are the scarce resources here, not just CPU.
func (s *Server) vmNodeUsageLocked(nodeID string) (count, cpus, memMB, diskGB int, guest map[string]bool, host map[int]bool) {
	guest = map[string]bool{}
	host = map[int]bool{}
	for _, inst := range s.vms {
		if inst.NodeID != nodeID || !inst.Active() {
			continue
		}
		count++
		cpus += inst.CPUs
		memMB += inst.MemMB
		diskGB += inst.DiskGB
		if inst.GuestIPv6 != "" {
			guest[inst.GuestIPv6] = true
		}
		for _, p := range inst.Ports {
			if p.HostPort > 0 {
				host[p.HostPort] = true
			}
		}
		if inst.ConsolePort > 0 {
			host[inst.ConsolePort] = true
		}
	}
	return
}

func vmInstanceRow(i *vm.Instance) *store.VMInstanceRow {
	var ep string
	if i.Endpoints != nil {
		ep = encJSON(i.Endpoints)
	}
	return &store.VMInstanceRow{
		ID: i.ID, OfferID: i.OfferID, NodeID: i.NodeID, BuyerID: i.BuyerID,
		SellerID: i.SellerID, Name: i.Name, Status: i.Status, CPUs: i.CPUs,
		MemMB: i.MemMB, DiskGB: i.DiskGB, ImageURL: i.ImageURL,
		ImageSHA256: i.ImageSHA256, ImageFormat: i.ImageFormat, OS: i.OS,
		NetMode: i.NetMode, GuestIPv6: i.GuestIPv6, PrefixLen: i.PrefixLen,
		Gateway: i.Gateway, HostIPv6: i.HostIPv6, Ports: encJSON(i.Ports),
		SSHUser: i.SSHUser, SSHPort: i.SSHPort, ConsolePort: i.ConsolePort,
		SSHPublicKey: i.SSHPublicKey, RootPasswordEnc: i.RootPasswordEnc,
		VNCPasswordEnc: i.VNCPasswordEnc, PriceHour: i.PriceHour, Hold: i.Hold,
		Spent: i.Spent, MaxHours: i.MaxHours, Progress: i.Progress,
		Endpoints: ep, Note: i.Note, Err: i.Err, Cleanup: i.Cleanup,
		CreatedAt: i.CreatedAt, StartedAt: i.StartedAt, StoppedAt: i.StoppedAt,
		LastSeen: i.LastSeen, ExpiresAt: i.ExpiresAt, StoppingAt: i.StoppingAt,
	}
}

// ---- buyer / seller: instances ----

func (s *Server) handleVMInstances(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, jsonErr("只支持 GET"), 405)
		return
	}
	s.mu.Lock()
	var out []map[string]any
	for _, inst := range s.vms {
		if inst.BuyerID != u.ID && inst.SellerID != u.ID {
			continue
		}
		out = append(out, s.vmViewLocked(inst, u))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i]["created_at"].(int64) > out[j]["created_at"].(int64)
	})
	writeJSON(w, map[string]any{"instances": out, "now": time.Now().Unix()})
}

func (s *Server) handleVMInstanceOp(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, jsonErr("只支持 POST"), 405)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/vm/instances/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id, op := parts[0], parts[1]
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.vms[id]
	if inst == nil {
		http.NotFound(w, r)
		return
	}
	if inst.BuyerID != u.ID && inst.SellerID != u.ID {
		http.Error(w, jsonErr("这台云主机不属于你"), 403)
		return
	}
	switch op {
	case "stop":
		if inst.StoppedAt != 0 || inst.Status == vm.StatusStopping {
			writeJSON(w, map[string]any{"ok": true, "instance": s.vmViewLocked(inst, u)})
			return
		}
		inst.Status = vm.StatusStopping
		inst.StoppingAt = time.Now().Unix()
		if inst.BuyerID == u.ID {
			inst.Note = "买方申请停机，正在等待卖方机器确认"
		} else {
			inst.Note = "卖方强制停机，正在等待机器确认"
		}
		inst.DispatchedAt = 0
		s.saveVM(inst)
		s.enqueueVMLocked(inst, vm.OpStop, time.Now())
		writeJSON(w, map[string]any{"ok": true, "instance": s.vmViewLocked(inst, u)})
	case "extend":
		if inst.BuyerID != u.ID {
			http.Error(w, jsonErr("只有买方可以续费"), 403)
			return
		}
		if inst.StoppedAt != 0 || inst.Status == vm.StatusStopping {
			http.Error(w, jsonErr("这台云主机已经停机，不能再续费"), 409)
			return
		}
		var req struct {
			Hours int `json:"hours"`
		}
		if err := decodeJSON(r, &req); err != nil || req.Hours <= 0 {
			http.Error(w, jsonErr("请填写续费小时数"), 400)
			return
		}
		maxHold := round4(inst.PriceHour * float64(inst.MaxHours))
		add := round4(inst.PriceHour * float64(req.Hours))
		if add < 0.01 {
			http.Error(w, jsonErr("续费金额过小"), 400)
			return
		}
		if inst.Hold+add > maxHold+0.0001 {
			http.Error(w, jsonErr(fmt.Sprintf("总租期不能超过 %d 小时", inst.MaxHours)), 400)
			return
		}
		newHold := round4(inst.Hold + add)
		newExpires := int64(0)
		if inst.StartedAt > 0 && inst.PriceHour > 0 {
			newExpires = inst.StartedAt + int64(newHold/inst.PriceHour*3600)
			if newExpires <= time.Now().Unix() {
				// The added hours only cover time already consumed; there is
				// nothing left to run, so do not take the money.
				http.Error(w, jsonErr("这台云主机已经到期，无法再续费"), 409)
				return
			}
		}
		bal, err := s.st.Balance(u.ID)
		if err != nil || bal < add {
			http.Error(w, jsonErr(fmt.Sprintf("算力点不足：续费需要 %.2f 点", add)), http.StatusPaymentRequired)
			return
		}
		if bal, err := s.st.AddCredits(u.ID, -add); err != nil {
			http.Error(w, jsonErr("预扣失败"), 500)
			return
		} else {
			s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: store.Hold, UserID: u.ID,
				JobID: inst.ID, Amount: -add, Balance: bal,
				Note: fmt.Sprintf("云主机 %s 续费 %d 小时", inst.Name, req.Hours)})
		}
		inst.Hold = newHold
		if newExpires > 0 {
			inst.ExpiresAt = newExpires
		}
		s.saveVM(inst)
		writeJSON(w, map[string]any{"ok": true, "instance": s.vmViewLocked(inst, u)})
	default:
		http.NotFound(w, r)
	}
}

// ---- agent channel ----

func (s *Server) handleVMClaim(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("worker")
	s.mu.Lock()
	wd := s.workers[id]
	s.mu.Unlock()
	if wd == nil {
		http.Error(w, "unknown worker, call /api/worker/hello first", 403)
		return
	}
	if !s.agentAuthed(r, id) {
		http.Error(w, jsonErr("节点凭据无效"), 403)
		return
	}
	s.mu.Lock()
	wd.seen = time.Now()
	s.mu.Unlock()

	wait := 20 * time.Second
	if v := r.URL.Query().Get("wait"); v != "" {
		if sec, err := strconv.ParseFloat(v, 64); err == nil && sec > 0 {
			if sec > 60 {
				sec = 60
			}
			wait = time.Duration(sec * float64(time.Second))
		}
	}
	select {
	case cmd := <-wd.vminbox:
		writeJSON(w, cmd)
	case <-time.After(wait):
		w.WriteHeader(204)
	case <-r.Context().Done():
	}
}

func (s *Server) handleVMReport(w http.ResponseWriter, r *http.Request) {
	var rep vm.Report
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rep); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !s.agentAuthed(r, rep.Worker) {
		http.Error(w, jsonErr("节点凭据无效"), 403)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wd := s.workers[rep.Worker]
	if wd != nil {
		wd.seen = time.Now()
	}
	inst := s.vms[rep.Instance]
	if inst == nil || inst.NodeID != rep.Worker {
		http.Error(w, "no such vm on this worker", 404)
		return
	}
	now := time.Now().Unix()
	inst.LastSeen = now
	switch rep.State {
	case "downloading", "provisioning":
		if inst.Status != vm.StatusStopping {
			inst.Status = vm.StatusProvisioning
		}
		if rep.Pct > 0 {
			inst.Progress = rep.Pct
			if inst.Progress > 99 {
				inst.Progress = 99
			}
		}
		if rep.Msg != "" {
			inst.Note = clip(rep.Msg, 300)
		}
		inst.Err = ""
		// A progress report is proof the agent is working; do not queue another
		// start behind the one it is already executing.
		inst.DispatchedAt = now
		s.saveVM(inst)
	case "running":
		inst.Endpoints = rep.Endpoints
		if inst.StoppedAt != 0 {
			// The broker already settled this instance but the agent still says
			// it is alive. That is exactly the case where a lost stop command
			// leaves a VM running for free, so ask again.
			if inst.Cleanup {
				inst.Cleanup = false
				s.saveVM(inst)
			}
			inst.DispatchedAt = 0
			s.enqueueVMLocked(inst, vm.OpStop, time.Now())
			writeJSON(w, map[string]any{"ok": true, "ignored": "stopped"})
			return
		}
		if inst.Status == vm.StatusStopping {
			// A stop was already ordered; a late heartbeat must not un-stop it.
			s.saveVM(inst)
			writeJSON(w, map[string]any{"ok": true, "ignored": "stopping"})
			return
		}
		inst.Status = vm.StatusRunning
		inst.Progress = 100
		inst.Err = ""
		inst.Note = ""
		if inst.StartedAt == 0 {
			inst.StartedAt = now
			if inst.PriceHour > 0 {
				inst.ExpiresAt = now + int64(inst.Hold/inst.PriceHour*3600)
			}
		}
		if inst.ExpiresAt > 0 && now >= inst.ExpiresAt {
			inst.Status = vm.StatusStopping
			inst.StoppingAt = now
			inst.Note = "预扣时长已用尽，正在自动停机"
			inst.DispatchedAt = 0
			s.saveVM(inst)
			s.enqueueVMLocked(inst, vm.OpStop, time.Now())
			writeJSON(w, map[string]any{"ok": true, "stopping": true})
			return
		}
		s.saveVM(inst)
		log.Printf("vm %s running on %s: %+v", inst.ID, inst.NodeID, rep.Endpoints)
	case "stopping":
		if inst.StoppedAt == 0 {
			inst.Status = vm.StatusStopping
			if inst.StoppingAt == 0 {
				inst.StoppingAt = now
			}
			s.saveVM(inst)
		}
	case "stopped":
		if inst.StoppedAt != 0 {
			// The broker already settled this one (e.g. while the agent was
			// offline). Trust the agent's own cleanup flag: if it could not
			// remove the disk, vmTick will keep retrying the stop.
			inst.Cleanup = rep.Cleaned
			s.saveVM(inst)
			break
		}
		s.vmFinalizeLocked(inst, vm.StatusStopped, now, "已停机，按实际运行秒数结算", rep.Err, rep.Cleaned)
	case "failed":
		if inst.StoppedAt != 0 {
			inst.Cleanup = rep.Cleaned
			s.saveVM(inst)
			break
		}
		s.vmFinalizeLocked(inst, vm.StatusFailed, now, "启动或运行失败，剩余预扣已退还", rep.Err, rep.Cleaned)
	default:
		http.Error(w, jsonErr("未知状态"), 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- SSH key generation ----

func newSSHKeypair() (pub, priv string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	var blob bytes.Buffer
	writeSSHString(&blob, []byte("ssh-rsa"))
	writeSSHMPInt(&blob, big.NewInt(int64(key.PublicKey.E)))
	writeSSHMPInt(&blob, key.PublicKey.N)
	pub = "ssh-rsa " + base64.StdEncoding.EncodeToString(blob.Bytes()) + " starstack"
	priv = string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	return pub, priv, nil
}

func writeSSHString(b *bytes.Buffer, v []byte) {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(v)))
	b.Write(l[:])
	b.Write(v)
}

func writeSSHMPInt(b *bytes.Buffer, n *big.Int) {
	v := n.Bytes()
	if len(v) > 0 && v[0]&0x80 != 0 {
		v = append([]byte{0}, v...)
	}
	writeSSHString(b, v)
}

// containsIPv6 compares canonical forms, so a seller typing the same address in
// another legal spelling is not rejected.
func containsIPv6(list []string, want string) bool {
	w, err := vm.ParseGlobalIPv6(want)
	if err != nil {
		return false
	}
	for _, s := range list {
		a, err := vm.ParseGlobalIPv6(s)
		if err == nil && a == w {
			return true
		}
	}
	return false
}
