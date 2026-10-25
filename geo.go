package main

import (
	"log"
	"net"
	"sync"
)

// GeoInfo is the location/network data attached to a session.
type GeoInfo struct {
	CC      string   `json:"cc,omitempty"`
	Country string   `json:"country,omitempty"`
	City    string   `json:"city,omitempty"`
	Lat     *float64 `json:"lat,omitempty"`
	Lon     *float64 `json:"lon,omitempty"`
	ASN     uint64   `json:"asn,omitempty"`
	Org     string   `json:"org,omitempty"`
}

// Geo performs offline lookups against City and ASN MMDB files.
// Both files are optional; Reload() re-opens them (e.g. after a monthly update).
type Geo struct {
	cityPath, asnPath string
	mu                sync.RWMutex
	city, asn         *mmdb
}

func NewGeo(cityPath, asnPath string) *Geo {
	g := &Geo{cityPath: cityPath, asnPath: asnPath}
	g.Reload()
	return g
}

func (g *Geo) Reload() {
	open := func(path, what string) *mmdb {
		if path == "" {
			return nil
		}
		db, err := openMMDB(path)
		if err != nil {
			log.Printf("geoip: cannot open %s database %s: %v", what, path, err)
			return nil
		}
		log.Printf("geoip: loaded %s database %s (%s)", what, path, db.dbType)
		return db
	}
	city := open(g.cityPath, "city")
	asn := open(g.asnPath, "ASN")
	if city == nil && asn == nil {
		log.Printf("geoip: no database loaded - sessions are stored without location (use -geoip-city / -geoip-asn)")
	}
	g.mu.Lock()
	g.city, g.asn = city, asn
	g.mu.Unlock()
}

// Loaded reports whether at least one GeoIP database is available.
func (g *Geo) Loaded() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.city != nil || g.asn != nil
}

func (g *Geo) Lookup(ipStr string) GeoInfo {
	var gi GeoInfo
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return gi
	}
	g.mu.RLock()
	city, asn := g.city, g.asn
	g.mu.RUnlock()

	if city != nil {
		if rec, err := city.Lookup(ip); err != nil {
			log.Printf("geoip: city lookup %s: %v", ipStr, err)
		} else if rec != nil {
			gi.CC, _ = mmdbPath(rec, "country", "iso_code").(string)
			gi.Country, _ = mmdbPath(rec, "country", "names", "en").(string)
			gi.City, _ = mmdbPath(rec, "city", "names", "en").(string)
			lat, ok1 := mmdbPath(rec, "location", "latitude").(float64)
			lon, ok2 := mmdbPath(rec, "location", "longitude").(float64)
			if ok1 && ok2 {
				gi.Lat, gi.Lon = &lat, &lon
			}
		}
	}
	if asn != nil {
		if rec, err := asn.Lookup(ip); err != nil {
			log.Printf("geoip: ASN lookup %s: %v", ipStr, err)
		} else if rec != nil {
			gi.ASN = asUint(rec["autonomous_system_number"])
			gi.Org, _ = rec["autonomous_system_organization"].(string)
		}
	}
	return gi
}
