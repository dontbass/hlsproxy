package main

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

// sysInfo: ресурсы роутера, которые важны прокси (RAM под кэш, нагрузка CPU).
type sysInfo struct {
	MemTotalMB int     `json:"mem_total_mb"`
	MemAvailMB int     `json:"mem_avail_mb"`
	CacheMB    float64 `json:"cache_mb"`     // our segments in RAM
	TmpUsedMB  int     `json:"tmp_used_mb"`  // tmpfs holding the cache
	TmpTotalMB int     `json:"tmp_total_mb"` //
	Load1      float64 `json:"load1"`
	CPUs       int     `json:"cpus"`
	ProxyRSSMB float64 `json:"proxy_rss_mb"`
	UpstreamGB float64 `json:"upstream_gb"` // downloaded from provider since start
	ServedGB   float64 `json:"served_gb"`   // sent to players since start
	Channels   int     `json:"channels"`
	Viewers    int     `json:"viewers"`
	External   int     `json:"external_viewers"`
}

var totals struct {
	upstream, served atomic.Int64 // bytes of channels already stopped
}

func readSys() sysInfo {
	s := sysInfo{CPUs: runtime.NumCPU()}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, _ := strings.Cut(sc.Text(), ":")
			kb, _ := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")))
			switch k {
			case "MemTotal":
				s.MemTotalMB = kb / 1024
			case "MemAvailable":
				s.MemAvailMB = kb / 1024
			}
		}
		f.Close()
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			s.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
				kb, _ := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")))
				s.ProxyRSSMB = round1(float64(kb) / 1024)
			}
		}
	}
	var st syscall.Statfs_t
	if syscall.Statfs(cfg.CacheDir, &st) == nil {
		bs := uint64(st.Bsize)
		s.TmpTotalMB = int(st.Blocks * bs >> 20)
		s.TmpUsedMB = int((st.Blocks - st.Bfree) * bs >> 20)
	}
	up, served := totals.upstream.Load(), totals.served.Load()
	ips := map[string]bool{}
	ext := map[string]bool{}
	for _, c := range channels.list() {
		c.mu.Lock()
		up += c.stats.Bytes
		served += c.served
		for _, s2 := range c.segs {
			if s2.state == stReady {
				s.CacheMB += float64(s2.size) / 1e6
			}
		}
		for _, v := range c.viewerList() {
			if v.Active {
				ips[v.IP] = true
				if v.External {
					ext[v.IP] = true
				}
			}
		}
		c.mu.Unlock()
		s.Channels++
	}
	s.CacheMB = round1(s.CacheMB)
	s.UpstreamGB = round2(float64(up) / 1e9)
	s.ServedGB = round2(float64(served) / 1e9)
	s.Viewers, s.External = len(ips), len(ext)
	return s
}
