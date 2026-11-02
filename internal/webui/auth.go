package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// The WebUI authenticates with a one-time token the daemon prints to its
// own stdout at startup: the human (who sees the terminal) exchanges it for
// an HttpOnly SameSite=Strict cookie; the token is then dead. An agent
// running curl cannot mint a session (issue #14). Cookie sessions live in
// memory only — a daemon restart forces re-authorization, and restart also
// regenerates the token.

const (
	authCookieName = "rsm_auth"
	authCookieTTL  = 30 * 24 * time.Hour // 30 days
)

// GenerateToken returns a fresh one-time token: random, URL-safe, single-use.
func GenerateToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// auther holds the one-time token and the live cookie sessions.
type auther struct {
	mu       sync.Mutex
	token    string // empty once consumed — it is single-use
	sessions map[string]time.Time
}

func newAuther(token string) *auther {
	return &auther{token: token, sessions: map[string]time.Time{}}
}

// exchange burns the token (if it matches) and mints a cookie session id.
// Constant-time compare; a wrong or replayed token gets nothing.
func (a *auther) exchange(token string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(a.token)) != 1 {
		return "", false
	}
	a.token = "" // single-use
	sid := GenerateToken()
	a.sessions[sid] = time.Now().Add(authCookieTTL)
	return sid, true
}

// valid reports whether the request carries a live session cookie.
func (a *auther) valid(r *http.Request) bool {
	c, err := r.Cookie(authCookieName)
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok || time.Now().After(exp) {
		if ok {
			delete(a.sessions, c.Value)
		}
		return false
	}
	return true
}

// requireAuth wraps an apiMux: every /api/* request needs a valid session
// cookie except POST /api/auth/exchange (which trades the token for one).
// The static page itself stays open — it holds no data and its fetches get
// 401 until the human pastes the token.
func (d *Deps) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/exchange" && r.Method == http.MethodPost {
			d.handleAuthExchange(w, r)
			return
		}
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" && !d.auth.valid(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized: open the WebUI and enter the token printed at daemon startup")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleAuthExchange is the only unauthenticated /api route: it trades the
// one-time token for the session cookie.
func (d *Deps) handleAuthExchange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	sid, ok := d.auth.exchange(req.Token)
	if !ok {
		writeErr(w, http.StatusForbidden, "invalid token")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    sid,
		Path:     "/",
		MaxAge:   int(authCookieTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
