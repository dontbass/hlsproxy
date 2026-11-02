package main

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// diagnosis: можно ли погасить провалы буфером и какими параметрами.
//
//	NeedS  — какая защита (с) нужна, чтобы ни один сегмент за последние 10 мин не опоздал:
//	         разброс готовности сегментов max(q) − min(q).
//	HaveS  — текущая защита: насколько край отстаёт от лучшего случая + буфер плеера.
//	AvailS — сколько защиты даёт окно плейлиста провайдера без пауз.
//	MaxS   — потолок max_delay (RAM).
//	CapX   — ёмкость канала: секунд контента скачано за секунду работы загрузчика.
//	         < 1 — провайдер в среднем медленнее битрейта, буфер не поможет.
type diagnosis struct {
	Verdict     string   `json:"verdict"` // learning | ok | fixable | fixable_hold | partial | marginal | bandwidth
	Level       string   `json:"level"`   // good | warning | serious | critical | info
	Title       string   `json:"title"`
	Reasons     []string `json:"reasons"`
	NeedS       float64  `json:"need_s"`
	NeedP95S    float64  `json:"need_p95_s"`
	HaveS       float64  `json:"have_s"`
	AvailS      float64  `json:"avail_s"`
	MaxS        float64  `json:"max_s"`
	CapX        float64  `json:"cap_x"` // -1 = unknown (loader mostly idle = plenty)
	BusyPct     float64  `json:"busy_pct"`
	BitrateMbps float64  `json:"bitrate_mbps"`
	Losses      int      `json:"losses"`
	RecDelay    int      `json:"rec_delay"`
	RecConc     int      `json:"rec_conc"`
	RecRAMMB    int      `json:"rec_ram_mb"`
	WindowS     float64  `json:"window_s"`
}

const analyzeWindow = 600.0 // s

func (c *channel) analyze(now time.Time) {
	nowR := c.rel(now)
	B := c.clientBufferNominal()
	var qs []float64
	var mbps []float64
	losses := 0
	first := nowR
	for i := len(c.recs) - 1; i >= 0; i-- {
		r := c.recs[i]
		if r.T < nowR-analyzeWindow {
			break
		}
		first = r.T
		if r.Failed {
			if r.Gap && c.targetDur > 0 {
				losses += max(1, int(math.Round(r.Dur/c.targetDur)))
			} else {
				losses++
			}
			continue
		}
		if r.Mbps > 0 {
			mbps = append(mbps, r.Mbps)
		}
		if !r.Init {
			qs = append(qs, r.Q)
		}
	}
	d := diagnosis{AvailS: round1(c.provWin), MaxS: cfg.MaxDelay.Seconds(), CapX: -1, Losses: losses,
		RecConc: c.conc, WindowS: round1(nowR - first)}

	qmin := 0.0
	if len(qs) > 0 {
		sort.Float64s(qs)
		qmin = qs[0]
		d.NeedS = round1(qs[len(qs)-1] - qmin)
		d.NeedP95S = round1(qs[int(float64(len(qs)-1)*0.95)] - qmin)
	}
	if c.stallActive { // an ongoing outage already counts
		end := c.edgeEnd + c.targetDur
		if s := c.segByID(c.servedEnd + 1); s != nil {
			end = s.content + s.dur
		}
		d.NeedS = round1(math.Max(d.NeedS, nowR-end-qmin))
	}
	if c.started {
		// player-side part: its estimated buffer (includes lag gained through
		// freezes/holds), never less than the nominal start buffer
		d.HaveS = round1(c.edgeOffset() - qmin + math.Max(c.clBuf, B))
	}
	if len(mbps) > 0 {
		sum := 0.0
		for _, v := range mbps {
			sum += v
		}
		d.BitrateMbps = round1(sum / float64(len(mbps)))
	}

	// capacity over the last ~5 min of samples
	if n := len(c.samples); n > 0 {
		j := n - 1
		for j > 0 && c.samples[j].T > nowR-300 {
			j--
		}
		s0 := c.samples[j]
		dBusy, dCDL, dT := c.busy-s0.Busy, c.cdl-s0.CDL, nowR-s0.T
		if dT > 0 {
			d.BusyPct = round1(math.Min(100, dBusy/dT*100))
		}
		if dBusy >= 15 {
			d.CapX = round2(dCDL / dBusy)
		}
	}

	margin := math.Max(0.25*d.NeedS, c.targetDur)
	want := d.NeedS + margin
	d.RecDelay = int(math.Ceil(math.Max(want-B, c.targetDur)/5) * 5)
	if d.CapX >= 0 && d.CapX < 1.6 {
		d.RecConc = min(c.conc+1, 4)
	}
	if d.BitrateMbps > 0 {
		d.RecRAMMB = int((float64(d.RecDelay) + c.provWin + float64(cfg.Window)*c.targetDur) * d.BitrateMbps / 8)
	}

	live := len(qs)
	switch {
	case live < 6 || nowR < 40:
		d.Verdict, d.Level, d.Title = "learning", "info", "Собираю статистику канала"
		d.Reasons = append(d.Reasons, fmt.Sprintf("Нужно ~1 мин эфира для оценки (сейчас %d сегм.)", live))
	case d.CapX >= 0 && d.CapX < 1.0 && d.BusyPct > 85:
		d.Verdict, d.Level, d.Title = "bandwidth", "critical", "Не исправить буфером: провайдер медленнее потока"
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"За последние минуты контент приходил в ×%.2f от скорости воспроизведения при загрузке %.0f%% времени. Буфер только отложит фриз. Помогут более низкое качество, другой канал или другая линия.",
			d.CapX, d.BusyPct))
	case d.CapX >= 0 && d.CapX < 1.15 && d.BusyPct > 85:
		d.Verdict, d.Level, d.Title = "marginal", "serious", "На грани: скорость провайдера ≈ битрейту"
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"Контент приходит лишь в ×%.2f от скорости воспроизведения, загрузчик занят %.0f%% времени. Любой провал почти не восстанавливается, буфер даст только отсрочку. Надёжнее качество ниже.",
			d.CapX, d.BusyPct))
	case want <= d.HaveS+0.01:
		d.Verdict, d.Level, d.Title = "ok", "good", "Запаса хватает"
		d.Reasons = append(d.Reasons, fmt.Sprintf("Самый длинный провал за %s — %.0f с, защита сейчас %.0f с.", humanWin(d.WindowS), d.NeedS, d.HaveS))
	case want <= d.AvailS+B:
		d.Verdict, d.Level, d.Title = "fixable", "warning", "Исправимо параметром delay"
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"Нужна защита ≥ %.0f с, сейчас %.0f с. Окно провайдера (%.0f с) позволяет начать с отставанием delay=%d с: сработает при следующем включении канала.",
			want, d.HaveS, d.AvailS, d.RecDelay))
	case want <= d.MaxS+B:
		d.Verdict, d.Level, d.Title = "fixable_hold", "serious", "Исправимо, но только паузой"
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"Нужна защита ≥ %.0f с, а окно провайдера даёт лишь %.0f с. Недостающие ≈%.0f с можно набрать только одной паузой (в авто-режиме это происходит само при первом фризе).",
			want, d.AvailS+B, want-(d.AvailS+B)))
	default:
		d.Verdict, d.Level, d.Title = "partial", "critical", "Полностью не исправить"
		d.Reasons = append(d.Reasons, fmt.Sprintf(
			"Провалы до %.0f с длиннее потолка max_delay=%.0f с. Можно поднять max_delay (RAM ≈ %d МБ), иначе часть фризов останется.",
			d.NeedS, d.MaxS, d.RecRAMMB))
	}
	if losses > 0 {
		d.Reasons = append(d.Reasons, fmt.Sprintf("Потеряно %d сегм. (у провайдера или после всех попыток). Эти секунды эфира выпадут при любом буфере.", losses))
	}
	if d.CapX >= 1.15 && d.CapX < 1.3 && d.Verdict != "learning" {
		refill := 20 / (d.CapX - 1)
		d.Reasons = append(d.Reasons, fmt.Sprintf("Запас скорости небольшой (×%.2f): после провала в 20 с запас восстанавливается ≈%.0f с. Частые провалы будут суммироваться.", d.CapX, refill))
	}
	if d.RecConc > c.conc {
		d.Reasons = append(d.Reasons, fmt.Sprintf("Загрузчик почти всегда занят (×%.2f): стоит попробовать %d потока.", d.CapX, d.RecConc))
	}
	c.diag = d
	if d.Verdict != "learning" && d.NeedS > c.sessNeed {
		c.sessNeed = d.NeedS
	}
}

func humanWin(s float64) string {
	if s >= 90 {
		return fmt.Sprintf("%.0f мин", s/60)
	}
	return fmt.Sprintf("%.0f с", s)
}

// autoTune runs once per second; changes are rate-limited.
func (c *channel) autoTune(now time.Time) {
	if cfg.Mode != "auto" || c.manual || c.diag.Verdict == "learning" || now.Sub(c.lastTuneAt) < 20*time.Second {
		return
	}
	_, pending := c.activeDownloads()
	d := c.diag
	switch {
	case d.CapX >= 0 && d.CapX < 1.6 && pending >= 2 && c.conc < 4:
		c.conc++
		c.lastTuneAt = now
		c.addEvent("tune", fmt.Sprintf("загрузчик не успевает (×%.2f, в очереди %d) — потоков: %d", d.CapX, pending, c.conc))
		c.saveProfile(false)
	case (d.CapX < 0 || d.CapX > 3.5) && pending == 0 && c.conc > cfg.Concurrency && d.Verdict == "ok":
		c.conc--
		c.lastTuneAt = now
		c.addEvent("tune", fmt.Sprintf("скорости с избытком — потоков: %d", c.conc))
		c.saveProfile(false)
	}
	rec := math.Min(float64(d.RecDelay), cfg.MaxDelay.Seconds())
	if (d.Verdict == "fixable" || d.Verdict == "fixable_hold" || d.Verdict == "partial") && rec > c.delay+2 {
		c.delay = rec
		c.lastTuneAt = now
		c.addEvent("tune", fmt.Sprintf("профиль канала: delay → %.0f с (при следующем включении; сейчас недостающее добирается паузой при фризе)", c.delay))
		c.saveProfile(false)
	}
}

func (c *channel) saveProfile(final bool) {
	if cfg.Mode != "auto" {
		return
	}
	p := profiles.get(c.id)
	if p == nil {
		p = &profile{}
	}
	p.Name = c.name
	p.Source = shortURL(c.srcURL)
	if p.Manual { // user-pinned: keep parameters, refresh observations only
		p.NeedS = round1(math.Max(p.NeedS*0.9, c.sessNeed))
		p.Verdict = c.diag.Verdict
		if final {
			p.Sessions++
		}
		profiles.put(c.id, p)
		return
	}
	if final && c.diag.Verdict != "learning" {
		// decay towards what this session actually needed
		want := math.Max(float64(c.diag.RecDelay), float64(p.Delay)*0.85)
		c.delay = math.Max(math.Min(want, c.delay), cfg.Delay.Seconds())
		p.Sessions++
	}
	p.Delay = int(math.Ceil(c.delay))
	p.Concurrency = c.conc
	p.NeedS = round1(math.Max(p.NeedS*0.9, c.sessNeed))
	p.Verdict = c.diag.Verdict
	p.Updated = time.Now().Format("2006-01-02 15:04")
	profiles.put(c.id, p)
}
