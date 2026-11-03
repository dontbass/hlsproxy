package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

/*
Авто-субтитры.

  готовые сегменты канала ──(по ~24 с)──► ffmpeg: звук 16 кГц моно FLAC
        ──► Whisper (Groq, whisper-large-v3-turbo): английские реплики с таймкодами
        ──► DeepSeek: перевод реплик на русский (с контекстом предыдущих)
        ──► реплики с MPEG-TS временем ──► WebVTT-дорожка в HLS (X-TIMESTAMP-MAP)

Обработка идёт по сегментам, которые уже скачаны, т.е. впереди края плеера на весь
запас (delay), поэтому субтитры готовы раньше, чем плеер до них доходит.
*/

type subsConfig struct {
	Enabled  bool
	ASRURL   string
	ASRKey   string
	ASRModel string
	LLMURL   string
	LLMKey   string
	LLMModel string
	Lang     string // target language code (ru)
	Proxy    string // http(s)/socks5 proxy for API calls (e.g. via own VPS)
	ChunkSec float64
	Mode     string // auto: only where speech is not in Lang | all | manual
}

type cue struct {
	Start int64   `json:"-"` // 90 kHz MPEG-TS
	End   int64   `json:"-"`
	T     float64 `json:"t"` // s since T0 (for the panel)
	Src   string  `json:"src"`
	Text  string  `json:"text"`
}

type subsState struct {
	on        bool
	detect    bool // auto mode: sampling speech language
	manual    bool // user pinned on/off for this channel
	why       string
	samples   []string
	tries     int
	streak    int // consecutive chunks already in the target language
	nextTry   time.Time
	recheckAt time.Time
	next      int64 // next segment id to transcribe
	ready     map[int64]bool
	cues      []cue
	busy      bool
	lastErr   string
	lastErrAt time.Time
	asrSec    float64
	llmTokIn  int
	llmTokOut int
	chunks    int
	lang      string // detected source language
	lagSec    float64
}

var apiClient = &http.Client{Timeout: 60 * time.Second}

func initSubs() {
	s := &cfg.Subs
	s.ASRKey = strings.TrimSpace(os.Getenv("HLSPROXY_ASR_KEY"))
	s.LLMKey = strings.TrimSpace(os.Getenv("HLSPROXY_LLM_KEY"))
	if s.ChunkSec <= 0 {
		s.ChunkSec = 24
	}
	tr := upstreamTransport.Clone()
	if s.Proxy != "" {
		if u, err := url.Parse(s.Proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	apiClient.Transport = tr
	if s.Enabled {
		ff := "нет ffmpeg"
		if _, err := exec.LookPath("ffmpeg"); err == nil {
			ff = "ffmpeg есть"
		}
		log.Printf("субтитры: распознавание %s, перевод %s → %s, %s", s.ASRModel, s.LLMModel, s.Lang, ff)
	}
}

func subsAvailable() (bool, string) {
	s := cfg.Subs
	switch {
	case !s.Enabled:
		return false, "субтитры выключены в настройках"
	case s.ASRKey == "":
		return false, "не задан ключ распознавания (Groq)"
	case s.LLMKey == "" && s.Lang != "en":
		return false, "не задан ключ перевода (DeepSeek)"
	}
	return true, ""
}

type subsPlanT struct {
	advertise, on, detect, manual bool
	why                           string
}

// subsPlan: what to do with subtitles for a channel when it opens.
func subsPlan(id string) subsPlanT {
	if ok, _ := subsAvailable(); !ok {
		return subsPlanT{}
	}
	p := profiles.get(id)
	if p != nil && p.Subs != nil {
		if *p.Subs {
			return subsPlanT{advertise: true, on: true, manual: true, why: "включены вручную"}
		}
		return subsPlanT{manual: true, why: "выключены вручную"}
	}
	switch cfg.Subs.Mode {
	case "all":
		return subsPlanT{advertise: true, on: true, why: "для всех каналов"}
	case "manual":
		return subsPlanT{why: "включаются кнопкой на канале"}
	}
	// auto: by detected speech language (re-checked weekly)
	if p != nil && p.SpeechLang != "" && time.Now().Unix()-p.LangChecked < 7*86400 {
		if langIs(p.SpeechLang, cfg.Subs.Lang) {
			return subsPlanT{why: "авто: речь " + langRu(p.SpeechLang) + " — не нужны"}
		}
		return subsPlanT{advertise: true, on: true, why: "авто: речь " + langRu(p.SpeechLang)}
	}
	return subsPlanT{advertise: true, detect: true, why: "авто: определяю язык речи…"}
}

// subsWanted: advertise a subtitle rendition in the master playlist.
func subsWanted(id string) bool { return subsPlan(id).advertise }

func langRu(l string) string {
	m := map[string]string{"english": "английская", "en": "английская", "russian": "русская", "ru": "русская",
		"ukrainian": "украинская", "uk": "украинская", "german": "немецкая", "french": "французская", "spanish": "испанская",
		"italian": "итальянская", "turkish": "турецкая", "polish": "польская", "chinese": "китайская", "japanese": "японская",
		"korean": "корейская", "arabic": "арабская", "portuguese": "португальская"}
	if v, ok := m[strings.ToLower(l)]; ok {
		return v
	}
	return l
}

func majority(xs []string) string {
	cnt := map[string]int{}
	best, bn := "", 0
	for _, x := range xs {
		x = strings.ToLower(x)
		cnt[x]++
		if cnt[x] > bn {
			best, bn = x, cnt[x]
		}
	}
	return best
}

func (c *channel) rememberLang(lang string) {
	p := profiles.get(c.id)
	if p == nil {
		p = &profile{Name: c.name, Source: shortURL(c.srcURL)}
	}
	p.SpeechLang, p.LangChecked = strings.ToLower(lang), time.Now().Unix()
	profiles.put(c.id, p)
}

// ---------------------------------------------------------------- worker

func (c *channel) subsLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		c.mu.Lock()
		if st := c.subs; st != nil && !st.on && !st.detect && !st.manual && !st.recheckAt.IsZero() && time.Now().After(st.recheckAt) {
			st.detect, st.recheckAt = true, time.Time{} // second opinion after 10 min
		}
		if c.subs == nil || !(c.subs.on || c.subs.detect) || c.subs.busy || time.Now().Before(c.subs.nextTry) {
			c.mu.Unlock()
			continue
		}
		batch := c.subsBatch()
		if len(batch) == 0 {
			c.mu.Unlock()
			continue
		}
		c.subs.busy = true
		c.mu.Unlock()

		err := c.subsProcess(batch)

		c.mu.Lock()
		c.subs.busy = false
		for _, s := range batch {
			c.subs.ready[s.id] = true
		}
		c.subs.next = batch[len(batch)-1].id + 1
		if err != nil {
			c.subs.lastErr = err.Error()
			c.subs.lastErrAt = time.Now()
			c.addEvent("warn", "субтитры: "+err.Error())
		}
		c.mu.Unlock()
		if err != nil {
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// subsBatch picks consecutive ready segments starting at next. A short batch is
// flushed early when the player edge is about to reach it. Caller holds c.mu.
func (c *channel) subsBatch() []*seg {
	st := c.subs
	if st.next <= c.servedEnd-3 { // fell behind the player: skip ahead
		for id := st.next; id <= c.servedEnd-3; id++ {
			st.ready[id] = true
		}
		st.next = c.servedEnd - 2
	}
	var out []*seg
	total := 0.0
	for s := c.segByID(st.next); s != nil; s = c.segByID(s.id + 1) {
		if s.state == stFailed {
			if len(out) == 0 {
				st.ready[s.id] = true
				st.next = s.id + 1
				continue
			}
			break
		}
		if s.state != stReady || s.ts == nil {
			break
		}
		out = append(out, s)
		total += s.dur
		if total >= cfg.Subs.ChunkSec {
			return out
		}
	}
	if len(out) == 0 {
		return nil
	}
	// flush a partial chunk if the edge is close
	if out[0].id <= c.servedEnd+2 {
		return out
	}
	return nil
}

func (c *channel) subsProcess(batch []*seg) error {
	base := batch[0].ts.FirstPTSA
	if base < 0 {
		base = batch[0].ts.FirstDTS
	}
	if base < 0 {
		return errors.New("нет таймстемпов в сегменте")
	}
	open := func() (io.Reader, func(), error) {
		var rs []io.Reader
		var fs []*os.File
		for _, s := range batch {
			f, err := os.Open(s.path)
			if err != nil {
				for _, x := range fs {
					x.Close()
				}
				return nil, nil, err
			}
			fs = append(fs, f)
			rs = append(rs, f)
		}
		return io.MultiReader(rs...), func() {
			for _, x := range fs {
				x.Close()
			}
		}, nil
	}
	ctx, cancel := context.WithTimeout(c.ctx, 90*time.Second)
	defer cancel()
	rd, closeAll, err := open()
	if err != nil {
		return err
	}
	audio, fname, codec, err := audioForASR(rd)
	closeAll()
	if err != nil {
		// rare codecs (E-AC-3, LATM): ffmpeg, if the router has it
		if _, lerr := exec.LookPath("ffmpeg"); lerr == nil && (codec == "eac3" || codec == "latm") {
			rd, closeAll, err = open()
			if err != nil {
				return err
			}
			audio, err = extractAudio(ctx, rd)
			closeAll()
			fname = "chunk.flac"
		}
		if err != nil {
			return err
		}
	}
	chunkDur := 0.0
	for _, s := range batch {
		chunkDur += s.dur
	}
	t0 := time.Now()
	segs, lang, err := transcribe(ctx, audio, fname)
	if err != nil {
		return fmt.Errorf("распознавание: %w", err)
	}
	c.mu.Lock()
	c.subs.asrSec += math.Max(chunkDur, 10) // Groq bills ≥10 s per request
	c.subs.chunks++
	if lang != "" {
		c.subs.lang = lang
	}
	st := c.subs
	if st.detect {
		if len(segs) == 0 { // no speech in this piece (music, ads, silence)
			st.tries++
			st.nextTry = time.Now().Add(30 * time.Second)
			if st.tries >= 10 {
				st.detect, st.why = false, "авто: речи не обнаружено"
				c.addEvent("info", "субтитры: за 10 попыток речь не найдена — выключены (переоткройте канал для новой проверки)")
			}
			c.mu.Unlock()
			return nil
		}
		st.samples = append(st.samples, lang)
		dec := majority(st.samples)
		st.detect = false
		if len(st.samples) < 2 {
			st.recheckAt = time.Now().Add(10 * time.Minute)
		}
		c.rememberLang(dec)
		if langIs(dec, cfg.Subs.Lang) {
			was := st.on
			st.on, st.why = false, "авто: речь "+langRu(dec)+" — не нужны"
			if was || len(st.samples) == 1 {
				c.addEvent("info", "субтитры: речь "+langRu(dec)+" — не нужны, распознавание остановлено")
			}
			c.mu.Unlock()
			return nil
		}
		if !st.on {
			c.addEvent("tune", "субтитры: речь "+langRu(dec)+" — включаю перевод")
		}
		st.on, st.why = true, "авто: речь "+langRu(dec)
	} else if !st.manual && cfg.Subs.Mode == "auto" && len(segs) > 0 {
		// the programme switched to the target language: stop paying for it
		if langIs(lang, cfg.Subs.Lang) {
			st.streak++
			if st.streak >= 3 {
				st.on, st.why, st.streak = false, "авто: речь "+langRu(lang)+" — не нужны", 0
				st.recheckAt = time.Now().Add(10 * time.Minute)
				c.rememberLang(lang)
				c.addEvent("info", "субтитры: передача перешла на язык субтитров — распознавание остановлено, проверю снова через 10 мин")
			}
		} else {
			st.streak = 0
		}
	}
	var ctxLines []string
	for i := max(0, len(c.subs.cues)-3); i < len(c.subs.cues); i++ {
		ctxLines = append(ctxLines, c.subs.cues[i].Src)
	}
	c.mu.Unlock()

	var src []string
	for _, s := range segs {
		src = append(src, s.Text)
	}
	dst := src
	if len(src) > 0 && cfg.Subs.Lang != "" && !langIs(lang, cfg.Subs.Lang) {
		tr, in, out, err := translate(ctx, src, ctxLines)
		if err != nil {
			return fmt.Errorf("перевод: %w", err)
		}
		dst = tr
		c.mu.Lock()
		c.subs.llmTokIn += in
		c.subs.llmTokOut += out
		c.mu.Unlock()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	baseT := batch[0].content // content timeline of the chunk start
	for i, s := range segs {
		text := strings.TrimSpace(dst[i])
		if text == "" {
			continue
		}
		st := base + int64(s.Start*90000)
		en := base + int64(math.Max(s.End, s.Start+0.8)*90000)
		c.subs.cues = append(c.subs.cues, cue{Start: st % tsWrap, End: en % tsWrap,
			T: round1(baseT + s.Start), Src: strings.TrimSpace(s.Text), Text: wrapCue(text)})
	}
	if len(c.subs.cues) > 400 {
		c.subs.cues = c.subs.cues[len(c.subs.cues)-400:]
	}
	c.subs.lagSec = round1(time.Since(t0).Seconds())
	return nil
}

func langIs(detected, want string) bool {
	m := map[string]string{"russian": "ru", "english": "en", "ukrainian": "uk", "german": "de", "french": "fr", "spanish": "es"}
	d := strings.ToLower(strings.TrimSpace(detected))
	if v, ok := m[d]; ok {
		d = v
	}
	return d == want || strings.HasPrefix(d, want)
}

// wrapCue: at most 2 lines of ~42 chars.
func wrapCue(s string) string {
	r := []rune(s)
	if len(r) <= 42 {
		return s
	}
	mid := len(r) / 2
	best := -1
	for d := 0; d < mid; d++ {
		for _, i := range []int{mid - d, mid + d} {
			if i > 0 && i < len(r) && r[i] == ' ' {
				best = i
				break
			}
		}
		if best >= 0 {
			break
		}
	}
	if best < 0 {
		return s
	}
	return string(r[:best]) + "\n" + string(r[best+1:])
}

// ---------------------------------------------------------------- external tools

func extractAudio(ctx context.Context, ts io.Reader) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "mpegts", "-i", "pipe:0",
		"-vn", "-map", "0:a:0", "-ac", "1", "-ar", "16000", "-c:a", "flac", "-f", "flac", "pipe:1")
	cmd.Stdin = ts
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	if out.Len() < 100 {
		return nil, errors.New("ffmpeg: пустой звук")
	}
	return out.Bytes(), nil
}

type asrSegment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// transcribe: OpenAI-compatible /audio/transcriptions (Groq) with verbose_json.
func transcribe(ctx context.Context, flac []byte, fname string) ([]asrSegment, string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", fname)
	fw.Write(flac)
	mw.WriteField("model", cfg.Subs.ASRModel)
	mw.WriteField("response_format", "verbose_json")
	mw.WriteField("temperature", "0")
	mw.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Subs.ASRURL, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+cfg.Subs.ASRKey)
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, trimErr(b))
	}
	var r struct {
		Language string       `json:"language"`
		Segments []asrSegment `json:"segments"`
		Text     string       `json:"text"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, "", err
	}
	var out []asrSegment
	for _, s := range r.Segments {
		t := strings.TrimSpace(s.Text)
		if t == "" || isHallucination(t) {
			continue
		}
		s.Text = t
		out = append(out, s)
	}
	return out, r.Language, nil
}

// Whisper on silence/music likes to invent these.
func isHallucination(t string) bool {
	l := strings.ToLower(strings.Trim(t, " .!?♪"))
	for _, h := range []string{"thank you", "thanks for watching", "you", "music", "subtitles by", "please subscribe", "bye"} {
		if l == h || strings.HasPrefix(l, "subtitles by") {
			return true
		}
	}
	return l == ""
}

// translate: OpenAI-compatible chat completions (DeepSeek), JSON in/out.
func translate(ctx context.Context, lines, context []string) ([]string, int, int, error) {
	in, _ := json.Marshal(map[string]any{"context": context, "lines": lines})
	langName := map[string]string{"ru": "русский", "uk": "украинский", "en": "английский"}[cfg.Subs.Lang]
	if langName == "" {
		langName = cfg.Subs.Lang
	}
	sys := "Ты переводчик телевизионных субтитров. Переведи каждую строку из \"lines\" на " + langName +
		" язык. \"context\" — предыдущие реплики, их не переводи, они только для связности. " +
		"Пиши кратко и разговорно, как в субтитрах, сохраняй имена. Не объединяй и не дели строки. " +
		"Ответ строго JSON: {\"lines\": [...]} — столько же строк и в том же порядке."
	reqBody, _ := json.Marshal(map[string]any{
		"model": cfg.Subs.LLMModel, "temperature": 0.2,
		"response_format": map[string]string{"type": "json_object"},
		"messages":        []map[string]string{{"role": "system", "content": sys}, {"role": "user", "content": string(in)}},
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Subs.LLMURL, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Subs.LLMKey)
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, 0, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, trimErr(b))
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			In  int `json:"prompt_tokens"`
			Out int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &r); err != nil || len(r.Choices) == 0 {
		return nil, 0, 0, fmt.Errorf("ответ не разобран: %s", trimErr(b))
	}
	content := strings.TrimSpace(r.Choices[0].Message.Content)
	content = strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(content, "```json"), "```"), "```")
	var out struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &out); err != nil {
		return nil, 0, 0, fmt.Errorf("перевод не JSON: %s", trimErr([]byte(content)))
	}
	res := make([]string, len(lines))
	for i := range lines {
		if i < len(out.Lines) && strings.TrimSpace(out.Lines[i]) != "" {
			res[i] = out.Lines[i]
		} else {
			res[i] = lines[i] // keep original rather than lose the line
		}
	}
	return res, r.Usage.In, r.Usage.Out, nil
}

func trimErr(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ---------------------------------------------------------------- HLS side

// subsMaster: master playlist wrapping one media playlist with a subtitle rendition.
func subsMaster(base, mediaURL, id string, bandwidth int) string {
	name := map[string]string{"ru": "Русский (авто)", "en": "English (auto)"}[cfg.Subs.Lang]
	if name == "" {
		name = cfg.Subs.Lang + " (auto)"
	}
	return fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:4\n"+
		"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"%s\",LANGUAGE=\"%s\",DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,URI=\"%s/subs/%s/index.m3u8\"\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=%d,SUBTITLES=\"subs\"\n%s\n",
		name, cfg.Subs.Lang, base, id, max(bandwidth, 500000), mediaURL)
}

// /subs/<chan>/index.m3u8 and /subs/<chan>/<segid>.vtt
func subsHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/subs/"), "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	c := channels.get(parts[0])
	if c == nil {
		http.NotFound(w, r)
		return
	}
	c.touch()
	if parts[1] == "index.m3u8" {
		c.mu.Lock()
		c.advance(time.Now())
		body := c.renderSubsPlaylist(baseURL(r))
		c.mu.Unlock()
		writeM3U(w, body)
		return
	}
	idStr := strings.TrimSuffix(parts[1], ".vtt")
	var id int64
	if _, err := fmt.Sscan(idStr, &id); err != nil {
		http.NotFound(w, r)
		return
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		c.mu.Lock()
		done := c.subs == nil || !(c.subs.on || c.subs.detect) || c.subs.ready[id]
		c.mu.Unlock()
		if done || time.Now().After(deadline) || r.Context().Err() != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	c.mu.Lock()
	body := c.renderVTT(id)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	io.WriteString(w, body)
}

func (c *channel) renderSubsPlaylist(base string) string {
	var b strings.Builder
	td := int(math.Ceil(c.targetDur))
	for _, s := range c.exposed {
		td = max(td, int(math.Ceil(s.dur-0.0005)))
	}
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", td, c.outSeq0)
	if c.discSeq > 0 {
		fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", c.discSeq)
	}
	for _, s := range c.exposed {
		if s.disc {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s/subs/%s/%d.vtt\n", s.dur, base, c.id, s.id)
	}
	return b.String()
}

func vttTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	ms := int64(math.Round(sec * 1000))
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// renderVTT: cues that start inside this segment, timed relative to its first PTS.
func (c *channel) renderVTT(id int64) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	s := c.segByID(id)
	if s == nil || s.ts == nil {
		b.WriteString("\n")
		return b.String()
	}
	segPTS := s.ts.FirstPTSA
	if segPTS < 0 {
		segPTS = s.ts.FirstDTS
	}
	if segPTS < 0 {
		b.WriteString("\n")
		return b.String()
	}
	fmt.Fprintf(&b, "X-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:00:00:00.000\n\n", segPTS)
	if c.subs == nil {
		return b.String()
	}
	segLen := int64(s.dur * 90000)
	type line struct {
		off, end int64
		text     string
	}
	var ls []line
	for _, q := range c.subs.cues {
		off := tsDelta(segPTS, q.Start)
		if off < 0 || off >= segLen {
			continue
		}
		ls = append(ls, line{off, tsDelta(segPTS, q.End), q.Text})
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].off < ls[j].off })
	for i, l := range ls {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, vttTime(float64(l.off)/90000), vttTime(float64(l.end)/90000), l.text)
	}
	return b.String()
}

// ---------------------------------------------------------------- state & panel

type subsOut struct {
	Available bool    `json:"available"`
	Why       string  `json:"why,omitempty"`
	On        bool    `json:"on"`
	Detecting bool    `json:"detecting"`
	Manual    bool    `json:"manual"`
	State     string  `json:"state"`
	Mode      string  `json:"mode"`
	Chunks    int     `json:"chunks"`
	Lang      string  `json:"lang,omitempty"`
	ASRMin    float64 `json:"asr_min"`
	CostUSD   float64 `json:"cost_usd"`
	LagSec    float64 `json:"lag_s"`
	AheadSec  float64 `json:"ahead_s"` // how far subtitles are ready past the player edge
	LastErr   string  `json:"last_error,omitempty"`
	ErrAgoSec int64   `json:"last_error_ago_s,omitempty"`
	Recent    []cue   `json:"recent"`
}

func (c *channel) subsSummary() subsOut {
	ok, why := subsAvailable()
	o := subsOut{Available: ok, Why: why, Recent: []cue{}, Mode: cfg.Subs.Mode}
	st := c.subs
	if st == nil {
		if ok {
			o.State = subsPlan(c.id).why
		}
		return o
	}
	o.Detecting, o.Manual, o.State = st.detect, st.manual, st.why
	o.On, o.Chunks, o.Lang, o.LagSec = st.on, st.chunks, st.lang, st.lagSec
	o.ASRMin = round1(st.asrSec / 60)
	// Groq whisper-large-v3-turbo ≈ $0.04/h; DeepSeek chat ≈ $0.27 / $1.10 per 1M tokens
	o.CostUSD = round2(st.asrSec/3600*0.04 + float64(st.llmTokIn)/1e6*0.27 + float64(st.llmTokOut)/1e6*1.10)
	if st.lastErr != "" {
		o.LastErr = st.lastErr
		o.ErrAgoSec = int64(time.Since(st.lastErrAt).Seconds())
	}
	ahead := 0.0
	for s := c.segByID(c.servedEnd + 1); s != nil && st.ready[s.id]; s = c.segByID(s.id + 1) {
		ahead += s.dur
	}
	o.AheadSec = round1(ahead)
	for i := max(0, len(st.cues)-8); i < len(st.cues); i++ {
		o.Recent = append(o.Recent, st.cues[i])
	}
	return o
}

// startSubs applies a plan to a running channel. Caller holds c.mu.
func (c *channel) startSubs(pl subsPlanT) {
	if c.subs == nil {
		c.subs = &subsState{ready: map[int64]bool{}, next: max(c.servedEnd-2, 0)}
		go c.subsLoop()
	}
	st := c.subs
	wasOn := st.on
	st.on, st.detect, st.manual, st.why = pl.on, pl.detect, pl.manual, pl.why
	st.tries, st.streak, st.nextTry = 0, 0, time.Time{}
	st.next = max(st.next, c.servedEnd-2)
	if pl.on && !wasOn {
		c.addEvent("info", "субтитры включены ("+pl.why+"): реплики появятся через ~30 с; если дорожки нет в плеере — переоткройте канал")
	}
}

// setSubsMode: user choice from the panel: on | off | auto. Caller holds c.mu.
func (c *channel) setSubsMode(mode string) {
	switch mode {
	case "on":
		c.startSubs(subsPlanT{advertise: true, on: true, manual: true, why: "включены вручную"})
	case "off":
		c.startSubs(subsPlanT{manual: true, why: "выключены вручную"})
	default:
		c.startSubs(subsPlan(c.id))
	}
}
