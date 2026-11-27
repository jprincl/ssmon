package main

// Guard - fail2ban-like protection for SpyServer.
//
// It watches the client -> SpyServer direction of streaming connections
// (SYN/FIN/RST, ACKs and the first command) and applies the rules from
// ssmon.conf:
//   - reconnect: a new connection shortly after the previous one ended
//     (automatic reconnect after a kick, bots hammering the server); short
//     previous connections (server full) are ignored,
//   - client:    the HELLO client name contains no whitelisted part, or
//                the client sent something other than HELLO first,
//   - quota:     total connected time per IP within a sliding window,
//   - camp:      the same MHz (from directory reports) for too long.
// Each rule can be off, flag (event only) or ban. Bans escalate per repeat
// offense and are enforced by the Firewall (nftables).

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	spyCmdHello      = 0
	connIdleTimeout  = 60 * time.Second // no packet from the client = gone
	eventRepeatQuiet = 10 * time.Minute // same ip+rule event is not repeated sooner
)

type connKey struct {
	src   [4]byte
	sport uint16
}

type gconn struct {
	ip          string
	start, last time.Time
	helloDone   bool
}

type span struct{ start, end time.Time }

type ipState struct {
	lastEnd   time.Time   // end of the most recent closed connection that lasted >= ReconnectMinSession
	fast      []time.Time // fast reconnects within ReconnectPeriod
	history   []span      // closed connections within QuotaWindow
	lastTouch time.Time
}

type Guard struct {
	mu      sync.Mutex
	cfg     *Config
	db      *sql.DB
	mon     *Monitor
	fw      Firewall
	fwReady bool
	conns   map[connKey]*gconn
	ips     map[string]*ipState
	quiet   map[string]time.Time // ip|rule -> last event
	camped  map[string]bool      // ip|since already reported
	banned  map[string]time.Time // active bans: ip -> expiry (zero = permanent)
}

func NewGuard(cfg *Config, mon *Monitor) *Guard {
	g := &Guard{
		cfg:    cfg,
		db:     mon.db,
		mon:    mon,
		conns:  map[connKey]*gconn{},
		ips:    map[string]*ipState{},
		quiet:  map[string]time.Time{},
		camped: map[string]bool{},
		banned: map[string]time.Time{},
	}
	for _, b := range g.activeBans(time.Now()) {
		g.banned[b.IP] = banExpiry(b.Expires)
	}
	g.setupFirewall()
	return g
}

// Reload swaps in a new configuration (SIGHUP).
func (g *Guard) Reload(cfg *Config) {
	g.mu.Lock()
	old := g.cfg
	g.cfg = cfg
	g.mu.Unlock()
	if old.Firewall != cfg.Firewall || old.BanPort != cfg.BanPort || !g.fwReady {
		g.setupFirewall()
	}
	log.Printf("guard: configuration reloaded (enabled=%v dry_run=%v firewall=%s)", cfg.GuardEnabled, cfg.DryRun, cfg.Firewall)
}

// Config returns the current configuration.
func (g *Guard) Config() *Config {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cfg
}

// setupFirewall creates the ban table and restores active bans. It is only
// needed when bans can happen (guard enabled or admin login configured).
func (g *Guard) setupFirewall() {
	g.mu.Lock()
	cfg := g.cfg
	g.mu.Unlock()
	if !cfg.GuardEnabled && cfg.AdminPasswordHash == "" {
		return
	}
	fw := newFirewall(cfg.Firewall, cfg.BanPort)
	active := map[string]time.Duration{}
	now := time.Now()
	for _, b := range g.activeBans(now) {
		if b.Expires == 0 {
			active[b.IP] = 0
		} else {
			active[b.IP] = time.Until(time.Unix(int64(b.Expires), 0))
		}
	}
	if err := fw.Init(active); err != nil {
		log.Printf("guard: firewall init failed, bans will not be enforced: %v", err)
	} else {
		log.Printf("guard: firewall %s ready (ban port %d), %d active bans restored", cfg.Firewall, cfg.BanPort, len(active))
	}
	g.mu.Lock()
	g.fw, g.fwReady = fw, true
	g.mu.Unlock()
}

// Packet handles one client -> SpyServer packet.
func (g *Guard) Packet(ts time.Time, s tcpSegment) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ip := net.IP(s.src[:]).String()
	if g.bannedNow(ip, ts) {
		return // the firewall drops it; do not count blocked attempts
	}
	key := connKey{s.src, s.sport}
	c := g.conns[key]
	st := g.state(ip, ts)

	switch {
	case s.flags&tcpSYN != 0 && s.flags&tcpACK == 0:
		if c != nil {
			c.last = ts // SYN retransmission
			break
		}
		// A connection of the same IP that went silent is over by now.
		for k, o := range g.conns {
			if o.ip == ip && ts.Sub(o.last) > 5*time.Second {
				g.closeConn(k, o, o.last)
			}
		}
		g.checkReconnect(ip, st, ts)
		c = &gconn{ip: ip, start: ts, last: ts}
		g.conns[key] = c
	case c == nil:
		// Mid-connection packet (ssmon started later): track it, but the
		// HELLO is gone, so do not judge the client.
		if s.flags&(tcpFIN|tcpRST) != 0 {
			return
		}
		c = &gconn{ip: ip, start: ts, last: ts, helloDone: true}
		g.conns[key] = c
	default:
		c.last = ts
	}

	if len(s.payload) > 0 && !c.helloDone {
		c.helloDone = true
		name, version, ok := parseHello(s.payload)
		display := name
		switch {
		case !ok:
			display = "(no HELLO)"
		case name == "":
			display = "(empty name)"
		}
		if version != "" {
			display += " · " + version
		}
		g.mon.SetClient(ip, display)
		if !ok || !g.cfg.ClientAllowed(name) {
			g.trigger(ip, "client", g.cfg.UnknownClientAction, fmt.Sprintf("client %q", display))
		}
	}

	if s.flags&(tcpFIN|tcpRST) != 0 {
		g.closeConn(key, c, ts)
	}
}

func (g *Guard) state(ip string, ts time.Time) *ipState {
	st := g.ips[ip]
	if st == nil {
		st = &ipState{}
		g.ips[ip] = st
	}
	st.lastTouch = ts
	return st
}

func (g *Guard) closeConn(k connKey, c *gconn, end time.Time) {
	delete(g.conns, k)
	st := g.state(c.ip, end)
	// Short connections (e.g. rejected because the server is full) do not
	// count as a session that a fast reconnect could follow.
	if end.Sub(c.start) >= g.cfg.ReconnectMinSession && end.After(st.lastEnd) {
		st.lastEnd = end
	}
	st.history = append(st.history, span{c.start, end})
}

func (g *Guard) checkReconnect(ip string, st *ipState, ts time.Time) {
	cfg := g.cfg
	if st.lastEnd.IsZero() {
		return
	}
	gap := ts.Sub(st.lastEnd)
	if gap < 0 || gap > cfg.ReconnectWindow {
		return
	}
	cut := ts.Add(-cfg.ReconnectPeriod)
	keep := st.fast[:0]
	for _, t := range st.fast {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	st.fast = append(keep, ts)
	if len(st.fast) >= cfg.ReconnectCount {
		g.trigger(ip, "reconnect", cfg.ReconnectAction,
			fmt.Sprintf("reconnected %.1f s after disconnect (%d× in %s)", gap.Seconds(), len(st.fast), fmtDur(cfg.ReconnectPeriod)))
	}
}

// Tick runs the time-based rules; call it every few seconds.
func (g *Guard) Tick(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cfg := g.cfg

	for k, c := range g.conns {
		if now.Sub(c.last) > connIdleTimeout {
			g.closeConn(k, c, c.last)
		}
	}

	// Quota: closed connections plus open ones, clipped to the window.
	from := now.Add(-cfg.QuotaWindow)
	open := map[string]time.Duration{}
	for _, c := range g.conns {
		s := c.start
		if s.Before(from) {
			s = from
		}
		open[c.ip] += now.Sub(s)
	}
	for ip, st := range g.ips {
		keep := st.history[:0]
		var total time.Duration
		for _, sp := range st.history {
			if sp.end.Before(from) {
				continue
			}
			keep = append(keep, sp)
			s := sp.start
			if s.Before(from) {
				s = from
			}
			total += sp.end.Sub(s)
		}
		st.history = keep
		total += open[ip]
		if total > cfg.QuotaTime && !g.bannedNow(ip, now) {
			g.trigger(ip, "quota", cfg.QuotaAction,
				fmt.Sprintf("connected %s within %s (limit %s)", fmtDur(total), fmtDur(cfg.QuotaWindow), fmtDur(cfg.QuotaTime)))
		}
		if open[ip] == 0 && now.Sub(st.lastTouch) > 24*time.Hour {
			delete(g.ips, ip)
		}
	}

	// Camping: same MHz in the open session (from directory reports).
	if ip, mhz, since, ok := g.mon.CampInfo(); ok {
		held := now.Sub(time.Unix(0, int64(since*1e9)))
		k := fmt.Sprintf("%s|%.0f", ip, since)
		if held > cfg.CampTime && !g.camped[k] {
			g.camped[k] = true
			g.trigger(ip, "camp", cfg.CampAction, fmt.Sprintf("%d MHz for %s", mhz, fmtDur(held)))
		}
	}

	for k, t := range g.quiet {
		if now.Sub(t) > 24*time.Hour {
			delete(g.quiet, k)
		}
	}
}

// trigger applies a rule's action. Called with g.mu held.
func (g *Guard) trigger(ip, rule, action, detail string) {
	cfg := g.cfg
	if !cfg.GuardEnabled || action == actOff {
		return
	}
	now := time.Now()
	qk := ip + "|" + rule
	if t, ok := g.quiet[qk]; ok && now.Sub(t) < eventRepeatQuiet {
		return
	}
	g.quiet[qk] = now

	switch {
	case cfg.Whitelisted(ip):
		g.event(now, ip, rule, "whitelisted", detail)
	case action == actFlag:
		g.event(now, ip, rule, "flag", detail)
	case cfg.DryRun:
		g.event(now, ip, rule, "dry-run", detail+" → would ban for "+fmtBanDur(g.nextBanDuration(ip, now)))
	default:
		if g.bannedNow(ip, now) {
			return
		}
		d := g.nextBanDuration(ip, now)
		if err := g.banLocked(ip, d, detail, rule, true); err != nil {
			log.Printf("guard: ban %s failed: %v", ip, err)
		}
	}
}

func (g *Guard) nextBanDuration(ip string, now time.Time) time.Duration {
	var n int
	since := float64(now.Add(-g.cfg.BanMemory).Unix())
	g.db.QueryRow(`SELECT count(*) FROM bans WHERE ip = ? AND created >= ?`, ip, since).Scan(&n)
	ds := g.cfg.BanDurations
	if n >= len(ds) {
		n = len(ds) - 1
	}
	return ds[n]
}

func fmtBanDur(d time.Duration) string {
	if d == 0 {
		return "permanent"
	}
	return fmtDur(d)
}

// fmtDur prints durations compactly: 45s, 12m, 1h30m, 2d.
func fmtDur(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	}
	d = d.Round(time.Minute)
	if m := int(d.Minutes()) % 60; m != 0 {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), m)
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// Ban bans ip manually (d = 0 means permanent).
func (g *Guard) Ban(ip string, d time.Duration, reason string) error {
	if net.ParseIP(ip).To4() == nil {
		return fmt.Errorf("invalid IPv4 address")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if reason == "" {
		reason = "manual"
	}
	return g.banLocked(ip, d, reason, "manual", false)
}

func (g *Guard) banLocked(ip string, d time.Duration, reason, rule string, auto bool) error {
	now := time.Now()
	var expires any
	if d > 0 {
		expires = float64(now.Add(d).Unix())
	}
	// A new ban replaces an active one.
	g.db.Exec(`UPDATE bans SET lifted = ? WHERE ip = ? AND lifted IS NULL`, float64(now.Unix()), ip)
	if _, err := g.db.Exec(`INSERT INTO bans (ip, created, expires, reason, rule, auto) VALUES (?, ?, ?, ?, ?, ?)`,
		ip, float64(now.Unix()), expires, reason, rule, auto); err != nil {
		return err
	}
	g.event(now, ip, rule, "ban", reason+" ("+fmtBanDur(d)+")")
	if d > 0 {
		g.banned[ip] = now.Add(d)
	} else {
		g.banned[ip] = time.Time{}
	}
	// The firewall cuts the open connections; stop tracking them.
	for k, c := range g.conns {
		if c.ip == ip {
			g.closeConn(k, c, now)
		}
	}
	if g.fw == nil {
		return fmt.Errorf("firewall not configured")
	}
	return g.fw.Ban(ip, d)
}

// Unban lifts all active bans of ip.
func (g *Guard) Unban(ip string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.db.Exec(`UPDATE bans SET lifted = ? WHERE ip = ? AND lifted IS NULL`, float64(now.Unix()), ip)
	g.event(now, ip, "manual", "unban", "")
	delete(g.banned, ip)
	delete(g.quiet, ip+"|reconnect")
	delete(g.quiet, ip+"|client")
	delete(g.quiet, ip+"|quota")
	if g.fw == nil {
		return fmt.Errorf("firewall not configured")
	}
	return g.fw.Unban(ip)
}

// bannedNow reports whether ip has an active ban. Called with g.mu held.
func (g *Guard) bannedNow(ip string, now time.Time) bool {
	exp, ok := g.banned[ip]
	if !ok {
		return false
	}
	if !exp.IsZero() && now.After(exp) {
		delete(g.banned, ip)
		return false
	}
	return true
}

func banExpiry(expires float64) time.Time {
	if expires == 0 {
		return time.Time{}
	}
	return time.Unix(int64(expires), 0)
}

func (g *Guard) event(now time.Time, ip, rule, action, detail string) {
	log.Printf("guard: %s %s [%s] %s", action, ip, rule, detail)
	if _, err := g.db.Exec(`INSERT INTO events (ts, ip, rule, action, detail) VALUES (?, ?, ?, ?, ?)`,
		float64(now.UnixNano())/1e9, ip, rule, action, detail); err != nil {
		log.Printf("guard: event: %v", err)
	}
}

// parseHello decodes a SpyServer HELLO command:
// header {uint32 command, uint32 body size}, body {uint32 protocol version,
// client name bytes}, all little endian.
func parseHello(p []byte) (name, version string, ok bool) {
	if len(p) < 12 || binary.LittleEndian.Uint32(p[0:4]) != spyCmdHello {
		return "", "", false
	}
	size := int(binary.LittleEndian.Uint32(p[4:8]))
	if size < 4 || size > 4096 {
		return "", "", false
	}
	body := p[8:]
	if len(body) > size {
		body = body[:size]
	}
	v := binary.LittleEndian.Uint32(body[0:4])
	version = fmt.Sprintf("%d.%d.%d", v>>24, (v>>16)&0xff, v&0xffff)
	name = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, string(body[4:]))
	if len(name) > 80 {
		name = name[:80]
	}
	return strings.TrimSpace(name), version, true
}

// ---- queries for the admin API ----

type BanRow struct {
	ID      int64   `json:"id"`
	IP      string  `json:"ip"`
	Created float64 `json:"created"`
	Expires float64 `json:"expires"` // 0 = permanent
	Lifted  float64 `json:"lifted"`  // 0 = not lifted
	Reason  string  `json:"reason"`
	Rule    string  `json:"rule"`
	Auto    bool    `json:"auto"`
	Active  bool    `json:"active"`
	CC      string  `json:"cc"`
	Org     string  `json:"org"`
}

func (g *Guard) activeBans(now time.Time) []BanRow {
	return g.queryBans(`lifted IS NULL AND (expires IS NULL OR expires > ?)`, 500, float64(now.Unix()))
}

// BanLists returns the active bans and the most recent inactive ones.
func (g *Guard) BanLists() (active, past []BanRow) {
	now := float64(time.Now().Unix())
	active = g.queryBans(`lifted IS NULL AND (expires IS NULL OR expires > ?)`, 500, now)
	past = g.queryBans(`NOT (lifted IS NULL AND (expires IS NULL OR expires > ?))`, 30, now)
	return
}

func (g *Guard) queryBans(where string, limit int, args ...any) []BanRow {
	out := []BanRow{}
	now := float64(time.Now().Unix())
	rows, err := g.db.Query(`SELECT b.id, b.ip, b.created, coalesce(b.expires, 0), coalesce(b.lifted, 0),
	    coalesce(b.reason, ''), coalesce(b.rule, ''), b.auto,
	    coalesce((SELECT cc FROM sessions WHERE ip = b.ip AND cc IS NOT NULL ORDER BY id DESC LIMIT 1), ''),
	    coalesce((SELECT org FROM sessions WHERE ip = b.ip AND org IS NOT NULL ORDER BY id DESC LIMIT 1), '')
	  FROM bans b WHERE `+where+` ORDER BY b.created DESC LIMIT `+fmt.Sprint(limit), args...)
	if err != nil {
		log.Printf("guard: bans query: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var b BanRow
		if rows.Scan(&b.ID, &b.IP, &b.Created, &b.Expires, &b.Lifted, &b.Reason, &b.Rule, &b.Auto, &b.CC, &b.Org) == nil {
			b.Active = b.Lifted == 0 && (b.Expires == 0 || b.Expires > now)
			out = append(out, b)
		}
	}
	return out
}

type EventRow struct {
	TS     float64 `json:"ts"`
	IP     string  `json:"ip"`
	Rule   string  `json:"rule"`
	Action string  `json:"action"`
	Detail string  `json:"detail"`
}

func (g *Guard) Events(limit int) []EventRow {
	out := []EventRow{}
	rows, err := g.db.Query(`SELECT ts, ip, rule, action, coalesce(detail, '') FROM events ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var e EventRow
		if rows.Scan(&e.TS, &e.IP, &e.Rule, &e.Action, &e.Detail) == nil {
			out = append(out, e)
		}
	}
	return out
}
