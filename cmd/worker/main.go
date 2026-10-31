package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"starstack/internal/ffx"
	"starstack/internal/hub"
	"starstack/internal/render"
	"starstack/internal/vmhost"
)

func main() {
	server := flag.String("server", "http://127.0.0.1:8787", "platform server base URL")
	id := flag.String("id", "", "stable node id (default: hostname-pid)")
	name := flag.String("name", "", "display name shown in the compute market (default: hostname)")
	slots := flag.Int("slots", 0, "concurrent segment slots (0 = auto)")
	price := flag.Float64("price", 1.0, "算力点 / 视频秒 the provider asks for")
	work := flag.String("work", "", "cache/work directory (default: ./worker-cache)")
	force := flag.String("encoder", "", "restrict to one encoder, e.g. h264_nvenc")
	ffmpeg := flag.String("ffmpeg", "ffmpeg", "ffmpeg binary")
	ffprobe := flag.String("ffprobe", "ffprobe", "ffprobe binary")
	blender := flag.String("blender", "blender", "blender binary (full path when it is not on PATH)")
	token := flag.String("token", "", "agent token a public broker requires at registration")
	launch := flag.String("launch", "", "one-use launch code from the web console; "+
		"the node is bound to the account that generated it and no token is needed")
	cacheGB := flag.Float64("cache-gb", 20, "keep cached source files under this much disk")
	vmOn := flag.Bool("vm", false, "host IPv6 virtual machines for the 云主机 market (requires qemu-system-x86_64 and qemu-img)")
	qemu := flag.String("qemu", "qemu-system-x86_64", "qemu system emulator binary")
	qemuImg := flag.String("qemu-img", "qemu-img", "qemu-img binary")
	vmBridge := flag.String("vm-bridge", "", "Linux bridge for routed IPv6 guests (e.g. br0); empty means slirp only")
	vmUplink := flag.String("vm-uplink", "", "uplink interface for IPv6 NDP proxy (default: the bridge's uplink)")
	vmMax := flag.Int("vm-max", 2, "maximum concurrent VMs this machine will host")
	vmDir := flag.String("vm-dir", "", "VM disk/image directory (default <work>/vms)")
	allowBuild := flag.Bool("allow-build", false, "accept compile/packaging jobs: the platform "+
		"will run buyer-supplied commands as this user. Enable only on a machine and platform "+
		"account you trust")
	autoProvision := flag.Bool("auto-provision", false, "install missing toolchains automatically "+
		"(currently the .NET SDK for build jobs) instead of failing the task")
	configPath := flag.String("config", "", "node config JSON (default: starstack-node.json next to the executable)")
	flag.Parse()

	// The bundle drops starstack-node.json next to the exe, so double-clicking
	// the worker is enough to join. Flags given on the command line win.
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if cfg, path := loadNodeConfig(*configPath, *work); cfg != nil {
		if !explicit["server"] && cfg.Server != "" {
			*server = cfg.Server
		}
		if !explicit["id"] && cfg.ID != "" {
			*id = cfg.ID
		}
		if !explicit["name"] && cfg.Name != "" {
			*name = cfg.Name
		}
		if !explicit["price"] && cfg.Price != nil {
			*price = *cfg.Price
		}
		if !explicit["slots"] && cfg.Slots != nil {
			*slots = *cfg.Slots
		}
		if !explicit["launch"] && cfg.Launch != "" {
			*launch = cfg.Launch
		}
		if !explicit["token"] && cfg.Token != "" {
			*token = cfg.Token
		}
		if !explicit["work"] && cfg.Work != "" {
			*work = cfg.Work
		}
		if !explicit["allow-build"] && cfg.AllowBuild != nil {
			*allowBuild = *cfg.AllowBuild
		}
		if !explicit["auto-provision"] && cfg.AutoProvision != nil {
			*autoProvision = *cfg.AutoProvision
		}
		log.Printf("loaded node config: %s", path)
	}
	autoProvisionEnabled = *autoProvision

	ffx.Ffmpeg, ffx.Ffprobe = *ffmpeg, *ffprobe
	blenderBin = *blender
	if *id == "" {
		hn, _ := os.Hostname()
		*id = fmt.Sprintf("%s-%d", hn, os.Getpid())
	}
	if *work == "" {
		*work = filepath.Join(".", "worker-cache")
	}
	// 0700: the cached node credential lands in here, and a shared machine must
	// not let another account read it and join the pool as this node.
	os.MkdirAll(*work, 0o700)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// one measurement, reported verbatim: what this machine cannot see it does
	// not advertise, and the broker will not promise it to a buyer
	info := ffx.Detect(ctx, *work)
	if *force != "" {
		info.Encoders = []string{*force}
	}
	if *allowBuild {
		// Advertising a "build" runtime is the provider's consent: the broker only
		// dispatches compile jobs to nodes that say they accept them.
		info.Runtimes = append(info.Runtimes, ffx.Runtime{Name: "build", Version: "1"})
		log.Printf("编译任务已启用：本机将执行平台派发的买家编译命令（--allow-build）")
	}
	if *vmOn {
		caps, err := ffx.DetectVM(ctx, *qemu, *qemuImg, *vmBridge, *vmUplink, *vmMax)
		if err != nil {
			log.Printf("云主机功能未启用：%v", err)
		} else {
			info.VM = caps
			if *vmDir == "" {
				*vmDir = filepath.Join(*work, "vms")
			}
			os.MkdirAll(*vmDir, 0o700)
			log.Printf("云主机可出租：qemu %s accel=%s bridge=%q max=%d ipv6=%v",
				caps.Version, caps.Accel, caps.Bridge, caps.MaxVMs, info.IPv6)
			if len(info.IPv6) == 0 {
				log.Printf("警告：没有检测到公网 IPv6，控制台不会允许在这台机器上发布云主机")
			}
		}
	}
	nv := 0
	for _, g := range info.GPUs {
		if g.MemMB > 0 {
			nv++
		}
	}
	if nv > 0 && !strings.Contains(strings.Join(info.Encoders, " "), "nvenc") {
		nv = 0
	}
	if *slots == 0 {
		if nv > 0 {
			*slots = 2 * nv // NVENC has its own ASIC; two concurrent sessions per GPU still pays off
		} else {
			*slots = maxInt(1, info.CPUs/4)
		}
	}

	a := &agent{server: *server, id: *id, token: *token, launch: *launch, work: *work,
		hc: newClient(26 * time.Second)}
	// A launcher from the console cannot carry the label the provider typed: cmd.exe
	// parses a .cmd in the machine's OEM codepage, so the UTF-8 name would arrive
	// mangled. It rides on the ticket instead, and nameSet=false tells the broker
	// this Name is only the hostname - so a restart of the same machine keeps the
	// name the platform gave it.
	nameSet := *name != ""
	if !nameSet {
		*name = info.Hostname
	}
	a.loadSecret()
	hello := hub.NodeInfo{Inventory: info, ID: *id, Name: *name, NameSet: nameSet, Slots: *slots, Price: *price}
	a.lastHello = hello
	code, owned, err := a.hello()
	if err != nil {
		// a rejected hello will not fix itself: the machine has no way in. Spinning
		// on the claim endpoint afterwards only hides that from the provider.
		log.Fatalf("接入被拒绝: %v", err)
	}
	a.commNote("hello", "node="+*id)
	log.Printf("node %s online: %s/%s cpus=%d mem=%dMB gpus=%v slots=%d price=%.2f encoders=%v",
		*id, info.OS, info.Arch, info.CPUs, info.MemMB, info.GPUs, *slots, *price, info.Encoders)
	if a.nodeSecret() == "" {
		log.Printf("提示：Broker 没有发回节点凭据")
	}
	switch {
	case owned && *launch != "":
		log.Printf("已用启动码绑定到生成它的那个账户，不需要配对码")
	case owned:
		log.Printf("平台已记录该机器归属于某个账户，继续参与调度")
	case code != "":
		log.Printf("在网页端「我的节点」输入配对码 %s 认领这台机器", code)
	}
	go a.reannounce()
	go a.janitor(*cacheGB)
	if info.VM != nil {
		host := vmhost.New(*vmDir, *qemu, *qemuImg, *vmBridge, *vmUplink, info.VM.Accel, *vmMax)
		va := newVMAgent(a, host, *vmMax)
		go va.loop()
		go va.heartbeat()
		go va.janitor()
	}

	sem := make(chan struct{}, *slots)

	for {
		// Take a slot before asking for work. Polling first and then blocking on
		// the semaphore strands a claim the broker already leased, and a wedged
		// job then stops the whole node from polling at all.
		sem <- struct{}{}
		claim := a.poll()
		if claim == nil {
			<-sem
			continue
		}
		go func(c *hub.Claim) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("seg %s panicked: %v", c.SegID, r)
					a.fail(c, fmt.Sprintf("节点执行异常: %v", r))
				}
				<-sem
			}()
			a.run(c)
		}(claim)
	}
}

// agent is the machine's own view of its connection to the platform: the id it
// registered under, the credentials it presents at hello, and the per-node key
// the broker handed back. That key is what turns "worker=node-b produced this
// slice" from a claim into a fact, so a node cannot report on someone else's
// slice or fill another machine's queue.
type agent struct {
	server, id, token, work string
	hc                      *http.Client

	mu        sync.Mutex
	secret    string
	launch    string // spent by the broker on the first accepted hello
	lastHello hub.NodeInfo
}

// secretFile is the cached per-node credential. It is named after the node id
// because one machine can run two launchers that share --work: a single node.key
// would hand node A's credential to node B, and the broker reads that as proof
// that B is a machine it already knows.
func (a *agent) secretFile() string {
	return filepath.Join(a.work, "node-"+a.id+".key")
}

func (a *agent) setSecret(s string) {
	a.mu.Lock()
	a.secret = s
	a.mu.Unlock()
	if s != "" {
		// Cached so re-double-clicking the launcher tomorrow needs no new code:
		// the broker recognises the machine by this and skips admission entirely.
		if err := os.WriteFile(a.secretFile(), []byte(s), 0o600); err != nil {
			log.Printf("cannot cache node credential in %s: %v", a.secretFile(), err)
		}
	}
}

// loadSecret restores a credential from a previous run before the first hello.
func (a *agent) loadSecret() {
	b, err := os.ReadFile(a.secretFile())
	if err != nil {
		return
	}
	if s := strings.TrimSpace(string(b)); len(s) >= 16 {
		a.mu.Lock()
		a.secret = s
		a.mu.Unlock()
	}
}

func (a *agent) nodeSecret() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.secret
}

// newClient bounds the handshake and the first response byte. A whole-request
// Timeout would kill both the long poll and a multi-gigabyte source download, so
// the transport is where the limits go.
func newClient(poll time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: poll + 20*time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		MaxIdleConnsPerHost:   8,
	}
	return &http.Client{Transport: tr}
}

// decorate adds the credentials. The bootstrap token only rides on hello —
// sending the fleet-wide secret on every request would let one compromised node
// hand it to everything else.
func (a *agent) decorate(req *http.Request, asNode bool) {
	if !asNode && a.token != "" {
		req.Header.Set("X-Agent-Token", a.token)
	}
	if asNode {
		if s := a.nodeSecret(); s != "" {
			req.Header.Set("X-Node-Secret", s)
		}
	}
}

// hello sends the machine's inventory and picks up its node credential. It
// carries all three ways in — the shared agent token, a launch code from the
// console, and the cached per-node secret — because which one the broker wants
// depends on the profile and on whether this machine has been here before.
// Returns the one-time pairing code (empty when the node is already claimed by
// an account) and a non-nil error only when the broker refused the machine.
func (a *agent) hello() (code string, owned bool, err error) {
	a.mu.Lock()
	info := a.lastHello
	launch := a.launch
	a.mu.Unlock()

	b, err := json.Marshal(info)
	if err != nil {
		return "", false, err
	}
	req, err := http.NewRequest("POST", a.server+"/api/worker/hello", bytes.NewReader(b))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("X-Agent-Token", a.token)
	}
	if launch != "" {
		req.Header.Set("X-Launch-Code", launch)
	}
	if s := a.nodeSecret(); s != "" {
		req.Header.Set("X-Node-Secret", s)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", false, fmt.Errorf("%d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		ID        string `json:"id"`
		ClaimCode string `json:"claim_code"`
		Owned     bool   `json:"owned"`
		Secret    string `json:"secret"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", false, nil
	}
	if out.Secret != "" {
		a.setSecret(out.Secret)
	}
	if out.ID != info.ID {
		log.Printf("note: platform registered this machine as %s", out.ID)
	}
	a.mu.Lock()
	a.launch = "" // single-use; a stale code on the next re-announce is noise at best
	a.mu.Unlock()
	return out.ClaimCode, out.Owned, nil
}

func (a *agent) poll() *hub.Claim {
	u := fmt.Sprintf("%s/api/claim?worker=%s&wait=20", a.server, url.QueryEscape(a.id))
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		time.Sleep(3 * time.Second)
		return nil
	}
	a.decorate(req, true)
	resp, err := a.hc.Do(req)
	if err != nil {
		log.Printf("claim failed: %v", err)
		time.Sleep(3 * time.Second)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		a.idle()
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		log.Printf("claim error %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		if resp.StatusCode == 403 {
			// credential gone (broker rotated its key, or was started without our
			// token): re-announce rather than spin against a closed door
			if _, _, err := a.hello(); err != nil {
				log.Printf("re-register: %v", err)
			}
		}
		time.Sleep(3 * time.Second)
		return nil
	}
	var c hub.Claim
	if err := json.Unmarshal(body, &c); err != nil {
		return nil
	}
	return &c
}

func (a *agent) idle() {
	// light heartbeat so the broker keeps this node marked online
	a.post(a.server+"/api/report", map[string]any{"worker": a.id, "seg": "", "state": "idle"}, true)
}

func (a *agent) run(c *hub.Claim) {
	a.commNote("claim", "job="+c.JobID, "seg="+c.SegID, "kind="+c.Kind)
	switch c.Kind {
	case render.KindBlender:
		a.runBlender(c)
	case render.KindPR:
		a.runTimeline(c)
	case render.KindBuild:
		a.runBuild(c)
	default:
		a.runTranscode(c)
	}
}

func (a *agent) runTranscode(c *hub.Claim) {
	log.Printf("seg %s (job %s #%d) %.1f-%.1fs -> %s", c.SegID, c.JobID, c.Index, c.Start, c.Start+c.Dur, c.VCodec)
	src, err := a.fetchSource(c)
	if err != nil {
		a.fail(c, "source: "+err.Error())
		return
	}
	dst := filepath.Join(a.work, fmt.Sprintf("out-%s.mkv", c.SegID))
	defer os.Remove(dst)

	timeout := c.Dur*60 + 120
	if timeout < 120 {
		timeout = 120
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	t0 := time.Now()
	speed, err := ffx.Encode(ctx, ffx.EncOpts{
		Src: src, Dst: dst, Start: c.Start, Dur: c.Dur, VCodec: c.VCodec,
		Preset: c.Preset, Q: c.Q, Height: c.Height, Audio: c.Audio,
	}, func(p ffx.Progress) {
		pct := 100 * p.Out / c.Dur
		if pct > 99 {
			pct = 99
		}
		a.post(a.server+"/api/report", map[string]any{
			"worker": a.id, "seg": c.SegID, "state": "running",
			"pct": pct, "speed": p.Speed,
		}, true)
	})
	if err != nil {
		a.fail(c, err.Error())
		return
	}
	wall := time.Since(t0).Seconds()
	if speed <= 0 {
		speed = c.Dur / wall
	}

	a.finish(c, dst, wall, speed)
}

func (a *agent) fail(c *hub.Claim, msg string) {
	log.Printf("seg %s FAILED: %s", c.SegID, msg)
	a.commNote("failed", "seg="+c.SegID, "err="+strings.ReplaceAll(msg, "\n", " "))
	a.post(a.server+"/api/report", map[string]any{
		"worker": a.id, "seg": c.SegID, "state": "failed", "err": msg}, true)
}

// fetchSource downloads the job source once per worker and caches it. The
// per-job lock matters: several slots start at the same time and would otherwise
// fight over the same temp file. The URL is the signed capability that came with
// the claim, so no credential rides along with it.
var dlLocks sync.Map

func lockFor(key string) *sync.Mutex {
	v, _ := dlLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

const maxSourceBytes = 2 << 30

func (a *agent) fetchSource(c *hub.Claim) (string, error) {
	dst := filepath.Join(a.work, "src-"+c.JobID+".bin")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	mu := lockFor(c.JobID)
	mu.Lock()
	defer mu.Unlock()
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	req, err := http.NewRequest("GET", c.SrcURL, nil)
	if err != nil {
		return "", err
	}
	// A source URL that never finishes must not hold a slot forever: the slot is
	// what the whole node uses to ask for more work.
	dlCtx, dlCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer dlCancel()
	req = req.WithContext(dlCtx)
	resp, err := a.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GET source: %d（签名链接可能已过期，下一轮派发会重来）", resp.StatusCode)
	}
	if resp.ContentLength > maxSourceBytes {
		return "", fmt.Errorf("源文件超过 2 GiB 上限")
	}
	// A slow broker uplink is not a hung node: on the deployed broker an 11 MiB
	// source took 5m55s to arrive, three times past the duration-shaped lease, and
	// the broker correctly reclaimed slices from machines that were only fetching.
	// Beating during the copy is what tells those two situations apart.
	tmp := fmt.Sprintf("%s.%d.tmp", dst, os.Getpid())
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	var got int64
	stop := a.beat(c.SegID, "fetching", &got, resp.ContentLength)
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(counting{resp.Body, &got}, maxSourceBytes+1))
	stop()
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if got > maxSourceBytes {
		os.Remove(tmp)
		return "", fmt.Errorf("源文件超过 2 GiB 上限")
	}
	// Windows refuses to rename over an existing file.
	os.Remove(dst)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	log.Printf("cached source for job %s (%.1f MiB)", c.JobID, (float64(fileSize(dst)) / (1 << 20)))
	return dst, nil
}

// counting tallies bytes copied so the heartbeat can report a real fraction
// instead of "still alive".
type counting struct {
	r io.Reader
	n *int64
}

func (c counting) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	atomic.AddInt64(c.n, int64(n))
	return n, err
}

// beat keeps this slice's lease alive while the agent is blocked on a transfer.
// Returns the stop function; the last beat is the one that ends the ticker.
func (a *agent) beat(segID, state string, got *int64, total int64) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(8 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				pct := 0.0
				if total > 0 {
					pct = 100 * float64(atomic.LoadInt64(got)) / float64(total)
					if pct > 99 {
						pct = 99
					}
				}
				a.post(a.server+"/api/report", map[string]any{
					"worker": a.id, "seg": segID, "state": state, "pct": pct,
				}, true)
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop); <-done }
}

// janitor trims the source cache: a node that has touched a hundred jobs would
// otherwise keep a hundred films on the provider's disk. Sources touched in the
// last two minutes are still being read by ffmpeg, so they are left alone.
func (a *agent) janitor(maxGB float64) {
	limit := int64(maxGB * (1 << 30))
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		// unpacked bundles, frame directories and partially-concatenated parts are
		// large; a crashed render must not keep them forever
		for _, pat := range []string{"job-*", "frames-*", "parts-*"} {
			paths, _ := filepath.Glob(filepath.Join(a.work, pat))
			for _, p := range paths {
				st, err := os.Stat(p)
				if err == nil && time.Since(st.ModTime()) > 24*time.Hour {
					if os.RemoveAll(p) == nil {
						log.Printf("cache: dropped stale %s", filepath.Base(p))
					}
				}
			}
		}
		files, err := filepath.Glob(filepath.Join(a.work, "src-*.bin"))
		if err != nil || len(files) == 0 {
			continue
		}
		type entry struct {
			p  string
			mt time.Time
			sz int64
		}
		var es []entry
		var total int64
		for _, p := range files {
			st, err := os.Stat(p)
			if err != nil {
				continue
			}
			if time.Since(st.ModTime()) < 2*time.Minute {
				continue
			}
			es = append(es, entry{p, st.ModTime(), st.Size()})
			total += st.Size()
		}
		if total <= limit {
			continue
		}
		sort.Slice(es, func(i, j int) bool { return es[i].mt.Before(es[j].mt) })
		for _, e := range es {
			if total <= limit {
				break
			}
			if os.Remove(e.p) == nil {
				total -= e.sz
				log.Printf("cache: dropped %s (%.1f MiB)", filepath.Base(e.p), float64(e.sz)/(1<<20))
			}
		}
	}
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

func (a *agent) upload(segID, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	size := fileSize(path)
	h := sha256.New()

	// Stream the multipart body instead of building it in a bytes.Buffer: a
	// node with eight slots must not need eight copies of a multi-gigabyte
	// output in RAM just to upload them.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	ctype := mw.FormDataContentType()
	go func() {
		var err error
		part, err := mw.CreateFormFile("file", filepath.Base(path))
		if err == nil {
			_, err = io.Copy(part, io.TeeReader(f, h))
		}
		if cerr := mw.Close(); err == nil {
			err = cerr
		}
		pw.CloseWithError(err)
	}()
	defer pr.Close()

	u := a.server + "/api/upload?seg=" + url.QueryEscape(segID) + "&worker=" + url.QueryEscape(a.id)
	req, err := http.NewRequest("POST", u, pr)
	if err != nil {
		pr.CloseWithError(err)
		return "", 0, err
	}
	req.Header.Set("Content-Type", ctype)
	a.decorate(req, true)
	resp, err := a.hc.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", 0, fmt.Errorf("%d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

func (a *agent) post(url string, v any, asNode bool) {
	if _, err := a.postJSON(url, v, asNode); err != nil {
		log.Printf("post %s: %v", url, err)
	}
}

// postJSON sends a JSON body and returns the response body.
func (a *agent) postJSON(url string, v any, asNode bool) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	a.decorate(req, asNode)
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return body, fmt.Errorf("%d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// reannounce refreshes this machine's inventory with the broker. The claim path
// only counts as a heartbeat while the agent is idle, so without this a node busy
// encoding a long slice would look offline to the scheduler and have its
// in-flight pieces reclaimed mid-run.
func (a *agent) reannounce() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for range t.C {
		if _, _, err := a.hello(); err != nil {
			log.Printf("re-announce: %v", err)
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
