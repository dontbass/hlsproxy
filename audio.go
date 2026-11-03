package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Извлечение звука из MPEG-TS без ffmpeg — для отправки в Whisper:
//   AAC (ADTS)      -> M4A (MP4 с mp4a/esds)
//   MPEG audio      -> «как есть» (.mp3, слои I–III)
//   AC-3            -> MP4 с ac-3/dac3
// E-AC-3 и AAC-LATM — только через ffmpeg (если установлен).

type audioTrack struct {
	pid   int
	codec string // aac | mpga | ac3 | eac3 | latm
	lang  string
}

// demuxAudio reads TS data, picks the first audio track and returns its
// elementary stream (PES payloads concatenated).
func demuxAudio(r io.Reader) (audioTrack, []byte, error) {
	var tr audioTrack
	pmtPID := -1
	var es bytes.Buffer
	pkt := make([]byte, 188)
	inPES := false
	for {
		if _, err := io.ReadFull(r, pkt); err != nil {
			break
		}
		if pkt[0] != 0x47 {
			// resync: drop one byte at a time until sync
			for pkt[0] != 0x47 {
				copy(pkt, pkt[1:])
				if _, err := io.ReadFull(r, pkt[187:]); err != nil {
					goto done
				}
			}
			if _, err := io.ReadFull(r, pkt[1:]); err != nil {
				break
			}
		}
		pid := int(pkt[1]&0x1F)<<8 | int(pkt[2])
		pusi := pkt[1]&0x40 != 0
		afc := (pkt[3] >> 4) & 3
		if afc&1 == 0 {
			continue
		}
		off := 4
		if afc&2 != 0 {
			off = 5 + int(pkt[4])
		}
		if off >= 188 {
			continue
		}
		pl := pkt[off:]
		switch {
		case pid == 0 && pusi && pmtPID < 0 && len(pl) > 1:
			sec := pl[1+int(pl[0]):]
			if len(sec) >= 12 && sec[0] == 0 {
				slen := int(sec[1]&0x0F)<<8 | int(sec[2])
				for i := 8; i+4 <= min(3+slen-4, len(sec)); i += 4 {
					if prog := int(sec[i])<<8 | int(sec[i+1]); prog != 0 {
						pmtPID = int(sec[i+2]&0x1F)<<8 | int(sec[i+3])
						break
					}
				}
			}
		case pid == pmtPID && pusi && tr.pid == 0 && len(pl) > 1:
			sec := pl[1+int(pl[0]):]
			if len(sec) < 12 || sec[0] != 2 {
				continue
			}
			slen := int(sec[1]&0x0F)<<8 | int(sec[2])
			end := min(3+slen-4, len(sec))
			pil := int(sec[10]&0x0F)<<8 | int(sec[11])
			for i := 12 + pil; i+5 <= end; {
				t := sec[i]
				epid := int(sec[i+1]&0x1F)<<8 | int(sec[i+2])
				esl := int(sec[i+3]&0x0F)<<8 | int(sec[i+4])
				var desc []byte
				if i+5+esl <= len(sec) {
					desc = sec[i+5 : i+5+esl]
				}
				codec := ""
				switch audioName(t, desc) {
				case "AAC":
					codec = "aac"
				case "AAC-LATM":
					codec = "latm"
				case "MPEG audio":
					codec = "mpga"
				case "AC-3":
					codec = "ac3"
				case "E-AC-3":
					codec = "eac3"
				}
				if codec != "" && tr.pid == 0 {
					tr = audioTrack{pid: epid, codec: codec, lang: isoLang(desc)}
				}
				i += 5 + esl
			}
		case tr.pid != 0 && pid == tr.pid:
			if pusi {
				if len(pl) < 9 || pl[0] != 0 || pl[1] != 0 || pl[2] != 1 {
					inPES = false
					continue
				}
				hl := 9 + int(pl[8])
				if hl > len(pl) {
					inPES = false
					continue
				}
				inPES = true
				es.Write(pl[hl:])
			} else if inPES {
				es.Write(pl)
			}
		}
	}
done:
	if tr.pid == 0 {
		return tr, nil, errors.New("в потоке нет звуковой дорожки")
	}
	if es.Len() == 0 {
		return tr, nil, errors.New("звуковая дорожка пустая")
	}
	return tr, es.Bytes(), nil
}

func isoLang(desc []byte) string {
	for i := 0; i+1 < len(desc); {
		tag, l := desc[i], int(desc[i+1])
		if tag == 0x0A && l >= 3 && i+5 <= len(desc) {
			return string(desc[i+2 : i+5])
		}
		i += 2 + l
	}
	return ""
}

// audioForASR: returns file bytes and a file name whose extension tells the API the format.
func audioForASR(ts io.Reader) ([]byte, string, string, error) {
	tr, es, err := demuxAudio(ts)
	if err != nil {
		return nil, "", "", err
	}
	switch tr.codec {
	case "aac":
		b, err := adtsToM4A(es)
		return b, "chunk.m4a", tr.codec, err
	case "mpga":
		if i := bytes.Index(es, []byte{0xFF}); i > 0 {
			es = es[i:]
		}
		return es, "chunk.mp3", tr.codec, nil
	case "ac3":
		b, err := ac3ToMP4(es)
		return b, "chunk.mp4", tr.codec, err
	}
	return nil, "", tr.codec, fmt.Errorf("кодек звука %s поддерживается только через ffmpeg", tr.codec)
}

// ---------------------------------------------------------------- MP4 writer

func mkbox(typ string, parts ...[]byte) []byte {
	n := 8
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 8, n)
	binary.BigEndian.PutUint32(out, uint32(n))
	copy(out[4:], typ)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func zeros(n int) []byte  { return make([]byte, n) }

func fullbox(typ string, ver byte, flags uint32, parts ...[]byte) []byte {
	h := u32(uint32(ver)<<24 | flags&0xFFFFFF)
	return mkbox(typ, append([][]byte{h}, parts...)...)
}

// writeAudioMP4: one track, constant sample duration.
func writeAudioMP4(brand string, sampleEntry []byte, frames [][]byte, timescale, sampleDur uint32) []byte {
	matrix := []byte{0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x40, 0, 0, 0}
	dur := uint32(len(frames)) * sampleDur
	var sizes bytes.Buffer
	for _, f := range frames {
		sizes.Write(u32(uint32(len(f))))
	}
	ftyp := mkbox("ftyp", []byte(brand), u32(0), []byte(brand), []byte("isom"), []byte("mp42"))
	build := func(chunkOff uint32) []byte {
		stbl := mkbox("stbl",
			fullbox("stsd", 0, 0, u32(1), sampleEntry),
			fullbox("stts", 0, 0, u32(1), u32(uint32(len(frames))), u32(sampleDur)),
			fullbox("stsc", 0, 0, u32(1), u32(1), u32(uint32(len(frames))), u32(1)),
			fullbox("stsz", 0, 0, u32(0), u32(uint32(len(frames))), sizes.Bytes()),
			fullbox("stco", 0, 0, u32(1), u32(chunkOff)),
		)
		minf := mkbox("minf",
			fullbox("smhd", 0, 0, u16(0), u16(0)),
			mkbox("dinf", fullbox("dref", 0, 0, u32(1), fullbox("url ", 0, 1))),
			stbl)
		mdia := mkbox("mdia",
			fullbox("mdhd", 0, 0, u32(0), u32(0), u32(timescale), u32(dur), u16(0x55C4), u16(0)),
			fullbox("hdlr", 0, 0, u32(0), []byte("soun"), zeros(12), []byte("SoundHandler\x00")),
			minf)
		tkhd := fullbox("tkhd", 0, 7, u32(0), u32(0), u32(1), u32(0), u32(dur), zeros(8), u16(0), u16(0), u16(0x0100), u16(0), matrix, u32(0), u32(0))
		mvhd := fullbox("mvhd", 0, 0, u32(0), u32(0), u32(timescale), u32(dur), u32(0x00010000), u16(0x0100), zeros(10), matrix, zeros(24), u32(2))
		return mkbox("moov", mvhd, mkbox("trak", tkhd, mdia))
	}
	moov := build(0)
	off := uint32(len(ftyp) + len(moov) + 8)
	moov = build(off)
	var mdat bytes.Buffer
	for _, f := range frames {
		mdat.Write(f)
	}
	out := append([]byte{}, ftyp...)
	out = append(out, moov...)
	out = append(out, mkbox("mdat", mdat.Bytes())...)
	return out
}

func audioSampleEntry(typ string, channels, rate uint32, ext []byte) []byte {
	return mkbox(typ, zeros(6), u16(1), zeros(8), u16(uint16(channels)), u16(16), u16(0), u16(0), u32(rate<<16), ext)
}

// ---------------------------------------------------------------- AAC

var aacRates = []uint32{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

func adtsToM4A(es []byte) ([]byte, error) {
	var frames [][]byte
	var prof, sfi, chc byte
	for i := 0; i+7 <= len(es); {
		if es[i] != 0xFF || es[i+1]&0xF6 != 0xF0 {
			i++
			continue
		}
		protAbsent := es[i+1] & 1
		p := (es[i+2] >> 6) & 3
		sf := (es[i+2] >> 2) & 0x0F
		ch := (es[i+2]&1)<<2 | es[i+3]>>6
		flen := int(es[i+3]&3)<<11 | int(es[i+4])<<3 | int(es[i+5]>>5)
		hl := 7
		if protAbsent == 0 {
			hl = 9
		}
		if int(sf) >= len(aacRates) || flen < hl || i+flen > len(es) {
			i++
			continue
		}
		if len(frames) == 0 {
			prof, sfi, chc = p, sf, ch
		}
		frames = append(frames, es[i+hl:i+flen])
		i += flen
	}
	if len(frames) == 0 {
		return nil, errors.New("AAC: не найдено ни одного кадра ADTS")
	}
	if chc == 0 {
		chc = 2
	}
	asc := u16(uint16(prof+1)<<11 | uint16(sfi)<<7 | uint16(chc)<<3)
	// ES_Descriptor > DecoderConfigDescriptor > DecoderSpecificInfo, SLConfig
	dsi := append([]byte{0x05, byte(len(asc))}, asc...)
	dcd := append([]byte{0x04, byte(13 + len(dsi)), 0x40, 0x15, 0, 0, 0}, u32(0)...)
	dcd = append(dcd, u32(0)...)
	dcd = append(dcd, dsi...)
	esd := append([]byte{0x03, byte(3 + len(dcd) + 3), 0, 1, 0}, dcd...)
	esd = append(esd, 0x06, 0x01, 0x02)
	entry := audioSampleEntry("mp4a", uint32(chc), aacRates[sfi], fullbox("esds", 0, 0, esd))
	return writeAudioMP4("M4A ", entry, frames, aacRates[sfi], 1024), nil
}

// ---------------------------------------------------------------- AC-3

var ac3Rates = []uint32{48000, 44100, 32000}

// words per syncframe, [frmsizecod/2][fscod]
var ac3Words = [19][3]int{
	{64, 69, 96}, {80, 87, 120}, {96, 104, 144}, {112, 121, 168}, {128, 139, 192}, {160, 174, 240},
	{192, 208, 288}, {224, 243, 336}, {256, 278, 384}, {320, 348, 480}, {384, 417, 576}, {448, 487, 672},
	{512, 557, 768}, {640, 696, 960}, {768, 835, 1152}, {896, 975, 1344}, {1024, 1114, 1536},
	{1152, 1253, 1728}, {1280, 1393, 1920},
}

func ac3FrameLen(fscod, frmsizecod byte) int {
	if fscod > 2 || frmsizecod > 37 {
		return 0
	}
	w := ac3Words[frmsizecod/2][fscod]
	if fscod == 1 && frmsizecod&1 == 1 {
		w++
	}
	return w * 2
}

func ac3ToMP4(es []byte) ([]byte, error) {
	var frames [][]byte
	var dac3 []byte
	var rate, chans uint32
	for i := 0; i+8 <= len(es); {
		if es[i] != 0x0B || es[i+1] != 0x77 {
			i++
			continue
		}
		fscod := es[i+4] >> 6
		frmsizecod := es[i+4] & 0x3F
		n := ac3FrameLen(fscod, frmsizecod)
		if n == 0 || i+n > len(es) {
			i++
			continue
		}
		if dac3 == nil {
			r := &bitReader{b: es[i+5 : i+8]}
			bsid := byte(r.u(5))
			bsmod := byte(r.u(3))
			acmod := byte(r.u(3))
			if acmod&1 == 1 && acmod != 1 {
				r.skip(2)
			}
			if acmod&4 != 0 {
				r.skip(2)
			}
			if acmod == 2 {
				r.skip(2)
			}
			lfe := byte(r.u(1))
			v := uint32(fscod)<<22 | uint32(bsid)<<17 | uint32(bsmod)<<14 | uint32(acmod)<<11 | uint32(lfe)<<10 | uint32(frmsizecod>>1)<<5
			dac3 = []byte{byte(v >> 16), byte(v >> 8), byte(v)}
			rate = ac3Rates[fscod]
			chans = []uint32{2, 1, 2, 3, 3, 4, 4, 5}[acmod] + uint32(lfe)
		}
		frames = append(frames, es[i:i+n])
		i += n
	}
	if len(frames) == 0 {
		return nil, errors.New("AC-3: не найдено ни одного кадра")
	}
	entry := audioSampleEntry("ac-3", min(chans, 2), rate, mkbox("dac3", dac3))
	return writeAudioMP4("isom", entry, frames, rate, 1536), nil
}
