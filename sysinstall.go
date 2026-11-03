package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Установка ffmpeg силами самого прокси: процесс, запущенный из LuCI (uhttpd CGI),
// убивается по таймауту запроса — opkg успевает только обновить списки.

const ffmpegLog = "/tmp/hlsproxy-ffmpeg.log"

var ffInstall struct {
	sync.Mutex
	running bool
}

type ffStatus struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Log       string `json:"log"`
}

func ffmpegStatus() ffStatus {
	ffInstall.Lock()
	running := ffInstall.running
	ffInstall.Unlock()
	_, err := exec.LookPath("ffmpeg")
	b, _ := os.ReadFile(ffmpegLog)
	if len(b) > 6000 {
		b = b[len(b)-6000:]
	}
	return ffStatus{Installed: err == nil, Running: running, Log: string(b)}
}

// GET: status + log; POST: start installation (LAN only).
func apiFFmpeg(w http.ResponseWriter, r *http.Request) {
	if isExternal(r) {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
		return
	}
	if r.Method == http.MethodPost {
		ffInstall.Lock()
		if !ffInstall.running {
			ffInstall.running = true
			go runFFmpegInstall()
		}
		ffInstall.Unlock()
	}
	writeJSON(w, ffmpegStatus())
}

func runFFmpegInstall() {
	defer func() {
		ffInstall.Lock()
		ffInstall.running = false
		ffInstall.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	f, err := os.Create(ffmpegLog)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s старт (запущено службой hlsproxy)\n", time.Now().Format("2006-01-02 15:04:05"))
	script := `df -h /overlay 2>/dev/null
if command -v apk >/dev/null 2>&1; then apk update && apk add ffmpeg
else opkg update >/dev/null && echo "списки пакетов обновлены" && opkg install ffmpeg; fi`
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = f, &buf
	err = cmd.Run()
	f.Write(buf.Bytes())
	code := 0
	if err != nil {
		code = -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	_, lerr := exec.LookPath("ffmpeg")
	fmt.Fprintf(f, "\n%s код выхода: %d, ffmpeg %s\n", time.Now().Format("15:04:05"), code,
		map[bool]string{true: "установлен", false: "НЕ установлен"}[lerr == nil])
}
