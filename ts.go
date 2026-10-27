package main

// MPEG-TS / PSI primitives. Nothing here allocates on the per-packet path.

import (
	"strings"
	"unicode/utf8"
)

const (
	tsSize   = 188
	tsPerDG  = 7
	dgSize   = tsSize * tsPerDG
	pidPAT   = 0x0000
	pidSDT   = 0x0011
	pidTDT   = 0x0014
	pidNull  = 0x1FFF
	maxSect  = 1024
	rtpHdrSz = 12
)

var crcTab [256]uint32

func init() {
	for i := uint32(0); i < 256; i++ {
		c := i << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		crcTab[i] = c
	}
}

func crc32mpeg(b []byte) uint32 {
	c := uint32(0xFFFFFFFF)
	for _, v := range b {
		c = c<<8 ^ crcTab[byte(c>>24)^v]
	}
	return c
}

// sectionAsm reassembles PSI sections on one PID.
type sectionAsm struct {
	buf    [maxSect]byte
	n      int
	active bool
}

// feed consumes one TS packet and calls cb for every complete, CRC-valid
// section. The slice passed to cb is only valid during the call.
func (s *sectionAsm) feed(p []byte, cb func([]byte)) {
	afc := p[3] >> 4 & 3
	if afc&1 == 0 {
		return
	}
	off := 4
	if afc&2 != 0 {
		off += 1 + int(p[4])
	}
	if off >= tsSize {
		return
	}
	pl := p[off:tsSize]
	if p[1]&0x40 == 0 {
		if s.active {
			s.add(pl, cb)
		}
		return
	}
	ptr := int(pl[0])
	pl = pl[1:]
	if ptr > len(pl) {
		s.active = false
		return
	}
	if s.active {
		s.add(pl[:ptr], cb)
	}
	pl = pl[ptr:]
	for len(pl) > 0 && pl[0] != 0xFF {
		s.n = 0
		s.active = true
		used := s.add(pl, cb)
		if s.active {
			return
		}
		pl = pl[used:]
	}
	s.active = false
}

func (s *sectionAsm) add(d []byte, cb func([]byte)) int {
	prev := s.n
	take := copy(s.buf[s.n:], d)
	s.n += take
	if s.n < 3 {
		return take
	}
	tot := 3 + (int(s.buf[1]&0x0F)<<8 | int(s.buf[2]))
	if tot < 12 || tot > maxSect {
		s.active = false
		return len(d)
	}
	if s.n < tot {
		return take
	}
	s.active = false
	if crc32mpeg(s.buf[:tot]) == 0 {
		cb(s.buf[:tot])
	}
	return tot - prev
}

// packetize writes a PSI section as TS packets on pid, appending to dst.
func packetize(dst []byte, pid uint16, cc *uint8, sec []byte) []byte {
	first := true
	for len(sec) > 0 || first {
		var pk [tsSize]byte
		pk[0] = 0x47
		pk[1] = byte(pid>>8) & 0x1F
		pk[2] = byte(pid)
		pk[3] = 0x10 | *cc
		*cc = (*cc + 1) & 0x0F
		i := 4
		if first {
			pk[1] |= 0x40
			pk[4] = 0
			i = 5
			first = false
		}
		n := copy(pk[i:], sec)
		sec = sec[n:]
		for j := i + n; j < tsSize; j++ {
			pk[j] = 0xFF
		}
		dst = append(dst, pk[:]...)
	}
	return dst
}

func putCRC(sec []byte) {
	c := crc32mpeg(sec[:len(sec)-4])
	n := len(sec)
	sec[n-4], sec[n-3], sec[n-2], sec[n-1] = byte(c>>24), byte(c>>16), byte(c>>8), byte(c)
}

// buildPAT returns a single-program PAT section. The version is derived from
// the PMT PID so it is identical across restarts and still changes whenever
// the content does.
func buildPAT(tsid, prog, pmtPID uint16) []byte {
	s := make([]byte, 16)
	s[0] = 0x00
	s[1], s[2] = 0xB0, 13
	s[3], s[4] = byte(tsid>>8), byte(tsid)
	s[5] = 0xC1 | byte(pmtPID&0x1F)<<1
	s[6], s[7] = 0, 0
	s[8], s[9] = byte(prog>>8), byte(prog)
	s[10], s[11] = 0xE0|byte(pmtPID>>8), byte(pmtPID)
	putCRC(s)
	return s
}

// rewritePMT copies a source PMT and sets its program_number.
func rewritePMT(src []byte, prog uint16) []byte {
	s := append([]byte(nil), src...)
	s[3], s[4] = byte(prog>>8), byte(prog)
	putCRC(s)
	return s
}

// buildSDT returns a single-service SDT-actual section. Version is a hash of
// the content so it is stable across restarts.
func buildSDT(tsid, onid, sid uint16, stype byte, provider, name string) []byte {
	pv := encodeDVBString(provider, 80)
	nm := encodeDVBString(name, 80)
	desc := []byte{0x48, byte(3 + len(pv) + len(nm)), stype, byte(len(pv))}
	desc = append(desc, pv...)
	desc = append(desc, byte(len(nm)))
	desc = append(desc, nm...)

	s := []byte{0x42, 0, 0, byte(tsid >> 8), byte(tsid), 0xC1, 0, 0, byte(onid >> 8), byte(onid), 0xFF}
	s = append(s, byte(sid>>8), byte(sid), 0xFC,
		0x80|byte(len(desc)>>8&0x0F), byte(len(desc))) // running, free_CA=0
	s = append(s, desc...)
	s = append(s, 0, 0, 0, 0)
	l := len(s) - 3
	s[1], s[2] = 0xF0|byte(l>>8), byte(l)
	s[5] = 0xC1 | byte(crc32mpeg(s[:len(s)-4])&0x1F)<<1
	putCRC(s)
	return s
}

func encodeDVBString(s string, max int) []byte {
	prefix := 0
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			prefix = 1 // UTF-8 per EN 300 468 Annex A
			break
		}
	}
	for len(s)+prefix > max {
		_, sz := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-sz]
	}
	if prefix == 1 {
		return append([]byte{0x15}, s...)
	}
	return []byte(s)
}

func decodeDVBString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	utf := false
	switch {
	case b[0] == 0x15:
		utf, b = true, b[1:]
	case b[0] == 0x10 && len(b) >= 3:
		b = b[3:]
	case b[0] == 0x1F && len(b) >= 2:
		b = b[2:]
	case b[0] < 0x20:
		b = b[1:]
	}
	var sb strings.Builder
	if utf {
		for _, r := range string(b) {
			if r >= 0x20 && r != 0x7F && !(r >= 0x80 && r < 0xA0) {
				sb.WriteRune(r)
			}
		}
	} else {
		for _, c := range b {
			if c >= 0x20 && c != 0x7F && !(c >= 0x80 && c < 0xA0) {
				sb.WriteRune(rune(c)) // latin-1 approximation
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

func streamKind(st byte, desc []byte) string {
	switch st {
	case 0x01, 0x02, 0x10, 0x1B, 0x24, 0x42, 0xEA, 0x33:
		return "video"
	case 0x03, 0x04, 0x0F, 0x11, 0x81, 0x87, 0x1C:
		return "audio"
	case 0x05, 0x0B, 0x0C, 0x0D:
		return "data"
	case 0x06:
		for i := 0; i+2 <= len(desc); i += 2 + int(desc[i+1]) {
			switch desc[i] {
			case 0x6A, 0x7A, 0x7C, 0x7F:
				return "audio"
			case 0x59:
				return "subtitle"
			case 0x56, 0x46:
				return "teletext"
			}
		}
		return "private"
	}
	return "other"
}

// caPIDs returns ECM PIDs from CA descriptors (tag 0x09) in a descriptor loop.
func caPIDs(dst []uint16, desc []byte) []uint16 {
	for i := 0; i+2 <= len(desc); {
		tag, l := desc[i], int(desc[i+1])
		if i+2+l > len(desc) {
			break
		}
		if tag == 0x09 && l >= 4 {
			dst = append(dst, uint16(desc[i+4]&0x1F)<<8|uint16(desc[i+5]))
		}
		i += 2 + l
	}
	return dst
}
