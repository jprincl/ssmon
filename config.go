package main

// ssmon.conf - settings for the guard (automatic banning) and the admin
// login. Format: "key = value" per line, '#' starts a comment, lists are
// comma separated, durations use Go syntax plus a "d" suffix for days
// (30s, 2m, 1h30m, 7d). Unknown keys are an error, so typos are caught.

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Rule actions.
const (
	actOff  = "off"
	actFlag = "flag"
	actBan  = "ban"
)

type Config struct {
	// Admin login for the dashboard (ban/unban). Empty = admin disabled.
	AdminPasswordHash string

	// Guard master switches.
	GuardEnabled bool
	DryRun       bool   // log what would be banned, do not ban
	Firewall     string // "nft" or "none" (log only)
	SpyPort      uint16 // SpyServer port as seen on the capture interface
	BanPort      uint16 // public port blocked by the ban rule (before DNAT)

	WhitelistNets    []*net.IPNet
	WhitelistClients []string // case-insensitive substrings of the client name

	// Fast reconnect: a new connection within ReconnectWindow after the
	// previous one from the same IP ended. Only a previous connection that
	// lasted at least ReconnectMinSession counts, so retries against a full
	// server (rejected within a second) are not punished.
	ReconnectAction     string
	ReconnectWindow     time.Duration
	ReconnectMinSession time.Duration
	ReconnectCount      int           // fast reconnects needed to trigger
	ReconnectPeriod     time.Duration // ... within this period

	// Unknown client: HELLO name not on the client whitelist, or no HELLO.
	UnknownClientAction string

	// Quota: total connected time per IP within a sliding window.
	QuotaAction string
	QuotaTime   time.Duration
	QuotaWindow time.Duration

	// Camping: same MHz (center frequency) for too long.
	CampAction string
	CampTime   time.Duration

	// Escalating ban durations per repeat offense (0 = permanent), and how
	// long earlier bans count as previous offenses.
	BanDurations []time.Duration
	BanMemory    time.Duration
}

func defaultConfig() *Config {
	return &Config{
		GuardEnabled:        false,
		DryRun:              true,
		Firewall:            "nft",
		SpyPort:             5555,
		BanPort:             5555,
		WhitelistClients:    []string{"SDR++", "SDR#"},
		ReconnectAction:     actBan,
		ReconnectWindow:     2 * time.Second,
		ReconnectMinSession: 30 * time.Second,
		ReconnectCount:      1,
		ReconnectPeriod:     10 * time.Minute,
		UnknownClientAction: actBan,
		QuotaAction:         actFlag,
		QuotaTime:           90 * time.Minute,
		QuotaWindow:         3 * time.Hour,
		CampAction:          actFlag,
		CampTime:            2 * time.Hour,
		BanDurations:        []time.Duration{time.Hour, 24 * time.Hour, 7 * 24 * time.Hour},
		BanMemory:           30 * 24 * time.Hour,
	}
}

// LoadConfig reads the file at path; an empty path returns the defaults.
func LoadConfig(path string) (*Config, error) {
	c := defaultConfig()
	if path == "" {
		return c, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected \"key = value\"", path, n)
		}
		if err := c.set(strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) set(k, v string) error {
	var err error
	switch k {
	case "admin_password_hash":
		if v != "" && !strings.HasPrefix(v, "pbkdf2-sha256$") {
			return fmt.Errorf("admin_password_hash: use the output of 'ssmon -hash-password'")
		}
		c.AdminPasswordHash = v
	case "guard_enabled":
		c.GuardEnabled, err = parseBool(v)
	case "dry_run":
		c.DryRun, err = parseBool(v)
	case "firewall":
		if v != "nft" && v != "none" {
			return fmt.Errorf("firewall: expected nft or none")
		}
		c.Firewall = v
	case "spyserver_port":
		c.SpyPort, err = parsePort(v)
	case "ban_port":
		c.BanPort, err = parsePort(v)
	case "whitelist_ips":
		c.WhitelistNets = nil
		for _, item := range splitList(v) {
			if !strings.Contains(item, "/") {
				if strings.Contains(item, ":") {
					item += "/128"
				} else {
					item += "/32"
				}
			}
			_, n, e := net.ParseCIDR(item)
			if e != nil {
				return fmt.Errorf("whitelist_ips: bad address %q", item)
			}
			c.WhitelistNets = append(c.WhitelistNets, n)
		}
	case "whitelist_clients":
		c.WhitelistClients = splitList(v)
	case "reconnect_action":
		c.ReconnectAction, err = parseAction(v)
	case "reconnect_window":
		c.ReconnectWindow, err = parseDur(v)
	case "reconnect_min_session":
		c.ReconnectMinSession, err = parseDur(v)
	case "reconnect_count":
		c.ReconnectCount, err = strconv.Atoi(v)
		if err == nil && c.ReconnectCount < 1 {
			err = fmt.Errorf("must be at least 1")
		}
	case "reconnect_period":
		c.ReconnectPeriod, err = parseDur(v)
	case "unknown_client_action":
		c.UnknownClientAction, err = parseAction(v)
	case "quota_action":
		c.QuotaAction, err = parseAction(v)
	case "quota_time":
		c.QuotaTime, err = parseDur(v)
	case "quota_window":
		c.QuotaWindow, err = parseDur(v)
	case "camp_action":
		c.CampAction, err = parseAction(v)
	case "camp_time":
		c.CampTime, err = parseDur(v)
	case "ban_durations":
		c.BanDurations = nil
		for _, item := range splitList(v) {
			if item == "permanent" || item == "0" {
				c.BanDurations = append(c.BanDurations, 0)
				continue
			}
			d, e := parseDur(item)
			if e != nil {
				return fmt.Errorf("ban_durations: %w", e)
			}
			c.BanDurations = append(c.BanDurations, d)
		}
		if len(c.BanDurations) == 0 {
			return fmt.Errorf("ban_durations: at least one value needed")
		}
	case "ban_memory":
		c.BanMemory, err = parseDur(v)
	default:
		return fmt.Errorf("unknown setting %q", k)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", k, err)
	}
	return nil
}

// Whitelisted reports whether ip is on the IP whitelist.
func (c *Config) Whitelisted(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	for _, n := range c.WhitelistNets {
		if n.Contains(p) {
			return true
		}
	}
	return false
}

// ClientAllowed reports whether a client name contains a whitelisted part.
func (c *Config) ClientAllowed(name string) bool {
	if name == "" {
		return false
	}
	ln := strings.ToLower(name)
	for _, w := range c.WhitelistClients {
		if strings.Contains(ln, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

func splitList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "true", "yes", "on", "1":
		return true, nil
	case "false", "no", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("expected true or false")
}

func parsePort(v string) (uint16, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("bad port %q", v)
	}
	return uint16(n), nil
}

func parseAction(v string) (string, error) {
	switch v = strings.ToLower(v); v {
	case actOff, actFlag, actBan:
		return v, nil
	}
	return "", fmt.Errorf("expected off, flag or ban")
}

// parseDur accepts Go durations plus whole days ("7d").
func parseDur(v string) (time.Duration, error) {
	if strings.HasSuffix(v, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad duration %q", v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad duration %q", v)
	}
	return d, nil
}
