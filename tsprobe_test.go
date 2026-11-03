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
