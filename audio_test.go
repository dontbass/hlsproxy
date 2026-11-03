package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestAudioForASR(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe")
	}
	for _, c := range []struct{ file, codec, ext string }{
		{"a_aac.ts", "aac", ".m4a"}, {"a_mp2.ts", "mpga", ".mp3"}, {"a_ac3.ts", "ac3", ".mp4"}, {"a_eac3.ts", "eac3", ""},
	} {
		f, err := os.Open("testdata/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		b, name, codec, err := audioForASR(f)
		f.Close()
		if codec != c.codec {
			t.Fatalf("%s: codec %s", c.file, codec)
		}
		if c.ext == "" {
			if err == nil {
				t.Fatalf("%s: expected unsupported", c.file)
			}
			continue
		}
		if err != nil || !strings.HasSuffix(name, c.ext) {
			t.Fatalf("%s: %v %s", c.file, err, name)
		}
		out := t.TempDir() + "/x" + c.ext
		os.WriteFile(out, b, 0644)
		// decodes completely and lasts ~3 s
		res, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration:stream=codec_name,sample_rate", "-of", "default=nw=1", out).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: ffprobe: %v %s", c.file, err, res)
		}
		dec, err := exec.Command("ffmpeg", "-v", "error", "-i", out, "-f", "null", "-").CombinedOutput()
		if err != nil || len(strings.TrimSpace(string(dec))) > 0 {
			t.Fatalf("%s: decode errors: %v %s", c.file, err, dec)
		}
		var dur float64
		for _, l := range strings.Split(string(res), "\n") {
			if v, ok := strings.CutPrefix(l, "duration="); ok {
				dur, _ = strconv.ParseFloat(v, 64)
			}
		}
		if dur < 2.5 || dur > 3.5 {
			t.Fatalf("%s: duration %.2f\n%s", c.file, dur, res)
		}
		t.Logf("%s -> %s (%d КБ): %s", c.file, name, len(b)/1024, strings.ReplaceAll(strings.TrimSpace(string(res)), "\n", " "))
	}
}
