package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var percentRe = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)%`)
var diagnosticURLRe = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
var diagnosticSecretRe = regexp.MustCompile(`(?i)(\b(?:token|access[_-]?token|refresh[_-]?token|signature|cookie|authorization|password|secret|api[_-]?key)\b\s*[:=]).*`)
var diagnosticFilesystemPathRe = regexp.MustCompile(`(?:[A-Za-z]:\\|\\\\)[^\s"'<>]+|(?:^|\s)/(?:[^\s"'<>]+)`)
var diagnosticControlCharsRe = regexp.MustCompile(`[\x00-\x1f\x7f]+`)
var diagnosticExtensionRe = regexp.MustCompile(`^\.[a-z0-9]{1,10}$`)

const maxDiagnosticErrorLength = 400

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
	a.captureCookiesForJobLocked(job.ID, kind)
	a.saveLocked()
	a.mu.Unlock()
	a.emit()
	go a.runDownload(ctx, job)
	return job, nil
}

func (a *App) runDownload(ctx context.Context, job Job) {
	defer a.releaseJobCookies(job.ID)
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
	var mediaFiles []mediaFinalFile
	var err error
	if job.Kind == "file" {
		out, err = a.downloadHTTP(ctx, job, folder)
	} else {
		mediaFiles, err = a.downloadMedia(ctx, job, folder)
	}
	if ctx.Err() != nil {
		a.finishJob(job.ID, "cancelled", "Cancelado", "", nil)
		return
	}
	if err != nil {
		a.finishJob(job.ID, "failed", "Falha no download", "", err)
		return
	}
	if job.Kind == "file" {
		a.finishJob(job.ID, "completed", "Concluído", out, nil)
		return
	}
	a.finishMediaJobs(job.ID, mediaFiles)
}

func (a *App) finishMediaJobs(id string, files []mediaFinalFile) {
	a.mu.Lock()
	a.jobs = replaceJobWithMediaFiles(a.jobs, id, files)
	delete(a.cancels, id)
	a.saveLocked()
	a.mu.Unlock()
	a.emit()
}

func replaceJobWithMediaFiles(jobs []Job, id string, files []mediaFinalFile) []Job {
	if len(files) == 0 {
		return jobs
	}
	replaced := make([]Job, 0, len(jobs)+len(files)-1)
	for _, job := range jobs {
		if job.ID == id {
			replaced = append(replaced, completedMediaJobs(job, files)...)
			continue
		}
		replaced = append(replaced, job)
	}
	return replaced
}

func completedMediaJobs(job Job, files []mediaFinalFile) []Job {
	completed := make([]Job, 0, len(files))
	for index, file := range files {
		item := job
		if index > 0 {
			item.ID = newID()
		}
		item.Title = filepath.Base(file.path)
		item.Status = "completed"
		item.Progress = 100
		item.BytesDone = 0
		item.BytesTotal = 0
		item.Detail = "Concluído"
		item.FilePath = file.path
		item.Error = ""
		completed = append(completed, item)
	}
	return completed
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
	dest, _, err := moveToUniqueCopyPath(folder, name, tempPath)
	if err != nil {
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

func parseMediaOutputPath(line string) (string, bool) {
	if !strings.HasPrefix(line, "OF_FILE:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "OF_FILE:")), true
}

func resolveMediaOutput(outputPath, lastError string, commandErr error) (string, error) {
	return resolveMediaOutputWithStat(outputPath, lastError, commandErr, func(path string) (os.FileInfo, error) {
		return os.Stat(path)
	})
}

func resolveMediaOutputWithStat(outputPath, lastError string, commandErr error, stat func(string) (os.FileInfo, error)) (string, error) {
	if commandErr != nil {
		if lastError != "" {
			return "", errors.New(lastError)
		}
		return "", fmt.Errorf("yt-dlp: %w", commandErr)
	}
	if outputPath == "" {
		return "", errors.New("não foi possível confirmar o arquivo de saída")
	}
	if _, err := stat(outputPath); err != nil {
		return "", errors.New("arquivo final não foi encontrado")
	}
	return outputPath, nil
}

func resolveMediaOutputWithCandidates(outputPath, lastError string, commandErr error, candidatePaths []string, stat func(string) (os.FileInfo, error)) (string, bool, error) {
	outputs := []capturedMediaPath(nil)
	if outputPath != "" {
		outputs = append(outputs, capturedMediaPath{path: outputPath})
	}
	resolved, fallbackUsed, err := resolveMediaOutputsWithCandidates(outputs, lastError, commandErr, candidatePaths, stat)
	if err != nil {
		return "", false, err
	}
	return resolved[0], fallbackUsed, nil
}

func resolveMediaOutputsWithCandidates(outputPaths []capturedMediaPath, lastError string, commandErr error, candidatePaths []string, stat func(string) (os.FileInfo, error)) ([]string, bool, error) {
	lastOutputPath := ""
	if len(outputPaths) > 0 {
		lastOutputPath = outputPaths[len(outputPaths)-1].path
	}
	if commandErr != nil {
		_, err := resolveMediaOutputWithStat(lastOutputPath, lastError, commandErr, stat)
		return nil, false, err
	}
	if len(outputPaths) == 0 {
		_, err := resolveMediaOutputWithStat("", lastError, nil, stat)
		return nil, false, err
	}

	resolved := make([]string, len(outputPaths))
	missingIndexes := make([]int, 0, len(outputPaths))
	resolvedPaths := make(map[string]struct{}, len(outputPaths))
	for index, output := range outputPaths {
		if output.path == "" {
			return nil, false, errors.New("não foi possível confirmar o arquivo de saída")
		}
		key := normalizedMediaPath(output.path)
		if _, duplicate := resolvedPaths[key]; duplicate {
			return nil, false, errors.New("saídas de mídia duplicadas")
		}
		resolvedPaths[key] = struct{}{}
		info, err := stat(output.path)
		if err == nil {
			if len(outputPaths) > 1 && !info.Mode().IsRegular() {
				return nil, false, errors.New("arquivo final não foi encontrado")
			}
			resolved[index] = output.path
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, false, errors.New("arquivo final não foi encontrado")
		}
		missingIndexes = append(missingIndexes, index)
	}
	if len(missingIndexes) == 0 {
		return resolved, false, nil
	}
	if lastError != "" {
		return nil, false, errors.New("arquivo final não foi encontrado")
	}

	availableCandidates := make([]string, 0, len(candidatePaths))
	seenCandidates := make(map[string]struct{}, len(candidatePaths))
	for _, candidatePath := range candidatePaths {
		key := normalizedMediaPath(candidatePath)
		if _, duplicate := seenCandidates[key]; duplicate {
			return nil, false, errors.New("arquivo final não foi encontrado")
		}
		seenCandidates[key] = struct{}{}
		if _, alreadyResolved := resolvedPaths[key]; alreadyResolved {
			continue
		}
		info, err := stat(candidatePath)
		if err != nil || !info.Mode().IsRegular() {
			return nil, false, errors.New("arquivo final não foi encontrado")
		}
		availableCandidates = append(availableCandidates, candidatePath)
	}
	if len(availableCandidates) != len(missingIndexes) {
		return nil, false, errors.New("arquivo final não foi encontrado")
	}

	expectedExtensions := make(map[string]int)
	for _, index := range missingIndexes {
		expectedExtensions[strings.ToLower(filepath.Ext(outputPaths[index].path))]++
	}
	for _, candidatePath := range availableCandidates {
		extension := strings.ToLower(filepath.Ext(candidatePath))
		if expectedExtensions[extension] == 0 {
			return nil, false, errors.New("arquivo final não foi encontrado")
		}
		expectedExtensions[extension]--
	}
	for _, count := range expectedExtensions {
		if count != 0 {
			return nil, false, errors.New("arquivo final não foi encontrado")
		}
	}

	candidatesByExtension := make(map[string][]string)
	for _, candidatePath := range availableCandidates {
		extension := strings.ToLower(filepath.Ext(candidatePath))
		candidatesByExtension[extension] = append(candidatesByExtension[extension], candidatePath)
	}
	for _, index := range missingIndexes {
		extension := strings.ToLower(filepath.Ext(outputPaths[index].path))
		matches := candidatesByExtension[extension]
		if len(matches) > 0 {
			resolved[index] = matches[0]
			candidatesByExtension[extension] = matches[1:]
		}
		if resolved[index] == "" {
			return nil, false, errors.New("arquivo final não foi encontrado")
		}
	}
	return resolved, true, nil
}

func normalizedMediaPath(path string) string {
	cleaned := filepath.Clean(path)
	if absolute, err := filepath.Abs(cleaned); err == nil {
		cleaned = filepath.Clean(absolute)
	}
	return strings.ToLower(cleaned)
}

func moveToUniqueCopyPath(folder, filename, sourcePath string) (string, bool, error) {
	filename = filepath.Base(filename)
	extension := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, extension)
	for copyNumber := 0; ; copyNumber++ {
		candidateName := filename
		if copyNumber > 0 {
			candidateName = fmt.Sprintf("%s.copia(%d)%s", stem, copyNumber, extension)
		}
		candidate := filepath.Join(folder, candidateName)
		reservation, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if err := reservation.Close(); err != nil {
			return "", false, errors.Join(err, os.Remove(candidate))
		}
		if err := os.Rename(sourcePath, candidate); err != nil {
			return "", false, errors.Join(err, os.Remove(candidate))
		}
		return candidate, copyNumber > 0, nil
	}
}

type mediaFinalFile struct {
	path        string
	copyCreated bool
}

func moveMediaFilesToUniqueCopyPaths(folder string, sourcePaths []string) ([]mediaFinalFile, error) {
	seen := make(map[string]struct{}, len(sourcePaths))
	for _, sourcePath := range sourcePaths {
		key := normalizedMediaPath(sourcePath)
		if _, duplicate := seen[key]; duplicate {
			return nil, errors.New("saídas de mídia duplicadas")
		}
		seen[key] = struct{}{}
		info, err := os.Stat(sourcePath)
		if err != nil {
			return nil, fmt.Errorf("não foi possível validar um arquivo de mídia: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("arquivo de mídia inválido")
		}
	}

	files := make([]mediaFinalFile, 0, len(sourcePaths))
	for _, sourcePath := range sourcePaths {
		destination, copyCreated, err := moveToUniqueCopyPath(folder, filepath.Base(sourcePath), sourcePath)
		if err != nil {
			rollbackErrors := make([]error, 0, len(files))
			for index := len(files) - 1; index >= 0; index-- {
				if rollbackErr := os.Rename(files[index].path, sourcePaths[index]); rollbackErr != nil {
					rollbackErrors = append(rollbackErrors, rollbackErr)
				}
			}
			return nil, errors.Join(fmt.Errorf("não foi possível salvar o conjunto de arquivos: %w", err), errors.Join(rollbackErrors...))
		}
		files = append(files, mediaFinalFile{path: destination, copyCreated: copyCreated})
	}
	return files, nil
}

func redactMediaDiagnosticError(err error, path string) string {
	if err == nil {
		return ""
	}
	return sanitizeMediaDiagnosticMessage(err.Error(), path)
}

func sanitizeMediaDiagnosticMessage(message, path string) string {
	if path != "" {
		message = strings.ReplaceAll(message, path, "[caminho redigido]")
	}
	message = diagnosticURLRe.ReplaceAllString(message, "[URL redigida]")
	message = diagnosticSecretRe.ReplaceAllString(message, "$1 [segredo redigido]")
	message = diagnosticFilesystemPathRe.ReplaceAllString(message, " [caminho redigido]")
	message = diagnosticControlCharsRe.ReplaceAllString(message, " ")
	message = strings.TrimSpace(message)
	if len(message) > maxDiagnosticErrorLength {
		message = message[:maxDiagnosticErrorLength] + "...[truncado]"
	}
	return message
}

func redactCookieTempPath(message, cookiePath string) string {
	if cookiePath != "" {
		message = strings.ReplaceAll(message, cookiePath, "[cookies temporários redigidos]")
	}
	return message
}

func normalizedCookieDownloadError(message string) string {
	message = strings.ToLower(message)
	switch {
	case strings.Contains(message, "sign in to confirm"),
		strings.Contains(message, "authentication is required"),
		strings.Contains(message, "login required"):
		return "o YouTube exige autenticação; verifique o arquivo de cookies selecionado"
	case strings.Contains(message, "http error 429"),
		strings.Contains(message, "http error 402"),
		strings.Contains(message, "too many requests"):
		return "o acesso foi temporariamente limitado pelo YouTube"
	case strings.Contains(message, "http error 403"):
		return "o YouTube recusou o acesso; os cookies podem estar inválidos ou sem permissão"
	default:
		return "não foi possível baixar a mídia com os cookies selecionados"
	}
}

func logMediaDiagnostic(jobID, stage, details string) {
	log.Printf("openfetch_media_diagnostic job=%s at=%s stage=%s %s", jobID, time.Now().Format(time.RFC3339Nano), stage, details)
}

type mediaFileSnapshot map[string]mediaFileMetadata

type mediaFileMetadata struct {
	size    int64
	modTime time.Time
}

type mediaFileComparison struct {
	changedCount      int
	changedExtensions map[string]int
	capturedMatches   []mediaPathMatchCounts
	candidatePaths    []string
}

type capturedMediaPath struct {
	path      string
	elapsedMS int64
}

type mediaPathMatchCounts struct {
	folder              int
	fullPath            int
	name                int
	ext                 int
	fullPathFirstSeenMS int64
	nameFirstSeenMS     int64
	extFirstSeenMS      int64
}

type mediaFileObservationResult struct {
	files map[string]int64
	err   error
}

func diagnosticExtension(path string) string {
	extension := strings.ToLower(filepath.Ext(path))
	if extension == "" {
		return "[sem extensão]"
	}
	if !diagnosticExtensionRe.MatchString(extension) {
		return "[outra extensão]"
	}
	return extension
}

func formatDiagnosticExtensions(extensions []string) string {
	if len(extensions) == 0 {
		return "[]"
	}
	return "[" + strings.Join(extensions, ",") + "]"
}

func formatDiagnosticExtensionCounts(extensions map[string]int) string {
	keys := make([]string, 0, len(extensions))
	for extension := range extensions {
		keys = append(keys, extension)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, extension := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", extension, extensions[extension]))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func snapshotMediaFiles(folder string) (mediaFileSnapshot, error) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil, err
	}
	snapshot := make(mediaFileSnapshot)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			snapshot[entry.Name()] = mediaFileMetadata{size: info.Size(), modTime: info.ModTime()}
		}
	}
	return snapshot, nil
}

func mediaFileChanged(before mediaFileSnapshot, name string, current mediaFileMetadata) bool {
	previous, existed := before[name]
	return !existed || previous.size != current.size || !previous.modTime.Equal(current.modTime)
}

func observeMediaFiles(folder string, before mediaFileSnapshot, startedAt time.Time, stop <-chan struct{}) (map[string]int64, error) {
	observed := make(map[string]int64)
	observe := func() error {
		current, err := snapshotMediaFiles(folder)
		if err != nil {
			return err
		}
		for name, metadata := range current {
			if mediaFileChanged(before, name, metadata) {
				if _, exists := observed[name]; !exists {
					observed[name] = time.Since(startedAt).Milliseconds()
				}
			}
		}
		return nil
	}
	if err := observe(); err != nil {
		return observed, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := observe(); err != nil {
				return observed, err
			}
		case <-stop:
			if err := observe(); err != nil {
				return observed, err
			}
			return observed, nil
		}
	}
}

func compareMediaFiles(folder string, before mediaFileSnapshot, outputPaths []capturedMediaPath, observed map[string]int64) (mediaFileComparison, error) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return mediaFileComparison{}, err
	}
	comparison := mediaFileComparison{
		changedExtensions: make(map[string]int),
		capturedMatches:   make([]mediaPathMatchCounts, len(outputPaths)),
	}
	for index := range comparison.capturedMatches {
		comparison.capturedMatches[index].fullPathFirstSeenMS = -1
		comparison.capturedMatches[index].nameFirstSeenMS = -1
		comparison.capturedMatches[index].extFirstSeenMS = -1
	}
	outputNames := make([]string, len(outputPaths))
	outputExtensions := make([]string, len(outputPaths))
	outputFolders := make([]string, len(outputPaths))
	outputFullPaths := make([]string, len(outputPaths))
	for index, output := range outputPaths {
		outputNames[index] = filepath.Base(output.path)
		outputExtensions[index] = strings.ToLower(filepath.Ext(output.path))
		outputFullPaths[index] = filepath.Clean(output.path)
		if absPath, absErr := filepath.Abs(outputFullPaths[index]); absErr == nil {
			outputFullPaths[index] = filepath.Clean(absPath)
		}
		outputFolders[index] = filepath.Clean(filepath.Dir(outputFullPaths[index]))
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return mediaFileComparison{}, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		metadata := mediaFileMetadata{size: info.Size(), modTime: info.ModTime()}
		if !mediaFileChanged(before, entry.Name(), metadata) {
			continue
		}
		comparison.changedCount++
		comparison.candidatePaths = append(comparison.candidatePaths, filepath.Join(folder, entry.Name()))
		comparison.changedExtensions[diagnosticExtension(entry.Name())]++
		fullPath := filepath.Clean(filepath.Join(folder, entry.Name()))
		if absPath, absErr := filepath.Abs(fullPath); absErr == nil {
			fullPath = filepath.Clean(absPath)
		}
		candidateFolder := filepath.Clean(filepath.Dir(fullPath))
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		firstSeenMS, wasObserved := observed[entry.Name()]
		for index, output := range outputPaths {
			if output.path == "" {
				continue
			}
			if strings.EqualFold(candidateFolder, outputFolders[index]) {
				comparison.capturedMatches[index].folder++
			}
			if strings.EqualFold(fullPath, outputFullPaths[index]) {
				comparison.capturedMatches[index].fullPath++
				if wasObserved && (comparison.capturedMatches[index].fullPathFirstSeenMS < 0 || firstSeenMS < comparison.capturedMatches[index].fullPathFirstSeenMS) {
					comparison.capturedMatches[index].fullPathFirstSeenMS = firstSeenMS
				}
			}
			if strings.EqualFold(entry.Name(), outputNames[index]) {
				comparison.capturedMatches[index].name++
				if wasObserved && (comparison.capturedMatches[index].nameFirstSeenMS < 0 || firstSeenMS < comparison.capturedMatches[index].nameFirstSeenMS) {
					comparison.capturedMatches[index].nameFirstSeenMS = firstSeenMS
				}
			}
			if extension == outputExtensions[index] {
				comparison.capturedMatches[index].ext++
				if wasObserved && (comparison.capturedMatches[index].extFirstSeenMS < 0 || firstSeenMS < comparison.capturedMatches[index].extFirstSeenMS) {
					comparison.capturedMatches[index].extFirstSeenMS = firstSeenMS
				}
			}
		}
	}
	return comparison, nil
}

func formatMediaPathMatches(outputPaths []capturedMediaPath, matches []mediaPathMatchCounts) string {
	parts := make([]string, 0, len(outputPaths))
	for index := range outputPaths {
		match := mediaPathMatchCounts{}
		if index < len(matches) {
			match = matches[index]
		}
		parts = append(parts, fmt.Sprintf(
			"capture=%d folder_match=%t name_match=%t extension_match=%t normalized_path_match=%t",
			index+1, match.folder > 0, match.name > 0, match.ext > 0, match.fullPath > 0,
		))
	}
	return "[" + strings.Join(parts, ";") + "]"
}

type mediaOutputEvent struct {
	line   string
	stream string
	done   bool
	err    error
}

func (a *App) downloadMedia(ctx context.Context, job Job, folder string) ([]mediaFinalFile, error) {
	yt := findTool("yt-dlp")
	if yt == "" {
		return nil, errors.New("yt-dlp não encontrado")
	}
	cookieTempPath := ""
	if selectedPath := a.selectedCookiesForJob(job.ID); selectedPath != "" {
		cacheDir, err := cookieCacheDirectory()
		if err != nil {
			return nil, errors.New("o arquivo de cookies selecionado não está acessível")
		}
		var cleanup func()
		cookieTempPath, cleanup, err = createYouTubeCookieCopy(selectedPath, cacheDir)
		if err != nil {
			return nil, err
		}
		defer func() {
			cleanup()
		}()
	}
	stagingFolder, err := os.MkdirTemp(folder, ".openfetch-"+job.ID+"-")
	if err != nil {
		return nil, fmt.Errorf("não foi possível preparar a pasta temporária do download: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(stagingFolder); cleanupErr != nil {
			logMediaDiagnostic(job.ID, "staging_cleanup_error", "error_type="+fmt.Sprintf("%T", cleanupErr))
		}
	}()
	deno := findTool("deno")
	ffmpegDir := ""
	if ff := findTool("ffmpeg"); ff != "" {
		ffmpegDir = filepath.Dir(ff)
	}
	args := buildMediaArgs(job, stagingFolder, deno, ffmpegDir, cookieTempPath)
	args = append(args, "--", job.URL)
	cmd := exec.CommandContext(ctx, yt, args...)
	filesBefore, snapshotErr := snapshotMediaFiles(stagingFolder)
	if snapshotErr != nil {
		logMediaDiagnostic(job.ID, "folder_snapshot_error", "error_type="+fmt.Sprintf("%T", snapshotErr))
	}
	logMediaDiagnostic(job.ID, "process_start", "started=false")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logMediaDiagnostic(job.ID, "stdout_pipe_error", "error_type="+fmt.Sprintf("%T", err))
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		logMediaDiagnostic(job.ID, "stderr_pipe_error", "error_type="+fmt.Sprintf("%T", err))
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		logMediaDiagnostic(job.ID, "process_start_error", "error="+strconv.Quote(redactMediaDiagnosticError(err, yt)))
		return nil, err
	}
	processStartedAt := time.Now()
	logMediaDiagnostic(job.ID, "process_started", "started=true")
	var stopFolderObserver chan struct{}
	var folderObservationResult chan mediaFileObservationResult
	if snapshotErr == nil {
		stopFolderObserver = make(chan struct{})
		folderObservationResult = make(chan mediaFileObservationResult, 1)
		go func() {
			files, observationErr := observeMediaFiles(stagingFolder, filesBefore, processStartedAt, stopFolderObserver)
			folderObservationResult <- mediaFileObservationResult{files: files, err: observationErr}
		}()
	}
	lines := make(chan mediaOutputEvent, 96)
	readLines := func(r io.Reader, stream string) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			select {
			case lines <- mediaOutputEvent{line: scanner.Text(), stream: stream}:
			case <-ctx.Done():
				lines <- mediaOutputEvent{stream: stream, done: true, err: scanner.Err()}
				return
			}
		}
		lines <- mediaOutputEvent{stream: stream, done: true, err: scanner.Err()}
	}
	go readLines(stdout, "stdout")
	go readLines(stderr, "stderr")
	streams := 0
	var outputPath string
	var capturedPaths []capturedMediaPath
	var lastError string
	var outputPathCount int
	var outputExtensions []string
	var stdoutScanErr, stderrScanErr error
	last := time.Time{}
	for streams < 2 {
		event := <-lines
		if event.done {
			streams++
			if event.err != nil {
				if event.stream == "stdout" {
					stdoutScanErr = event.err
				} else {
					stderrScanErr = event.err
				}
			}
			continue
		}
		line := event.line
		if path, ok := parseMediaOutputPath(line); ok {
			outputPath = path
			outputPathCount++
			captured := capturedMediaPath{path: path, elapsedMS: time.Since(processStartedAt).Milliseconds()}
			capturedPaths = append(capturedPaths, captured)
			outputExtensions = append(outputExtensions, diagnosticExtension(path))
			logMediaDiagnostic(job.ID, "output_path_captured", fmt.Sprintf(
				"capture=%d elapsed_ms=%d extension=%s",
				outputPathCount, captured.elapsedMS, diagnosticExtension(path),
			))
		}
		if strings.Contains(strings.ToLower(line), "error:") {
			lastError = sanitizeMediaDiagnosticMessage(redactCookieTempPath(line, cookieTempPath), outputPath)
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
	waitDetails := "error=nil"
	if err != nil {
		waitDetails = "error=" + strconv.Quote(redactMediaDiagnosticError(err, outputPath))
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			waitDetails += fmt.Sprintf(" exit_code=%d", exitErr.ExitCode())
		}
	}
	logMediaDiagnostic(job.ID, "process_wait", waitDetails)
	observedFiles := make(map[string]int64)
	if folderObservationResult != nil {
		close(stopFolderObserver)
		observation := <-folderObservationResult
		observedFiles = observation.files
		if observation.err != nil {
			logMediaDiagnostic(job.ID, "folder_observer_error", "error_type="+fmt.Sprintf("%T", observation.err))
		}
	}

	var outputInfo os.FileInfo
	var statErr error
	if outputPath != "" {
		outputInfo, statErr = os.Stat(outputPath)
	}
	statDetails := fmt.Sprintf("path_captured=%t of_file_lines=%d exists=%t", outputPath != "", outputPathCount, statErr == nil && outputInfo != nil)
	if statErr != nil {
		statDetails += " stat_error=" + strconv.Quote(redactMediaDiagnosticError(statErr, outputPath))
	} else if outputInfo != nil {
		statDetails += fmt.Sprintf(" regular_file=%t size_bytes=%d mod_time=%s", outputInfo.Mode().IsRegular(), outputInfo.Size(), outputInfo.ModTime().Format(time.RFC3339Nano))
	}
	statDetails += " captured_extensions=" + formatDiagnosticExtensions(outputExtensions)
	logMediaDiagnostic(job.ID, "output_stat", statDetails)
	logMediaDiagnostic(job.ID, "scanner_results", fmt.Sprintf("stdout_error=%q stderr_error=%q", redactMediaDiagnosticError(stdoutScanErr, outputPath), redactMediaDiagnosticError(stderrScanErr, outputPath)))
	logMessage := sanitizeMediaDiagnosticMessage(lastError, outputPath)
	if cookieTempPath != "" && logMessage != "" {
		logMessage = normalizedCookieDownloadError(logMessage)
	}
	logMediaDiagnostic(job.ID, "last_error", "message="+strconv.Quote(logMessage))

	var comparison mediaFileComparison
	comparisonReady := false
	if snapshotErr == nil {
		var compareErr error
		comparison, compareErr = compareMediaFiles(stagingFolder, filesBefore, capturedPaths, observedFiles)
		if compareErr != nil {
			logMediaDiagnostic(job.ID, "folder_comparison_error", "error_type="+fmt.Sprintf("%T", compareErr))
		} else {
			comparisonReady = true
			logMediaDiagnostic(job.ID, "folder_comparison", fmt.Sprintf(
				"candidate_count=%d captures=%s",
				comparison.changedCount,
				formatMediaPathMatches(capturedPaths, comparison.capturedMatches),
			))
		}
	}

	candidatePaths := []string(nil)
	if comparisonReady {
		candidatePaths = comparison.candidatePaths
	}
	resolvedPaths, fallbackUsed, resolveErr := resolveMediaOutputsWithCandidates(capturedPaths, lastError, err, candidatePaths, os.Stat)
	logMediaDiagnostic(job.ID, "output_path_fallback", fmt.Sprintf("used=%t", fallbackUsed))
	if resolveErr != nil {
		if cookieTempPath != "" {
			return nil, errors.New(normalizedCookieDownloadError(lastError))
		}
		return nil, resolveErr
	}
	a.fileMu.Lock()
	files, destinationErr := moveMediaFilesToUniqueCopyPaths(folder, resolvedPaths)
	a.fileMu.Unlock()
	if destinationErr != nil {
		return nil, destinationErr
	}
	copyFlags := make([]string, len(files))
	for index, file := range files {
		copyFlags[index] = strconv.FormatBool(file.copyCreated)
	}
	if len(files) == 1 {
		logMediaDiagnostic(job.ID, "final_destination", fmt.Sprintf("copy_created=%s", copyFlags[0]))
	} else {
		logMediaDiagnostic(job.ID, "final_destination", fmt.Sprintf(
			"file_count=%d copy_created_flags=[%s]",
			len(files),
			strings.Join(copyFlags, ","),
		))
	}
	return files, nil
}

func buildMediaArgs(job Job, stagingFolder, denoPath, ffmpegDir, cookieTempPath string) []string {
	args := []string{"--no-playlist", "--no-overwrites", "--newline", "--no-warnings", "--progress-template", "download:%(progress._percent_str)s|%(progress._speed_str)s|%(progress._eta_str)s", "--print", "after_move:OF_FILE:%(filepath)s", "-P", stagingFolder, "-o", "%(title).150B [%(id)s].%(ext)s"}
	if denoPath != "" {
		args = append(args, "--js-runtimes", "deno:"+denoPath)
	}
	if ffmpegDir != "" {
		args = append(args, "--ffmpeg-location", ffmpegDir)
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
	return cookieArgs(args, cookieTempPath)
}
