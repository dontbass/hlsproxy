package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAccess(t *testing.T) {
	cfg = config{CacheDir: t.TempDir(), Delay: 2 * time.Second, MaxDelay: 30 * time.Second, StartSegs: 1, Window: 4,
		Concurrency: 1, Idle: 60 * time.Second, IdleRead: time.Second, MaxAttempts: 2, MaxBurstSegs: 2,
		StartWait: 3 * time.Second, Mode: "off", AccessKey: "s3cretKey12345"}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/list.m3u":
			fmt.Fprintf(w, "#EXTM3U\n#EXTINF:-1,Канал\nhttp://%s/ch.m3u8\n", r.Host)
		case r.URL.Path == "/ch.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:1,\na.ts\n#EXTINF:1,\nb.ts\n")
		default:
			w.Write([]byte{0x47, 0, 0, 0})
		}
	}))
	defer up.Close()
	routerIP = func(ip net.IP) bool { return false } // test upstream is on loopback
	defer func() { routerIP = isRouterIP }()
	h := newMux()
	do := func(remote, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://203.0.113.5:8888"+path, nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	ext, lan := "8.8.8.8:5555", "192.168.1.50:5555"
	list := "/playlist?url=" + url.QueryEscape(up.URL+"/list.m3u")

	if c := do(ext, "/ui").Code; c != 404 {
		t.Fatalf("external without key: %d", c)
	}
	if c := do(ext, "/k/wrong/ui").Code; c != 404 {
		t.Fatalf("external wrong key: %d", c)
	}
	if c := do(ext, "/k/s3cretKey12345/ui").Code; c != 404 {
		t.Fatalf("panel must be LAN-only by default: %d", c)
	}
	if c := do(ext, "/k/s3cretKey12345/api/status").Code; c != 404 {
		t.Fatalf("api must be LAN-only by default: %d", c)
	}
	if c := do(lan, "/ui").Code; c != 200 {
		t.Fatalf("lan ui: %d", c)
	}
	if c := do(ext, "/k/s3cretKey12345/playlist?url="+url.QueryEscape("http://192.168.1.1/cgi-bin/luci")).Code; c != 404 {
		t.Fatalf("SSRF to LAN must be denied: %d", c)
	}
	if c := do(ext, "/k/s3cretKey12345/ts?url=x").Code; c != 404 {
		t.Fatalf("legacy /ts external: %d", c)
	}
	// upstream on 127.0.0.1 (private): unknown -> denied for external, allowed after LAN used it
	if c := do(ext, "/k/s3cretKey12345"+list).Code; c != 404 {
		t.Fatalf("private upstream unknown host: %d", c)
	}
	w := do(lan, list)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "http://203.0.113.5:8888/proxy?url=") {
		t.Fatalf("lan list: %d %s", w.Code, w.Body.String())
	}
	w = do(ext, "/k/s3cretKey12345"+list)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "http://203.0.113.5:8888/k/s3cretKey12345/proxy?url=") {
		t.Fatalf("external list links must carry key: %d %s", w.Code, w.Body.String())
	}
	w = do(ext, "/k/s3cretKey12345/proxy?url="+url.QueryEscape(up.URL+"/ch.m3u8"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "http://203.0.113.5:8888/k/s3cretKey12345/seg/") {
		t.Fatalf("external media links must carry key: %d %s", w.Code, w.Body.String())
	}
	// the router itself is never reachable for external clients, even a known host
	routerIP = isRouterIP
	if c := do(ext, "/k/s3cretKey12345/proxy?url="+url.QueryEscape(up.URL+"/other.m3u8")).Code; c != 404 {
		t.Fatalf("router self-address must be refused: %d", c)
	}
	if _, _, err := fetchPlaylist(markUntrusted(context.Background()), up.URL+"/list.m3u", ""); err == nil {
		t.Fatal("untrusted dial to the router itself must fail")
	}
	routerIP = func(ip net.IP) bool { return false }
	// dial guard: untrusted fetch to an unknown name resolving into LAN is refused
	// (covers redirects / DNS rebinding); "localhost" was never learned
	port := up.URL[strings.LastIndex(up.URL, ":")+1:]
	if _, _, err := fetchPlaylist(markUntrusted(context.Background()), "http://localhost:"+port+"/list.m3u", ""); err == nil {
		t.Fatal("untrusted fetch to localhost must fail")
	}
	if _, _, err := fetchPlaylist(context.Background(), "http://localhost:"+port+"/list.m3u", ""); err != nil {
		t.Fatalf("trusted fetch must work: %v", err)
	}
	// brute force: 20 wrong keys -> IP blocked, even with the right key
	for i := 0; i < 20; i++ {
		do("9.9.9.9:1", "/k/guess/ui")
	}
	if c := do("9.9.9.9:1", "/k/s3cretKey12345/ui").Code; c != 404 {
		t.Fatalf("blocked IP must be refused: %d", c)
	}
	// connection to the public address is external even from a private source
	r := httptest.NewRequest("GET", "http://203.0.113.5:8888/ui", nil)
	r.RemoteAddr = "10.0.0.7:4444"
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("203.0.113.5"), Port: 8888}))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != 404 {
		t.Fatalf("private source on public local addr must need key: %d", rw.Code)
	}
	// saved playlists: /pl/<id> from UCI, short links, works for LAN and key clients
	uciPath := t.TempDir() + "/hlsproxy"
	os.WriteFile(uciPath, []byte("config hlsproxy 'main'\n\toption port '8888'\n\nconfig playlist 'pl_a1b2c3'\n\toption name 'Дом'\n\toption url '"+up.URL+"/list.m3u'\n"), 0644)
	saved.Lock()
	saved.path, saved.checked = uciPath, time.Time{}
	saved.Unlock()
	if w := do(lan, "/pl/a1b2c3"); w.Code != 200 || !strings.Contains(w.Body.String(), "/proxy?url=") {
		t.Fatalf("saved playlist lan: %d %s", w.Code, w.Body.String())
	}
	if w := do(ext, "/k/s3cretKey12345/pl/a1b2c3"); w.Code != 200 || !strings.Contains(w.Body.String(), "/k/s3cretKey12345/proxy?url=") {
		t.Fatalf("saved playlist external: %d %s", w.Code, w.Body.String())
	}
	if c := do(ext, "/pl/a1b2c3").Code; c != 404 {
		t.Fatalf("saved playlist external without key: %d", c)
	}
	if c := do(lan, "/pl/nope").Code; c != 404 {
		t.Fatalf("unknown saved id: %d", c)
	}
	for _, c := range channels.list() {
		channels.remove(c)
	}
}
