package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var version = "3.0.0"

type config struct {
	Listen       string
	CacheDir     string
	Delay        time.Duration
	MaxDelay     time.Duration
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
	Mode         string // auto | advise | off
	ProfilePath  string
	MaxHold      time.Duration // max single auto-pause
}

var cfg config
var startedAt = time.Now()

func main() {
	var delay, maxDelay, idle, idleRead, startWait, maxHold int
	flag.StringVar(&cfg.Listen, "listen", ":8888", "адрес:порт")
	flag.StringVar(&cfg.CacheDir, "cache", "/tmp/hls_cache", "каталог кэша (tmpfs)")
	flag.IntVar(&delay, "delay", 60, "отставание от эфира провайдера по умолчанию, сек")
	flag.IntVar(&maxDelay, "max-delay", 180, "потолок защиты (авто-подстройка не превысит), сек")
	flag.StringVar(&cfg.Mode, "mode", "auto", "auto — подстраиваться по каналу; advise — только советовать; off — фиксированные параметры")
	flag.StringVar(&cfg.ProfilePath, "profiles", "/etc/hlsproxy/profiles.json", "файл профилей каналов")
	flag.IntVar(&cfg.StartSegs, "start-segs", 3, "сегментов плееру на старте")
	flag.IntVar(&cfg.Window, "window", 8, "сегментов в плейлисте для плеера")
	flag.IntVar(&cfg.Concurrency, "concurrency", 2, "параллельных загрузок на канал (начальное)")
	flag.IntVar(&idle, "idle", 60, "остановить канал после N сек без запросов")
	flag.IntVar(&idleRead, "idle-read", 12, "оборвать загрузку после N сек без данных (затем докачка)")
	flag.IntVar(&cfg.MaxAttempts, "max-attempts", 8, "попыток на сегмент")
	flag.IntVar(&cfg.MaxBurstSegs, "max-burst", 4, "макс. сегментов пачкой при догоне")
	flag.IntVar(&startWait, "start-wait", 7, "держать первый запрос плеера до N сек")
	flag.IntVar(&maxHold, "max-pause", 40, "макс. длительность одной авто-паузы, сек (0 = не делать)")
	flag.BoolVar(&cfg.AllVariants, "all-variants", false, "оставлять все качества в master-плейлисте")
	flag.StringVar(&cfg.UA, "ua", "", "User-Agent к провайдеру (по умолчанию — как у плеера)")
	showVer := flag.Bool("version", false, "версия")
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	cfg.Delay = time.Duration(delay) * time.Second
	cfg.MaxDelay = time.Duration(max(maxDelay, delay)) * time.Second
	cfg.Idle = time.Duration(idle) * time.Second
	cfg.IdleRead = time.Duration(idleRead) * time.Second
	cfg.StartWait = time.Duration(startWait) * time.Second
	cfg.MaxHold = time.Duration(maxHold) * time.Second
	normalizeConfig()

	log.SetFlags(0)
	os.RemoveAll(cfg.CacheDir)
	if err := os.MkdirAll(cfg.CacheDir, 0755); err != nil {
		log.Fatal(err)
	}
	profiles.load(cfg.ProfilePath)
	go channels.janitor()

	srv := &http.Server{Addr: cfg.Listen, Handler: newMux(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("hlsproxy %s на %s: mode=%s delay=%s max-delay=%s concurrency=%d",
		version, cfg.Listen, cfg.Mode, cfg.Delay, cfg.MaxDelay, cfg.Concurrency)
	log.Fatal(srv.ListenAndServe())
}

func normalizeConfig() {
	cfg.StartSegs = max(cfg.StartSegs, 1)
	cfg.Window = max(cfg.Window, cfg.StartSegs+2)
	cfg.Concurrency = min(max(cfg.Concurrency, 1), 6)
	cfg.MaxAttempts = max(cfg.MaxAttempts, 1)
	switch cfg.Mode {
	case "auto", "advise", "off":
	default:
		cfg.Mode = "auto"
	}
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist", playlistHandler)
	mux.HandleFunc("/proxy", playlistHandler)
	mux.HandleFunc("/seg/", segHandler)
	mux.HandleFunc("/ts", legacyTSHandler)
	mux.HandleFunc("/status", apiStatus)
	mux.HandleFunc("/api/status", apiStatus)
	mux.HandleFunc("/api/channel/", apiChannel)
	mux.HandleFunc("/api/profiles", apiProfiles)
	mux.HandleFunc("/ui", uiHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/ui", http.StatusFound)
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
		name := channels.nameOf(src)
		writeM3U(w, rewriteMaster(body, base, cfg.AllVariants, func(u string) string {
			channels.setName(u, name)
			return wrap(u)
		}))
	case kindChannels:
		for u, n := range channelNames(body, base) {
			channels.setName(u, n)
		}
		writeM3U(w, rewriteChannels(body, base, wrap))
	case kindMedia:
		p := parseMedia(body, base)
		if p.endList || p.byteRange || len(p.segments) == 0 {
			writeM3U(w, absolutizeMedia(body, base))
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
	c.noteClientSeg(id)
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
	if until, on := c.holdUntilTime(); on && r.Method == http.MethodGet && r.Header.Get("Range") == "" {
		trickle(w, r, f, st.Size(), until)
		return
	}
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// trickle: during an auto-pause send the segment one TS packet every 3 s (keeps
// the player's read timeout happy, gives it no playable media), then the rest.
func trickle(w http.ResponseWriter, r *http.Request, f *os.File, size int64, until time.Time) {
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	pkt := make([]byte, 188)
	for {
		n, _ := f.Read(pkt)
		if n > 0 {
			if _, err := w.Write(pkt[:n]); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		left := time.Until(until)
		if n == 0 || left <= 0 {
			break
		}
		select {
		case <-time.After(min(left, 3*time.Second)):
		case <-r.Context().Done():
			return
		}
	}
	io.Copy(w, f)
}

func legacyTSHandler(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if u == "" {
		http.Error(w, "url= required", http.StatusBadRequest)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
	if err != nil {
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
