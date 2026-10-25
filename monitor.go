package main

// Session tracking and persistence (SQLite).
//
// SpyServer reports its state to the directory every ~15 s. A session is a run
// of consecutive reports carrying the same client IP; it ends when a report
// shows a different/no client, or when no report arrives within the gap.
// The open session is written to the DB on every report, so a crash or
// restart loses nothing.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  id        INTEGER PRIMARY KEY,
  ip        TEXT    NOT NULL,
  start     REAL    NOT NULL,          -- unix time of the first report
  last_seen REAL    NOT NULL,          -- unix time of the last report
  reports   INTEGER NOT NULL DEFAULT 1,
  open      INTEGER NOT NULL DEFAULT 1,
  cc TEXT, country TEXT, city TEXT, lat REAL, lon REAL,
  asn INTEGER, org TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_last ON sessions(last_seen);
CREATE INDEX IF NOT EXISTS idx_sessions_ip   ON sessions(ip);
CREATE TABLE IF NOT EXISTS tunings (
  session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  ts         REAL    NOT NULL,
  freq       INTEGER NOT NULL           -- receiver center frequency, Hz
);
CREATE INDEX IF NOT EXISTS idx_tunings_session ON tunings(session_id, ts);
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

type Status struct {
	LastReport float64           `json:"last_report"`
	Fields     map[string]string `json:"fields"`
}

// openSession is the in-memory view of the currently open session.
type openSession struct {
	id       int64
	ip       string
	lastSeen float64
	lastFreq int64
}

type Monitor struct {
	mu       sync.Mutex
	db       *sql.DB
	geo      *Geo
	gap      float64
	interval float64
	cur      *openSession
	status   Status
	anonDays int

	// InitialMHz is SpyServer's initial_frequency in MHz (0 = unknown). The
	// first tuning of a session at this frequency is left out of statistics.
	InitialMHz int64
}

func ts(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open(sqliteDriver, path)
	if err != nil {
		return nil, err
	}
	// A single connection serializes writers and readers - plenty for this
	// load and avoids SQLITE_BUSY entirely.
	db.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(p); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return db, nil
}

func NewMonitor(dbPath string, geo *Geo, interval, gap time.Duration, anonDays int) (*Monitor, error) {
	db, err := openDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("database %s: %w", dbPath, err)
	}
	m := &Monitor{db: db, geo: geo, gap: gap.Seconds(), interval: interval.Seconds(), anonDays: anonDays}

	// Fail fast when the database is not writable (wrong owner, read-only fs).
	if _, err := db.Exec(`INSERT INTO kv (key, value) VALUES ('started', ?)
	  ON CONFLICT(key) DO UPDATE SET value = excluded.value`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return nil, fmt.Errorf("database %s is not writable: %w", dbPath, err)
	}

	// Resume the newest open session; any older open ones are stale.
	var o openSession
	var freq sql.NullInt64
	err = db.QueryRow(`SELECT s.id, s.ip, s.last_seen,
	    (SELECT freq FROM tunings WHERE session_id = s.id ORDER BY ts DESC LIMIT 1)
	  FROM sessions s WHERE open = 1 ORDER BY id DESC LIMIT 1`).Scan(&o.id, &o.ip, &o.lastSeen, &freq)
	switch {
	case err == nil:
		o.lastFreq = freq.Int64
		m.cur = &o
		db.Exec(`UPDATE sessions SET open = 0 WHERE open = 1 AND id <> ?`, o.id)
	case err != sql.ErrNoRows:
		return nil, err
	}

	var raw string
	if db.QueryRow(`SELECT value FROM kv WHERE key = 'status'`).Scan(&raw) == nil {
		json.Unmarshal([]byte(raw), &m.status)
	}

	var n int
	db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n)
	log.Printf("database %s: %d sessions", dbPath, n)
	m.anonymize(time.Now())
	m.BackfillGeo()
	return m, nil
}

// BackfillGeo fills location/network data for sessions recorded while the
// GeoIP databases were missing. Called at startup and after a GeoIP reload.
func (m *Monitor) BackfillGeo() {
	if !m.geo.Loaded() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, err := m.db.Query(`SELECT id, ip FROM sessions WHERE cc IS NULL AND asn IS NULL AND instr(ip, '/') = 0`)
	if err != nil {
		log.Printf("geoip backfill: %v", err)
		return
	}
	type row struct {
		id int64
		ip string
	}
	var todo []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.ip) == nil {
			todo = append(todo, r)
		}
	}
	rows.Close()
	if len(todo) == 0 {
		return
	}
	tx, err := m.db.Begin()
	if err != nil {
		return
	}
	filled := 0
	for _, r := range todo {
		g := m.geo.Lookup(r.ip)
		if g.CC == "" && g.ASN == 0 {
			continue
		}
		tx.Exec(`UPDATE sessions SET cc = ?, country = ?, city = ?, lat = ?, lon = ?, asn = ?, org = ? WHERE id = ?`,
			nullStr(g.CC), nullStr(g.Country), nullStr(g.City), g.Lat, g.Lon, nullInt(g.ASN), nullStr(g.Org), r.id)
		filled++
	}
	if err := tx.Commit(); err == nil {
		log.Printf("geoip backfill: filled %d of %d sessions without location", filled, len(todo))
	}
}

// HandleReport processes one directory report observed at time t.
func (m *Monitor) HandleReport(t time.Time, fields map[string]string) {
	now := ts(t)
	m.mu.Lock()
	defer m.mu.Unlock()

	tx, err := m.db.Begin()
	if err != nil {
		log.Printf("db: %v", err)
		return
	}
	defer tx.Rollback()

	m.tickTx(tx, now)
	ip := strings.TrimSpace(fields["CurrentClients"])
	if net.ParseIP(ip) == nil {
		ip = "" // MaxClients=1, so a single address (or nothing) is expected
	}
	freq, _ := strconv.ParseInt(fields["CurrentCenterFrequency"], 10, 64)

	if m.cur != nil && m.cur.ip != ip {
		m.closeTx(tx)
	}
	if ip != "" {
		if m.cur == nil {
			g := m.geo.Lookup(ip)
			res, err := tx.Exec(`INSERT INTO sessions (ip, start, last_seen, cc, country, city, lat, lon, asn, org)
			  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ip, now, now, nullStr(g.CC), nullStr(g.Country), nullStr(g.City), g.Lat, g.Lon, nullInt(g.ASN), nullStr(g.Org))
			if err != nil {
				log.Printf("db insert session: %v", err)
				return
			}
			id, _ := res.LastInsertId()
			m.cur = &openSession{id: id, ip: ip, lastSeen: now}
			log.Printf("session %d open: %s (%s, %s, AS%d %s)", id, ip, g.City, g.CC, g.ASN, g.Org)
		} else {
			m.cur.lastSeen = now
			tx.Exec(`UPDATE sessions SET last_seen = ?, reports = reports + 1 WHERE id = ?`, now, m.cur.id)
		}
		if freq > 0 && freq != m.cur.lastFreq {
			tx.Exec(`INSERT INTO tunings (session_id, ts, freq) VALUES (?, ?, ?)`, m.cur.id, now, freq)
			m.cur.lastFreq = freq
		}
	}

	m.status = Status{LastReport: now, Fields: fields}
	b, _ := json.Marshal(m.status)
	tx.Exec(`INSERT INTO kv (key, value) VALUES ('status', ?)
	  ON CONFLICT(key) DO UPDATE SET value = excluded.value`, string(b))
	if err := tx.Commit(); err != nil {
		log.Printf("db commit: %v", err)
	}
}

// Tick closes the open session when reports stopped arriving.
func (m *Monitor) Tick(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || ts(t)-m.cur.lastSeen <= m.gap {
		return
	}
	tx, err := m.db.Begin()
	if err != nil {
		return
	}
	m.tickTx(tx, ts(t))
	tx.Commit()
}

func (m *Monitor) tickTx(tx *sql.Tx, now float64) {
	if m.cur != nil && now-m.cur.lastSeen > m.gap {
		m.closeTx(tx)
	}
}

func (m *Monitor) closeTx(tx *sql.Tx) {
	tx.Exec(`UPDATE sessions SET open = 0 WHERE id = ?`, m.cur.id)
	log.Printf("session %d closed: %s", m.cur.id, m.cur.ip)
	m.cur = nil
}

// Status returns the last report and whether a session is open.
func (m *Monitor) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Monitor) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.db.Close()
}

// anonymize masks IPs of sessions older than anonDays to /24 (IPv4) or /48
// (IPv6). Location and network data are kept.
func (m *Monitor) anonymize(t time.Time) {
	if m.anonDays <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := ts(t) - float64(m.anonDays)*86400
	rows, err := m.db.Query(`SELECT id, ip FROM sessions WHERE open = 0 AND last_seen < ? AND instr(ip, '/') = 0`, cutoff)
	if err != nil {
		log.Printf("anonymize: %v", err)
		return
	}
	type row struct {
		id int64
		ip string
	}
	var todo []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.ip) == nil {
			todo = append(todo, r)
		}
	}
	rows.Close()
	if len(todo) == 0 {
		return
	}
	tx, err := m.db.Begin()
	if err != nil {
		return
	}
	for _, r := range todo {
		tx.Exec(`UPDATE sessions SET ip = ? WHERE id = ?`, maskIP(r.ip), r.id)
	}
	if err := tx.Commit(); err == nil {
		log.Printf("anonymize: masked %d IP addresses older than %d days", len(todo), m.anonDays)
	}
}

func maskIP(s string) string {
	ip := net.ParseIP(s)
	if ip == nil {
		return s
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return ip.Mask(net.CIDRMask(48, 128)).String() + "/48"
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(v uint64) any {
	if v == 0 {
		return nil
	}
	return int64(v)
}
