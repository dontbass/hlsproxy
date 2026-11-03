package main

import "fmt"

// videoInfo: параметры картинки из SPS (H.264 / HEVC) и PMT.
type videoInfo struct {
	Codec      string  `json:"codec"`   // H.264 | HEVC | MPEG-2 | …
	Profile    string  `json:"profile"` // High, Main 10 …
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Interlaced bool    `json:"interlaced,omitempty"`
	BitDepth   int     `json:"bit_depth,omitempty"`
	FPS        float64 `json:"fps,omitempty"`
	Audio      string  `json:"audio,omitempty"`  // AAC, AC-3 …
	Tracks     int     `json:"tracks,omitempty"` // audio tracks
}

func (v videoInfo) Label() string {
	if v.Width == 0 {
		return v.Codec
	}
	s := fmt.Sprintf("%dx%d", v.Width, v.Height)
	if v.Interlaced {
		s += "i"
	} else {
		s += "p"
	}
	return s
}

// Tier: SD / HD / Full HD / 4K by vertical resolution.
func (v videoInfo) Tier() string {
	h := v.Height
	if v.Width > 0 && v.Width*9 < v.Height*16 { // portrait-ish/odd: use width
		h = v.Width * 9 / 16
	}
	switch {
	case h == 0:
		return ""
	case h >= 2000:
		return "4K"
	case h >= 1000:
		return "Full HD"
	case h >= 700:
		return "HD"
	default:
		return "SD"
	}
}

type bitReader struct {
	b   []byte
	pos int // bit position
	err bool
}

func (r *bitReader) u(n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		if r.pos>>3 >= len(r.b) {
			r.err = true
			return 0
		}
		bit := (r.b[r.pos>>3] >> (7 - uint(r.pos&7))) & 1
		v = v<<1 | uint64(bit)
		r.pos++
	}
	return v
}

func (r *bitReader) skip(n int) { r.pos += n }

func (r *bitReader) ue() uint64 {
	zeros := 0
	for r.u(1) == 0 {
		zeros++
		if zeros > 31 || r.err {
			r.err = true
			return 0
		}
	}
	return (uint64(1)<<zeros - 1) + r.u(zeros)
}

func (r *bitReader) se() int64 {
	v := r.ue()
	if v&1 == 1 {
		return int64((v + 1) / 2)
	}
	return -int64(v / 2)
}

// unescapeRBSP removes emulation prevention bytes (00 00 03 -> 00 00).
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// findSPS scans an elementary stream chunk for the first SPS NAL.
func findSPS(es []byte, hevc bool) []byte {
	for i := 0; i+4 < len(es); i++ {
		if es[i] != 0 || es[i+1] != 0 || es[i+2] != 1 {
			continue
		}
		start := i + 3
		var isSPS bool
		if hevc {
			isSPS = (es[start]>>1)&0x3F == 33
		} else {
			isSPS = es[start]&0x1F == 7
		}
		if !isSPS {
			continue
		}
		end := len(es)
		for j := start + 1; j+2 < len(es); j++ {
			if es[j] == 0 && es[j+1] == 0 && (es[j+2] == 1 || es[j+2] == 0) {
				end = j
				break
			}
		}
		return unescapeRBSP(es[start:end])
	}
	return nil
}

func parseH264SPS(nal []byte, v *videoInfo) bool {
	if len(nal) < 4 {
		return false
	}
	r := &bitReader{b: nal[1:]}
	profile := int(r.u(8))
	r.skip(8) // constraint flags
	level := int(r.u(8))
	r.ue() // sps id
	chroma := uint64(1)
	v.BitDepth = 8
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma = r.ue()
		if chroma == 3 {
			r.skip(1)
		}
		v.BitDepth = int(r.ue()) + 8
		r.ue()
		r.skip(1)
		if r.u(1) == 1 { // seq_scaling_matrix_present
			n := 8
			if chroma == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if r.u(1) == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					last, next := int64(8), int64(8)
					for j := 0; j < size; j++ {
						if next != 0 {
							next = (last + r.se() + 256) % 256
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
	}
	r.ue()          // log2_max_frame_num
	switch r.ue() { // pic_order_cnt_type
	case 0:
		r.ue()
	case 1:
		r.skip(1)
		r.se()
		r.se()
		n := r.ue()
		for i := uint64(0); i < n && i < 256; i++ {
			r.se()
		}
	}
	r.ue()    // max_num_ref_frames
	r.skip(1) // gaps
	wMbs := int(r.ue()) + 1
	hMap := int(r.ue()) + 1
	frameOnly := int(r.u(1))
	if frameOnly == 0 {
		r.skip(1)
	}
	r.skip(1) // direct_8x8
	w := wMbs * 16
	h := (2 - frameOnly) * hMap * 16
	if r.u(1) == 1 { // cropping
		cl, cr, ct, cb := int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
		cx, cy := 1, 2-frameOnly
		switch chroma {
		case 1:
			cx, cy = 2, 2*(2-frameOnly)
		case 2:
			cx, cy = 2, 2-frameOnly
		}
		w -= cx * (cl + cr)
		h -= cy * (ct + cb)
	}
	if r.err || w <= 0 || h <= 0 || w > 8192 || h > 8192 {
		return false
	}
	v.Codec, v.Width, v.Height, v.Interlaced = "H.264", w, h, frameOnly == 0
	names := map[int]string{66: "Baseline", 77: "Main", 88: "Extended", 100: "High", 110: "High 10", 122: "High 4:2:2", 244: "High 4:4:4"}
	v.Profile = names[profile]
	if v.Profile == "" {
		v.Profile = fmt.Sprintf("profile %d", profile)
	}
	v.Profile += fmt.Sprintf(" @L%.1f", float64(level)/10)
	return true
}

func parseHEVCSPS(nal []byte, v *videoInfo) bool {
	if len(nal) < 6 {
		return false
	}
	r := &bitReader{b: nal[2:]} // 2-byte NAL header
	r.skip(4)                   // vps id
	maxSub := int(r.u(3))
	r.skip(1)
	// profile_tier_level
	r.skip(2)
	tier := r.u(1)
	profile := int(r.u(5))
	r.skip(32) // compatibility flags
	progressive := r.u(1)
	interlaced := r.u(1)
	r.skip(2 + 43 + 1)
	level := int(r.u(8))
	subProf := make([]bool, maxSub)
	subLvl := make([]bool, maxSub)
	for i := 0; i < maxSub; i++ {
		subProf[i] = r.u(1) == 1
		subLvl[i] = r.u(1) == 1
	}
	if maxSub > 0 {
		for i := maxSub; i < 8; i++ {
			r.skip(2)
		}
	}
	for i := 0; i < maxSub; i++ {
		if subProf[i] {
			r.skip(88)
		}
		if subLvl[i] {
			r.skip(8)
		}
	}
	r.ue() // sps id
	chroma := r.ue()
	if chroma == 3 {
		r.skip(1)
	}
	w := int(r.ue())
	h := int(r.ue())
	if r.u(1) == 1 { // conformance window
		cl, cr, ct, cb := int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
		sx, sy := 1, 1
		switch chroma {
		case 1:
			sx, sy = 2, 2
		case 2:
			sx = 2
		}
		w -= sx * (cl + cr)
		h -= sy * (ct + cb)
	}
	v.BitDepth = int(r.ue()) + 8
	if r.err || w <= 0 || h <= 0 || w > 8192 || h > 8192 {
		return false
	}
	v.Codec, v.Width, v.Height = "HEVC", w, h
	v.Interlaced = interlaced == 1 && progressive == 0
	switch profile {
	case 1:
		v.Profile = "Main"
	case 2:
		v.Profile = "Main 10"
	case 3:
		v.Profile = "Main Still"
	case 4:
		v.Profile = "RExt"
	default:
		v.Profile = fmt.Sprintf("profile %d", profile)
	}
	t := ""
	if tier == 1 {
		t = " High"
	}
	v.Profile += fmt.Sprintf(" @L%.1f%s", float64(level)/30, t)
	return true
}

func audioName(streamType byte, desc []byte) string {
	switch streamType {
	case 0x0F:
		return "AAC"
	case 0x11:
		return "AAC-LATM"
	case 0x03, 0x04:
		return "MPEG audio"
	case 0x81:
		return "AC-3"
	case 0x87:
		return "E-AC-3"
	case 0x06:
		for i := 0; i+1 < len(desc); {
			tag, l := desc[i], int(desc[i+1])
			switch tag {
			case 0x6A:
				return "AC-3"
			case 0x7A:
				return "E-AC-3"
			case 0x7C:
				return "AAC"
			}
			i += 2 + l
		}
	}
	return ""
}
