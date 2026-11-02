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
		MaxAttempts: 8, MaxBurstSegs: 4, StartWait: 7 * time.Second, MaxHold: 8 * time.Second, Mode: "auto",
	}
	profiles.load(t.TempDir() + "/p.json")
	up := &fakeUpstream{t0: time.Now(), rnd: rand.New(rand.NewSource(3))}
	us := httptest.NewServer(up)
	defer us.Close()
	channels.setName(us.URL+"/live.m3u8", "Первый UHD")
	go http.ListenAndServe("127.0.0.1:18888", newMux())
	time.Sleep(300 * time.Millisecond)
	simPlayer(t, "http://127.0.0.1:18888/proxy?url="+url.QueryEscape(us.URL+"/live.m3u8"), time.Duration(envInt("DEMO_SECS", 80))*time.Second)
}
