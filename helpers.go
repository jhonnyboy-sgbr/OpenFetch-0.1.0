package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var invalidFileChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1F]`)
var reservedFileName = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])($|\.)`)

func checkURL(value string) (*url.URL, error) {
	if len(value) > 4096 {
		return nil, errors.New("link muito longo")
	}
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("insira um link HTTP ou HTTPS válido")
	}
	return u, nil
}

func safeName(value string) string {
	value = invalidFileChars.ReplaceAllString(strings.TrimSpace(value), "_")
	value = strings.TrimRight(value, " .")
	if len([]rune(value)) > 170 {
		value = string([]rune(value)[:170])
	}
	if value == "" || value == "." || value == ".." {
		value = "arquivo"
	}
	if reservedFileName.MatchString(value) {
		value = "_" + value
	}
	return value
}

func uniquePath(folder, filename string) string {
	filename = safeName(filename)
	candidate := filepath.Join(folder, filename)
	stem, ext := strings.TrimSuffix(filename, filepath.Ext(filename)), filepath.Ext(filename)
	for i := 2; ; i++ {
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
		candidate = filepath.Join(folder, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
}

func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(buf)
}

func findTool(name string) string {
	executable := name
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		for _, path := range []string{filepath.Join(dir, "tools", executable), filepath.Join(dir, executable)} {
			if st, err := os.Stat(path); err == nil && !st.IsDir() {
				return path
			}
		}
	}
	path, err := exec.LookPath(executable)
	if err == nil {
		return path
	}
	return ""
}

func getTools() ToolsInfo {
	return ToolsInfo{YTDLP: findTool("yt-dlp") != "", FFMPEG: findTool("ffmpeg") != "", Deno: findTool("deno") != ""}
}

func (a *App) OpenDownloadDir() error {
	dir := a.GetState().Settings.DownloadDir
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer.exe", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	return cmd.Start()
}
