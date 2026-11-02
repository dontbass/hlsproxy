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
  кэш:                 [ok ][ok ][ok ][ok ][dl ][   ]  <- качаем по порядку
  плееру:    [s8 ][s9 ][s10][s11]                     <- край, отстающий на delay

Край для плеера двигается по часам и только по полностью скачанным сегментам.
Всё время канала отсчитывается от T0; у каждого сегмента есть позиция на шкале
контента (content). Для сегмента q = (готов − T0) − (content + dur): насколько
поздно он стал готов относительно идеального эфира. Защита = насколько край
отстаёт от этой шкалы + буфер плеера. Нужный запас = разброс q (см. analyze.go).
*/

const (
	stPending = iota
	stDownloading
	stReady
	stFailed
)

type seg struct {
	id         int64
	uri        string
	dur        float64
	content    float64 // position on the content timeline (s)
	disc       bool
	initial    bool // was already in the provider window when the channel started
	keyLine    string
	mapLine    string
	ext        string
	state      int
	attempts   int
	nextTry    time.Time
	appearedAt time.Time
	dlStart    time.Time
	readyAt    time.Time
	size       int64
	path       string
	lastErr    string
	done       chan struct{} // closed on ready/failed
}

type segRec struct {
	ID       int64   `json:"id"`
	T        float64 `json:"t"`    // ready (or failed) time, s since T0
	Dur      float64 `json:"dur"`  // s
	SizeKB   int64   `json:"kb"`   //
	Mbps     float64 `json:"mbps"` // stream bitrate of the segment
	X        float64 `json:"x"`    // dur / (ready - first attempt start)
	Wait     float64 `json:"wait"` // first attempt start - appeared
	Q        float64 `json:"q"`    // readiness offset, see header
	Attempts int     `json:"tries"`
	Failed   bool    `json:"failed,omitempty"`
	Gap      bool    `json:"gap,omitempty"`  // lost at provider before we saw it
	Init     bool    `json:"init,omitempty"` // from the start window: Q not meaningful
}

type sample struct {
	T      float64 `json:"t"`
	Ahead  float64 `json:"ahead"`  // ready reserve past the player edge (s)
	Prot   float64 `json:"prot"`   // protection: edge lag vs best case + player buffer (s)
	Need   float64 `json:"need"`   // reserve needed for the last 10 min (s)
	Client float64 `json:"client"` // estimated player buffer (s)
	Busy   float64 `json:"-"`      // cumulative seconds with ≥1 download active
	CDL    float64 `json:"-"`      // cumulative content seconds downloaded
	Stall  bool    `json:"stall,omitempty"`
	Hold   bool    `json:"hold,omitempty"`
	Freeze bool    `json:"freeze,omitempty"`
}

type event struct {
	T    float64 `json:"t"`
	At   string  `json:"at"`
	Kind string  `json:"kind"` // info | warn | crit | tune
	Text string  `json:"text"`
}

type channel struct {
	id      string
	srcURL  string
	pollURL string
	ua      string
	name    string

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}

	lastAccess atomic.Int64

	mu        sync.Mutex
	t0        time.Time
	segs      []*seg
	nextID    int64
	lastUp    int64
	haveUp    bool
	uriMode   bool
	seenURI   map[string]struct{}
	seenOrder []string
	version   int
	targetDur float64
	provWin   float64 // provider playlist window (s)

	startSet  []*seg
	started   bool
	startedCh chan struct{}
	exposed   []*seg
	outSeq0   int64
	discSeq   int64
	servedEnd int64
	edgeEnd   float64 // content end of the last segment passed by the edge
	pendDisc  bool
	edgeAt    time.Time
	lastExp   time.Time
	contentNx float64
	pollFails int

	// tuning (per channel)
	delay      float64 // s, behind provider edge at start
	conc       int
	lastTuneAt time.Time
	manual     bool // parameters pinned by the user: auto-tune only observes
	sessNeed   float64

	// stall / hold
	stallActive bool
	stallAt     time.Time
	holding     bool
	holdStart   time.Time
	holdUntil   time.Time
	holdEnd     time.Time
	winSegs     int

	// estimated player
	clBuf      float64
	clPlaying  bool
	clStarted  bool
	clFrozeAt  time.Time
	clLastSeg  int64
	clLastSegT time.Time

	// telemetry
	recs     []segRec
	samples  []sample
	events   []event
	busy     float64
	cdl      float64
	lastTick time.Time
	lastSamp time.Time
	diag     diagnosis
	stats    chanStats
}

type chanStats struct {
	Downloads  int64
	Retries    int64
	Failed     int64
	Skipped    int64
	Gaps       int64
	EdgeStalls int64
	EdgeStallS float64
	Freezes    int64 // estimated at player
	FreezeS    float64
	Holds      int64
	HoldS      float64
	Bytes      int64
	LastErr    string
	LastErrAt  time.Time
	PollErrors int64
	Resets     int64
	ClientRest int64
}

const (
	maxRecs    = 900
	maxSamples = 900
	maxEvents  = 150
)

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
	mu    sync.Mutex
	m     map[string]*channel
	names map[string]string // upstream URL -> channel name from the m3u list
}

var channels = &registry{m: map[string]*channel{}, names: map[string]string{}}

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

func (r *registry) setName(u, name string) {
	if name == "" {
		return
	}
	r.mu.Lock()
	r.names[u] = name
	if len(r.names) > 5000 {
		r.names = map[string]string{u: name}
	}
	r.mu.Unlock()
}

func (r *registry) nameOf(u string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.names[u]
}

func (r *registry) create(src, finalURL, ua string, p *mediaPlaylist) *channel {
	id := chanID(src)
	r.mu.Lock()
	if c := r.m[id]; c != nil {
		r.mu.Unlock()
		return c
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	c := &channel{
		id: id, srcURL: src, pollURL: finalURL, ua: ua, name: r.names[src],
		ctx: ctx, cancel: cancel,
		wake:      make(chan struct{}, 1),
		startedCh: make(chan struct{}),
		seenURI:   map[string]struct{}{},
		t0:        now, lastTick: now,
		servedEnd: -1, clLastSeg: -1,
		delay: cfg.Delay.Seconds(), conc: cfg.Concurrency,
	}
	c.touch()
	r.m[id] = c
	r.mu.Unlock()

	if c.name == "" {
		c.name = shortURL(src)
	}
	prof := profiles.get(id)
	if cfg.Mode != "off" && prof != nil {
		if prof.Delay > 0 {
			c.delay = math.Min(float64(prof.Delay), cfg.MaxDelay.Seconds())
		}
		if prof.Concurrency > 0 {
			c.conc = prof.Concurrency
		}
		c.manual = prof.Manual
	}

	os.MkdirAll(c.dir(), 0755)
	c.mu.Lock()
	c.ingest(p)
	from := "по умолчанию"
	if prof != nil && cfg.Mode != "off" {
		from = "из профиля канала"
	}
	c.addEvent("info", fmt.Sprintf("старт: сегмент %.1f с, окно провайдера %.0f с, delay %.0f с и %d потока (%s)",
		c.targetDur, c.provWin, c.delay, c.conc, from))
	c.mu.Unlock()

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
	c.mu.Lock()
	c.saveProfile(true)
	c.mu.Unlock()
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
				log.Printf("[%s] стоп «%s» (нет зрителей %s). Фризов у плеера (оценка) %d (%.0f с), пауз-добора %d, потеряно %d",
					c.id, c.name, cfg.Idle, st.Freezes, st.FreezeS, st.Holds, st.Skipped+st.Gaps)
				r.remove(c)
			}
		}
	}
}

// ---------------------------------------------------------------- helpers

func (c *channel) dir() string { return filepath.Join(cfg.CacheDir, c.id) }
func (c *channel) touch()      { c.lastAccess.Store(time.Now().UnixNano()) }
func (c *channel) rel(t time.Time) float64 {
	return t.Sub(c.t0).Seconds()
}

func (c *channel) kick() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *channel) addEvent(kind, text string) {
	now := time.Now()
	c.events = append(c.events, event{T: round1(c.rel(now)), At: now.Format("15:04:05"), Kind: kind, Text: text})
	if len(c.events) > maxEvents {
		c.events = c.events[len(c.events)-maxEvents:]
	}
	if kind != "info" {
		log.Printf("[%s] %s", c.id, text)
	}
}

func (c *channel) setErr(e string) {
	c.stats.LastErr = e
	c.stats.LastErrAt = time.Now()
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

func (c *channel) clientBufferNominal() float64 {
	return float64(cfg.StartSegs) * c.targetDur
}

func (c *channel) newSeg(rs rawSeg, disc bool, now time.Time) *seg {
	s := &seg{
		id: c.nextID, uri: rs.uri, dur: rs.dur, disc: rs.disc || disc,
		keyLine: rs.keyLine, mapLine: rs.mapLine, ext: segExt(rs.uri),
		appearedAt: now, done: make(chan struct{}),
	}
	if s.dur <= 0 {
		s.dur = c.targetDur
	}
	s.content = c.contentNx
	c.contentNx += s.dur
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

func (c *channel) addRec(r segRec) {
	c.recs = append(c.recs, r)
	if len(c.recs) > maxRecs {
		c.recs = c.recs[len(c.recs)-maxRecs:]
	}
}

// ---------------------------------------------------------------- upstream

// ingest merges a fresh upstream playlist. Caller holds c.mu.
func (c *channel) ingest(p *mediaPlaylist) {
	now := time.Now()
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
	win := 0.0
	for _, s := range p.segments {
		win += s.dur
	}
	c.provWin = win

	if !c.haveUp {
		c.haveUp = true
		c.uriMode = !p.hasMediaSeq
		cum := make([]float64, n)
		t := 0.0
		for i, s := range p.segments {
			t += s.dur
			cum[i] = t
		}
		e := -1
		for i := n - 1; i >= 0; i-- {
			if t-cum[i] >= c.delay {
				e = i
				break
			}
		}
		minE := min(cfg.StartSegs-1, n-1)
		if e < minE {
			e = minE
		}
		start := max(e-cfg.StartSegs+1, 0)
		// Older segments are "virtually" in the past: backdate their appearance so
		// lateness of the start set is not counted as a provider delay.
		for i := start; i < n; i++ {
			back := time.Duration((t - cum[i]) * float64(time.Second))
			s := c.newSeg(p.segments[i], false, now.Add(-back))
			s.initial = true
			if i <= e {
				c.startSet = append(c.startSet, s)
			}
		}
		// content timeline: put the provider's live edge at T0
		shift := t - cum[start] + p.segments[start].dur
		for _, s := range c.segs {
			s.content -= shift
		}
		c.contentNx -= shift
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
			c.newSeg(rs, false, now)
		}
		return
	}

	newest := p.mediaSeq + int64(n) - 1
	if newest < c.lastUp-int64(2*n)-5 || p.mediaSeq > c.lastUp+1000 {
		c.addEvent("warn", fmt.Sprintf("провайдер сбросил нумерацию (%d → %d), продолжаю с разрывом", c.lastUp, p.mediaSeq))
		c.stats.Resets++
		for i, rs := range p.segments {
			c.newSeg(rs, i == 0, now)
		}
		c.lastUp = newest
		return
	}
	for i, rs := range p.segments {
		up := p.mediaSeq + int64(i)
		if up <= c.lastUp {
			continue
		}
		if miss := up - c.lastUp - 1; miss > 0 {
			// Segments rolled out of the provider window before we saw them: lost.
			lost := float64(miss) * c.targetDur
			c.stats.Gaps += miss
			c.addRec(segRec{ID: -1, T: round1(c.rel(now)), Dur: lost, Gap: true, Failed: true})
			c.contentNx += lost
			c.addEvent("crit", fmt.Sprintf("потеряно у провайдера %d сегм. (≈%.0f с): плейлист не обновлялся дольше, чем его окно %.0f с — это не исправить буфером", miss, lost, c.provWin))
		}
		c.newSeg(rs, up > c.lastUp+1, now)
		c.lastUp = up
	}
}

func (c *channel) pollInterval() time.Duration {
	d := time.Duration(c.targetDur / 2 * float64(time.Second))
	return min(max(d, time.Second), 6*time.Second)
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
			pu = c.srcURL
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
				c.setErr("плейлист: " + err.Error())
				if c.pollFails == 3 || c.pollFails%20 == 0 {
					c.addEvent("warn", fmt.Sprintf("плейлист провайдера недоступен x%d: %v", c.pollFails, err))
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

// ---------------------------------------------------------------- scheduler

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
		c.tick(now)
		c.cleanup()
		c.dispatch(now)
		c.mu.Unlock()
	}
}

func (c *channel) activeDownloads() (active, pending int) {
	for _, s := range c.segs {
		switch s.state {
		case stDownloading:
			active++
		case stPending:
			if s.id > c.servedEnd {
				pending++
			}
		}
	}
	return
}

func (c *channel) dispatch(now time.Time) {
	active, _ := c.activeDownloads()
	for _, s := range c.segs {
		if active >= c.conc {
			return
		}
		if s.id <= c.servedEnd || s.state != stPending || now.Before(s.nextTry) {
			continue
		}
		s.state = stDownloading
		s.attempts++
		if s.dlStart.IsZero() {
			s.dlStart = now
		}
		active++
		go c.download(s)
	}
}

func (c *channel) download(s *seg) {
	c.mu.Lock()
	dur := s.dur
	c.mu.Unlock()
	attemptCap := time.Duration(math.Max(30, 4*dur) * float64(time.Second))
	n, err := fetchToFile(c.ctx, s.uri, c.ua, s.path, cfg.IdleRead, attemptCap)

	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.kick()
	c.stats.Bytes += n
	if c.ctx.Err() != nil {
		return
	}
	now := time.Now()
	if err != nil {
		c.stats.Retries++
		s.lastErr = err.Error()
		c.setErr(fmt.Sprintf("сегм. %d: %v", s.id, err))
		if s.attempts >= cfg.MaxAttempts || err == errBadPayload && s.attempts >= 3 {
			s.state = stFailed
			c.stats.Failed++
			close(s.done)
			os.Remove(s.path + ".part")
			c.addRec(segRec{ID: s.id, T: round1(c.rel(now)), Dur: s.dur, Attempts: s.attempts, Failed: true,
				Wait: round2(s.dlStart.Sub(s.appearedAt).Seconds())})
			c.addEvent("crit", fmt.Sprintf("сегмент %d не скачался за %d попыток (%v): будет пропущен", s.id, s.attempts, err))
			return
		}
		back := time.Duration(250*(1<<min(s.attempts, 4))) * time.Millisecond
		s.state = stPending
		s.nextTry = now.Add(back)
		return
	}
	if st, _ := os.Stat(s.path); st != nil {
		s.size = st.Size()
	}
	s.state = stReady
	s.readyAt = now
	close(s.done)
	c.stats.Downloads++
	c.cdl += s.dur
	el := now.Sub(s.dlStart).Seconds()
	r := segRec{
		ID: s.id, T: round1(c.rel(now)), Dur: round2(s.dur), SizeKB: s.size / 1024,
		Attempts: s.attempts, Wait: round2(s.dlStart.Sub(s.appearedAt).Seconds()),
		Q: round2(c.rel(now) - (s.content + s.dur)), Init: s.initial,
	}
	if s.dur > 0 {
		r.Mbps = round2(float64(s.size) * 8 / 1e6 / s.dur)
	}
	if el > 0.01 {
		r.X = round2(s.dur / el)
	}
	c.addRec(r)
}

// ---------------------------------------------------------------- edge

func (c *channel) expose(s *seg, now time.Time) {
	s.disc = s.disc || c.pendDisc
	c.pendDisc = false
	c.exposed = append(c.exposed, s)
	c.servedEnd = s.id
	c.edgeEnd = s.content + s.dur
	c.lastExp = now
	if c.clStarted {
		c.clBuf += s.dur
	}
}

func (c *channel) skip(s *seg) {
	c.servedEnd = s.id
	c.edgeEnd = s.content + s.dur
	c.pendDisc = true
	c.stats.Skipped++
}

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
			c.skip(s)
			continue
		}
		if s.state != stReady {
			break
		}
		c.expose(s, now)
	}
	if len(c.exposed) == 0 {
		return false
	}
	c.started = true
	c.edgeAt = now
	c.clStarted = true
	c.clBuf = 0
	for _, s := range c.exposed {
		c.clBuf += s.dur
	}
	close(c.startedCh)
	c.addEvent("info", fmt.Sprintf("плееру отдан старт: %d сегм., отставание от эфира ≈%.0f с", len(c.exposed), c.leadSec()))
	return true
}

func (c *channel) forceStart() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tryStart(time.Now(), true)
}

// readyAhead: contiguous downloaded content past the edge (s).
func (c *channel) readyAhead() float64 {
	t := 0.0
	for s := c.segByID(c.servedEnd + 1); s != nil; s = c.segByID(s.id + 1) {
		if s.state == stFailed {
			continue
		}
		if s.state != stReady {
			break
		}
		t += s.dur
	}
	return t
}

// leadSec: how far the edge is behind the provider's newest segment (s).
func (c *channel) leadSec() float64 {
	t := 0.0
	for s := c.segByID(c.servedEnd + 1); s != nil; s = c.segByID(s.id + 1) {
		t += s.dur
	}
	return t
}

// edgeOffset: wall-clock offset of the edge's schedule vs the content timeline.
func (c *channel) edgeOffset() float64 {
	return c.rel(c.edgeAt) - c.edgeEnd
}

func (c *channel) advance(now time.Time) {
	if !c.started {
		return
	}
	maxBurst := time.Duration(float64(cfg.MaxBurstSegs) * c.targetDur * float64(time.Second))
	for {
		s := c.segByID(c.servedEnd + 1)
		if s != nil && s.state == stFailed {
			c.skip(s)
			continue
		}
		dur := c.targetDur
		if s != nil {
			dur = s.dur
		}
		due := c.edgeAt.Add(time.Duration(dur * float64(time.Second)))
		if now.Before(due) {
			break
		}
		if s == nil || s.state != stReady {
			if !c.stallActive {
				c.stallActive = true
				c.stallAt = due
				c.stats.EdgeStalls++
				what := "не скачан"
				if s == nil {
					what = "ещё не появился у провайдера"
				}
				c.addEvent("warn", fmt.Sprintf("запас исчерпан: следующий сегмент %s, край ждёт (плеер ест свой буфер)", what))
			}
			break
		}
		if c.stallActive {
			late := s.readyAt.Sub(c.stallAt).Seconds()
			c.stats.EdgeStallS += math.Max(0, late)
			c.stallActive = false
			if late > 2*c.targetDur {
				c.addEvent("info", fmt.Sprintf("край стоял %.1f с, догоняю", late))
			}
		}
		// Anchored schedule: after a stall catch up in a burst (refills the player),
		// never more than maxBurst at once.
		c.edgeAt = due
		if floor := now.Add(-maxBurst); c.edgeAt.Before(floor) {
			c.edgeAt = floor
		}
		c.expose(s, now)
	}
	// The window must always contain the player's position, including the lag it
	// accumulated through freezes and holds.
	need := cfg.Window
	if c.targetDur > 0 {
		need = max(need, int(math.Ceil((c.clBuf+2*c.targetDur)/c.targetDur))+1)
	}
	if need > c.winSegs {
		c.winSegs = min(need, 60)
	}
	for len(c.exposed) > max(c.winSegs, cfg.Window) {
		if c.exposed[0].disc {
			c.discSeq++
		}
		c.exposed = c.exposed[1:]
		c.outSeq0++
	}
}

// startHold: the player has already run dry and the protection is short of what
// this channel needs. Instead of letting it resume on the first segment (and freeze
// again on the next dip), keep the player buffering a bit longer by trickling the
// segment it waits for. The playlist keeps updating, so the player never declares
// the stream stuck; the time it waits becomes permanent reserve.
func (c *channel) maybeHold(now time.Time) {
	if cfg.Mode != "auto" || c.holding || !c.clStarted || c.clPlaying || !c.clientActive(now) {
		return
	}
	if c.diag.Verdict == "bandwidth" || c.diag.Verdict == "marginal" || now.Sub(c.holdEnd) < 30*time.Second {
		return
	}
	want := math.Max(c.diag.NeedS*1.25+c.targetDur, 2*c.targetDur)
	deficit := want - c.diag.HaveS
	if deficit < 1 {
		return
	}
	room := cfg.MaxDelay.Seconds() + c.clientBufferNominal() - c.diag.HaveS
	h := math.Min(math.Min(deficit, cfg.MaxHold.Seconds()), room)
	if h < 1 {
		return
	}
	c.holding = true
	c.holdStart = now
	c.holdUntil = now.Add(time.Duration(h * float64(time.Second)))
	c.addEvent("tune", fmt.Sprintf("плеер встал, защиты не хватает (%.0f из %.0f с) — авто-пауза %.0f с: одна пауза сейчас вместо фризов на следующих провалах",
		c.diag.HaveS, want, h))
}

func (c *channel) clientActive(now time.Time) bool {
	return time.Duration(now.UnixNano()-c.lastAccess.Load()) < 15*time.Second
}

// holdWait blocks a segment response while a hold is active (trickle keeps the
// connection alive). Returns when the hold is over.
func (c *channel) holdUntilTime() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.holdUntil, c.holding && time.Now().Before(c.holdUntil)
}

// tick: estimated player model, busy accounting, samples, analysis, auto-tuning.
func (c *channel) tick(now time.Time) {
	dt := now.Sub(c.lastTick).Seconds()
	c.lastTick = now
	if dt <= 0 || dt > 5 {
		return
	}
	if a, _ := c.activeDownloads(); a > 0 {
		c.busy += dt
	}
	if c.holding && !now.Before(c.holdUntil) {
		c.holding = false
		c.holdEnd = now
		held := now.Sub(c.holdStart).Seconds()
		c.stats.Holds++
		c.stats.HoldS += held
		c.addEvent("tune", fmt.Sprintf("авто-пауза окончена (%.0f с): защита выросла примерно до %.0f с", held, c.diag.HaveS+held))
	}
	if c.clStarted && c.clientActive(now) {
		if c.clPlaying {
			c.clBuf -= dt
			if c.clBuf <= 0 {
				c.clBuf = 0
				c.clPlaying = false
				c.clFrozeAt = now
				c.stats.Freezes++
			}
		} else if !c.holding {
			need := 2.5
			if c.stats.Freezes > 0 {
				need = 5 // ExoPlayer: bufferForPlaybackAfterRebuffer
			}
			if c.clBuf >= math.Min(need, c.clientBufferNominal()) {
				if !c.clFrozeAt.IsZero() {
					f := now.Sub(c.clFrozeAt).Seconds()
					c.stats.FreezeS += f
					c.addEvent("crit", fmt.Sprintf("фриз у плеера (оценка): %.1f с", f))
				}
				c.clPlaying = true
				c.clFrozeAt = time.Time{}
			}
		}
	}
	if now.Sub(c.lastSamp) >= time.Second {
		c.lastSamp = now
		c.analyze(now)
		c.maybeHold(now)
		c.samples = append(c.samples, sample{
			T: round1(c.rel(now)), Ahead: round1(c.readyAhead()), Prot: round1(c.diag.HaveS),
			Need: round1(c.diag.NeedS), Client: round1(c.clBuf), Busy: c.busy, CDL: c.cdl,
			Stall: c.stallActive, Hold: c.holding, Freeze: c.clStarted && !c.clPlaying && c.clientActive(now) && !c.holding,
		})
		if len(c.samples) > maxSamples {
			c.samples = c.samples[len(c.samples)-maxSamples:]
		}
		c.autoTune(now)
	}
}

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

func (c *channel) noteClientSeg(id int64) {
	now := time.Now()
	if c.clLastSeg >= 0 && id < c.clLastSeg-2 && now.Sub(c.clLastSegT) < 60*time.Second {
		c.stats.ClientRest++
		c.addEvent("warn", "плеер начал поток заново (переподключение или ошибка плеера)")
	}
	if id > c.clLastSeg || id < c.clLastSeg-2 {
		c.clLastSeg = id
	}
	c.clLastSegT = now
}

func (c *channel) render(host string) string {
	var b strings.Builder
	v := max(c.version, 3)
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

func round1(f float64) float64 { return math.Round(f*10) / 10 }
func round2(f float64) float64 { return math.Round(f*100) / 100 }
