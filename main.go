// ssmon - passive SpyServer listener monitor.
//
// Sniffs the status reports SpyServer POSTs to the Airspy directory
// (directory.api.airspy.com:8080/register), turns them into listening
// sessions (who, when, how long, which frequencies) stored in SQLite, geolocates client IPs
// offline from MMDB files and serves a dashboard with a map and statistics.
// Nothing is inserted into the SpyServer data path.
//
// The optional guard (ssmon.conf) also watches client -> SpyServer traffic,
// decodes the client name from HELLO and bans abusive clients via nftables;
// bans can also be set by hand from the dashboard after an admin login.
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
	configPath := flag.String("config", "", "ssmon.conf with guard rules and admin login (empty = defaults: guard off, no admin)")
	hashPw := flag.Bool("hash-password", false, "read a password from stdin, print admin_password_hash for ssmon.conf and exit")
	flag.Parse()
	if *hashPw {
		runHashPassword()
		return
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("ssmon: ")

	if *bandsFile != "" {
		if err := LoadBands(*bandsFile); err != nil {
			log.Fatalf("bands: %v", err)
		}
		log.Printf("loaded %d bands from %s", len(bands), *bandsFile)
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *configPath != "" {
		log.Printf("config %s: guard enabled=%v dry_run=%v firewall=%s, admin login %v",
			*configPath, cfg.GuardEnabled, cfg.DryRun, cfg.Firewall, cfg.AdminPasswordHash != "")
	}

	geo := NewGeo(*cityDB, *asnDB)
	mon, err := NewMonitor(*dbPath, geo, *interval, *gap, *anonDays)
	if err != nil {
		log.Fatal(err)
	}
	mon.InitialMHz = *initMHz
	guard := NewGuard(cfg, mon)

	port := uint16(*dirPort)
	spyPort := cfg.SpyPort // capture filter is fixed at start; changing it needs a restart
	reasm := newReassembler(port)
	onPacket := func(t time.Time, ip []byte) {
		seg, ok := parseIPv4TCP(ip)
		if !ok {
			return
		}
		if seg.dport == spyPort {
			guard.Packet(t, seg)
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
			Handler:           newWebHandler(mon, guard, *interval),
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
			log.Printf("capturing on %s: directory reports (TCP %d), SpyServer clients (TCP %d)", *iface, port, spyPort)
			err := captureLive(*iface, port, spyPort, onPacket, func(t time.Time) {
				mon.Tick(t)
				guard.Tick(t)
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
			log.Printf("SIGHUP: reloading GeoIP databases and config")
			geo.Reload()
			mon.BackfillGeo()
			if c, err := LoadConfig(*configPath); err != nil {
				log.Printf("config reload failed, keeping the old one: %v", err)
			} else {
				if c.SpyPort != spyPort {
					log.Printf("config: spyserver_port change needs a restart")
				}
				guard.Reload(c)
			}
			continue
		}
		log.Printf("%v: shutting down", s)
		mon.Close()
		return
	}
}
