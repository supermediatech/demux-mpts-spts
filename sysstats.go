package main

import (
	"bytes"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SysStats is host and process resource usage, read from /proc.
type SysStats struct {
	CPU      float64 `json:"cpu"`     // host CPU busy %, all cores
	AppCPU   float64 `json:"app_cpu"` // this process, % of one core (like top)
	MemUsed  uint64  `json:"mem_used"`
	MemTotal uint64  `json:"mem_total"`
	AppRSS   uint64  `json:"app_rss"`
	Cores    int     `json:"cores"`
}

const clkTck = 100 // USER_HZ on Linux

type sysSampler struct {
	mu                    sync.Mutex
	at                    time.Time
	total, idle, procTick uint64
	cur                   SysStats
}

// get returns stats averaged since the previous sample; samples closer than
// one second apart reuse the cached value so many UI clients cost nothing.
func (s *sysSampler) get() SysStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !s.at.IsZero() && now.Sub(s.at) < time.Second {
		return s.cur
	}
	total, idle := readCPU()
	proc := readProcTicks()
	if !s.at.IsZero() {
		if dt := total - s.total; dt > 0 {
			s.cur.CPU = float64(dt-(idle-s.idle)) / float64(dt) * 100
		}
		s.cur.AppCPU = float64(proc-s.procTick) / (now.Sub(s.at).Seconds() * clkTck) * 100
	}
	s.at, s.total, s.idle, s.procTick = now, total, idle, proc
	s.cur.MemTotal, s.cur.MemUsed = readMem()
	s.cur.AppRSS = readRSS()
	s.cur.Cores = runtime.NumCPU()
	return s.cur
}

func readCPU() (total, idle uint64) {
	b, _ := os.ReadFile("/proc/stat")
	line, _, _ := bytes.Cut(b, []byte("\n"))
	f := strings.Fields(string(line))
	for i := 1; i < len(f) && i <= 8; i++ { // user..steal
		v, _ := strconv.ParseUint(f[i], 10, 64)
		total += v
		if i == 4 || i == 5 { // idle, iowait
			idle += v
		}
	}
	return
}

func readProcTicks() uint64 {
	b, _ := os.ReadFile("/proc/self/stat")
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 13 {
		return 0
	}
	u, _ := strconv.ParseUint(f[11], 10, 64) // utime
	k, _ := strconv.ParseUint(f[12], 10, 64) // stime
	return u + k
}

func readMem() (total, used uint64) {
	b, _ := os.ReadFile("/proc/meminfo")
	var avail uint64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	return total, total - avail
}

func readRSS() uint64 {
	b, _ := os.ReadFile("/proc/self/statm")
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(f[1], 10, 64)
	return v * uint64(os.Getpagesize())
}
