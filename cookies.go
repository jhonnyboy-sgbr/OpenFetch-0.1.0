package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const maxCookieFileSize = 10 << 20

func (a *App) ChooseCookiesFile() (bool, error) {
	if a.ctx == nil {
		return false, errors.New("a janela ainda não está pronta")
	}
	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Selecionar arquivo de cookies do YouTube",
		Filters: []runtime.FileFilter{{DisplayName: "Cookies Netscape", Pattern: "*.txt"}},
	})
	if err != nil {
		return false, errors.New("não foi possível abrir o seletor de cookies")
	}
	if path == "" {
		return false, nil
	}
	selected, err := a.applyCookiesSelection(path)
	if err != nil {
		return false, err
	}
	return selected, nil
}

func (a *App) applyCookiesSelection(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	if _, err := readYouTubeCookies(path); err != nil {
		return false, err
	}
	a.mu.Lock()
	a.selectedCookiesPath = path
	a.mu.Unlock()
	a.emit()
	return true, nil
}

func (a *App) RemoveCookiesFile() {
	a.mu.Lock()
	a.selectedCookiesPath = ""
	a.mu.Unlock()
	a.emit()
}

func (a *App) selectedCookiesForJob(jobID string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.jobCookies[jobID]
}

func (a *App) captureCookiesForJobLocked(jobID, kind string) {
	if kind == "file" || a.selectedCookiesPath == "" {
		return
	}
	if a.jobCookies == nil {
		a.jobCookies = make(map[string]string)
	}
	a.jobCookies[jobID] = a.selectedCookiesPath
}

func (a *App) releaseJobCookies(jobID string) {
	a.mu.Lock()
	delete(a.jobCookies, jobID)
	a.mu.Unlock()
}

func readYouTubeCookies(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxCookieFileSize {
		return nil, errors.New("o arquivo de cookies não está acessível ou não é válido")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("o arquivo de cookies não está acessível ou não é válido")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, errors.New("o arquivo de cookies não está acessível ou não é válido")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxCookieFileSize+1))
	if err != nil || len(raw) > maxCookieFileSize {
		return nil, errors.New("o arquivo de cookies não está acessível ou não é válido")
	}
	filtered, err := filterYouTubeCookieJar(raw)
	if err != nil {
		return nil, errors.New("o arquivo de cookies não está acessível ou não é válido")
	}
	return filtered, nil
}

func filterYouTubeCookieJar(raw []byte) ([]byte, error) {
	text := strings.TrimPrefix(string(raw), "\ufeff")
	var output bytes.Buffer
	output.WriteString("# Netscape HTTP Cookie File\n")
	cookieCount := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 7 {
			return nil, errors.New("invalid cookie row")
		}
		domain := strings.TrimPrefix(fields[0], "#HttpOnly_")
		if !isValidCookieDomain(domain) ||
			!isCookieBoolean(fields[1]) ||
			!strings.HasPrefix(fields[2], "/") ||
			!isCookieBoolean(fields[3]) ||
			fields[5] == "" {
			return nil, errors.New("invalid cookie row")
		}
		if _, err := strconv.ParseInt(fields[4], 10, 64); err != nil {
			return nil, errors.New("invalid cookie expiry")
		}
		if !isYouTubeCookieDomain(domain) {
			continue
		}
		output.WriteString(line)
		output.WriteByte('\n')
		cookieCount++
	}
	if cookieCount == 0 {
		return nil, errors.New("no YouTube cookies")
	}
	return output.Bytes(), nil
}

func isCookieBoolean(value string) bool {
	return strings.EqualFold(value, "TRUE") || strings.EqualFold(value, "FALSE")
}

func isValidCookieDomain(domain string) bool {
	domain = strings.TrimPrefix(domain, ".")
	if domain == "" {
		return false
	}
	for _, r := range domain {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

func isYouTubeCookieDomain(domain string) bool {
	domain = strings.TrimPrefix(strings.ToLower(domain), ".")
	return domain == "youtube.com" || strings.HasSuffix(domain, ".youtube.com")
}

func createYouTubeCookieCopy(sourcePath, cacheDir string) (string, func(), error) {
	cookieData, err := readYouTubeCookies(sourcePath)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return "", nil, errors.New("não foi possível preparar o armazenamento temporário seguro")
	}
	if err := restrictPrivateDirectory(cacheDir); err != nil {
		return "", nil, errors.New("não foi possível proteger o armazenamento temporário de cookies")
	}
	file, err := os.CreateTemp(cacheDir, "openfetch-cookies-*.txt")
	if err != nil {
		return "", nil, errors.New("não foi possível criar a cópia temporária de cookies")
	}
	tempPath := file.Name()
	remove := func() {
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("openfetch_cookie_temp_cleanup error_type=%T", err)
		}
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		remove()
		return "", nil, errors.New("não foi possível proteger a cópia temporária de cookies")
	}
	if _, err := file.Write(cookieData); err != nil {
		file.Close()
		remove()
		return "", nil, errors.New("não foi possível preparar a cópia temporária de cookies")
	}
	if err := file.Close(); err != nil {
		remove()
		return "", nil, errors.New("não foi possível preparar a cópia temporária de cookies")
	}
	return tempPath, remove, nil
}

func cookieCacheDirectory() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil || cacheDir == "" {
		return "", errors.New("armazenamento temporário de cookies indisponível")
	}
	return filepath.Join(cacheDir, "OpenFetch", "cookie-temp"), nil
}

func cleanupStaleCookieCopies(cacheDir string, now time.Time) error {
	entries, err := os.ReadDir(cacheDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("não foi possível verificar temporários antigos de cookies")
	}
	const staleAfter = 24 * time.Hour
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "openfetch-cookies-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return errors.New("não foi possível verificar um temporário antigo de cookies")
		}
		if now.Sub(info.ModTime()) < staleAfter {
			continue
		}
		if err := os.Remove(filepath.Join(cacheDir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("não foi possível limpar temporários antigos de cookies: %T", err)
		}
	}
	return nil
}

func cookieArgs(args []string, tempPath string) []string {
	if tempPath == "" {
		return args
	}
	result := make([]string, 0, len(args)+2)
	result = append(result, args...)
	return append(result, "--cookies", tempPath)
}
