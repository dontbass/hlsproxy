package main

import (
	"io"
	"math"
	"os"
	"sort"
)

// tsInfo — результат разбора MPEG-TS сегмента провайдера. Нужен, чтобы отличить
// фризы из-за доставки (лечатся буфером) от дефектов самого потока (не лечатся):
//   - CC-ошибки: потерянные TS-пакеты (артефакты, «замирание» картинки);
//   - дыры DTS внутри сегмента: пропущенные кадры;
//   - скачок DTS на стыке с предыдущим сегментом;
//   - сегмент по таймстемпам короче заявленного EXTINF.
type tsInfo struct {
	OK        bool    // разобран как TS
	HasVideo  bool    //
	Packets   int     //
	SyncErr   int     // потеря синхробайта
	CCErr     int     // разрывы continuity counter (видео+аудио)
	Frames    int     // видео PES
	FirstDTS  int64   // 90 кГц, -1 если нет
	LastDTS   int64   //
	FrameDur  float64 // с, медиана шага DTS
	MaxGap    float64 // с, наибольшая дыра сверх нормального шага кадра
	Backs     int     // DTS шёл назад
	Span      float64 // с, длительность по таймстемпам
	Video     *videoInfo
	FirstPTSA int64 // first audio PTS (90 kHz), -1 if none — anchor for subtitles
}

type tsState struct {
	pmtPID   int
	videoPID int
	audioPID int
	vtype    byte
	audio    string
	tracks   int
	video    *videoInfo // last parsed SPS
}

const tsWrap = int64(1) << 33

func tsDelta(a, b int64) int64 { // b - a with 33-bit wrap
	d := (b - a) % tsWrap
	if d < 0 {
		d += tsWrap
	}
	if d > tsWrap/2 {
		d -= tsWrap
	}
	return d
}

func readTS33(b []byte) int64 {
	return int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
}

func isVideoType(t byte) bool {
	switch t {
	case 0x01, 0x02, 0x10, 0x1B, 0x24, 0x42, 0xEA:
		return true
	}
	return false
}

func isAudioType(t byte) bool {
	switch t {
	case 0x03, 0x04, 0x0F, 0x11, 0x81, 0x87:
		return true
	}
	return false
}

// probeTS scans a segment file. st carries PIDs between segments (PMT may be absent).
func probeTS(path string, st *tsState) tsInfo {
	info := tsInfo{FirstDTS: -1, LastDTS: -1, FirstPTSA: -1}
	f, err := os.Open(path)
	if err != nil {
		return info
	}
	defer f.Close()

	buf := make([]byte, 188*2048)
	var carry []byte
	lastCC := map[int]int{}
	var prevDTS int64 = -1
	var deltas []int64
	var gaps []int64
	var es []byte
	const esLimit = 300 << 10

	handle := func(p []byte) {
		info.Packets++
		pid := int(p[1]&0x1F)<<8 | int(p[2])
		pusi := p[1]&0x40 != 0
		afc := (p[3] >> 4) & 3
		cc := int(p[3] & 0x0F)
		off := 4
		disc := false
		if afc&2 != 0 {
			alen := int(p[4])
			if alen > 0 && 5 < len(p) {
				disc = p[5]&0x80 != 0
			}
			off = 5 + alen
		}
		hasPayload := afc&1 != 0
		if off > 188 {
			return
		}
		if hasPayload && (pid == st.videoPID || pid == st.audioPID) && pid > 0 {
			if prev, ok := lastCC[pid]; ok && !disc {
				if cc != (prev+1)&0xF && cc != prev {
					info.CCErr++
				}
			}
			lastCC[pid] = cc
		}
		if !hasPayload {
			return
		}
		pl := p[off:]
		switch {
		case pid == 0 && pusi && len(pl) > 1:
			sec := pl[1+int(pl[0]):]
			if len(sec) >= 12 && sec[0] == 0x00 {
				slen := int(sec[1]&0x0F)<<8 | int(sec[2])
				end := min(3+slen-4, len(sec))
				for i := 8; i+4 <= end; i += 4 {
					prog := int(sec[i])<<8 | int(sec[i+1])
					if prog != 0 {
						st.pmtPID = int(sec[i+2]&0x1F)<<8 | int(sec[i+3])
						break
					}
				}
			}
		case pid == st.pmtPID && st.pmtPID > 0 && pusi && len(pl) > 1:
			sec := pl[1+int(pl[0]):]
			if len(sec) >= 12 && sec[0] == 0x02 {
				slen := int(sec[1]&0x0F)<<8 | int(sec[2])
				end := min(3+slen-4, len(sec))
				pil := int(sec[10]&0x0F)<<8 | int(sec[11])
				v, a, tracks := 0, 0, 0
				var vt byte
				an := ""
				for i := 12 + pil; i+5 <= end; {
					t := sec[i]
					epid := int(sec[i+1]&0x1F)<<8 | int(sec[i+2])
					esl := int(sec[i+3]&0x0F)<<8 | int(sec[i+4])
					var desc []byte
					if i+5+esl <= len(sec) {
						desc = sec[i+5 : i+5+esl]
					}
					if v == 0 && isVideoType(t) {
						v, vt = epid, t
					}
					if name := audioName(t, desc); name != "" || isAudioType(t) {
						tracks++
						if a == 0 {
							a, an = epid, name
						}
					}
					i += 5 + esl
				}
				if v > 0 {
					st.videoPID, st.vtype = v, vt
				}
				if a > 0 {
					st.audioPID, st.audio, st.tracks = a, an, tracks
				}
			}
		case pid == st.audioPID && st.audioPID > 0 && pusi && info.FirstPTSA < 0 && len(pl) >= 14 && pl[0] == 0 && pl[1] == 0 && pl[2] == 1:
			if pl[7]>>6 >= 2 {
				info.FirstPTSA = readTS33(pl[9:14])
			}
		case pid == st.videoPID && st.videoPID > 0 && !pusi:
			if len(es) > 0 && len(es) < esLimit {
				es = append(es, pl...)
			}
		case pid == st.videoPID && st.videoPID > 0 && pusi && len(pl) >= 14 && pl[0] == 0 && pl[1] == 0 && pl[2] == 1:
			if hl := 9 + int(pl[8]); hl < len(pl) && len(es) < esLimit {
				es = append(es, pl[hl:]...)
			}
			flags := pl[7] >> 6
			var ts int64 = -1
			if flags == 3 && len(pl) >= 19 {
				ts = readTS33(pl[14:19])
			} else if flags >= 2 {
				ts = readTS33(pl[9:14])
			}
			if ts < 0 {
				return
			}
			info.HasVideo = true
			info.Frames++
			if info.FirstDTS < 0 {
				info.FirstDTS = ts
			}
			if prevDTS >= 0 {
				d := tsDelta(prevDTS, ts)
				if d < 0 {
					info.Backs++
				} else {
					deltas = append(deltas, d)
				}
			}
			prevDTS = ts
			info.LastDTS = ts
		}
	}

	for {
		n, err := f.Read(buf)
		data := buf[:n]
		if len(carry) > 0 {
			data = append(carry, data...)
			carry = nil
		}
		i := 0
		for i+188 <= len(data) {
			if data[i] != 0x47 {
				info.SyncErr++
				j := i + 1
				for j < len(data) && data[j] != 0x47 {
					j++
				}
				i = j
				continue
			}
			handle(data[i : i+188])
			i += 188
		}
		if i < len(data) {
			carry = append([]byte(nil), data[i:]...)
		}
		if err == io.EOF || n == 0 {
			break
		}
		if err != nil {
			break
		}
	}
	info.OK = info.Packets > 0 && info.SyncErr < info.Packets/10+1
	if len(deltas) > 0 {
		sorted := append([]int64(nil), deltas...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		med := sorted[len(sorted)/2]
		info.FrameDur = float64(med) / 90000
		limit := max(med*4, 45000) // 0.5 s or 4 frames
		for _, d := range deltas {
			if d > limit {
				gaps = append(gaps, d-med)
			}
		}
		for _, g := range gaps {
			if v := float64(g) / 90000; v > info.MaxGap {
				info.MaxGap = v
			}
		}
	}
	if info.FirstDTS >= 0 && info.LastDTS >= 0 {
		info.Span = float64(tsDelta(info.FirstDTS, info.LastDTS))/90000 + info.FrameDur
	}
	if info.HasVideo {
		v := &videoInfo{}
		ok := false
		switch st.vtype {
		case 0x1B:
			ok = parseH264SPS(findSPS(es, false), v)
		case 0x24:
			ok = parseHEVCSPS(findSPS(es, true), v)
		case 0x01, 0x02:
			v.Codec, ok = "MPEG-2", true
		}
		if ok {
			st.video = v
		}
		if st.video != nil {
			cp := *st.video
			if info.FrameDur > 0 {
				cp.FPS = math.Round(100/info.FrameDur) / 100
			}
			cp.Audio, cp.Tracks = st.audio, st.tracks
			info.Video = &cp
		}
	}
	return info
}
