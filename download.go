package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Upstream HTTP client. No global Timeout: a 19 MB UHD segment over a slow link
// may legitimately take longer than any fixed limit. Instead we use connect /
// header timeouts plus an idle-read watchdog (no bytes for N seconds = dead).
var upstreamTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	TLSHandshakeTimeout:   6 * time.Second,
	ResponseHeaderTimeout: 10 * time.Second,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConns:          64,
	MaxIdleConnsPerHost:   16,
	DisableCompression:    true,  // byte counts must match Content-Length
	ForceAttemptHTTP2:     false, // parallel segments over separate TCP connections
}

var upstreamClient = &http.Client{Transport: upstreamTransport}

// playlistClient: playlists are small, a hard timeout is fine here.
var playlistClient = &http.Client{Transport: upstreamTransport, Timeout: 10 * time.Second}

type idleReader struct {
	r io.Reader
	t *time.Timer
	d time.Duration
}

func (ir *idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 {
		ir.t.Reset(ir.d)
	}
	return n, err
}

var errBadPayload = errors.New("payload is not media (html/json error page?)")

// fetchToFile downloads uri into dst. Partial data from a previous failed attempt
// is kept in dst+".part" and resumed with a Range request when the server allows.
// Returns bytes transferred during this attempt.
func fetchToFile(parent context.Context, uri, ua, dst string, idle, attemptCap time.Duration) (int64, error) {
	part := dst + ".part"
	var offset int64
	if st, err := os.Stat(part); err == nil {
		offset = st.Size()
	}

	ctx, cancel := context.WithTimeout(parent, attemptCap)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return 0, err
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := upstreamClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var f *os.File
	var expected int64 = -1
	switch {
	case resp.StatusCode == http.StatusPartialContent && offset > 0:
		f, err = os.OpenFile(part, os.O_WRONLY|os.O_APPEND, 0644)
		if resp.ContentLength >= 0 {
			expected = offset + resp.ContentLength
		}
	case resp.StatusCode == http.StatusOK:
		offset = 0
		f, err = os.Create(part)
		expected = resp.ContentLength
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		os.Remove(part)
		return 0, fmt.Errorf("HTTP 416, restarting from zero")
	default:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err != nil {
		return 0, err
	}

	t := time.AfterFunc(idle, cancel)
	defer t.Stop()
	n, cerr := io.Copy(f, &idleReader{r: resp.Body, t: t, d: idle})
	if err := f.Close(); err != nil && cerr == nil {
		cerr = err
	}
	if cerr != nil {
		if ctx.Err() != nil && parent.Err() == nil {
			cerr = fmt.Errorf("stalled/too slow after %d bytes: %w", offset+n, cerr)
		}
		return n, cerr
	}
	total := offset + n
	if expected >= 0 && total != expected {
		return n, fmt.Errorf("short body %d/%d", total, expected)
	}
	if total == 0 {
		os.Remove(part)
		return n, errors.New("empty body")
	}
	if err := checkPayload(part); err != nil {
		os.Remove(part)
		return n, err
	}
	return n, os.Rename(part, dst)
}

func checkPayload(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b := make([]byte, 16)
	k, _ := io.ReadFull(f, b)
	s := strings.TrimSpace(string(b[:k]))
	if strings.HasPrefix(s, "<") || strings.HasPrefix(s, "{") || strings.HasPrefix(s, "#EXTM3U") {
		return errBadPayload
	}
	return nil
}

// fetchPlaylist returns body and the final URL (after redirects).
func fetchPlaylist(ctx context.Context, uri, ua string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return "", uri, err
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := playlistClient.Do(req)
	if err != nil {
		return "", uri, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", resp.Request.URL.String(), fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", resp.Request.URL.String(), err
	}
	return string(b), resp.Request.URL.String(), nil
}
