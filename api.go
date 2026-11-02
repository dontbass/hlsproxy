package main

import (
	"embed"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

//go:embed ui/index.html
var uiFS embed.FS

func uiHandler(w http.ResponseWriter, r *http.Request) {
	b, _ := uiFS.ReadFile("ui/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

type chanSummary struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Source      string    `json:"source"`
	UptimeSec   int64     `json:"uptime_s"`
	IdleSec     int64     `json:"idle_s"`
	Started     bool      `json:"started"`
	SegDur      float64   `json:"seg_dur_s"`
	LeadSec     float64   `json:"delay_s"`       // behind provider edge now
	ReadyAhead  float64   `json:"ready_ahead_s"` // downloaded reserve past the edge
	ClientBuf   float64   `json:"client_buf_s"`  // estimated player buffer
	ClientPlay  bool      `json:"client_playing"`
	Holding     bool      `json:"holding"`
	Stall       bool      `json:"stall"`
	Delay       float64   `json:"delay_param_s"`
	Conc        int       `json:"concurrency"`
	Exposed     int       `json:"exposed"`
	Downloading int       `json:"downloading"`
	Pending     int       `json:"pending"`
	CacheMB     float64   `json:"cache_mb"`
	Stats       chanStats `json:"stats"`
	Diag        diagnosis `json:"diag"`
}

func (c *channel) summary() chanSummary {
	now := time.Now()
	st := chanSummary{
		ID: c.id, Name: c.name, Source: shortURL(c.srcURL),
		UptimeSec: int64(now.Sub(c.t0).Seconds()),
		IdleSec:   int64(time.Duration(now.UnixNano() - c.lastAccess.Load()).Seconds()),
		Started:   c.started, SegDur: c.targetDur, Exposed: len(c.exposed),
		LeadSec: round1(c.leadSec()), ReadyAhead: round1(c.readyAhead()),
		ClientBuf: round1(c.clBuf), ClientPlay: c.clPlaying,
		Holding: c.holding, Stall: c.stallActive,
		Delay: c.delay, Conc: c.conc, Stats: c.stats, Diag: c.diag,
	}
	st.Stats.EdgeStallS = round1(st.Stats.EdgeStallS)
	st.Stats.FreezeS = round1(st.Stats.FreezeS)
	st.Stats.HoldS = round1(st.Stats.HoldS)
	var bytes int64
	for _, s := range c.segs {
		switch s.state {
		case stDownloading:
			st.Downloading++
		case stPending:
			if s.id > c.servedEnd {
				st.Pending++
			}
		case stReady:
			bytes += s.size
		}
	}
	st.CacheMB = round1(float64(bytes) / 1e6)
	return st
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache")
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	enc.Encode(v)
}

func apiStatus(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Version   string        `json:"version"`
		Mode      string        `json:"mode"`
		UptimeSec int64         `json:"uptime_s"`
		DelaySec  int           `json:"delay_cfg_s"`
		MaxDelay  int           `json:"max_delay_s"`
		Profiles  int           `json:"profiles"`
		Channels  []chanSummary `json:"channels"`
	}{version, cfg.Mode, int64(time.Since(startedAt).Seconds()), int(cfg.Delay.Seconds()),
		int(cfg.MaxDelay.Seconds()), len(profiles.all()), []chanSummary{}}
	for _, c := range channels.list() {
		c.mu.Lock()
		out.Channels = append(out.Channels, c.summary())
		c.mu.Unlock()
	}
	sort.Slice(out.Channels, func(i, j int) bool { return out.Channels[i].IdleSec < out.Channels[j].IdleSec })
	writeJSON(w, out)
}

// GET  /api/channel/<id>            — подробности: сэмплы, сегменты, события
// POST /api/channel/<id>/apply      — {"delay":90,"concurrency":3}
// POST /api/channel/<id>/forget     — удалить профиль канала
func apiChannel(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/channel/"), "/")
	id, action, _ := strings.Cut(rest, "/")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		return
	}
	c := channels.get(id)

	switch action {
	case "":
		if c == nil {
			http.NotFound(w, r)
			return
		}
		c.mu.Lock()
		recs := c.recs
		if len(recs) > 400 {
			recs = recs[len(recs)-400:]
		}
		out := struct {
			chanSummary
			NowT    float64  `json:"now_t"`
			Samples []sample `json:"samples"`
			Recs    []segRec `json:"segments"`
			Events  []event  `json:"events"`
			Profile *profile `json:"profile"`
		}{c.summary(), round1(c.rel(time.Now())), append([]sample(nil), c.samples...),
			append([]segRec(nil), recs...), append([]event(nil), c.events...), profiles.get(id)}
		c.mu.Unlock()
		writeJSON(w, out)

	case "apply":
		if r.Method != http.MethodPost {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Delay       *float64 `json:"delay"`
			Concurrency *int     `json:"concurrency"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p := profiles.get(id)
		if p == nil {
			p = &profile{}
		}
		if c != nil {
			c.mu.Lock()
			if req.Delay != nil {
				c.delay = math.Min(math.Max(*req.Delay, 0), cfg.MaxDelay.Seconds())
				c.addEvent("tune", "вручную: delay → "+itoa(int(c.delay))+" с (при следующем включении канала)")
			}
			if req.Concurrency != nil {
				c.conc = min(max(*req.Concurrency, 1), 6)
				c.addEvent("tune", "вручную: потоков → "+itoa(c.conc))
			}
			c.manual = true
			p.Name, p.Source = c.name, shortURL(c.srcURL)
			p.Delay, p.Concurrency = int(math.Ceil(c.delay)), c.conc
			c.mu.Unlock()
		} else {
			if req.Delay != nil {
				p.Delay = int(math.Min(*req.Delay, cfg.MaxDelay.Seconds()))
			}
			if req.Concurrency != nil {
				p.Concurrency = min(max(*req.Concurrency, 1), 6)
			}
		}
		p.Manual = true
		p.Updated = time.Now().Format("2006-01-02 15:04")
		profiles.put(id, p)
		profiles.flush()
		writeJSON(w, p)

	case "forget":
		if r.Method != http.MethodPost {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return
		}
		profiles.del(id)
		profiles.flush()
		if c != nil {
			c.mu.Lock()
			c.manual = false
			c.mu.Unlock()
		}
		writeJSON(w, map[string]bool{"ok": true})

	default:
		http.NotFound(w, r)
	}
}

func apiProfiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, profiles.all())
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
