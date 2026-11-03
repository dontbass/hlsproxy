package main

import (
	"context"
	"encoding/json"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Справочник названий каналов: id канала (хеш ссылки) -> название из списка каналов
// провайдера (#EXTINF:…,Название). Хранится на диске, чтобы после перезапуска прокси
// каналы не превращались в адреса — плеер (OTT Navigator) повторно список не запрашивает.

type nameStore struct {
	mu    sync.Mutex
	path  string
	m     map[string]nameEntry
	lists map[string]int64 // channel-list URLs seen (to refresh titles in background)
	dirty bool
}

type nameFile struct {
	Names map[string]nameEntry `json:"names"`
	Lists map[string]int64     `json:"lists"`
}

type nameEntry struct {
	Name string `json:"name"`
	Seen int64  `json:"seen"` // unix, for pruning
}

var names = &nameStore{m: map[string]nameEntry{}, lists: map[string]int64{}}

const maxNames = 20000

func (ns *nameStore) load(path string) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	ns.path = path
	if b, err := os.ReadFile(path); err == nil {
		var f nameFile
		if err := json.Unmarshal(b, &f); err != nil {
			log.Printf("названия: %v", err)
		} else {
			if f.Names != nil {
				ns.m = f.Names
			}
			if f.Lists != nil {
				ns.lists = f.Lists
			}
		}
	}
	go func() {
		for range time.Tick(30 * time.Second) {
			ns.flush()
		}
	}()
	go ns.refresher()
}

func (ns *nameStore) noteList(u string) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	now := time.Now().Unix()
	if now-ns.lists[u] > 3600 {
		ns.dirty = true
	}
	ns.lists[u] = now
	if len(ns.lists) > 50 {
		for k, v := range ns.lists {
			if now-v > 30*86400 {
				delete(ns.lists, k)
			}
		}
	}
}

// refresher: at start and every 6 h re-read channel lists (saved on the router and
// recently used by players) just to learn titles — the player may never re-fetch
// its list after the proxy restarts.
func (ns *nameStore) refresher() {
	time.Sleep(5 * time.Second)
	for {
		urls := map[string]bool{}
		for _, p := range allSaved() {
			urls[p.URL] = true
		}
		ns.mu.Lock()
		now := time.Now().Unix()
		for u, t := range ns.lists {
			if now-t < 7*86400 {
				urls[u] = true
			}
		}
		ns.mu.Unlock()
		learned := 0
		for u := range urls {
			body, final, err := fetchPlaylist(context.Background(), u, cfg.UA)
			if err != nil || detectKind(body) != kindChannels {
				continue
			}
			base, _ := url.Parse(final)
			for cu, n := range channelNames(body, base) {
				if channels.setName(cu, n) {
					learned++
				}
			}
		}
		if learned > 0 {
			log.Printf("названия каналов: обновлено %d", learned)
		}
		time.Sleep(6 * time.Hour)
	}
}

func (ns *nameStore) set(id, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || id == "" {
		return false
	}
	if len([]rune(name)) > 120 {
		name = string([]rune(name)[:120])
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()
	old, ok := ns.m[id]
	now := time.Now().Unix()
	if ok && old.Name == name && now-old.Seen < 86400 {
		return false
	}
	ns.m[id] = nameEntry{Name: name, Seen: now}
	ns.dirty = ns.dirty || !ok || old.Name != name || now-old.Seen >= 86400
	if len(ns.m) > maxNames {
		cut := now - 60*86400
		for k, v := range ns.m {
			if v.Seen < cut {
				delete(ns.m, k)
			}
		}
	}
	return !ok || old.Name != name
}

func (ns *nameStore) get(id string) string {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return ns.m[id].Name
}

func (ns *nameStore) flush() {
	ns.mu.Lock()
	if !ns.dirty || ns.path == "" {
		ns.mu.Unlock()
		return
	}
	b, _ := json.Marshal(nameFile{Names: ns.m, Lists: ns.lists})
	ns.dirty = false
	path := ns.path
	ns.mu.Unlock()
	os.MkdirAll(filepath.Dir(path), 0755)
	writeAtomic(path, b)
}

// fallbackName: "канал 2414" for .../2414/index.m3u8, else host/…/file.
func fallbackName(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return shortURL(u)
	}
	parts := strings.Split(strings.Trim(p.Path, "/"), "/")
	if n := len(parts); n >= 2 {
		file := strings.ToLower(parts[n-1])
		generic := strings.HasPrefix(file, "index") || strings.HasPrefix(file, "playlist") || strings.HasPrefix(file, "mono") ||
			strings.HasPrefix(file, "video") || strings.HasPrefix(file, "tracks") || strings.HasPrefix(file, "chunklist")
		if generic && parts[n-2] != "" {
			return "канал " + parts[n-2]
		}
	}
	return shortURL(u)
}

// looksLikeURL: a fallback name produced from the address, not a real title.
func looksLikeURL(s string) bool {
	return s == "" || strings.Contains(s, "/") || strings.HasPrefix(s, "http") || strings.HasPrefix(s, "канал ")
}
