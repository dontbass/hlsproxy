package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Постоянный журнал: events.log (все события всех каналов, ротация 2 МБ) и
// sessions/<старт>_<id>.json — полный снимок сеанса канала (сэмплы, сегменты,
// доставка до ТВ, события, отметки фризов), чтобы разбирать «что было вчера».
type logStore struct {
	mu   sync.Mutex
	dir  string
	f    *os.File
	size int64
}

var logs = &logStore{}

const (
	logRotateBytes = 2 << 20
	keepSessions   = 60
)

func (l *logStore) init(dir string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0755); err != nil {
		log.Printf("журнал: %v", err)
		return
	}
	l.dir = dir
	l.open()
}

func (l *logStore) open() {
	f, err := os.OpenFile(filepath.Join(l.dir, "events.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("журнал: %v", err)
		return
	}
	st, _ := f.Stat()
	l.f = f
	if st != nil {
		l.size = st.Size()
	}
}

func (l *logStore) write(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if l.size > logRotateBytes {
		l.f.Close()
		p := filepath.Join(l.dir, "events.log")
		os.Rename(p, p+".1")
		l.open()
		if l.f == nil {
			return
		}
		l.size = 0
	}
	n, _ := l.f.WriteString(line)
	l.size += int64(n)
}

func (l *logStore) event(c *channel, e event) {
	l.write(fmt.Sprintf("%s  %-5s [%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), e.Kind, c.name, e.Text))
}

type sessionMeta struct {
	File     string  `json:"file"`
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Start    string  `json:"start"`
	End      string  `json:"end"`
	Minutes  float64 `json:"minutes"`
	Live     bool    `json:"live"`
	Verdict  string  `json:"verdict"`
	Level    string  `json:"level"`
	Title    string  `json:"title"`
	Freezes  int64   `json:"freezes"`
	Holds    int64   `json:"holds"`
	Defects  int64   `json:"defects"`
	Marks    int     `json:"marks"`
	Aborted  int64   `json:"aborted"`
	Restarts int64   `json:"restarts"`
	Score    int     `json:"score"`
	Grade    string  `json:"grade"`
	QLevel   string  `json:"qlevel"`
	Picture  string  `json:"picture"`
}

// saveSession snapshots the channel. Caller holds c.mu; the write happens async.
func (c *channel) saveSession(final bool) {
	logs.mu.Lock()
	dir := logs.dir
	logs.mu.Unlock()
	if dir == "" || len(c.samples) < 5 {
		return
	}
	c.lastSave = time.Now()
	d := c.detail(true)
	d.Live = !final
	body, err := json.Marshal(d)
	if err != nil {
		return
	}
	name := c.t0.Format("20060102-150405") + "_" + c.id
	meta := sessionMeta{
		File: name, ID: c.id, Name: c.name, Start: c.t0.Format("2006-01-02 15:04"),
		End: time.Now().Format("2006-01-02 15:04"), Minutes: round1(time.Since(c.t0).Minutes()), Live: !final,
		Verdict: c.diag.Verdict, Level: c.diag.Level, Title: c.diag.Title,
		Freezes: c.stats.Freezes, Holds: c.stats.Holds, Defects: c.stats.Defects, Marks: len(c.marks),
		Aborted: c.stats.ClAborted, Restarts: c.stats.ClientRest,
		Score: c.qual.Score, Grade: c.qual.Grade, QLevel: c.qual.Level,
	}
	if v := c.video; v != nil {
		meta.Picture = strings.TrimSpace(v.Tier() + " " + v.Codec)
	}
	mb, _ := json.Marshal(meta)
	go func() {
		sd := filepath.Join(dir, "sessions")
		writeAtomic(filepath.Join(sd, name+".json"), body)
		writeAtomic(filepath.Join(sd, name+".meta"), mb)
		pruneSessions(sd)
	}()
}

func writeAtomic(path string, b []byte) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err == nil {
		os.Rename(tmp, path)
	}
}

func pruneSessions(sd string) {
	ents, err := os.ReadDir(sd)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".meta") {
			names = append(names, strings.TrimSuffix(e.Name(), ".meta"))
		}
	}
	sort.Strings(names)
	for len(names) > keepSessions {
		os.Remove(filepath.Join(sd, names[0]+".json"))
		os.Remove(filepath.Join(sd, names[0]+".meta"))
		names = names[1:]
	}
}

func sessionsDir() string {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	if logs.dir == "" {
		return ""
	}
	return filepath.Join(logs.dir, "sessions")
}

// GET /api/sessions, GET /api/sessions/<file>
func apiSessions(w http.ResponseWriter, r *http.Request) {
	sd := sessionsDir()
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/sessions"), "/")
	if sd == "" {
		writeJSON(w, []sessionMeta{})
		return
	}
	if name != "" {
		if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile(filepath.Join(sd, name+".json"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.URL.Query().Get("download") != "" {
			w.Header().Set("Content-Disposition", `attachment; filename="hlsproxy_`+name+`.json"`)
		}
		w.Write(b)
		return
	}
	ents, _ := os.ReadDir(sd)
	out := []sessionMeta{}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".meta") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(sd, e.Name()))
		if err != nil {
			continue
		}
		var m sessionMeta
		if json.Unmarshal(b, &m) == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File > out[j].File })
	writeJSON(w, out)
}

// GET /api/log?n=500 — хвост events.log текстом
func apiLog(w http.ResponseWriter, r *http.Request) {
	logs.mu.Lock()
	dir := logs.dir
	logs.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if dir == "" {
		io.WriteString(w, "журнал выключен (log_dir пуст)\n")
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 500
	}
	var data []byte
	for _, p := range []string{"events.log.1", "events.log"} {
		b, _ := os.ReadFile(filepath.Join(dir, p))
		data = append(data, b...)
	}
	ls := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	w.Write(bytes.Join(ls, []byte("\n")))
	io.WriteString(w, "\n")
}
