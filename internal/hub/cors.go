package hub

import (
	"net/http"
	"strings"
)

// corsWrap lets the Android app (bundled assets on https://localhost) and other
// non-browser clients call /api without a proxy. Sessions in those clients ride
// on Authorization: Bearer, not on the SameSite=Lax cookie, so allowing every
// origin here does not expose a cookie-based CSRF surface: credentials are never
// attached implicitly, and the allow-list deliberately omits Allow-Credentials.
func corsWrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/v1/") &&
			!strings.HasPrefix(r.URL.Path, "/download/") && !strings.HasPrefix(r.URL.Path, "/launch/") &&
			!strings.HasPrefix(r.URL.Path, "/dl/") {
			h.ServeHTTP(w, r)
			return
		}
		oh := w.Header()
		oh.Set("Access-Control-Allow-Origin", "*")
		oh.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		oh.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept")
		oh.Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
