package hub

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"starstack/internal/store"
)

func newTestServer(t *testing.T, pol Policy) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewServer(dir, "http://127.0.0.1:0", st, pol)
}

// node builds an online worker. Speed 0 means "never measured", which the
// scorer treats as a trial probe, so tests that compare throughput must set it.
func node(id string, price, speed float64) *Worker {
	return &Worker{
		ID: id, Name: id, Price: price, Speed: speed, Slots: 1,
		CPUs: 8, MemMB: 16384, Arch: "amd64", Encoders: []string{"libx264"},
		seen:  time.Now(),
		inbox: make(chan *Claim, 8),
	}
}

func TestCostWeightMovesTheChoice(t *testing.T) {
	// fast-but-dear against slower-but-cheap: the same two machines must rank
	// differently once money is allowed into the score
	fast, cheap := node("fast", 2.0, 4.0), node("cheap", 0.4, 3.0)

	pol := DefaultPolicy()
	pol.CostWeight = 0
	s := newTestServer(t, pol)
	s.workers[fast.ID], s.workers[cheap.ID] = fast, cheap
	if got := s.pickWorkerLocked(Req{}, "libx264", nil); got != fast {
		t.Fatalf("with money ignored, want the faster node, got %v", got)
	}

	pol.CostWeight = DefaultPolicy().CostWeight
	s.policy = pol
	if got := s.pickWorkerLocked(Req{}, "libx264", nil); got != cheap {
		t.Fatalf("with money weighted, want the cheaper node, got %v", got)
	}
}

func TestExcludedAndParkedNodesAreSkipped(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	a, b := node("a", 1, 2), node("b", 1, 1) // a is the better machine
	s.workers[a.ID], s.workers[b.ID] = a, b

	seg := &Seg{ID: "j-0", JobID: "j"}
	seg.exclude(a.ID)
	seg.exclude(a.ID)
	if len(seg.Excluded) != 1 {
		t.Fatalf("exclude must be idempotent, got %v", seg.Excluded)
	}
	if got := s.pickWorkerLocked(Req{}, "libx264", seg); got != b {
		t.Fatalf("the node that dropped this slice must not get it back, got %v", got)
	}

	seg2 := &Seg{ID: "j-1", JobID: "j"}
	if got := s.pickWorkerLocked(Req{}, "libx264", seg2); got != a {
		t.Fatalf("exclusion is per slice, other work still goes to a, got %v", got)
	}

	// three losses in a row parks a node; it stays listed, it just gets no work
	for i := 0; i < 3; i++ {
		s.punishLocked(b)
	}
	if !b.cooling() {
		t.Fatal("3 lost slices must park the node")
	}
	if got := s.pickWorkerLocked(Req{}, "libx264", seg); got != nil {
		t.Fatalf("the only machine left for this slice is parked, so nothing may be promised, got %v", got)
	}
	if got := s.pickWorkerLocked(Req{}, "libx264", seg2); got != a {
		t.Fatalf("parking b must not stop a from working, got %v", got)
	}
	if len(s.workers) != 2 {
		t.Fatal("parking is not removal: the machine stays in the market view")
	}
}

func TestHoldPriceBasis(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	j := &Job{Duration: 60, Profile: Profile{VCodec: "libx264"}}

	hold, price, basis := s.estimateHoldLocked(j)
	if price != defaultNodePrice || basis == "" {
		t.Fatalf("with no market at all the hold must name its fallback rate, got %v %q", price, basis)
	}
	if hold != round4(60*defaultNodePrice) {
		t.Fatalf("hold = duration x rate, got %v", hold)
	}

	// nodes exist but nothing satisfies the constraint: the hold is priced on the
	// dearest online machine, never on an invented floor
	dear := node("dear", 3.0, 1)
	dear.Encoders = []string{"libx265"}
	s.workers[dear.ID] = dear
	hold, price, _ = s.estimateHoldLocked(j)
	if price != 3.0 || hold != 180 {
		t.Fatalf("want the widest online quote 3.0/60s=180, got %v / %v", price, hold)
	}

	cheap, mid := node("cheap", 0.5, 1), node("mid", 1.5, 1)
	s.workers[cheap.ID], s.workers[mid.ID] = cheap, mid
	if _, price, _ = s.estimateHoldLocked(j); price != 1.5 {
		t.Fatalf("hold must be the dearest *feasible* quote, got %v", price)
	}

	j.Req.MaxPrice = 0.6
	if _, price, _ = s.estimateHoldLocked(j); price != 0.5 {
		t.Fatalf("a price cap must shrink the hold to what is still eligible, got %v", price)
	}
}

func TestSettlementUsesTheRateFrozenAtDispatch(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	wd := node("a", 4.0, 1) // it raised its price while the slice was running
	s.workers[wd.ID] = wd
	j := &Job{ID: "j", Duration: 10}
	seg := &Seg{ID: "j-0", JobID: j.ID, Dur: 10, PriceAt: 1.0}

	s.settleSegment(j, seg, wd)
	if seg.Cost != 10 {
		t.Fatalf("buyer authorised the slice at 1.0/s, settled %.2f", seg.Cost)
	}
	if j.Spent != 10 {
		t.Fatalf("job spend should follow the settled cost, got %v", j.Spent)
	}

	// a slice restored from an older database has no frozen rate
	old := &Seg{ID: "j-1", JobID: j.ID, Dur: 10}
	s.settleSegment(j, old, wd)
	if old.Cost != 40 {
		t.Fatalf("without a frozen rate, fall back to the node's quote, got %v", old.Cost)
	}
}

func TestDispatchBudgetSharesCapacity(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	a := node("a", 1, 1)
	a.Slots = 4
	s.workers[a.ID] = a

	if got := s.dispatchBudgetLocked(2); got != 2 {
		t.Fatalf("4 free slots over 2 jobs = 2 each, got %d", got)
	}
	if got := s.dispatchBudgetLocked(1); got != 4 {
		t.Fatalf("a lone job takes the whole pool, got %d", got)
	}
	a.Slots = 1
	if got := s.dispatchBudgetLocked(5); got != 1 {
		t.Fatalf("a trickle of capacity still has to move one job forward, got %d", got)
	}
	// the slot is already busy, so nothing may be promised this tick
	seg := &Seg{ID: "x-0", JobID: "x", Status: "running", Worker: a.ID}
	s.jobs["x"] = &Job{ID: "x", Segs: []*Seg{seg}}
	if got := s.dispatchBudgetLocked(1); got != 1 {
		t.Fatalf("budget floors at 1; the picker, not the budget, blocks an empty pool")
	}
	if s.pickWorkerLocked(Req{}, "libx264", nil) != nil {
		t.Fatal("a node with no free slot must not be picked")
	}
}

func TestJobDeadlineIsProportionalAndFloored(t *testing.T) {
	pol := DefaultPolicy()
	s := newTestServer(t, pol)
	now := time.Now()

	// 20s of footage: 20 x 60 = 20 minutes, under the floor, so the floor rules
	short := &Job{ID: "s", Duration: 20, CreatedAt: now.Add(-31 * time.Minute)}
	if !s.expiredLocked(short, now) {
		t.Fatal("a 20s job queued for over half an hour is stuck, not slow")
	}
	if d := s.deadlineLocked(short); d != pol.MinJobDeadline {
		t.Fatalf("short jobs must use the floor, got %v", d)
	}

	// an hour of footage scales: 3600 x 60s = 60 hours of allowance
	long := &Job{ID: "l", Duration: 3600, CreatedAt: now.Add(-70 * time.Minute)}
	if s.expiredLocked(long, now) {
		t.Fatal("an hour of footage on one slow machine must not be killed at 70 minutes")
	}
	if d := s.deadlineLocked(long); d != 60*time.Hour {
		t.Fatalf("long jobs must scale with duration, got %v", d)
	}
}

func TestMeetsFailsAFieldTheBuyerConstrained(t *testing.T) {
	// an agent that cannot see its memory reports 0; a buyer who asked for 8 GB
	// must not be silently matched to it
	blind := node("blind", 1, 1)
	blind.MemMB, blind.CPUs, blind.DiskFree = 0, 0, 0
	if blind.meets(Req{MinMemMB: 8192}, "libx264") {
		t.Fatal("unknown memory cannot satisfy a stated minimum")
	}
	if blind.meets(Req{MinCPUs: 4}, "libx264") {
		t.Fatal("unknown core count cannot satisfy a stated minimum")
	}
	if !blind.meets(Req{}, "libx264") {
		t.Fatal("with no stated minimum an incomplete inventory may still be used")
	}
}

// TestTransferPhasesRenewTheLease covers the failure the public deployment
// measured: an 11 MiB source took 5m55s to arrive from the broker over the
// internet, three times past a lease sized by the clip's duration, so the
// scheduler reclaimed slices from nodes that were healthy and merely fetching.
func TestTransferPhasesRenewTheLease(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	n := node("a", 1, 2)
	s.workers[n.ID] = n
	j := &Job{ID: "j", Status: "running", Duration: 60, CreatedAt: time.Now(),
		Profile: Profile{VCodec: "libx264"},
		Segs:    []*Seg{{ID: "j-0", JobID: "j", Dur: 15, Status: "pending"}}}
	s.jobs[j.ID] = j
	seg := j.Segs[0]
	if !s.dispatchLocked(seg, n) {
		t.Fatal("dispatch refused by a node with a free slot")
	}

	report := func(state string, pct float64) {
		body, _ := json.Marshal(map[string]any{"worker": n.ID, "seg": seg.ID,
			"state": state, "pct": pct})
		s.handleReport(httptest.NewRecorder(),
			httptest.NewRequest("POST", "/api/report", bytes.NewReader(body)))
	}

	for _, ph := range []string{"fetching", "uploading"} {
		seg.lease = time.Now().Add(-time.Second) // the clock the slow link blew past
		report(ph, 43)
		if seg.Status != ph {
			t.Fatalf("phase %s must be recorded, got %q", ph, seg.Status)
		}
		if !seg.lease.After(time.Now()) {
			t.Fatalf("phase %s left the lease expired: reclaim would fire on a busy node", ph)
		}
		if s.activeLocked(n) != 1 {
			t.Fatalf("a slice in %s still occupies its slot, counted %d", ph, s.activeLocked(n))
		}
		if got := s.pickWorkerLocked(Req{}, "libx264", nil); got != nil {
			t.Fatalf("the fetching node's only slot must not be promised twice, got %v", got)
		}
	}

	// a node that goes quiet mid-download is still a dead node: the reclaim path
	// keys on the heartbeat, and idle beats are the only thing that refreshes it
	seg.lease = time.Now().Add(time.Minute)
	n.seen = time.Now().Add(-30 * time.Second)
	if n.online() {
		t.Fatal("30s of silence must mark the node offline even with a fresh lease")
	}
}
