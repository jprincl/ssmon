package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

// Leaflet is bundled so the dashboard does not depend on a CDN.
//
//go:embed web/leaflet
var leafletFS embed.FS

// Report fields exposed by the API (OwnerEmail and serial are left out).
var publicFields = []string{
	"ServerVersion", "OperatingSystem", "MaxClients", "MaxSessionDuration",
	"DeviceType", "AntennaType", "AntennaLocation", "GeneralDescription",
	"OwnerName", "CurrentCenterFrequency", "CurrentClientCount",
	"MinimumFrequency", "MaximumFrequency", "MaximumStreamedBandwidth",
}

func newWebHandler(m *Monitor, interval time.Duration) http.Handler {
	mux := http.NewServeMux()
	iv := interval.Seconds()

	leaflet, _ := fs.Sub(leafletFS, "web")
	static := http.FileServer(http.FS(leaflet))
	mux.Handle("/leaflet/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=604800")
		static.ServeHTTP(w, r)
	}))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(indexHTML)
	})

	// GET /api/summary?days=7&tz=7200&lang=en  (tz = local offset from UTC, seconds)
	mux.HandleFunc("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		now := ts(time.Now())
		q := r.URL.Query()
		days, since := sinceParam(q.Get("days"), now)
		tz, _ := strconv.ParseInt(q.Get("tz"), 10, 64)
		sum, err := m.Summary(since, iv, tz, days == 1, langParam(r))
		if err != nil {
			apiError(w, err)
			return
		}
		st := m.Status()
		fields := map[string]string{}
		for _, k := range publicFields {
			if v, ok := st.Fields[k]; ok {
				fields[k] = v
			}
		}
		var station *[2]float64
		if lat, lon, ok := parseLatLon(st.Fields["AntennaLocation"]); ok {
			station = &[2]float64{lat, lon}
		}
		writeJSON(w, map[string]any{
			"now":         now,
			"interval":    iv,
			"last_report": st.LastReport,
			"online":      st.LastReport > 0 && now-st.LastReport < 3*iv,
			"fields":      fields,
			"station":     station,
			"summary":     sum,
		})
	})

	// GET /api/sessions?days=7&q=praha&limit=50&offset=0&minsecs=30
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		_, since := sinceParam(q.Get("days"), ts(time.Now()))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 50
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		if offset < 0 {
			offset = 0
		}
		minSecs, _ := strconv.ParseFloat(q.Get("minsecs"), 64)
		list, total, hidden, err := m.Sessions(since, iv, q.Get("q"), limit, offset, langParam(r), minSecs)
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, map[string]any{"total": total, "hidden": hidden, "sessions": list})
	})
	// GET /api/session/timeline?id=123
	mux.HandleFunc("/api/session/timeline", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		steps, err := m.Timeline(id, iv, langParam(r))
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, map[string]any{"tunings": steps})
	})
	return mux
}

// langParam returns the dashboard language from ?lang= ("cs" by default).
func langParam(r *http.Request) string {
	if r.URL.Query().Get("lang") == "en" {
		return "en"
	}
	return "cs"
}

// sinceParam turns ?days=N into a unix-time lower bound (0 = everything).
func sinceParam(s string, now float64) (float64, float64) {
	days, _ := strconv.ParseFloat(s, 64)
	if days <= 0 {
		return 0, 0
	}
	return days, now - days*86400
}

func apiError(w http.ResponseWriter, err error) {
	log.Printf("api: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// parseLatLon parses "50.0755, 14.4378" (SpyServer may append a stray dot).
func parseLatLon(s string) (float64, float64, bool) {
	a, b, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, false
	}
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(a), 64)
	lon, err2 := strconv.ParseFloat(strings.TrimRight(strings.TrimSpace(b), "."), 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return lat, lon, true
}
