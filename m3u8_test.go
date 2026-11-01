package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestRewrite(t *testing.T) {
	wrap := func(u string) string { return "P(" + u + ")" }
	base, _ := url.Parse("http://prov.tv/live/ch1/index.m3u8?token=x")
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720\nmid.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=16000000,RESOLUTION=3840x2160\nhttp://cdn.tv/uhd.m3u8?t=1\n"
	if detectKind(master) != kindMaster {
		t.Fatal("kind master")
	}
	out := rewriteMaster(master, base, false, wrap)
	if !strings.Contains(out, "P(http://cdn.tv/uhd.m3u8?t=1)") || strings.Contains(out, "mid.m3u8") {
		t.Fatal(out)
	}
	list := "#EXTM3U url-tvg=\"x\"\n#EXTINF:-1 tvg-id=\"1\",Канал 1\nhttp://prov.tv/ch1.m3u8\n#EXTINF:-1,Канал 2\nch2/index.m3u8\n"
	if detectKind(list) != kindChannels {
		t.Fatal("kind channels")
	}
	out = rewriteChannels(list, base, wrap)
	if !strings.Contains(out, "P(http://prov.tv/ch1.m3u8)") || !strings.Contains(out, "P(http://prov.tv/live/ch1/ch2/index.m3u8)") || !strings.Contains(out, "Канал 2") {
		t.Fatal(out)
	}
	media := "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:500\n#EXTINF:9.6,\nseg500.ts\n#EXT-X-DISCONTINUITY\n#EXTINF:10.0,\nhttp://cdn/seg501.ts\n"
	p := parseMedia(media, base)
	if len(p.segments) != 2 || p.mediaSeq != 500 || !p.segments[1].disc || p.segments[0].uri != "http://prov.tv/live/ch1/seg500.ts" {
		t.Fatalf("%+v", p)
	}
}
