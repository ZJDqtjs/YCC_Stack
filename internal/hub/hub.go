package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"starstack/internal/auth"
	"starstack/internal/ffx"
	"starstack/internal/render"
	"starstack/internal/store"
	"starstack/internal/vm"
)

type Profile struct {
	VCodec   string `json:"vcodec"`
	Preset   string `json:"preset"`
	Q        int    `json:"q"`
	Height   int    `json:"height"`
	Audio    bool   `json:"audio"`
	Segs     int    `json:"segs"`
	Baseline bool   `json:"baseline"`
}

// Req is the hard-constraint half of a task description: what a piece of idle
// compute must physically have before it is even considered for matching.
// Zero values mean "unconstrained", because some agents cannot measure a field.
type Req struct {
	Runtime   string  `json:"runtime,omitempty"`  // e.g. "ffmpeg"
	Arch      string  `json:"arch,omitempty"`     // windows/amd64
	NeedGPU   bool    `json:"need_gpu,omitempty"` // require >=1 discrete GPU
	MinGPUMem int     `json:"min_gpu_mem_mb,omitempty"`
	MinCPUs   int     `json:"min_cpus,omitempty"`
	MinMemMB  int     `json:"min_mem_mb,omitempty"`
	MinDiskMB int     `json:"min_disk_mb,omitempty"`
	MaxPrice  float64 `json:"max_price,omitempty"` // 算力点/视频秒, 0 = 不限
}

type Seg struct {
	ID      string  `json:"id"`
	JobID   string  `json:"job_id"`
	Index   int     `json:"index"`
	Start   float64 `json:"start"`
	Dur     float64 `json:"dur"`
	Status  string  `json:"status"`
	Worker  string  `json:"worker"`
	Attempt int     `json:"attempt"`
	Pct     float64 `json:"pct"`
	Speed   float64 `json:"speed"`
	Wall    float64 `json:"wall"`
	Cost    float64 `json:"cost"`
	SHA     string  `json:"sha"`
	SrvSHA  string  `json:"srv_sha"`
	Match   bool    `json:"match"`
	Bytes   int64   `json:"bytes"`
	Err     string  `json:"err"`
	// PriceAt is the rate the node was quoted when the slice was handed out. A
	// provider that raises its price mid-flight must not be paid the new rate for
	// work the buyer authorised at the old one, and the hold was sized on it.
	PriceAt float64 `json:"price_at,omitempty"`
	// Excluded lists nodes that already lost this slice. Re-offering a failed
	// piece to the machine that just dropped it is how one bad card turns a
	// retryable slice into three failures.
	Excluded []string `json:"excluded,omitempty"`

	path  string
	lease time.Time
}

type Run struct {
	Status string  `json:"status"`
	Wall   float64 `json:"wall"`
	Err    string  `json:"err"`
}

type Job struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id,omitempty"`
	Name    string `json:"name"`
	// Kind is "transcode" (empty means that, for rows written before the field
	// existed) or one of the render kinds. It decides how a slice is executed,
	// what its artifact is named and how the pieces are put back together.
	Kind      string       `json:"kind,omitempty"`
	Spec      *render.Spec `json:"spec,omitempty"`
	Duration  float64      `json:"duration"`
	HasAudio  bool         `json:"has_audio"`
	SrcHeight int          `json:"src_height"`
	Profile   Profile      `json:"profile"`
	Req       Req          `json:"req"`
	Status    string       `json:"status"`
	Segs      []*Seg       `json:"segs"`
	Speed     float64      `json:"speed"`
	Wall      float64      `json:"wall"`
	SumWall   float64      `json:"sum_wall"`
	Baseline  *Run         `json:"baseline"`
	Hold      float64      `json:"credits_hold"`
	Spent     float64      `json:"credits_spent"`
	Note      string       `json:"note"`
	Err       string       `json:"err"`
	CreatedAt time.Time    `json:"created_at"`
	StartedAt time.Time    `json:"started_at"`
	Finished  time.Time    `json:"finished_at"`
	// Resumed marks a job that was re-dispatched after a broker restart: its
	// wall clock contains the downtime, so the speedup derived from it is not a
	// measurement of the scheduler and must not be presented as one.
	Resumed bool `json:"resumed,omitempty"`
	// Released guards the refund so a job can only give its unused hold back
	// once, even if two terminal paths (merge failure + scheduler failure) fire.
	Released bool `json:"released,omitempty"`

	path string
	out  string
}

func (j *Job) Snapshot() map[string]any {
	kind := j.Kind
	if kind == "" {
		kind = "transcode"
	}
	unit := "视频秒"
	if j.Spec != nil {
		unit = j.Spec.Unit()
	}
	return map[string]any{
		"id": j.ID, "owner_id": j.OwnerID, "name": j.Name, "duration": j.Duration,
		"kind": kind, "spec": j.Spec, "unit": unit,
		"has_audio": j.HasAudio, "src_height": j.SrcHeight, "profile": j.Profile,
		"req": j.Req, "status": j.Status, "speed": j.Speed, "wall": j.Wall,
		"sum_wall": j.SumWall, "baseline": j.Baseline, "credits_hold": j.Hold,
		"credits_spent": j.Spent, "note": j.Note, "err": j.Err, "created_at": j.CreatedAt,
		"started_at": j.StartedAt, "finished_at": j.Finished, "segs": j.Segs,
		"resumed": j.Resumed, "released": j.Released,
	}
}

// NodeInfo is the registration payload a provider agent sends: the machine's
// measured inventory plus the few things the provider chooses for itself.
type NodeInfo struct {
	ffx.Inventory
	ID   string `json:"id"`
	Name string `json:"node_name"`
	// NameSet says the provider chose Name on the command line. Without it Name is
	// just this machine's hostname, and a node the platform already labelled (from
	// a launcher ticket, or from the console) keeps that label across restarts.
	NameSet bool    `json:"name_set"`
	Slots   int     `json:"slots"`
	Price   float64 `json:"price"`
}

type Worker struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	OwnerID  string `json:"owner_id,omitempty"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUs     int    `json:"cpus"`
	MemMB    uint64 `json:"mem_mb"`
	// disk is what this machine had free at its last registration; it is
	// deliberately not live data, so it can only be a coarse constraint
	DiskFree uint64           `json:"disk_free_mb"`
	GPUs     []ffx.GPUDetail  `json:"gpus"`
	Encoders []string         `json:"encoders"`
	Runtimes []ffx.Runtime    `json:"runtimes"`
	IPv6     []string         `json:"ipv6,omitempty"`
	VM       *ffx.VMInventory `json:"vm,omitempty"`
	Price    float64          `json:"price"`
	Slots    int              `json:"slots"`
	Speed    float64          `json:"speed"`
	OK       int              `json:"ok"`
	Bad      int              `json:"bad"`
	Billed   float64          `json:"seconds_billed"`
	Earned   float64          `json:"earned"`
	Address  string           `json:"address"`
	// Streak counts consecutive slice failures and CoolUntil parks the node:
	// a machine that keeps dropping work stays in the market view but stops
	// receiving new slices until it has proven itself again by coming back clean.
	Streak    int       `json:"streak"`
	CoolUntil time.Time `json:"-"`
	// Paused is the owner's switch in the console: the agent keeps running and
	// keeps heartbeating, it is simply not promised any new slice.
	Paused bool `json:"paused"`

	seen    time.Time
	inbox   chan *Claim
	vminbox chan *vm.Command
	pair    string // pending pairing code, stable until someone claims this node
}

func (w *Worker) online() bool { return time.Since(w.seen) < 25*time.Second }

// cooling reports whether this node is parked after repeatedly dropping slices.
func (w *Worker) cooling() bool { return w.CoolUntil.After(time.Now()) }

func (seg *Seg) excludes(id string) bool {
	for _, x := range seg.Excluded {
		if x == id {
			return true
		}
	}
	return false
}

// exclude remembers that this node lost the slice. It is not a blacklist: the
// machine stays in the market and can pick up any other piece, it just cannot
// be handed the one it proved it would drop.
func (seg *Seg) exclude(id string) {
	if id == "" || seg.excludes(id) {
		return
	}
	seg.Excluded = append(seg.Excluded, id)
}

func (w *Worker) maxGPUMem() int {
	m := 0
	for _, g := range w.GPUs {
		if g.MemMB > m {
			m = g.MemMB
		}
	}
	return m
}

func (w *Worker) hasRuntime(name string) bool {
	for _, r := range w.Runtimes {
		if r.Name == name {
			return true
		}
	}
	return false
}

// meets reports whether this node satisfies a task's hard constraints. A field
// the agent could not measure (0) counts as unknown, and unknown now fails a
// stated minimum: the broker must only promise a buyer what a machine has
// demonstrated, otherwise "needs 8 GB of VRAM" silently matches a node that
// reported nothing at all.
func (w *Worker) meets(r Req, vcodec string) bool {
	// render jobs have no target codec; their hard requirement is a runtime
	// (Blender), which the caller puts in Req.Runtime instead
	if vcodec != "" && !has(w.Encoders, vcodec) {
		return false
	}
	if r.Runtime != "" && !w.hasRuntime(r.Runtime) {
		return false
	}
	if r.Arch != "" && w.Arch != r.Arch {
		return false
	}
	if r.NeedGPU && len(w.GPUs) == 0 {
		return false
	}
	if r.MinGPUMem > 0 && (w.maxGPUMem() == 0 || w.maxGPUMem() < r.MinGPUMem) {
		return false
	}
	if r.MinCPUs > 0 && (w.CPUs == 0 || w.CPUs < r.MinCPUs) {
		return false
	}
	if r.MinMemMB > 0 && (w.MemMB == 0 || int(w.MemMB) < r.MinMemMB) {
		return false
	}
	if r.MinDiskMB > 0 && (w.DiskFree == 0 || int(w.DiskFree) < r.MinDiskMB) {
		return false
	}
	if r.MaxPrice > 0 && w.Price > r.MaxPrice {
		return false
	}
	return true
}

type Claim struct {
	SegID  string  `json:"seg_id"`
	JobID  string  `json:"job_id"`
	Index  int     `json:"index"`
	Start  float64 `json:"start"`
	Dur    float64 `json:"dur"`
	SrcURL string  `json:"src_url"`
	VCodec string  `json:"vcodec"`
	Preset string  `json:"preset"`
	Q      int     `json:"q"`
	Height int     `json:"height"`
	Audio  bool    `json:"audio"`
	Lease  float64 `json:"lease"`
	// Render claims carry the whole spec minus the timeline (an agent only needs
	// the clips of its own slice) plus those clips, already trimmed and rebased
	// to the start of the slice.
	Kind  string        `json:"kind,omitempty"`
	Spec  *render.Spec  `json:"spec,omitempty"`
	Clips []render.Clip `json:"clips,omitempty"`
	// Bundle says the source URL is an archive that must be unpacked first.
	Bundle bool `json:"bundle,omitempty"`
}

type Server struct {
	mu         sync.Mutex
	dataDir    string
	baseURL    string
	st         *store.Store
	workers    map[string]*Worker
	jobs       map[string]*Job
	order      []string
	leaseBase  time.Duration
	codes      map[string]string // one-time node pairing code -> worker id
	launches   map[string]*launchTicket
	nodeBinDir string // where the worker binaries for the one-click launcher live
	policy     Policy
	key        *agentKey
	limiter    *rateLimiter
	secrets    *secretBox
	gateway    *gatewayState
	comfyHub   string // optional reverse-proxy target for the AI video share page
	// Cloud hosts are a second market on the same nodes: offers are snapshots
	// published by sellers, instances are prepaid rentals that keep their own
	// lifecycle and billing clock.
	voffers map[string]*vm.Offer
	vms     map[string]*vm.Instance
}

func NewServer(dataDir, baseURL string, st *store.Store, pol Policy) *Server {
	s := &Server{
		dataDir: dataDir, baseURL: baseURL, st: st,
		workers: map[string]*Worker{}, jobs: map[string]*Job{},
		leaseBase: 45 * time.Second, codes: map[string]string{},
		launches: map[string]*launchTicket{},
		policy:   pol, key: loadAgentKey(dataDir), limiter: newRateLimiter(),
		secrets: loadSecretBox(dataDir), gateway: newGatewayState(),
		voffers: map[string]*vm.Offer{}, vms: map[string]*vm.Instance{},
	}
	// A broker reached over HTTPS must not send its session cookie in the clear
	// on the next plaintext request.
	if strings.HasPrefix(baseURL, "https://") {
		s.policy.secureCookie = true
	}
	os.MkdirAll(filepath.Join(dataDir, "jobs"), 0o755)
	s.restore()
	return s
}

// SetComfyHubURL turns on the /comfy/ reverse proxy. The target is an internal
// HTTP service (ComfyHub) that reuses this broker's session cookie.
func (s *Server) SetComfyHubURL(u string) {
	s.comfyHub = strings.TrimRight(strings.TrimSpace(u), "/")
}

// restore reloads yesterday's state so a broker restart is not a reset: known
// nodes keep their owner, price and lifetime reputation (still offline until
// their agent calls home), and jobs that were mid-flight go back into the
// scheduler with their in-flight pieces un-leased.
func (s *Server) restore() {
	nodes, err := s.st.Nodes()
	if err != nil {
		log.Printf("restore nodes: %v", err)
	}
	for _, n := range nodes {
		var caps *ffx.VMInventory
		if n.VM != "" {
			var c ffx.VMInventory
			if json.Unmarshal([]byte(n.VM), &c) == nil {
				caps = &c
			}
		}
		s.workers[n.ID] = &Worker{
			ID: n.ID, Name: n.Name, OwnerID: n.OwnerID, Hostname: n.Hostname,
			OS: n.OS, Arch: n.Arch, CPUs: n.CPUs, MemMB: uint64(n.MemMB),
			DiskFree: uint64(n.DiskFreeMB), GPUs: fromStoreGPUs(n.GPUs),
			Encoders: n.Encoders, Runtimes: fromStoreRuntimes(n.Runtimes),
			IPv6: n.IPv6, VM: caps,
			Price: n.Price, Slots: n.Slots, Speed: n.Speed, OK: n.OK, Bad: n.Bad,
			Billed: n.SecondsBilled, Earned: n.Earned, Paused: n.Paused,
			seen:  time.Unix(n.LastSeen, 0),
			inbox: make(chan *Claim, 64), vminbox: make(chan *vm.Command, 16),
		}
	}
	jobs, err := s.st.LoadResumable()
	if err != nil {
		log.Printf("restore jobs: %v", err)
	}
	var rerun []string
	for _, row := range jobs {
		j := jobFromRow(row)
		var redisp bool
		for _, seg := range j.Segs {
			if inFlight(seg.Status) {
				seg.Status, seg.Worker, seg.Pct = "pending", "", 0
				seg.Attempt++
				redisp = true
			}
		}
		if j.Status == "running" {
			j.Status = "queued"
		}
		// StartedAt is deliberately kept: the wall clock of a resumed job spans
		// the outage, and Resumed is what tells the UI not to call that a speedup
		if redisp {
			j.Resumed = true
		}
		if j.Baseline != nil && j.Profile.Baseline {
			if row.BaselineWall > 0 {
				j.Baseline.Status, j.Baseline.Wall = "done", row.BaselineWall
			} else {
				j.Baseline = &Run{Status: "queued"}
				rerun = append(rerun, j.ID)
			}
		}
		s.jobs[j.ID] = j
		s.order = append(s.order, j.ID)
	}
	for _, id := range rerun {
		go s.runBaseline(id)
	}
	s.restoreVM()
	if len(nodes) > 0 || len(jobs) > 0 || len(s.vms) > 0 {
		log.Printf("restored %d node(s), %d unfinished job(s), %d vm offer(s), %d vm instance(s)",
			len(nodes), len(jobs), len(s.voffers), len(s.vms))
	}
}

func fromStoreGPUs(in []store.GPU) []ffx.GPUDetail {
	out := make([]ffx.GPUDetail, 0, len(in))
	for _, g := range in {
		out = append(out, ffx.GPUDetail{Name: g.Name, MemMB: g.MemMB})
	}
	return out
}

func fromStoreRuntimes(in []store.Runtime) []ffx.Runtime {
	out := make([]ffx.Runtime, 0, len(in))
	for _, r := range in {
		out = append(out, ffx.Runtime{Name: r.Name, Version: r.Version})
	}
	return out
}

// ---- write-through persistence ----
// The scheduler and reporting paths hold s.mu while they write; SQLite is
// configured with WAL + busy_timeout and one connection, so these are short,
// ordered and never contend with themselves.

func (s *Server) saveJobLocked(j *Job) {
	if err := s.st.UpdateJob(jobRowOf(j)); err != nil {
		log.Printf("persist job %s: %v", j.ID, err)
	}
}

func (s *Server) saveSegLocked(g *Seg) {
	if err := s.st.UpdateSegment(segRowOf(g)); err != nil {
		log.Printf("persist seg %s: %v", g.ID, err)
	}
}

func jobRowOf(j *Job) *store.JobRow {
	kind, plan := j.Kind, ""
	if kind == "" {
		kind = "transcode"
	}
	if j.Spec != nil {
		plan = string(mustJSON(j.Spec))
	}
	return &store.JobRow{
		ID: j.ID, OwnerID: j.OwnerID, Name: j.Name, SrcPath: j.path, Duration: j.Duration,
		Profile: string(mustJSON(j.Profile)), Req: string(mustJSON(j.Req)),
		Kind: kind, Plan: plan, Status: j.Status,
		CreditsHold: j.Hold, CreditsSpent: j.Spent, Note: j.Note, Err: j.Err,
		CreatedAt: unixOrZero(j.CreatedAt), StartedAt: unixOrZero(j.StartedAt),
		FinishedAt: unixOrZero(j.Finished), Wall: j.Wall, SumWall: j.SumWall,
		BaselineWall: baselineWall(j),
	}
}

func baselineWall(j *Job) float64 {
	if j.Baseline != nil {
		return j.Baseline.Wall
	}
	return 0
}

func segRowOf(g *Seg) *store.SegRow {
	return &store.SegRow{
		ID: g.ID, JobID: g.JobID, Idx: g.Index, Start: g.Start, Dur: g.Dur,
		Status: g.Status, NodeID: g.Worker, Attempt: g.Attempt, Pct: g.Pct,
		Speed: g.Speed, Wall: g.Wall, Bytes: g.Bytes, SHA: g.SHA, SrvSHA: g.SrvSHA,
		Verified: g.Match, Err: g.Err, Cost: g.Cost, Path: g.path,
		Price: g.PriceAt, Excluded: string(mustJSON(g.Excluded)),
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func timeOrZero(ts int64) time.Time {
	if ts == 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// jobFromRow rebuilds a live Job from a stored row, including its segments.
func jobFromRow(row *store.JobRow) *Job {
	j := &Job{
		ID: row.ID, OwnerID: row.OwnerID, Name: row.Name, path: row.SrcPath,
		Kind: row.Kind, Duration: row.Duration, Status: row.Status, Hold: row.CreditsHold,
		Spent: row.CreditsSpent, Note: row.Note, Err: row.Err,
		Wall: row.Wall, SumWall: row.SumWall,
		CreatedAt: timeOrZero(row.CreatedAt), StartedAt: timeOrZero(row.StartedAt),
		Finished: timeOrZero(row.FinishedAt),
	}
	if j.Kind == "transcode" {
		j.Kind = ""
	}
	json.Unmarshal([]byte(row.Profile), &j.Profile)
	json.Unmarshal([]byte(row.Req), &j.Req)
	if row.Plan != "" {
		var spec render.Spec
		if err := json.Unmarshal([]byte(row.Plan), &spec); err == nil {
			j.Spec = &spec
		}
	}
	if j.Profile.Baseline {
		j.Baseline = &Run{Status: "queued", Wall: row.BaselineWall}
	}
	for _, g := range row.Segs {
		seg := &Seg{
			ID: g.ID, JobID: g.JobID, Index: g.Idx, Start: g.Start, Dur: g.Dur,
			Status: g.Status, Worker: g.NodeID, Attempt: g.Attempt, Pct: g.Pct,
			Speed: g.Speed, Wall: g.Wall, Cost: g.Cost, SHA: g.SHA, SrvSHA: g.SrvSHA,
			Match: g.Verified, Bytes: g.Bytes, Err: g.Err, path: g.Path,
			PriceAt: g.Price,
		}
		json.Unmarshal([]byte(g.Excluded), &seg.Excluded)
		j.Segs = append(j.Segs, seg)
	}
	return j
}

func newID() string {
	b := make([]byte, 5)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) Handlers() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/worker/hello", s.handleHello)
	mux.HandleFunc("/api/claim", s.handleClaim)
	mux.HandleFunc("/api/report", s.handleReport)
	mux.HandleFunc("/api/upload", s.handleUpload)
	mux.HandleFunc("/api/jobs", s.handleJobs)
	mux.HandleFunc("/api/jobs/render", s.handleCreateRender)
	mux.HandleFunc("/api/estimate", s.handleEstimate)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/jobs/", s.handleJobFile)
	mux.HandleFunc("/download/", s.handleDownload)

	mux.HandleFunc("/api/auth/register", s.handleRegister)
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/logout", s.handleLogout)
	mux.HandleFunc("/api/me", s.handleMe)
	mux.HandleFunc("/api/me/recharge", s.handleRecharge)
	mux.HandleFunc("/api/pair", s.handlePair)
	mux.HandleFunc("/api/pending", s.handlePending)
	mux.HandleFunc("/api/nodes/launch", s.handleLaunchMint)
	mux.HandleFunc("/api/nodes/", s.handleNodeOp)
	mux.HandleFunc("/launch/", s.handleLaunchScript)
	mux.HandleFunc("/dl/", s.handleNodeBin)
	mux.HandleFunc("/api/market", s.handleMarket)
	mux.HandleFunc("/api/ledger", s.handleLedger)

	// Console preference (simple vs. advanced task form) and the community board.
	mux.HandleFunc("/api/me/settings", s.handleMySettings)
	mux.HandleFunc("/api/forum/posts", s.handleForumPosts)
	mux.HandleFunc("/api/forum/posts/", s.handleForumPost)
	mux.HandleFunc("/api/forum/media/", s.handleForumMedia)

	// IPv6 cloud hosts: buyer/seller console APIs, and the agent's own channel.
	mux.HandleFunc("/api/vm/offers", s.handleVMOffers)
	mux.HandleFunc("/api/vm/offers/", s.handleVMOfferOp)
	mux.HandleFunc("/api/vm/orders", s.handleVMOrder)
	mux.HandleFunc("/api/vm/instances", s.handleVMInstances)
	mux.HandleFunc("/api/vm/instances/", s.handleVMInstanceOp)
	mux.HandleFunc("/api/vm/claim", s.handleVMClaim)
	mux.HandleFunc("/api/vm/report", s.handleVMReport)

	// Model gateway: console management on /api, buyer API on /v1.
	mux.HandleFunc("/api/keys", s.handleAPIKeys)
	mux.HandleFunc("/api/keys/", s.handleAPIKeyOp)
	mux.HandleFunc("/api/models/upstreams", s.handleUpstreams)
	mux.HandleFunc("/api/models/upstreams/", s.handleUpstreamOp)
	mux.HandleFunc("/api/models/catalog", s.handleModelCatalog)
	mux.HandleFunc("/v1/models", s.handleV1Models)
	mux.HandleFunc("/v1/chat/completions", s.handleV1Chat)

	if s.comfyHub != "" {
		target, err := url.Parse(s.comfyHub)
		if err != nil || target.Host == "" {
			log.Printf("comfyhub: invalid URL %q, proxy disabled", s.comfyHub)
		} else {
			proxy := httputil.NewSingleHostReverseProxy(target)
			director := proxy.Director
			proxy.Director = func(req *http.Request) {
				req.URL.Path = strings.TrimPrefix(req.URL.Path, "/comfy")
				if req.URL.Path == "" {
					req.URL.Path = "/"
				}
				director(req)
				req.Host = target.Host
			}
			proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
				log.Printf("comfyhub proxy: %v", err)
				http.Error(w, "ComfyHub 暂不可用", http.StatusBadGateway)
			}
			mux.Handle("/comfy/", proxy)
			mux.HandleFunc("/comfy", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/comfy/", http.StatusPermanentRedirect)
			})
		}
	}
	mux.HandleFunc("/", handleIndex)
	return logWrap(mux)
}

// chatter are the agent's heartbeat paths: they fire every few seconds per
// node, so the handlers log the events that matter (a node coming online, a
// segment finishing) instead of the poll itself.
var chatter = map[string]bool{"/api/state": true, "/api/worker/hello": true, "/api/report": true,
	"/api/vm/claim": true, "/api/vm/report": true}

func logWrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/api/") &&
			!strings.HasPrefix(r.URL.Path, "/v1/") {
			h.ServeHTTP(w, r)
			return
		}
		if chatter[r.URL.Path] {
			h.ServeHTTP(w, r)
			return
		}
		t0 := time.Now()
		rw := &statusWriter{ResponseWriter: w, code: 200}
		h.ServeHTTP(rw, r)
		if r.URL.Path == "/api/claim" && rw.code == 204 {
			return // an idle long-poll timing out is not an event
		}
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rw.code, time.Since(t0).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

// ---- workers ----

// handleHello is the provider side of the platform: an agent arrives with the
// inventory it measured, and the broker stores it as the machine's advertised
// capability. The scheduler can then promise a buyer only what a node can
// demonstrate, and a node can only be matched by fields it actually reported.
func (s *Server) handleHello(w http.ResponseWriter, r *http.Request) {
	var info NodeInfo
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&info); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// Joining the pool is the step that must not be anonymous once the broker is
	// public: without it anyone can register a fake node, win every quote and
	// poison the reputation counters of the real machines next to it.
	ticket, why := s.admitAgent(r, info.ID)
	if ticket == nil && why != "" {
		http.Error(w, jsonErr(why), http.StatusForbidden)
		return
	}
	if info.Slots < 1 {
		info.Slots = 1
	}
	// Slots are self-reported; a node that claims 5000 concurrent slices would
	// otherwise be handed the whole queue and never finish it.
	if info.Slots > s.policy.MaxSlots {
		log.Printf("node %s reported %d slots, capped at %d", info.ID, info.Slots, s.policy.MaxSlots)
		info.Slots = s.policy.MaxSlots
	}
	if info.ID == "" {
		info.ID = info.Hostname + "-" + newID()
	}
	if info.Name == "" {
		info.Name = info.Hostname
	}

	s.mu.Lock()
	wd := s.workers[info.ID]
	fresh := wd == nil || !wd.online()
	if wd == nil {
		wd = &Worker{ID: info.ID, inbox: make(chan *Claim, 64), vminbox: make(chan *vm.Command, 16)}
		s.workers[info.ID] = wd
	}
	if wd.vminbox == nil {
		wd.vminbox = make(chan *vm.Command, 16)
	}
	if !info.NameSet && wd.Name != "" {
		// a machine the platform already labelled - from a launcher ticket, or from
		// an earlier run - keeps that label; the agent's Name here is only a hostname
		info.Name = wd.Name
	}
	// the agent owns this view of its machine; ownership, price and lifetime
	// reputation belong to the platform's ledger and survive its restarts
	wd.Name, wd.Hostname, wd.OS, wd.Arch = info.Name, info.Hostname, info.OS, info.Arch
	wd.CPUs, wd.MemMB, wd.DiskFree = info.CPUs, info.MemMB, info.DiskFreeMB
	wd.GPUs, wd.Encoders, wd.Runtimes = info.GPUs, info.Encoders, info.Runtimes
	wd.IPv6, wd.VM = info.IPv6, info.VM
	wd.Slots = info.Slots
	wd.seen, wd.Address = time.Now(), r.RemoteAddr
	if wd.Price <= 0 {
		wd.Price = info.Price
	}
	if wd.Price <= 0 {
		wd.Price = 1
	}
	if ticket != nil {
		// A launcher ticket already carries the account it was minted for, so this
		// machine is bound on arrival and never appears in anyone's 待认领 list.
		wd.OwnerID = ticket.OwnerID
		// The label the provider typed in the console rides on the ticket rather
		// than on the command line: a Windows .cmd cannot carry UTF-8 Chinese as an
		// argument without the OEM codepage mangling it.
		if ticket.Name != "" {
			wd.Name = ticket.Name
		}
		if err := s.st.SetNodeOwner(wd.ID, ticket.OwnerID); err != nil {
			log.Printf("launch bind %s: %v", wd.ID, err)
		}
	} else {
		owner, _ := s.st.NodeOwner(wd.ID)
		wd.OwnerID = owner
	}
	code := ""
	if wd.OwnerID == "" {
		// the code must stay stable across re-announcements, otherwise the
		// agent's startup log goes stale ten seconds later
		if wd.pair == "" {
			wd.pair = s.newPairCode(wd.ID)
		}
		code = wd.pair
	}
	row := &store.NodeRow{
		ID: wd.ID, OwnerID: wd.OwnerID, Name: wd.Name, Hostname: wd.Hostname,
		OS: wd.OS, Arch: wd.Arch, CPUs: wd.CPUs, MemMB: int(wd.MemMB),
		DiskFreeMB: int64(wd.DiskFree), GPUs: toStoreGPUs(wd.GPUs),
		Encoders: wd.Encoders, Runtimes: toStoreRuntimes(wd.Runtimes),
		IPv6: wd.IPv6, VM: vmCapsJSON(wd.VM),
		Price: wd.Price, Slots: wd.Slots, Speed: wd.Speed, OK: wd.OK, Bad: wd.Bad,
		RegisteredAt: time.Now().Unix(), LastSeen: time.Now().Unix(),
	}
	unclaimed := wd.OwnerID == ""
	s.mu.Unlock()

	if err := s.st.RegisterNode(row); err != nil {
		log.Printf("register node %s: %v", wd.ID, err)
	}
	// A machine coming back is the moment to resume its VM commands: a start
	// that was queued while it was offline, or a stop for an instance the
	// broker already settled and wants destroyed.
	s.mu.Lock()
	s.vmWakeNodeLocked(wd.ID, time.Now())
	s.mu.Unlock()
	if fresh {
		// the agent re-announces every few seconds as a heartbeat; logging each
		// one would bury the scheduling events that actually matter
		log.Printf("node %s came online: %s cpus=%d mem=%dMB disk=%dMB gpus=%d slots=%d price=%.2f encoders=%v runtimes=%d%s",
			wd.ID, wd.OS, wd.CPUs, wd.MemMB, wd.DiskFree, len(wd.GPUs), wd.Slots,
			wd.Price, wd.Encoders, len(wd.Runtimes), map[bool]string{
				// the broker mints the code, so after a restart it is only in this
				// line and the console's 待认领 list
				true:  " [待认领 配对码 " + code + "]",
				false: "",
			}[unclaimed])
	}
	writeJSON(w, map[string]any{"ok": true, "id": wd.ID, "price": wd.Price, "name": wd.Name,
		"claim_code": code, "owned": !unclaimed, "secret": s.nodeSecret(wd.ID)})
}

func toStoreGPUs(in []ffx.GPUDetail) []store.GPU {
	out := make([]store.GPU, 0, len(in))
	for _, g := range in {
		out = append(out, store.GPU{Name: g.Name, MemMB: g.MemMB})
	}
	return out
}

func toStoreRuntimes(in []ffx.Runtime) []store.Runtime {
	out := make([]store.Runtime, 0, len(in))
	for _, r := range in {
		out = append(out, store.Runtime{Name: r.Name, Version: r.Version})
	}
	return out
}

// newPairCode mints the one-time code an agent prints so a logged-in user can
// claim that machine. Without it, any account could absorb someone else's node.
func (s *Server) newPairCode(workerID string) string {
	for i := 0; i < 20; i++ {
		code := strings.ToUpper(auth.RandomHex(3))
		if _, taken := s.codes[code]; !taken {
			s.codes[code] = workerID
			return code
		}
	}
	return ""
}

// inFlight is every status in which a node holds a slice: handed out, fetching
// the source, transcoding, or pushing the result back. A slot is occupied and the
// lease applies in all of them, so the list is one predicate rather than four
// hand-copied conditionals — the fetching/uploading phases are exactly the ones a
// duration-shaped lease forgets, and on a slow-egress broker they were reclaimed
// from nodes that were perfectly healthy.
func inFlight(status string) bool {
	switch status {
	case "leased", "fetching", "running", "uploading":
		return true
	}
	return false
}

func (s *Server) activeLocked(w *Worker) int {
	n := 0
	for _, j := range s.jobs {
		for _, seg := range j.Segs {
			if seg.Worker == w.ID && inFlight(seg.Status) {
				n++
			}
		}
	}
	return n
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("worker")
	s.mu.Lock()
	wd := s.workers[id]
	s.mu.Unlock()
	if wd == nil {
		http.Error(w, "unknown worker, call /api/worker/hello first", 403)
		return
	}
	if !s.agentAuthed(r, id) {
		http.Error(w, jsonErr("节点凭据无效"), http.StatusForbidden)
		return
	}
	s.mu.Lock()
	wd.seen = time.Now()
	s.mu.Unlock()

	wait := 20 * time.Second
	if v := r.URL.Query().Get("wait"); v != "" {
		if sec, err := strconv.ParseFloat(v, 64); err == nil && sec > 0 {
			// an unbounded wait is a free way to pin a goroutine and a connection
			// on a 1 GB box; agents re-poll anyway
			if sec > 60 {
				sec = 60
			}
			wait = time.Duration(sec * float64(time.Second))
		}
	}
	select {
	case claim := <-wd.inbox:
		writeJSON(w, claim)
	case <-time.After(wait):
		w.WriteHeader(204)
	case <-r.Context().Done():
	}
}

// dispatchLocked reserves a slot on wd and hands the piece to its inbox.
// Callers hold s.mu.
func (s *Server) dispatchLocked(seg *Seg, wd *Worker) bool {
	j := s.jobs[seg.JobID]
	lease := s.leaseBase + time.Duration(seg.Dur*2*float64(time.Second))
	if lease < 30*time.Second {
		lease = 30 * time.Second
	}
	seg.Status = "leased"
	seg.Worker = wd.ID
	seg.Pct = 0
	seg.Err = ""
	// the rate is frozen with the lease: settlement uses this, not whatever the
	// provider's slider says by the time the slice comes back
	seg.PriceAt = wd.Price
	seg.lease = time.Now().Add(lease)
	// The source URL is a signed capability, not a guessable path: an anonymous
	// /api/jobs/{id}/source would let anyone who learns a job id pull the buyer's
	// original file, which on a public broker is other people's footage.
	src := s.signedSourceURL(j.ID, lease+10*time.Minute)
	claim := &Claim{
		SegID: seg.ID, JobID: j.ID, Index: seg.Index, Start: seg.Start, Dur: seg.Dur,
		SrcURL: src, VCodec: j.Profile.VCodec, Preset: j.Profile.Preset, Q: j.Profile.Q,
		Height: j.Profile.Height, Audio: j.Profile.Audio && j.HasAudio,
		Lease: lease.Seconds(),
	}
	if j.Kind != "" && j.Kind != "transcode" && j.Spec != nil {
		spec := *j.Spec
		// the full timeline stays on the broker: shipping it to every agent would
		// hand out the whole edit (asset names, cut structure) for one slice, and
		// the agent only ever renders its own range
		spec.Timeline = nil
		claim.Kind, claim.Spec, claim.Bundle = j.Kind, &spec, j.Spec.Bundle
		if j.Kind == render.KindPR {
			claim.Clips = render.SliceClips(j.Spec.Timeline, seg.Start, seg.Dur)
		}
	}
	select {
	case wd.inbox <- claim:
		return true
	default:
		seg.Status, seg.Worker = "pending", ""
		return false
	}
}

// ---- scheduler ----

func (s *Server) RunScheduler(ctx context.Context) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick()
		}
	}
}

// failureCooldown is how long a node that dropped three slices in a row is kept
// out of the matching pool. It is a pause, not a ban: the machine stays listed
// in the market so its owner can see it and raise its reliability.
const failureCooldown = 90 * time.Second

func (s *Server) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	// Reclaim in-flight pieces whose owner can no longer finish them. The sweep
	// is driven by segments, not workers: an offline node is exactly the case
	// that must be recovered, so it cannot be skipped for being offline.
	type jobQueue struct {
		job     *Job
		pending []*Seg
	}
	var queues []jobQueue
	for _, j := range s.jobsOrdered() {
		if j.Status == "done" || j.Status == "merging" || j.Status == "failed" {
			continue
		}
		for _, seg := range j.Segs {
			if !inFlight(seg.Status) {
				continue
			}
			if len(seg.path) > 0 {
				continue
			}
			wd := s.workers[seg.Worker]
			dead := wd == nil || !wd.online()
			if !dead && !seg.lease.Before(now) {
				continue
			}
			seg.Attempt++
			why := "lease expired"
			if dead {
				why = "node offline"
			}
			log.Printf("reclaiming job %s seg %d from %s (%s, attempt %d)",
				j.ID, seg.Index, seg.Worker, why, seg.Attempt)
			// a node that went quiet mid-slice must not get the same slice back the
			// moment it reconnects; that is how one flaky machine eats three retries
			seg.exclude(seg.Worker)
			if !dead {
				// an unreachable machine is not a dishonest one: strike only a node that
				// was still talking to the broker and let the lease run out anyway
				s.punishLocked(wd)
			}
			seg.Status, seg.Worker = "pending", ""
			if seg.Attempt >= 3 {
				seg.Status, seg.Err = "failed", "lost 3 times"
				s.failJobLocked(j)
				continue
			}
			s.saveSegLocked(seg)
		}
	}

	for _, j := range s.jobsOrdered() {
		if j.Status == "done" || j.Status == "merging" || j.Status == "failed" {
			continue
		}
		if s.expiredLocked(j, now) {
			limit := s.deadlineLocked(j)
			log.Printf("job %s exceeded its %v wall-clock ceiling, releasing the hold", j.ID, limit)
			j.Err = "超过 " + limit.String() + " 仍未完成"
			s.failJobLocked(j)
			continue
		}
		var pending []*Seg
		for _, seg := range j.Segs {
			if seg.Status == "pending" {
				pending = append(pending, seg)
			}
		}
		if len(pending) == 0 {
			if j.Status == "queued" {
				j.Status = "running"
				if j.StartedAt.IsZero() {
					j.StartedAt = time.Now()
				}
				s.saveJobLocked(j)
			}
			continue
		}
		if j.Status == "queued" {
			j.Status = "running"
			if j.StartedAt.IsZero() {
				j.StartedAt = time.Now()
			}
			s.saveJobLocked(j)
		}
		queues = append(queues, jobQueue{j, pending})
	}

	// Fair share. A plain FIFO pass lets the oldest queued job claim every free
	// slot and starve the rest, so each live job may only take its equal slice of
	// the capacity free at this tick.
	budget := s.dispatchBudgetLocked(len(queues))
	for _, q := range queues {
		for i, seg := range q.pending {
			if i >= budget {
				break
			}
			req, vc := jobReq(q.job)
			wd := s.pickWorkerLocked(req, vc, seg)
			if wd == nil {
				break
			}
			if !s.dispatchLocked(seg, wd) {
				break
			}
			s.saveSegLocked(seg)
		}
	}
	// The VM market has its own clock: prepaid runtime, heartbeats and stop
	// commands. It piggybacks on this tick so there is one scheduler goroutine.
	s.vmTickLocked(now)
}

// dispatchBudgetLocked is how many slices one job may claim per tick. It spreads
// the currently free slots over the jobs waiting for them; at least one so a
// lone job still makes progress when capacity trickles in.
func (s *Server) dispatchBudgetLocked(liveJobs int) int {
	if liveJobs < 1 {
		liveJobs = 1
	}
	free := 0
	for _, wd := range s.workers {
		if !wd.online() {
			continue
		}
		if n := wd.Slots - s.activeLocked(wd); n > 0 {
			free += n
		}
	}
	if n := free / liveJobs; n > 1 {
		return n
	}
	return 1
}

// deadlineLocked is the wall-clock ceiling on a live job: proportional to the
// footage so a long video is not killed mid-flight, floored so a short one still
// gets a reasonable grace period for queueing and failover.
func (s *Server) deadlineLocked(j *Job) time.Duration {
	d := time.Duration(j.Duration * s.policy.JobSlowFactor * float64(time.Second))
	if d < s.policy.MinJobDeadline {
		d = s.policy.MinJobDeadline
	}
	return d
}

// expiredLocked reports a job that has outlived its ceiling. The clock starts at
// submission, since the queue wait is exactly what locks the buyer's credits.
func (s *Server) expiredLocked(j *Job, now time.Time) bool {
	if j.CreatedAt.IsZero() || s.policy.JobSlowFactor <= 0 {
		return false
	}
	return now.Sub(j.CreatedAt) > s.deadlineLocked(j)
}

// punishLocked strikes a node that lost a slice it was holding. Three in a row
// parks it: the machine stays visible in the market but stops receiving work
// until the cooldown has passed.
func (s *Server) punishLocked(wd *Worker) {
	if wd == nil {
		return
	}
	wd.Bad++
	wd.Streak++
	if wd.Streak >= 3 {
		wd.Streak = 0
		wd.CoolUntil = time.Now().Add(failureCooldown)
		log.Printf("node %s parked for %v after 3 lost slices", wd.ID, failureCooldown)
	}
	s.st.TouchNode(wd.ID, wd.Speed, wd.OK, wd.Bad)
}

// pickWorkerLocked is the soft-scoring half of matching: among nodes that pass
// the hardware/software hard filter, prefer measured throughput x reliability
// per free slot, and give unproven new nodes an exploratory weight during
// their trial period. Price enters as a ratio against the average quote of the
// eligible pool, so the preference for cheaper compute is scale-free: it does
// not matter whether the market trades at 0.1 or 10 points per second.
// jobReq is the hard filter a job imposes on the market. A render job's
// capability is a tool (Blender) or an encoder (ffmpeg), not the buyer's target
// codec, so the mapping lives here instead of in every caller.
func jobReq(j *Job) (Req, string) {
	r, v := j.Req, j.Profile.VCodec
	// Render jobs have a fixed executor. For transcode the codec is the real
	// constraint, but a buyer-supplied runtime still must not point at a
	// different tool: an explicit value becomes ffmpeg, while an empty one stays
	// empty so nodes that predate runtime reporting keep working.
	switch j.Kind {
	case render.KindBlender:
		r.Runtime = "blender"
		v = ""
	case render.KindPR:
		r.Runtime = "ffmpeg"
		v = "libx264"
	default:
		if r.Runtime != "" {
			r.Runtime = "ffmpeg"
		}
	}
	return r, v
}

func (s *Server) pickWorkerLocked(req Req, vcodec string, seg *Seg) *Worker {
	var cands []*Worker
	for _, wd := range s.workers {
		if !wd.online() || !wd.meets(req, vcodec) {
			continue
		}
		if seg != nil && seg.excludes(wd.ID) {
			continue
		}
		if wd.cooling() {
			continue
		}
		// the owner put it on the page switch: it stays in the market, it just
		// takes nothing until they turn it back on
		if wd.Paused {
			continue
		}
		if wd.Slots-s.activeLocked(wd) <= 0 {
			continue
		}
		cands = append(cands, wd)
	}
	if len(cands) == 0 {
		return nil
	}
	ref := 0.0
	for _, wd := range cands {
		if wd.Price > 0 {
			ref += wd.Price
		}
	}
	ref /= float64(len(cands))

	var best *Worker
	var bestScore float64
	for _, wd := range cands {
		speed := wd.Speed
		if speed <= 0 {
			speed = 1.0 // trial-period probe value
		}
		conf := 0.5
		if wd.OK+wd.Bad > 0 {
			conf = float64(wd.OK) / float64(wd.OK+wd.Bad)
		}
		cost := 1.0
		if s.policy.CostWeight > 0 && wd.Price > 0 && ref > 0 {
			cost = math.Pow(ref/wd.Price, s.policy.CostWeight)
		}
		score := speed * conf * cost / float64(1+s.activeLocked(wd))
		if score > bestScore {
			bestScore, best = score, wd
		}
	}
	return best
}

func (s *Server) failJobLocked(j *Job) {
	if j.Status == "failed" {
		return
	}
	for _, seg := range j.Segs {
		if seg.Status == "pending" || inFlight(seg.Status) {
			seg.Status = "failed"
			if seg.Err == "" {
				seg.Err = "job failed"
			}
		}
		s.saveSegLocked(seg)
	}
	j.Status, j.Finished = "failed", time.Now()
	if j.Err == "" {
		j.Err = "segment failed after retries"
	}
	// nothing ran to completion on someone else's machine beyond what already
	// settled, so the unused part of the hold goes back
	s.releaseHold(j, "任务失败，未使用的预扣已退还")
	s.saveJobLocked(j)
}

func (s *Server) maybeFinishLocked(j *Job) {
	if j.Status == "done" || j.Status == "merging" || j.Status == "failed" {
		return
	}
	done, failed := 0, 0
	for _, seg := range j.Segs {
		switch seg.Status {
		case "done":
			done++
		case "failed":
			failed++
		}
	}
	if failed > 0 {
		s.failJobLocked(j)
		return
	}
	if done == len(j.Segs) && len(j.Segs) > 0 {
		j.Status = "merging"
		s.saveJobLocked(j)
		go s.merge(j)
	}
}

func (s *Server) jobsOrdered() []*Job {
	out := make([]*Job, 0, len(s.order))
	for _, id := range s.order {
		if j := s.jobs[id]; j != nil {
			out = append(out, j)
		}
	}
	return out
}

func (s *Server) merge(j *Job) {
	var err error
	if j.Kind == render.KindBlender || j.Kind == render.KindPR {
		err = s.mergeRender(j)
	} else {
		parts := make([]string, len(j.Segs))
		for i, seg := range j.Segs {
			parts[i] = seg.path
		}
		dst := filepath.Join(s.dataDir, "jobs", j.ID, "out.mp4")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err = ffx.Concat(ctx, parts, dst); err == nil {
			j.out = dst
		}
	}
	if err != nil {
		s.mu.Lock()
		j.Status, j.Err = "failed", "merge: "+err.Error()
		s.releaseHold(j, "合并失败，未使用的预扣已退还")
		s.saveJobLocked(j)
		s.mu.Unlock()
		log.Printf("job %s merge failed: %v", j.ID, err)
		return
	}
	s.mu.Lock()
	j.Status = "done"
	j.Finished = time.Now()
	j.Wall = j.Finished.Sub(j.StartedAt).Seconds()
	sum := 0.0
	for _, seg := range j.Segs {
		sum += seg.Wall
	}
	j.SumWall = sum
	if j.Wall > 0 {
		j.Speed = j.Duration / j.Wall
	}
	// providers are paid only for what actually ran; the rest of the hold comes back
	s.releaseHold(j, "任务完成，预扣差额退还")
	s.saveJobLocked(j)
	if j.Resumed {
		// wall contains the outage window, so this ratio says nothing about the
		// scheduler; print the raw numbers and refuse the speedup
		log.Printf("job %s done after broker restart: wall=%.1fs seg-sum=%.1fs billed=%.3f points (跨重启，不计算加速比)",
			j.ID, j.Wall, sum, j.Spent)
	} else {
		log.Printf("job %s done: wall=%.1fs seg-sum=%.1fs speedup=%.2fx billed=%.3f points",
			j.ID, j.Wall, sum, sum/j.Wall, j.Spent)
	}
	s.mu.Unlock()
}

// runBaseline is the single-machine reference run that makes the speedup claim
// measurable rather than derived.
func (s *Server) runBaseline(jobID string) {
	s.mu.Lock()
	j := s.jobs[jobID]
	if j == nil || j.Baseline == nil {
		s.mu.Unlock()
		return
	}
	j.Baseline.Status = "running"
	src, dst, p := j.path, filepath.Join(s.dataDir, "jobs", j.ID, "baseline.mp4"), j.Profile
	s.mu.Unlock()

	log.Printf("job %s baseline single-machine encode start", jobID)
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	_, err := ffx.Encode(ctx, ffx.EncOpts{
		Src: src, Dst: dst, VCodec: p.VCodec, Preset: p.Preset, Q: p.Q,
		Height: p.Height, Audio: p.Audio,
	}, nil)
	s.mu.Lock()
	j = s.jobs[jobID]
	if j != nil && j.Baseline != nil {
		j.Baseline.Wall = time.Since(t0).Seconds()
		if err != nil {
			j.Baseline.Status, j.Baseline.Err = "failed", err.Error()
		} else {
			j.Baseline.Status = "done"
		}
		s.saveJobLocked(j)
	}
	s.mu.Unlock()
	if err != nil {
		log.Printf("job %s baseline failed: %v", jobID, err)
		return
	}
	log.Printf("job %s baseline done in %.1fs", jobID, time.Since(t0).Seconds())
}

// ---- reports / uploads ----

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	var rep struct {
		Worker, Seg, State string
		Pct, Speed         float64
		Err, SHA           string
		Bytes              int64
		Wall               float64
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rep); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// the worker id arrives in the body, so it is only a fact once the credential
	// derived from it checks out
	if !s.agentAuthed(r, rep.Worker) {
		http.Error(w, jsonErr("节点凭据无效"), http.StatusForbidden)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rep.State == "idle" {
		if wd := s.workers[rep.Worker]; wd != nil {
			wd.seen = time.Now()
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	seg := s.segByID(rep.Seg)
	if seg == nil {
		http.Error(w, "no such segment", 404)
		return
	}
	if wd := s.workers[rep.Worker]; wd != nil {
		wd.seen = time.Now()
	}
	// one gate for every state: a node that no longer owns this slice cannot
	// report progress, a failure, or a duplicate result for it
	if !holdsLease(seg, rep.Worker) {
		log.Printf("seg %s: stale %s from %s ignored (held by %q, status %s)",
			seg.ID, rep.State, rep.Worker, seg.Worker, seg.Status)
		writeJSON(w, map[string]any{"ok": true, "ignored": "stale"})
		return
	}
	switch rep.State {
	case "fetching":
		// The node has the slice and is still pulling the source. This is not a
		// transcoding percentage: pct is how much of the file has landed, so the
		// console can show "取源 43%" instead of a slice that looks stuck at 0%.
		seg.Status = "fetching"
		seg.Worker = rep.Worker
		seg.Err = ""
		seg.Pct = rep.Pct
		seg.lease = time.Now().Add(s.leaseBase + time.Duration(seg.Dur*2*float64(time.Second)))
		s.saveSegLocked(seg)
	case "running":
		seg.Status = "running"
		seg.Worker = rep.Worker
		seg.Err = ""
		seg.Pct = rep.Pct
		seg.Speed = rep.Speed
		seg.lease = time.Now().Add(s.leaseBase + time.Duration(seg.Dur*2*float64(time.Second)))
		s.saveSegLocked(seg)
	case "uploading":
		seg.Status = "uploading"
		seg.Worker = rep.Worker
		seg.Pct = 100
		// same reason as fetching: a big slice pushed over a slow uplink outlasts
		// a lease sized by the clip's duration, and reclaiming it burns the work
		seg.lease = time.Now().Add(s.leaseBase + time.Duration(seg.Dur*2*float64(time.Second)))
		s.saveSegLocked(seg)
	case "done":
		j := s.jobs[seg.JobID]
		if seg.path == "" {
			kind := ""
			if j != nil {
				kind = j.Kind
			}
			seg.path = s.artifactPath(seg.JobID, kind, seg.Index)
		}
		seg.Status, seg.Pct, seg.Worker = "done", 100, rep.Worker
		seg.Err = ""
		seg.SHA, seg.Bytes = rep.SHA, rep.Bytes
		seg.Wall, seg.Speed = rep.Wall, rep.Speed
		// the broker re-hashes the bytes it received (see handleUpload); a
		// double match is what lets a semi-trusted platform pay out at all
		seg.Match = seg.SrvSHA != "" && seg.SrvSHA == rep.SHA
		wd := s.workers[rep.Worker]
		if wd != nil {
			wd.OK++
			// a clean delivery is the proof that ends any parked state early
			wd.Streak = 0
			wd.CoolUntil = time.Time{}
			if rep.Speed > 0 {
				wd.Speed = wd.Speed*0.6 + rep.Speed*0.4
			}
			s.st.TouchNode(wd.ID, wd.Speed, wd.OK, wd.Bad)
		}
		if j != nil && wd != nil && seg.Match {
			// only a verified result earns money
			s.settleSegment(j, seg, wd)
		}
		s.saveSegLocked(seg)
		if j != nil {
			s.saveJobLocked(j)
			s.maybeFinishLocked(j)
		}
		log.Printf("seg %s done on %s: %.1fs, verified=%v, cost=%.3f points, sha=%.12s",
			seg.ID, rep.Worker, seg.Wall, seg.Match, seg.Cost, rep.SHA)
	case "failed":
		seg.Attempt++
		why := clip(rep.Err, 200)
		seg.Err = why
		seg.exclude(rep.Worker)
		s.punishLocked(s.workers[rep.Worker])
		if j := s.jobs[seg.JobID]; j != nil && seg.Attempt < 3 {
			seg.Status, seg.Worker, seg.Pct, seg.Err = "pending", "", 0, ""
			if j.Status == "failed" {
				j.Status, j.Err = "running", ""
			}
			s.saveSegLocked(seg)
			s.saveJobLocked(j)
			log.Printf("seg %s failed on %s, requeued: %s", seg.ID, rep.Worker, why)
		} else {
			seg.Status = "failed"
			s.saveSegLocked(seg)
			log.Printf("seg %s permanently failed after %d attempts: %s", seg.ID, seg.Attempt, why)
			if j := s.jobs[seg.JobID]; j != nil {
				s.failJobLocked(j)
			}
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) segByID(id string) *Seg {
	for _, j := range s.jobs {
		for _, seg := range j.Segs {
			if seg.ID == id {
				return seg
			}
		}
	}
	return nil
}

// holdsLease is what keeps at-least-once from turning into double spending.
// A node whose lease was reclaimed can still come back with progress, a failed
// run, or an upload for the very same slice; if any of that were honoured it
// would overwrite the record (and the file) of the node that now really holds
// the slice. An unassigned slice accepts its old holder back, because nobody
// else has been given it yet.
func holdsLease(seg *Seg, worker string) bool {
	return seg.Status != "done" && (seg.Worker == worker || seg.Worker == "")
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	segID := r.URL.Query().Get("seg")
	worker := r.URL.Query().Get("worker")
	s.mu.Lock()
	seg := s.segByID(segID)
	// the bytes of a slice the platform already accepted (or already handed to
	// someone else) must not land on the same path and overwrite that result
	stale := seg != nil && !holdsLease(seg, worker)
	kind := ""
	if seg != nil {
		if j := s.jobs[seg.JobID]; j != nil {
			kind = j.Kind
		}
	}
	s.mu.Unlock()
	if seg == nil {
		http.Error(w, "no such segment", 404)
		return
	}
	if stale {
		http.Error(w, "lease no longer held", http.StatusConflict)
		return
	}
	if !s.agentAuthed(r, worker) {
		http.Error(w, jsonErr("节点凭据无效"), http.StatusForbidden)
		return
	}
	if err := r.ParseMultipartForm(1 << 24); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	defer f.Close()
	dst := s.artifactPath(seg.JobID, kind, seg.Index)
	os.MkdirAll(filepath.Dir(dst), 0o755)
	out, err := os.Create(dst + ".tmp")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	h := sha256.New()
	// one slice can never legitimately be bigger than the whole upload budget, and
	// an unbounded copy here is a disk-fill primitive available to any agent
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(f, s.policy.MaxUploadBytes))
	if n >= s.policy.MaxUploadBytes {
		out.Close()
		os.Remove(dst + ".tmp")
		http.Error(w, jsonErr("分片超出大小上限"), 413)
		return
	}
	cerr := out.Close()
	if err != nil || cerr != nil {
		os.Remove(dst + ".tmp")
		http.Error(w, fmt.Sprintf("write: %v", err), 500)
		return
	}
	os.Rename(dst+".tmp", dst)
	s.mu.Lock()
	seg.path, seg.Bytes = dst, n
	seg.SrvSHA = hex.EncodeToString(h.Sum(nil))
	s.saveSegLocked(seg)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "sha": seg.SrvSHA, "bytes": n})
}

// ---- jobs ----

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	// compute is bought, not borrowed: an anonymous submission would have no
	// account to hold credits against and no provider to pay out to
	u := s.currentUser(r)
	if u == nil {
		http.Error(w, jsonErr("请先登录后再提交任务"), 401)
		return
	}
	// The body cap is what stops a submission from eating the whole box: the old
	// ParseMultipartForm(1<<30) buffered a gigabyte of film in RAM on a machine
	// with one. MaxBytesReader rejects mid-upload instead of trusting Content-Length.
	r.Body = http.MaxBytesReader(w, r.Body, s.policy.MaxUploadBytes)
	if err := r.ParseMultipartForm(s.policy.MemoryBuffer); err != nil {
		s.deny(w, 400, "上传失败或超出大小上限", "job upload", err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	form := r.Form
	prof := Profile{
		VCodec: first(form, "vcodec", "libx264"),
		Preset: first(form, "preset", "veryfast"),
		Q:      atoi(first(form, "q", "26")),
		Height: atoi(first(form, "height", "0")),
		Audio:  first(form, "audio", "1") == "1",
		Segs:   atoi(first(form, "segs", "4")),
	}
	if prof.Segs < 1 {
		prof.Segs = 1
	}
	if prof.Segs > s.policy.MaxSegs {
		prof.Segs = s.policy.MaxSegs
	}
	if !allowedVCodec(prof.VCodec) || !allowedPreset(prof.Preset) {
		http.Error(w, jsonErr("编码器或 preset 不在允许列表内"), 400)
		return
	}
	if prof.Q > 51 {
		prof.Q = 51
	}
	if prof.Height > 4320 {
		http.Error(w, jsonErr("目标高度过大"), 400)
		return
	}
	prof.Baseline = first(form, "baseline", "0") == "1" && !s.policy.Public
	var req Req
	if rs := form.Get("req"); rs != "" && rs != "{}" {
		if len(rs) > 4096 {
			http.Error(w, jsonErr("硬件要求过长"), 400)
			return
		}
		if err := json.Unmarshal([]byte(rs), &req); err != nil {
			http.Error(w, jsonErr("硬件要求格式错误"), 400)
			return
		}
	}

	// A public broker never reads a client-supplied path: "path" used to go
	// straight into filepath.Abs and then ffprobe, which is an arbitrary file read
	// (/etc/passwd) and an SSRF (http://169.254.169.254/...) in one field.
	if p := form.Get("path"); p != "" && !s.policy.AllowLocalPath {
		http.Error(w, jsonErr("本 Broker 只接受文件上传，不接受服务器本地路径"), 400)
		return
	}

	var srcPath, name string
	// accepted/uploaded pair up in a defer: a submission that is refused after its
	// file already hit the disk (no credits, no matching node, too many jobs) must
	// not leave the video sitting in the data directory, or rejection becomes a
	// cheap way to fill the broker's disk.
	var uploaded, accepted bool
	defer func() {
		if uploaded && !accepted {
			os.RemoveAll(filepath.Dir(srcPath))
		}
	}()
	if f, hdr, err := r.FormFile("file"); err == nil {
		uploaded = true
		name = hdr.Filename
		id0 := newID()
		srcPath = filepath.Join(s.dataDir, "jobs", id0, "source"+safeExt(hdr.Filename))
		if err := s.diskOk(filepath.Dir(srcPath)); err != nil {
			s.deny(w, 507, "Broker 磁盘空间不足，请稍后重试", "disk", err)
			return
		}
		os.MkdirAll(filepath.Dir(srcPath), 0o755)
		out, err := os.Create(srcPath)
		if err != nil {
			s.deny(w, 500, "写入失败", "create source", err)
			return
		}
		if _, err := io.Copy(out, f); err != nil {
			out.Close()
			s.deny(w, 500, "上传中断", "copy source", err)
			return
		}
		out.Close()
		f.Close()
	} else if p := form.Get("path"); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			s.deny(w, 400, "路径无效", "abs path", err)
			return
		}
		if _, err := os.Stat(abs); err != nil {
			s.deny(w, 400, "服务器读不到这个路径", "stat path", err)
			return
		}
		srcPath, name = abs, filepath.Base(abs)
	} else {
		http.Error(w, "field 'file' or 'path' required", 400)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	meta, err := ffx.Probe(ctx, srcPath)
	if err != nil {
		s.deny(w, 400, "这个文件不是 Broker 能解析的视频", "probe", err)
		return
	}
	kfs, kerr := ffx.Keyframes(ctx, srcPath)
	segs := ffx.Plan(meta.Duration, kfs, prof.Segs)

	id := newID()
	job := &Job{
		ID: id, Name: name, Duration: meta.Duration, HasAudio: meta.HasAudio,
		SrcHeight: meta.Height, Profile: prof, Req: req, Status: "queued",
		path: srcPath, CreatedAt: time.Now(),
	}
	note := fmt.Sprintf("source %s: %.1fs, %dx%d %s, keyframes=%d",
		meta.VCodec, meta.Duration, meta.Width, meta.Height, boolStr(meta.HasAudio, "audio", "no audio"), len(kfs))
	if kerr == nil && len(segs) < prof.Segs {
		note += fmt.Sprintf("; asked for %d pieces, got %d (not enough keyframes to cut finer)", prof.Segs, len(segs))
	}
	job.Note = note
	for i, sg := range segs {
		job.Segs = append(job.Segs, &Seg{
			ID: id + "-" + strconv.Itoa(i), JobID: id, Index: i,
			Start: sg.Start, Dur: sg.Dur, Status: "pending",
		})
	}
	if prof.Baseline {
		job.Baseline = &Run{Status: "queued"}
		go s.runBaseline(id)
	}
	if len(job.Segs) == 0 {
		job.Segs = append(job.Segs, &Seg{ID: id + "-0", JobID: id, Index: 0, Start: 0, Dur: meta.Duration, Status: "pending"})
	}

	s.mu.Lock()
	// One account must not be able to park a thousand jobs in the queue, and a
	// job whose slices are refused later still has to release its upload.
	if live := s.countLiveJobsLocked(u.ID); live >= s.policy.MaxActiveJobs {
		s.mu.Unlock()
		http.Error(w, jsonErr(fmt.Sprintf("你有 %d 个任务在跑或排队，单账户上限 %d 个",
			live, s.policy.MaxActiveJobs)), http.StatusTooManyRequests)
		return
	}
	if meta.Duration > maxSourceSeconds {
		s.mu.Unlock()
		http.Error(w, jsonErr(fmt.Sprintf("源片时长 %.0f 秒超过上限 %d 秒",
			meta.Duration, maxSourceSeconds)), 400)
		return
	}
	hold, holdPrice, basis := s.estimateHoldLocked(job)
	job.OwnerID, job.Hold = u.ID, hold
	needReq, needVCodec := jobReq(job)
	if n := s.eligibleLocked(needReq, needVCodec); n == 0 {
		job.Note += "; 当前没有满足硬件要求的在线节点，任务将排队等待匹配算力上线"
	} else {
		job.Note += fmt.Sprintf("; 预扣依据：%s × %.2f 点/秒", basis, holdPrice)
	}
	if u.Credits < hold {
		s.mu.Unlock()
		http.Error(w, jsonErr(fmt.Sprintf("算力点不足：本次任务预扣 %.2f 点，余额 %.2f 点，请先充值",
			hold, u.Credits)), http.StatusPaymentRequired)
		return
	}
	if _, err := s.chargeJob(job, hold, store.Hold, "任务预扣（按实际用量结算，差额退还）"); err != nil {
		s.mu.Unlock()
		http.Error(w, jsonErr("预扣失败"), 500)
		return
	}
	if err := s.st.InsertJob(jobRowOf(job), segRowsOf(job)); err != nil {
		s.mu.Unlock()
		http.Error(w, jsonErr("任务登记失败"), 500)
		return
	}
	s.jobs[id] = job
	s.order = append(s.order, id)
	accepted = true
	log.Printf("job %s queued by %s: %s pieces, profile=%+v req=%+v hold=%.3f points",
		id, u.Name, job.Note, prof, req, hold)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "id": id, "note": job.Note,
		"pieces": len(job.Segs), "hold": hold})
}

func segRowsOf(j *Job) []*store.SegRow {
	out := make([]*store.SegRow, 0, len(j.Segs))
	for _, seg := range j.Segs {
		out = append(out, segRowOf(seg))
	}
	return out
}

// eligibleLocked counts the nodes that survive the hard filter, which is what
// turns "no matching machine" from a mysterious stall into a stated condition.
func (s *Server) eligibleLocked(r Req, vcodec string) int {
	n := 0
	for _, wd := range s.workers {
		if wd.online() && wd.meets(r, vcodec) {
			n++
		}
	}
	return n
}

func boolStr(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func (s *Server) handleJobFile(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // api jobs id field
	if len(parts) != 4 {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	j := s.jobs[parts[2]]
	s.mu.Unlock()
	if j == nil {
		http.NotFound(w, r)
		return
	}
	switch parts[3] {
	case "source":
		// This used to be a bare ServeFile: anyone who learned a 10-hex job id
		// could pull another user's footage, and in broker-local-path mode the
		// same route served any file ffprobe would accept. A worker now gets a
		// signed, expiring capability URL at claim time; the buyer may still fetch
		// their own source with their session.
		q := r.URL.Query()
		exp, _ := strconv.ParseInt(q.Get("exp"), 10, 64)
		owner := s.currentUser(r)
		if !s.sourceAllowed(j.ID, q.Get("sig"), exp) && !(owner != nil && owner.ID == j.OwnerID) {
			http.Error(w, jsonErr("无权读取该源文件"), http.StatusForbidden)
			return
		}
		http.ServeFile(w, r, j.path)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/download/")
	u := s.currentUser(r)
	s.mu.Lock()
	j, out := s.jobs[id], ""
	if j != nil && u != nil && j.OwnerID == u.ID {
		out = j.out
		if r.URL.Query().Get("which") == "baseline" && j.Baseline != nil {
			out = filepath.Join(s.dataDir, "jobs", id, "baseline.mp4")
		}
		if out == "" {
			// a job finished before the last broker restart lives only in
			// SQLite, but its merged file is still on disk under a known name
			for _, name := range []string{"out.mp4", "out.zip"} {
				p := filepath.Join(s.dataDir, "jobs", id, name)
				if _, err := os.Stat(p); err == nil {
					out = p
					break
				}
			}
		}
	}
	s.mu.Unlock()
	if out == "" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, out)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		// On a public broker this response is a map of every machine's hardware,
		// price and reputation, so it is not something an anonymous visitor gets.
		// The console only calls it after login.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"signed_in": false, "me": meView(nil),
			"workers": []any{}, "jobs": []any{}, "now": time.Now().Unix(),
			"policy": s.policyView()})
		return
	}
	s.mu.Lock()
	ws := make([]map[string]any, 0, len(s.workers))
	for _, wd := range s.workers {
		mine := u != nil && wd.OwnerID == u.ID
		view := map[string]any{
			"id": wd.ID, "name": wd.Name, "os": wd.OS, "arch": wd.Arch, "cpus": wd.CPUs,
			"mem_mb": wd.MemMB, "gpus": wd.GPUs, "encoders": wd.Encoders,
			"runtimes": wd.Runtimes, "slots": wd.Slots, "active": s.activeLocked(wd),
			"speed": wd.Speed, "ok": wd.OK, "bad": wd.Bad, "online": wd.online(),
			"since": time.Since(wd.seen).Seconds(), "price": wd.Price, "mine": mine,
			"paused": wd.Paused,
			// who a node belongs to is private; whether it belongs to anyone at all
			// is what tells the console to keep it out of the 待认领 list
			"claimed": wd.OwnerID != "",
			// hypervisor capability is shown to everyone (it is what the VM market
			// is selling); the interface addresses only to their owner
			"vm": wd.VM,
		}
		if wd.cooling() {
			view["parked_sec"] = int(time.Until(wd.CoolUntil).Seconds()) + 1
		}
		// a machine's hostname, address and earnings identify its owner; only
		// the owner's own console gets to see them
		if mine {
			view["hostname"] = wd.Hostname
			view["address"] = wd.Address
			view["ipv6"] = wd.IPv6
			view["disk_free_mb"] = wd.DiskFree
			view["earned"] = wd.Earned
			view["seconds_billed"] = wd.Billed
		}
		ws = append(ws, view)
	}
	// tasks are private: other users' filenames and progress are not part of
	// the marketplace view
	jobs := make([]map[string]any, 0, len(s.order))
	if u != nil {
		for _, j := range s.jobsOrdered() {
			if j.OwnerID == u.ID {
				jobs = append(jobs, j.Snapshot())
			}
		}
	}
	s.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{
		"workers": ws, "jobs": jobs, "now": time.Now().Unix(),
		"signed_in": u != nil,
		"me":        meView(u),
		"policy":    s.policyView(),
		"server":    map[string]any{"go_os": runtime.GOOS, "go_arch": runtime.GOARCH},
	})
}

func meView(u *store.User) map[string]any {
	if u == nil {
		return map[string]any{"signed_in": false}
	}
	return map[string]any{"signed_in": true, "id": u.ID, "name": u.Name, "credits": round4(u.Credits)}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func first(m map[string][]string, k, def string) string {
	if v := m[k]; len(v) > 0 && v[0] != "" {
		return v[0]
	}
	return def
}

func atoi(s string) int { n, _ := strconv.Atoi(strings.TrimSpace(s)); return n }

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
