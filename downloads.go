package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var percentRe = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)%`)

func (a *App) StartDownload(raw, kind, quality string) (Job, error) {
	u, err := checkURL(raw)
	if err != nil {
		return Job{}, err
	}
	if kind != "video" && kind != "audio" && kind != "file" {
		return Job{}, errors.New("tipo de download inválido")
	}
	if kind == "file" && !isDirect(raw) {
		return Job{}, errors.New("downloads comuns precisam de um link direto para arquivo")
	}
	if kind != "file" && findTool("yt-dlp") == "" {
		return Job{}, errors.New("yt-dlp não disponível")
	}
	if kind == "audio" && findTool("ffmpeg") == "" {
		return Job{}, errors.New("FFmpeg é necessário para converter em MP3")
	}
	if kind == "video" && quality != "best" && quality != "1080" && quality != "720" && quality != "480" {
		quality = "best"
	}
	if kind != "video" {
		quality = "best"
	}
	job := Job{ID: newID(), URL: u.String(), Title: path.Base(u.Path), Kind: kind, Format: "MP4", Quality: quality, Status: "queued", Detail: "Na fila", CreatedAt: time.Now()}
	if job.Title == "" || job.Title == "/" {
		job.Title = u.Hostname()
	}
	if kind == "audio" {
		job.Format = "MP3"
	}
	if kind == "file" {
		job.Format = "Original"
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.jobs = append([]Job{job}, a.jobs...)
	a.cancels[job.ID] = cancel
	a.saveLocked()
	a.mu.Unlock()
	a.emit()
	go a.runDownload(ctx, job)
	return job, nil
}

func (a *App) runDownload(ctx context.Context, job Job) {
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-ctx.Done():
		a.finishJob(job.ID, "cancelled", "Cancelado", "", nil)
		return
	}
	if ctx.Err() != nil {
		a.finishJob(job.ID, "cancelled", "Cancelado", "", nil)
		return
	}
	a.updateJob(job.ID, func(j *Job) { j.Status = "downloading"; j.Detail = "Conectando..." })
	folder := a.GetState().Settings.DownloadDir
	if err := os.MkdirAll(folder, 0755); err != nil {
		a.finishJob(job.ID, "failed", "Erro ao criar pasta", "", err)
		return
	}
	var out string
	var err error
	if job.Kind == "file" {
		out, err = a.downloadHTTP(ctx, job, folder)
	} else {
		out, err = a.downloadMedia(ctx, job, folder)
	}
	if ctx.Err() != nil {
		a.finishJob(job.ID, "cancelled", "Cancelado", "", nil)
		return
	}
	if err != nil {
		a.finishJob(job.ID, "failed", "Falha no download", "", err)
		return
	}
	a.finishJob(job.ID, "completed", "Concluído", out, nil)
}

func (a *App) finishJob(id, status, detail, out string, err error) {
	a.updateJob(id, func(j *Job) {
		j.Status, j.Detail, j.FilePath = status, detail, out
		if status == "completed" {
			j.Progress = 100
		}
		if err != nil {
			j.Error = err.Error()
		}
	})
	a.mu.Lock()
	delete(a.cancels, id)
	a.mu.Unlock()
}

func (a *App) CancelDownload(id string) error {
	a.mu.RLock()
	cancel, ok := a.cancels[id]
	a.mu.RUnlock()
	if !ok {
		return errors.New("download não está em andamento")
	}
	cancel()
	return nil
}

func (a *App) downloadHTTP(ctx context.Context, job Job, folder string) (string, error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: dialer.DialContext, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("redirecionamentos demais")
		}
		if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
			return errors.New("redirecionamento inseguro")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, job.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "OpenFetch/0.1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("servidor respondeu HTTP %d", resp.StatusCode)
	}
	mediaType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(mediaType, "text/html") {
		return "", errors.New("o link retornou uma página HTML, não um arquivo")
	}
	u, _ := checkURL(job.URL)
	name := path.Base(u.Path)
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = params["filename"]
	}
	if name == "" || name == "." || name == "/" {
		name = "download.bin"
	}
	name = safeName(name)
	temp, err := os.CreateTemp(folder, ".openfetch-*.part")
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	defer temp.Close()
	total := resp.ContentLength
	a.updateJob(job.ID, func(j *Job) { j.Title = name; j.BytesTotal = total; j.Detail = "Baixando arquivo..." })
	data := make([]byte, 128*1024)
	var done int64
	last := time.Now()
	for {
		n, readErr := resp.Body.Read(data)
		if n > 0 {
			written, writeErr := temp.Write(data[:n])
			done += int64(written)
			if writeErr != nil {
				return "", writeErr
			}
			if written != n {
				return "", io.ErrShortWrite
			}
		}
		if time.Since(last) > 250*time.Millisecond || readErr != nil {
			pct := float64(0)
			if total > 0 {
				pct = float64(done) * 100 / float64(total)
			}
			a.updateJob(job.ID, func(j *Job) { j.BytesDone = done; j.Progress = pct; j.Detail = formatBytes(done) + " recebidos" })
			last = time.Now()
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	if total >= 0 && total != done {
		return "", fmt.Errorf("arquivo incompleto: %d de %d bytes", done, total)
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	dest := uniquePath(folder, name)
	if err := os.Rename(tempPath, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, unit := range []string{"KB", "MB", "GB", "TB"} {
		value /= 1024
		if value < 1024 {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%.1f PB", value/1024)
}

func (a *App) downloadMedia(ctx context.Context, job Job, folder string) (string, error) {
	yt := findTool("yt-dlp")
	if yt == "" {
		return "", errors.New("yt-dlp não encontrado")
	}
	template := "%(title).150B [%(id)s].%(ext)s"
	args := []string{"--no-playlist", "--no-overwrites", "--newline", "--no-warnings", "--progress-template", "download:%(progress._percent_str)s|%(progress._speed_str)s|%(progress._eta_str)s", "--print", "after_move:OF_FILE:%(filepath)s", "-P", folder, "-o", template}
	if deno := findTool("deno"); deno != "" {
		args = append(args, "--js-runtimes", "deno:"+deno)
	}
	if ff := findTool("ffmpeg"); ff != "" {
		args = append(args, "--ffmpeg-location", filepath.Dir(ff))
	}
	if job.Kind == "audio" {
		args = append(args, "-x", "--audio-format", "mp3", "--audio-quality", "0")
	} else {
		selector := "bv*+ba/b"
		if job.Quality != "best" {
			selector = "bv*[height<=" + job.Quality + "]+ba/b[height<=" + job.Quality + "]"
		}
		args = append(args, "-f", selector, "--merge-output-format", "mp4")
	}
	args = append(args, "--", job.URL)
	cmd := exec.CommandContext(ctx, yt, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	lines := make(chan string, 96)
	readLines := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}
	go func() { readLines(stdout); lines <- "OF_STREAM_END" }()
	go func() { readLines(stderr); lines <- "OF_STREAM_END" }()
	streams := 0
	var outputPath string
	var lastError string
	last := time.Time{}
	for streams < 2 {
		line := <-lines
		if line == "OF_STREAM_END" {
			streams++
			continue
		}
		if strings.HasPrefix(line, "OF_FILE:") {
			outputPath = strings.TrimSpace(strings.TrimPrefix(line, "OF_FILE:"))
		}
		if strings.Contains(strings.ToLower(line), "error:") {
			lastError = line
		}
		if strings.HasPrefix(line, "download:") {
			parts := strings.SplitN(strings.TrimPrefix(line, "download:"), "|", 3)
			pct := percentRe.FindStringSubmatch(parts[0])
			if len(pct) > 1 && time.Since(last) >= 150*time.Millisecond {
				value, _ := strconv.ParseFloat(pct[1], 64)
				detail := "Baixando mídia"
				if len(parts) >= 3 {
					detail = strings.TrimSpace(parts[1]) + " · ETA " + strings.TrimSpace(parts[2])
				}
				a.updateJob(job.ID, func(j *Job) { j.Progress = value; j.Detail = detail })
				last = time.Now()
			}
		} else if strings.Contains(line, "[ExtractAudio]") || strings.Contains(line, "[Merger]") {
			a.updateJob(job.ID, func(j *Job) { j.Status = "converting"; j.Detail = "Processando mídia..." })
		}
	}
	err = cmd.Wait()
	if err != nil {
		if lastError != "" {
			return "", errors.New(lastError)
		}
		return "", fmt.Errorf("yt-dlp: %w", err)
	}
	if outputPath == "" {
		return "", errors.New("não foi possível confirmar o arquivo de saída")
	}
	if _, err := os.Stat(outputPath); err != nil {
		return "", errors.New("arquivo final não foi encontrado")
	}
	return outputPath, nil
}
