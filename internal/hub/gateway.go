package hub

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"starstack/internal/auth"
	"starstack/internal/store"
)

// The model gateway turns the same pool into an OpenAI-compatible endpoint: a
// buyer creates an sk-ss- key, a provider registers an OpenAI-compatible
// upstream (llama.cpp, vLLM, Ollama, ...), and the broker routes each call to
// the cheapest healthy upstream that serves the requested model. Tokens are
// metered from the upstream's usage block (or estimated when the upstream does
// not report one) and settled in the same credits ledger as compute slices.

const (
	gatewayKeyPrefix       = "sk-ss-"
	gatewayMaxBody         = 4 << 20
	gatewayMaxResponse     = 32 << 20
	gatewayMaxPromptTokens = 32768
	gatewayMaxTokens       = 8192
	gatewayDefaultTokens   = 1024
	gatewayProviderShare   = 0.9 // the remaining 10% is the matching fee
	gatewayCooldown        = 20 * time.Second
	gatewayTimeout         = 10 * time.Minute
	gatewayStickyTTL       = 30 * time.Minute
)

// ---- sealed upstream credentials ----

// secretBox seals provider API keys with a key file next to the database, so a
// backup of starstack.db alone does not expose every provider credential.
type secretBox struct {
	aead      cipher.AEAD
	ephemeral bool
}

func loadSecretBox(dataDir string) *secretBox {
	p := filepath.Join(dataDir, "gateway.key")
	key := make([]byte, 32)
	if b, err := os.ReadFile(p); err == nil && len(b) >= 32 {
		copy(key, b[:32])
	} else {
		if _, err := rand.Read(key); err != nil {
			log.Fatalf("gateway key: %v", err)
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			log.Printf("gateway key: cannot persist %s (%v); upstream credentials will not survive a restart", p, err)
		} else {
			f.Write(key)
			f.Close()
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		log.Fatalf("gateway key: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		log.Fatalf("gateway key: %v", err)
	}
	return &secretBox{aead: aead}
}

func (b *secretBox) seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := b.aead.Seal(nil, nonce, []byte(plain), nil)
	return base64.RawStdEncoding.EncodeToString(append(nonce, ct...)), nil
}

func (b *secretBox) open(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil {
		return "", errors.New("加密凭据格式损坏")
	}
	n := b.aead.NonceSize()
	if len(raw) <= n {
		return "", errors.New("加密凭据格式损坏")
	}
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("凭据无法解密（gateway.key 可能已被更换）")
	}
	return string(plain), nil
}

// ---- upstream health & HTTP client ----

type upstreamHealth struct {
	until   time.Time
	fails   int
	lastErr string
	lastOK  time.Time
}

type gatewayState struct {
	mu     sync.Mutex
	health map[string]*upstreamHealth
	sticky map[string]stickyPin
	seq    uint64
	client *http.Client
	probe  *http.Client
}

// stickyPin keeps one conversation on the upstream that already holds its
// prefix, so a multi-turn chat does not pay for the same prefill on every hop.
type stickyPin struct {
	upID string
	exp  time.Time
}

func newGatewayState() *gatewayState {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// A dead upstream should fail over quickly; a live one may stream for minutes.
	tr.ResponseHeaderTimeout = 60 * time.Second
	tr.MaxIdleConnsPerHost = 4
	return &gatewayState{
		health: map[string]*upstreamHealth{},
		sticky: map[string]stickyPin{},
		client: &http.Client{Transport: tr},
		probe:  &http.Client{Transport: tr.Clone(), Timeout: 8 * time.Second},
	}
}

func (g *gatewayState) h(id string) *upstreamHealth {
	h := g.health[id]
	if h == nil {
		h = &upstreamHealth{}
		g.health[id] = h
	}
	return h
}

func (g *gatewayState) cooling(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.h(id)
	return h.until.After(time.Now())
}

func (g *gatewayState) fail(id string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.h(id)
	h.fails++
	h.lastErr = clip(err.Error(), 240)
	h.until = time.Now().Add(gatewayCooldown)
}

func (g *gatewayState) ok(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.h(id)
	h.fails = 0
	h.lastErr = ""
	h.until = time.Time{}
	h.lastOK = time.Now()
}

func (g *gatewayState) view(id string) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.h(id)
	out := map[string]any{"fails": h.fails, "last_error": h.lastErr}
	if h.until.After(time.Now()) {
		out["cooling_sec"] = int(time.Until(h.until).Seconds()) + 1
	}
	if !h.lastOK.IsZero() {
		out["last_ok"] = h.lastOK.Unix()
	}
	return out
}

func (g *gatewayState) pin(key, upID string) {
	if key == "" || upID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	for k, p := range g.sticky {
		if now.After(p.exp) {
			delete(g.sticky, k)
		}
	}
	g.sticky[key] = stickyPin{upID: upID, exp: now.Add(gatewayStickyTTL)}
}

func (g *gatewayState) pinned(key string) string {
	if key == "" {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.sticky[key]
	if !ok || time.Now().After(p.exp) {
		delete(g.sticky, key)
		return ""
	}
	return p.upID
}

// ---- registration / catalog ----

type modelRoute struct {
	up    *store.Upstream
	price float64
}

// routeNeed is everything the gateway knows about a request before choosing a
// provider. Price is only one axis: an upstream that cannot hold the context,
// has not declared the required capability, or is not deterministic must not be
// picked even if it is the cheapest.
type routeNeed struct {
	PromptTokens int
	MaxTokens    int
	Caps         []string
	Sticky       string
}

// routesFor returns enabled upstreams serving model that fit the request,
// cheapest first. Equal prices rotate between calls so a single cheap machine
// is not the only one ever picked. The second return value counts every
// upstream that serves the model, before capability filtering, so the caller
// can distinguish "unknown model" from "no capable provider".
func (s *Server) routesFor(model string, need routeNeed) ([]modelRoute, int) {
	ups, err := s.st.Upstreams()
	if err != nil {
		log.Printf("gateway: list upstreams: %v", err)
		return nil, 0
	}
	total := 0
	var out []modelRoute
	for _, u := range ups {
		serves := false
		for _, m := range u.Models {
			if strings.EqualFold(m, model) {
				serves = true
				break
			}
		}
		if !serves {
			continue
		}
		total++
		if !upstreamFits(u, need) {
			continue
		}
		out = append(out, modelRoute{up: u, price: u.Price})
	}
	if len(out) == 0 {
		return nil, total
	}
	s.gateway.mu.Lock()
	rot := int(s.gateway.seq % uint64(len(out)))
	s.gateway.seq++
	s.gateway.mu.Unlock()
	if rot > 0 {
		out = append(out[rot:], out[:rot]...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].price < out[j].price })
	// A conversation that already warmed one upstream's prefix goes back there;
	// price still decides when the pinned provider is cooling or gone.
	if pinned := s.gateway.pinned(need.Sticky); pinned != "" {
		for i, rt := range out {
			if rt.up.ID != pinned {
				continue
			}
			if i > 0 {
				reordered := make([]modelRoute, 0, len(out))
				reordered = append(reordered, rt)
				reordered = append(reordered, out[:i]...)
				reordered = append(reordered, out[i+1:]...)
				out = reordered
			}
			break
		}
	}
	return out, total
}

func upstreamFits(u *store.Upstream, need routeNeed) bool {
	limit := u.MaxContext
	if limit <= 0 {
		limit = 32768
	}
	if need.PromptTokens+need.MaxTokens > limit {
		return false
	}
	for _, c := range need.Caps {
		if !has(u.Caps, c) {
			return false
		}
	}
	return true
}

// routeCost prices a call against one provider's three rates. Cached prompt
// tokens are charged at PriceCached when set; output tokens at PriceOut. A zero
// override means "same as the input price".
func routeCost(u *store.Upstream, prompt, cached, completion int) float64 {
	if cached > prompt {
		cached = prompt
	}
	inPrice := u.Price
	cachePrice := u.PriceCached
	if cachePrice <= 0 {
		cachePrice = inPrice
	}
	outPrice := u.PriceOut
	if outPrice <= 0 {
		outPrice = inPrice
	}
	return round4(float64(prompt-cached)/1000*inPrice +
		float64(cached)/1000*cachePrice +
		float64(completion)/1000*outPrice)
}

// routeReserve assumes no cache hit: the hold must cover the worst case.
func routeReserve(u *store.Upstream, prompt, maxTokens int) float64 {
	r := routeCost(u, prompt, 0, maxTokens)
	if r < 0.0001 {
		r = 0.0001
	}
	return r
}

type catalogModel struct {
	ID           string   `json:"id"`
	Providers    int      `json:"providers"`
	PricePer1K   float64  `json:"price_per_1k"`
	MaxContext   int      `json:"max_context"`
	Capabilities []string `json:"capabilities,omitempty"`
	CheapestName string   `json:"cheapest_upstream,omitempty"`
}

func (s *Server) modelCatalog() []catalogModel {
	ups, err := s.st.Upstreams()
	if err != nil {
		log.Printf("gateway: catalog: %v", err)
		return nil
	}
	idx := map[string]*catalogModel{}
	var order []string
	for _, u := range ups {
		for _, m := range u.Models {
			key := strings.ToLower(strings.TrimSpace(m))
			c := idx[key]
			if c == nil {
				c = &catalogModel{ID: m, PricePer1K: u.Price, CheapestName: u.Name,
					MaxContext: contextOf(u)}
				idx[key] = c
				order = append(order, key)
			}
			c.Providers++
			if u.Price < c.PricePer1K {
				c.PricePer1K = u.Price
				c.CheapestName = u.Name
			}
			if n := contextOf(u); n > c.MaxContext {
				c.MaxContext = n
			}
			for _, cap := range u.Caps {
				if !has(c.Capabilities, cap) {
					c.Capabilities = append(c.Capabilities, cap)
				}
			}
		}
	}
	out := make([]catalogModel, 0, len(order))
	for _, k := range order {
		c := idx[k]
		sort.Strings(c.Capabilities)
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func contextOf(u *store.Upstream) int {
	if u.MaxContext <= 0 {
		return 32768
	}
	return u.MaxContext
}

// ---- console: API keys ----

func (s *Server) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		keys, err := s.st.APIKeysByUser(u.ID)
		if err != nil {
			http.Error(w, jsonErr("读取失败"), 500)
			return
		}
		out := make([]map[string]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, keyView(k))
		}
		writeJSON(w, map[string]any{"keys": out})
	case http.MethodPost:
		var req struct{ Name string }
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, jsonErr("请求格式错误"), 400)
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = "默认密钥"
		}
		if utf8.RuneCountInString(name) > 40 {
			http.Error(w, jsonErr("名称最多 40 个字符"), 400)
			return
		}
		if n, err := s.st.CountAPIKeys(u.ID); err != nil {
			http.Error(w, jsonErr("读取失败"), 500)
			return
		} else if n >= 20 {
			http.Error(w, jsonErr("每个账户最多保留 20 个有效密钥"), 400)
			return
		}
		plain := gatewayKeyPrefix + auth.RandomHex(24)
		k := &store.APIKey{
			ID: "k" + newID() + newID(), UserID: u.ID, Name: name,
			Hash: hashAPIKey(plain), Prefix: plain[:len(gatewayKeyPrefix)+8],
			CreatedAt: time.Now().Unix(),
		}
		if err := s.st.CreateAPIKey(k); err != nil {
			http.Error(w, jsonErr("创建失败"), 500)
			return
		}
		log.Printf("user %s created API key %s (%s)", u.Name, k.ID, k.Prefix)
		// The plaintext key is intentionally absent from keyView: this response
		// is the only place it will ever exist.
		writeJSON(w, map[string]any{"key": plain, "id": k.ID, "name": k.Name,
			"prefix": k.Prefix, "created_at": k.CreatedAt})
	default:
		http.Error(w, jsonErr("只支持 GET / POST"), 405)
	}
}

func (s *Server) handleAPIKeyOp(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, jsonErr("只支持 DELETE"), 405)
		return
	}
	done, err := s.st.RevokeAPIKey(id, u.ID)
	if err != nil {
		http.Error(w, jsonErr("撤销失败"), 500)
		return
	}
	if !done {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

func keyView(k *store.APIKey) map[string]any {
	return map[string]any{
		"id": k.ID, "name": k.Name, "prefix": k.Prefix + "…",
		"created_at": k.CreatedAt, "last_used": k.LastUsed, "revoked": k.Revoked,
	}
}

func hashAPIKey(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:])
}

// ---- console: upstreams ----

func (s *Server) handleUpstreams(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		ups, err := s.st.UpstreamsByOwner(u.ID)
		if err != nil {
			http.Error(w, jsonErr("读取失败"), 500)
			return
		}
		out := make([]map[string]any, 0, len(ups))
		for _, up := range ups {
			out = append(out, s.upstreamView(up))
		}
		writeJSON(w, map[string]any{"upstreams": out})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, jsonErr("只支持 GET / POST"), 405)
		return
	}
	var req struct {
		Name        string  `json:"name"`
		BaseURL     string  `json:"base_url"`
		APIKey      string  `json:"api_key"`
		Models      string  `json:"models"`
		Price       float64 `json:"price"`
		MaxContext  int     `json:"max_context"`
		Caps        string  `json:"caps"`
		PriceOut    float64 `json:"price_out"`
		PriceCached float64 `json:"price_cached"`
	}
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, jsonErr("请求格式错误"), 400)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "我的模型上游"
	}
	if utf8.RuneCountInString(name) > 60 {
		http.Error(w, jsonErr("名称最多 60 个字符"), 400)
		return
	}
	base, err := validateUpstreamURL(req.BaseURL, s.policy.Public)
	if err != nil {
		http.Error(w, jsonErr(err.Error()), 400)
		return
	}
	models, err := normalizeModels(req.Models)
	if err != nil {
		http.Error(w, jsonErr(err.Error()), 400)
		return
	}
	caps, err := normalizeCaps(req.Caps)
	if err != nil {
		http.Error(w, jsonErr(err.Error()), 400)
		return
	}
	if req.Price <= 0 || req.Price > 10000 {
		http.Error(w, jsonErr("输入单价需在 0 ~ 10000 点/千 token 之间且大于 0"), 400)
		return
	}
	if req.PriceOut < 0 || req.PriceOut > 10000 || req.PriceCached < 0 || req.PriceCached > 10000 {
		http.Error(w, jsonErr("输出/缓存单价需在 0 ~ 10000 点/千 token 之间；0 表示与输入单价相同"), 400)
		return
	}
	maxContext := req.MaxContext
	if maxContext == 0 {
		maxContext = 32768
	}
	if maxContext < 1024 || maxContext > 1048576 {
		http.Error(w, jsonErr("最大上下文需在 1024 ~ 1048576 tokens 之间"), 400)
		return
	}
	enc, err := s.secrets.seal(strings.TrimSpace(req.APIKey))
	if err != nil {
		http.Error(w, jsonErr("保存上游密钥失败"), 500)
		return
	}
	up := &store.Upstream{
		ID: "m" + newID() + newID(), OwnerID: u.ID, Name: name, BaseURL: base,
		EncKey: enc, Models: models, Price: round4(req.Price),
		MaxContext: maxContext, Caps: caps,
		PriceOut: round4(req.PriceOut), PriceCached: round4(req.PriceCached),
		Enabled: true, CreatedAt: time.Now().Unix(),
	}
	if err := s.st.CreateUpstream(up); err != nil {
		http.Error(w, jsonErr("保存失败"), 500)
		return
	}
	log.Printf("user %s registered model upstream %s (%s, %v)", u.Name, up.ID, up.BaseURL, up.Models)
	writeJSON(w, map[string]any{"ok": true, "upstream": s.upstreamView(up)})
}

func (s *Server) handleUpstreamOp(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/models/upstreams/")
	if rest == "" || strings.Contains(rest, "/") {
		http.NotFound(w, r)
		return
	}
	if rest == "probe" {
		if r.Method != http.MethodPost {
			http.Error(w, jsonErr("只支持 POST"), 405)
			return
		}
		var req struct {
			BaseURL string `json:"base_url"`
			APIKey  string `json:"api_key"`
		}
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, jsonErr("请求格式错误"), 400)
			return
		}
		base, err := validateUpstreamURL(req.BaseURL, s.policy.Public)
		if err != nil {
			http.Error(w, jsonErr(err.Error()), 400)
			return
		}
		t0 := time.Now()
		models, err := s.probeUpstream(r.Context(), base, strings.TrimSpace(req.APIKey))
		if err != nil {
			http.Error(w, jsonErr("探测失败："+err.Error()), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "models": models,
			"latency_ms": time.Since(t0).Milliseconds()})
		return
	}
	id := rest
	switch r.Method {
	case http.MethodDelete:
		done, err := s.st.DeleteUpstream(id, u.ID)
		if err != nil {
			http.Error(w, jsonErr("删除失败"), 500)
			return
		}
		if !done {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": id})
	case http.MethodPut:
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decodeJSON(r, &req); err != nil || req.Enabled == nil {
			http.Error(w, jsonErr("需要 {\"enabled\": true|false}"), 400)
			return
		}
		done, err := s.st.SetUpstreamEnabled(id, u.ID, *req.Enabled)
		if err != nil {
			http.Error(w, jsonErr("更新失败"), 500)
			return
		}
		if !done {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "enabled": *req.Enabled})
	default:
		http.Error(w, jsonErr("只支持 PUT / DELETE"), 405)
	}
}

func (s *Server) upstreamView(up *store.Upstream) map[string]any {
	key, err := s.secrets.open(up.EncKey)
	hasKey, keyState := key != "", "ok"
	if err != nil {
		hasKey, keyState = up.EncKey != "", "error"
	}
	return map[string]any{
		"id": up.ID, "name": up.Name, "base_url": up.BaseURL, "models": up.Models,
		"price": up.Price, "price_out": up.PriceOut, "price_cached": up.PriceCached,
		"max_context": up.MaxContext, "caps": up.Caps,
		"enabled": up.Enabled, "created_at": up.CreatedAt,
		"has_key": hasKey, "key_state": keyState, "health": s.gateway.view(up.ID),
	}
}

func (s *Server) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	writeJSON(w, map[string]any{"models": s.modelCatalog(),
		"provider_share": gatewayProviderShare})
}

func normalizeModels(raw string) ([]string, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == ' ' || r == '\t'
	})
	seen := map[string]bool{}
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[strings.ToLower(p)] {
			continue
		}
		if len(p) > 120 {
			return nil, fmt.Errorf("模型名 %q 过长", p)
		}
		for _, r := range p {
			if !(r == '.' || r == '-' || r == '_' || r == '/' || r == ':' ||
				unicode.IsLetter(r) || unicode.IsDigit(r)) {
				return nil, fmt.Errorf("模型名 %q 含有不支持的字符", p)
			}
		}
		seen[strings.ToLower(p)] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("请至少填写一个模型名")
	}
	if len(out) > 20 {
		return nil, errors.New("一个上游最多登记 20 个模型")
	}
	return out, nil
}

// upstreamCaps is the routing contract a provider can claim. "chat" is always
// present; everything else is opt-in because a wrong claim sends work to a
// server that cannot finish it.
var upstreamCaps = []string{
	"chat", "tools", "json_object", "json_schema", "regex",
	"vision", "reasoning", "deterministic", "prefix_cache",
}

var capAliases = map[string]string{
	"batch_invariant": "deterministic",
	"cache":           "prefix_cache",
	"json":            "json_object",
	"grammar":         "regex",
}

func normalizeCaps(raw string) ([]string, error) {
	parts := strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == ' ' || r == '\t'
	})
	out := []string{"chat"}
	for _, p := range parts {
		if p == "" || p == "chat" {
			continue
		}
		if alias, ok := capAliases[p]; ok {
			p = alias
		}
		if !has(upstreamCaps, p) {
			return nil, fmt.Errorf("不支持的能力 %q；可用：%s", p,
				strings.Join(upstreamCaps[1:], ", "))
		}
		if !has(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// requiredCaps derives routing requirements from the request body. Absent
// capabilities are not sent to an upstream that did not claim them.
func requiredCaps(body map[string]any) []string {
	var out []string
	add := func(c string) {
		if !has(out, c) {
			out = append(out, c)
		}
	}
	if v, ok := body["tools"].([]any); ok && len(v) > 0 {
		add("tools")
	}
	if v, ok := body["functions"].([]any); ok && len(v) > 0 {
		add("tools")
	}
	switch structuredKind(body) {
	case "json_object":
		add("json_object")
	case "json_schema":
		add("json_schema")
	case "regex":
		add("regex")
	}
	if hasImagePart(body["messages"]) {
		add("vision")
	}
	if deterministicRequested(body) {
		add("deterministic")
	}
	return out
}

func structuredKind(body map[string]any) string {
	rf, _ := body["response_format"].(map[string]any)
	if rf == nil {
		return ""
	}
	t, _ := rf["type"].(string)
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "json_object":
		return "json_object"
	case "json_schema":
		return "json_schema"
	case "regex", "grammar":
		return "regex"
	default:
		return ""
	}
}

func deterministicRequested(body map[string]any) bool {
	if v, ok := body["starstack_deterministic"].(bool); ok && v {
		return true
	}
	_, hasSeed := body["seed"]
	return hasSeed
}

func hasImagePart(v any) bool {
	msgs, _ := v.([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		parts, _ := msg["content"].([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			if t, _ := part["type"].(string); t == "image_url" || t == "image" {
				return true
			}
		}
	}
	return false
}

// stickyKey is an explicit conversation id. We do not infer one from message
// content: two buyers with the same prompt should still be free to route to
// different providers.
func stickyKey(r *http.Request, body map[string]any) string {
	key := strings.TrimSpace(r.Header.Get("X-Starstack-Session"))
	if key == "" {
		key, _ = body["prompt_cache_key"].(string)
		key = strings.TrimSpace(key)
	}
	if len(key) > 120 {
		key = key[:120]
	}
	return strings.ToLower(key)
}

// validateUpstreamURL blocks schemes and destinations that would turn the
// broker into a request proxy. In public mode private/link-local targets are
// refused; loopback stays allowed because an operator may host a model server
// next to the broker, and the gateway only ever appends fixed OpenAI paths.
func validateUpstreamURL(raw string, public bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("请填写上游地址，例如 http://127.0.0.1:8080")
	}
	if len(raw) > 500 {
		return "", errors.New("上游地址过长")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("上游地址格式错误")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("上游地址只支持 http:// 或 https://")
	}
	if u.User != nil {
		return "", errors.New("上游地址不能内嵌用户名密码，请使用独立的 API Key 字段")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("上游地址缺少主机名")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("上游端口无效")
		}
	}
	if public {
		if host == "metadata.google.internal" || host == "metadata" {
			return "", errors.New("该上游地址不被允许")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return "", errors.New("无法解析上游主机名")
		}
		for _, ip := range ips {
			if blockedUpstreamIP(ip.IP) {
				return "", errors.New("公网部署不允许把上游指向内网、链路本地或保留地址")
			}
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func blockedUpstreamIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast()
}

func joinAPI(base, endpoint string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(strings.ToLower(base), "/v1") {
		return base + endpoint
	}
	return base + "/v1" + endpoint
}

func (s *Server) probeUpstream(ctx context.Context, base, key string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinAPI(base, "/models"), nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := s.gateway.probe.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, clip(strings.TrimSpace(string(body)), 200))
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, errors.New("上游没有返回 OpenAI 兼容的 /v1/models 列表")
	}
	models := make([]string, 0, len(doc.Data))
	for _, m := range doc.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	if len(models) == 0 {
		return nil, errors.New("上游 /v1/models 返回了空列表")
	}
	return models, nil
}

// ---- public OpenAI-compatible API ----

func (s *Server) gatewayUser(w http.ResponseWriter, r *http.Request) (*store.User, *store.APIKey, bool) {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(raw) >= 7 && strings.EqualFold(raw[:7], "bearer ") {
		raw = strings.TrimSpace(raw[7:])
	} else {
		raw = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	if raw == "" {
		oaiError(w, http.StatusUnauthorized, "invalid_api_key", "invalid_request_error",
			"缺少 API Key。请带上 Authorization: Bearer sk-ss-...")
		return nil, nil, false
	}
	k, err := s.st.APIKeyByHash(hashAPIKey(raw))
	if errors.Is(err, store.ErrNotFound) || (k != nil && k.Revoked) {
		oaiError(w, http.StatusUnauthorized, "invalid_api_key", "invalid_request_error", "API Key 无效或已撤销")
		return nil, nil, false
	}
	if err != nil {
		oaiError(w, http.StatusInternalServerError, "server_error", "server_error", "读取密钥失败")
		return nil, nil, false
	}
	u, err := s.st.User(k.UserID)
	if err != nil {
		oaiError(w, http.StatusUnauthorized, "invalid_api_key", "invalid_request_error", "密钥所属账户不存在")
		return nil, nil, false
	}
	s.st.TouchAPIKey(k.ID)
	return u, k, true
}

func (s *Server) handleV1Models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		oaiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "invalid_request_error", "只支持 GET")
		return
	}
	if _, _, ok := s.gatewayUser(w, r); !ok {
		return
	}
	models := s.modelCatalog()
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id": m.ID, "object": "model", "created": 0, "owned_by": "starstack",
			"starstack_price_per_1k": m.PricePer1K, "starstack_providers": m.Providers,
			"starstack_max_context": m.MaxContext, "starstack_capabilities": m.Capabilities,
		})
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

type tokenUsage struct {
	Prompt     int
	Completion int
	Cached     int // prompt tokens served from the provider's prefix cache
	Reasoning  int // subset of Completion, reported for visibility only
}

func (u tokenUsage) total() int { return u.Prompt + u.Completion }

func (s *Server) handleV1Chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oaiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "invalid_request_error", "只支持 POST")
		return
	}
	u, _, ok := s.gatewayUser(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, gatewayMaxBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		oaiError(w, http.StatusRequestEntityTooLarge, "request_too_large", "invalid_request_error",
			"请求体过大或读取失败（上限 4 MiB）")
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		oaiError(w, http.StatusBadRequest, "invalid_json", "invalid_request_error", "请求体不是合法 JSON")
		return
	}
	model, _ := body["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		oaiError(w, http.StatusBadRequest, "missing_model", "invalid_request_error", "缺少 model")
		return
	}
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) == 0 {
		oaiError(w, http.StatusBadRequest, "missing_messages", "invalid_request_error", "messages 必须是非空数组")
		return
	}
	stream, _ := body["stream"].(bool)

	maxTok, set, err := intField(body, "max_tokens", "max_completion_tokens")
	if err != nil {
		oaiError(w, http.StatusBadRequest, "invalid_max_tokens", "invalid_request_error", err.Error())
		return
	}
	if !set || maxTok <= 0 {
		maxTok = gatewayDefaultTokens
		body["max_tokens"] = maxTok
	}
	if maxTok > gatewayMaxTokens {
		oaiError(w, http.StatusBadRequest, "max_tokens_too_large", "invalid_request_error",
			fmt.Sprintf("max_tokens 最大 %d", gatewayMaxTokens))
		return
	}
	if stream {
		opts, _ := body["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		body["stream_options"] = opts
	}
	// Deterministic requests must not be silently answered by a provider that
	// only promises best-effort sampling. Pinning temperature/seed here also
	// makes the output hash meaningful across calls.
	if v, ok := body["starstack_deterministic"].(bool); ok && v {
		body["temperature"] = 0
		if _, ok := body["seed"]; !ok {
			body["seed"] = 1
		}
	}

	// Prompt tokens are estimated before any upstream is contacted: the hold
	// must be sized from the request, not from a provider's later claim.
	promptEst := estimateAnyTokens(body["messages"])
	for _, k := range []string{"tools", "functions"} {
		if v, ok := body[k]; ok {
			promptEst += estimateAnyTokens(v)
		}
	}
	promptEst += 8
	if promptEst > gatewayMaxPromptTokens {
		oaiError(w, http.StatusBadRequest, "prompt_too_long", "invalid_request_error",
			fmt.Sprintf("提示词约 %d tokens，超过上限 %d", promptEst, gatewayMaxPromptTokens))
		return
	}

	need := routeNeed{PromptTokens: promptEst, MaxTokens: maxTok,
		Caps: requiredCaps(body), Sticky: stickyKey(r, body)}
	routes, served := s.routesFor(model, need)
	if len(routes) == 0 {
		if served == 0 {
			oaiError(w, http.StatusNotFound, "model_not_found", "invalid_request_error",
				"没有可用上游提供模型 "+model+"；请让上游方在控制台登记该模型")
			return
		}
		oaiError(w, http.StatusBadRequest, "no_capable_upstream", "invalid_request_error",
			fmt.Sprintf("有 %d 家上游提供 %s，但没有一家满足本次请求（上下文约 %d tokens，需要能力：%s）",
				served, model, promptEst, strings.Join(need.Caps, ", ")))
		return
	}
	healthy := 0
	for _, rt := range routes {
		if !s.gateway.cooling(rt.up.ID) {
			healthy++
		}
	}
	if healthy == 0 {
		oaiError(w, http.StatusServiceUnavailable, "upstream_busy", "server_error",
			"该模型的上游都在冷却中（最近连续失败），请稍后重试")
		return
	}
	// Reserve against the dearest candidate at its own three rates, so a
	// failover onto a pricier or output-heavy upstream cannot overdraw.
	reserve := 0.0
	for _, rt := range routes {
		if r := routeReserve(rt.up, promptEst, maxTok); r > reserve {
			reserve = r
		}
	}
	if reserve < 0.0001 {
		reserve = 0.0001
	}
	bal, err := s.st.Balance(u.ID)
	if err != nil {
		oaiError(w, http.StatusInternalServerError, "balance_error", "server_error", "读取余额失败")
		return
	}
	if bal < reserve {
		oaiError(w, http.StatusPaymentRequired, "insufficient_quota", "invalid_request_error",
			fmt.Sprintf("余额不足：本次最多预扣 %.4f 点，当前余额 %.4f 点", reserve, bal))
		return
	}

	reqID := "req_" + newID() + newID()
	if bal, err := s.st.AddCredits(u.ID, -reserve); err != nil {
		oaiError(w, http.StatusInternalServerError, "balance_error", "server_error", "预扣失败")
		return
	} else {
		s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: store.Hold, UserID: u.ID,
			Amount: -reserve, Balance: bal, Note: "模型调用预扣 · " + model + " · " + reqID})
	}

	upBody, err := json.Marshal(body)
	if err != nil {
		s.gatewayRefund(u, reqID, reserve, "请求序列化失败")
		oaiError(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", "请求无法序列化")
		return
	}

	structKind := structuredKind(body)
	var lastErr error
	for _, rt := range routes {
		if s.gateway.cooling(rt.up.ID) {
			continue
		}
		res, err := s.callUpstream(r.Context(), rt.up, upBody)
		if err != nil {
			lastErr = err
			s.gateway.fail(rt.up.ID, err)
			log.Printf("gateway %s upstream %s failed: %v", reqID, rt.up.ID, err)
			continue
		}
		if res.StatusCode != http.StatusOK {
			bodyErr, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
			res.Body.Close()
			if res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests {
				lastErr = fmt.Errorf("HTTP %d", res.StatusCode)
				s.gateway.fail(rt.up.ID, lastErr)
				continue
			}
			// The buyer's request is invalid for this upstream, not the
			// upstream's fault: refund and relay without cooling it down.
			s.gatewayRefund(u, reqID, reserve, "上游拒绝请求，未产生用量")
			s.gateway.ok(rt.up.ID)
			relayUpstreamError(w, res.StatusCode, bodyErr)
			return
		}

		if stream {
			// Some OpenAI-compatible servers ignore stream=true and answer with a
			// normal JSON body. Relaying that as if it were SSE would hand the
			// client a stream that never parses, so detect it before any header is
			// written and settle it like a non-streaming call.
			if !strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "text/event-stream") {
				bodyOut, readErr := io.ReadAll(io.LimitReader(res.Body, gatewayMaxResponse))
				res.Body.Close()
				if readErr != nil || !json.Valid(bodyOut) {
					lastErr = errors.New("上游忽略 stream 且响应不可读")
					s.gateway.fail(rt.up.ID, lastErr)
					continue
				}
				if !structuredOutputOK(bodyOut, structKind) {
					lastErr = errors.New("上游返回的结构化输出不是合法 JSON")
					s.gateway.fail(rt.up.ID, lastErr)
					log.Printf("gateway %s upstream %s returned invalid structured output", reqID, rt.up.ID)
					continue
				}
				us := usageFromResponse(bodyOut, promptEst)
				s.gateway.ok(rt.up.ID)
				s.gateway.pin(need.Sticky, rt.up.ID)
				s.settleGateway(u, rt.up, model, reqID, us, reserve, promptEst)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Starstack-Request-Id", reqID)
				w.Header().Set("X-Starstack-Output-SHA256", outputHash(bodyOut))
				w.WriteHeader(http.StatusOK)
				w.Write(bodyOut)
				return
			}
			started, us := s.streamUpstream(w, res, reqID)
			if !started {
				lastErr = errors.New("上游在返回首个事件前断开")
				s.gateway.fail(rt.up.ID, lastErr)
				continue
			}
			s.gateway.ok(rt.up.ID)
			s.gateway.pin(need.Sticky, rt.up.ID)
			s.settleGateway(u, rt.up, model, reqID, us, reserve, promptEst)
			return
		}

		bodyOut, err := io.ReadAll(io.LimitReader(res.Body, gatewayMaxResponse))
		res.Body.Close()
		if err != nil || !json.Valid(bodyOut) {
			lastErr = errors.New("上游响应不完整或不是 JSON")
			s.gateway.fail(rt.up.ID, lastErr)
			continue
		}
		if !structuredOutputOK(bodyOut, structKind) {
			lastErr = errors.New("上游返回的结构化输出不是合法 JSON")
			s.gateway.fail(rt.up.ID, lastErr)
			log.Printf("gateway %s upstream %s returned invalid structured output", reqID, rt.up.ID)
			continue
		}
		us := usageFromResponse(bodyOut, promptEst)
		s.gateway.ok(rt.up.ID)
		s.gateway.pin(need.Sticky, rt.up.ID)
		s.settleGateway(u, rt.up, model, reqID, us, reserve, promptEst)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Starstack-Request-Id", reqID)
		w.Header().Set("X-Starstack-Output-SHA256", outputHash(bodyOut))
		w.WriteHeader(http.StatusOK)
		w.Write(bodyOut)
		return
	}

	s.gatewayRefund(u, reqID, reserve, "所有上游均不可用，未产生用量")
	if lastErr != nil {
		log.Printf("gateway %s all upstreams failed: %v", reqID, lastErr)
	}
	oaiError(w, http.StatusBadGateway, "upstream_unavailable", "server_error",
		"所有提供该模型的上游都暂时不可用，请稍后重试")
}

// callUpstream creates the outgoing request. The resolved model name is kept in
// the body exactly as the buyer sent it: providers advertise the same ids they
// serve, and rewriting casing here would hide a bad registration.
func (s *Server) callUpstream(ctx context.Context, up *store.Upstream, body []byte) (*http.Response, error) {
	key, err := s.secrets.open(up.EncKey)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, gatewayTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinAPI(up.BaseURL, "/chat/completions"), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "starstack-gateway/1")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := s.gateway.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	// The cancel runs when the response body is closed by the caller; wrapping
	// it here keeps the upstream connection alive for a streaming response.
	res.Body = &cancelOnClose{ReadCloser: res.Body, cancel: cancel}
	return res, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func (s *Server) settleGateway(u *store.User, up *store.Upstream, model, reqID string, us tokenUsage, reserve float64, promptEst int) {
	if us.Prompt <= 0 {
		us.Prompt = promptEst
	}
	cost := routeCost(up, us.Prompt, us.Cached, us.Completion)
	delta := round4(reserve - cost)
	detail := fmt.Sprintf("输入 %d（缓存 %d）输出 %d", us.Prompt, us.Cached, us.Completion)
	if delta > 0 {
		if bal, err := s.st.AddCredits(u.ID, delta); err == nil {
			s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: store.Refund, UserID: u.ID,
				Amount: delta, Balance: bal,
				Note: fmt.Sprintf("模型 %s %s，差额退还 · %s", model, detail, reqID)})
		}
	} else if delta < 0 {
		if bal, err := s.st.AddCredits(u.ID, delta); err == nil {
			s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: store.Settle, UserID: u.ID,
				Amount: delta, Balance: bal,
				Note: fmt.Sprintf("模型 %s %s，超出预扣补扣 · %s", model, detail, reqID)})
		}
	}
	pay := round4(cost * gatewayProviderShare)
	if pay > 0 && up.OwnerID != "" {
		if bal, err := s.st.AddCredits(up.OwnerID, pay); err == nil {
			s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: store.Earn, UserID: up.OwnerID,
				Amount: pay, Balance: bal,
				Note: fmt.Sprintf("模型 %s · %s · 上游 %s · %s", model, detail, up.Name, reqID)})
		}
	}
	log.Printf("gateway %s user=%s model=%s upstream=%s prompt=%d cached=%d completion=%d cost=%.4f",
		reqID, u.Name, model, up.ID, us.Prompt, us.Cached, us.Completion, cost)
}

func (s *Server) gatewayRefund(u *store.User, reqID string, reserve float64, why string) {
	if reserve <= 0 {
		return
	}
	if bal, err := s.st.AddCredits(u.ID, reserve); err == nil {
		s.st.Ledger(&store.Entry{TS: time.Now().Unix(), Kind: store.Refund, UserID: u.ID,
			Amount: reserve, Balance: bal, Note: "模型调用预扣退还 · " + why + " · " + reqID})
	}
}

// streamUpstream relays an SSE response line by line. Headers are only written
// after the first event arrives, so an upstream that dies immediately can still
// be failed over. Once bytes reach the buyer, metering is based on what was
// actually delivered.
func (s *Server) streamUpstream(w http.ResponseWriter, res *http.Response, reqID string) (started bool, us tokenUsage) {
	defer res.Body.Close()
	fl, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(res.Body, 64<<10)
	var content strings.Builder
	var seen *tokenUsage
	sawDone := false
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			if !started {
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				w.Header().Set("Cache-Control", "no-cache, no-transform")
				w.Header().Set("X-Accel-Buffering", "no")
				w.Header().Set("X-Starstack-Request-Id", reqID)
				w.WriteHeader(http.StatusOK)
				started = true
			}
			if p, ok := strings.CutPrefix(line, "data:"); ok {
				p = strings.TrimSpace(p)
				if p == "[DONE]" {
					sawDone = true
				} else if p != "" {
					parseStreamChunk(p, &content, &seen)
				}
			}
			if _, werr := io.WriteString(w, line); werr != nil {
				break
			}
			if line == "\n" || line == "\r\n" {
				if fl != nil {
					fl.Flush()
				}
			}
		}
		if err != nil {
			break
		}
	}
	if started && !sawDone {
		io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
	if seen != nil {
		us = *seen
	}
	if us.Completion <= 0 {
		us.Completion = estimateTextTokens(content.String())
	}
	return started, us
}

func parseStreamChunk(payload string, content *strings.Builder, seen **tokenUsage) {
	var doc struct {
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return
	}
	if doc.Usage != nil && (doc.Usage.PromptTokens > 0 || doc.Usage.CompletionTokens > 0) {
		us := tokenUsage{Prompt: doc.Usage.PromptTokens, Completion: doc.Usage.CompletionTokens}
		if doc.Usage.PromptTokensDetails != nil {
			us.Cached = doc.Usage.PromptTokensDetails.CachedTokens
		}
		if doc.Usage.CompletionTokensDetails != nil {
			us.Reasoning = doc.Usage.CompletionTokensDetails.ReasoningTokens
		}
		*seen = &us
	}
	for _, c := range doc.Choices {
		content.WriteString(c.Delta.Content)
		content.WriteString(c.Delta.ReasoningContent)
		content.WriteString(c.Message.Content)
		for _, tc := range c.Delta.ToolCalls {
			content.WriteString(tc.Function.Arguments)
		}
	}
}

func usageFromResponse(body []byte, promptEst int) tokenUsage {
	var doc struct {
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.Unmarshal(body, &doc)
	if doc.Usage != nil && (doc.Usage.PromptTokens > 0 || doc.Usage.CompletionTokens > 0) {
		us := tokenUsage{Prompt: doc.Usage.PromptTokens, Completion: doc.Usage.CompletionTokens}
		if doc.Usage.PromptTokensDetails != nil {
			us.Cached = doc.Usage.PromptTokensDetails.CachedTokens
		}
		if doc.Usage.CompletionTokensDetails != nil {
			us.Reasoning = doc.Usage.CompletionTokensDetails.ReasoningTokens
		}
		if us.Prompt <= 0 {
			us.Prompt = promptEst
		}
		return us
	}
	var text strings.Builder
	for _, c := range doc.Choices {
		text.WriteString(c.Message.Content)
		text.WriteString(c.Message.ReasoningContent)
		for _, tc := range c.Message.ToolCalls {
			text.WriteString(tc.Function.Arguments)
		}
	}
	return tokenUsage{Prompt: promptEst, Completion: estimateTextTokens(text.String())}
}

// ---- token estimation ----

// estimateAnyTokens walks the JSON value and estimates how many tokens its
// strings represent. ASCII text averages ~4 bytes/token; CJK is close to one
// token per character, and over-estimating is the safe direction for a hold.
func estimateAnyTokens(v any) int {
	switch t := v.(type) {
	case string:
		return estimateTextTokens(t)
	case []any:
		n := 0
		for _, x := range t {
			n += estimateAnyTokens(x)
		}
		return n
	case map[string]any:
		n := 0
		for k, x := range t {
			n += estimateTextTokens(k) + estimateAnyTokens(x)
		}
		return n
	default:
		return 0
	}
}

func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	ascii, wide := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	return (ascii+3)/4 + wide
}

// ---- OpenAI error helpers ----

func oaiError(w http.ResponseWriter, status int, code, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"message": msg, "type": typ, "param": nil, "code": code,
	}})
}

func relayUpstreamError(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	if json.Valid(body) && len(bytes.TrimSpace(body)) > 0 {
		w.WriteHeader(status)
		w.Write(body)
		return
	}
	oaiError(w, http.StatusBadGateway, "upstream_rejected", "invalid_request_error",
		"上游拒绝了请求："+clip(strings.TrimSpace(string(body)), 300))
}

// structuredOutputOK catches the cheapest failure mode: a provider that claims
// JSON support but returns prose or truncated JSON. Full schema validation is
// not attempted; syntax plus a top-level object is what the gateway can check
// without knowing the provider's decoder.
func structuredOutputOK(body []byte, kind string) bool {
	if kind != "json_object" && kind != "json_schema" {
		return true
	}
	var doc struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &doc) != nil || len(doc.Choices) == 0 {
		return false
	}
	var content string
	if err := json.Unmarshal(doc.Choices[0].Message.Content, &content); err != nil {
		return false
	}
	var v any
	if json.Unmarshal([]byte(strings.TrimSpace(content)), &v) != nil {
		return false
	}
	_, ok := v.(map[string]any)
	return ok
}

func outputHash(body []byte) string {
	var doc struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.Unmarshal(body, &doc)
	var b strings.Builder
	for _, c := range doc.Choices {
		b.WriteString(c.Message.Content)
		b.WriteString(c.Message.ReasoningContent)
		for _, tc := range c.Message.ToolCalls {
			b.WriteString(tc.Function.Arguments)
		}
		b.WriteString("\x00")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func intField(m map[string]any, keys ...string) (int, bool, error) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				if n != math.Trunc(n) {
					return 0, true, fmt.Errorf("%s 必须是整数", k)
				}
				return int(n), true, nil
			case int:
				return n, true, nil
			default:
				return 0, true, fmt.Errorf("%s 必须是整数", k)
			}
		}
	}
	return 0, false, nil
}

func ceil4(f float64) float64 { return math.Ceil(f*10000) / 10000 }
