package server

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// guard protects a server with no accounts from being driven by other web
// pages in the user's browser:
//
//   - Cross-site writes: a page on another site can send POSTs to
//     127.0.0.1:8080 (as a form or a no-cors fetch). Browsers mark such
//     requests with Origin and Sec-Fetch-Site, so writes carrying a foreign
//     origin are refused. Requests with neither header (curl, scripts) aren't
//     from a browser page and are allowed.
//   - DNS rebinding: a page on evil.example can point that name at 127.0.0.1
//     and then make "same-origin" requests, reading and writing freely. Those
//     requests carry Host: evil.example, so the Host must be one the server
//     is known by.
func guard(next http.Handler, allowedHosts []string) http.Handler {
	allowed := map[string]bool{"localhost": true}
	if name, err := os.Hostname(); err == nil && name != "" {
		name = strings.ToLower(name)
		allowed[name] = true
		allowed[name+".local"] = true
	}
	for _, h := range allowedHosts {
		allowed[strings.ToLower(strings.TrimSpace(h))] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host := hostname(r.Host); !hostAllowed(host, allowed) {
			http.Error(w, "STLton doesn't recognize the host name "+host+
				". If that's how you reach this server, restart it with -hosts "+host+".", http.StatusMisdirectedRequest)
			return
		}
		if isWrite(r.Method) && crossSite(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isWrite(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// crossSite reports whether a browser marked r as coming from another site.
func crossSite(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" { // includes "null" from sandboxed frames
		return true
	}
	return !strings.EqualFold(u.Host, r.Host)
}

// hostname strips the port from a Host header and lowercases it.
func hostname(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

// hostAllowed accepts IP literals (rebinding needs a DNS name), loopback
// names, and configured names.
func hostAllowed(host string, allowed map[string]bool) bool {
	if net.ParseIP(host) != nil || allowed[host] {
		return true
	}
	return strings.HasSuffix(host, ".localhost")
}
