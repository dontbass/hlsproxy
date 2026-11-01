package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var version = "2.0.0"

type config struct {
	Listen       string
	CacheDir     string
	Delay        time.Duration
	StartSegs    int
	Window       int
	Concurrency  int
	Idle         time.Duration
	IdleRead     time.Duration
	MaxAttempts  int
	MaxBurstSegs int
	StartWait    time.Duration
	AllVariants  bool
	UA           string
}

var cfg config
var startedAt = time.Now()

func main() {
	var delay, idle, idleRead, startWait int
	flag.StringVar(&cfg.Listen, "listen", ":8888", "адрес:порт")
	flag.StringVar(&cfg.CacheDir, "cache", "/tmp/hls_cache", "каталог кэша (tmpfs)")
	flag.IntVar(&delay, "delay", 60, "отставание от эфира провайдера, сек (запас против провалов)")
	flag.IntVar(&cfg.StartSegs, "start-segs", 3, "сколько сегментов отдать плееру на старте (его собственный буфер)")
	flag.IntVar(&cfg.Window, "window", 8, "сегментов в плейлисте для плеера")
	flag.IntVar(&cfg.Concurrency, "concurrency", 2, "параллельных загрузок на канал")
	flag.IntVar(&idle, "idle", 60, "остановить канал после N сек без запросов")
	flag.IntVar(&idleRead, "idle-read", 12, "оборвать загрузку, если N сек не пришло ни байта (затем докачка)")
	flag.IntVar(&cfg.MaxAttempts, "max-attempts", 8, "попыток на сегмент")
	flag.IntVar(&cfg.MaxBurstSegs, "max-burst", 4, "макс. сегментов, отдаваемых пачкой при догоне после провала")
	flag.IntVar(&startWait, "start-wait", 7, "сколько сек держать первый запрос плеера, пока качается старт")
	flag.BoolVar(&cfg.AllVariants, "all-variants", false, "оставлять все качества в master-плейлисте (по умолчанию только лучшее)")
	flag.StringVar(&cfg.UA, "ua", "", "User-Agent к провайдеру (по умолчанию — как у плеера)")
	showVer := flag.Bool("version", false, "версия")
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	cfg.Delay = time.Duration(delay) * time.Second
	cfg.Idle = time.Duration(idle) * time.Second
	cfg.IdleRead = time.Duration(idleRead) * time.Second
	cfg.StartWait = time.Duration(startWait) * time.Second
	if cfg.StartSegs < 1 {
		cfg.StartSegs = 1
	}
	if cfg.Window < cfg.StartSegs+2 {
		cfg.Window = cfg.StartSegs + 2
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}

	log.SetFlags(0)
	os.RemoveAll(cfg.CacheDir)
	if err := os.MkdirAll(cfg.CacheDir, 0755); err != nil {
		log.Fatal(err)
	}
	go channels.janitor()

	srv := &http.Server{Addr: cfg.Listen, Handler: newMux(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("hlsproxy %s на %s: delay=%s start-segs=%d window=%d concurrency=%d",
		version, cfg.Listen, cfg.Delay, cfg.StartSegs, cfg.Window, cfg.Concurrency)
	log.Fatal(srv.ListenAndServe())
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist", playlistHandler)
	mux.HandleFunc("/proxy", playlistHandler)
	mux.HandleFunc("/seg/", segHandler)
	mux.HandleFunc("/ts", legacyTSHandler)
	mux.HandleFunc("/status", statusHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "hlsproxy %s\n/playlist?url=<m3u/m3u8>  /status\n", version)
	})
	return mux
}

func clientUA(r *http.Request) string {
	if cfg.UA != "" {
		return cfg.UA
	}
	return r.UserAgent()
}

func playlistHandler(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("url")
	if src == "" {
		http.Error(w, "url= required", http.StatusBadRequest)
		return
	}
	if c := channels.get(chanID(src)); c != nil {
		serveChannel(c, w, r)
		return
	}

	ua := clientUA(r)
	body, final, err := fetchPlaylist(r.Context(), src, ua)
	if err != nil {
		log.Printf("плейлист %s: %v", shortURL(src), err)
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	base, _ := url.Parse(final)
	wrap := func(u string) string {
		return "http://" + r.Host + "/proxy?url=" + url.QueryEscape(u)
	}

	switch detectKind(body) {
	case kindMaster:
		writeM3U(w, rewriteMaster(body, base, cfg.AllVariants, wrap))
	case kindChannels:
		writeM3U(w, rewriteChannels(body, base, wrap))
	case kindMedia:
		p := parseMedia(body, base)
		if p.endList || p.byteRange || len(p.segments) == 0 {
			writeM3U(w, absolutizeMedia(body, base)) // VOD / exotic: no delay logic
			return
		}
		serveChannel(channels.create(src, final, ua, p), w, r)
	default:
		http.Error(w, "upstream did not return an m3u playlist", http.StatusBadGateway)
	}
}

func serveChannel(c *channel, w http.ResponseWriter, r *http.Request) {
	c.touch()
	select {
	case <-c.startedCh:
	case <-time.After(cfg.StartWait):
		if !c.forceStart() {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "буферизация канала, повторите", http.StatusServiceUnavailable)
			return
		}
	case <-r.Context().Done():
		return
	}
	c.mu.Lock()
	c.advance(time.Now())
	body := c.render(r.Host)
	c.mu.Unlock()
	writeM3U(w, body)
}

func writeM3U(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	io.WriteString(w, body)
}

func absolutizeMedia(body string, base *url.URL) string {
	var b strings.Builder
	for _, l := range lines(body) {
		t := strings.TrimSpace(l)
		switch {
		case t == "":
			continue
		case strings.HasPrefix(t, "#"):
			if strings.Contains(t, `URI="`) {
				t = rewriteURIAttr(t, base, func(s string) string { return s })
			}
		default:
			t = resolveURL(base, t)
		}
		b.WriteString(t)
		b.WriteByte('\n')
	}
	return b.String()
}

// /seg/<chan>/<id>.<ext>
func segHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/seg/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	c := channels.get(parts[0])
	if c == nil {
		http.NotFound(w, r)
		return
	}
	name := parts[1]
	ext := ""
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name, ext = name[:i], name[i:]
	}
	id, err := strconv.ParseInt(name, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c.touch()
	c.mu.Lock()
	s := c.segByID(id)
	c.mu.Unlock()
	if s == nil {
		http.NotFound(w, r)
		return
	}
	select {
	case <-s.done:
	case <-time.After(20 * time.Second):
	case <-r.Context().Done():
		return
	}
	c.mu.Lock()
	ready := s.state == stReady
	c.mu.Unlock()
	if !ready {
		http.Error(w, "segment unavailable", http.StatusBadGateway)
		return
	}
	f, err := os.Open(s.path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(ext)
	switch ext {
	case ".ts":
		ct = "video/mp2t"
	case ".m4s", ".mp4":
		ct = "video/mp4"
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "max-age=300")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// Old playlists from v1 pointed segments at /ts?url=...; just stream them through.
func legacyTSHandler(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if u == "" {
		http.Error(w, "url= required", http.StatusBadRequest)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
	if req == nil {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	req.Header.Set("User-Agent", clientUA(r))
	resp, err := upstreamClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "video/mp2t")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	list := channels.list()
	out := struct {
		Version   string       `json:"version"`
		UptimeSec int64        `json:"uptime_s"`
		DelaySec  int          `json:"delay_cfg_s"`
		Channels  []chanStatus `json:"channels"`
	}{version, int64(time.Since(startedAt).Seconds()), int(cfg.Delay.Seconds()), []chanStatus{}}
	for _, c := range list {
		out.Channels = append(out.Channels, c.status())
	}
	sort.Slice(out.Channels, func(i, j int) bool { return out.Channels[i].IdleSec < out.Channels[j].IdleSec })
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(out)
}
