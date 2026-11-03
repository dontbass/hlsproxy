package main

import (
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

// DEMO=1 go test -run TestDemo — прокси на :18888 с нестабильным фейковым провайдером
// и симулятором плеера, чтобы посмотреть /ui.
func TestDemo(t *testing.T) {
	if os.Getenv("DEMO") == "" {
		t.Skip()
	}
	cfg = config{
		CacheDir: t.TempDir(), Delay: 4 * time.Second, MaxDelay: 60 * time.Second,
		StartSegs: 3, Window: 8, Concurrency: 2, Idle: 60 * time.Second, IdleRead: 1500 * time.Millisecond,
		MaxAttempts: 8, MaxBurstSegs: 4, StartWait: 7 * time.Second, MaxHold: 8 * time.Second, Mode: "auto", LogDir: "/tmp/demolog",
	}
	profiles.load(t.TempDir() + "/p.json")
	os.RemoveAll(cfg.LogDir)
	logs.init(cfg.LogDir)
	names.load(t.TempDir() + "/names.json")
	if os.Getenv("DEMO_SUBS") != "" {
		asr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"language":"english","segments":[{"start":1.0,"end":4.2,"text":" Welcome back to the evening news."},{"start":5.0,"end":9.5,"text":" Tonight we look at the weather across the country."},{"start":12,"end":16,"text":" Stay with us after the break."}]}`))
		}))
		llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"choices":[{"message":{"content":"{\"lines\":[\"С возвращением в вечерние новости.\",\"Сегодня — о погоде по всей стране.\",\"Оставайтесь с нами после рекламы.\"]}"}}],"usage":{"prompt_tokens":120,"completion_tokens":60}}`))
		}))
		cfg.Subs = subsConfig{Enabled: true, Mode: "auto", Lang: "ru", ASRURL: asr.URL, ASRModel: "whisper-large-v3-turbo", LLMURL: llm.URL, LLMModel: "deepseek-chat", ChunkSec: 20}
		initSubs()
		cfg.Subs.ASRKey, cfg.Subs.LLMKey = "k", "k"
	}
	up := &fakeUpstream{t0: time.Now(), rnd: rand.New(rand.NewSource(3))}
	us := httptest.NewServer(up)
	defer us.Close()
	if n := os.Getenv("DEMO_NAME"); n != "-" {
		channels.setName(us.URL+"/live.m3u8", "Первый UHD")
	}
	go http.ListenAndServe("127.0.0.1:18888", newMux())
	time.Sleep(300 * time.Millisecond)
	simPlayer(t, "http://127.0.0.1:18888/proxy?url="+url.QueryEscape(us.URL+"/live.m3u8"), time.Duration(envInt("DEMO_SECS", 80))*time.Second)
}
