package main

// Blocking recvmmsg/sendmmsg on raw fds. Bypassing Go's netpoller removes the
// EAGAIN + epoll round trip per batch and the per-datagram address
// allocations of x/net, which halves CPU at high packet rates.

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

type mmsghdr struct {
	hdr unix.Msghdr
	len uint32
	_   [4]byte
}

type rxRing struct {
	msgs []mmsghdr
	iovs []unix.Iovec
	bufs [][]byte
}

func newRxRing(n, size int) *rxRing {
	r := &rxRing{msgs: make([]mmsghdr, n), iovs: make([]unix.Iovec, n), bufs: make([][]byte, n)}
	for i := range r.msgs {
		r.bufs[i] = make([]byte, size)
		r.iovs[i].Base = &r.bufs[i][0]
		r.iovs[i].SetLen(size)
		r.msgs[i].hdr.Iov = &r.iovs[i]
		r.msgs[i].hdr.SetIovlen(1)
	}
	return r
}

// recv blocks until at least one datagram (or SO_RCVTIMEO) and returns the
// number received; a timeout returns 0, nil.
func (r *rxRing) recv(fd int) (int, error) {
	for {
		n, _, e := unix.Syscall6(unix.SYS_RECVMMSG, uintptr(fd), uintptr(unsafe.Pointer(&r.msgs[0])),
			uintptr(len(r.msgs)), unix.MSG_WAITFORONE, 0, 0)
		switch e {
		case 0:
			return int(n), nil
		case unix.EINTR:
			continue
		case unix.EAGAIN:
			return 0, nil
		}
		return 0, e
	}
}

func (r *rxRing) data(i int) []byte { return r.bufs[i][:r.msgs[i].len] }

func sendmmsg(fd int, msgs []mmsghdr) (int, error) {
	for {
		n, _, e := unix.Syscall6(unix.SYS_SENDMMSG, uintptr(fd), uintptr(unsafe.Pointer(&msgs[0])),
			uintptr(len(msgs)), 0, 0, 0)
		if e == unix.EINTR {
			continue
		}
		if e != 0 {
			return int(n), e
		}
		return int(n), nil
	}
}

func sockaddr4(ip [4]byte, port uint16) *unix.RawSockaddrInet4 {
	sa := &unix.RawSockaddrInet4{Family: unix.AF_INET, Addr: ip}
	p := (*[2]byte)(unsafe.Pointer(&sa.Port))
	p[0], p[1] = byte(port>>8), byte(port)
	return sa
}
