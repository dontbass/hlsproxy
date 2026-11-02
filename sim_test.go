package main

// Симуляция: нестабильный провайдер + плеер в стиле ExoPlayer.
// go test -run TestSim -v      (≈2×70 c реального времени)

import (
	"bufio"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	simSegDur  = 1.0 // сек
	simSegSize = 200 * 1024

	simRunFor = 70 * time.Second
)

var simRate = envInt("SIM_RATE", 800*1024) // байт/с на соединение (~4x realtime)

var simWindow = envInt("SIM_WINDOW", 6) // сегментов в плейлисте провайдера

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return d
}

// Плохие периоды провайдера: [from,to) сек от старта — запросы висят.
var simBad = [][2]float64{{14, 17.5}, {27, 30}, {45, 54}, {60, 62.5}}

type fakeUpstream struct {
	t0  time.Time
	rnd *rand.Rand
	mu  sync.Mutex
}

func (u *fakeUpstream) now() float64 { return time.Since(u.t0).Seconds() }

func init() {
	if v := os.Getenv("SIM_BAD"); v != "" {
		simBad = nil
		for _, p := range strings.Split(v, ",") {
			a, b, _ := strings.Cut(p, "-")
			x, _ := strconv.ParseFloat(a, 64)
			y, _ := strconv.ParseFloat(b, 64)
			simBad = append(simBad, [2]float64{x, y})
		}
	}
}

func (u *fakeUpstream) badUntil() float64 {
	n := u.now()
	for _, b := range simBad {
		if n >= b[0] && n < b[1] {
			return b[1]
		}
	}
	return 0
}

func (u *fakeUpstream) waitGood() {
	for {
		until := u.badUntil()
		if until == 0 {
			return
		}
		time.Sleep(time.Duration((until - u.now()) * float64(time.Second)))
	}
}

func (u *fakeUpstream) roll() float64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.rnd.Float64()
}

func (u *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/live.m3u8":
		u.waitGood()
		live := int64(u.now()/simSegDur) + 100 // newest complete segment
		first := live - int64(simWindow) + 1
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", first)
		for s := first; s <= live; s++ {
			fmt.Fprintf(w, "#EXTINF:%.3f,\nseg/%d.ts?token=abc\n", simSegDur, s)
		}
	case strings.HasPrefix(r.URL.Path, "/seg/"):
		n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/seg/"), ".ts"), 10, 64)
		u.waitGood()
		if u.roll() < 0.05 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		body := simBody(n)
		start := 0
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}
		cut := len(body)
		if u.roll() < 0.10 {
			cut = start + (len(body)-start)/2 // обрыв соединения посередине
		}
		chunk := simRate / 20
		for p := start; p < cut; p += chunk {
			e := min(p+chunk, cut)
			if _, err := w.Write(body[p:e]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
			if until := u.badUntil(); until > 0 { // провал посреди передачи
				time.Sleep(time.Duration((until - u.now()) * float64(time.Second)))
			}
		}
		if cut < len(body) {
			panic(http.ErrAbortHandler)
		}
	default:
		http.NotFound(w, r)
	}
}

func simBody(n int64) []byte {
	b := make([]byte, simSegSize)
	for i := range b {
		b[i] = 0x47
	}
	copy(b[1:], fmt.Sprintf("SEQ%012d", n))
	return b
}

type simResult struct {
	stalls             int
	stallSec           float64
	gaps, corrupt, beh int
	played             float64
}

// simPlayer: ExoPlayer-like client. Starts 3 segments before playlist end,
// needs 2.5 s to start, 1.5 s to resume after rebuffer.
func simPlayer(t *testing.T, plURL string, dur time.Duration) simResult {
	var res simResult
	var bufMu sync.Mutex
	buf := 0.0
	stop := time.Now().Add(dur)
	cl := &http.Client{Timeout: 10 * time.Second}

	type pl struct {
		seq0 int64
		uris []string
		base *url.URL
	}
	fetch := func() (*pl, error) {
		resp, err := cl.Get(plURL)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		p := &pl{base: resp.Request.URL}
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if v, ok := strings.CutPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"); ok {
				p.seq0, _ = strconv.ParseInt(v, 10, 64)
			} else if l != "" && !strings.HasPrefix(l, "#") {
				p.uris = append(p.uris, l)
			}
		}
		return p, nil
	}

	go func() { // downloader
		var next int64 = -1
		var lastContent int64 = -1
		for time.Now().Before(stop) {
			p, err := fetch()
			if err != nil || len(p.uris) == 0 {
				time.Sleep(300 * time.Millisecond)
				continue
			}
			if next < 0 {
				next = p.seq0 + int64(max(0, len(p.uris)-3))
			}
			if next < p.seq0 {
				res.beh++
				next = p.seq0 + int64(max(0, len(p.uris)-3))
			}
			for next < p.seq0+int64(len(p.uris)) && time.Now().Before(stop) {
				bufMu.Lock()
				full := buf > 30
				bufMu.Unlock()
				if full {
					time.Sleep(100 * time.Millisecond)
					continue
				}
				ref, _ := url.Parse(p.uris[next-p.seq0])
				resp, err := cl.Get(p.base.ResolveReference(ref).String())
				if err != nil {
					continue
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != 200 {
					continue // retry same seq
				}
				if len(body) != simSegSize {
					res.corrupt++
				} else {
					c, _ := strconv.ParseInt(string(body[4:16]), 10, 64)
					if lastContent >= 0 && c != lastContent+1 {
						res.gaps++
					}
					lastContent = c
				}
				bufMu.Lock()
				buf += simSegDur
				bufMu.Unlock()
				next++
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	playing := false
	var stallStart time.Time
	started := false
	tick := 50 * time.Millisecond
	for time.Now().Before(stop) {
		time.Sleep(tick)
		bufMu.Lock()
		if playing {
			buf -= tick.Seconds()
			res.played += tick.Seconds()
			if buf <= 0 {
				buf = 0
				playing = false
				res.stalls++
				stallStart = time.Now()
			}
		} else {
			need := 2.5
			if started {
				need = 1.5
			}
			if buf >= need {
				if started {
					res.stallSec += time.Since(stallStart).Seconds()
				}
				playing, started = true, true
			}
		}
		bufMu.Unlock()
	}
	return res
}

func runSim(t *testing.T, mode string) simResult {
	up := &fakeUpstream{t0: time.Now(), rnd: rand.New(rand.NewSource(7))}
	us := httptest.NewServer(up)
	defer us.Close()
	src := us.URL + "/live.m3u8"
	pl := src
	if mode != "direct" {
		ps := httptest.NewServer(newMux())
		defer ps.Close()
		pl = ps.URL + "/proxy?url=" + url.QueryEscape(src)
		defer func() {
			for _, c := range channels.list() {
				if !strings.Contains(c.srcURL, us.URL) {
					continue
				}
				c.mu.Lock()
				sm := c.summary()
				c.mu.Unlock()
				t.Logf("[%s] диагноз: %s (%s) need=%.1f have=%.1f avail=%.1f capX=%.2f rec_delay=%d | est.freezes=%d holds=%d",
					mode, sm.Diag.Verdict, sm.Diag.Title, sm.Diag.NeedS, sm.Diag.HaveS, sm.Diag.AvailS, sm.Diag.CapX,
					sm.Diag.RecDelay, sm.Stats.Freezes, sm.Stats.Holds)
				for _, r := range sm.Diag.Reasons {
					t.Logf("    %s", r)
				}
				channels.remove(c)
			}
		}()
	}
	return simPlayer(t, pl, simRunFor)
}

func TestSim(t *testing.T) {
	if os.Getenv("SIM") == "" {
		t.Skip("SIM=1 для запуска (долго)")
	}
	cfg = config{
		CacheDir: t.TempDir(), Delay: time.Duration(envInt("SIM_DELAY", 6)) * time.Second, MaxDelay: 60 * time.Second,
		StartSegs: 3, Window: 8, Concurrency: 2, Idle: 60 * time.Second, IdleRead: 1500 * time.Millisecond,
		MaxAttempts: 8, MaxBurstSegs: 4, StartWait: 7 * time.Second, MaxHold: 8 * time.Second, Mode: "off",
	}
	profiles.load(t.TempDir() + "/p.json")
	modes := strings.Split(envStr("SIM_MODES", "direct,off,auto"), ",")
	results := map[string]simResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, m := range modes {
		if m == "direct" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := runSim(t, "direct")
				mu.Lock()
				results["direct"] = r
				mu.Unlock()
			}()
		}
	}
	for _, m := range modes {
		if m == "direct" {
			continue
		}
		cfg.Mode = m
		r := runSim(t, m)
		mu.Lock()
		results[m] = r
		mu.Unlock()
	}
	wg.Wait()
	for _, m := range modes {
		r := results[m]
		t.Logf("%-6s: фризов %d (%.1f c), пропусков контента %d, битых %d, выпал из окна %d, показано %.0f c",
			m, r.stalls, r.stallSec, r.gaps, r.corrupt, r.beh, r.played)
	}
	for _, m := range modes {
		if m == "direct" {
			continue
		}
		if r := results[m]; r.corrupt > 0 || r.beh > 0 {
			t.Fatalf("%s: битые сегменты или плеер выпал из окна", m)
		}
	}
	if p, ok := results["auto"]; ok && p.stalls > envInt("SIM_MAX_STALLS", 2) {
		t.Fatalf("авто-режим: слишком много фризов: %d", p.stalls)
	}
}

func envStr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
