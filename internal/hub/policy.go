package hub

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"starstack/internal/ffx"
)

// Policy is the deployment profile. The same binary is used for the thesis demo
// (everything permissive, one machine, no accounts to protect) and for the
// internet-facing broker, where each convenience below is an attack surface:
// a broker-local file path is an arbitrary file read, the pairing-code list is
// a way to steal someone else's machine, and self-service recharge is free
// money. Public mode turns all of them off and requires agent credentials.
type Policy struct {
	Public bool

	// AgentToken is the shared secret an agent must present in
	// X-Agent-Token when it first registers a node. Empty means "any machine
	// may join", which is only acceptable on a laptop demo.
	AgentToken string

	AllowLocalPath bool    // accept a broker-local path as the job source
	ShowPending    bool    // list unclaimed agents with their pairing codes
	Recharge       bool    // allow /api/me/recharge
	SignupCredit   float64 // free credits for a fresh account
	secureCookie   bool    // set the Secure flag on session cookies (TLS / reverse proxy)
	CookiePath     string  // Path the session cookie is scoped to ("/starstack/" behind a proxy)
	TrustProxy     bool    // believe X-Forwarded-For for the client address

	MaxUploadBytes int64 // request body cap for a job submission
	MemoryBuffer   int64 // how much of that body may sit in RAM
	MaxSegs        int   // slices per job
	MaxSlots       int   // server-side ceiling on self-reported concurrency
	MaxActiveJobs  int   // live jobs per account
	LoginPerMinute int   // auth attempts per client IP

	// CostWeight prices the quote inside the soft score: the candidate's rate is
	// compared with the average of the eligible pool and raised to this power.
	// 0 ignores money (pure measured throughput), 1 makes cost the whole decision.
	CostWeight float64

	// A job lives at most Duration x JobSlowFactor seconds, floored at
	// MinJobDeadline. Without a ceiling a queued job that no node will ever match
	// keeps the buyer's authorised credits locked for good.
	JobSlowFactor  float64
	MinJobDeadline time.Duration
}

// DefaultPolicy is the demo profile: convenient, and useless on a public IP.
func DefaultPolicy() Policy {
	return Policy{
		AgentToken: "", AllowLocalPath: true, ShowPending: true, Recharge: true,
		SignupCredit:   100.0,
		MaxUploadBytes: 4 << 30, MemoryBuffer: 32 << 20,
		MaxSegs: 64, MaxSlots: 16, MaxActiveJobs: 8,
		LoginPerMinute: 30,
		CostWeight:     0.5,
		JobSlowFactor:  60, MinJobDeadline: 30 * time.Minute,
	}
}

// PublicPolicy is what --public installs. Signup credit survives because the
// platform has no payment provider yet, but it is capped and one-time.
func PublicPolicy() Policy {
	return Policy{
		Public:         true,
		AllowLocalPath: false, ShowPending: false, Recharge: false,
		SignupCredit:   20.0,
		MaxUploadBytes: 512 << 20, MemoryBuffer: 8 << 20,
		MaxSegs: 32, MaxSlots: 8, MaxActiveJobs: 3,
		LoginPerMinute: 10,
		// A stranger's machine must not pin a stranger's credits, and the market
		// is only honest about price if price actually moves the choice.
		CostWeight: 0.75, JobSlowFactor: 30, MinJobDeadline: 15 * time.Minute,
	}
}

// policyView tells the console which affordances this deployment turned on, so
// it stops offering a recharge box or a broker-local path field that the server
// would refuse anyway. These are limits, not secrets.
func (s *Server) policyView() map[string]any {
	return map[string]any{
		"public": s.policy.Public, "recharge": s.policy.Recharge,
		"local_path": s.policy.AllowLocalPath, "pending": s.policy.ShowPending,
		"signup_credit": s.policy.SignupCredit, "max_segs": s.policy.MaxSegs,
		"max_active_jobs": s.policy.MaxActiveJobs, "cost_weight": s.policy.CostWeight,
		"min_job_deadline_sec": int(s.policy.MinJobDeadline / time.Second),
		"job_slow_factor":      s.policy.JobSlowFactor,
		"max_slots":            s.policy.MaxSlots,
		// The console only offers the one-click launcher when there is actually a
		// windows/amd64 binary to hand out; the default platform is what a visitor
		// clicking the button without saying otherwise is going to get.
		"launch_windows": s.nodeBinReady("windows", "amd64"),
		"launch_linux":   s.nodeBinReady("linux", "amd64"),
		"launch_darwin":  s.nodeBinReady("darwin", "arm64"),
	}
}

// agentKeyFile holds the broker's HMAC master key. Signing claims with a key
// that lives next to the database means source URLs and node credentials can be
// verified without another table, and survive a restart unchanged.
type agentKey struct {
	mu sync.Mutex
	b  []byte
}

func loadAgentKey(dataDir string) *agentKey {
	k := &agentKey{}
	p := filepath.Join(dataDir, "agent.key")
	if b, err := os.ReadFile(p); err == nil && len(b) >= 32 {
		k.b = b[:32]
		return k
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("agent key: %v", err)
	}
	// 0600 before the write: this file authorises work dispatch.
	f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("agent key: cannot persist %s (%v); using an in-memory key, "+
			"issued tokens stop validating after a restart", p, err)
		k.b = b
		return k
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		log.Printf("agent key write: %v", err)
	}
	k.b = b
	return k
}

func (k *agentKey) sign(parts ...string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	m := hmac.New(sha256.New, k.b)
	for _, p := range parts {
		m.Write([]byte(p))
		m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// nodeSecret is derived, not stored: the same node id always maps to the same
// credential, so an agent does not have to persist anything and a broker
// restart does not lock every machine out. Revoking one node means rotating the
// whole key file — acceptable until there is a real device registry.
func (s *Server) nodeSecret(workerID string) string {
	return s.key.sign("node", workerID)
}

// constantEqual keeps token comparisons off the "leak by timing" path.
func constantEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// agentAuthed checks the per-node credential the broker issued at hello. It is
// what makes "worker=node-b" in a report or an upload a fact rather than a
// claim, so one machine cannot report on another's slices or fill its slots.
func (s *Server) agentAuthed(r *http.Request, workerID string) bool {
	if s.policy.AgentToken == "" {
		return true // demo profile: a local broker with no shared secret configured
	}
	return constantEqual(r.Header.Get("X-Node-Secret"), s.nodeSecret(workerID))
}

// signedSourceURL hands a worker a capability URL for one job's source file.
// The expiry is part of the signature, so a leaked URL stops working on its own.
func (s *Server) signedSourceURL(jobID string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	return s.baseURL + "/api/jobs/" + jobID + "/source?exp=" + strconv.FormatInt(exp, 10) +
		"&sig=" + s.key.sign("src", jobID, strconv.FormatInt(exp, 10))
}

func (s *Server) sourceAllowed(jobID, sig string, exp int64) bool {
	if exp < time.Now().Unix() {
		return false
	}
	want := s.key.sign("src", jobID, strconv.FormatInt(exp, 10))
	return hmac.Equal([]byte(want), []byte(strings.TrimSpace(sig)))
}

// ---- auth rate limiting ----

type bucket struct {
	n   int
	sec int64
}

type rateLimiter struct {
	mu sync.Mutex
	m  map[string]*bucket
}

func newRateLimiter() *rateLimiter { return &rateLimiter{m: map[string]*bucket{}} }

// allow counts one attempt for key inside the current fixed window.
func (rl *rateLimiter) allow(key string, perMinute int) bool {
	if perMinute <= 0 {
		return true
	}
	now := time.Now().Unix() / 60
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if len(rl.m) > 4096 { // the map is keyed by client IP; drop stale windows
		for k, b := range rl.m {
			if b.sec != now {
				delete(rl.m, k)
			}
		}
	}
	b := rl.m[key]
	if b == nil || b.sec != now {
		rl.m[key] = &bucket{n: 1, sec: now}
		return true
	}
	b.n++
	return b.n <= perMinute
}

// clientIP strips the port and prefers the reverse-proxy hop. nginx sets
// X-Forwarded-For; without reading it every user of the proxy shares one bucket.
// clientIP is the address the auth rate limiter keys on. X-Forwarded-For is
// only believed when the deployment says it sits behind a proxy: a broker with
// a socket open to the internet would otherwise let anyone pick their own
// apparent source and never be limited at all.
func (s *Server) clientIP(r *http.Request) string {
	if s.policy.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				xff = xff[:i]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.TrimPrefix(host, "[")
}

// deny is the client-facing error. On a public broker the detail goes to the
// log instead of the response: ffprobe/ffmpeg stderr carries absolute paths and
// sometimes the arguments we were called with.
func (s *Server) deny(w http.ResponseWriter, code int, public, why string, err error) {
	if err != nil {
		log.Printf("%s: %v", why, err)
	}
	http.Error(w, jsonErr(public), code)
}

// ---- submission guards ----

// maxSourceSeconds caps how long a submitted clip may be. Slicing, holding and
// merging all scale with it, and on a shared broker one 40-hour file would pin
// the queue behind it.
const maxSourceSeconds = 4 * 3600

// These reach the broker's own ffmpeg command line when a baseline run is
// enabled, so they are a list rather than a sanity check on user input.
var (
	allowedVCodecs = map[string]bool{"libx264": true, "libx265": true, "libsvtav1": true,
		"h264_nvenc": true, "hevc_nvenc": true, "h264_amf": true, "hevc_amf": true,
		"h264_qsv": true, "hevc_qsv": true}
	allowedPresets = map[string]bool{"ultrafast": true, "superfast": true, "veryfast": true,
		"faster": true, "fast": true, "medium": true, "slow": true, "slower": true,
		"veryslow": true, "p1": true, "p4": true, "p5": true, "p8": true}
)

func allowedVCodec(v string) bool { return allowedVCodecs[v] }

func allowedPreset(p string) bool { return allowedPresets[p] }

// safeExt keeps an upload's extension from becoming something odd on disk; the
// stored name is always "source"+ext under the job directory.
func safeExt(name string) string {
	e := strings.ToLower(filepath.Ext(name))
	if len(e) < 2 || len(e) > 8 {
		return ".bin"
	}
	for _, r := range e[1:] {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return ".bin"
		}
	}
	return e
}

// diskOk refuses a submission when the broker is nearly out of room, instead of
// discovering it halfway through a write. ffx.DiskFreeMB is the same probe the
// agents use, so it needs no extra dependency and returns 0 where unsupported.
func (s *Server) diskOk(dir string) error {
	freeMB := ffx.DiskFreeMB(dir)
	if freeMB == 0 {
		return nil // platform without a cheap statfs: let the write try
	}
	if freeMB < minFreeBytes>>20 {
		return &diskFullError{free: int64(freeMB) << 20}
	}
	return nil
}

const minFreeBytes = 2 << 30 // keep two gigabytes spare on the broker

type diskFullError struct{ free int64 }

func (e *diskFullError) Error() string {
	return strconv.FormatInt(e.free>>20, 10) + " MiB free, need " +
		strconv.FormatInt(minFreeBytes>>20, 10) + " MiB"
}

// countLiveJobsLocked counts submissions that still occupy the queue or a lease.
// Callers hold s.mu.
func (s *Server) countLiveJobsLocked(uid string) int {
	n := 0
	for _, j := range s.jobs {
		if j.OwnerID == uid && (j.Status == "queued" || j.Status == "running" || j.Status == "merging") {
			n++
		}
	}
	return n
}
