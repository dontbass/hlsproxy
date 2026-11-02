package main

import (
	"bufio"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type playlistKind int

const (
	kindUnknown  playlistKind = iota
	kindMaster                // HLS master (EXT-X-STREAM-INF)
	kindMedia                 // HLS media (EXT-X-TARGETDURATION)
	kindChannels              // IPTV channel list (EXTINF + URLs, no HLS tags)
)

func detectKind(body string) playlistKind {
	if !strings.HasPrefix(strings.TrimLeft(body, "\uFEFF \t\r\n"), "#EXTM3U") {
		return kindUnknown
	}
	switch {
	case strings.Contains(body, "#EXT-X-STREAM-INF"):
		return kindMaster
	case strings.Contains(body, "#EXT-X-TARGETDURATION"), strings.Contains(body, "#EXT-X-MEDIA-SEQUENCE"):
		return kindMedia
	default:
		return kindChannels
	}
}

func resolveURL(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(u).String()
}

var uriAttrRe = regexp.MustCompile(`URI="([^"]*)"`)

// rewriteURIAttr replaces URI="..." in a tag line using fn(absoluteURI).
func rewriteURIAttr(line string, base *url.URL, fn func(string) string) string {
	return uriAttrRe.ReplaceAllStringFunc(line, func(m string) string {
		sub := uriAttrRe.FindStringSubmatch(m)
		abs := resolveURL(base, sub[1])
		return `URI="` + fn(abs) + `"`
	})
}

var bandwidthRe = regexp.MustCompile(`(?:^|[:,])BANDWIDTH=(\d+)`)

func attrBandwidth(line string) int64 {
	m := bandwidthRe.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	v, _ := strconv.ParseInt(m[1], 10, 64)
	return v
}

func lines(body string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r")
		l = strings.TrimPrefix(l, "\uFEFF")
		out = append(out, l)
	}
	return out
}

// ---------- master playlist ----------

// rewriteMaster rewrites variant/rendition URIs to go through the proxy.
// If allVariants is false, only the highest-BANDWIDTH variant is kept: switching
// variants means a cold start of a new upstream channel, i.e. a guaranteed freeze.
func rewriteMaster(body string, base *url.URL, allVariants bool, wrap func(string) string) string {
	ls := lines(body)
	type variant struct {
		inf string
		uri string
		bw  int64
	}
	var head []string
	var variants []variant
	for i := 0; i < len(ls); i++ {
		l := ls[i]
		switch {
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF"):
			j := i + 1
			for j < len(ls) && (strings.TrimSpace(ls[j]) == "" || strings.HasPrefix(ls[j], "#")) {
				j++
			}
			if j < len(ls) {
				variants = append(variants, variant{inf: l, uri: resolveURL(base, ls[j]), bw: attrBandwidth(l)})
			}
			i = j
		case strings.HasPrefix(l, "#EXT-X-I-FRAME-STREAM-INF"), strings.HasPrefix(l, "#EXT-X-SESSION-KEY"):
			// trick-play/session keys are not needed
		case strings.HasPrefix(l, "#EXT-X-MEDIA"):
			head = append(head, rewriteURIAttr(l, base, wrap))
		case strings.TrimSpace(l) == "":
		default:
			head = append(head, l)
		}
	}
	if !allVariants && len(variants) > 1 {
		best := variants[0]
		for _, v := range variants[1:] {
			if v.bw > best.bw {
				best = v
			}
		}
		variants = []variant{best}
	}
	var b strings.Builder
	for _, l := range head {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	for _, v := range variants {
		b.WriteString(v.inf)
		b.WriteByte('\n')
		b.WriteString(wrap(v.uri))
		b.WriteByte('\n')
	}
	return b.String()
}

// ---------- channel list (IPTV m3u) ----------

func rewriteChannels(body string, base *url.URL, wrap func(string) string) string {
	var b strings.Builder
	for _, l := range lines(body) {
		t := strings.TrimSpace(l)
		if t != "" && !strings.HasPrefix(t, "#") {
			abs := resolveURL(base, t)
			if strings.HasPrefix(abs, "http://") || strings.HasPrefix(abs, "https://") {
				l = wrap(abs)
			}
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// ---------- media playlist ----------

type rawSeg struct {
	uri     string // absolute
	dur     float64
	disc    bool
	keyLine string // effective #EXT-X-KEY line (URI absolute), "" if none
	mapLine string // effective #EXT-X-MAP line (URI absolute), "" if none
}

type mediaPlaylist struct {
	version        int
	targetDuration float64
	mediaSeq       int64
	hasMediaSeq    bool
	endList        bool
	byteRange      bool
	segments       []rawSeg
}

func parseMedia(body string, base *url.URL) *mediaPlaylist {
	p := &mediaPlaylist{version: 3}
	var curKey, curMap string
	var pendDur float64 = -1
	var pendDisc bool
	for _, l := range lines(body) {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") {
			tag, val, _ := strings.Cut(t, ":")
			switch tag {
			case "#EXT-X-VERSION":
				p.version, _ = strconv.Atoi(val)
			case "#EXT-X-TARGETDURATION":
				p.targetDuration, _ = strconv.ParseFloat(val, 64)
			case "#EXT-X-MEDIA-SEQUENCE":
				p.mediaSeq, _ = strconv.ParseInt(val, 10, 64)
				p.hasMediaSeq = true
			case "#EXT-X-ENDLIST":
				p.endList = true
			case "#EXT-X-DISCONTINUITY":
				pendDisc = true
			case "#EXT-X-BYTERANGE":
				p.byteRange = true
			case "#EXT-X-KEY":
				if strings.Contains(val, "METHOD=NONE") {
					curKey = ""
				} else {
					curKey = rewriteURIAttr(t, base, func(s string) string { return s })
				}
			case "#EXT-X-MAP":
				curMap = rewriteURIAttr(t, base, func(s string) string { return s })
			case "#EXTINF":
				d, _, _ := strings.Cut(val, ",")
				pendDur, _ = strconv.ParseFloat(strings.TrimSpace(d), 64)
			}
			continue
		}
		if pendDur < 0 {
			pendDur = p.targetDuration
		}
		p.segments = append(p.segments, rawSeg{
			uri: resolveURL(base, t), dur: pendDur, disc: pendDisc,
			keyLine: curKey, mapLine: curMap,
		})
		pendDur, pendDisc = -1, false
	}
	if p.targetDuration <= 0 {
		for _, s := range p.segments {
			if s.dur > p.targetDuration {
				p.targetDuration = s.dur
			}
		}
	}
	if p.targetDuration <= 0 {
		p.targetDuration = 6
	}
	return p
}

// channelNames maps absolute stream URL -> channel title from "#EXTINF:...,Title".
func channelNames(body string, base *url.URL) map[string]string {
	out := map[string]string{}
	name := ""
	for _, l := range lines(body) {
		t := strings.TrimSpace(l)
		switch {
		case t == "":
		case strings.HasPrefix(t, "#EXTINF"):
			name = ""
			if i := strings.LastIndex(t, ","); i >= 0 {
				name = strings.TrimSpace(t[i+1:])
			}
		case strings.HasPrefix(t, "#"):
		default:
			if name != "" {
				out[resolveURL(base, t)] = name
			}
			name = ""
		}
	}
	return out
}
