package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// registerAndCookie creates an account through the real handler so the test
// exercises the same signup credit and session path the console uses.
func registerAndCookie(t *testing.T, base, name string) (*http.Cookie, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name, "password": "testpass123"})
	res, err := http.Post(base+"/api/auth/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("register %s: HTTP %d %s", name, res.StatusCode, b)
	}
	var out struct {
		ID string `json:"id"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	cs := res.Cookies()
	if len(cs) == 0 {
		t.Fatalf("register %s: no session cookie", name)
	}
	return cs[0], out.ID
}

func withCookie(req *http.Request, c *http.Cookie) *http.Request {
	req.AddCookie(c)
	return req
}

func postJSON(t *testing.T, req *http.Request, v any, out any) int {
	t.Helper()
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", req.Method, req.URL, body, err)
		}
	}
	return res.StatusCode
}

func createKey(t *testing.T, base string, cookie *http.Cookie, name string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/api/keys",
		strings.NewReader(fmt.Sprintf(`{"name":%q}`, name)))
	var out struct {
		Key string `json:"key"`
	}
	if code := postJSON(t, withCookie(req, cookie), nil, &out); code != 200 || !strings.HasPrefix(out.Key, gatewayKeyPrefix) {
		t.Fatalf("create key: HTTP %d key=%q", code, out.Key)
	}
	return out.Key
}

func createUpstream(t *testing.T, base string, cookie *http.Cookie, name, upstreamURL, models string, price float64) {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"base_url":%q,"models":%q,"price":%g}`, name, upstreamURL, models, price)
	createUpstreamJSON(t, base, cookie, body)
}

func createUpstreamJSON(t *testing.T, base string, cookie *http.Cookie, body string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/api/models/upstreams", strings.NewReader(body))
	var out map[string]any
	if code := postJSON(t, withCookie(req, cookie), nil, &out); code != 200 {
		t.Fatalf("create upstream: HTTP %d %v", code, out)
	}
	return out
}

func chatWithSession(t *testing.T, base, key, session, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Starstack-Session", session)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func chat(t *testing.T, base, key, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestSecretBoxRoundTrip(t *testing.T) {
	box := loadSecretBox(t.TempDir())
	enc, err := box.seal("sk-provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "provider") {
		t.Fatal("ciphertext must not contain the plaintext")
	}
	got, err := box.open(enc)
	if err != nil || got != "sk-provider-secret" {
		t.Fatalf("open = %q, %v", got, err)
	}
	if enc, err := box.seal(""); err != nil || enc != "" {
		t.Fatalf("empty secret should stay empty: %q %v", enc, err)
	}
	// a different key file must not decrypt the old ciphertext
	if got, err := loadSecretBox(t.TempDir()).open(enc); err == nil || got != "" {
		t.Fatal("ciphertext opened with the wrong key")
	}
}

func TestValidateUpstreamURL(t *testing.T) {
	if _, err := validateUpstreamURL("file:///etc/passwd", false); err == nil {
		t.Fatal("file scheme must be refused")
	}
	if _, err := validateUpstreamURL("http://user:pass@host:8080", false); err == nil {
		t.Fatal("embedded credentials must be refused")
	}
	if _, err := validateUpstreamURL("http://127.0.0.1:8080", false); err != nil {
		t.Fatalf("loopback must work in the demo profile: %v", err)
	}
	if _, err := validateUpstreamURL("http://10.1.2.3:8080/v1/", true); err == nil {
		t.Fatal("private address must be refused in public mode")
	}
	if _, err := validateUpstreamURL("http://169.254.169.254", true); err == nil {
		t.Fatal("link-local metadata address must be refused in public mode")
	}
	if _, err := validateUpstreamURL("http://169.254.169.254", false); err != nil {
		t.Fatalf("the demo profile should not require public DNS: %v", err)
	}
	got, err := validateUpstreamURL("http://example.com:8080/v1/?x=1", false)
	if err != nil || got != "http://example.com:8080/v1" {
		t.Fatalf("expected a normalized base URL, got %q %v", got, err)
	}
}

func TestGatewayChatSettlesBetweenBuyerAndProvider(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer up-secret" {
			t.Errorf("upstream did not receive its key: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion",
			"choices":[{"index":0,"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}`)
	}))
	defer upstream.Close()

	s := newTestServer(t, DefaultPolicy())
	broker := httptest.NewServer(s.Handlers())
	defer broker.Close()

	if _, err := validateUpstreamURL(upstream.URL, false); err != nil {
		t.Fatalf("test upstream URL rejected: %v", err)
	}
	providerCookie, providerID := registerAndCookie(t, broker.URL, "provider")
	buyerCookie, buyerID := registerAndCookie(t, broker.URL, "buyer")

	// register the upstream through the provider console with a secret key
	body := fmt.Sprintf(`{"name":"宿舍 4090","base_url":%q,"api_key":"up-secret","models":"qwen,llama-3","price":1.0}`, upstream.URL)
	req, _ := http.NewRequest(http.MethodPost, broker.URL+"/api/models/upstreams", strings.NewReader(body))
	if code := postJSON(t, withCookie(req, providerCookie), nil, nil); code != 200 {
		t.Fatalf("create upstream: HTTP %d", code)
	}

	// the provider key must be encrypted at rest
	ups, err := s.st.Upstreams()
	if err != nil || len(ups) != 1 {
		t.Fatalf("upstreams: %v %d", err, len(ups))
	}
	if ups[0].EncKey == "" || strings.Contains(ups[0].EncKey, "up-secret") {
		t.Fatalf("upstream key not sealed: %q", ups[0].EncKey)
	}

	key := createKey(t, broker.URL, buyerCookie, "测试")
	code, resp := chat(t, broker.URL, key,
		`{"model":"qwen","messages":[{"role":"user","content":"hello"}],"max_tokens":100}`)
	if code != 200 {
		t.Fatalf("chat: HTTP %d %s", code, resp)
	}
	if !strings.Contains(resp, "你好") {
		t.Fatalf("response not relayed: %s", resp)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("want exactly one upstream call, got %d", calls)
	}

	// prompt 12 + completion 34 at 1 点/1k = 0.046; provider gets 90%
	buyerBal, _ := s.st.Balance(buyerID)
	providerBal, _ := s.st.Balance(providerID)
	if diff := 100 - buyerBal; diff < 0.0459 || diff > 0.0461 {
		t.Fatalf("buyer charged %.4f, want 0.0460", diff)
	}
	if diff := providerBal - 100; diff < 0.0413 || diff > 0.0415 {
		t.Fatalf("provider earned %.4f, want 0.0414", diff)
	}

	// listed keys never reveal the secret again
	req, _ = http.NewRequest(http.MethodGet, broker.URL+"/api/keys", nil)
	var list struct {
		Keys []map[string]any `json:"keys"`
	}
	if code := postJSON(t, withCookie(req, buyerCookie), nil, &list); code != 200 || len(list.Keys) != 1 {
		t.Fatalf("list keys: HTTP %d %+v", code, list)
	}
	if p, _ := list.Keys[0]["prefix"].(string); !strings.HasPrefix(p, gatewayKeyPrefix) || strings.Contains(p, strings.TrimPrefix(key, gatewayKeyPrefix)) {
		t.Fatalf("prefix leaks the key: %v", list.Keys[0])
	}
	if _, ok := list.Keys[0]["key"]; ok {
		t.Fatal("full key returned by list")
	}
}

func TestGatewayStreamsAndMetersUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] != true {
			t.Errorf("stream flag not forwarded: %v", req["stream"])
		}
		opts, _ := req["stream_options"].(map[string]any)
		if opts == nil || opts["include_usage"] != true {
			t.Errorf("include_usage was not injected: %v", req["stream_options"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	s := newTestServer(t, DefaultPolicy())
	broker := httptest.NewServer(s.Handlers())
	defer broker.Close()
	providerCookie, providerID := registerAndCookie(t, broker.URL, "stream-provider")
	buyerCookie, buyerID := registerAndCookie(t, broker.URL, "stream-buyer")
	createUpstream(t, broker.URL, providerCookie, "local-vllm", upstream.URL, "qwen", 2.0)
	key := createKey(t, broker.URL, buyerCookie, "stream")

	code, resp := chat(t, broker.URL, key,
		`{"model":"qwen","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":50}`)
	if code != 200 {
		t.Fatalf("stream chat: HTTP %d %s", code, resp)
	}
	if !strings.Contains(resp, `"你"`) || !strings.Contains(resp, "[DONE]") {
		t.Fatalf("stream not relayed: %q", resp)
	}
	buyerBal, _ := s.st.Balance(buyerID)
	providerBal, _ := s.st.Balance(providerID)
	// 30 tokens at 2 点/1k = 0.06, provider 0.054
	if diff := 100 - buyerBal; diff < 0.0599 || diff > 0.0601 {
		t.Fatalf("stream buyer charged %.4f, want 0.0600", diff)
	}
	if diff := providerBal - 100; diff < 0.0539 || diff > 0.0541 {
		t.Fatalf("stream provider earned %.4f, want 0.0540", diff)
	}
}

func TestGatewayFailsOverAndCoolsBadUpstream(t *testing.T) {
	var badCalls, goodCalls int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&badCalls, 1)
		http.Error(w, "gpu on fire", http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&goodCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`)
	}))
	defer good.Close()

	s := newTestServer(t, DefaultPolicy())
	broker := httptest.NewServer(s.Handlers())
	defer broker.Close()
	providerCookie, _ := registerAndCookie(t, broker.URL, "pool-provider")
	buyerCookie, _ := registerAndCookie(t, broker.URL, "pool-buyer")
	createUpstream(t, broker.URL, providerCookie, "cheap-broken", bad.URL, "m", 0.5)
	createUpstream(t, broker.URL, providerCookie, "dear-working", good.URL, "m", 2.0)
	key := createKey(t, broker.URL, buyerCookie, "pool")

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	for i := 0; i < 2; i++ {
		if code, resp := chat(t, broker.URL, key, body); code != 200 || !strings.Contains(resp, "ok") {
			t.Fatalf("call %d: HTTP %d %s", i, code, resp)
		}
	}
	if atomic.LoadInt32(&badCalls) != 1 {
		t.Fatalf("broken upstream should be tried once then cooled down, got %d calls", badCalls)
	}
	if atomic.LoadInt32(&goodCalls) != 2 {
		t.Fatalf("working upstream should serve both calls, got %d", goodCalls)
	}

	// a buyer without an active key must not reach the upstreams
	if code, _ := chat(t, broker.URL, "sk-ss-nope", body); code != http.StatusUnauthorized {
		t.Fatalf("bad key: got HTTP %d, want 401", code)
	}
}

func TestGatewayCatalogListsCheapestPrice(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upB.Close()

	s := newTestServer(t, DefaultPolicy())
	broker := httptest.NewServer(s.Handlers())
	defer broker.Close()
	providerCookie, _ := registerAndCookie(t, broker.URL, "catalog-provider")
	buyerCookie, _ := registerAndCookie(t, broker.URL, "catalog-buyer")
	createUpstream(t, broker.URL, providerCookie, "a", upA.URL, "qwen,llama", 0.8)
	createUpstream(t, broker.URL, providerCookie, "b", upB.URL, "qwen", 0.3)

	req, _ := http.NewRequest(http.MethodGet, broker.URL+"/api/models/catalog", nil)
	var out struct {
		Models []catalogModel `json:"models"`
	}
	if code := postJSON(t, withCookie(req, buyerCookie), nil, &out); code != 200 {
		t.Fatalf("catalog: HTTP %d", code)
	}
	byID := map[string]catalogModel{}
	for _, m := range out.Models {
		byID[m.ID] = m
	}
	if byID["qwen"].Providers != 2 || byID["qwen"].PricePer1K != 0.3 {
		t.Fatalf("qwen catalog wrong: %+v", byID["qwen"])
	}
	if byID["llama"].Providers != 1 || byID["llama"].PricePer1K != 0.8 {
		t.Fatalf("llama catalog wrong: %+v", byID["llama"])
	}

	key := createKey(t, broker.URL, buyerCookie, "catalog")
	req, _ = http.NewRequest(http.MethodGet, broker.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var listing struct {
		Object string `json:"object"`
		Data   []struct {
			ID    string  `json:"id"`
			Price float64 `json:"starstack_price_per_1k"`
		} `json:"data"`
	}
	json.NewDecoder(res.Body).Decode(&listing)
	if listing.Object != "list" || len(listing.Data) != 2 {
		t.Fatalf("unexpected /v1/models: %+v", listing)
	}
}

// A cheap upstream is skipped when the prompt no longer fits its window or the
// request needs a capability it never claimed.
func TestGatewayRoutesByCapabilityAndContext(t *testing.T) {
	var smallCalls, bigCalls int32
	okBody := `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`
	small := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&smallCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okBody)
	}))
	defer small.Close()
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bigCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okBody)
	}))
	defer big.Close()

	s := newTestServer(t, DefaultPolicy())
	broker := httptest.NewServer(s.Handlers())
	defer broker.Close()
	providerCookie, _ := registerAndCookie(t, broker.URL, "cap-provider")
	buyerCookie, _ := registerAndCookie(t, broker.URL, "cap-buyer")
	createUpstreamJSON(t, broker.URL, providerCookie, fmt.Sprintf(
		`{"name":"tiny","base_url":%q,"models":"m","price":0.1,"max_context":4096,"caps":"chat"}`, small.URL))
	createUpstreamJSON(t, broker.URL, providerCookie, fmt.Sprintf(
		`{"name":"large","base_url":%q,"models":"m","price":1.0,"max_context":32768,"caps":"chat,tools,deterministic"}`, big.URL))
	key := createKey(t, broker.URL, buyerCookie, "cap")

	if code, resp := chat(t, broker.URL, key,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`); code != 200 {
		t.Fatalf("short chat: HTTP %d %s", code, resp)
	}
	if atomic.LoadInt32(&smallCalls) != 1 || atomic.LoadInt32(&bigCalls) != 0 {
		t.Fatalf("short chat should use the cheap upstream: small=%d big=%d", smallCalls, bigCalls)
	}

	longPrompt := strings.Repeat("a", 20000)
	if code, resp := chat(t, broker.URL, key, fmt.Sprintf(
		`{"model":"m","messages":[{"role":"user","content":%q}],"max_tokens":100}`, longPrompt)); code != 200 {
		t.Fatalf("long chat: HTTP %d %s", code, resp)
	}
	if atomic.LoadInt32(&bigCalls) != 1 {
		t.Fatalf("long prompt should exceed the small window and fail over: big=%d", bigCalls)
	}

	tools := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":100,
		"tools":[{"type":"function","function":{"name":"now","description":"当前时间","parameters":{"type":"object"}}}]}`
	if code, resp := chat(t, broker.URL, key, tools); code != 200 {
		t.Fatalf("tools chat: HTTP %d %s", code, resp)
	}
	if atomic.LoadInt32(&smallCalls) != 1 || atomic.LoadInt32(&bigCalls) != 2 {
		t.Fatalf("tools request should skip the chat-only upstream: small=%d big=%d", smallCalls, bigCalls)
	}
}

// Once a conversation warms one upstream's prefix, later turns must return to
// it even though routing would otherwise rotate between equal-priced peers.
func TestGatewaySessionAffinity(t *testing.T) {
	var aCalls, bCalls int32
	okBody := `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&aCalls, 1)
		io.WriteString(w, okBody)
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bCalls, 1)
		io.WriteString(w, okBody)
	}))
	defer upB.Close()

	s := newTestServer(t, DefaultPolicy())
	broker := httptest.NewServer(s.Handlers())
	defer broker.Close()
	providerCookie, _ := registerAndCookie(t, broker.URL, "sticky-provider")
	buyerCookie, _ := registerAndCookie(t, broker.URL, "sticky-buyer")
	createUpstream(t, broker.URL, providerCookie, "a", upA.URL, "m", 1.0)
	createUpstream(t, broker.URL, providerCookie, "b", upB.URL, "m", 1.0)
	key := createKey(t, broker.URL, buyerCookie, "sticky")

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	for i := 0; i < 3; i++ {
		if code, resp := chatWithSession(t, broker.URL, key, "conv-42", body); code != 200 {
			t.Fatalf("turn %d: HTTP %d %s", i, code, resp)
		}
	}
	a, b := atomic.LoadInt32(&aCalls), atomic.LoadInt32(&bCalls)
	if !(a == 3 && b == 0) && !(b == 3 && a == 0) {
		t.Fatalf("session should stick to one upstream, got a=%d b=%d", a, b)
	}
}

func TestGatewayChargesCachedAndOutputTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],
			"usage":{"prompt_tokens":120,"completion_tokens":10,
			"prompt_tokens_details":{"cached_tokens":100},
			"completion_tokens_details":{"reasoning_tokens":2}}}`)
	}))
	defer upstream.Close()

	s := newTestServer(t, DefaultPolicy())
	broker := httptest.NewServer(s.Handlers())
	defer broker.Close()
	providerCookie, providerID := registerAndCookie(t, broker.URL, "price-provider")
	buyerCookie, buyerID := registerAndCookie(t, broker.URL, "price-buyer")
	createUpstreamJSON(t, broker.URL, providerCookie, fmt.Sprintf(
		`{"name":"three-rate","base_url":%q,"models":"m","price":1.0,"price_out":2.0,"price_cached":0.25}`, upstream.URL))
	key := createKey(t, broker.URL, buyerCookie, "price")

	if code, resp := chat(t, broker.URL, key,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`); code != 200 {
		t.Fatalf("chat: HTTP %d %s", code, resp)
	}
	buyerBal, _ := s.st.Balance(buyerID)
	providerBal, _ := s.st.Balance(providerID)
	// 20 uncached input at 1.0 + 100 cached at 0.25 + 10 output at 2.0 = 0.065
	if diff := 100 - buyerBal; diff < 0.0649 || diff > 0.0651 {
		t.Fatalf("buyer charged %.4f, want 0.0650", diff)
	}
	if diff := providerBal - 100; diff < 0.0584 || diff > 0.0586 {
		t.Fatalf("provider earned %.4f, want 0.0585", diff)
	}
}

func TestGatewayDeterministicRouting(t *testing.T) {
	var plainCalls, detCalls int32
	var gotTemp, gotSeed any
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&plainCalls, 1)
		io.WriteString(w, `{"choices":[{"message":{"content":"plain"}}]}`)
	}))
	defer plain.Close()
	det := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&detCalls, 1)
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		gotTemp, gotSeed = req["temperature"], req["seed"]
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"same"}}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`)
	}))
	defer det.Close()

	s := newTestServer(t, DefaultPolicy())
	broker := httptest.NewServer(s.Handlers())
	defer broker.Close()
	providerCookie, _ := registerAndCookie(t, broker.URL, "det-provider")
	buyerCookie, _ := registerAndCookie(t, broker.URL, "det-buyer")
	createUpstreamJSON(t, broker.URL, providerCookie, fmt.Sprintf(
		`{"name":"plain","base_url":%q,"models":"m","price":0.1,"caps":"chat"}`, plain.URL))
	createUpstreamJSON(t, broker.URL, providerCookie, fmt.Sprintf(
		`{"name":"det","base_url":%q,"models":"m","price":1.0,"caps":"chat,deterministic"}`, det.URL))
	key := createKey(t, broker.URL, buyerCookie, "det")

	req, _ := http.NewRequest(http.MethodPost, broker.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"m","starstack_deterministic":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":10}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("deterministic chat: HTTP %d %s", res.StatusCode, out)
	}
	if atomic.LoadInt32(&plainCalls) != 0 || atomic.LoadInt32(&detCalls) != 1 {
		t.Fatalf("deterministic request must skip plain providers: plain=%d det=%d", plainCalls, detCalls)
	}
	if gotTemp != float64(0) || gotSeed != float64(1) {
		t.Fatalf("upstream did not receive pinned sampling: temperature=%v seed=%v", gotTemp, gotSeed)
	}
	if res.Header.Get("X-Starstack-Output-SHA256") == "" {
		t.Fatal("deterministic response is missing the output hash")
	}
}

func TestGatewayRejectsInvalidStructuredOutput(t *testing.T) {
	var badCalls, goodCalls int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&badCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"not json"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&goodCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer good.Close()

	s := newTestServer(t, DefaultPolicy())
	broker := httptest.NewServer(s.Handlers())
	defer broker.Close()
	providerCookie, _ := registerAndCookie(t, broker.URL, "json-provider")
	buyerCookie, _ := registerAndCookie(t, broker.URL, "json-buyer")
	createUpstreamJSON(t, broker.URL, providerCookie, fmt.Sprintf(
		`{"name":"broken-json","base_url":%q,"models":"m","price":0.1,"caps":"chat,json_object"}`, bad.URL))
	createUpstreamJSON(t, broker.URL, providerCookie, fmt.Sprintf(
		`{"name":"real-json","base_url":%q,"models":"m","price":1.0,"caps":"chat,json_object"}`, good.URL))
	key := createKey(t, broker.URL, buyerCookie, "json")

	body := `{"model":"m","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	for i := 0; i < 2; i++ {
		code, resp := chat(t, broker.URL, key, body)
		if code != 200 || !strings.Contains(resp, `\"ok\":true`) {
			t.Fatalf("call %d: HTTP %d %s", i, code, resp)
		}
	}
	if atomic.LoadInt32(&badCalls) != 1 {
		t.Fatalf("invalid JSON provider should be cooled after one try, got %d calls", badCalls)
	}
	if atomic.LoadInt32(&goodCalls) != 2 {
		t.Fatalf("valid provider should serve both calls, got %d", goodCalls)
	}
}
