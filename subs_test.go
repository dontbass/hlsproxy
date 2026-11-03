package main

import (
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// SUBS_TEST=1 SIM_TS_DIR=/tmp/demots2 go test -run TestSubs -v
func TestSubs(t *testing.T) {
	if os.Getenv("SUBS_TEST") == "" || len(simTS) == 0 {
		t.Skip("SUBS_TEST=1 SIM_TS_DIR=<real TS segments>")
	}
	asrCalls, llmCalls := 0, 0
	asr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asrCalls++
		if r.Header.Get("Authorization") != "Bearer asr-key" {
			http.Error(w, "no auth", 401)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		b, _ := io.ReadAll(f)
		if len(b) < 4 || string(b[:4]) != "fLaC" {
			http.Error(w, "not flac", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"language": "english", "segments": []map[string]any{
			{"start": 0.5, "end": 3.0, "text": " Hello world"},
			{"start": 4.0, "end": 5.5, "text": " Thank you."}, // hallucination filter
			{"start": 6.0, "end": 9.0, "text": " This is a test of live subtitles for the proxy"},
		}})
	}))
	defer asr.Close()
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCalls++
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var in struct{ Lines []string }
		json.Unmarshal([]byte(req.Messages[1].Content), &in)
		var out []string
		for _, l := range in.Lines {
			out = append(out, "RU:"+l)
		}
		c, _ := json.Marshal(map[string]any{"lines": out})
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": string(c)}}},
			"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 40},
		})
	}))
	defer llm.Close()

	cfg = config{
		CacheDir: t.TempDir(), Delay: 8 * time.Second, MaxDelay: 60 * time.Second,
		StartSegs: 3, Window: 8, Concurrency: 2, Idle: 60 * time.Second, IdleRead: 3 * time.Second,
		MaxAttempts: 8, MaxBurstSegs: 4, StartWait: 7 * time.Second, MaxHold: 0, Mode: "off",
		Subs: subsConfig{Enabled: true, Default: true, Lang: "ru", ASRURL: asr.URL, ASRModel: "whisper-large-v3-turbo",
			LLMURL: llm.URL, LLMModel: "deepseek-chat", ChunkSec: 6},
	}
	profiles = &profileStore{m: map[string]*profile{}}
	initSubs()
	cfg.Subs.ASRKey, cfg.Subs.LLMKey = "asr-key", "llm-key"
	simBad = nil
	simWindow = 12
	up := httptest.NewServer(&fakeUpstream{t0: time.Now(), rnd: rand.New(rand.NewSource(1))})
	defer up.Close()
	px := httptest.NewServer(newMux())
	defer px.Close()

	src := up.URL + "/live.m3u8"
	pl := px.URL + "/proxy?url=" + url.QueryEscape(src)
	get := func(u string) string {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	master := get(pl)
	if !strings.Contains(master, "TYPE=SUBTITLES") || !strings.Contains(master, "SUBTITLES=\"subs\"") || !strings.Contains(master, "&m=1") {
		t.Fatalf("master with subtitles expected:\n%s", master)
	}
	// keep the channel alive like a player would
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
				get(pl + "&m=1")
			}
		}
	}()
	time.Sleep(30 * time.Second)
	id := chanID(src)
	subsPl := get(px.URL + "/subs/" + id + "/index.m3u8")
	var vtts []string
	for _, l := range strings.Split(subsPl, "\n") {
		if strings.HasSuffix(l, ".vtt") {
			vtts = append(vtts, l)
		}
	}
	if len(vtts) == 0 {
		t.Fatalf("subtitle playlist empty:\n%s", subsPl)
	}
	found := ""
	for _, v := range vtts {
		body := get(v)
		if !strings.HasPrefix(body, "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:") {
			t.Fatalf("bad vtt header: %q", body)
		}
		if strings.Contains(body, "RU:") {
			found = body
		}
		if strings.Contains(body, "Thank you") {
			t.Fatal("hallucination not filtered")
		}
	}
	if found == "" {
		t.Fatalf("no translated cue in %d vtt segments (asr %d, llm %d calls)", len(vtts), asrCalls, llmCalls)
	}
	t.Logf("vtt sample:\n%s", found)
	t.Logf("asr calls %d, llm calls %d", asrCalls, llmCalls)

	// ffprobe must see a subtitle stream in our master
	if _, err := exec.LookPath("ffprobe"); err == nil {
		out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type,codec_name", "-of", "csv=p=0", pl).CombinedOutput()
		if err != nil {
			t.Fatalf("ffprobe cannot open our master: %v\n%s", err, out)
		}
		t.Logf("ffprobe opens the master (its HLS demuxer ignores WebVTT renditions):\n%s", out)
	}
	c := channels.get(id)
	c.mu.Lock()
	so := c.subsSummary()
	c.mu.Unlock()
	t.Logf("subs summary: chunks=%d lang=%s ahead=%.1fs cost=$%.4f recent=%d err=%q", so.Chunks, so.Lang, so.AheadSec, so.CostUSD, len(so.Recent), so.LastErr)
	close(stop)
	for _, c := range channels.list() {
		channels.remove(c)
	}
}
