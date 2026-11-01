package daemon

import (
	"net"
	"net/http"
	"strings"
)

// localOriginNames are the Host header values that count as "this machine".
// The port is checked separately against the actual bound port so the guard
// stays correct when the listen address is overridden.
var localOriginNames = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
	"[::1]":     true,
}

// Guard wraps the whole HTTP mux so a web page the user visits cannot drive
// the daemon. Three checks, all cheap and all at the edge:
//
//  1. Host must be localhost / 127.0.0.1 / [::1] with the actual bound port —
//     blocks DNS-rebinding, where a hostile site rebinds its name to
//     127.0.0.1 so the browser treats it as same-origin.
//  2. If an Origin header is present it must be same-origin with Host —
//     blocks a cross-site page from issuing fetch() calls. No Origin header
//     (curl, native MCP clients, same-origin navigations) is allowed through.
//  3. POSTs under apiPrefix must carry a JSON content type — browsers can
//     only send "simple" cross-site requests with a handful of content types
//     (text/plain, form-encoded); requiring application/json forces a
//     preflight that the Origin check then rejects.
//
// boundAddr is the listener's real "host:port" (e.g. "127.0.0.1:47180").
// apiPrefix is the path prefix whose mutating routes need the JSON check
// ("" disables it).
func Guard(boundAddr, apiPrefix string, next http.Handler) http.Handler {
	_, boundPort, err := net.SplitHostPort(boundAddr)
	if err != nil {
		boundPort = ""
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, boundPort) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin, boundPort) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if apiPrefix != "" && r.Method == http.MethodPost &&
			strings.HasPrefix(r.URL.Path, apiPrefix) &&
			!jsonContentType(r.Header.Get("Content-Type")) {
			http.Error(w, "api requires a JSON content type", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether the request's Host header names a loopback
// authority on the bound port. A missing/empty Host is allowed (HTTP/1.0
// clients, some native tools).
func hostAllowed(host, boundPort string) bool {
	if host == "" {
		return true
	}
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		// Host had no port (e.g. just "localhost") — treat the name part.
		name, port = host, ""
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if !localOriginNames[name] && !isLoopbackIP(name) {
		return false
	}
	// The Host must carry the bound port. A bare "localhost" implies the
	// scheme's default port (80/443), which is NOT the daemon's port — a
	// rebind to another local process on :80 would otherwise pass. Native
	// clients and browsers always send the explicit port.
	return boundPort == "" || port == boundPort
}

// originAllowed reports whether an Origin header is same-origin with the
// loopback bind. We require a loopback host and a matching port — that is
// exactly "the page was served by this daemon", since only this daemon serves
// content on boundPort.
func originAllowed(origin, boundPort string) bool {
	// Strip the scheme.
	rest := origin
	if i := strings.Index(rest, "://"); i >= 0 {
		scheme := strings.ToLower(rest[:i])
		if scheme != "http" && scheme != "https" {
			return false
		}
		rest = rest[i+3:]
	}
	// Drop any path/query — Origin shouldn't carry one, but be lenient.
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	name, port, err := net.SplitHostPort(rest)
	if err != nil {
		name, port = rest, ""
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if !localOriginNames[name] && !isLoopbackIP(name) {
		return false
	}
	// Same-origin requires the same port. A port-less Origin resolves to the
	// scheme's default (80/443), which is a different origin from the
	// daemon's bound port — a page served by another local process on :80
	// would otherwise slip through. Browsers always send the explicit port.
	return boundPort == "" || port == boundPort
}

func isLoopbackIP(name string) bool {
	ip := net.ParseIP(strings.Trim(name, "[]"))
	return ip != nil && ip.IsLoopback()
}

// jsonContentType reports whether the Content-Type is application/json (any
// casing/charset). Browsers cannot send this cross-site without a preflight.
func jsonContentType(ct string) bool {
	media := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	return media == "application/json"
}
