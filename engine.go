package main

import (
	"bytes"
	"hash/fnv"
	"log"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const (
	rxBatch = 64
	rxSize  = 2048
	pace    = 2 * time.Millisecond

	kindNone = 0
	kindPAT  = 1
	kindPMT  = 2
	kindSDT  = 3
)

// OutCfg is everything a worker needs to produce one SPTS.
type OutCfg struct {
	ID       int64
	SrcSID   uint16
	OutSID   uint16
	TSID     uint16
	ONID     uint16
	Name     string
	Provider string
	SType    byte
	URL      StreamURL
	TTL      int
}

type program struct {
	sid     uint16
	pmtPID  uint16
	pmt     []byte
	pcrPID  uint16
	pids    []uint16
	streams []StreamInfo
}

type sdtEntry struct {
	name, provider string
	stype          byte
}

type outRT struct {
	cfg     OutCfg
	dst     *unix.RawSockaddrInet4
	tx      *txSock
	rtp     bool
	ssrc    uint32
	seq     uint16
	buf     [dgSize]byte
	n       int
	patCC   uint8
	pmtCC   uint8
	sdtCC   uint8
	sdtPkts []byte
	pmtSrc  []byte // source PMT the cached psi packets came from
	pmtPID  uint16
	psi     []byte // cached PAT+PMT packets
	clock   *uint32
	pkts    uint64
	prev    uint64
}

// Status types exposed to the web UI.
type StreamInfo struct {
	PID  uint16 `json:"pid"`
	Type byte   `json:"type"`
	Kind string `json:"kind"`
}

type ServiceInfo struct {
	SID      uint16       `json:"sid"`
	Name     string       `json:"name"`
	Provider string       `json:"provider"`
	Type     byte         `json:"type"`
	PMTPID   uint16       `json:"pmt_pid"`
	PCRPID   uint16       `json:"pcr_pid"`
	Streams  []StreamInfo `json:"streams"`
	Kbps     float64      `json:"kbps"`
}

type OutStatus struct {
	Kbps   float64 `json:"kbps"`
	Active bool    `json:"active"`
	Error  string  `json:"error,omitempty"`
}

type InputStatus struct {
	State    string              `json:"state"` // starting|ok|nosignal|error
	Error    string              `json:"error,omitempty"`
	Kbps     float64             `json:"kbps"`
	TSID     uint16              `json:"tsid"`
	ONID     uint16              `json:"onid"`
	Services []ServiceInfo       `json:"services"`
	Outputs  map[int64]OutStatus `json:"-"`
	Since    time.Time           `json:"-"`
}

type worker struct {
	id  int64
	url StreamURL

	pending atomic.Pointer[[]OutCfg]
	stopped atomic.Bool
	done    chan struct{}

	statusMu sync.RWMutex
	status   InputStatus

	// owned by the run goroutine
	pat     sectionAsm
	sdt     sectionAsm
	pmtAsm  map[uint16]*sectionAsm
	patCRC  uint32
	tsid    uint16
	onid    uint16
	haveSDT bool
	progs   map[uint16]*program
	sdtSvc  map[uint16]sdtEntry
	kind    [8192]uint8
	route   [8192][]*outRT
	cnt     [8192]uint32
	prevCnt [8192]uint32
	outs    map[int64]*outRT
	txs     map[string]*txSock
	onPATf  func([]byte)
	onPMTf  func([]byte)
	onSDTf  func([]byte)
	lastPkt time.Time
	rtpTS   uint32
}

func newWorker(id int64, su StreamURL, outs []OutCfg) *worker {
	w := &worker{id: id, url: su, done: make(chan struct{})}
	w.pending.Store(&outs)
	w.status = InputStatus{State: "starting", Since: time.Now()}
	go w.loop()
	return w
}

func (w *worker) setOutputs(outs []OutCfg) { w.pending.Store(&outs) }

// stop signals the worker; the reader notices within SO_RCVTIMEO.
func (w *worker) stop() { w.stopped.Store(true) }
func (w *worker) wait() { <-w.done }

func (w *worker) snapshot() InputStatus {
	w.statusMu.RLock()
	defer w.statusMu.RUnlock()
	return w.status
}

func (w *worker) setError(err error) {
	w.statusMu.Lock()
	w.status = InputStatus{State: "error", Error: err.Error(), Since: time.Now()}
	w.statusMu.Unlock()
}

func (w *worker) loop() {
	defer close(w.done)
	for !w.stopped.Load() {
		err := w.run()
		if w.stopped.Load() {
			break
		}
		if err != nil {
			log.Printf("input %d %s: %v", w.id, w.url, err)
			w.setError(err)
		}
		for i := 0; i < 50 && !w.stopped.Load(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func (w *worker) reset() {
	w.pat = sectionAsm{}
	w.sdt = sectionAsm{}
	w.pmtAsm = map[uint16]*sectionAsm{}
	w.patCRC, w.tsid, w.onid, w.haveSDT = 0, 0, 0, false
	w.progs = map[uint16]*program{}
	w.sdtSvc = map[uint16]sdtEntry{}
	w.kind = [8192]uint8{}
	w.kind[pidPAT] = kindPAT
	w.kind[pidSDT] = kindSDT
	w.route = [8192][]*outRT{}
	w.cnt = [8192]uint32{}
	w.prevCnt = [8192]uint32{}
	w.outs = map[int64]*outRT{}
	w.txs = map[string]*txSock{}
	w.onPATf, w.onPMTf, w.onSDTf = w.onPAT, w.onPMT, w.onSDT
}

func (w *worker) run() error {
	fd, err := openInput(w.url)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	// The reader blocks in recvmmsg on its own OS thread, like a C loop.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	w.reset()
	defer func() {
		for _, t := range w.txs {
			if t.fd >= 0 {
				t.close()
			}
		}
	}()
	w.applyConfig(*w.pending.Swap(nil))

	rx := newRxRing(rxBatch, rxSize)
	lastTick, lastSDT := time.Now(), time.Time{}
	w.publish(0)

	for !w.stopped.Load() {
		n, err := rx.recv(fd)
		if err != nil {
			return err
		}
		now := time.Now()
		w.rtpTS = uint32(now.UnixNano() / 11111) // 90 kHz
		for i := 0; i < n; i++ {
			w.handleDgram(rx.data(i))
		}
		if n > 0 {
			w.lastPkt = now
		}
		if p := w.pending.Swap(nil); p != nil {
			w.applyConfig(*p)
		}
		if now.Sub(lastSDT) >= 2*time.Second {
			lastSDT = now
			for _, o := range w.outs {
				if w.progs[o.cfg.SrcSID] != nil && w.progs[o.cfg.SrcSID].pmt != nil {
					restamp(o.sdtPkts, &o.sdtCC)
					o.putRaw(o.sdtPkts)
				}
			}
		}
		w.flushTx()
		if n > 0 && n < rxBatch/2 {
			// Let datagrams queue in the socket buffer so the next recvmmsg
			// drains many at once: trades <=2ms latency for far fewer
			// syscalls and scheduler wakeups.
			time.Sleep(pace)
		}
		if dt := now.Sub(lastTick); dt >= time.Second {
			lastTick = now
			w.publish(dt.Seconds())
		}
	}
	return nil
}

func (w *worker) flushTx() {
	for _, t := range w.txs {
		if t.n > 0 {
			t.flush()
		}
	}
}

func (w *worker) handleDgram(p []byte) {
	// strip RTP header if present (any payload type)
	if len(p) >= 12 && p[0] != 0x47 && p[0]&0xC0 == 0x80 {
		h := 12 + 4*int(p[0]&0x0F)
		if p[0]&0x10 != 0 {
			if len(p) < h+4 {
				return
			}
			h += 4 + 4*(int(p[h+2])<<8|int(p[h+3]))
		}
		l := len(p)
		if p[0]&0x20 != 0 {
			l -= int(p[l-1])
		}
		if h >= l {
			return
		}
		p = p[h:l]
	}
	for len(p) >= tsSize {
		if p[0] != 0x47 {
			p = p[1:]
			continue
		}
		w.handleTS(p[:tsSize])
		p = p[tsSize:]
	}
}

func (w *worker) handleTS(p []byte) {
	pid := uint16(p[1]&0x1F)<<8 | uint16(p[2])
	w.cnt[pid]++
	switch w.kind[pid] {
	case kindPAT:
		w.pat.feed(p, w.onPATf)
		return
	case kindSDT:
		w.sdt.feed(p, w.onSDTf)
		return
	case kindPMT:
		if a := w.pmtAsm[pid]; a != nil {
			a.feed(p, w.onPMTf)
		}
		return
	}
	for _, o := range w.route[pid] {
		o.put(p)
	}
}

func (w *worker) onPAT(d []byte) {
	if d[0] != 0x00 || d[6] != 0 { // single-section PATs only
		return
	}
	crc := uint32(d[len(d)-4])<<24 | uint32(d[len(d)-3])<<16 | uint32(d[len(d)-2])<<8 | uint32(d[len(d)-1])
	if crc == w.patCRC {
		return
	}
	w.patCRC = crc
	w.tsid = uint16(d[3])<<8 | uint16(d[4])
	for pid := range w.kind {
		if w.kind[pid] == kindPMT {
			w.kind[pid] = kindNone
		}
	}
	seen := map[uint16]bool{}
	for j := 8; j+4 <= len(d)-4; j += 4 {
		prog := uint16(d[j])<<8 | uint16(d[j+1])
		pid := uint16(d[j+2]&0x1F)<<8 | uint16(d[j+3])
		if prog == 0 {
			continue // NIT
		}
		seen[prog] = true
		w.kind[pid] = kindPMT
		if w.pmtAsm[pid] == nil {
			w.pmtAsm[pid] = &sectionAsm{}
		}
		pr := w.progs[prog]
		if pr == nil || pr.pmtPID != pid {
			w.progs[prog] = &program{sid: prog, pmtPID: pid}
		}
	}
	for sid := range w.progs {
		if !seen[sid] {
			delete(w.progs, sid)
		}
	}
	for pid, a := range w.pmtAsm {
		if w.kind[pid] != kindPMT {
			delete(w.pmtAsm, pid)
		} else {
			a.active = false
		}
	}
	w.kind[pidPAT], w.kind[pidSDT] = kindPAT, kindSDT
	w.rebuildRoutes()
}

func (w *worker) onPMT(d []byte) {
	if d[0] != 0x02 {
		return
	}
	sid := uint16(d[3])<<8 | uint16(d[4])
	pr := w.progs[sid]
	if pr == nil {
		return
	}
	if !bytes.Equal(pr.pmt, d) {
		pr.pmt = append(pr.pmt[:0], d...)
		pr.pcrPID = uint16(d[8]&0x1F)<<8 | uint16(d[9])
		pil := int(d[10]&0x0F)<<8 | int(d[11])
		end := len(d) - 4
		if 12+pil > end {
			return
		}
		pr.pids = caPIDs(pr.pids[:0], d[12:12+pil])
		pr.streams = pr.streams[:0]
		for i := 12 + pil; i+5 <= end; {
			st := d[i]
			pid := uint16(d[i+1]&0x1F)<<8 | uint16(d[i+2])
			eil := int(d[i+3]&0x0F)<<8 | int(d[i+4])
			if i+5+eil > end {
				break
			}
			desc := d[i+5 : i+5+eil]
			pr.pids = append(pr.pids, pid)
			pr.pids = caPIDs(pr.pids, desc)
			pr.streams = append(pr.streams, StreamInfo{pid, st, streamKind(st, desc)})
			i += 5 + eil
		}
		if pr.pcrPID != pidNull {
			pr.pids = append(pr.pids, pr.pcrPID)
		}
		w.rebuildRoutes()
	}
	for _, o := range w.outs {
		if o.cfg.SrcSID == sid {
			w.emitPSI(o, pr)
		}
	}
}

func (w *worker) onSDT(d []byte) {
	if d[0] != 0x42 || len(d) < 15 {
		return
	}
	w.onid = uint16(d[8])<<8 | uint16(d[9])
	w.haveSDT = true
	end := len(d) - 4
	for i := 11; i+5 <= end; {
		sid := uint16(d[i])<<8 | uint16(d[i+1])
		dl := int(d[i+3]&0x0F)<<8 | int(d[i+4])
		desc := d[i+5 : min(i+5+dl, end)]
		for j := 0; j+2 <= len(desc); {
			tag, l := desc[j], int(desc[j+1])
			if j+2+l > len(desc) {
				break
			}
			if tag == 0x48 && l >= 3 {
				b := desc[j+2 : j+2+l]
				pl := int(b[1])
				if 2+pl < len(b) {
					nl := int(b[2+pl])
					if 3+pl+nl <= len(b) {
						w.sdtSvc[sid] = sdtEntry{
							provider: decodeDVBString(b[2 : 2+pl]),
							name:     decodeDVBString(b[3+pl : 3+pl+nl]),
							stype:    b[0],
						}
					}
				}
			}
			j += 2 + l
		}
		i += 5 + dl
	}
}

// emitPSI sends PAT + PMT for an output. Called each time the source PMT for
// that program arrives, so the output repetition rate follows the source.
func (w *worker) emitPSI(o *outRT, pr *program) {
	if o.pmtPID != pr.pmtPID || !bytes.Equal(o.pmtSrc, pr.pmt) {
		o.pmtSrc = append(o.pmtSrc[:0], pr.pmt...)
		o.pmtPID = pr.pmtPID
		pmt := pr.pmt
		if o.cfg.OutSID != pr.sid {
			pmt = rewritePMT(pr.pmt, o.cfg.OutSID)
		}
		o.psi = packetize(o.psi[:0], pidPAT, &o.patCC, buildPAT(o.cfg.TSID, o.cfg.OutSID, pr.pmtPID))
		o.psi = packetize(o.psi, pr.pmtPID, &o.pmtCC, pmt)
	} else {
		restamp(o.psi[:tsSize], &o.patCC)
		restamp(o.psi[tsSize:], &o.pmtCC)
	}
	o.putRaw(o.psi)
}

// restamp rewrites continuity counters of cached PSI packets before resending.
func restamp(pkts []byte, cc *uint8) {
	for i := 0; i+tsSize <= len(pkts); i += tsSize {
		pkts[i+3] = pkts[i+3]&0xF0 | *cc
		*cc = (*cc + 1) & 0x0F
	}
}

func (w *worker) rebuildRoutes() {
	for i := range w.route {
		w.route[i] = w.route[i][:0]
	}
	for _, o := range w.outs {
		pr := w.progs[o.cfg.SrcSID]
		if pr == nil || pr.pmt == nil {
			continue
		}
		add := func(pid uint16) {
			if pid >= pidNull || w.kind[pid] != kindNone {
				return
			}
			for _, x := range w.route[pid] {
				if x == o {
					return
				}
			}
			w.route[pid] = append(w.route[pid], o)
		}
		for _, pid := range pr.pids {
			add(pid)
		}
		add(pidTDT)
	}
}

func (w *worker) applyConfig(cfgs []OutCfg) {
	old := w.outs
	w.outs = make(map[int64]*outRT, len(cfgs))
	used := map[string]bool{}
	for _, c := range cfgs {
		key := c.URL.LocalAddr
		used[key] = true
		if o := old[c.ID]; o != nil && o.cfg == c {
			w.outs[c.ID] = o
			delete(old, c.ID)
			continue
		}
		t := w.txs[key]
		if t == nil {
			var err error
			t, err = openTx(key, c.TTL)
			if err != nil {
				log.Printf("input %d: output %d: %v", w.id, c.ID, err)
				t = &txSock{fd: -1, err: err}
			}
			w.txs[key] = t
		}
		h := fnv.New32a()
		h.Write([]byte(c.URL.String()))
		o := &outRT{
			clock: &w.rtpTS,
			cfg:   c,
			dst:   sockaddr4(c.URL.Addr.Addr().As4(), c.URL.Addr.Port()),
			tx:    t,
			rtp:   c.URL.Proto == "rtp",
			ssrc:  h.Sum32(),
		}
		o.sdtPkts = packetize(nil, pidSDT, &o.sdtCC, buildSDT(c.TSID, c.ONID, c.OutSID, c.SType, c.Provider, c.Name))
		if p := old[c.ID]; p != nil {
			o.patCC, o.pmtCC, o.sdtCC, o.seq = p.patCC, p.pmtCC, p.sdtCC, p.seq
			delete(old, c.ID)
		}
		w.outs[c.ID] = o
	}
	for _, o := range old {
		o.flushPartial()
	}
	for key, t := range w.txs {
		if !used[key] {
			if t.fd >= 0 {
				t.flush()
				t.close()
			}
			delete(w.txs, key)
		} else if t.fd < 0 {
			// retry sockets that failed to open earlier
			if nt, err := openTx(key, firstTTL(cfgs, key)); err == nil {
				w.txs[key] = nt
				for _, o := range w.outs {
					if o.tx == t {
						o.tx = nt
					}
				}
			}
		}
	}
	w.rebuildRoutes()
}

func firstTTL(cfgs []OutCfg, key string) int {
	for _, c := range cfgs {
		if c.URL.LocalAddr == key {
			return c.TTL
		}
	}
	return 16
}

func (o *outRT) put(p []byte) {
	copy(o.buf[o.n*tsSize:], p)
	o.n++
	o.pkts++
	if o.n == tsPerDG {
		o.send()
	}
}

func (o *outRT) putRaw(pkts []byte) {
	for i := 0; i+tsSize <= len(pkts); i += tsSize {
		o.put(pkts[i : i+tsSize])
	}
}

func (o *outRT) flushPartial() {
	if o.n > 0 {
		o.send()
	}
}

func (o *outRT) send() {
	sz := o.n * tsSize
	if o.tx.fd < 0 {
		o.n = 0
		return
	}
	if o.rtp {
		b := o.tx.next(o.dst, rtpHdrSz+sz)
		b[0], b[1] = 0x80, 33
		b[2], b[3] = byte(o.seq>>8), byte(o.seq)
		ts := *o.clock
		b[4], b[5], b[6], b[7] = byte(ts>>24), byte(ts>>16), byte(ts>>8), byte(ts)
		b[8], b[9], b[10], b[11] = byte(o.ssrc>>24), byte(o.ssrc>>16), byte(o.ssrc>>8), byte(o.ssrc)
		copy(b[rtpHdrSz:], o.buf[:sz])
		o.seq++
	} else {
		copy(o.tx.next(o.dst, sz), o.buf[:sz])
	}
	o.n = 0
}

// publish computes rates and exposes a status snapshot to the UI.
func (w *worker) publish(dt float64) {
	st := InputStatus{TSID: w.tsid, ONID: w.onid, Outputs: map[int64]OutStatus{}, Since: time.Now()}
	rate := func(pkts uint32) float64 {
		if dt == 0 {
			return 0
		}
		return float64(pkts) * tsSize * 8 / dt / 1000
	}
	var total uint32
	delta := func(pid uint16) uint32 { return w.cnt[pid] - w.prevCnt[pid] }
	for pid := range w.cnt {
		total += w.cnt[pid] - w.prevCnt[pid]
	}
	st.Kbps = rate(total)
	switch {
	case w.lastPkt.IsZero() || time.Since(w.lastPkt) > 3*time.Second:
		st.State = "nosignal"
	default:
		st.State = "ok"
	}
	for _, pr := range w.progs {
		si := ServiceInfo{SID: pr.sid, PMTPID: pr.pmtPID, PCRPID: pr.pcrPID, Type: 1,
			Streams: append([]StreamInfo(nil), pr.streams...)}
		if e, ok := w.sdtSvc[pr.sid]; ok {
			si.Name, si.Provider, si.Type = e.name, e.provider, e.stype
		}
		var s uint32
		seen := map[uint16]bool{}
		seen[pr.pmtPID] = true
		s += delta(pr.pmtPID)
		for _, pid := range pr.pids {
			if !seen[pid] {
				seen[pid] = true
				s += delta(pid)
			}
		}
		si.Kbps = rate(s)
		st.Services = append(st.Services, si)
	}
	sort.Slice(st.Services, func(i, j int) bool { return st.Services[i].SID < st.Services[j].SID })
	for id, o := range w.outs {
		os := OutStatus{Kbps: rate(uint32(o.pkts - o.prev))}
		os.Active = os.Kbps > 0
		if o.tx.err != nil {
			os.Error = o.tx.err.Error()
		} else if pr := w.progs[o.cfg.SrcSID]; st.State == "ok" && pr == nil && w.patCRC != 0 {
			os.Error = "service not found in source"
		}
		o.prev = o.pkts
		st.Outputs[id] = os
	}
	w.prevCnt = w.cnt
	w.statusMu.Lock()
	w.status = st
	w.statusMu.Unlock()
}

// Engine owns all workers.
type Engine struct {
	mu      sync.Mutex
	workers map[int64]*worker
}

func NewEngine() *Engine { return &Engine{workers: map[int64]*worker{}} }

type InputRun struct {
	ID   int64
	URL  StreamURL
	Outs []OutCfg
}

// Sync makes the running set match the desired set.
func (e *Engine) Sync(want []InputRun) {
	e.mu.Lock()
	defer e.mu.Unlock()
	keep := map[int64]bool{}
	var stopping []*worker
	for _, in := range want {
		keep[in.ID] = true
		w := e.workers[in.ID]
		if w != nil && w.url != in.URL {
			w.stop()
			stopping = append(stopping, w)
			w = nil
		}
		if w == nil {
			e.workers[in.ID] = newWorker(in.ID, in.URL, in.Outs)
		} else {
			w.setOutputs(in.Outs)
		}
	}
	for id, w := range e.workers {
		if !keep[id] {
			w.stop()
			stopping = append(stopping, w)
			delete(e.workers, id)
		}
	}
	for _, w := range stopping {
		w.wait()
	}
}

func (e *Engine) Status(id int64) (InputStatus, bool) {
	e.mu.Lock()
	w := e.workers[id]
	e.mu.Unlock()
	if w == nil {
		return InputStatus{}, false
	}
	return w.snapshot(), true
}

func (e *Engine) StopAll() {
	e.Sync(nil)
}

// Probe listens to a stream for d and returns what it found. Used for
// scanning and for previewing an input before it is saved.
func Probe(su StreamURL, d time.Duration) InputStatus {
	w := newWorker(0, su, nil)
	time.Sleep(d)
	st := w.snapshot()
	w.stop()
	w.wait()
	return st
}
