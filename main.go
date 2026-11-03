package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var version = "3.5.0"

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
	LogDir       string
	NamesPath    string
	Subs         subsConfig
	AccessKey    string
	ExtUI        bool
	MaxChannels  int
	MaxExtChans  int
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
	flag.StringVar(&cfg.AccessKey, "access-key", "", "ключ для доступа извне: http://<IP>:порт/k/<ключ>/... (пусто = извне запрещено)")
	flag.StringVar(&saved.path, "uci", "/etc/config/hlsproxy", "UCI-файл с сохранёнными плейлистами (config playlist)")
	flag.BoolVar(&cfg.ExtUI, "ext-ui", false, "разрешить панель и API внешним клиентам с ключом (по умолчанию только просмотр)")
	flag.IntVar(&cfg.MaxChannels, "max-channels", 6, "макс. одновременных каналов")
	flag.IntVar(&cfg.MaxExtChans, "max-ext-channels", 2, "из них открытых внешними клиентами")
	flag.BoolVar(&cfg.Subs.Enabled, "subs", false, "авто-субтитры (ключи: env HLSPROXY_ASR_KEY, HLSPROXY_LLM_KEY)")
	flag.BoolVar(&cfg.Subs.Default, "subs-default", false, "включать субтитры для каналов без явного выбора")
	flag.StringVar(&cfg.Subs.Lang, "subs-lang", "ru", "язык субтитров (ru — перевод, en — без перевода)")
	flag.StringVar(&cfg.Subs.Proxy, "subs-proxy", "", "прокси до API распознавания/перевода (http://, socks5://)")
	flag.StringVar(&cfg.Subs.ASRURL, "subs-asr-url", "https://api.groq.com/openai/v1/audio/transcriptions", "API распознавания (OpenAI-совместимый)")
	flag.StringVar(&cfg.Subs.ASRModel, "subs-asr-model", "whisper-large-v3-turbo", "модель распознавания")
	flag.StringVar(&cfg.Subs.LLMURL, "subs-llm-url", "https://api.deepseek.com/chat/completions", "API перевода (OpenAI-совместимый)")
	flag.StringVar(&cfg.Subs.LLMModel, "subs-llm-model", "deepseek-chat", "модель перевода")
	flag.StringVar(&cfg.NamesPath, "names", "/etc/hlsproxy/names.json", "справочник названий каналов")
	flag.StringVar(&cfg.LogDir, "log-dir", "/tmp/hlsproxy-log", "журнал и история сеансов (пусто = выкл.)")
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
	logs.init(cfg.LogDir)
	names.load(cfg.NamesPath)
	initSubs()
	logs.write(time.Now().Format("2006-01-02 15:04:05") + "  info  [hlsproxy] запуск " + version + ", mode=" + cfg.Mode + "\n")
	go channels.janitor()

	srv := &http.Server{Addr: cfg.Listen, Handler: newMux(), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10}
	if cfg.AccessKey != "" && len(cfg.AccessKey) < 12 {
		log.Printf("ВНИМАНИЕ: access_key короче 12 символов")
	}
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

func newMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist", playlistHandler)
	mux.HandleFunc("/proxy", playlistHandler)
	mux.HandleFunc("/pl/", savedHandler)
	mux.HandleFunc("/subs/", subsHandler)
	mux.HandleFunc("/seg/", segHandler)
	mux.HandleFunc("/ts", legacyTSHandler)
	mux.HandleFunc("/status", apiStatus)
	mux.HandleFunc("/api/status", apiStatus)
	mux.HandleFunc("/api/channel/", apiChannel)
	mux.HandleFunc("/api/profiles", apiProfiles)
	mux.HandleFunc("/api/sessions", apiSessions)
	mux.HandleFunc("/api/sessions/", apiSessions)
	mux.HandleFunc("/api/log", apiLog)
	mux.HandleFunc("/ui", uiHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, baseURL(r)+"/ui", http.StatusFound)
	})
	return withAccess(mux)
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
	servePlaylist(w, r, src, false)
}

// servePlaylist: trusted=true for playlists saved by the owner on the router —
// their hosts are allowed and learned even when an external client opens them.
func servePlaylist(w http.ResponseWriter, r *http.Request, src string, trusted bool) {
	if c := channels.get(chanID(src)); c != nil {
		serveChannel(c, w, r)
		return
	}
	if trusted {
		if p, err := url.Parse(src); err == nil {
			if ip := net.ParseIP(p.Hostname()); ip == nil || !routerIP(ip) {
				learnHost(src)
			}
		}
	}
	if ok, why := upstreamAllowed(r, src); !ok {
		denied(w, r, why)
		return
	}

	ua := clientUA(r)
	ext := isExternal(r)
	fctx := r.Context()
	if ext {
		fctx = markUntrusted(fctx)
	}
	// hosts from this playlist become "known" (allowed even on private addresses)
	// only if the request is from LAN or the playlist itself came from a known host
	trustLearn := !ext
	if p, err := url.Parse(src); err == nil && hostKnown(p.Hostname()) {
		trustLearn = true
	}
	body, final, err := fetchPlaylist(fctx, src, ua)
	if err != nil {
		log.Printf("плейлист %s: %v", shortURL(src), err)
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	base, _ := url.Parse(final)
	wrap := func(u string) string {
		if trustLearn {
			learnHost(u)
		}
		return baseURL(r) + "/proxy?url=" + url.QueryEscape(u)
	}

	switch detectKind(body) {
	case kindMaster:
		name := channels.nameOf(src)
		var variants []string
		out := rewriteMaster(body, base, cfg.AllVariants, func(u string) string {
			channels.setName(u, name)
			variants = append(variants, u)
			return wrap(u) + "&m=1"
		})
		// one video variant and subtitles wanted for it: add the subtitle rendition
		if vid := ""; len(variants) > 0 {
			last := variants[len(variants)-1]
			vid = chanID(last)
			if subsWanted(vid) && strings.Count(out, "#EXT-X-STREAM-INF") == 1 && !strings.Contains(out, "TYPE=SUBTITLES") {
				out = injectSubs(out, baseURL(r), vid)
			}
		}
		writeM3U(w, out)
	case kindChannels:
		names.noteList(src)
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
		c := channels.create(src, final, ua, p, ext)
		if c == nil {
			http.Error(w, "слишком много одновременных каналов", http.StatusServiceUnavailable)
			return
		}
		serveChannel(c, w, r)
	default:
		http.Error(w, "upstream did not return an m3u playlist", http.StatusBadGateway)
	}
}

func serveChannel(c *channel, w http.ResponseWriter, r *http.Request) {
	c.touch()
	// subtitles: the player gets a master playlist with a subtitle rendition;
	// the media itself is behind &m=1
	if r.URL.Query().Get("m") == "" {
		c.mu.Lock()
		on := c.subs != nil && c.subs.on
		bw := int(c.diag.BitrateMbps * 1.2e6)
		src := c.srcURL
		c.mu.Unlock()
		if on {
			writeM3U(w, subsMaster(baseURL(r), baseURL(r)+"/proxy?url="+url.QueryEscape(src)+"&m=1", c.id, bw))
			return
		}
	}
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
	c.noteViewer(clientIP(r).String(), r.UserAgent(), isExternal(r), 0)
	c.notePoll(time.Now())
	c.advance(time.Now())
	body := c.render(baseURL(r))
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
	cw := &countingWriter{ResponseWriter: w}
	t0 := time.Now()
	held := 0.0
	full := r.Header.Get("Range") == ""
	if until, on := c.holdUntilTime(); on && r.Method == http.MethodGet && full {
		held = trickle(cw, r, f, st.Size(), until)
	} else {
		http.ServeContent(cw, r, "", st.ModTime(), f)
	}
	if r.Method != http.MethodGet {
		return
	}
	sec := time.Since(t0).Seconds() - held
	rec := clRec{ID: id, KB: cw.n / 1024, Held: round1(held),
		Aborted: r.Context().Err() != nil || full && cw.n < st.Size()}
	c.mu.Lock()
	rec.T = round1(c.rel(time.Now()))
	sec = math.Max(sec, 0.001)
	rec.Sec = math.Round(sec*1000) / 1000
	rec.X = round2(math.Min(s.dur/sec, 99))
	rec.Mbps = round1(math.Min(float64(cw.n)*8/1e6/sec, 9999))
	if rec.Aborted {
		c.stats.ClAborted++
		c.addEvent("warn", fmt.Sprintf("плеер оборвал загрузку сегм. %d на %d из %d КБ (таймаут/переключение/ошибка у плеера)", id, cw.n/1024, st.Size()/1024))
	}
	c.addCl(rec)
	c.served += cw.n
	c.noteViewer(clientIP(r).String(), r.UserAgent(), isExternal(r), cw.n)
	c.mu.Unlock()
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (cw *countingWriter) Write(b []byte) (int, error) {
	n, err := cw.ResponseWriter.Write(b)
	cw.n += int64(n)
	return n, err
}

func (cw *countingWriter) Flush() {
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// trickle: during an auto-pause send the segment one TS packet every 3 s (keeps
// the player's read timeout happy, gives it no playable media), then the rest.
// Returns how long the response was held back.
func trickle(w http.ResponseWriter, r *http.Request, f *os.File, size int64, until time.Time) float64 {
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	pkt := make([]byte, 188)
	t0 := time.Now()
	for {
		n, _ := f.Read(pkt)
		if n > 0 {
			if _, err := w.Write(pkt[:n]); err != nil {
				return time.Since(t0).Seconds()
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
			return time.Since(t0).Seconds()
		}
	}
	held := time.Since(t0).Seconds()
	io.Copy(w, f)
	return held
}

func legacyTSHandler(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if u == "" {
		http.Error(w, "url= required", http.StatusBadRequest)
		return
	}
	if isExternal(r) {
		denied(w, r, "/ts недоступен извне")
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
