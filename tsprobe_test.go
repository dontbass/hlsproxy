package main

import "testing"

func TestProbeTS(t *testing.T) {
	st := &tsState{}
	a := probeTS("testdata/seg0.ts", st)
	if !a.OK || !a.HasVideo || a.CCErr != 0 || a.MaxGap > 0 || a.Span < 3.9 || a.Span > 4.1 {
		t.Fatalf("seg0 clean expected: %+v", a)
	}
	b := probeTS("testdata/seg1.ts", st)
	if b.CCErr != 0 || b.MaxGap > 0 {
		t.Fatalf("seg1 clean expected: %+v", b)
	}
	if j := float64(tsDelta(a.LastDTS, b.FirstDTS))/90000 - a.FrameDur; j > 0.05 || j < -0.05 {
		t.Fatalf("boundary jump %.3f", j)
	}
	st2 := &tsState{}
	probeTS("testdata/seg0.ts", st2)
	c := probeTS("testdata/seg1_bad.ts", st2)
	if c.CCErr == 0 || c.MaxGap < 0.5 || c.Span > 3.5 && c.MaxGap == 0 {
		t.Fatalf("damaged seg expected CC errors and DTS hole: %+v", c)
	}
	t.Logf("clean: %+v\ndamaged: %+v", b, c)
}

func TestProbeVideo(t *testing.T) {
	cases := []struct {
		file, codec string
		w, h        int
		inter       bool
		audio       string
	}{
		{"v_h264.ts", "H.264", 1280, 720, false, "AC-3"},
		{"v_h264i.ts", "H.264", 1920, 1080, false, "AAC"},
		{"v_h264int.ts", "H.264", 1920, 1080, true, ""},
		{"v_hevc.ts", "HEVC", 3840, 2160, false, "AC-3"},
		{"v_hevc1080.ts", "HEVC", 1920, 1080, false, "AAC"},
	}
	for _, c := range cases {
		st := &tsState{}
		i := probeTS("testdata/"+c.file, st)
		v := i.Video
		if v == nil || v.Codec != c.codec || v.Width != c.w || v.Height != c.h || v.Interlaced != c.inter || v.Audio != c.audio {
			t.Fatalf("%s: got %+v", c.file, v)
		}
		t.Logf("%s: %s %s %s fps=%.2f audio=%s tracks=%d tier=%s", c.file, v.Codec, v.Profile, v.Label(), v.FPS, v.Audio, v.Tracks, v.Tier())
	}
}
