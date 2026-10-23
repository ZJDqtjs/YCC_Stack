package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"starstack/internal/auth"
	"starstack/internal/ffx"
	"starstack/internal/store"
)

// asUser creates a account plus a live session and returns the cookie the
// console would be carrying. The password fields are stubs: nothing here logs in
// through the API, because PBKDF2 at 200k iterations is not what is under test.
func asUser(t *testing.T, s *Server, name string) *http.Cookie {
	t.Helper()
	u := &store.User{ID: "u-" + name, Name: name, PassHash: "x", Salt: "y", Iter: auth.Iterations,
		CreatedAt: time.Now().Unix()}
	if err := s.st.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	tok := auth.RandomHex(32)
	if err := s.st.PutSession(tok, u.ID, time.Hour, "test"); err != nil {
		t.Fatalf("session: %v", err)
	}
	return &http.Cookie{Name: sessionCookie, Value: tok}
}

func helloReq(t *testing.T, id string) *http.Request {
	t.Helper()
	b, err := json.Marshal(NodeInfo{
		ID: id, Name: "launcher test", Slots: 2, Price: 0.8,
		Inventory: ffx.Inventory{Hostname: "testbox", OS: "windows", Arch: "amd64",
			CPUs: 8, MemMB: 16384, Encoders: []string{"libx264"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest("POST", "/api/worker/hello", strings.NewReader(string(b)))
}

// TestLaunchTicketBindsOnArrival walks the whole one-click path against a public
// broker: mint → download script → register with the code → re-register with
// only the credential the agent cached → an unrelated machine still gets 403.
func TestLaunchTicketBindsOnArrival(t *testing.T) {
	s := newTestServer(t, PublicPolicy())
	s.policy.AgentToken = "shared-token" // what deploy.sh puts in /etc/starstack/env
	ck := asUser(t, s, "alice")

	body, _ := json.Marshal(map[string]any{"price": 0.8, "slots": 2,
		// the display name is pasted into a .cmd that runs on the provider's
		// machine, so it arrives as a shell metacharacter carrier
		"name": `台式机"&calc|echo`, "os": "windows", "arch": "amd64"})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/nodes/launch", strings.NewReader(string(body)))
	r.AddCookie(ck)
	s.handleLaunchMint(rec, r)
	if rec.Code != 200 {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	var mint struct {
		NodeID string `json:"node_id"`
		Code   string
		Script string
		Worker string
		Ready  bool
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mint); err != nil {
		t.Fatal(err)
	}
	if mint.Ready {
		t.Fatal("no worker binary was installed, the console has to say so")
	}

	s.mu.Lock()
	name := s.launches[mint.Code].Name
	s.mu.Unlock()
	if strings.ContainsAny(name, `"&|`) {
		t.Fatalf("name %q still carries shell metacharacters", name)
	}

	rec = httptest.NewRecorder()
	s.handleLaunchScript(rec, httptest.NewRequest("GET", mint.Script, nil))
	if rec.Code != 200 {
		t.Fatalf("script: %d %s", rec.Code, rec.Body.String())
	}
	script := rec.Body.String()
	// cmd.exe parses a .cmd in the machine's OEM codepage, so anything non-ASCII
	// here (the label included) has to travel some other way
	for i := 0; i < len(script); i++ {
		if script[i] >= 0x80 {
			t.Fatalf("launcher is not pure ASCII at byte %d; a GBK cmd.exe would misparse it", i)
		}
	}
	if strings.Contains(script, `&calc|echo`) || strings.Contains(script, "--name") {
		t.Fatalf("the typed label must not ride on a Windows command line:\n%s", script)
	}
	if !strings.Contains(script, "LAUNCH="+mint.Code) || !strings.Contains(script, mint.NodeID) {
		t.Fatalf("launcher does not carry the ticket:\n%s", script)
	}

	hello := helloReq(t, mint.NodeID)
	hello.Header.Set("X-Launch-Code", mint.Code)
	rec = httptest.NewRecorder()
	s.handleHello(rec, hello)
	if rec.Code != 200 {
		t.Fatalf("hello with code: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Secret string `json:"secret"`
		Name   string `json:"name"`
		Owned  bool   `json:"owned"`
		Code   string `json:"claim_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// the label the provider typed in the page, delivered over JSON instead
	if out.Name != name {
		t.Fatalf("hello returned name %q, want %q", out.Name, name)
	}
	if !out.Owned || out.Code != "" {
		t.Fatalf("the ticket must bind the node and skip pairing: %+v", out)
	}
	if owner, _ := s.st.NodeOwner(mint.NodeID); owner == "" || owner != "u-alice" {
		t.Fatalf("node owner = %q, want the account that clicked", owner)
	}

	// the code in the URL is the whole capability, and hello spent it: a second
	// download of the same script must not work
	rec = httptest.NewRecorder()
	s.handleLaunchScript(rec, httptest.NewRequest("GET", mint.Script, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("spent code handed out a script: %d", rec.Code)
	}

	// re-double-clicking tomorrow: no code, only the credential cached on disk
	hello = helloReq(t, mint.NodeID)
	hello.Header.Set("X-Node-Secret", out.Secret)
	rec = httptest.NewRecorder()
	s.handleHello(rec, hello)
	if rec.Code != 200 {
		t.Fatalf("returning agent: %d %s", rec.Code, rec.Body.String())
	}

	// a machine that presents neither the token nor a matching credential stays
	// out — including one that claims the launched node's id without its secret
	for _, id := range []string{"someone-elses-box", mint.NodeID} {
		rec = httptest.NewRecorder()
		s.handleHello(rec, helloReq(t, id))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("anonymous hello as %s: %d, want 403", id, rec.Code)
		}
	}
}

// TestPauseSwitchStopsNewSlices is the page switch: the machine keeps its
// reputation and stays in the market view, it just stops being promised work.
func TestPauseSwitchStopsNewSlices(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	ck := asUser(t, s, "bob")
	n := node("a", 1, 2)
	s.workers[n.ID] = n
	if err := s.st.RegisterNode(&store.NodeRow{ID: n.ID, OwnerID: "u-bob", Name: n.Name,
		Price: n.Price, Slots: n.Slots, RegisteredAt: time.Now().Unix()}); err != nil {
		t.Fatalf("register node: %v", err)
	}

	switchTo := func(paused bool) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]bool{"paused": paused})
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", "/api/nodes/"+n.ID+"/pause", strings.NewReader(string(b)))
		r.AddCookie(ck)
		s.handleNodePause(rec, r)
		return rec
	}
	if rec := switchTo(true); rec.Code != 200 {
		t.Fatalf("pause: %d %s", rec.Code, rec.Body.String())
	}
	if !n.Paused {
		t.Fatal("the online worker must carry the switch")
	}
	if got := s.pickWorkerLocked(Req{}, "libx264", nil); got != nil {
		t.Fatalf("a paused node must not be picked, got %v", got.ID)
	}
	rows, err := s.st.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Paused {
		t.Fatalf("pause must survive a restart (restore reads it from the row): %+v", rows)
	}

	// someone else's machine is not yours to switch
	other := asUser(t, s, "mallory")
	b, _ := json.Marshal(map[string]bool{"paused": false})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/api/nodes/"+n.ID+"/pause", strings.NewReader(string(b)))
	r.AddCookie(other)
	s.handleNodePause(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign pause attempt: %d, want 403", rec.Code)
	}

	if rec := switchTo(false); rec.Code != 200 {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body.String())
	}
	if s.pickWorkerLocked(Req{}, "libx264", nil) == nil {
		t.Fatal("a resumed node must be pickable again")
	}
}
