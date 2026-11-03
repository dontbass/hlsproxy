package main

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Доступ извне (белый IP, другой город):
//   - клиенты из LAN/VPN (частные адреса, 100.64/10 — Tailscale) — без ключа, как раньше;
//   - внешние клиенты — только с ключом в пути: http://<белый IP>:8888/k/<ключ>/playlist?url=...
//     Все ссылки, которые прокси отдаёт такому клиенту (каналы, качества, сегменты, панель),
//     уже содержат /k/<ключ>.
//   - внешний клиент не может заставить прокси сходить во внутреннюю сеть (SSRF): частные
//     адреса разрешены, только если такой хост уже встречался в плейлистах провайдера.

type ctxKey int

const prefixKey ctxKey = 1

var cgnat = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func isLocalIP(ip net.IP) bool {
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || cgnat.Contains(ip))
}

func clientIP(r *http.Request) net.IP {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		h = r.RemoteAddr
	}
	return net.ParseIP(h)
}

func isExternal(r *http.Request) bool { return !isLocalIP(clientIP(r)) }

// withAccess strips /k/<key>/ and enforces the key for external clients.
func withAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := ""
		if rest, ok := strings.CutPrefix(r.URL.Path, "/k/"); ok {
			key, tail, _ := strings.Cut(rest, "/")
			if cfg.AccessKey == "" || subtle.ConstantTimeCompare([]byte(key), []byte(cfg.AccessKey)) != 1 {
				denied(w, r, "неверный ключ")
				return
			}
			prefix = "/k/" + key
			r2 := r.Clone(context.WithValue(r.Context(), prefixKey, prefix))
			r2.URL.Path = "/" + tail
			r2.URL.RawPath = ""
			next.ServeHTTP(w, r2)
			return
		}
		if isExternal(r) {
			if cfg.AccessKey == "" {
				denied(w, r, "доступ извне выключен (не задан access_key)")
			} else {
				denied(w, r, "нужен ключ доступа")
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

var deniedLog sync.Map // ip -> time of last log line

func denied(w http.ResponseWriter, r *http.Request, why string) {
	ip := clientIP(r).String()
	if v, ok := deniedLog.Load(ip); !ok || time.Since(v.(time.Time)) > 10*time.Minute {
		deniedLog.Store(ip, time.Now())
		logs.write(time.Now().Format("2006-01-02 15:04:05") + "  warn  [доступ] отказ " + ip + " " + r.URL.Path + ": " + why + "\n")
	}
	http.Error(w, "403 "+why, http.StatusForbidden)
}

// baseURL: scheme+host+key prefix for links handed to this client.
func baseURL(r *http.Request) string {
	p, _ := r.Context().Value(prefixKey).(string)
	return "http://" + r.Host + p
}

// ---- SSRF guard for external clients

var knownHosts = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

func learnHost(u string) {
	p, err := url.Parse(u)
	if err != nil || p.Hostname() == "" {
		return
	}
	knownHosts.Lock()
	knownHosts.m[strings.ToLower(p.Hostname())] = time.Now()
	if len(knownHosts.m) > 2000 {
		for k, t := range knownHosts.m {
			if time.Since(t) > 24*time.Hour {
				delete(knownHosts.m, k)
			}
		}
	}
	knownHosts.Unlock()
}

func hostKnown(h string) bool {
	knownHosts.Lock()
	defer knownHosts.Unlock()
	_, ok := knownHosts.m[strings.ToLower(h)]
	return ok
}

// upstreamAllowed: may this client make the proxy fetch u?
func upstreamAllowed(r *http.Request, u string) (bool, string) {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Hostname() == "" {
		return false, "нужна http(s)-ссылка"
	}
	if !isExternal(r) {
		learnHost(u)
		return true, ""
	}
	h := p.Hostname()
	if hostKnown(h) {
		return true, ""
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, h)
	if err != nil || len(ips) == 0 {
		return false, "хост не найден"
	}
	for _, ip := range ips {
		if isLocalIP(ip.IP) {
			return false, "адрес во внутренней сети"
		}
	}
	return true, ""
}
