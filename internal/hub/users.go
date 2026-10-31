package hub

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"starstack/internal/auth"
	"starstack/internal/render"
	"starstack/internal/store"
)

const sessionCookie = "starstack_session"

var sessionTTL = 30 * 24 * time.Hour

// ---- session plumbing ----

// sessionToken accepts the browser's HttpOnly cookie and the Authorization:
// Bearer header used by the Android app and the mini program, which live on
// origins where the SameSite=Lax cookie is never sent.
func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		return c.Value
	}
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func (s *Server) currentUser(r *http.Request) *store.User {
	tok := sessionToken(r)
	if tok == "" {
		return nil
	}
	uid, err := s.st.Session(tok)
	if err != nil || uid == "" {
		return nil
	}
	u, err := s.st.User(uid)
	if err != nil {
		return nil
	}
	return u
}

func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	u := s.currentUser(r)
	if u == nil {
		http.Error(w, `{"error":"未登录"}`, http.StatusUnauthorized)
		return nil, false
	}
	return u, true
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: s.cookiePath(), HttpOnly: true,
		Secure:   s.policy.secureCookie,
		SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(sessionTTL), MaxAge: int(sessionTTL.Seconds()),
	})
}

// cookiePath is the Path the session cookie is scoped to. Behind a reverse
// proxy that mounts the broker under a prefix (nginx: location /starstack/ with
// a stripping proxy_pass), the server only sees /api/..., so a cookie without an
// explicit Path would be stored against whatever directory the browser asked
// for — /starstack/api/auth/ for the login call — and vanish from every later
// request. Scoping it to the public prefix is what keeps sessions alive.
func (s *Server) cookiePath() string {
	if p := s.policy.CookiePath; p != "" {
		return p
	}
	return "/"
}

// authAllowed caps login/register attempts per client IP. Password hashing is
// deliberately slow (200k PBKDF2 rounds), which makes an unthrottled login
// endpoint a way to spend the broker's CPU on someone else's password list.
func (s *Server) authAllowed(w http.ResponseWriter, r *http.Request) bool {
	if s.limiter.allow(s.clientIP(r), s.policy.LoginPerMinute) {
		return true
	}
	log.Printf("auth rate limit hit for %s", s.clientIP(r))
	http.Error(w, jsonErr("尝试过于频繁，请一分钟后再试"), http.StatusTooManyRequests)
	return false
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}

// ---- accounts ----

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.authAllowed(w, r) {
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Invite   string `json:"invite"`
	}
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, `{"error":"请求格式错误"}`, 400)
		return
	}
	if s.policy.SignupInvite != "" &&
		!constantEqual(strings.TrimSpace(req.Invite), s.policy.SignupInvite) {
		http.Error(w, jsonErr("邀请码无效"), http.StatusForbidden)
		return
	}
	name := strings.TrimSpace(req.Name)
	if err := auth.ValidName(name); err != nil {
		http.Error(w, jsonErr(err.Error()), 400)
		return
	}
	if err := auth.ValidPassword(req.Password); err != nil {
		http.Error(w, jsonErr(err.Error()), 400)
		return
	}
	if _, err := s.st.UserByName(name); err == nil {
		http.Error(w, jsonErr("用户名已存在"), 409)
		return
	}
	salt := auth.NewSalt()
	u := &store.User{
		ID: "u" + newID() + newID(), Name: name,
		PassHash: auth.Hash(req.Password, salt, auth.Iterations),
		Salt:     hex.EncodeToString(salt), Iter: auth.Iterations,
		Credits: 0, CreatedAt: time.Now().Unix(),
	}
	if err := s.st.CreateUser(u); err != nil {
		log.Printf("create user: %v", err)
		http.Error(w, jsonErr("注册失败"), 500)
		return
	}
	token := auth.RandomHex(32)
	if err := s.st.PutSession(token, u.ID, sessionTTL, r.UserAgent()); err != nil {
		http.Error(w, jsonErr("会话创建失败"), 500)
		return
	}
	s.setSessionCookie(w, token)
	credit := s.policy.SignupCredit
	log.Printf("user %s (%s) registered, granted %g signup credits", u.Name, u.ID, credit)
	if _, err := s.st.AddCredits(u.ID, credit); err != nil {
		log.Printf("signup credit: %v", err)
	}
	bal, _ := s.st.Balance(u.ID)
	if credit > 0 {
		s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: store.Recharge, UserID: u.ID,
			Amount: credit, Balance: bal, Note: "新用户赠送额度"})
	}
	writeJSON(w, map[string]any{"id": u.ID, "name": u.Name, "credits": bal, "token": token})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authAllowed(w, r) {
		return
	}
	var req struct{ Name, Password string }
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, `{"error":"请求格式错误"}`, 400)
		return
	}
	u, err := s.st.UserByName(strings.TrimSpace(req.Name))
	// Same response for unknown user and wrong password: no account enumeration.
	if errors.Is(err, store.ErrNotFound) {
		auth.Hash(req.Password, auth.NewSalt(), auth.Iterations)
		http.Error(w, jsonErr("用户名或密码错误"), 401)
		return
	}
	if err != nil {
		http.Error(w, jsonErr("登录失败"), 500)
		return
	}
	if !auth.Match(req.Password, u.Salt, u.PassHash, u.Iter) {
		http.Error(w, jsonErr("用户名或密码错误"), 401)
		return
	}
	token := auth.RandomHex(32)
	if err := s.st.PutSession(token, u.ID, sessionTTL, r.UserAgent()); err != nil {
		http.Error(w, jsonErr("会话创建失败"), 500)
		return
	}
	s.setSessionCookie(w, token)
	log.Printf("user %s logged in", u.Name)
	writeJSON(w, map[string]any{"id": u.ID, "name": u.Name, "credits": u.Credits, "token": token})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := sessionToken(r); tok != "" {
		s.st.DropSession(tok)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: s.cookiePath(),
		MaxAge: -1, Expires: time.Unix(0, 0), HttpOnly: true,
		Secure: s.policy.secureCookie, SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		writeJSON(w, map[string]any{"signed_in": false})
		return
	}
	mine, _ := s.st.NodesByOwner(u.ID)
	bal, _ := s.st.Balance(u.ID)
	writeJSON(w, map[string]any{
		"signed_in": true, "id": u.ID, "name": u.Name, "credits": bal,
		"nodes": len(mine),
	})
}

// handleRecharge stands in for a payment provider: the ledger shape is real,
// only the money source is simulated. That is exactly why it must not be
// reachable on a public broker — self-service credits with no payment behind
// them are free money, so --public turns it off and the console hides the field.
func (s *Server) handleRecharge(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if !s.policy.Recharge {
		http.Error(w, jsonErr("本 Broker 未开放自助充值"), http.StatusForbidden)
		return
	}
	var req struct {
		Amount float64 `json:"amount"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Amount <= 0 {
		http.Error(w, jsonErr("金额无效"), 400)
		return
	}
	if req.Amount > 100000 {
		http.Error(w, jsonErr("单笔充值上限 100000 点"), 400)
		return
	}
	bal, err := s.st.AddCredits(u.ID, req.Amount)
	if err != nil {
		http.Error(w, jsonErr("充值失败"), 500)
		return
	}
	s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: store.Recharge, UserID: u.ID,
		Amount: req.Amount, Balance: bal, Note: "充值"})
	writeJSON(w, map[string]any{"ok": true, "credits": bal})
}

// ---- tasks ----

// handleJobs is the account's task list. POST submits a new job; GET returns
// history from SQLite (so it survives a broker restart) with the fresher
// in-memory snapshot substituted for anything still live.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleCreateJob(w, r)
		return
	}
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	rows, err := s.st.JobsByOwner(u.ID)
	if err != nil {
		http.Error(w, jsonErr("读取失败"), 500)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if live := s.jobs[row.ID]; live != nil {
			out = append(out, live.Snapshot())
			continue
		}
		out = append(out, jobFromRow(row).Snapshot())
	}
	writeJSON(w, map[string]any{"jobs": out, "credits": u.Credits})
}

// handleEstimate answers "what will this cost and can the pool do it" before a
// single byte is uploaded: it runs the same hard filter the scheduler uses and
// the same conservative hold the submission would take.
func (s *Server) handleEstimate(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	dur, _ := strconv.ParseFloat(r.URL.Query().Get("duration"), 64)
	if dur <= 0 {
		http.Error(w, jsonErr("缺少 duration"), 400)
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind == "transcode" {
		kind = ""
	}
	probe := &Job{
		Kind:     kind,
		Duration: dur,
		Profile:  Profile{VCodec: r.URL.Query().Get("vcodec")},
	}
	if probe.Profile.VCodec == "" {
		probe.Profile.VCodec = "libx264"
	}
	if rs := r.URL.Query().Get("req"); rs != "" {
		if err := json.Unmarshal([]byte(rs), &probe.Req); err != nil {
			http.Error(w, jsonErr("硬件要求格式错误"), 400)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	type cand struct {
		ID    string  `json:"id"`
		Name  string  `json:"name"`
		Price float64 `json:"price"`
		Speed float64 `json:"speed"`
		Free  int     `json:"free_slots"`
		GPU   string  `json:"gpu"`
		Mine  bool    `json:"mine"`
	}
	needReq, needVCodec := jobReq(probe)
	var cands []cand
	lowest := 0.0
	for _, wd := range s.workers {
		if !wd.online() || !wd.meets(needReq, needVCodec) {
			continue
		}
		gpu := ""
		if len(wd.GPUs) > 0 {
			gpu = wd.GPUs[0].Name
		}
		cands = append(cands, cand{ID: wd.ID, Name: wd.Name, Price: wd.Price,
			Speed: wd.Speed, Free: wd.Slots - s.activeLocked(wd), GPU: gpu,
			Mine: wd.OwnerID == u.ID})
		if lowest == 0 || wd.Price < lowest {
			lowest = wd.Price
		}
	}
	hold, holdPrice, basis := s.estimateHoldLocked(probe)
	// the node the scheduler would hand the first slice to, so the page can quote
	// the price that will probably be paid instead of only a theoretical floor
	var pick *Worker
	if len(cands) > 0 {
		pick = s.pickWorkerLocked(needReq, needVCodec, nil)
	}
	unit := "视频秒"
	if kind == "blender" {
		unit = "帧"
	}
	out := map[string]any{
		"duration": dur, "hold": hold, "hold_price": holdPrice, "hold_basis": basis,
		"cheapest": round4(dur * lowest), "eligible": len(cands), "nodes": cands,
		"balance": u.Credits, "affordable": u.Credits >= hold, "unit": unit,
	}
	if pick != nil {
		out["first_pick"] = map[string]any{"id": pick.ID, "name": pick.Name,
			"price": pick.Price, "speed": pick.Speed, "cost_weight": s.policy.CostWeight}
	}
	writeJSON(w, out)
}

// ---- node ownership ----

// handlePair binds an agent to the logged-in account using the one-time code
// the agent printed at registration.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req struct{ Code string }
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, jsonErr("请求格式错误"), 400)
		return
	}
	code := strings.ToUpper(strings.TrimSpace(req.Code))
	s.mu.Lock()
	nodeID, found := s.codes[code]
	if found {
		delete(s.codes, code)
		if wd := s.workers[nodeID]; wd != nil {
			wd.OwnerID = u.ID
			wd.pair = ""
		}
	}
	s.mu.Unlock()
	if !found {
		http.Error(w, jsonErr("配对码无效或已使用"), 404)
		return
	}
	if err := s.st.SetNodeOwner(nodeID, u.ID); err != nil {
		http.Error(w, jsonErr("绑定失败"), 500)
		return
	}
	log.Printf("user %s claimed node %s", u.Name, nodeID)
	writeJSON(w, map[string]any{"ok": true, "node_id": nodeID})
}

// handlePending lists unclaimed machines together with their pairing codes.
// It exists only to keep the single-machine demo one-click; a deployed platform
// would switch it off, because printing the code on the machine itself is the
// proof that whoever claims it physically held it.
func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	if !s.policy.ShowPending {
		http.Error(w, jsonErr("本 Broker 不公开待认领列表"), http.StatusForbidden)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for code, id := range s.codes {
		wd := s.workers[id]
		if wd == nil {
			continue
		}
		out = append(out, map[string]any{"code": code, "id": id, "name": wd.Name,
			"os": wd.OS, "cpus": wd.CPUs, "price": wd.Price, "online": wd.online()})
	}
	writeJSON(w, map[string]any{"pending": out})
}

// handleNodeOp is PUT /api/nodes/{id} with {"price": 0.8}: a provider re-prices
// its own machine, and the new rate applies to segments matched from now on.
func (s *Server) handleNodeOp(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if id, ok := commNodeSuffix(r.URL.Path); ok {
			s.handleNodeLogs(w, r, id)
			return
		}
	}
	if strings.HasSuffix(r.URL.Path, "/pause") {
		s.handleNodePause(w, r)
		return
	}
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, jsonErr("只支持 PUT"), 405)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	owner, err := s.st.NodeOwner(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, jsonErr("读取失败"), 500)
		return
	}
	if owner != u.ID {
		http.Error(w, jsonErr("这台机器不属于你"), 403)
		return
	}
	var req struct {
		Price float64 `json:"price"`
	}
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, jsonErr("请求格式错误"), 400)
		return
	}
	if req.Price < 0.05 || req.Price > 1000 {
		http.Error(w, jsonErr("单价需在 0.05 ~ 1000 之间"), 400)
		return
	}
	if err := s.st.SetNodePrice(id, req.Price); err != nil {
		http.Error(w, jsonErr("更新失败"), 500)
		return
	}
	s.mu.Lock()
	if wd := s.workers[id]; wd != nil {
		wd.Price = req.Price
	}
	s.mu.Unlock()
	log.Printf("node %s repriced to %.3f 点/视频秒 by %s", id, req.Price, u.Name)
	writeJSON(w, map[string]any{"ok": true, "id": id, "price": req.Price})
}

// ---- marketplace ----

// handleMarket aggregates what the pool can actually serve right now:
// schedulable slices per capability, not a raw node dump. Other users'
// machines are shown as counts and specs only.
func (s *Server) handleMarket(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type bucket struct {
		Encoder   string  `json:"encoder"`
		Nodes     int     `json:"nodes"`
		FreeSlots int     `json:"free_slots"`
		MinPrice  float64 `json:"min_price"`
		MaxPrice  float64 `json:"max_price"`
		AvgSpeed  float64 `json:"avg_speed"`
		GPU       string  `json:"best_gpu"`
	}
	buckets := map[string]*bucket{}
	for _, wd := range s.workers {
		if !wd.online() {
			continue
		}
		free := wd.Slots - s.activeLocked(wd)
		if free < 0 {
			free = 0
		}
		bestGPU := ""
		if len(wd.GPUs) > 0 {
			best := wd.GPUs[0]
			for _, g := range wd.GPUs {
				if g.MemMB > best.MemMB {
					best = g
				}
			}
			bestGPU = best.Name
		}
		for _, enc := range wd.Encoders {
			b := buckets[enc]
			if b == nil {
				b = &bucket{Encoder: enc, MinPrice: wd.Price, MaxPrice: wd.Price}
				buckets[enc] = b
			}
			b.Nodes++
			b.FreeSlots += free
			b.AvgSpeed += wd.Speed
			if wd.Price < b.MinPrice {
				b.MinPrice = wd.Price
			}
			if wd.Price > b.MaxPrice {
				b.MaxPrice = wd.Price
			}
			if bestGPU > b.GPU {
				b.GPU = bestGPU
			}
		}
	}
	out := make([]bucket, 0, len(buckets))
	for _, b := range buckets {
		if b.Nodes > 0 {
			b.AvgSpeed /= float64(b.Nodes)
		}
		out = append(out, *b)
	}
	writeJSON(w, map[string]any{"slices": out})
}

func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 500 {
		limit = v
	}
	spend, err := s.st.LedgerForUser(u.ID, limit)
	if err != nil {
		http.Error(w, jsonErr("读取失败"), 500)
		return
	}
	s.mu.Lock()
	var mine []string
	for _, wd := range s.workers {
		if wd.OwnerID == u.ID {
			mine = append(mine, wd.ID)
		}
	}
	s.mu.Unlock()
	if rows, err := s.st.Nodes(); err == nil {
		for _, n := range rows {
			if n.OwnerID == u.ID && !has(mine, n.ID) {
				mine = append(mine, n.ID)
			}
		}
	}
	earn, err := s.st.LedgerForNodes(mine, limit)
	if err != nil {
		http.Error(w, jsonErr("读取失败"), 500)
		return
	}
	writeJSON(w, map[string]any{
		"spend": spend, "earn": earn, "balance": round4(u.Credits), "nodes": len(mine),
	})
}

// ---- billing ----

// estimateHold prices a job conservatively: the whole duration at the most
// expensive node that could still satisfy the constraints, so a mid-job
// failover onto a pricier machine can never leave the platform underfunded.
// The optional baseline run happens on the broker's own machine and occupies no
// third-party compute, so it produces no segments and settles nothing.
// The rate and a plain-language basis come back with the total, because a
// number the buyer cannot interrogate is not a quote but a surprise deferred.
func (s *Server) estimateHoldLocked(j *Job) (hold, price float64, basis string) {
	price, basis = s.holdPriceLocked(j)
	return round4(j.Duration * price), price, basis
}

func (s *Server) holdPriceLocked(j *Job) (float64, string) {
	eligible, widest := 0.0, 0.0
	req, vcodec := jobReq(j)
	for _, wd := range s.workers {
		if !wd.online() || wd.Price <= 0 {
			continue
		}
		if wd.Price > widest {
			widest = wd.Price
		}
		if wd.meets(req, vcodec) && wd.Price > eligible {
			eligible = wd.Price
		}
	}
	if eligible > 0 {
		return eligible, "满足约束的在线节点中最高报价"
	}
	// Nothing matches the constraints right now, but the job is still queued and
	// may be placed later onto a machine that does. Price the hold at the dearest
	// online node rather than at a made-up floor: underfunding a failover is a
	// real loss, an over-sized hold is only a bigger refund.
	if widest > 0 {
		return widest, "暂无满足约束的节点，按在线节点最高报价预留"
	}
	// No agent is online at all, so there is no market to read. Use the rate a
	// fresh agent quotes by default.
	return defaultNodePrice, "暂无在线节点，按节点默认报价预留"
}

// defaultNodePrice mirrors cmd/worker's --price default. It is a documented
// fallback, not a measurement of anything.
const defaultNodePrice = 1.0

func (s *Server) settleSegment(j *Job, seg *Seg, wd *Worker) {
	// the rate frozen when the slice was handed out, not the node's current one:
	// the buyer authorised this piece at the price quoted for it
	price := seg.PriceAt
	if price <= 0 {
		price = wd.Price
	}
	cost := round4(seg.Dur * price)
	billed := seg.Dur
	note := "分片 " + seg.ID + " 提供 " + itoa(seg.Dur) + "s 算力"
	if j.Kind == render.KindBuild {
		// A build has no clip length: bill the wall-clock minutes it actually
		// occupied, rounded up, with a one-minute floor for a quick package.
		minutes := math.Ceil(seg.Wall / 60)
		if minutes < 1 {
			minutes = 1
		}
		cost = round4(price * minutes)
		billed = seg.Wall
		note = "编译任务 " + j.ID + " 提供 " + itoa(minutes) + " 分钟算力"
	}
	if cost <= 0 {
		return
	}
	seg.Cost = cost
	j.Spent = round4(j.Spent + cost)
	// An agent that has never been claimed has no account to pay, so the slice
	// still counts against the buyer's spend but the points go nowhere. A real
	// platform would hold them in escrow until the machine finds an owner; here
	// they are simply retained, which is why the demo always pairs its nodes first.
	if owner, err := s.st.NodeOwner(wd.ID); err == nil && owner != "" {
		if bal, err := s.st.AddCredits(owner, cost); err == nil {
			s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: store.Earn, UserID: owner,
				NodeID: wd.ID, JobID: j.ID, Amount: cost, Balance: bal,
				Note: note})
		}
		s.st.AddNodeEarnings(wd.ID, billed, cost)
		wd.Earned = round4(wd.Earned + cost)
		wd.Billed += billed
	}
	s.segComm(seg, wd.ID, "server", "seg.settled", map[string]any{
		"cost": cost, "price": price, "billed": billed, "note": note})
}

// releaseHold refunds whatever a completed job did not consume. Hold itself is
// left untouched: it is the amount the buyer was asked to authorise, and
// rewriting it to the spend would hide the refund from the job card.
func (s *Server) releaseHold(j *Job, why string) {
	if j.Released || j.OwnerID == "" {
		return
	}
	j.Released = true
	back := round4(j.Hold - j.Spent)
	if back <= 0 {
		return
	}
	if bal, err := s.st.AddCredits(j.OwnerID, back); err == nil {
		s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: store.Refund, UserID: j.OwnerID,
			JobID: j.ID, Amount: back, Balance: bal, Note: why})
	}
}

func (s *Server) chargeJob(j *Job, amount float64, kind, note string) (float64, error) {
	bal, err := s.st.AddCredits(j.OwnerID, -amount)
	if err != nil {
		return 0, err
	}
	s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: kind, UserID: j.OwnerID,
		JobID: j.ID, Amount: -amount, Balance: bal, Note: note})
	return bal, nil
}

func jsonErr(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

func round4(f float64) float64 { return float64(int64(f*10000+0.5)) / 10000 }

// clip bounds a string a provider agent typed at it: an unbounded error field
// is a cheap way to fill the database and every viewer's page.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func itoa(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }
