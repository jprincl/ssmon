package main

// Admin login and API (ban/unban, bans and guard events).
//
// The password is stored in ssmon.conf as a PBKDF2-HMAC-SHA256 hash created
// with "ssmon -hash-password". A successful login sets an HttpOnly,
// SameSite=Strict session cookie (sessions live in memory, so a restart
// logs you out). State-changing requests additionally require the
// X-SSMon-Admin header, which a cross-site form cannot send (CSRF guard).

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pbkdf2Iter     = 210000
	sessionTTL     = 12 * time.Hour
	sessionCookie  = "ssmon_session"
	csrfHeader     = "X-SSMon-Admin"
	loginFailLimit = 5 // failed logins per IP per 15 minutes
)

// pbkdf2SHA256 derives a key (RFC 8018) using only the standard library.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	var out []byte
	for block := uint32(1); len(out) < keyLen; block++ {
		prf.Reset()
		prf.Write(salt)
		var bn [4]byte
		binary.BigEndian.PutUint32(bn[:], block)
		prf.Write(bn[:])
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// HashPassword returns "pbkdf2-sha256$<iter>$<salt>$<hash>".
func HashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key := pbkdf2SHA256([]byte(pw), salt, pbkdf2Iter, 32)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, enc.EncodeToString(salt), enc.EncodeToString(key))
}

func checkPassword(pw, stored string) bool {
	p := strings.Split(stored, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 1000 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(p[2])
	want, err2 := enc.DecodeString(p[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// runHashPassword implements "ssmon -hash-password": reads the password
// from stdin (one line) and prints the hash for ssmon.conf.
func runHashPassword() {
	fmt.Fprint(os.Stderr, "Password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, "no password given")
		os.Exit(1)
	}
	pw := strings.TrimRight(line, "\r\n")
	if len(pw) < 8 {
		fmt.Fprintln(os.Stderr, "\npassword must have at least 8 characters")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Println("admin_password_hash = " + HashPassword(pw))
}

type adminAuth struct {
	guard    *Guard
	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
	fails    map[string][]time.Time
}

func newAdminAuth(g *Guard) *adminAuth {
	return &adminAuth{guard: g, sessions: map[string]time.Time{}, fails: map[string][]time.Time{}}
}

func (a *adminAuth) enabled() bool { return a.guard.Config().AdminPasswordHash != "" }

func (a *adminAuth) isAdmin(r *http.Request) bool {
	if !a.enabled() {
		return false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok || time.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

func clientAddr(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func (a *adminAuth) tooManyFails(addr string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cut := time.Now().Add(-15 * time.Minute)
	keep := a.fails[addr][:0]
	for _, t := range a.fails[addr] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	a.fails[addr] = keep
	return len(keep) >= loginFailLimit
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(v)
}

// requireAdmin wraps admin handlers: valid session and, for POST, the CSRF header.
func (a *adminAuth) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.isAdmin(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get(csrfHeader) != "1" {
			http.Error(w, "missing "+csrfHeader+" header", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func (a *adminAuth) register(mux *http.ServeMux) {
	g := a.guard

	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		cfg := g.Config()
		writeJSON(w, map[string]any{
			"admin":         a.isAdmin(r),
			"admin_enabled": a.enabled(),
			"guard":         map[string]any{"enabled": cfg.GuardEnabled, "dry_run": cfg.DryRun, "firewall": cfg.Firewall},
		})
	})

	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get(csrfHeader) != "1" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !a.enabled() {
			http.Error(w, "admin login is not configured", http.StatusServiceUnavailable)
			return
		}
		addr := clientAddr(r)
		if a.tooManyFails(addr) {
			http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
			return
		}
		var req struct{ Password string }
		if readJSON(r, &req) != nil || !checkPassword(req.Password, g.Config().AdminPasswordHash) {
			a.mu.Lock()
			a.fails[addr] = append(a.fails[addr], time.Now())
			a.mu.Unlock()
			time.Sleep(time.Second)
			http.Error(w, "wrong password", http.StatusUnauthorized)
			return
		}
		tok := make([]byte, 32)
		rand.Read(tok)
		token := hex.EncodeToString(tok)
		a.mu.Lock()
		a.sessions[token] = time.Now().Add(sessionTTL)
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
			Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		})
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			a.mu.Lock()
			delete(a.sessions, c.Value)
			a.mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/admin/bans", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		active, past := g.BanLists()
		writeJSON(w, map[string]any{"active": active, "past": past})
	}))

	mux.HandleFunc("/api/admin/events", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		writeJSON(w, map[string]any{"events": g.Events(limit)})
	}))

	mux.HandleFunc("/api/admin/ban", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			IP     string  `json:"ip"`
			Hours  float64 `json:"hours"` // 0 = permanent
			Reason string  `json:"reason"`
		}
		if err := readJSON(r, &req); err != nil || req.Hours < 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		reason := strings.TrimSpace(req.Reason)
		if len(reason) > 200 {
			reason = reason[:200]
		}
		err := g.Ban(strings.TrimSpace(req.IP), time.Duration(req.Hours*float64(time.Hour)), reason)
		adminResult(w, err)
	}))

	mux.HandleFunc("/api/admin/unban", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			IP string `json:"ip"`
		}
		if err := readJSON(r, &req); err != nil || net.ParseIP(req.IP) == nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		adminResult(w, g.Unban(req.IP))
	}))
}

func adminResult(w http.ResponseWriter, err error) {
	if err != nil {
		// The ban is recorded even when the firewall call fails; report it.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
