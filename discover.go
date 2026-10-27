package main

// Discover finds every MPEG-TS stream (any IP, any port) arriving on an
// interface by sniffing UDP with an AF_PACKET socket in all-multicast mode.
// Needs CAP_NET_RAW. Streams behind an IGMP-snooping switch only show up
// if something on this host has joined them; use the range scan for those.

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type found struct {
	proto string
	dst   netip.AddrPort
	src   netip.Addr
	ifidx int
	bytes uint64
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// classic BPF: accept the first 400 bytes of unfragmented (or first
// fragment) IPv4/UDP packets, drop everything else in the kernel.
var udpFilter = []unix.SockFilter{
	{Code: 0x30, K: 9},                    // ldb [9]          protocol
	{Code: 0x15, Jt: 0, Jf: 3, K: 17},     // jeq #17          UDP?
	{Code: 0x28, K: 6},                    // ldh [6]          flags/frag offset
	{Code: 0x45, Jt: 1, Jf: 0, K: 0x1fff}, // jset #0x1fff     non-first fragment -> drop
	{Code: 0x06, K: 400},                  // ret #400
	{Code: 0x06, K: 0},                    // ret #0
}

// classifyTS reports whether a UDP payload carries TS, raw or in RTP.
func classifyTS(p []byte) string {
	isTS := func(b []byte) bool {
		return len(b) >= 1 && b[0] == 0x47 && (len(b) <= tsSize || b[tsSize] == 0x47)
	}
	if isTS(p) {
		return "udp"
	}
	if len(p) >= 12 && p[0]&0xC0 == 0x80 {
		h := 12 + 4*int(p[0]&0x0F)
		if p[0]&0x10 != 0 && len(p) >= h+4 {
			h += 4 + 4*(int(p[h+2])<<8|int(p[h+3]))
		}
		if h < len(p) && isTS(p[h:]) {
			return "rtp"
		}
	}
	return ""
}

func Discover(localAddr string, d time.Duration) ([]scanResult, error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_IP)))
	if err != nil {
		if errors.Is(err, unix.EPERM) {
			return nil, errors.New("discovery needs CAP_NET_RAW: run as root or `sudo setcap cap_net_raw+ep mpts2spts`")
		}
		return nil, err
	}
	defer unix.Close(fd)

	ifs, _ := net.Interfaces()
	ifAddr := map[int]string{} // ifindex -> IPv4 used as localaddr
	for _, ifc := range ifs {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				if _, dup := ifAddr[ifc.Index]; !dup {
					ifAddr[ifc.Index] = ipn.IP.String()
				}
			}
		}
	}
	var only *net.Interface
	if localAddr != "" {
		if only, err = ifaceByAddr(localAddr); err != nil {
			return nil, err
		}
		ifAddr[only.Index] = localAddr
	}

	prog := unix.SockFprog{Len: uint16(len(udpFilter)), Filter: &udpFilter[0]}
	if err := unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &prog); err != nil {
		return nil, fmt.Errorf("attach filter: %w", err)
	}
	sll := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP)}
	if only != nil {
		sll.Ifindex = only.Index
	}
	if err := unix.Bind(fd, sll); err != nil {
		return nil, err
	}
	// Ask NICs to pass up all multicast frames, joined or not.
	for _, ifc := range ifs {
		if (only == nil || ifc.Index == only.Index) && ifc.Flags&net.FlagUp != 0 {
			mr := unix.PacketMreq{Ifindex: int32(ifc.Index), Type: unix.PACKET_MR_ALLMULTI}
			unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, &mr)
		}
	}
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 200000})

	type key struct {
		dst   netip.AddrPort
		ifidx int
	}
	streams := map[key]*found{}
	buf := make([]byte, 512)
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		n, from, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return nil, err
		}
		ll, ok := from.(*unix.SockaddrLinklayer)
		if !ok || ll.Pkttype == unix.PACKET_OUTGOING {
			continue
		}
		if only == nil && ll.Ifindex == loIndex(ifs) {
			continue
		}
		p := buf[:n]
		if len(p) < 20 || p[0]>>4 != 4 {
			continue
		}
		ihl := int(p[0]&0x0F) * 4
		if len(p) < ihl+8 {
			continue
		}
		total := uint64(p[2])<<8 | uint64(p[3])
		src := netip.AddrFrom4([4]byte(p[12:16]))
		dstIP := netip.AddrFrom4([4]byte(p[16:20]))
		u := p[ihl:]
		dst := netip.AddrPortFrom(dstIP, uint16(u[2])<<8|uint16(u[3]))
		k := key{dst, ll.Ifindex}
		f := streams[k]
		if f == nil {
			proto := classifyTS(u[8:])
			if proto == "" || len(streams) >= 1024 {
				continue
			}
			f = &found{proto: proto, dst: dst, src: src, ifidx: ll.Ifindex}
			streams[k] = f
		}
		f.bytes += total
	}

	// Probe each stream briefly to list its programs.
	var (
		res = []scanResult{}
		mu  sync.Mutex
		wg  sync.WaitGroup
	)
	secs := d.Seconds()
	for _, f := range streams {
		su := StreamURL{Proto: f.proto, Addr: f.dst, LocalAddr: ifAddr[f.ifidx]}
		if !f.dst.Addr().IsMulticast() {
			su.LocalAddr = "" // unicast to this host: bind to the destination address
		}
		kbps := float64(f.bytes) * 8 / secs / 1000
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := Probe(su, 3*time.Second)
			mu.Lock()
			res = append(res, scanResult{URL: su.String(), Kbps: kbps, Services: st.Services, Source: f.src.String()})
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(res, func(i, j int) bool { return res[i].URL < res[j].URL })
	return res, nil
}

func loIndex(ifs []net.Interface) int {
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 {
			return ifc.Index
		}
	}
	return -1
}
