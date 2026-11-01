package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

/*
Модель канала (один upstream media-плейлист):

  upstream:  ... [s10][s11][s12][s13][s14][s15][s16]  <- live edge провайдера
  кэш:                 [ok ][ok ][ok ][ok ][dl ][   ]  <- качаем по порядку, 2 потока
  плееру:    [s8 ][s9 ][s10][s11]                     <- край, отстающий на Delay

Край, отдаваемый плееру, двигается строго по часам (1 сегмент за его длительность)
и только по уже полностью скачанным сегментам. Рывки провайдера гасятся запасом
готовых сегментов между краем плеера и краем провайдера. Если запас кончился и край
встал — после восстановления он догоняет расписание пачкой, возвращая плееру буфер.
*/

const (
	stPending = iota
	stDownloading
	stReady
	stFailed
)

type seg struct {
	id       int64
	uri      string
	dur      float64
	disc     bool
	keyLine  string
	mapLine  string
	ext      string
	state    int
	attempts int
	nextTry  time.Time
	readyAt  time.Time
	size     int64
	path     string
	lastErr  string
	done     chan struct{} // closed on ready/failed
}

type channel struct {
	id      string
	srcURL  string // URL as requested by the player
	pollURL string // sticky final URL after redirects
	ua      string

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}

	lastAccess atomic.Int64

	mu        sync.Mutex
	segs      []*seg // contiguous ids, ascending
	nextID    int64
	lastUp    int64
	haveUp    bool
	uriMode   bool
	seenURI   map[string]struct{}
	seenOrder []string
	version   int
	targetDur float64
	startSet  []*seg
	started   bool
	startedCh chan struct{}
	exposed   []*seg
	outSeq0   int64
	discSeq   int64
	servedEnd int64 // id of last segment passed by the edge (exposed or skipped)
	pendDisc  bool
	edgeAt    time.Time
	pollFails int
	inStall   bool
	created   time.Time
	stats     chanStats
}

type chanStats struct {
	Downloads  int64
	Retries    int64
	Failed     int64
	Skipped    int64
	Stalls     int64
	StallSec   float64
	Bytes      int64
	SpeedMbps  float64 // EWMA
	RealtimeX  float64 // EWMA: seg duration / download time
	LastErr    string
	LastErrAt  time.Time
	PollErrors int64
	Resets     int64
}

func chanID(src string) string {
	h := md5.Sum([]byte(src))
	return hex.EncodeToString(h[:6])
}

func shortURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "?"
	}
	pth := p.Path
	if len(pth) > 40 {
		pth = "…" + pth[len(pth)-39:]
	}
	return p.Host + pth
}

func segExt(u string) string {
	p, err := url.Parse(u)
	if err == nil {
		switch e := strings.ToLower(path.Ext(p.Path)); e {
		case ".ts", ".m4s", ".mp4", ".aac", ".m4a", ".mp3", ".vtt", ".webvtt":
			return e
		}
	}
	return ".ts"
}

// ---------------------------------------------------------------- registry

type registry struct {
	mu sync.Mutex
	m  map[string]*channel
}

var channels = &registry{m: map[string]*channel{}}

func (r *registry) get(id string) *channel {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}

func (r *registry) list() []*channel {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*channel, 0, len(r.m))
	for _, c := range r.m {
		out = append(out, c)
	}
	return out
}

// create registers a new channel from an already fetched media playlist.
func (r *registry) create(src, finalURL, ua string, p *mediaPlaylist) *channel {
	id := chanID(src)
	r.mu.Lock()
	if c := r.m[id]; c != nil {
		r.mu.Unlock()
		return c
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &channel{
		id: id, srcURL: src, pollURL: finalURL, ua: ua,
		ctx: ctx, cancel: cancel,
		wake:      make(chan struct{}, 1),
		startedCh: make(chan struct{}),
		seenURI:   map[string]struct{}{},
		created:   time.Now(),
		servedEnd: -1,
	}
	c.touch()
	r.m[id] = c
	r.mu.Unlock()

	os.MkdirAll(c.dir(), 0755)
	c.mu.Lock()
	c.ingest(p)
	c.mu.Unlock()
	log.Printf("[%s] старт канала %s (сегмент %.1fс, окно провайдера %d сегм.)", id, shortURL(src), p.targetDuration, len(p.segments))

	go c.pollLoop()
	go c.schedLoop()
	return c
}

func (r *registry) remove(c *channel) {
	r.mu.Lock()
	if r.m[c.id] == c {
		delete(r.m, c.id)
	}
	r.mu.Unlock()
	c.cancel()
	os.RemoveAll(c.dir())
}

func (r *registry) janitor() {
	for range time.Tick(5 * time.Second) {
		now := time.Now().UnixNano()
		for _, c := range r.list() {
			if time.Duration(now-c.lastAccess.Load()) > cfg.Idle {
				c.mu.Lock()
				st := c.stats
				c.mu.Unlock()
				log.Printf("[%s] стоп (нет зрителей %s). Итог: фризов-источника %d (%.1fс), ретраев %d, потерянных сегм. %d",
					c.id, cfg.Idle, st.Stalls, st.StallSec, st.Retries, st.Skipped)
				r.remove(c)
			}
		}
	}
}

// ---------------------------------------------------------------- channel

func (c *channel) dir() string { return filepath.Join(cfg.CacheDir, c.id) }
func (c *channel) touch()      { c.lastAccess.Store(time.Now().UnixNano()) }

func (c *channel) kick() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *channel) segByID(id int64) *seg {
	if len(c.segs) == 0 {
		return nil
	}
	i := id - c.segs[0].id
	if i < 0 || i >= int64(len(c.segs)) {
		return nil
	}
	return c.segs[i]
}

func (c *channel) newSeg(rs rawSeg, disc bool) *seg {
	s := &seg{
		id: c.nextID, uri: rs.uri, dur: rs.dur, disc: rs.disc || disc,
		keyLine: rs.keyLine, mapLine: rs.mapLine, ext: segExt(rs.uri),
		done: make(chan struct{}),
	}
	if s.dur <= 0 {
		s.dur = c.targetDur
	}
	s.path = filepath.Join(c.dir(), fmt.Sprintf("%d%s", s.id, s.ext))
	c.nextID++
	c.segs = append(c.segs, s)
	return s
}

func (c *channel) markSeen(uri string) {
	if _, ok := c.seenURI[uri]; ok {
		return
	}
	c.seenURI[uri] = struct{}{}
	c.seenOrder = append(c.seenOrder, uri)
	if len(c.seenOrder) > 500 {
		delete(c.seenURI, c.seenOrder[0])
		c.seenOrder = c.seenOrder[1:]
	}
}

// ingest merges a fresh upstream playlist. Caller holds c.mu.
func (c *channel) ingest(p *mediaPlaylist) {
	if p.version > c.version {
		c.version = p.version
	}
	if p.targetDuration > c.targetDur {
		c.targetDur = p.targetDuration
	}
	n := len(p.segments)
	if n == 0 {
		return
	}

	if !c.haveUp {
		c.haveUp = true
		c.uriMode = !p.hasMediaSeq
		// Choose the starting edge: the newest segment that is at least Delay behind
		// the provider's live edge, but expose at least StartSegs segments.
		cum := make([]float64, n)
		t := 0.0
		for i, s := range p.segments {
			t += s.dur
			cum[i] = t
		}
		e := -1
		for i := n - 1; i >= 0; i-- {
			if t-cum[i] >= cfg.Delay.Seconds() {
				e = i
				break
			}
		}
		minE := cfg.StartSegs - 1
		if minE > n-1 {
			minE = n - 1
		}
		if e < minE {
			e = minE
		}
		start := e - cfg.StartSegs + 1
		if start < 0 {
			start = 0
		}
		for i := start; i < n; i++ {
			s := c.newSeg(p.segments[i], false)
			if i <= e {
				c.startSet = append(c.startSet, s)
			}
		}
		for _, s := range p.segments {
			c.markSeen(s.uri)
		}
		c.lastUp = p.mediaSeq + int64(n) - 1
		return
	}

	if c.uriMode {
		for _, rs := range p.segments {
			if _, ok := c.seenURI[rs.uri]; ok {
				continue
			}
			c.markSeen(rs.uri)
			c.newSeg(rs, false)
		}
		return
	}

	newest := p.mediaSeq + int64(n) - 1
	if newest < c.lastUp-int64(2*n)-5 || p.mediaSeq > c.lastUp+1000 {
		log.Printf("[%s] провайдер сбросил нумерацию (%d -> %d), продолжаю с разрывом", c.id, c.lastUp, p.mediaSeq)
		c.stats.Resets++
		c.lastUp = p.mediaSeq - 1
		for i, rs := range p.segments {
			c.newSeg(rs, i == 0)
		}
		c.lastUp = newest
		return
	}
	for i, rs := range p.segments {
		up := p.mediaSeq + int64(i)
		if up <= c.lastUp {
			continue
		}
		gap := up > c.lastUp+1
		if gap {
			log.Printf("[%s] пропущено %d сегм. у провайдера (опрос не успел)", c.id, up-c.lastUp-1)
		}
		c.newSeg(rs, gap)
		c.lastUp = up
	}
}

func (c *channel) pollInterval() time.Duration {
	d := time.Duration(c.targetDur / 2 * float64(time.Second))
	if d < time.Second {
		d = time.Second
	}
	if d > 6*time.Second {
		d = 6 * time.Second
	}
	return d
}

func (c *channel) pollLoop() {
	for {
		c.mu.Lock()
		iv := c.pollInterval()
		if c.pollFails > 0 {
			iv = time.Second
		}
		c.mu.Unlock()
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(iv):
		}

		c.mu.Lock()
		pu := c.pollURL
		if c.pollFails >= 2 {
			pu = c.srcURL // sticky URL may have expired, go through the original
		}
		c.mu.Unlock()

		body, final, err := fetchPlaylist(c.ctx, pu, c.ua)
		if err == nil && detectKind(body) != kindMedia {
			err = fmt.Errorf("upstream вернул не media-плейлист")
		}
		c.mu.Lock()
		if err != nil {
			if c.ctx.Err() == nil {
				c.pollFails++
				c.stats.PollErrors++
				c.setErr("playlist: " + err.Error())
				if c.pollFails == 3 || c.pollFails%20 == 0 {
					log.Printf("[%s] ошибка плейлиста x%d: %v", c.id, c.pollFails, err)
				}
			}
			c.mu.Unlock()
			continue
		}
		c.pollFails = 0
		c.pollURL = final
		base, _ := url.Parse(final)
		c.ingest(parseMedia(body, base))
		c.mu.Unlock()
		c.kick()
	}
}

func (c *channel) setErr(e string) {
	c.stats.LastErr = e
	c.stats.LastErrAt = time.Now()
}

func (c *channel) schedLoop() {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		case <-c.wake:
		}
		c.mu.Lock()
		now := time.Now()
		c.tryStart(now, false)
		c.advance(now)
		c.cleanup()
		c.dispatch(now)
		c.mu.Unlock()
	}
}

// dispatch starts downloads in playlist order, up to Concurrency at once.
func (c *channel) dispatch(now time.Time) {
	active := 0
	for _, s := range c.segs {
		if s.state == stDownloading {
			active++
		}
	}
	for _, s := range c.segs {
		if active >= cfg.Concurrency {
			return
		}
		if s.id <= c.servedEnd || s.state != stPending || now.Before(s.nextTry) {
			continue
		}
		s.state = stDownloading
		s.attempts++
		active++
		go c.download(s)
	}
}

func (c *channel) download(s *seg) {
	c.mu.Lock()
	dur := s.dur
	c.mu.Unlock()
	attemptCap := time.Duration(math.Max(30, 4*dur) * float64(time.Second))
	t0 := time.Now()
	n, err := fetchToFile(c.ctx, s.uri, c.ua, s.path, cfg.IdleRead, attemptCap)
	el := time.Since(t0).Seconds()

	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.kick()
	c.stats.Bytes += n
	if c.ctx.Err() != nil {
		return
	}
	if err != nil {
		c.stats.Retries++
		s.lastErr = err.Error()
		c.setErr(fmt.Sprintf("seg %d: %v", s.id, err))
		if s.attempts >= cfg.MaxAttempts || err == errBadPayload && s.attempts >= 3 {
			s.state = stFailed
			c.stats.Failed++
			close(s.done)
			os.Remove(s.path + ".part")
			log.Printf("[%s] сегмент %d потерян после %d попыток: %v", c.id, s.id, s.attempts, err)
			return
		}
		back := time.Duration(250*(1<<min(s.attempts, 4))) * time.Millisecond // 0.5..4 s
		s.state = stPending
		s.nextTry = time.Now().Add(back)
		if s.attempts >= 2 {
			log.Printf("[%s] сегмент %d, попытка %d: %v", c.id, s.id, s.attempts, err)
		}
		return
	}
	st, _ := os.Stat(s.path)
	if st != nil {
		s.size = st.Size()
	}
	s.state = stReady
	s.readyAt = time.Now()
	close(s.done)
	c.stats.Downloads++
	if el > 0.05 && s.size > 0 {
		mbps := float64(s.size) * 8 / 1e6 / el
		rx := dur / el
		if c.stats.SpeedMbps == 0 {
			c.stats.SpeedMbps, c.stats.RealtimeX = mbps, rx
		} else {
			c.stats.SpeedMbps = 0.8*c.stats.SpeedMbps + 0.2*mbps
			c.stats.RealtimeX = 0.8*c.stats.RealtimeX + 0.2*rx
		}
	}
}

// tryStart exposes the initial segments once they are downloaded.
// force: expose whatever contiguous prefix is ready (player is waiting too long).
func (c *channel) tryStart(now time.Time, force bool) bool {
	if c.started {
		return true
	}
	if len(c.startSet) == 0 {
		return false
	}
	allDone := true
	for _, s := range c.startSet {
		if s.state != stReady && s.state != stFailed {
			allDone = false
		}
	}
	if !allDone && !force {
		return false
	}
	for _, s := range c.startSet {
		if s.state == stFailed {
			c.servedEnd = s.id
			c.pendDisc = len(c.exposed) > 0
			continue
		}
		if s.state != stReady {
			break
		}
		s.disc = s.disc || c.pendDisc
		c.pendDisc = false
		c.exposed = append(c.exposed, s)
		c.servedEnd = s.id
	}
	if len(c.exposed) == 0 {
		return false
	}
	c.outSeq0 = 0
	c.started = true
	c.edgeAt = now
	close(c.startedCh)
	return true
}

func (c *channel) forceStart() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tryStart(time.Now(), true)
}

// advance moves the player-visible edge along the wall clock over ready segments.
func (c *channel) advance(now time.Time) {
	if !c.started {
		return
	}
	maxBurst := time.Duration(float64(cfg.MaxBurstSegs) * c.targetDur * float64(time.Second))
	for {
		s := c.segByID(c.servedEnd + 1)
		if s == nil {
			break
		}
		if s.state == stFailed {
			c.servedEnd = s.id
			c.pendDisc = true
			c.stats.Skipped++
			continue
		}
		due := c.edgeAt.Add(time.Duration(s.dur * float64(time.Second)))
		if now.Before(due) {
			break
		}
		if s.state != stReady {
			break // edge waits; player eats its own buffer meanwhile
		}
		if late := s.readyAt.Sub(due); late > 300*time.Millisecond {
			if !c.inStall {
				c.stats.Stalls++
				c.stats.StallSec += late.Seconds()
				log.Printf("[%s] запас исчерпан: сегмент %d опоздал на %.1fс, край стоял (плеер ел свой буфер), догоняю", c.id, s.id, late.Seconds())
			}
			c.inStall = true
		} else {
			c.inStall = false
		}
		// Anchored schedule: after a stall we catch up in a burst (refilling the
		// player's buffer), but never more than maxBurst at once.
		c.edgeAt = due
		if floor := now.Add(-maxBurst); c.edgeAt.Before(floor) {
			c.edgeAt = floor
		}
		s.disc = s.disc || c.pendDisc
		c.pendDisc = false
		c.exposed = append(c.exposed, s)
		c.servedEnd = s.id
	}
	for len(c.exposed) > cfg.Window {
		if c.exposed[0].disc {
			c.discSeq++
		}
		c.exposed = c.exposed[1:]
		c.outSeq0++
	}
}

// cleanup frees segments that are well behind the player's window.
func (c *channel) cleanup() {
	if len(c.exposed) == 0 {
		return
	}
	keepFrom := c.exposed[0].id - 3
	k := 0
	for k < len(c.segs) && c.segs[k].id < keepFrom && c.segs[k].state != stDownloading {
		os.Remove(c.segs[k].path)
		os.Remove(c.segs[k].path + ".part")
		k++
	}
	if k > 0 {
		c.segs = append([]*seg(nil), c.segs[k:]...)
	}
}

// render produces the delayed media playlist for the player. Caller holds c.mu.
func (c *channel) render(host string) string {
	var b strings.Builder
	v := c.version
	if v < 3 {
		v = 3
	}
	td := int(math.Ceil(c.targetDur))
	for _, s := range c.exposed {
		if d := int(math.Ceil(s.dur - 0.0005)); d > td {
			td = d
		}
	}
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:%d\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", v, td, c.outSeq0)
	if c.discSeq > 0 {
		fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", c.discSeq)
	}
	var prevKey, prevMap string
	for i, s := range c.exposed {
		if s.disc {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if i == 0 || s.keyLine != prevKey {
			if s.keyLine != "" {
				b.WriteString(s.keyLine + "\n")
			} else if i > 0 {
				b.WriteString("#EXT-X-KEY:METHOD=NONE\n")
			}
		}
		if (i == 0 || s.mapLine != prevMap) && s.mapLine != "" {
			b.WriteString(s.mapLine + "\n")
		}
		prevKey, prevMap = s.keyLine, s.mapLine
		fmt.Fprintf(&b, "#EXTINF:%.3f,\nhttp://%s/seg/%s/%d%s\n", s.dur, host, c.id, s.id, s.ext)
	}
	return b.String()
}

// ---------------------------------------------------------------- status

type chanStatus struct {
	ID            string  `json:"id"`
	Source        string  `json:"source"`
	UptimeSec     int64   `json:"uptime_s"`
	IdleSec       int64   `json:"idle_s"`
	Started       bool    `json:"started"`
	SegDur        float64 `json:"seg_dur_s"`
	DelaySec      float64 `json:"delay_s"`       // provider edge - player edge
	ReadyAhead    float64 `json:"ready_ahead_s"` // contiguous downloaded reserve past player edge
	Exposed       int     `json:"exposed"`
	Downloading   int     `json:"downloading"`
	Pending       int     `json:"pending"`
	CacheMB       float64 `json:"cache_mb"`
	SpeedMbps     float64 `json:"speed_mbps"`
	RealtimeX     float64 `json:"realtime_x"`
	Stalls        int64   `json:"stalls"`
	StallSec      float64 `json:"stall_s"`
	Retries       int64   `json:"retries"`
	Failed        int64   `json:"failed"`
	Skipped       int64   `json:"skipped"`
	PollErrors    int64   `json:"poll_errors"`
	LastErr       string  `json:"last_error,omitempty"`
	LastErrAgoSec int64   `json:"last_error_ago_s,omitempty"`
}

func (c *channel) status() chanStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	st := chanStatus{
		ID: c.id, Source: shortURL(c.srcURL),
		UptimeSec: int64(now.Sub(c.created).Seconds()),
		IdleSec:   int64(time.Duration(now.UnixNano() - c.lastAccess.Load()).Seconds()),
		Started:   c.started, SegDur: c.targetDur, Exposed: len(c.exposed),
		SpeedMbps: round1(c.stats.SpeedMbps), RealtimeX: round1(c.stats.RealtimeX),
		Stalls: c.stats.Stalls, StallSec: round1(c.stats.StallSec),
		Retries: c.stats.Retries, Failed: c.stats.Failed, Skipped: c.stats.Skipped,
		PollErrors: c.stats.PollErrors, LastErr: c.stats.LastErr,
	}
	if !c.stats.LastErrAt.IsZero() {
		st.LastErrAgoSec = int64(now.Sub(c.stats.LastErrAt).Seconds())
	}
	contiguous := true
	var bytes int64
	for _, s := range c.segs {
		switch s.state {
		case stDownloading:
			st.Downloading++
		case stPending:
			st.Pending++
		case stReady:
			bytes += s.size
		}
		if s.id > c.servedEnd {
			st.DelaySec += s.dur
			if contiguous && s.state == stReady {
				st.ReadyAhead += s.dur
			} else if s.state != stFailed {
				contiguous = false
			}
		}
	}
	st.DelaySec = round1(st.DelaySec)
	st.ReadyAhead = round1(st.ReadyAhead)
	st.CacheMB = round1(float64(bytes) / 1e6)
	return st
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
