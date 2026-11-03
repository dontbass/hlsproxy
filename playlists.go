package main

import (
	"bufio"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Сохранённые плейлисты: секции `config playlist 'pl_<id>'` в /etc/config/hlsproxy
// (их ведёт страница LuCI). Короткая ссылка /pl/<id> отдаёт плейлист провайдера
// через прокси. Файл перечитывается при изменении — перезапуск не нужен.

type savedPlaylist struct {
	ID   string
	Name string
	URL  string
}

var saved = struct {
	sync.Mutex
	path    string
	mtime   time.Time
	checked time.Time
	m       map[string]savedPlaylist
}{m: map[string]savedPlaylist{}}

func uciUnquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '\'' && v[len(v)-1] == '\'' || v[0] == '"' && v[len(v)-1] == '"') {
		q := v[0]
		v = v[1 : len(v)-1]
		if q == '\'' {
			v = strings.ReplaceAll(v, `'\''`, `'`)
		}
	}
	return v
}

func parseUCIPlaylists(path string) map[string]savedPlaylist {
	out := map[string]savedPlaylist{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	var cur *savedPlaylist
	flush := func() {
		if cur != nil && cur.URL != "" {
			out[cur.ID] = *cur
		}
		cur = nil
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(l, "config "):
			flush()
			parts := strings.Fields(l)
			if len(parts) >= 3 && parts[1] == "playlist" {
				id := strings.TrimPrefix(uciUnquote(strings.Join(parts[2:], " ")), "pl_")
				cur = &savedPlaylist{ID: id}
			}
		case cur != nil && strings.HasPrefix(l, "option "):
			k, v, _ := strings.Cut(strings.TrimPrefix(l, "option "), " ")
			switch strings.TrimSpace(k) {
			case "url":
				cur.URL = uciUnquote(v)
			case "name":
				cur.Name = uciUnquote(v)
			}
		}
	}
	flush()
	return out
}

func lookupSaved(id string) (savedPlaylist, bool) {
	saved.Lock()
	defer saved.Unlock()
	if saved.path == "" {
		return savedPlaylist{}, false
	}
	if time.Since(saved.checked) > 2*time.Second {
		saved.checked = time.Now()
		if st, err := os.Stat(saved.path); err == nil && !st.ModTime().Equal(saved.mtime) {
			saved.mtime = st.ModTime()
			saved.m = parseUCIPlaylists(saved.path)
		}
	}
	p, ok := saved.m[id]
	return p, ok
}

func allSaved() []savedPlaylist {
	lookupSaved("") // refresh from disk if changed
	saved.Lock()
	defer saved.Unlock()
	out := make([]savedPlaylist, 0, len(saved.m))
	for _, p := range saved.m {
		out = append(out, p)
	}
	return out
}

// /pl/<id>
func savedHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/pl/"), "/")
	p, ok := lookupSaved(id)
	if !ok || id == "" {
		if isExternal(r) {
			noteBad(clientIP(r).String())
		}
		http.NotFound(w, r)
		return
	}
	if p.Name != "" {
		channels.setName(p.URL, p.Name)
	}
	servePlaylist(w, r, p.URL, true)
}
