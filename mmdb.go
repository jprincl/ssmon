package main

// Minimal read-only MaxMind DB (MMDB) reader - just enough for City and ASN
// lookups (DB-IP Lite, GeoLite2). Format spec:
// https://maxmind.github.io/MaxMind-DB/

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
)

var mmdbMetaMarker = []byte("\xAB\xCD\xEFMaxMind.com")

type mmdb struct {
	buf        []byte
	data       []byte
	nodeCount  uint
	recordSize uint
	ipVersion  uint
	ipv4Start  uint
	dbType     string
}

func openMMDB(path string) (*mmdb, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndex(buf, mmdbMetaMarker)
	if i < 0 {
		return nil, errors.New("not an MMDB file (metadata marker missing)")
	}
	md := mmdbDecoder{buf: buf[i+len(mmdbMetaMarker):]}
	v, _, err := md.decodeSafe(0)
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	meta, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("metadata is not a map")
	}
	db := &mmdb{
		buf:        buf,
		nodeCount:  uint(asUint(meta["node_count"])),
		recordSize: uint(asUint(meta["record_size"])),
		ipVersion:  uint(asUint(meta["ip_version"])),
	}
	db.dbType, _ = meta["database_type"].(string)
	if db.recordSize != 24 && db.recordSize != 28 && db.recordSize != 32 {
		return nil, fmt.Errorf("unsupported record size %d", db.recordSize)
	}
	treeSize := db.nodeCount * db.recordSize / 4 // two records per node
	dataStart := treeSize + 16                   // 16-byte zero separator
	if dataStart > uint(i) {
		return nil, errors.New("search tree larger than file")
	}
	db.data = buf[dataStart:i]

	// IPv4 addresses live under ::/96 in IPv6 trees.
	if db.ipVersion == 6 {
		node := uint(0)
		for k := 0; k < 96 && node < db.nodeCount; k++ {
			node = db.readNode(node, 0)
		}
		db.ipv4Start = node
	}
	return db, nil
}

func (db *mmdb) readNode(node, bit uint) uint {
	b := db.buf
	switch db.recordSize {
	case 24:
		o := node*6 + bit*3
		return uint(b[o])<<16 | uint(b[o+1])<<8 | uint(b[o+2])
	case 28:
		o := node * 7
		if bit == 0 {
			return uint(b[o+3]&0xF0)<<20 | uint(b[o])<<16 | uint(b[o+1])<<8 | uint(b[o+2])
		}
		return uint(b[o+3]&0x0F)<<24 | uint(b[o+4])<<16 | uint(b[o+5])<<8 | uint(b[o+6])
	default: // 32
		o := node*8 + bit*4
		return uint(binary.BigEndian.Uint32(b[o : o+4]))
	}
}

// Lookup returns the record for ip, or nil when the address is not in the DB.
func (db *mmdb) Lookup(ip net.IP) (rec map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			rec, err = nil, fmt.Errorf("corrupt database: %v", r)
		}
	}()
	var addr []byte
	node := uint(0)
	if v4 := ip.To4(); v4 != nil {
		addr = v4
		if db.ipVersion == 6 {
			node = db.ipv4Start
		}
	} else {
		if db.ipVersion == 4 {
			return nil, nil
		}
		addr = ip.To16()
	}
	for i := 0; i < len(addr)*8 && node < db.nodeCount; i++ {
		bit := uint(addr[i>>3]>>(7-uint(i&7))) & 1
		node = db.readNode(node, bit)
	}
	if node == db.nodeCount {
		return nil, nil
	}
	if node < db.nodeCount {
		return nil, errors.New("search tree did not terminate")
	}
	d := mmdbDecoder{buf: db.data}
	v, _, err := d.decode(node - db.nodeCount - 16)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return m, nil
}

type mmdbDecoder struct{ buf []byte }

func (d *mmdbDecoder) decodeSafe(off uint) (v any, next uint, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("decode: %v", r)
		}
	}()
	return d.decode(off)
}

func (d *mmdbDecoder) decode(off uint) (any, uint, error) {
	b := d.buf
	ctrl := b[off]
	off++
	typ := uint(ctrl >> 5)

	if typ == 1 { // pointer: follow it, but continue after the pointer itself
		ss := (ctrl >> 3) & 3
		vvv := uint(ctrl & 7)
		var p uint
		switch ss {
		case 0:
			p = vvv<<8 | uint(b[off])
			off++
		case 1:
			p = (vvv<<16 | uint(b[off])<<8 | uint(b[off+1])) + 2048
			off += 2
		case 2:
			p = (vvv<<24 | uint(b[off])<<16 | uint(b[off+1])<<8 | uint(b[off+2])) + 526336
			off += 3
		default:
			p = uint(binary.BigEndian.Uint32(b[off : off+4]))
			off += 4
		}
		v, _, err := d.decode(p)
		return v, off, err
	}

	if typ == 0 { // extended type
		typ = 7 + uint(b[off])
		off++
	}

	size := uint(ctrl & 0x1f)
	switch size {
	case 29:
		size = 29 + uint(b[off])
		off++
	case 30:
		size = 285 + (uint(b[off])<<8 | uint(b[off+1]))
		off += 2
	case 31:
		size = 65821 + (uint(b[off])<<16 | uint(b[off+1])<<8 | uint(b[off+2]))
		off += 3
	}

	switch typ {
	case 2: // UTF-8 string
		return string(b[off : off+size]), off + size, nil
	case 3: // double
		if size != 8 {
			return nil, 0, errors.New("bad double size")
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b[off : off+8])), off + 8, nil
	case 4: // bytes
		return append([]byte(nil), b[off:off+size]...), off + size, nil
	case 5, 6, 9, 10: // uint16/32/64/128 (128 truncated to the low 64 bits)
		var u uint64
		for _, c := range b[off : off+size] {
			u = u<<8 | uint64(c)
		}
		return u, off + size, nil
	case 8: // int32
		var u uint32
		for _, c := range b[off : off+size] {
			u = u<<8 | uint32(c)
		}
		return int64(int32(u)), off + size, nil
	case 7: // map
		m := make(map[string]any, size)
		for i := uint(0); i < size; i++ {
			k, n, err := d.decode(off)
			if err != nil {
				return nil, 0, err
			}
			v, n2, err := d.decode(n)
			if err != nil {
				return nil, 0, err
			}
			ks, _ := k.(string)
			m[ks] = v
			off = n2
		}
		return m, off, nil
	case 11: // array
		a := make([]any, 0, size)
		for i := uint(0); i < size; i++ {
			v, n, err := d.decode(off)
			if err != nil {
				return nil, 0, err
			}
			a = append(a, v)
			off = n
		}
		return a, off, nil
	case 14: // boolean, value is in the size field
		return size != 0, off, nil
	case 15: // float
		if size != 4 {
			return nil, 0, errors.New("bad float size")
		}
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b[off : off+4]))), off + 4, nil
	}
	return nil, 0, fmt.Errorf("unsupported data type %d", typ)
}

func asUint(v any) uint64 {
	switch x := v.(type) {
	case uint64:
		return x
	case int64:
		return uint64(x)
	case float64:
		return uint64(x)
	}
	return 0
}

// mmdbPath walks nested maps: mmdbPath(rec, "country", "names", "en").
func mmdbPath(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}
