package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := guard(ok, []string{"nas.home", ""})

	for _, tc := range []struct {
		name, method, host string
		headers            map[string]string
		want               int
	}{
		{"same-origin write", "POST", "127.0.0.1:8080", map[string]string{"Origin": "http://127.0.0.1:8080", "Sec-Fetch-Site": "same-origin"}, 200},
		{"no browser headers (curl)", "POST", "localhost:8080", nil, 200},
		{"cross-origin write", "POST", "127.0.0.1:8080", map[string]string{"Origin": "https://evil.example"}, 403},
		{"cross-site no-cors fetch", "DELETE", "127.0.0.1:8080", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"same-site but different origin", "PATCH", "127.0.0.1:8080", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"sandboxed frame", "POST", "127.0.0.1:8080", map[string]string{"Origin": "null"}, 403},
		{"other port", "POST", "127.0.0.1:8080", map[string]string{"Origin": "http://127.0.0.1:3000"}, 403},
		{"cross-site read is fine", "GET", "127.0.0.1:8080", map[string]string{"Sec-Fetch-Site": "cross-site"}, 200},
		// Vite's dev proxy keeps Host as localhost:5173, matching the page's origin.
		{"vite dev proxy", "POST", "localhost:5173", map[string]string{"Origin": "http://localhost:5173", "Sec-Fetch-Site": "same-origin"}, 200},
		{"DNS rebinding read", "GET", "evil.example:8080", nil, 421},
		{"DNS rebinding write", "POST", "evil.example:8080", map[string]string{"Origin": "http://evil.example:8080"}, 421},
		{"LAN IP", "GET", "192.168.1.5:8080", nil, 200},
		{"IPv6 loopback", "GET", "[::1]:8080", nil, 200},
		{"configured host", "POST", "NAS.home", map[string]string{"Origin": "http://nas.home"}, 200},
		{"localhost subdomain", "GET", "stlton.localhost:8080", nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/rescan", nil)
			req.Host = tc.host
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status %d, want %d (%s)", rec.Code, tc.want, strings.TrimSpace(rec.Body.String()))
			}
			if tc.want == 421 && !strings.Contains(rec.Body.String(), "-hosts evil.example") {
				t.Errorf("421 body doesn't say how to allow the host: %q", rec.Body.String())
			}
		})
	}
}
