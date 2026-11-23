package main

// Program preview: the engine sends the chosen program as a hidden SPTS
// output to 127.0.0.1:<port>, ffmpeg reads it and writes low-latency HLS
// (H.264 + AAC, fMP4, short segments) into a private temp directory, and
// the web UI plays it with hls.js. A preview ends when the viewer closes
// it, stops fetching it, ffmpeg exits, or the app shuts down; its directory
// is deleted every time.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	previewMax     = 4                // concurrent previews
	previewIdle    = 20 * time.Second // stop when nothing fetched it for this long
	previewStartup = 15 * time.Second // wait this long for the first playlist
	previewPrefix  = "mpts2spts-preview-"
)

type preview struct {
	key      string
	inputID  int64     // negative for a temporary input (a stream not added yet)
	url      StreamURL // temporary input only
	temp     bool
	sid      uint16
	outID    int64 // negative: never collides with database output IDs
	port     int
	dir      string
	cmd      *exec.Cmd
	stderr   *tailBuf
	exited   chan struct{}
	lastSeen atomic.Int64
	stopOnce sync.Once
}

type previews struct {
	mu     sync.Mutex
	m      map[string]*preview
	nextID int64
}

func newPreviews() *previews {
	// remove directories left behind by a crash
	if old, _ := filepath.Glob(filepath.Join(os.TempDir(), previewPrefix+"*")); len(old) > 0 {
		for _, d := range old {
			os.RemoveAll(d)
		}
	}
	return &previews{m: map[string]*preview{}}
}

// cfgs returns the hidden outputs the engine must add to inputID.
func (ps *previews) cfgs(inputID int64) []OutCfg {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	var out []OutCfg
	for _, p := range ps.m {
		if p.inputID != inputID {
			continue
		}
		out = append(out, OutCfg{
			ID: p.outID, SrcSID: p.sid, OutSID: p.sid, TSID: 1, ONID: 1, SType: 1,
			Name: "preview", URL: StreamURL{Proto: "udp", Addr: netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(p.port))},
			TTL: 1,
		})
	}
	return out
}

// temps returns the temporary inputs previews of not-yet-added streams need.
func (ps *previews) temps() []InputRun {
	ps.mu.Lock()
	var ids []int64
	var urls []StreamURL
	for _, p := range ps.m {
		if p.temp {
			ids = append(ids, p.inputID)
			urls = append(urls, p.url)
		}
	}
	ps.mu.Unlock()
	runs := make([]InputRun, len(ids))
	for i, id := range ids {
		runs[i] = InputRun{ID: id, URL: urls[i], Outs: ps.cfgs(id)}
	}
	return runs
}

// takeInputs removes the previews of the given inputs and returns them; the
// caller re-applies the engine config, then kills them.
func (ps *previews) takeInputs(ids map[int64]bool) []*preview {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	var out []*preview
	for k, p := range ps.m {
		if !p.temp && ids[p.inputID] {
			out = append(out, p)
			delete(ps.m, k)
		}
	}
	return out
}

func (ps *previews) get(key string) *preview {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.m[key]
}

// take removes a preview from the set (its hidden output disappears at the
// next Apply) and returns it, or nil if it does not exist.
func (ps *previews) take(key string) *preview {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.m[key]
	delete(ps.m, key)
	return p
}

func (p *preview) kill() {
	p.stopOnce.Do(func() {
		if p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
		<-p.exited
		if err := os.RemoveAll(p.dir); err != nil {
			log.Printf("preview %s: remove %s: %v", p.key, p.dir, err)
		}
	})
}

// stale returns the keys of previews that should end.
func (ps *previews) stale() []string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	var keys []string
	now := time.Now().UnixNano()
	for k, p := range ps.m {
		select {
		case <-p.exited:
			keys = append(keys, k)
			continue
		default:
		}
		if time.Duration(now-p.lastSeen.Load()) > previewIdle {
			keys = append(keys, k)
		}
	}
	return keys
}

func (ps *previews) keys() []string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	keys := make([]string, 0, len(ps.m))
	for k := range ps.m {
		keys = append(keys, k)
	}
	return keys
}

// freeUDPPort asks the kernel for an unused loopback UDP port.
func freeUDPPort() (int, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return 0, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port, nil
}

func ffmpegArgs(port int, dir string) []string {
	in := fmt.Sprintf("udp://127.0.0.1:%d?fifo_size=1000000&overrun_nonfatal=1&timeout=10000000", port)
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-fflags", "+genpts+discardcorrupt", "-probesize", "5M", "-analyzeduration", "3M",
		"-i", in,
		"-map", "0:v:0?", "-map", "0:a:0?", "-sn", "-dn",
		"-vf", "yadif=deint=interlaced,scale=-2:'min(480,ih)'",
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-profile:v", "main",
		"-pix_fmt", "yuv420p", "-b:v", "900k", "-maxrate", "1200k", "-bufsize", "1200k",
		"-force_key_frames", "expr:gte(t,n_forced*1)", "-sc_threshold", "0",
		"-c:a", "aac", "-b:a", "96k", "-ac", "2", "-ar", "48000",
		"-f", "hls", "-hls_time", "1", "-hls_list_size", "6",
		"-hls_flags", "delete_segments+independent_segments+omit_endlist",
		"-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(dir, "seg%05d.m4s"),
		filepath.Join(dir, "index.m3u8"),
	}
}

// start launches ffmpeg for inputID/sid. With temp set, the program comes
// from a temporary input opened on url (inputID is ignored). The caller must
// re-apply the engine config afterwards so the hidden output starts flowing.
func (ps *previews) start(inputID int64, sid uint16, temp bool, url StreamURL) (*preview, error) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, errors.New("ffmpeg is not installed on the server (apt install ffmpeg)")
	}
	ps.mu.Lock()
	if len(ps.m) >= previewMax {
		ps.mu.Unlock()
		return nil, fmt.Errorf("too many previews running (max %d); close one first", previewMax)
	}
	ps.nextID--
	outID := ps.nextID
	ps.mu.Unlock()
	if temp {
		inputID = outID // negative: never collides with database input IDs
	}

	port, err := freeUDPPort()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", previewPrefix)
	if err != nil {
		return nil, err
	}
	kb := make([]byte, 12)
	rand.Read(kb)
	p := &preview{key: hex.EncodeToString(kb), inputID: inputID, url: url, temp: temp, sid: sid, outID: outID,
		port: port, dir: dir, stderr: &tailBuf{}, exited: make(chan struct{})}
	p.lastSeen.Store(time.Now().UnixNano())
	p.cmd = exec.Command(ff, ffmpegArgs(port, dir)...)
	p.cmd.Stderr = p.stderr
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} // never outlive the app
	if err := p.cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("start ffmpeg: %v", err)
	}
	// the service runs at Nice=-5; keep the encoder behind the splitter
	unix.Setpriority(unix.PRIO_PROCESS, p.cmd.Process.Pid, 10)
	go func() { p.cmd.Wait(); close(p.exited) }()

	ps.mu.Lock()
	ps.m[p.key] = p
	ps.mu.Unlock()
	if temp {
		log.Printf("preview %s: %s sid %d started (ffmpeg pid %d)", p.key, url, sid, p.cmd.Process.Pid)
	} else {
		log.Printf("preview %s: input %d sid %d started (ffmpeg pid %d)", p.key, inputID, sid, p.cmd.Process.Pid)
	}
	return p, nil
}

// waitReady blocks until the first playlist exists or ffmpeg fails.
func (p *preview) waitReady() error {
	deadline := time.Now().Add(previewStartup)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(p.dir, "index.m3u8")); err == nil {
			return nil
		}
		select {
		case <-p.exited:
			if msg := strings.TrimSpace(p.stderr.String()); msg != "" {
				return fmt.Errorf("ffmpeg stopped: %s", lastLine(msg))
			}
			return errors.New("ffmpeg stopped without output")
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New("no picture after 15 s: the program is not delivering decodable video or audio, often because the input is losing packets")
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// tailBuf keeps the last few KB written to it (ffmpeg's stderr).
type tailBuf struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(bytes.TrimSpace(t.b))
}

// ---------- HTTP ----------

var previewFile = regexp.MustCompile(`^(index\.m3u8|init\.mp4|seg[0-9]+\.m4s)$`)

func (a *App) startPreview(r *http.Request) (any, error) {
	var req struct {
		InputID int64  `json:"input_id"`
		SID     int    `json:"sid"`
		URL     string `json:"url"` // a stream that is not an input yet (Discover / Scan results)
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.SID < 1 || req.SID > 65535 {
		return nil, errors.New("invalid service id")
	}
	if req.URL != "" {
		su, err := ParseStreamURL(req.URL)
		if err != nil {
			return nil, err
		}
		// already an input that is running: preview through it
		ins, err := a.store.Inputs()
		if err != nil {
			return nil, err
		}
		req.InputID = 0
		for _, in := range ins {
			cu, err := ParseStreamURL(in.URL)
			if _, ok := a.engine.Status(in.ID); ok && in.Enabled && err == nil && sameInput(su, cu) {
				req.InputID = in.ID
			}
		}
		if req.InputID == 0 {
			return a.runPreview(0, req.SID, true, su, 0)
		}
	}
	st, ok := a.engine.Status(req.InputID)
	if !ok {
		return nil, errors.New("input is not running")
	}
	var svc *ServiceInfo
	for i := range st.Services {
		if int(st.Services[i].SID) == req.SID {
			svc = &st.Services[i]
		}
	}
	if svc == nil {
		return nil, errors.New("program not found in the input")
	}
	if len(svc.Streams) == 0 {
		return nil, fmt.Errorf("this program's PMT has not been received, so there is nothing to play. "+
			"The input is arriving at only %.0f kb/s; it is probably losing packets", st.Kbps)
	}
	return a.runPreview(req.InputID, req.SID, false, StreamURL{}, svc.Kbps)
}

// runPreview starts ffmpeg, adds the hidden output (and temporary input) and
// waits for the first playlist.
func (a *App) runPreview(inputID int64, sid int, temp bool, su StreamURL, kbps float64) (any, error) {
	p, err := a.prev.start(inputID, uint16(sid), temp, su)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	err = a.Apply()
	a.mu.Unlock()
	if err == nil {
		err = p.waitReady()
	}
	if err != nil && strings.Contains(err.Error(), "Input/output error") {
		err = fmt.Errorf("no usable data arrived from this program (%.0f kb/s). "+
			"The input is probably losing packets", kbps)
	}
	if err != nil && temp {
		if st, ok := a.engine.Status(p.inputID); ok && st.Error != "" {
			err = fmt.Errorf("cannot receive this stream: %s", st.Error)
		} else if ok && st.Kbps == 0 {
			err = errors.New("no data arrived from this stream. Check the interface and that the group is reachable")
		}
	}
	if err != nil {
		a.endPreview(p.key)
		log.Printf("preview %s: %v", p.key, err)
		return nil, err
	}
	return map[string]string{"key": p.key, "url": "api/preview/" + p.key + "/index.m3u8"}, nil
}

// endPreview stops a preview, deletes its files and drops its hidden output.
func (a *App) endPreview(key string) {
	p := a.prev.take(key)
	if p == nil {
		return
	}
	a.mu.Lock()
	a.Apply() // drop the hidden output first, then stop ffmpeg
	a.mu.Unlock()
	p.kill()
	log.Printf("preview %s: stopped, files deleted", key)
}

func (a *App) stopPreview(r *http.Request) (any, error) {
	a.endPreview(r.PathValue("key"))
	return nil, nil
}

func (a *App) pingPreview(r *http.Request) (any, error) {
	p := a.prev.get(r.PathValue("key"))
	if p == nil {
		return nil, errors.New("preview ended")
	}
	p.lastSeen.Store(time.Now().UnixNano())
	return nil, nil
}

func (a *App) previewFile(w http.ResponseWriter, r *http.Request) {
	p := a.prev.get(r.PathValue("key"))
	name := r.PathValue("file")
	if p == nil || !previewFile.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	p.lastSeen.Store(time.Now().UnixNano())
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case strings.HasSuffix(name, ".m3u8"):
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	case strings.HasSuffix(name, ".m4s"):
		w.Header().Set("Content-Type", "video/iso.segment")
	default:
		w.Header().Set("Content-Type", "video/mp4")
	}
	http.ServeFile(w, r, filepath.Join(p.dir, name))
}

// reapPreviews ends previews nobody is watching or whose ffmpeg exited.
func (a *App) reapPreviews() {
	for range time.Tick(5 * time.Second) {
		for _, k := range a.prev.stale() {
			if p := a.prev.get(k); p != nil {
				select {
				case <-p.exited:
					log.Printf("preview %s: ffmpeg exited: %s", k, lastLine(p.stderr.String()))
				default:
					log.Printf("preview %s: idle", k)
				}
			}
			a.endPreview(k)
		}
	}
}

// StopPreviews ends every preview (used at shutdown).
func (a *App) StopPreviews() {
	for _, k := range a.prev.keys() {
		if p := a.prev.take(k); p != nil {
			p.kill()
		}
	}
}
