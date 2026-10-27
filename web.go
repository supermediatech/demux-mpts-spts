package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

type App struct {
	store  *Store
	engine *Engine
	user   string
	pass   string
	secret []byte
	mu     sync.Mutex // serialises config changes
	sys    sysSampler
}

func NewApp(store *Store, engine *Engine, user, pass string) *App {
	sec := store.Setting("session_secret")
	if sec == "" {
		b := make([]byte, 32)
		rand.Read(b)
		sec = hex.EncodeToString(b)
		store.SetSetting("session_secret", sec)
	}
	return &App{store: store, engine: engine, user: user, pass: pass, secret: []byte(sec)}
}

// Apply loads the configuration from the database and pushes it to the engine.
func (a *App) Apply() error {
	ins, err := a.store.Inputs()
	if err != nil {
		return err
	}
	outs, err := a.store.Outputs()
	if err != nil {
		return err
	}
	ttl := parsePort(a.store.Setting("ttl"), 16)
	byIn := map[int64][]OutCfg{}
	for _, o := range outs {
		if !o.Enabled {
			continue
		}
		su, err := ParseStreamURL(o.URL)
		if err != nil {
			log.Printf("output %d: %v", o.ID, err)
			continue
		}
		byIn[o.InputID] = append(byIn[o.InputID], OutCfg{
			ID: o.ID, SrcSID: uint16(o.SrcSID), OutSID: uint16(o.OutSID),
			TSID: uint16(o.TSID), ONID: uint16(o.ONID), Name: o.Name, Provider: o.Provider,
			SType: byte(o.SType), URL: su, TTL: ttl,
		})
	}
	var want []InputRun
	for _, in := range ins {
		if !in.Enabled {
			continue
		}
		su, err := ParseStreamURL(in.URL)
		if err != nil {
			log.Printf("input %d: %v", in.ID, err)
			continue
		}
		want = append(want, InputRun{ID: in.ID, URL: su, Outs: byIn[in.ID]})
	}
	a.engine.Sync(want)
	return nil
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.logout)

	api := func(pattern string, h func(*http.Request) (any, error)) {
		mux.HandleFunc(pattern, a.auth(func(w http.ResponseWriter, r *http.Request) {
			v, err := h(r)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			if v == nil {
				v = map[string]bool{"ok": true}
			}
			json.NewEncoder(w).Encode(v)
		}))
	}
	api("GET /api/state", a.state)
	api("GET /api/interfaces", func(*http.Request) (any, error) { return listIfaces(), nil })
	api("PUT /api/settings", a.saveSettings)
	api("POST /api/inputs", a.saveInput)
	api("PUT /api/inputs/{id}", a.saveInput)
	api("POST /api/inputs/bulk", a.addInputs)
	api("DELETE /api/inputs/{id}", a.deleteInput)
	api("POST /api/outputs", a.saveOutput)
	api("PUT /api/outputs/{id}", a.saveOutput)
	api("DELETE /api/outputs/{id}", a.deleteOutput)
	api("POST /api/outputs/enabled", a.setOutputsEnabled)
	api("POST /api/outputs/delete", a.deleteOutputs)
	api("POST /api/control/{action}", a.control)
	api("POST /api/scan", a.scan)
	api("POST /api/discover", a.discover)
	mux.HandleFunc("GET /api/playlist.m3u", a.auth(a.playlist))
	return mux
}

// ---------- auth ----------

func (a *App) sign(exp int64) string {
	m := hmac.New(sha256.New, a.secret)
	fmt.Fprintf(m, "%s|%d", a.user, exp)
	return fmt.Sprintf("%d.%s", exp, hex.EncodeToString(m.Sum(nil)))
}

func (a *App) authed(r *http.Request) bool {
	c, err := r.Cookie("session")
	if err != nil {
		return false
	}
	exp, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	e, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > e {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(a.sign(e)))
}

func (a *App) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password string }
	json.NewDecoder(r.Body).Decode(&req)
	uok := subtle.ConstantTimeCompare([]byte(req.Username), []byte(a.user)) == 1
	pok := subtle.ConstantTimeCompare([]byte(req.Password), []byte(a.pass)) == 1
	if !uok || !pok {
		time.Sleep(700 * time.Millisecond)
		http.Error(w, `{"error":"invalid username or password"}`, http.StatusUnauthorized)
		return
	}
	exp := time.Now().Add(7 * 24 * time.Hour).Unix()
	http.SetCookie(w, &http.Cookie{Name: "session", Value: a.sign(exp), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Expires: time.Unix(exp, 0)})
	w.Write([]byte(`{"ok":true}`))
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", MaxAge: -1})
	w.Write([]byte(`{"ok":true}`))
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	b, _ := webFS.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// ---------- handlers ----------

type inputView struct {
	Input
	Status *InputStatus `json:"status,omitempty"`
}

type outputView struct {
	Output
	Status *OutStatus `json:"status,omitempty"`
}

func (a *App) state(r *http.Request) (any, error) {
	ins, err := a.store.Inputs()
	if err != nil {
		return nil, err
	}
	outs, err := a.store.Outputs()
	if err != nil {
		return nil, err
	}
	settings, err := a.store.Settings()
	if err != nil {
		return nil, err
	}
	delete(settings, "session_secret")
	st := map[int64]InputStatus{}
	iv := make([]inputView, 0, len(ins))
	for _, in := range ins {
		v := inputView{Input: in}
		if s, ok := a.engine.Status(in.ID); ok {
			st[in.ID] = s
			v.Status = &s
		}
		iv = append(iv, v)
	}
	ov := make([]outputView, 0, len(outs))
	for _, o := range outs {
		v := outputView{Output: o}
		if s, ok := st[o.InputID].Outputs[o.ID]; ok {
			v.Status = &s
		}
		ov = append(ov, v)
	}
	return map[string]any{"inputs": iv, "outputs": ov, "settings": settings, "system": a.sys.get()}, nil
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("bad request: %v", err)
	}
	return nil
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func (a *App) saveSettings(r *http.Request) (any, error) {
	var req map[string]string
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range req {
		v = strings.TrimSpace(v)
		switch k {
		case "auto_group":
			if ad, err := netip.ParseAddr(v); err != nil || !ad.Is4() || !ad.IsMulticast() {
				return nil, errors.New("auto group must be an IPv4 multicast address")
			}
		case "auto_port", "ttl":
			if _, err := strconv.Atoi(v); err != nil || parsePort(v, 0) == 0 {
				return nil, fmt.Errorf("invalid %s", k)
			}
		case "auto_proto":
			if v != "udp" && v != "rtp" {
				return nil, errors.New("protocol must be udp or rtp")
			}
		case "auto_localaddr":
			if v != "" {
				if ad, err := netip.ParseAddr(v); err != nil || !ad.Is4() {
					return nil, errors.New("invalid local address")
				}
			}
		default:
			continue
		}
		if err := a.store.SetSetting(k, v); err != nil {
			return nil, err
		}
	}
	return nil, a.Apply()
}

func (a *App) saveInput(r *http.Request) (any, error) {
	var req struct {
		Name    string
		URL     string
		Enabled *bool
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	in := Input{Name: req.Name, URL: req.URL, Enabled: req.Enabled == nil || *req.Enabled}
	su, err := ParseStreamURL(in.URL)
	if err != nil {
		return nil, err
	}
	in.URL = su.String()
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = su.Addr.String()
	}
	in.ID = pathID(r)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.SaveInput(&in); err != nil {
		return nil, err
	}
	return in, a.Apply()
}

// addInputs creates many inputs at once (e.g. a group range on one
// interface). URLs that already exist are skipped, not treated as errors.
func (a *App) addInputs(r *http.Request) (any, error) {
	var req struct {
		Name string   `json:"name"`
		URLs []string `json:"urls"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if len(req.URLs) == 0 || len(req.URLs) > 1024 {
		return nil, errors.New("give 1..1024 URLs")
	}
	name := strings.TrimSpace(req.Name)
	ins := make([]Input, 0, len(req.URLs))
	for _, u := range req.URLs {
		su, err := ParseStreamURL(u)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u, err)
		}
		in := Input{URL: su.String(), Name: su.Addr.String(), Enabled: true}
		if name != "" && len(req.URLs) == 1 {
			in.Name = name
		} else if name != "" {
			in.Name = name + " " + su.Addr.String()
		}
		ins = append(ins, in)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ids, skipped, err := a.store.AddInputs(ins)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ids": ids, "skipped": skipped}, a.Apply()
}

func (a *App) deleteInput(r *http.Request) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.DeleteInput(pathID(r)); err != nil {
		return nil, err
	}
	return nil, a.Apply()
}

type outputReq struct {
	InputID   int64   `json:"input_id"`
	SrcSID    int     `json:"src_sid"`
	OutSID    *int    `json:"out_sid"` // nil/0 = same as source
	TSID      *int    `json:"tsid"`    // nil/0 = same as output service id
	Name      *string `json:"name"`
	Provider  *string `json:"provider"`
	Proto     string  `json:"proto"`
	Addr      string  `json:"addr"` // "" = auto allocate (new) / keep (edit)
	LocalAddr *string `json:"localaddr"`
	Enabled   *bool   `json:"enabled"`
}

func (a *App) saveOutput(r *http.Request) (any, error) {
	var req outputReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	var o Output
	var cur StreamURL
	if id := pathID(r); id != 0 {
		var err error
		if o, err = a.store.Output(id); err != nil {
			return nil, errors.New("output not found")
		}
		cur, _ = ParseStreamURL(o.URL)
	} else {
		if _, err := a.store.Input(req.InputID); err != nil {
			return nil, errors.New("input not found")
		}
		if req.SrcSID < 1 || req.SrcSID > 65535 {
			return nil, errors.New("invalid source service id")
		}
		o = Output{InputID: req.InputID, SrcSID: req.SrcSID, Enabled: true, SType: 1, ONID: 1}
		cur = StreamURL{Proto: a.store.Setting("auto_proto"), LocalAddr: a.store.Setting("auto_localaddr")}
		// capture identity from the live source so it is frozen in the DB
		if st, ok := a.engine.Status(req.InputID); ok {
			if st.ONID != 0 {
				o.ONID = int(st.ONID)
			}
			for _, s := range st.Services {
				if int(s.SID) == req.SrcSID {
					o.Name, o.Provider = s.Name, s.Provider
					if s.Type != 0 {
						o.SType = int(s.Type)
					}
				}
			}
		}
		if o.Name == "" {
			o.Name = fmt.Sprintf("Service %d", req.SrcSID)
		}
	}

	if req.OutSID != nil {
		o.OutSID = *req.OutSID
	}
	if o.OutSID == 0 {
		o.OutSID = o.SrcSID
	}
	if o.OutSID < 1 || o.OutSID > 65535 {
		return nil, errors.New("service id must be 1..65535")
	}
	if req.TSID != nil {
		o.TSID = *req.TSID
	}
	if o.TSID == 0 {
		o.TSID = o.OutSID
	}
	if o.TSID < 1 || o.TSID > 65535 {
		return nil, errors.New("transport stream id must be 1..65535")
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		o.Name = strings.TrimSpace(*req.Name)
	}
	if req.Provider != nil {
		o.Provider = strings.TrimSpace(*req.Provider)
	}
	if req.Enabled != nil {
		o.Enabled = *req.Enabled
	}
	if req.Proto != "" {
		cur.Proto = req.Proto
	}
	if req.LocalAddr != nil {
		cur.LocalAddr = strings.TrimSpace(*req.LocalAddr)
	}
	if req.Addr = strings.TrimSpace(req.Addr); req.Addr != "" {
		ap, err := netip.ParseAddrPort(req.Addr)
		if err != nil || !ap.Addr().Is4() || ap.Port() == 0 {
			return nil, errors.New("address must be ip:port")
		}
		cur.Addr = ap
	} else if !cur.Addr.IsValid() {
		ap, err := a.store.NextAutoAddr(nil)
		if err != nil {
			return nil, err
		}
		cur.Addr = ap
	}
	su, err := ParseStreamURL(cur.String())
	if err != nil {
		return nil, err
	}
	o.URL = su.String()
	if err := a.store.SaveOutput(&o); err != nil {
		return nil, err
	}
	return o, a.Apply()
}

func (a *App) deleteOutput(r *http.Request) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.DeleteOutput(pathID(r)); err != nil {
		return nil, err
	}
	return nil, a.Apply()
}

// setOutputsEnabled enables or disables a set of outputs in one transaction.
func (a *App) setOutputsEnabled(r *http.Request) (any, error) {
	var req struct {
		IDs     []int64 `json:"ids"`
		Enabled bool    `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.SetOutputsEnabled(req.IDs, req.Enabled); err != nil {
		return nil, err
	}
	return nil, a.Apply()
}

func (a *App) deleteOutputs(r *http.Request) (any, error) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.DeleteOutputs(req.IDs); err != nil {
		return nil, err
	}
	return nil, a.Apply()
}

// control handles start/stop/restart of every input. Start and stop are
// persisted (they toggle the inputs' enabled flag); restart reopens all
// sockets without changing the configuration.
func (a *App) control(r *http.Request) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch r.PathValue("action") {
	case "start":
		if err := a.store.SetAllInputsEnabled(true); err != nil {
			return nil, err
		}
	case "stop":
		if err := a.store.SetAllInputsEnabled(false); err != nil {
			return nil, err
		}
	case "restart":
		a.engine.StopAll()
	default:
		return nil, errors.New("unknown action")
	}
	return nil, a.Apply()
}

type scanResult struct {
	URL      string        `json:"url"`
	Kbps     float64       `json:"kbps"`
	Services []ServiceInfo `json:"services"`
	Source   string        `json:"source,omitempty"`
}

// scan joins a range of groups on one interface at once and reports the
// ones carrying a transport stream.
func (a *App) scan(r *http.Request) (any, error) {
	var req struct {
		LocalAddr string `json:"localaddr"`
		Proto     string `json:"proto"`
		Start     string `json:"start"`
		Count     int    `json:"count"`
		Port      int    `json:"port"`
		Seconds   int    `json:"seconds"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	start, err := netip.ParseAddr(strings.TrimSpace(req.Start))
	if err != nil || !start.Is4() {
		return nil, errors.New("invalid start address")
	}
	if req.Count < 1 || req.Count > 256 {
		return nil, errors.New("count must be 1..256")
	}
	if req.Port < 1 || req.Port > 65535 {
		return nil, errors.New("invalid port")
	}
	req.Seconds = max(2, min(req.Seconds, 15))
	if req.Proto != "rtp" {
		req.Proto = "udp"
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	res := []scanResult{}
	addr := start
	for i := 0; i < req.Count && addr.Is4(); i, addr = i+1, addr.Next() {
		su := StreamURL{Proto: req.Proto, Addr: netip.AddrPortFrom(addr, uint16(req.Port)), LocalAddr: req.LocalAddr}
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := Probe(su, time.Duration(req.Seconds)*time.Second)
			if st.State == "error" && i == 0 {
				mu.Lock()
				err = errors.New(st.Error)
				mu.Unlock()
			}
			if st.Kbps > 0 {
				mu.Lock()
				res = append(res, scanResult{URL: su.String(), Kbps: st.Kbps, Services: st.Services})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(res) == 0 && err != nil {
		return nil, err
	}
	sort.Slice(res, func(i, j int) bool { return res[i].URL < res[j].URL })
	return res, nil
}

func (a *App) discover(r *http.Request) (any, error) {
	var req struct {
		LocalAddr string `json:"localaddr"`
		Seconds   int    `json:"seconds"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	return Discover(strings.TrimSpace(req.LocalAddr), time.Duration(max(2, min(req.Seconds, 30)))*time.Second)
}

func (a *App) playlist(w http.ResponseWriter, r *http.Request) {
	outs, err := a.store.Outputs()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl")
	w.Header().Set("Content-Disposition", `attachment; filename="spts.m3u"`)
	fmt.Fprintln(w, "#EXTM3U")
	for _, o := range outs {
		if !o.Enabled {
			continue
		}
		su, _ := ParseStreamURL(o.URL)
		fmt.Fprintf(w, "#EXTINF:-1 tvg-id=\"%d\",%s\n%s://@%s\n", o.OutSID, o.Name, su.Proto, su.Addr)
	}
}
