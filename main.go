// ssmon - passive SpyServer listener monitor.
//
// Sniffs the status reports SpyServer POSTs to the Airspy directory
// (directory.api.airspy.com:8080/register), turns them into listening
// sessions (who, when, how long, which frequencies) stored in SQLite, geolocates client IPs
// offline from MMDB files and serves a dashboard with a map and statistics.
// Nothing is inserted into the SpyServer data path.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	iface := flag.String("iface", "wg0", "interface carrying SpyServer's directory traffic (empty = no capture)")
	dirPort := flag.Int("dir-port", 8080, "TCP port of the Airspy directory")
	dbPath := flag.String("db", "/var/lib/ssmon/ssmon.db", "SQLite database file")
	listen := flag.String("listen", ":8090", "dashboard listen address (empty = no dashboard)")
	cityDB := flag.String("geoip-city", "", "City MMDB file (DB-IP Lite / GeoLite2)")
	asnDB := flag.String("geoip-asn", "", "ASN MMDB file (DB-IP Lite / GeoLite2)")
	interval := flag.Duration("interval", 15*time.Second, "SpyServer report interval")
	gap := flag.Duration("gap", 45*time.Second, "close a session when no report mentions it for this long")
	anonDays := flag.Int("anonymize-after", 0, "mask client IPs to /24 after this many days (0 = never)")
	initMHz := flag.Int64("initial-mhz", 0, "SpyServer initial_frequency in MHz; a session's first tuning there is not counted (0 = off)")
	bandsFile := flag.String("bands", "", "band plan file (name; low MHz; high MHz per line), default = built-in")
	pcap := flag.String("pcap", "", "replay a pcap file (tcpdump -w) into the database and exit")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("ssmon: ")

	if *bandsFile != "" {
		if err := LoadBands(*bandsFile); err != nil {
			log.Fatalf("bands: %v", err)
		}
		log.Printf("loaded %d bands from %s", len(bands), *bandsFile)
	}

	geo := NewGeo(*cityDB, *asnDB)
	mon, err := NewMonitor(*dbPath, geo, *interval, *gap, *anonDays)
	if err != nil {
		log.Fatal(err)
	}
	mon.InitialMHz = *initMHz

	port := uint16(*dirPort)
	reasm := newReassembler(port)
	onPacket := func(t time.Time, ip []byte) {
		seg, ok := parseIPv4TCP(ip)
		if !ok {
			return
		}
		if body := reasm.feed(t, seg); body != nil {
			mon.HandleReport(t, parseReport(body))
		}
	}

	if *pcap != "" {
		if err := replayPcap(*pcap, onPacket); err != nil {
			log.Fatal(err)
		}
		mon.Close()
		log.Printf("replay of %s done", *pcap)
		return
	}

	if *listen != "" {
		srv := &http.Server{
			Addr:              *listen,
			Handler:           newWebHandler(mon, *interval),
			ReadHeaderTimeout: 10 * time.Second,
			WriteTimeout:      30 * time.Second,
		}
		go func() {
			log.Printf("dashboard on %s", *listen)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal(err)
			}
		}()
		defer srv.Shutdown(context.Background())
	}

	if *iface != "" {
		go func() {
			log.Printf("capturing directory reports on %s, TCP port %d", *iface, port)
			err := captureLive(*iface, port, onPacket, func(t time.Time) {
				mon.Tick(t)
				reasm.expire(t)
			})
			log.Fatalf("capture: %v", err) // systemd restarts us
		}()
	}

	// Daily anonymization pass.
	go func() {
		for range time.Tick(time.Hour) {
			mon.anonymize(time.Now())
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	for s := range sig {
		if s == syscall.SIGHUP {
			log.Printf("SIGHUP: reloading GeoIP databases")
			geo.Reload()
			mon.BackfillGeo()
			continue
		}
		log.Printf("%v: shutting down", s)
		mon.Close()
		return
	}
}
