// Package vmhost runs the provider side of the IPv6 cloud-host market: it
// downloads the seller's image once, builds a cloud-init seed, starts QEMU with
// a separate disk overlay per tenant, and exposes the guest over the seller's
// IPv6 address through a loopback proxy.
package vmhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"starstack/internal/vm"
)

const (
	maxImageBytes = int64(32) << 30
	imageTTL      = 7 * 24 * time.Hour
)

type Progress func(pct float64, msg string)

type Host struct {
	Dir     string
	QEMU    string
	QEMUImg string
	Bridge  string
	Uplink  string
	Accel   string
	MaxVMs  int

	mu       sync.Mutex
	forwards map[string]*forwarder
	imgLocks sync.Map
}

func New(dir, qemu, qemuImg, bridge, uplink, accel string, maxVMs int) *Host {
	if maxVMs <= 0 {
		maxVMs = 2
	}
	return &Host{
		Dir: dir, QEMU: qemu, QEMUImg: qemuImg, Bridge: bridge,
		Uplink: uplink, Accel: accel, MaxVMs: maxVMs,
		forwards: map[string]*forwarder{},
	}
}

type State struct {
	Plan      *vm.Plan      `json:"plan"`
	PID       int           `json:"pid"`
	QMPPort   int           `json:"qmp_port"`
	Tap       string        `json:"tap,omitempty"`
	Endpoints *vm.Endpoints `json:"endpoints"`
	StartedAt int64         `json:"started_at"`
}

func (h *Host) instanceDir(id string) string { return filepath.Join(h.Dir, id) }
func (h *Host) statePath(id string) string   { return filepath.Join(h.instanceDir(id), "state.json") }

// Start is idempotent: the broker retries provisioning commands until the
// agent reports running, so a second start must reattach instead of building a
// second overlay on top of the same instance directory.
func (h *Host) Start(ctx context.Context, plan *vm.Plan, progress Progress) (*vm.Endpoints, error) {
	if plan == nil || !safeID(plan.InstanceID) {
		return nil, errors.New("虚拟机实例 ID 不合法")
	}
	p := func(pct float64, msg string) {
		if progress != nil {
			progress(pct, msg)
		}
	}
	dir := h.instanceDir(plan.InstanceID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	if st, err := h.loadState(plan.InstanceID); err == nil {
		if ep, running, _ := h.statusState(ctx, st); running {
			if err := h.ensureForwarder(st); err != nil {
				return nil, err
			}
			return ep, nil
		}
		if fileExists(filepath.Join(dir, "disk.qcow2")) {
			p(60, "复用已有磁盘，重新启动虚拟机")
			h.cleanupStateNet(st)
			return h.boot(ctx, plan, dir, "", true, p)
		}
	}

	base := ""
	if plan.ImageFormat == "iso" {
		p(2, "下载安装镜像")
		var err error
		base, err = h.fetchImage(ctx, plan, func(pct float64) {
			p(2+pct*38, fmt.Sprintf("下载安装镜像 %.0f%%", pct*100))
		})
		if err != nil {
			return nil, err
		}
	} else {
		p(2, "下载系统镜像")
		var err error
		base, err = h.fetchImage(ctx, plan, func(pct float64) {
			p(2+pct*50, fmt.Sprintf("下载系统镜像 %.0f%%", pct*100))
		})
		if err != nil {
			return nil, err
		}
	}

	p(55, "创建差量磁盘")
	if err := h.createDisk(ctx, plan, base); err != nil {
		return nil, err
	}
	p(70, "生成 cloud-init 种子盘")
	seed, err := BuildSeed(plan)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.iso"), seed, 0o600); err != nil {
		return nil, err
	}
	p(80, "配置 IPv6 网络")
	tap := ""
	if plan.Network.Mode == vm.NetRouted {
		tap, err = h.setupRouted(ctx, plan)
		if err != nil {
			return nil, err
		}
	}
	p(88, "启动 QEMU")
	ep, err := h.boot(ctx, plan, dir, tap, false, p)
	if err != nil {
		h.cleanupRouted(plan, tap)
		return nil, err
	}
	p(100, "虚拟机已启动")
	return ep, nil
}

func (h *Host) createDisk(ctx context.Context, plan *vm.Plan, base string) error {
	disk := filepath.Join(h.instanceDir(plan.InstanceID), "disk.qcow2")
	os.Remove(disk)
	size := strconv.Itoa(plan.DiskGB) + "G"
	args := []string{"create", "-q", "-f", "qcow2"}
	if plan.ImageFormat != "iso" {
		args = append(args, "-F", plan.ImageFormat, "-b", base)
	}
	args = append(args, disk, size)
	c, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(c, h.QEMUImg, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("创建磁盘失败：%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (h *Host) boot(ctx context.Context, plan *vm.Plan, dir, tap string, reuse bool, p Progress) (*vm.Endpoints, error) {
	if plan.Network.ConsolePort < 5900 || plan.Network.ConsolePort > 5999 {
		return nil, fmt.Errorf("控制台端口 %d 不在 5900-5999 范围内", plan.Network.ConsolePort)
	}
	createdTap := false
	if plan.Network.Mode == vm.NetRouted && tap == "" {
		var err error
		tap, err = h.setupRouted(ctx, plan)
		if err != nil {
			return nil, err
		}
		createdTap = true
	}
	success := false
	defer func() {
		if !success && createdTap {
			h.cleanupRouted(plan, tap)
		}
	}()
	qmpPort, err := freePort()
	if err != nil {
		return nil, err
	}
	base := ""
	if plan.ImageFormat == "iso" && !reuse {
		base, _ = h.imagePath(plan)
	}
	args := h.qemuArgs(plan, dir, tap, base, reuse, qmpPort)
	logPath := filepath.Join(dir, "qemu.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(h.QEMU, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	p(90, "等待 QEMU 就绪")
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("启动 qemu：%w", err)
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()
	logf.Close()

	var q *qmpConn
	deadline := time.Now().Add(30 * time.Second)
	for {
		q, err = dialQMP(qmpPort)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			killProcess(pid)
			return nil, fmt.Errorf("QEMU 没有就绪：%v；日志：%s", err, tailFile(logPath, 1200))
		}
		select {
		case <-ctx.Done():
			killProcess(pid)
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	defer q.Close()
	if _, err := q.Cmd("qmp_capabilities", nil); err != nil {
		killProcess(pid)
		return nil, fmt.Errorf("QMP 握手失败：%w", err)
	}
	if plan.Network.VNCPassword == "" {
		killProcess(pid)
		return nil, errors.New("控制台口令为空，拒绝启动无口令 VNC")
	}
	if _, err := q.Cmd("set_password", map[string]any{
		"protocol": "vnc", "password": plan.Network.VNCPassword,
	}); err != nil {
		killProcess(pid)
		return nil, fmt.Errorf("设置 VNC 口令失败：%w", err)
	}
	p(95, "等待虚拟机进入运行状态")
	statusDeadline := time.Now().Add(20 * time.Second)
	for {
		stt, err := q.status()
		if err == nil && (stt == "running" || stt == "paused") {
			break
		}
		if time.Now().After(statusDeadline) {
			q.Cmd("quit", nil)
			killProcess(pid)
			return nil, fmt.Errorf("虚拟机没有进入 running 状态（当前 %q）", stt)
		}
		time.Sleep(300 * time.Millisecond)
	}

	ep := endpointsFor(plan)
	st := &State{Plan: plan, PID: pid, QMPPort: qmpPort, Tap: tap, Endpoints: ep, StartedAt: time.Now().Unix()}
	if err := h.saveState(st); err != nil {
		q.Cmd("quit", nil)
		killProcess(pid)
		return nil, err
	}
	if err := h.ensureForwarder(st); err != nil {
		q.Cmd("quit", nil)
		killProcess(pid)
		h.cleanupStateNet(st)
		return nil, err
	}
	success = true
	return ep, nil
}

func (h *Host) qemuArgs(plan *vm.Plan, dir, tap, base string, reuse bool, qmpPort int) []string {
	disk := filepath.Join(dir, "disk.qcow2")
	seed := filepath.Join(dir, "seed.iso")
	args := []string{
		"-name", "starstack-" + plan.InstanceID,
		"-machine", "q35",
		"-accel", h.Accel,
		"-smp", strconv.Itoa(plan.CPUs),
		"-m", strconv.Itoa(plan.MemMB),
		"-drive", "file=" + disk + ",if=virtio,format=qcow2,cache=writeback",
		"-display", "none",
		"-monitor", "none",
		"-vga", "std",
		"-qmp", fmt.Sprintf("tcp:127.0.0.1:%d,server=on,wait=off", qmpPort),
		"-vnc", fmt.Sprintf("127.0.0.1:%d,password=on", plan.Network.ConsolePort-5900),
		"-serial", "file:" + filepath.Join(dir, "console.log"),
		"-pidfile", filepath.Join(dir, "qemu.pid"),
	}
	if h.Accel == "kvm" || h.Accel == "hvf" {
		args = append(args, "-cpu", "host")
	} else {
		args = append(args, "-cpu", "max")
	}
	if plan.ImageFormat == "iso" {
		if !reuse && base != "" {
			args = append(args, "-drive", "file="+base+",media=cdrom,readonly=on")
		}
		args = append(args, "-drive", "file="+seed+",media=cdrom,readonly=on")
		// Disk first, installer second: an empty disk falls through to the
		// installer, and after the buyer finishes installing the same VM boots
		// the installed system instead of reinstalling on every reset.
		args = append(args, "-boot", "order=cd,menu=off")
	} else {
		args = append(args, "-drive", "file="+seed+",media=cdrom,readonly=on")
		args = append(args, "-boot", "order=c,menu=off")
	}
	if plan.Network.Mode == vm.NetRouted {
		args = append(args, "-netdev", "tap,id=net0,ifname="+tap+",script=no,downscript=no")
	} else {
		nd := "user,id=net0,ipv6=off"
		for _, f := range plan.Network.Forwards {
			nd += fmt.Sprintf(",hostfwd=%s:127.0.0.1:%d-:%d", f.Proto, f.HostPort, f.GuestPort)
		}
		args = append(args, "-netdev", nd)
	}
	args = append(args, "-device", "virtio-net-pci,netdev=net0")
	return args
}

func endpointsFor(plan *vm.Plan) *vm.Endpoints {
	ep := &vm.Endpoints{SSHUser: plan.SSHUser}
	if plan.Network.Mode == vm.NetRouted {
		ep.IPv6 = plan.Network.GuestIPv6
		ep.SSH = bracket(plan.Network.GuestIPv6, 22)
		ep.Direct = true
		ep.VNC = "vnc://" + bracket(plan.Network.HostIPv6, plan.Network.ConsolePort)
		return ep
	}
	ep.IPv6 = plan.Network.HostIPv6
	for _, p := range plan.Network.Forwards {
		listen := bracket(plan.Network.HostIPv6, p.HostPort)
		ep.Forwards = append(ep.Forwards, vm.Forward{Proto: p.Proto, Listen: listen, GuestPort: p.GuestPort})
		if p.Proto == "tcp" && p.GuestPort == 22 {
			ep.SSH = listen
		}
	}
	if plan.Network.ConsolePort > 0 {
		ep.VNC = "vnc://" + bracket(plan.Network.HostIPv6, plan.Network.ConsolePort)
	}
	return ep
}

func bracket(host string, port int) string {
	return "[" + strings.Trim(host, "[]") + "]:" + strconv.Itoa(port)
}

func (h *Host) ensureForwarder(st *State) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := st.Plan.InstanceID
	if h.forwards[id] != nil {
		return nil
	}
	f, err := startForwarder(st.Plan)
	if err != nil {
		return fmt.Errorf("IPv6 端口转发启动失败（端口可能被占用）：%w", err)
	}
	h.forwards[id] = f
	return nil
}

func (h *Host) closeForwarder(id string) {
	h.mu.Lock()
	f := h.forwards[id]
	delete(h.forwards, id)
	h.mu.Unlock()
	if f != nil {
		f.Close()
	}
}

func (h *Host) statusState(ctx context.Context, st *State) (*vm.Endpoints, bool, error) {
	q, err := dialQMP(st.QMPPort)
	if err != nil {
		return st.Endpoints, false, nil
	}
	defer q.Close()
	if _, err := q.Cmd("qmp_capabilities", nil); err != nil {
		return st.Endpoints, false, nil
	}
	status, err := q.status()
	if err != nil {
		return st.Endpoints, false, nil
	}
	running := status == "running" || status == "paused"
	return st.Endpoints, running, nil
}

// Status is the heartbeat view: running is decided by QEMU itself, not by a
// file that could be stale after a crash.
func (h *Host) Status(ctx context.Context, id string) (*vm.Endpoints, bool, error) {
	if !safeID(id) {
		return nil, false, errors.New("虚拟机实例 ID 不合法")
	}
	st, err := h.loadState(id)
	if err != nil {
		return nil, false, err
	}
	ep, running, err := h.statusState(ctx, st)
	if err != nil {
		return st.Endpoints, false, nil
	}
	if running {
		if err := h.ensureForwarder(st); err != nil {
			return st.Endpoints, false, err
		}
	}
	return ep, running, nil
}

func (h *Host) Stop(ctx context.Context, id string) error {
	if !safeID(id) {
		return errors.New("虚拟机实例 ID 不合法")
	}
	st, err := h.loadState(id)
	if err != nil {
		// The state file can be lost while QEMU is still alive (disk error,
		// manual cleanup). Fall back to the pidfile so a stop still stops.
		if pid := readPID(filepath.Join(h.instanceDir(id), "qemu.pid")); pid > 0 {
			killProcess(pid)
		}
		h.closeForwarder(id)
		os.RemoveAll(h.instanceDir(id))
		return nil
	}
	if q, err := dialQMP(st.QMPPort); err == nil {
		q.Cmd("qmp_capabilities", nil)
		q.Cmd("system_powerdown", nil)
		deadline := time.Now().Add(20 * time.Second)
	wait:
		for time.Now().Before(deadline) {
			status, err := q.status()
			if err != nil || status == "shutdown" || status == "prelaunch" {
				break
			}
			select {
			case <-ctx.Done():
				break wait
			case <-time.After(500 * time.Millisecond):
			}
		}
		q.Cmd("quit", nil)
		q.Close()
	}
	killProcess(st.PID)
	h.closeForwarder(id)
	h.cleanupStateNet(st)
	dir := h.instanceDir(id)
	for i := 0; i < 10; i++ {
		if err := os.RemoveAll(dir); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// List returns instances this machine knows it was running. It is how an agent
// restart re-adopts VMs instead of waiting for the broker to ask again.
func (h *Host) List() []*State {
	entries, err := os.ReadDir(h.Dir)
	if err != nil {
		return nil
	}
	var out []*State
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := h.loadState(e.Name())
		if err == nil {
			out = append(out, st)
		}
	}
	return out
}

func (h *Host) loadState(id string) (*State, error) {
	if !safeID(id) {
		return nil, errors.New("虚拟机实例 ID 不合法")
	}
	b, err := os.ReadFile(h.statePath(id))
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	if st.Plan == nil {
		return nil, errors.New("状态文件损坏")
	}
	return &st, nil
}

func (h *Host) saveState(st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := h.statePath(st.Plan.InstanceID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.statePath(st.Plan.InstanceID))
}

// ---- image cache ----

func (h *Host) imagePath(plan *vm.Plan) (string, error) {
	key := plan.ImageSHA256
	if key == "" {
		sum := sha256.Sum256([]byte(plan.ImageURL + "|" + plan.ImageFormat))
		key = hex.EncodeToString(sum[:])[:32]
	}
	return filepath.Join(h.Dir, "images", key+"."+plan.ImageFormat), nil
}

func (h *Host) fetchImage(ctx context.Context, plan *vm.Plan, progress func(float64)) (string, error) {
	dst, err := h.imagePath(plan)
	if err != nil {
		return "", err
	}
	if fileExists(dst) {
		if plan.ImageSHA256 == "" || fileExists(dst+".ok") {
			return dst, nil
		}
		f, err := os.Open(dst)
		if err == nil {
			hsh := sha256.New()
			_, err = io.Copy(hsh, f)
			f.Close()
			if err == nil && hex.EncodeToString(hsh.Sum(nil)) == plan.ImageSHA256 {
				os.WriteFile(dst+".ok", []byte(plan.ImageSHA256), 0o600)
				return dst, nil
			}
			os.Remove(dst)
		}
	}
	if !strings.HasPrefix(plan.ImageURL, "http://") && !strings.HasPrefix(plan.ImageURL, "https://") {
		return "", errors.New("镜像地址必须是 http(s)")
	}

	// One download per image even when two tenants start at the same time.
	lk, _ := h.imgLocks.LoadOrStore(dst, &sync.Mutex{})
	mu := lk.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if fileExists(dst) {
		return dst, nil
	}
	os.MkdirAll(filepath.Dir(dst), 0o700)
	req, err := http.NewRequestWithContext(ctx, "GET", plan.ImageURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
	}}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("下载镜像：HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxImageBytes {
		return "", fmt.Errorf("镜像超过 32 GiB 上限")
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	hsh := sha256.New()
	var total int64
	last := time.Now()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if total+int64(n) > maxImageBytes {
				f.Close()
				os.Remove(tmp)
				return "", errors.New("镜像超过 32 GiB 上限")
			}
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				os.Remove(tmp)
				return "", err
			}
			hsh.Write(buf[:n])
			total += int64(n)
			if progress != nil && time.Since(last) > 400*time.Millisecond {
				last = time.Now()
				pct := 0.0
				if resp.ContentLength > 0 {
					pct = float64(total) / float64(resp.ContentLength)
				}
				progress(pct)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return "", rerr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	sum := hex.EncodeToString(hsh.Sum(nil))
	if plan.ImageSHA256 != "" && sum != plan.ImageSHA256 {
		os.Remove(tmp)
		return "", fmt.Errorf("镜像 SHA-256 不匹配：期望 %s，实际 %s", plan.ImageSHA256, sum)
	}
	os.Remove(dst)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if plan.ImageSHA256 != "" {
		os.WriteFile(dst+".ok", []byte(plan.ImageSHA256), 0o600)
	}
	if progress != nil {
		progress(1)
	}
	return dst, nil
}

// ---- helpers ----

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func tailFile(p string, n int) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, _ := f.Stat()
	off := int64(0)
	if st != nil && st.Size() > int64(n) {
		off = st.Size() - int64(n)
	}
	b := make([]byte, n)
	f.Seek(off, io.SeekStart)
	got, _ := f.Read(b)
	return strings.TrimSpace(string(b[:got]))
}

func readPID(p string) int {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func safeID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// CleanupImages drops image cache entries older than imageTTL. Agents are
// expected to run for weeks on a home machine; a cache that only grows would
// eventually fill the disk the VMs live on.
func (h *Host) CleanupImages() {
	dir := filepath.Join(h.Dir, "images")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// An overlay disk cannot be booted again without its backing image, so any
	// image referenced by a known instance is off limits no matter how old.
	inUse := map[string]bool{}
	for _, st := range h.List() {
		if p, err := h.imagePath(st.Plan); err == nil {
			inUse[filepath.Base(p)] = true
		}
	}
	cutoff := time.Now().Add(-imageTTL)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".part") {
			if time.Since(info.ModTime()) > time.Hour {
				os.Remove(filepath.Join(dir, name))
			}
			continue
		}
		if strings.HasSuffix(name, ".ok") || inUse[name] {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		os.Remove(filepath.Join(dir, name))
		os.Remove(filepath.Join(dir, name+".ok"))
	}
}
