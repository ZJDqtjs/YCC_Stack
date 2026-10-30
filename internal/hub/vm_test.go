package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"starstack/internal/ffx"
	"starstack/internal/store"
	"starstack/internal/vm"
)

// addVMNode registers the machine in both the live map and SQLite: payouts go
// through NodeOwner(), which reads the database, so a test that only fills the
// map would silently exercise an unclaimed machine.
func addVMNode(t *testing.T, s *Server, n *Worker) {
	t.Helper()
	s.workers[n.ID] = n
	if err := s.st.RegisterNode(&store.NodeRow{
		ID: n.ID, OwnerID: n.OwnerID, Name: n.Name, Hostname: n.Hostname,
		CPUs: n.CPUs, MemMB: int(n.MemMB), DiskFreeMB: int64(n.DiskFree),
		IPv6: n.IPv6, VM: vmCapsJSON(n.VM), Price: 1, Slots: 1,
		RegisteredAt: time.Now().Unix(), LastSeen: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
}

func vmNode(id, owner string) *Worker {
	return &Worker{
		ID: id, Name: id, OwnerID: owner, Hostname: "host-" + id,
		CPUs: 8, MemMB: 16384, DiskFree: 200000, Slots: 1,
		IPv6: []string{"2001:db8:1::10"},
		VM:   &ffx.VMInventory{Driver: "qemu", Accel: "kvm", KVM: true, MaxVMs: 2, Bridge: ""},
		seen: time.Now(), inbox: make(chan *Claim, 8), vminbox: make(chan *vm.Command, 8),
	}
}

func createVMOffer(t *testing.T, s *Server, seller *http.Cookie, nodeID string, extra map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{
		"node_id": nodeID, "name": "轻量云主机", "image_url": "https://example.com/ubuntu-24.04.qcow2",
		"image_format": "qcow2", "os": "ubuntu 24.04", "cpus": 2, "mem_mb": 2048, "disk_gb": 20,
		"net_mode": "slirp", "host_ipv6": "2001:db8:1::10",
		"ports":      []map[string]any{{"proto": "tcp", "guest_port": 80}},
		"price_hour": 2.0, "max_hours": 24,
	}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/vm/offers", strings.NewReader(string(b)))
	r.AddCookie(seller)
	s.handleVMOffers(rec, r)
	if rec.Code != 200 {
		t.Fatalf("create offer: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Offer map[string]any `json:"offer"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Offer
}

func orderVM(t *testing.T, s *Server, buyer *http.Cookie, offerID string, hours int) (int, map[string]any, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"offer_id": offerID, "name": "my-vm", "hours": hours})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/vm/orders", strings.NewReader(string(b)))
	r.AddCookie(buyer)
	s.handleVMOrder(rec, r)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func reportVM(t *testing.T, s *Server, rep vm.Report) {
	t.Helper()
	b, _ := json.Marshal(rep)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/vm/report", strings.NewReader(string(b)))
	s.handleVMReport(rec, r)
	if rec.Code != 200 {
		t.Fatalf("vm report %s: %d %s", rep.State, rec.Code, rec.Body.String())
	}
}

func TestVMSlirpOrderReportsAndSettles(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	seller := asUser(t, s, "seller")
	buyer := asUser(t, s, "buyer")
	s.st.AddCredits("u-seller", 0)
	s.st.AddCredits("u-buyer", 100)
	addVMNode(t, s, vmNode("node-vm", "u-seller"))

	offer := createVMOffer(t, s, seller, "node-vm", nil)
	code, out, raw := orderVM(t, s, buyer, offer["id"].(string), 1)
	if code != 200 {
		t.Fatalf("order: %d %s", code, raw)
	}
	inst := out["instance"].(map[string]any)
	id := inst["id"].(string)
	if inst["status"] != vm.StatusQueued {
		t.Fatalf("status = %v", inst["status"])
	}
	priv, _ := out["ssh_private_key"].(string)
	if !strings.HasPrefix(priv, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Fatalf("no generated private key: %q", priv)
	}
	root, _ := out["root_password"].(string)
	vnc, _ := out["vnc_password"].(string)
	if len(root) < 12 || len(vnc) < 8 {
		t.Fatalf("short secrets root=%q vnc=%q", root, vnc)
	}
	bal, _ := s.st.Balance("u-buyer")
	if bal != 98 {
		t.Fatalf("balance after hold = %v, want 98 (2 points/hour)", bal)
	}
	var cmd *vm.Command
	select {
	case cmd = <-s.workers["node-vm"].vminbox:
	case <-time.After(time.Second):
		t.Fatal("no start command queued for the agent")
	}
	if cmd.Op != vm.OpStart || cmd.Plan == nil || cmd.Plan.InstanceID != id {
		t.Fatalf("bad command: %+v", cmd)
	}
	if cmd.Plan.RootPassword != root || cmd.Plan.Network.VNCPassword != vnc {
		t.Fatal("agent plan does not carry the same secrets the buyer saw")
	}
	if !strings.Contains(cmd.Plan.SSHAuthorizedKey, "ssh-rsa ") {
		t.Fatalf("plan has no authorized key: %q", cmd.Plan.SSHAuthorizedKey)
	}
	seenSSH := false
	for _, f := range cmd.Plan.Network.Forwards {
		if f.Proto == "tcp" && f.GuestPort == 22 {
			seenSSH = true
		}
	}
	if !seenSSH {
		t.Fatal("plan has no SSH forward")
	}

	reportVM(t, s, vm.Report{Worker: "node-vm", Instance: id, State: "running",
		Endpoints: &vm.Endpoints{IPv6: "2001:db8:1::10", SSH: "[2001:db8:1::10]:20000", SSHUser: "starstack"}})
	s.mu.Lock()
	live := s.vms[id]
	if live.Status != vm.StatusRunning || live.StartedAt == 0 || live.ExpiresAt <= live.StartedAt {
		t.Fatalf("running state not recorded: %+v", live)
	}
	// pretend it ran for half of the prepaid hour
	live.StartedAt = time.Now().Add(-30 * time.Minute).Unix()
	live.ExpiresAt = live.StartedAt + 3600
	s.mu.Unlock()

	reportVM(t, s, vm.Report{Worker: "node-vm", Instance: id, State: "stopped", Cleaned: true})
	s.mu.Lock()
	got := s.vms[id]
	if got.Status != vm.StatusStopped || !got.Cleanup {
		t.Fatalf("not finalized: %+v", got)
	}
	if got.Spent != 1 {
		t.Fatalf("spent = %v, want 1 (half of 2 points/hour)", got.Spent)
	}
	s.mu.Unlock()

	buyerBal, _ := s.st.Balance("u-buyer")
	sellerBal, _ := s.st.Balance("u-seller")
	if buyerBal != 99 {
		t.Fatalf("buyer balance = %v, want 99 (98 + 1 refund)", buyerBal)
	}
	if sellerBal != 0.9 {
		t.Fatalf("seller balance = %v, want 0.9 (90%% of 1)", sellerBal)
	}
}

func TestVMRoutedAllocatesGuestIPv6(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	seller := asUser(t, s, "seller")
	buyer := asUser(t, s, "buyer")
	s.st.AddCredits("u-buyer", 100)
	wd := vmNode("node-routed", "u-seller")
	wd.VM.Bridge = "br0"
	wd.VM.Uplink = "eth0"
	addVMNode(t, s, wd)

	offer := createVMOffer(t, s, seller, "node-routed", map[string]any{
		"net_mode": "routed", "ipv6_prefix": "2001:db8:42::/64", "ipv6_gateway": "2001:db8:42::1",
		"ports": []map[string]any{},
	})
	code, out, raw := orderVM(t, s, buyer, offer["id"].(string), 2)
	if code != 200 {
		t.Fatalf("order: %d %s", code, raw)
	}
	inst := out["instance"].(map[string]any)
	guest, _ := inst["guest_ipv6"].(string)
	if !strings.HasPrefix(guest, "2001:db8:42:") {
		t.Fatalf("guest address %q not inside the prefix", guest)
	}
	if inst["prefix_len"].(float64) != 64 {
		t.Fatalf("prefix_len = %v", inst["prefix_len"])
	}
	ep := inst["endpoints"].(map[string]any)
	if ep["ssh"] != "["+guest+"]:22" || ep["direct"] != true {
		t.Fatalf("routed endpoint = %+v", ep)
	}
	select {
	case cmd := <-s.workers["node-routed"].vminbox:
		if cmd.Plan.Network.Mode != vm.NetRouted || cmd.Plan.Network.Gateway != "2001:db8:42::1" {
			t.Fatalf("plan network = %+v", cmd.Plan.Network)
		}
		if cmd.Plan.Network.GuestIPv6 != guest {
			t.Fatalf("plan guest %q != instance %q", cmd.Plan.Network.GuestIPv6, guest)
		}
	case <-time.After(time.Second):
		t.Fatal("no start command")
	}
}

func TestVMAutoStopsAtPrepaidLimit(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	seller := asUser(t, s, "seller")
	buyer := asUser(t, s, "buyer")
	s.st.AddCredits("u-buyer", 100)
	addVMNode(t, s, vmNode("node-vm", "u-seller"))
	offer := createVMOffer(t, s, seller, "node-vm", nil)

	code, out, raw := orderVM(t, s, buyer, offer["id"].(string), 1)
	if code != 200 {
		t.Fatalf("order: %d %s", code, raw)
	}
	id := out["instance"].(map[string]any)["id"].(string)
	<-s.workers["node-vm"].vminbox // consume start
	reportVM(t, s, vm.Report{Worker: "node-vm", Instance: id, State: "running",
		Endpoints: &vm.Endpoints{IPv6: "2001:db8:1::10", SSH: "[2001:db8:1::10]:20000"}})

	now := time.Now().Unix()
	s.mu.Lock()
	inst := s.vms[id]
	inst.StartedAt, inst.ExpiresAt = now-3600, now
	s.mu.Unlock()
	s.mu.Lock()
	s.vmTickLocked(time.Unix(now, 0))
	got := s.vms[id].Status
	s.mu.Unlock()
	if got != vm.StatusStopping {
		t.Fatalf("status = %s, want stopping after the prepaid hour", got)
	}
	select {
	case cmd := <-s.workers["node-vm"].vminbox:
		if cmd.Op != vm.OpStop {
			t.Fatalf("command = %s, want stop", cmd.Op)
		}
	case <-time.After(time.Second):
		t.Fatal("no stop command was queued at expiry")
	}

	reportVM(t, s, vm.Report{Worker: "node-vm", Instance: id, State: "stopped", Cleaned: true})
	buyerBal, _ := s.st.Balance("u-buyer")
	sellerBal, _ := s.st.Balance("u-seller")
	if buyerBal != 98 {
		t.Fatalf("buyer balance = %v, want 98 (full hour consumed, no refund)", buyerBal)
	}
	if sellerBal != 1.8 {
		t.Fatalf("seller balance = %v, want 1.8", sellerBal)
	}
}

func TestVMRestoreKeepsOfferInstanceAndSecrets(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(dir, "http://127.0.0.1:0", st, DefaultPolicy())
	seller := asUser(t, s, "seller")
	buyer := asUser(t, s, "buyer")
	s.st.AddCredits("u-buyer", 100)
	addVMNode(t, s, vmNode("node-vm", "u-seller"))
	offer := createVMOffer(t, s, seller, "node-vm", nil)
	code, out, raw := orderVM(t, s, buyer, offer["id"].(string), 1)
	if code != 200 {
		t.Fatalf("order: %d %s", code, raw)
	}
	id := out["instance"].(map[string]any)["id"].(string)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.Open(dir + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	s2 := NewServer(dir, "http://127.0.0.1:0", st2, DefaultPolicy())
	if len(s2.voffers) != 1 || len(s2.vms) != 1 {
		t.Fatalf("restored offers=%d instances=%d", len(s2.voffers), len(s2.vms))
	}
	wd := s2.workers["node-vm"]
	if wd == nil || wd.VM == nil || len(wd.IPv6) != 1 {
		t.Fatalf("node capability not restored: %+v", wd)
	}
	inst := s2.vms[id]
	if inst == nil || inst.Status != vm.StatusQueued || inst.BuyerID != "u-buyer" {
		t.Fatalf("instance not restored: %+v", inst)
	}
	plan, err := s2.vmPlanLocked(inst)
	if err != nil {
		t.Fatalf("plan after restore: %v", err)
	}
	if plan.RootPassword == "" || plan.SSHAuthorizedKey == "" || plan.Network.ConsolePort != inst.ConsolePort {
		t.Fatalf("plan lost secrets/allocation: %+v", plan)
	}
}

func TestVMClaimDeliversQueuedCommand(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	addVMNode(t, s, vmNode("node-vm", "u-seller"))
	s.workers["node-vm"].vminbox <- &vm.Command{Op: vm.OpStop, Instance: "vmi-test"}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/vm/claim?worker=node-vm&wait=0.2", nil)
	s.handleVMClaim(rec, r)
	if rec.Code != 200 {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}
	var cmd vm.Command
	if err := json.Unmarshal(rec.Body.Bytes(), &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Op != vm.OpStop || cmd.Instance != "vmi-test" {
		t.Fatalf("command = %+v", cmd)
	}
}

func TestVMOrderGuards(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	seller := asUser(t, s, "seller")
	broke := asUser(t, s, "broke")
	addVMNode(t, s, vmNode("node-vm", "u-seller"))
	offer := createVMOffer(t, s, seller, "node-vm", nil)

	if code, _, _ := orderVM(t, s, broke, offer["id"].(string), 1); code != 402 {
		t.Fatalf("order without credits = %d, want 402", code)
	}
	s.st.AddCredits("u-seller", 100)
	if code, _, _ := orderVM(t, s, seller, offer["id"].(string), 1); code != 403 {
		t.Fatalf("self-rental = %d, want 403", code)
	}

	rich := asUser(t, s, "rich")
	s.st.AddCredits("u-rich", 100)
	fixed := createVMOffer(t, s, seller, "node-vm", map[string]any{
		"name":  "fixed-port",
		"ports": []map[string]any{{"proto": "tcp", "host_port": 22022, "guest_port": 80}},
	})
	if code, _, raw := orderVM(t, s, rich, fixed["id"].(string), 1); code != 200 {
		t.Fatalf("first fixed-port order: %d %s", code, raw)
	}
	if code, _, _ := orderVM(t, s, rich, fixed["id"].(string), 1); code != 409 {
		t.Fatalf("port conflict = %d, want 409", code)
	}
}
