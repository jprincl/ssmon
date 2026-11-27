package main

// Bans are enforced with a dedicated nftables table:
//
//	table inet ssmon {
//	  set banned { type ipv4_addr; flags timeout; }
//	  chain prerouting {
//	    type filter hook prerouting priority -300; policy accept;
//	    ip saddr @banned tcp dport <ban_port> drop
//	  }
//	}
//
// Priority -300 (raw) runs before conntrack and DNAT, so both new and
// already established connections of a banned IP are dropped, and only the
// SpyServer port is affected (no risk of locking yourself out of SSH).
// Set elements carry their own timeout, so temporary bans expire in the
// kernel without any cleanup. The table is independent of ufw.

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"
	"time"
)

type Firewall interface {
	// Init (re)creates the ban table and loads the currently active bans
	// (ip -> remaining duration, 0 = permanent).
	Init(active map[string]time.Duration) error
	Ban(ip string, d time.Duration) error
	Unban(ip string) error
}

func newFirewall(kind string, banPort uint16) Firewall {
	if kind == "nft" {
		return &nftFirewall{port: banPort}
	}
	return logFirewall{}
}

type nftFirewall struct{ port uint16 }

func (f *nftFirewall) run(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

func nftTimeout(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	s := int64(d.Seconds())
	if s < 1 {
		s = 1
	}
	return fmt.Sprintf(" timeout %ds", s)
}

func (f *nftFirewall) Init(active map[string]time.Duration) error {
	var b strings.Builder
	// "add" then "delete" makes the delete safe when the table is missing.
	b.WriteString("add table inet ssmon\ndelete table inet ssmon\n")
	fmt.Fprintf(&b, `table inet ssmon {
	set banned { type ipv4_addr; flags timeout; }
	chain prerouting {
		type filter hook prerouting priority -300; policy accept;
		ip saddr @banned tcp dport %d drop
	}
}
`, f.port)
	for ip, d := range active {
		if net.ParseIP(ip).To4() == nil {
			continue
		}
		fmt.Fprintf(&b, "add element inet ssmon banned { %s%s }\n", ip, nftTimeout(d))
	}
	return f.run(b.String())
}

func (f *nftFirewall) Ban(ip string, d time.Duration) error {
	if net.ParseIP(ip).To4() == nil {
		return fmt.Errorf("only IPv4 addresses can be banned: %q", ip)
	}
	// Remove first so a new timeout replaces the old one; the delete fails
	// harmlessly when the address is not in the set.
	_ = f.run(fmt.Sprintf("delete element inet ssmon banned { %s }\n", ip))
	return f.run(fmt.Sprintf("add element inet ssmon banned { %s%s }\n", ip, nftTimeout(d)))
}

func (f *nftFirewall) Unban(ip string) error {
	if net.ParseIP(ip).To4() == nil {
		return fmt.Errorf("bad address %q", ip)
	}
	err := f.run(fmt.Sprintf("delete element inet ssmon banned { %s }\n", ip))
	if err != nil && strings.Contains(err.Error(), "No such file") {
		return nil // already expired
	}
	return err
}

// logFirewall only logs (firewall = none): bans are recorded but not enforced.
type logFirewall struct{}

func (logFirewall) Init(active map[string]time.Duration) error {
	log.Printf("firewall: none - %d active bans are recorded but not enforced", len(active))
	return nil
}
func (logFirewall) Ban(ip string, d time.Duration) error {
	log.Printf("firewall: none - would ban %s for %v", ip, d)
	return nil
}
func (logFirewall) Unban(ip string) error {
	log.Printf("firewall: none - would unban %s", ip)
	return nil
}
