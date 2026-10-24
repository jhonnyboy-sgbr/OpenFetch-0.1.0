package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

func (a *App) ChooseMediaFile() (string, error) {
	if a.ctx == nil {
		return "", errors.New("janela ainda não está pronta")
	}
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Selecionar vídeo ou áudio",
		Filters: []runtime.FileFilter{{DisplayName: "Arquivos de mídia", Pattern: "*.mp4;*.mkv;*.webm;*.mov;*.avi;*.mp3;*.wav;*.flac;*.m4a;*.ogg"}},
	})
}

func (a *App) ConvertMedia(input, format string) (Job, error) {
	if format != "mp3" && format != "mp4" {
		return Job{}, errors.New("formato não suportado")
	}
	if findTool("ffmpeg") == "" {
		return Job{}, errors.New("FFmpeg não encontrado")
	}
	info, err := os.Stat(input)
	if err != nil || info.IsDir() {
		return Job{}, errors.New("arquivo de origem inválido")
	}
	if strings.EqualFold(filepath.Ext(input), "."+format) {
		return Job{}, errors.New("o arquivo já está nesse formato")
	}
	job := Job{ID: newID(), Title: filepath.Base(input), URL: input, Kind: "convert", Format: strings.ToUpper(format), Status: "queued", Quality: "best", Detail: "Na fila", CreatedAt: time.Now()}
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.jobs = append([]Job{job}, a.jobs...)
	a.cancels[job.ID] = cancel
	a.saveLocked()
	a.mu.Unlock()
	a.emit()
	go func() {
		select {
		case a.sem <- struct{}{}:
			defer func() { <-a.sem }()
		case <-ctx.Done():
			a.finishJob(job.ID, "cancelled", "Cancelado", "", nil)
			return
		}
		a.updateJob(job.ID, func(j *Job) { j.Status = "converting"; j.Detail = "Convertendo mídia..." })
		folder := a.GetState().Settings.DownloadDir
		if err := os.MkdirAll(folder, 0755); err != nil {
			a.finishJob(job.ID, "failed", "Erro", "", err)
			return
		}
		sourceName := strings.TrimSuffix(filepath.Base(input), filepath.Ext(input))
		output := uniquePath(folder, safeName(sourceName)+"."+format)
		args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-n", "-i", input}
		if format == "mp3" {
			args = append(args, "-vn", "-codec:a", "libmp3lame", "-qscale:a", "2")
		} else {
			args = append(args, "-c:v", "libx264", "-preset", "medium", "-crf", "23", "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart")
		}
		args = append(args, output)
		cmd := exec.CommandContext(ctx, findTool("ffmpeg"), args...)
		err := cmd.Run()
		if ctx.Err() != nil {
			_ = os.Remove(output)
			a.finishJob(job.ID, "cancelled", "Cancelado", "", nil)
			return
		}
		if err != nil {
			_ = os.Remove(output)
			a.finishJob(job.ID, "failed", "Falha na conversão", "", fmt.Errorf("ffmpeg: %w", err))
			return
		}
		a.finishJob(job.ID, "completed", "Conversão concluída", output, nil)
	}()
	return job, nil
}
