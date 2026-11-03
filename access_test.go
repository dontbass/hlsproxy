package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	if c := do(ext, "/ui").Code; c != 403 {
		t.Fatalf("external without key: %d", c)
	}
	if c := do(ext, "/k/wrong/ui").Code; c != 403 {
		t.Fatalf("external wrong key: %d", c)
	}
	if c := do(ext, "/k/s3cretKey12345/ui").Code; c != 200 {
		t.Fatalf("external with key ui: %d", c)
	}
	if c := do(ext, "/k/s3cretKey12345/playlist?url="+url.QueryEscape("http://192.168.1.1/cgi-bin/luci")).Code; c != 403 {
		t.Fatalf("SSRF to LAN must be denied: %d", c)
	}
	if c := do(ext, "/k/s3cretKey12345/ts?url=x").Code; c != 403 {
		t.Fatalf("legacy /ts external: %d", c)
	}
	// upstream on 127.0.0.1 (private): unknown -> denied for external, allowed after LAN used it
	if c := do(ext, "/k/s3cretKey12345"+list).Code; c != 403 {
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
	for _, c := range channels.list() {
		channels.remove(c)
	}
}
