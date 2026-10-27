package main

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// StreamURL is a parsed udp:// or rtp:// address, e.g.
// udp://239.2.2.2:1234?localaddr=192.168.10.10[&sources=10.0.0.1]
type StreamURL struct {
	Proto     string // "udp" or "rtp"
	Addr      netip.AddrPort
	LocalAddr string
	Source    string // optional SSM source
}

func ParseStreamURL(raw string) (StreamURL, error) {
	var su StreamURL
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return su, err
	}
	su.Proto = strings.ToLower(u.Scheme)
	if su.Proto != "udp" && su.Proto != "rtp" {
		return su, fmt.Errorf("scheme must be udp:// or rtp://")
	}
	host := strings.TrimPrefix(u.Host, "@")
	ap, err := netip.ParseAddrPort(host)
	if err != nil || !ap.Addr().Is4() || ap.Port() == 0 {
		return su, fmt.Errorf("address must be IPv4 ip:port")
	}
	su.Addr = ap
	q := u.Query()
	su.LocalAddr = q.Get("localaddr")
	if su.LocalAddr != "" {
		if a, err := netip.ParseAddr(su.LocalAddr); err != nil || !a.Is4() {
			return su, fmt.Errorf("invalid localaddr")
		}
	}
	su.Source = q.Get("sources")
	if su.Source != "" {
		if a, err := netip.ParseAddr(su.Source); err != nil || !a.Is4() {
			return su, fmt.Errorf("invalid sources")
		}
	}
	return su, nil
}

// String returns the canonical form, used as the unique key in the database.
func (su StreamURL) String() string {
	s := su.Proto + "://" + su.Addr.String()
	var q []string
	if su.LocalAddr != "" {
		q = append(q, "localaddr="+su.LocalAddr)
	}
	if su.Source != "" {
		q = append(q, "sources="+su.Source)
	}
	if len(q) > 0 {
		s += "?" + strings.Join(q, "&")
	}
	return s
}

func ifaceByAddr(addr string) (*net.Interface, error) {
	if addr == "" {
		return nil, nil
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range ifs {
		addrs, _ := ifs[i].Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.String() == addr {
				return &ifs[i], nil
			}
		}
	}
	return nil, fmt.Errorf("no interface has address %s", addr)
}

type IfaceInfo struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

func listIfaces() []IfaceInfo {
	var out []IfaceInfo
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, IfaceInfo{ifc.Name, ipn.IP.String()})
			}
		}
	}
	return out
}

// openInput returns a blocking UDP socket bound and joined to the input.
// IP_MULTICAST_ALL=0 makes the kernel deliver only groups joined on *this*
// socket/interface, so the same group:port can be received independently
// on several interfaces. SO_RCVTIMEO lets the reader notice stop requests.
func openInput(su StreamURL) (int, error) {
	ifi, err := ifaceByAddr(su.LocalAddr)
	if err != nil {
		return -1, err
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	fail := func(what string, err error) (int, error) {
		unix.Close(fd)
		return -1, fmt.Errorf("%s: %w", what, err)
	}
	group := su.Addr.Addr()
	mcast := group.IsMulticast()
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, 16<<20) != nil {
		unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 16<<20)
	}
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 500000})
	if mcast {
		unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_MULTICAST_ALL, 0)
	}
	if ifi != nil {
		unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifi.Name) // best effort
	}
	bind := group
	if !mcast && su.LocalAddr != "" && group.IsUnspecified() {
		bind = netip.MustParseAddr(su.LocalAddr)
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Port: int(su.Addr.Port()), Addr: bind.As4()}); err != nil {
		return fail("bind "+su.Addr.String(), err)
	}
	if mcast {
		var local [4]byte
		if su.LocalAddr != "" {
			local = netip.MustParseAddr(su.LocalAddr).As4()
		}
		if su.Source != "" {
			// struct ip_mreq_source { multiaddr; interface; sourceaddr }
			var mr [12]byte
			g, src := group.As4(), netip.MustParseAddr(su.Source).As4()
			copy(mr[0:], g[:])
			copy(mr[4:], local[:])
			copy(mr[8:], src[:])
			err = unix.SetsockoptString(fd, unix.IPPROTO_IP, unix.IP_ADD_SOURCE_MEMBERSHIP, string(mr[:]))
		} else {
			mr := &unix.IPMreqn{Multiaddr: group.As4(), Address: local}
			if ifi != nil {
				mr.Ifindex = int32(ifi.Index)
			}
			err = unix.SetsockoptIPMreqn(fd, unix.IPPROTO_IP, unix.IP_ADD_MEMBERSHIP, mr)
		}
		if err != nil {
			return fail("join "+group.String(), err)
		}
	}
	return fd, nil
}

// txSock is one sending socket per output interface, owned by one worker.
type txSock struct {
	fd   int
	msgs []mmsghdr
	iovs []unix.Iovec
	slab [][]byte
	n    int
	err  error
	bad  int // consecutive seconds in error, for healTx
}

const txQueue = 256

func openTx(localAddr string, ttl int) (*txSock, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var local [4]byte
	if localAddr != "" {
		local = netip.MustParseAddr(localAddr).As4()
		if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: local}); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("bind %s: %w", localAddr, err)
		}
		unix.SetsockoptInet4Addr(fd, unix.IPPROTO_IP, unix.IP_MULTICAST_IF, local)
	}
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 4<<20)
	unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_MULTICAST_TTL, ttl)
	unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_MULTICAST_LOOP, 1)
	t := &txSock{fd: fd, msgs: make([]mmsghdr, txQueue), iovs: make([]unix.Iovec, txQueue), slab: make([][]byte, txQueue)}
	for i := range t.slab {
		t.slab[i] = make([]byte, rtpHdrSz+dgSize)
		t.iovs[i].Base = &t.slab[i][0]
		t.msgs[i].hdr.Iov = &t.iovs[i]
		t.msgs[i].hdr.SetIovlen(1)
		t.msgs[i].hdr.Namelen = unix.SizeofSockaddrInet4
	}
	return t, nil
}

// next returns the buffer for the next queued datagram.
func (t *txSock) next(dst *unix.RawSockaddrInet4, size int) []byte {
	if t.n == txQueue {
		t.flush()
	}
	t.iovs[t.n].SetLen(size)
	t.msgs[t.n].hdr.Name = (*byte)(unsafe.Pointer(dst))
	t.n++
	return t.slab[t.n-1][:size]
}

func (t *txSock) flush() {
	sent := 0
	for sent < t.n {
		k, err := sendmmsg(t.fd, t.msgs[sent:t.n])
		if err != nil {
			t.err = err
			sent++ // skip the datagram that failed
			continue
		}
		sent += k
		t.err = nil
	}
	t.n = 0
}

func (t *txSock) close() { unix.Close(t.fd) }

func parsePort(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 && v < 65536 {
		return v
	}
	return def
}
