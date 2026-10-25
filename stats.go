package main

// Dashboard aggregations, computed in SQL so only small results leave the
// server regardless of how much history is stored.

import (
	"database/sql"
	"sort"
	"strconv"
	"strings"
)

type countRow struct {
	CC       string  `json:"cc"`
	Country  string  `json:"country"`
	Sessions int     `json:"sessions"`
	IPs      int     `json:"ips"`
	Secs     float64 `json:"secs"`
}

type asnRow struct {
	ASN      int64  `json:"asn"`
	Org      string `json:"org"`
	Sessions int    `json:"sessions"`
	IPs      int    `json:"ips"`
}

type locRow struct {
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	City     string  `json:"city"`
	Country  string  `json:"country"`
	CC       string  `json:"cc"`
	Org      string  `json:"org"`
	Sessions int     `json:"sessions"`
	IPs      int     `json:"ips"`
	Secs     float64 `json:"secs"`
	Last     float64 `json:"last"`
	Open     bool    `json:"open"`
}

type freqRow struct {
	MHz      int64   `json:"mhz"`
	Band     string  `json:"band"`
	Secs     float64 `json:"secs"`
	Sessions int     `json:"sessions"`
}

type bandRow struct {
	Band string  `json:"band"`
	Secs float64 `json:"secs"`
}

type bucketRow struct {
	K int64 `json:"k"` // hour: unix hour number; day: local day number
	N int   `json:"n"`
}

type Summary struct {
	Totals struct {
		Sessions  int     `json:"sessions"`
		IPs       int     `json:"ips"`
		Countries int     `json:"countries"`
		Secs      float64 `json:"secs"`
	} `json:"totals"`
	Countries []countRow  `json:"countries"`
	ASNs      []asnRow    `json:"asns"`
	Locations []locRow    `json:"locations"`
	Freqs     []freqRow   `json:"freqs"`
	Bands     []bandRow   `json:"bands"`
	Hours     [24]int     `json:"hours"`
	Timeline  []bucketRow `json:"timeline"`
	Unit      string      `json:"unit"` // "hour" or "day"
	Live      *SessionRow `json:"live"`
}

type SessionRow struct {
	ID         int64      `json:"id"`
	IP         string     `json:"ip"`
	Start      float64    `json:"start"`
	Last       float64    `json:"last"`
	Secs       float64    `json:"secs"`
	Open       bool       `json:"open"`
	CC         string     `json:"cc,omitempty"`
	Country    string     `json:"country,omitempty"`
	City       string     `json:"city,omitempty"`
	Lat        *float64   `json:"lat,omitempty"`
	Lon        *float64   `json:"lon,omitempty"`
	ASN        int64      `json:"asn,omitempty"`
	Org        string     `json:"org,omitempty"`
	Freqs      []FreqSpan `json:"freqs"`   // distinct MHz values in order of first use (initial one excluded)
	Tunings    int        `json:"tunings"` // number of tuning entries in the session (all)
	IPSessions int        `json:"ip_sessions"`
}

// FreqSpan is the total time a session spent around one center frequency (MHz).
type FreqSpan struct {
	MHz  int64   `json:"mhz"`
	Band string  `json:"band"`
	Secs float64 `json:"secs"`
}

// TuningStep is one entry of a session's tuning timeline.
type TuningStep struct {
	TS      float64 `json:"ts"`
	MHz     int64   `json:"mhz"`
	Secs    float64 `json:"secs"`
	Band    string  `json:"band"`
	Initial bool    `json:"initial,omitempty"` // server's initial frequency, not counted
}

// Estimated session length: each report stands for ~one interval of listening.
const durExpr = `(s.last_seen - s.start + :iv)`

// Center frequency truncated to whole MHz (145.0-145.999 -> 145). The report
// carries the receiver's center frequency, not the client's VFO, so finer
// precision is meaningless. Bands are always derived from the exact value.
const mhzExpr = `(t.freq / 1000000)`

// countedExpr drops the first tuning of a session when it is the server's
// initial frequency (-initial-mhz): every client lands there on connect.
const countedExpr = `NOT (t.ts = s.start AND :init > 0 AND ` + mhzExpr + ` = :init)`

func (m *Monitor) Summary(since, iv float64, tzSec int64, hourly bool, lang string) (*Summary, error) {
	sum := &Summary{Countries: []countRow{}, ASNs: []asnRow{}, Locations: []locRow{}, Freqs: []freqRow{}, Bands: []bandRow{}, Timeline: []bucketRow{}}
	args := []any{sql.Named("since", since), sql.Named("iv", iv), sql.Named("tz", tzSec), sql.Named("init", m.InitialMHz)}
	db := m.db

	if err := qryRow(db, `SELECT count(*), count(DISTINCT ip), count(DISTINCT cc), coalesce(sum(`+durExpr+`), 0)
	  FROM sessions s WHERE s.last_seen >= :since`, args...).
		Scan(&sum.Totals.Sessions, &sum.Totals.IPs, &sum.Totals.Countries, &sum.Totals.Secs); err != nil {
		return nil, err
	}

	rows, err := qry(db, `SELECT coalesce(cc, ''), coalesce(max(country), ''), count(*), count(DISTINCT ip), sum(`+durExpr+`)
	  FROM sessions s WHERE s.last_seen >= :since GROUP BY cc ORDER BY 3 DESC LIMIT 15`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r countRow
		rows.Scan(&r.CC, &r.Country, &r.Sessions, &r.IPs, &r.Secs)
		sum.Countries = append(sum.Countries, r)
	}
	rows.Close()

	rows, err = qry(db, `SELECT coalesce(asn, 0), coalesce(max(org), ''), count(*), count(DISTINCT ip)
	  FROM sessions s WHERE s.last_seen >= :since GROUP BY asn ORDER BY 3 DESC LIMIT 10`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r asnRow
		rows.Scan(&r.ASN, &r.Org, &r.Sessions, &r.IPs)
		sum.ASNs = append(sum.ASNs, r)
	}
	rows.Close()

	rows, err = qry(db, `SELECT avg(lat), avg(lon), coalesce(max(city), ''), coalesce(max(country), ''), coalesce(max(cc), ''),
	    coalesce(max(org), ''), count(*), count(DISTINCT ip), sum(`+durExpr+`), max(last_seen), max(open)
	  FROM sessions s WHERE s.last_seen >= :since AND lat IS NOT NULL
	  GROUP BY round(lat, 3), round(lon, 3) ORDER BY 7 DESC LIMIT 2000`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r locRow
		rows.Scan(&r.Lat, &r.Lon, &r.City, &r.Country, &r.CC, &r.Org, &r.Sessions, &r.IPs, &r.Secs, &r.Last, &r.Open)
		sum.Locations = append(sum.Locations, r)
	}
	rows.Close()

	// Time spent on each tuning: until the next tuning, or the session end.
	rows, err = qry(db, `SELECT t.session_id, t.freq,
	    coalesce(lead(t.ts) OVER (PARTITION BY t.session_id ORDER BY t.ts), s.last_seen + :iv) - t.ts
	  FROM tunings t JOIN sessions s ON s.id = t.session_id
	  WHERE s.last_seen >= :since AND `+countedExpr, args...)
	if err != nil {
		return nil, err
	}
	type facc struct {
		secs     float64
		sessions map[int64]struct{}
		band     string
	}
	byMHz := map[int64]*facc{}
	byBand := map[string]float64{}
	for rows.Next() {
		var sid, freq int64
		var secs float64
		if rows.Scan(&sid, &freq, &secs) != nil || secs < 0 {
			continue
		}
		k := freq / 1000000
		a := byMHz[k]
		if a == nil {
			a = &facc{sessions: map[int64]struct{}{}, band: bandLabel(freq, lang)}
			byMHz[k] = a
		}
		a.secs += secs
		a.sessions[sid] = struct{}{}
		byBand[bandLabel(freq, lang)] += secs
	}
	rows.Close()
	for k, a := range byMHz {
		sum.Freqs = append(sum.Freqs, freqRow{MHz: k, Band: a.band, Secs: a.secs, Sessions: len(a.sessions)})
	}
	sort.Slice(sum.Freqs, func(i, j int) bool { return sum.Freqs[i].Secs > sum.Freqs[j].Secs })
	if len(sum.Freqs) > 10 {
		sum.Freqs = sum.Freqs[:10]
	}
	for b, secs := range byBand {
		sum.Bands = append(sum.Bands, bandRow{b, secs})
	}
	sort.Slice(sum.Bands, func(i, j int) bool { return sum.Bands[i].Secs > sum.Bands[j].Secs })

	// Hour of day in the viewer's local time (tz = local - UTC, seconds).
	rows, err = qry(db, `SELECT CAST(strftime('%H', s.start + :tz, 'unixepoch') AS INTEGER), count(*)
	  FROM sessions s WHERE s.last_seen >= :since GROUP BY 1`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var h, n int
		if rows.Scan(&h, &n) == nil && h >= 0 && h < 24 {
			sum.Hours[h] = n
		}
	}
	rows.Close()

	bucket := `CAST((s.start + :tz) / 86400 AS INTEGER)`
	sum.Unit = "day"
	if hourly {
		bucket = `CAST(s.start / 3600 AS INTEGER)`
		sum.Unit = "hour"
	}
	rows, err = qry(db, `SELECT `+bucket+`, count(*) FROM sessions s WHERE s.last_seen >= :since GROUP BY 1 ORDER BY 1`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b bucketRow
		rows.Scan(&b.K, &b.N)
		sum.Timeline = append(sum.Timeline, b)
	}
	rows.Close()

	live, err := m.querySessions(`s.open = 1`, nil, since, iv, 1, 0, lang)
	if err != nil {
		return nil, err
	}
	if len(live) > 0 {
		sum.Live = &live[0]
	}
	return sum, nil
}

// Sessions returns one page of sessions matching the free-text filter q
// (IP, place, network, "AS123", MHz prefix like "145.5" or a band name).
func (m *Monitor) Sessions(since, iv float64, q string, limit, offset int, lang string) ([]SessionRow, int, error) {
	where := `s.last_seen >= :since`
	var args []any
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + strings.ToLower(q) + "%"
		conds := []string{
			`lower(s.ip) LIKE :like`, `lower(coalesce(s.city, '')) LIKE :like`,
			`lower(coalesce(s.country, '')) LIKE :like`, `lower(coalesce(s.cc, '')) LIKE :like`,
			`lower(coalesce(s.org, '')) LIKE :like`, `('as' || coalesce(s.asn, '')) LIKE :like`,
		}
		args = append(args, sql.Named("like", like))
		if f, err := strconv.ParseFloat(strings.ReplaceAll(q, ",", "."), 64); err == nil && f > 0 {
			conds = append(conds, `EXISTS (SELECT 1 FROM tunings t WHERE t.session_id = s.id AND `+mhzExpr+` = :mhz AND `+countedExpr+`)`)
			args = append(args, sql.Named("mhz", int64(f)))
		}
		for i, b := range bandsMatching(q) {
			lo, hi := "lo"+strconv.Itoa(i), "hi"+strconv.Itoa(i)
			conds = append(conds, `EXISTS (SELECT 1 FROM tunings t WHERE t.session_id = s.id AND t.freq >= :`+lo+` AND t.freq < :`+hi+` AND `+countedExpr+`)`)
			args = append(args, sql.Named(lo, int64(b.LoMHz*1e6)), sql.Named(hi, int64(b.HiMHz*1e6)))
		}
		args = append(args, sql.Named("init", m.InitialMHz))
		where += ` AND (` + strings.Join(conds, " OR ") + `)`
	}

	var total int
	cargs := append([]any{sql.Named("since", since)}, args...)
	if err := qryRow(m.db, `SELECT count(*) FROM sessions s WHERE `+where, cargs...).Scan(&total); err != nil {
		return nil, 0, err
	}
	list, err := m.querySessions(where, args, since, iv, limit, offset, lang)
	return list, total, err
}

func (m *Monitor) querySessions(where string, args []any, since, iv float64, limit, offset int, lang string) ([]SessionRow, error) {
	all := append([]any{sql.Named("since", since), sql.Named("iv", iv), sql.Named("limit", limit), sql.Named("offset", offset)}, args...)
	if !hasNamed(args, "init") {
		all = append(all, sql.Named("init", m.InitialMHz))
	}
	rows, err := qry(m.db, `SELECT s.id, s.ip, s.start, s.last_seen, `+durExpr+`, s.open,
	    coalesce(s.cc, ''), coalesce(s.country, ''), coalesce(s.city, ''), s.lat, s.lon, coalesce(s.asn, 0), coalesce(s.org, ''),
	    (SELECT group_concat(mhz || ':' || secs || ':' || freq) FROM (
	        SELECT mhz, sum(d) AS secs, min(ts) AS first, min(freq) AS freq FROM (
	          SELECT `+mhzExpr+` AS mhz, t.ts, t.freq,
	            coalesce(lead(t.ts) OVER (ORDER BY t.ts), s.last_seen + :iv) - t.ts AS d
	          FROM tunings t WHERE t.session_id = s.id AND `+countedExpr+`)
	        GROUP BY mhz ORDER BY first)),
	    (SELECT count(*) FROM tunings WHERE session_id = s.id),
	    (SELECT count(*) FROM sessions s2 WHERE s2.ip = s.ip AND s2.last_seen >= :since)
	  FROM sessions s WHERE `+where+` ORDER BY s.start DESC LIMIT :limit OFFSET :offset`, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionRow{}
	for rows.Next() {
		var r SessionRow
		var lat, lon sql.NullFloat64
		var freqs sql.NullString
		if err := rows.Scan(&r.ID, &r.IP, &r.Start, &r.Last, &r.Secs, &r.Open, &r.CC, &r.Country, &r.City,
			&lat, &lon, &r.ASN, &r.Org, &freqs, &r.Tunings, &r.IPSessions); err != nil {
			return nil, err
		}
		if lat.Valid && lon.Valid {
			r.Lat, r.Lon = &lat.Float64, &lon.Float64
		}
		r.Freqs = []FreqSpan{}
		for _, item := range strings.Split(freqs.String, ",") {
			p := strings.Split(item, ":")
			if len(p) != 3 {
				continue
			}
			mhz, err1 := strconv.ParseInt(p[0], 10, 64)
			secs, err2 := strconv.ParseFloat(p[1], 64)
			freq, err3 := strconv.ParseInt(p[2], 10, 64)
			if err1 == nil && err2 == nil && err3 == nil {
				r.Freqs = append(r.Freqs, FreqSpan{MHz: mhz, Band: bandLabel(freq, lang), Secs: secs})
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Timeline returns the full tuning history of one session.
func (m *Monitor) Timeline(id int64, iv float64, lang string) ([]TuningStep, error) {
	rows, err := qry(m.db, `SELECT t.ts, t.freq,
	    coalesce(lead(t.ts) OVER (ORDER BY t.ts), s.last_seen + :iv) - t.ts,
	    NOT (`+countedExpr+`)
	  FROM tunings t JOIN sessions s ON s.id = t.session_id
	  WHERE t.session_id = :id ORDER BY t.ts`, sql.Named("id", id), sql.Named("iv", iv), sql.Named("init", m.InitialMHz))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TuningStep{}
	for rows.Next() {
		var t TuningStep
		var freq int64
		if err := rows.Scan(&t.TS, &freq, &t.Secs, &t.Initial); err != nil {
			return nil, err
		}
		t.MHz = freq / 1000000
		t.Band = bandLabel(freq, lang)
		out = append(out, t)
	}
	return out, rows.Err()
}

// qry/qryRow pass only the named arguments the query actually references:
// drivers differ in how they treat unused named arguments.
func qry(db *sql.DB, query string, args ...any) (*sql.Rows, error) {
	return db.Query(query, pick(query, args)...)
}

func qryRow(db *sql.DB, query string, args ...any) *sql.Row {
	return db.QueryRow(query, pick(query, args)...)
}

func pick(query string, args []any) []any {
	out := make([]any, 0, len(args))
	for _, a := range args {
		if na, ok := a.(sql.NamedArg); ok && !strings.Contains(query, ":"+na.Name) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func hasNamed(args []any, name string) bool {
	for _, a := range args {
		if na, ok := a.(sql.NamedArg); ok && na.Name == name {
			return true
		}
	}
	return false
}
