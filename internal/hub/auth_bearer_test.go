package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Mobile clients keep the session token in the login response body and replay
// it as Authorization: Bearer, because a SameSite=Lax cookie is never sent from
// the app's own origin.
func TestBearerSessionAndCORS(t *testing.T) {
	s := newTestServer(t, DefaultPolicy())
	ts := httptest.NewServer(s.Handlers())
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"name": "mobile", "password": "pass12345"})
	res, err := http.Post(ts.URL+"/api/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("register: %d", res.StatusCode)
	}
	var reg struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&reg); err != nil {
		t.Fatal(err)
	}
	if reg.Token == "" {
		t.Fatal("register response carries no token")
	}

	me := func(auth string) map[string]any {
		req, _ := http.NewRequest("GET", ts.URL+"/api/me", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var out map[string]any
		json.NewDecoder(r.Body).Decode(&out)
		return out
	}

	if got := me(reg.Token); got["signed_in"] != true || got["name"] != "mobile" {
		t.Fatalf("bearer did not authenticate: %v", got)
	}
	if got := me(""); got["signed_in"] != false {
		t.Fatalf("anonymous request looked signed in: %v", got)
	}

	// Preflight for the Capacitor origin.
	req, _ := http.NewRequest("OPTIONS", ts.URL+"/api/me", nil)
	req.Header.Set("Origin", "https://localhost")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight: %d", r.StatusCode)
	}
	if r.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("missing allow-origin: %v", r.Header)
	}

	// Logout must revoke the bearer token, not only the cookie.
	req, _ = http.NewRequest("POST", ts.URL+"/api/auth/logout", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer "+reg.Token)
	if r, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
	}
	if got := me(reg.Token); got["signed_in"] != false {
		t.Fatalf("token still valid after logout: %v", got)
	}
}
