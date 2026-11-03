package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Per-channel learned parameters, persisted rarely (channel stop, tuning changes),
// debounced so flash is not hammered.
type profile struct {
	Name        string  `json:"name"`
	Source      string  `json:"source"`
	Delay       int     `json:"delay"`
	Concurrency int     `json:"concurrency"`
	NeedS       float64 `json:"need_s"`
	Verdict     string  `json:"verdict"`
	Sessions    int     `json:"sessions"`
	Manual      bool    `json:"manual,omitempty"`
	Subs        *bool   `json:"subs,omitempty"` // subtitles on/off for this channel (nil = default)
	Updated     string  `json:"updated"`
}

type profileStore struct {
	mu    sync.Mutex
	path  string
	m     map[string]*profile
	dirty bool
}

var profiles = &profileStore{m: map[string]*profile{}}

func (ps *profileStore) load(path string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.path = path
	go ps.flusher()
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, &ps.m); err != nil {
		log.Printf("профили: %v", err)
		ps.m = map[string]*profile{}
	}
}

func (ps *profileStore) get(id string) *profile {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if p := ps.m[id]; p != nil {
		cp := *p
		return &cp
	}
	return nil
}

func (ps *profileStore) all() map[string]profile {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make(map[string]profile, len(ps.m))
	for k, v := range ps.m {
		out[k] = *v
	}
	return out
}

func (ps *profileStore) put(id string, p *profile) {
	ps.mu.Lock()
	cp := *p
	ps.m[id] = &cp
	ps.dirty = true
	ps.mu.Unlock()
}

func (ps *profileStore) del(id string) {
	ps.mu.Lock()
	delete(ps.m, id)
	ps.dirty = true
	ps.mu.Unlock()
}

func (ps *profileStore) flusher() {
	for range time.Tick(30 * time.Second) {
		ps.flush()
	}
}

func (ps *profileStore) flush() {
	ps.mu.Lock()
	if !ps.dirty || ps.path == "" {
		ps.mu.Unlock()
		return
	}
	b, _ := json.MarshalIndent(ps.m, "", " ")
	ps.dirty = false
	path := ps.path
	ps.mu.Unlock()
	os.MkdirAll(filepath.Dir(path), 0755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err == nil {
		os.Rename(tmp, path)
	} else {
		log.Printf("профили: %v", err)
	}
}
