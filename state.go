package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const appVersion = "0.1.0"

type Settings struct {
	DownloadDir string `json:"downloadDir"`
	MaxParallel int    `json:"maxParallel"`
}

type Job struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	Title      string    `json:"title"`
	Kind       string    `json:"kind"`
	Format     string    `json:"format"`
	Quality    string    `json:"quality"`
	Status     string    `json:"status"`
	Progress   float64   `json:"progress"`
	BytesDone  int64     `json:"bytesDone"`
	BytesTotal int64     `json:"bytesTotal"`
	Detail     string    `json:"detail"`
	FilePath   string    `json:"filePath"`
	Error      string    `json:"error"`
	CreatedAt  time.Time `json:"createdAt"`
}

type ToolsInfo struct {
	YTDLP  bool `json:"ytDlp"`
	FFMPEG bool `json:"ffmpeg"`
	Deno   bool `json:"deno"`
}

type Snapshot struct {
	Version  string    `json:"version"`
	Settings Settings  `json:"settings"`
	Jobs     []Job     `json:"jobs"`
	Tools    ToolsInfo `json:"tools"`
}

type App struct {
	ctx        context.Context
	mu         sync.RWMutex
	fileMu     sync.Mutex
	jobs       []Job
	cancels    map[string]context.CancelFunc
	settings   Settings
	sem        chan struct{}
	configPath string
}

func NewApp() *App {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "OpenFetch")
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	app := &App{
		configPath: filepath.Join(dir, "state.json"),
		settings:   Settings{DownloadDir: filepath.Join(home, "Downloads", "OpenFetch"), MaxParallel: 2},
		cancels:    make(map[string]context.CancelFunc),
		sem:        make(chan struct{}, 2),
	}
	app.load()
	return app
}

func (a *App) startup(ctx context.Context) { a.ctx = ctx }
func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	for _, cancel := range a.cancels {
		cancel()
	}
	a.saveLocked()
	a.mu.Unlock()
}

func (a *App) GetState() Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	jobs := make([]Job, len(a.jobs))
	copy(jobs, a.jobs)
	return Snapshot{Version: appVersion, Jobs: jobs, Settings: a.settings, Tools: getTools()}
}

func (a *App) ChooseDownloadDir() (string, error) {
	if a.ctx == nil {
		return "", errors.New("a janela ainda não está pronta")
	}
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: "Escolher pasta de downloads", DefaultDirectory: a.GetState().Settings.DownloadDir})
	if err != nil {
		return "", err
	}
	if dir == "" {
		return "", nil
	}
	a.mu.Lock()
	a.settings.DownloadDir = dir
	a.saveLocked()
	a.mu.Unlock()
	a.emit()
	return dir, nil
}

func (a *App) emit() {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "openfetch:update")
	}
}

func (a *App) updateJob(id string, edit func(*Job)) {
	a.mu.Lock()
	for i := range a.jobs {
		if a.jobs[i].ID == id {
			edit(&a.jobs[i])
			break
		}
	}
	a.saveLocked()
	a.mu.Unlock()
	a.emit()
}

func (a *App) load() {
	raw, err := os.ReadFile(a.configPath)
	if err != nil {
		return
	}
	var data struct {
		Settings Settings `json:"settings"`
		Jobs     []Job    `json:"jobs"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	if data.Settings.DownloadDir != "" {
		a.settings.DownloadDir = data.Settings.DownloadDir
	}
	if data.Settings.MaxParallel >= 1 && data.Settings.MaxParallel <= 4 {
		a.settings.MaxParallel = data.Settings.MaxParallel
	}
	a.jobs = data.Jobs
	for i := range a.jobs {
		if a.jobs[i].Status == "downloading" || a.jobs[i].Status == "queued" || a.jobs[i].Status == "converting" {
			a.jobs[i].Status = "interrupted"
			a.jobs[i].Detail = "Interrompido ao encerrar o aplicativo"
		}
	}
	if len(a.jobs) > 100 {
		a.jobs = a.jobs[:100]
	}
}

func (a *App) saveLocked() {
	_ = os.MkdirAll(filepath.Dir(a.configPath), 0700)
	jobs := a.jobs
	if len(jobs) > 100 {
		jobs = jobs[:100]
	}
	payload, err := json.MarshalIndent(struct {
		Settings Settings `json:"settings"`
		Jobs     []Job    `json:"jobs"`
	}{a.settings, jobs}, "", "  ")
	if err != nil {
		return
	}
	tmp := a.configPath + ".tmp"
	if os.WriteFile(tmp, payload, 0600) != nil {
		return
	}
	_ = os.Rename(tmp, a.configPath)
}
