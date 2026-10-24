package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"strings"
	"time"
)

type ProbeResult struct {
	Kind      string  `json:"kind"`
	Title     string  `json:"title"`
	Thumbnail string  `json:"thumbnail"`
	Duration  float64 `json:"duration"`
	Host      string  `json:"host"`
	Size      int64   `json:"size"`
	URL       string  `json:"url"`
}

func (a *App) Probe(raw string) (ProbeResult, error) {
	u, err := checkURL(raw)
	if err != nil {
		return ProbeResult{}, err
	}
	result := ProbeResult{Kind: "file", Title: safeName(path.Base(u.Path)), Host: u.Hostname(), URL: u.String()}
	if result.Title == "arquivo" || result.Title == "." {
		result.Title = u.Hostname()
	}
	// Link direto pode ser processado sem yt-dlp.
	ext := strings.ToLower(path.Ext(u.Path))
	directExts := map[string]bool{".zip": true, ".rar": true, ".7z": true, ".pdf": true, ".exe": true, ".iso": true, ".msi": true, ".apk": true, ".jpg": true, ".png": true, ".mp3": true, ".wav": true, ".mp4": true, ".mkv": true, ".webm": true}
	if directExts[ext] {
		return probeDirect(result)
	}
	yt := findTool("yt-dlp")
	if yt == "" {
		// Sem o extrator não tratamos uma página HTML como arquivo para baixar.
		return ProbeResult{}, errors.New("o yt-dlp não foi encontrado. Instale os componentes pela versão empacotada ou use um link direto para arquivo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	args := []string{"--no-playlist", "--no-warnings", "--dump-single-json", "--skip-download"}
	if deno := findTool("deno"); deno != "" {
		args = append(args, "--js-runtimes", "deno:"+deno)
	}
	args = append(args, "--", u.String())
	cmd := exec.CommandContext(ctx, yt, args...)
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return ProbeResult{}, errors.New("tempo limite excedido ao analisar o link")
	}
	if err != nil {
		return ProbeResult{}, fmt.Errorf("não foi possível identificar um vídeo nesse link: %w", err)
	}
	var meta struct {
		Title     string  `json:"title"`
		Thumbnail string  `json:"thumbnail"`
		Duration  float64 `json:"duration"`
	}
	if json.Unmarshal(out, &meta) != nil {
		return ProbeResult{}, errors.New("a resposta da fonte não pôde ser interpretada")
	}
	result.Kind = "video"
	if strings.TrimSpace(meta.Title) != "" {
		result.Title = meta.Title
	}
	result.Thumbnail = meta.Thumbnail
	result.Duration = meta.Duration
	return result, nil
}

func probeDirect(p ProbeResult) (ProbeResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, p.URL, nil)
	if err != nil {
		return p, nil
	}
	client := &http.Client{Timeout: 8 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return p, nil
	} // alguns servidores não respondem ao HEAD
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 400 {
		p.Size = res.ContentLength
		disposition := res.Header.Get("Content-Disposition")
		if _, params, e := mime.ParseMediaType(disposition); e == nil && params["filename"] != "" {
			p.Title = safeName(params["filename"])
		}
	}
	return p, nil
}

func isDirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	ext := strings.ToLower(path.Ext(u.Path))
	switch ext {
	case ".zip", ".rar", ".7z", ".pdf", ".exe", ".iso", ".msi", ".apk", ".jpg", ".png", ".mp3", ".wav", ".mp4", ".mkv", ".webm", ".gif", ".tar", ".gz":
		return true
	}
	return false
}
