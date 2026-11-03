package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Оценка потока для человека: «Картинка» (что приходит от провайдера) и
// «Стабильность» (насколько ровно это доходит до ТВ), обе 0–100 и словом.

type factor struct {
	Text    string `json:"text"`
	Penalty int    `json:"penalty"`
}

type quality struct {
	Score       int        `json:"score"` // общая
	Grade       string     `json:"grade"`
	Level       string     `json:"level"`
	Picture     int        `json:"picture"`
	PicGrade    string     `json:"pic_grade"`
	PicLevel    string     `json:"pic_level"`
	Stability   int        `json:"stability"`
	StabGrade   string     `json:"stab_grade"`
	StabLevel   string     `json:"stab_level"`
	Learning    bool       `json:"learning,omitempty"`
	Video       *videoInfo `json:"video,omitempty"`
	Tier        string     `json:"tier"`
	BitrateMbps float64    `json:"bitrate_mbps"`
	PeakMbps    float64    `json:"peak_mbps"`
	BPP         float64    `json:"bpp"`          // bits per pixel per frame
	BitrateNote string     `json:"bitrate_note"` // высокий / нормальный / низкий для разрешения
	PicFactors  []factor   `json:"pic_factors"`
	StabFactors []factor   `json:"stab_factors"`
}

func gradeOf(score int) (string, string) {
	switch {
	case score >= 90:
		return "Отлично", "good"
	case score >= 75:
		return "Хорошо", "good"
	case score >= 55:
		return "Удовлетворительно", "warning"
	case score >= 35:
		return "Плохо", "serious"
	default:
		return "Очень плохо", "critical"
	}
}

const qualityWindow = 1800.0 // s: stability looks at the last 30 min

func (c *channel) computeQuality(d diagnosis, nowR float64) quality {
	q := quality{Video: c.video, BitrateMbps: d.BitrateMbps}
	from := nowR - qualityWindow
	for i := len(c.recs) - 1; i >= 0 && c.recs[i].T >= nowR-analyzeWindow; i-- {
		if m := c.recs[i].Mbps; m > q.PeakMbps {
			q.PeakMbps = m
		}
	}

	// ---- picture
	pic := 50
	if v := c.video; v != nil && v.Width > 0 {
		q.Tier = v.Tier()
		switch q.Tier {
		case "4K":
			pic = 100
		case "Full HD":
			pic = 85
		case "HD":
			pic = 68
		default:
			pic = 42
			q.PicFactors = append(q.PicFactors, factor{"SD-разрешение " + v.Label(), 0})
		}
		if v.FPS > 0 && d.BitrateMbps > 0 {
			q.BPP = round2(d.BitrateMbps * 1e6 / (float64(v.Width*v.Height) * v.FPS))
			// thresholds for H.264; HEVC needs ~45% less, MPEG-2 ~60% more
			k := 1.0
			switch v.Codec {
			case "HEVC":
				k = 0.55
			case "MPEG-2":
				k = 1.6
			}
			switch b := q.BPP / k; {
			case b >= 0.10:
				q.BitrateNote = "высокий для " + q.Tier
			case b >= 0.06:
				q.BitrateNote = "нормальный для " + q.Tier
				pic -= 5
			case b >= 0.035:
				q.BitrateNote = "низкий для " + q.Tier
				pic -= 18
				q.PicFactors = append(q.PicFactors, factor{fmt.Sprintf("битрейт %.1f Мбит/с низкий для %s %s: возможны «кубики» в динамике", d.BitrateMbps, v.Codec, v.Label()), 18})
			default:
				q.BitrateNote = "очень низкий для " + q.Tier
				pic -= 32
				q.PicFactors = append(q.PicFactors, factor{fmt.Sprintf("битрейт %.1f Мбит/с очень низкий для %s %s: заметная потеря деталей", d.BitrateMbps, v.Codec, v.Label()), 32})
			}
		}
		if v.FPS >= 48 && !v.Interlaced {
			pic = min(100, pic+5)
		}
		if v.Interlaced {
			pic -= 5
			q.PicFactors = append(q.PicFactors, factor{"чересстрочная развёртка (i): гребёнка на движении, если ТВ плохо деинтерлейсит", 5})
		}
	} else if c.video != nil {
		q.Tier = c.video.Codec
	} else {
		q.PicFactors = append(q.PicFactors, factor{"параметры картинки ещё не определены", 0})
	}
	q.Picture = max(0, min(100, pic))

	// ---- stability
	if d.Verdict == "learning" {
		q.Learning = true
	}
	stab := 100
	pen := func(p int, text string) {
		if p <= 0 {
			return
		}
		stab -= p
		q.StabFactors = append(q.StabFactors, factor{text, p})
	}
	countEv := func(substr string) int {
		n := 0
		for _, e := range c.events {
			if e.T >= from && strings.Contains(e.Text, substr) {
				n++
			}
		}
		return n
	}
	if n := countEv("фриз у плеера"); n > 0 {
		pen(min(50, 12*n), fmt.Sprintf("фризы у плеера (оценка): %d за 30 мин", n))
	}
	defects, lost := 0, 0
	for i := len(c.recs) - 1; i >= 0 && c.recs[i].T >= from; i-- {
		r := c.recs[i]
		if r.Bad {
			defects++
		}
		if r.Failed {
			if r.Gap && c.targetDur > 0 {
				lost += max(1, int(math.Round(r.Dur/c.targetDur)))
			} else {
				lost++
			}
		}
	}
	if defects > 0 {
		pen(min(30, 6*defects), fmt.Sprintf("дефекты в потоке провайдера: %d сегм.", defects))
	}
	if lost > 0 {
		pen(min(25, 5*lost), fmt.Sprintf("потеряно сегментов: %d", lost))
	}
	if n := countEv("авто-пауза окончена"); n > 0 {
		pen(min(16, 4*n), fmt.Sprintf("авто-паузы для добора запаса: %d", n))
	}
	switch d.Verdict {
	case "fixable":
		pen(12, "запаса меньше нужного (исправимо параметром delay)")
	case "fixable_hold":
		pen(18, "запаса меньше нужного, окно провайдера короткое")
	case "partial":
		pen(30, "провалы провайдера длиннее допустимого запаса")
	case "marginal":
		pen(25, fmt.Sprintf("скорость провайдера на грани битрейта (×%.2f)", d.CapX))
	case "bandwidth":
		pen(45, fmt.Sprintf("провайдер отдаёт медленнее потока (×%.2f)", d.CapX))
	}
	for _, o := range d.Other {
		if strings.HasPrefix(o.Text, "Доставка роутер → ТВ медленнее") {
			pen(15, "медленный Wi-Fi/LAN до ТВ")
		}
	}
	ab := 0
	for i := len(c.cl) - 1; i >= 0 && c.cl[i].T >= from; i-- {
		if c.cl[i].Aborted {
			ab++
		}
	}
	if r := ab + countEv("начал поток заново"); r > 0 {
		pen(min(15, 5*r), fmt.Sprintf("обрывы и перезапуски у плеера: %d", r))
	}
	q.Stability = max(0, min(100, stab))

	q.PicGrade, q.PicLevel = gradeOf(q.Picture)
	q.StabGrade, q.StabLevel = gradeOf(q.Stability)
	// overall: a perfect picture that freezes is not good — stability weighs more,
	// and the worse of the two caps the result
	q.Score = int(math.Round(0.6*float64(q.Stability) + 0.4*float64(q.Picture)))
	q.Score = min(q.Score, min(q.Stability, q.Picture)+20)
	q.Grade, q.Level = gradeOf(q.Score)
	if q.Learning {
		q.StabGrade, q.StabLevel = "оценивается…", "info"
		q.Grade, q.Level = "оценивается…", "info"
	}
	return q
}

// ---------------------------------------------------------------- viewers

type viewerOut struct {
	IP       string  `json:"ip"`
	UA       string  `json:"ua"`
	External bool    `json:"external"`
	Active   bool    `json:"active"`
	MB       float64 `json:"mb"`
	Minutes  float64 `json:"minutes"`
}

func (c *channel) viewerList() []viewerOut {
	out := []viewerOut{}
	for _, v := range c.viewers {
		if time.Since(v.Last) > 10*time.Minute {
			continue
		}
		out = append(out, viewerOut{IP: v.IP, UA: shortUA(v.UA), External: v.External,
			Active: time.Since(v.Last) < 20*time.Second, MB: round1(float64(v.Bytes) / 1e6),
			Minutes: round1(v.Last.Sub(v.First).Minutes())})
	}
	return out
}
