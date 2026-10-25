package main

// Passive capture of SpyServer directory reports.
//
// Live mode uses an AF_PACKET socket with a kernel BPF filter (IPv4, TCP,
// destination port = directory port), so only the report packets ever reach
// userspace - the IQ stream on the same interface costs nothing.
// Replay mode reads a classic pcap file (tcpdump -w) for testing.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	ethPAll  = 0x0003
	ethPIPv4 = 0x0800
)

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// bpfTCPDstPort accepts unfragmented IPv4/TCP packets to the given port.
// Offsets are relative to the IP header (SOCK_DGRAM packet socket).
func bpfTCPDstPort(port uint16) []syscall.SockFilter {
	return []syscall.SockFilter{
		{Code: 0x30, K: 0},                          // 0: ldb [0]
		{Code: 0x54, K: 0xf0},                       // 1: and #0xf0
		{Code: 0x15, Jt: 0, Jf: 8, K: 0x40},         // 2: jeq #0x40 (IPv4) else drop
		{Code: 0x30, K: 9},                          // 3: ldb [9]
		{Code: 0x15, Jt: 0, Jf: 6, K: 6},            // 4: jeq #6 (TCP) else drop
		{Code: 0x28, K: 6},                          // 5: ldh [6]
		{Code: 0x45, Jt: 4, Jf: 0, K: 0x1fff},       // 6: jset fragment offset -> drop
		{Code: 0xb1, K: 0},                          // 7: ldxb 4*([0]&0xf)
		{Code: 0x48, K: 2},                          // 8: ldh [x+2] (TCP dst port)
		{Code: 0x15, Jt: 0, Jf: 1, K: uint32(port)}, // 9: jeq #port
		{Code: 0x06, K: 0xffff},                     // 10: accept
		{Code: 0x06, K: 0},                          // 11: drop
	}
}

// captureLive blocks forever, calling onPacket for each matching IPv4 packet
// and onIdle at least every few seconds.
func captureLive(iface string, port uint16, onPacket func(time.Time, []byte), onIdle func(time.Time)) error {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return err
	}
	// Protocol 0 = receive nothing until bound, so the filter is in place first.
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("packet socket (needs CAP_NET_RAW): %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.AttachLsf(fd, bpfTCPDstPort(port)); err != nil {
		return fmt.Errorf("attach BPF filter: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: htons(ethPAll), Ifindex: ifi.Index}); err != nil {
		return fmt.Errorf("bind to %s: %w", iface, err)
	}
	tv := syscall.Timeval{Sec: 5}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return err
	}

	buf := make([]byte, 65536)
	lastIdle := time.Now()
	for {
		n, from, err := syscall.Recvfrom(fd, buf, 0)
		now := time.Now()
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				onIdle(now)
				lastIdle = now
				continue
			}
			return err
		}
		if ll, ok := from.(*syscall.SockaddrLinklayer); ok && ll.Protocol != htons(ethPIPv4) {
			continue
		}
		onPacket(now, buf[:n])
		if now.Sub(lastIdle) > 5*time.Second {
			onIdle(now)
			lastIdle = now
		}
	}
}

// replayPcap feeds IPv4 packets from a classic pcap file (not pcapng).
func replayPcap(path string, onPacket func(time.Time, []byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gh := make([]byte, 24)
	if _, err := io.ReadFull(f, gh); err != nil {
		return fmt.Errorf("pcap header: %w", err)
	}
	var bo binary.ByteOrder
	nano := false
	switch {
	case bytes.Equal(gh[:4], []byte{0xd4, 0xc3, 0xb2, 0xa1}):
		bo = binary.LittleEndian
	case bytes.Equal(gh[:4], []byte{0x4d, 0x3c, 0xb2, 0xa1}):
		bo, nano = binary.LittleEndian, true
	case bytes.Equal(gh[:4], []byte{0xa1, 0xb2, 0xc3, 0xd4}):
		bo = binary.BigEndian
	case bytes.Equal(gh[:4], []byte{0xa1, 0xb2, 0x3c, 0x4d}):
		bo, nano = binary.BigEndian, true
	default:
		return errors.New("not a classic pcap file (pcapng is not supported; use tcpdump -w)")
	}
	linkType := bo.Uint32(gh[20:24]) & 0x0fffffff

	rh := make([]byte, 16)
	for {
		if _, err := io.ReadFull(f, rh); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		sec, frac, incl := bo.Uint32(rh[0:4]), bo.Uint32(rh[4:8]), bo.Uint32(rh[8:12])
		data := make([]byte, incl)
		if _, err := io.ReadFull(f, data); err != nil {
			return nil
		}
		div := int64(time.Microsecond)
		if nano {
			div = 1
		}
		ts := time.Unix(int64(sec), int64(frac)*div)
		if ip := stripLinkLayer(linkType, data); ip != nil {
			onPacket(ts, ip)
		}
	}
}

func stripLinkLayer(linkType uint32, d []byte) []byte {
	switch linkType {
	case 12, 101, 228: // raw IP (e.g. WireGuard interfaces)
		return d
	case 0: // BSD loopback
		if len(d) > 4 {
			return d[4:]
		}
	case 1: // Ethernet
		if len(d) > 14 && binary.BigEndian.Uint16(d[12:14]) == ethPIPv4 {
			return d[14:]
		}
	case 113: // Linux cooked (tcpdump -i any)
		if len(d) > 16 && binary.BigEndian.Uint16(d[14:16]) == ethPIPv4 {
			return d[16:]
		}
	case 276: // Linux cooked v2
		if len(d) > 20 && binary.BigEndian.Uint16(d[0:2]) == ethPIPv4 {
			return d[20:]
		}
	}
	return nil
}

type tcpSegment struct {
	src, dst     [4]byte
	sport, dport uint16
	seq          uint32
	payload      []byte
}

func parseIPv4TCP(p []byte) (tcpSegment, bool) {
	var s tcpSegment
	if len(p) < 20 || p[0]>>4 != 4 || p[9] != 6 {
		return s, false
	}
	ihl := int(p[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(p[2:4]))
	if total > len(p) || total == 0 {
		total = len(p)
	}
	if ihl < 20 || ihl+20 > total {
		return s, false
	}
	copy(s.src[:], p[12:16])
	copy(s.dst[:], p[16:20])
	t := p[ihl:total]
	s.sport = binary.BigEndian.Uint16(t[0:2])
	s.dport = binary.BigEndian.Uint16(t[2:4])
	s.seq = binary.BigEndian.Uint32(t[4:8])
	doff := int(t[12]>>4) * 4
	if doff < 20 || doff > len(t) {
		return s, false
	}
	s.payload = t[doff:]
	return s, true
}

// reassembler rebuilds HTTP POST /register requests that may span several
// TCP segments (normally they fit in one).
type reassembler struct {
	port  uint16
	flows map[flowKey]*flow
	// Requests already delivered, so a packet seen twice (loopback, or a
	// retransmitted first segment) is not reported again.
	done map[doneKey]time.Time
}

type doneKey struct {
	flowKey
	seq uint32
}

type flowKey struct {
	src, dst     [4]byte
	sport, dport uint16
}

type flow struct {
	startSeq uint32
	buf      []byte
	started  time.Time
}

func newReassembler(port uint16) *reassembler {
	return &reassembler{port: port, flows: map[flowKey]*flow{}, done: map[doneKey]time.Time{}}
}

var registerPrefix = []byte("POST /register")

// feed returns a complete request body when one has been assembled.
func (r *reassembler) feed(ts time.Time, s tcpSegment) []byte {
	if s.dport != r.port || len(s.payload) == 0 {
		return nil
	}
	k := flowKey{s.src, s.dst, s.sport, s.dport}
	f := r.flows[k]
	if bytes.HasPrefix(s.payload, registerPrefix) {
		if _, dup := r.done[doneKey{k, s.seq}]; dup {
			return nil
		}
		f = &flow{startSeq: s.seq, started: ts}
		r.flows[k] = f
	}
	if f == nil {
		return nil
	}
	off := int(s.seq - f.startSeq) // wraps correctly in uint32
	switch {
	case off == len(f.buf):
		f.buf = append(f.buf, s.payload...)
	case off < len(f.buf): // retransmission, possibly with new data at the end
		if end := off + len(s.payload); end > len(f.buf) {
			f.buf = append(f.buf, s.payload[len(f.buf)-off:]...)
		}
	default: // gap (out of order) - wait for retransmission
		return nil
	}
	if len(f.buf) > 64<<10 {
		delete(r.flows, k)
		return nil
	}
	hdrEnd := bytes.Index(f.buf, []byte("\r\n\r\n"))
	if hdrEnd < 0 {
		return nil
	}
	clen := 0
	for _, line := range strings.Split(string(f.buf[:hdrEnd]), "\r\n") {
		if name, val, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			clen, _ = strconv.Atoi(strings.TrimSpace(val))
		}
	}
	body := f.buf[hdrEnd+4:]
	if len(body) < clen {
		return nil
	}
	delete(r.flows, k)
	r.done[doneKey{k, f.startSeq}] = ts
	return append([]byte(nil), body[:clen]...)
}

func (r *reassembler) expire(now time.Time) {
	for k, f := range r.flows {
		if now.Sub(f.started) > 30*time.Second {
			delete(r.flows, k)
		}
	}
	for k, t := range r.done {
		if now.Sub(t) > 2*time.Minute {
			delete(r.done, k)
		}
	}
}

// parseReport turns the "Key=Value" lines of a report body into a map.
func parseReport(body []byte) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if k, v, ok := strings.Cut(line, "="); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}
