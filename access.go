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

// isExternal: the client is not on LAN/VPN. A connection that arrived on the
// router's public address is external even if its source looks private (ISP
// internal routing, hairpin NAT, spoofing).
func isExternal(r *http.Request) bool {
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if ta, ok := la.(*net.TCPAddr); ok && !isLocalIP(ta.IP) {
			return true
		}
	}
	return !isLocalIP(clientIP(r))
}

// ---- brute-force / scanner throttling

type badState struct {
	n     int
	first time.Time
	until time.Time
}

var bad = struct {
	sync.Mutex
	m map[string]*badState
}{m: map[string]*badState{}}

func blocked(ip string) bool {
	bad.Lock()
	defer bad.Unlock()
	b := bad.m[ip]
	return b != nil && time.Now().Before(b.until)
}

func noteBad(ip string) {
	bad.Lock()
	defer bad.Unlock()
	now := time.Now()
	b := bad.m[ip]
	if b == nil || now.Sub(b.first) > 10*time.Minute {
		b = &badState{first: now}
		bad.m[ip] = b
	}
	b.n++
	if b.n >= 20 && now.After(b.until) {
		b.until = now.Add(time.Hour)
		logs.write(now.Format("2006-01-02 15:04:05") + "  warn  [доступ] " + ip + " заблокирован на 1 ч: 20 неудачных попыток\n")
	}
	if len(bad.m) > 10000 {
		for k, v := range bad.m {
			if now.After(v.until) && now.Sub(v.first) > 10*time.Minute {
				delete(bad.m, k)
			}
		}
	}
}

// withAccess strips /k/<key>/ and enforces the key for external clients.
func withAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ext := isExternal(r)
		if ext && blocked(clientIP(r).String()) {
			http.NotFound(w, r)
			return
		}
		prefix := ""
		if rest, ok := strings.CutPrefix(r.URL.Path, "/k/"); ok {
			key, tail, _ := strings.Cut(rest, "/")
			if cfg.AccessKey == "" || subtle.ConstantTimeCompare([]byte(key), []byte(cfg.AccessKey)) != 1 {
				if ext {
					noteBad(clientIP(r).String())
				}
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
		if ext {
			noteBad(clientIP(r).String())
			if cfg.AccessKey == "" {
				denied(w, r, "доступ извне выключен (не задан access_key)")
			} else {
				denied(w, r, "нет ключа")
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
	if isExternal(r) {
		// do not tell scanners what is here
		http.NotFound(w, r)
		return
	}
	http.Error(w, "403 "+why, http.StatusForbidden)
}

// ---- dial-time guard for fetches started by external clients: re-checks every
// resolved address at connect time (covers redirects and DNS rebinding) and dials
// the checked IP itself.

type untrustedKey struct{}

func markUntrusted(ctx context.Context) context.Context {
	return context.WithValue(ctx, untrustedKey{}, true)
}

func guardedDial(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if v, _ := ctx.Value(untrustedKey{}).(bool); !v {
			return d.DialContext(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error = &net.AddrError{Err: "no address", Addr: host}
		for _, ip := range ips {
			if isLocalIP(ip.IP) && !hostKnown(host) {
				lastErr = &net.AddrError{Err: "адрес во внутренней сети запрещён для внешнего клиента", Addr: ip.IP.String()}
				continue
			}
			c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return c, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
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
