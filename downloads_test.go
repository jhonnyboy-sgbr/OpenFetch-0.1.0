package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseMediaProgressPercent(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		want   float64
		wantOK bool
	}{
		{name: "integer percentage without template type marker", line: "42%|1.2MiB/s|00:10", want: 42, wantOK: true},
		{name: "decimal percentage without template type marker", line: "42.5%|1.2MiB/s|00:10", want: 42.5, wantOK: true},
		{name: "percentage with template type marker", line: "download: 42%|1.2MiB/s|00:10", want: 42, wantOK: true},
		{name: "non-progress output", line: "OF_FILE:C:\\media.mp4"},
		{name: "missing percentage", line: "NA|1.2MiB/s|00:10"},
		{name: "incomplete progress output", line: "42%|1.2MiB/s"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseMediaProgressPercent(test.line)
			if ok != test.wantOK {
				t.Fatalf("parseMediaProgressPercent() ok = %t, want %t", ok, test.wantOK)
			}
			if ok && got != test.want {
				t.Errorf("parseMediaProgressPercent() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestVideoFormatSelectorUsesCompatibleFormatsAndResolution(t *testing.T) {
	tests := []struct {
		quality string
		want    string
	}{
		{
			quality: "best",
			want:    "bv[vcodec^=avc1]+ba[acodec^=mp4a]/b[vcodec^=avc1][acodec^=mp4a][ext=mp4]",
		},
		{
			quality: "1080",
			want:    "bv[vcodec^=avc1][height<=1080]+ba[acodec^=mp4a]/b[vcodec^=avc1][acodec^=mp4a][ext=mp4][height<=1080]",
		},
		{
			quality: "720",
			want:    "bv[vcodec^=avc1][height<=720]+ba[acodec^=mp4a]/b[vcodec^=avc1][acodec^=mp4a][ext=mp4][height<=720]",
		},
		{
			quality: "480",
			want:    "bv[vcodec^=avc1][height<=480]+ba[acodec^=mp4a]/b[vcodec^=avc1][acodec^=mp4a][ext=mp4][height<=480]",
		},
	}

	for _, test := range tests {
		t.Run(test.quality, func(t *testing.T) {
			if got := videoFormatSelector(test.quality); got != test.want {
				t.Errorf("videoFormatSelector(%q) = %q, want %q", test.quality, got, test.want)
			}
			args := buildMediaArgs(Job{Kind: "video", Quality: test.quality}, "staging", "", "", "")
			if !containsArgument(args, "--progress") {
				t.Errorf("video args do not force yt-dlp progress output: %v", args)
			}
			formatIndex := -1
			mergeIndex := -1
			for index, arg := range args {
				if arg == "-f" {
					formatIndex = index
				}
				if arg == "--merge-output-format" {
					mergeIndex = index
				}
			}
			if formatIndex < 0 || formatIndex+1 >= len(args) || args[formatIndex+1] != test.want {
				t.Errorf("video args do not use the expected selector: %v", args)
			}
			if mergeIndex < 0 || mergeIndex+1 >= len(args) || args[mergeIndex+1] != "mp4" {
				t.Errorf("video args do not preserve MP4 output: %v", args)
			}
		})
	}
}

func TestUnavailableVideoFormatErrorIsClearAndScoped(t *testing.T) {
	err := unavailableVideoFormatError(
		Job{Kind: "video", Quality: "720"},
		"ERROR: Requested format is not available. Use --list-formats for a list of available formats",
	)
	if err == nil {
		t.Fatal("unavailable compatible video formats did not produce a user-facing error")
	}
	for _, want := range []string{"H.264/AVC", "AAC", "reduzir o limite de resolução"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	if got := unavailableVideoFormatError(Job{Kind: "audio"}, "Requested format is not available"); got != nil {
		t.Errorf("audio format error was rewritten as video compatibility error: %v", got)
	}
	if got := unavailableVideoFormatError(Job{Kind: "video"}, "network timeout"); got != nil {
		t.Errorf("unrelated download error was rewritten as format error: %v", got)
	}
}

func TestBuildMediaArgsKeepsAudioFormatAndCookies(t *testing.T) {
	args := buildMediaArgs(Job{Kind: "audio"}, "staging", "", "", "temporary-cookie-file")
	for _, want := range []string{"--progress", "-x", "--audio-format", "mp3", "--audio-quality", "0", "--cookies", "temporary-cookie-file"} {
		if !containsArgument(args, want) {
			t.Errorf("audio arguments %v do not contain %q", args, want)
		}
	}
	if containsArgument(args, "--merge-output-format") || containsArgument(args, "-f") {
		t.Errorf("audio arguments unexpectedly contain video format selection: %v", args)
	}
}

func TestDownloadHTTPFileFlowUnaffectedByVideoFormatSelection(t *testing.T) {
	content := []byte("controlled local file response")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		_, _ = w.Write(content)
	}))
	defer server.Close()

	folder := t.TempDir()
	app := &App{configPath: filepath.Join(t.TempDir(), "state.json")}
	job := Job{ID: "file-job", URL: server.URL + "/sample.bin", Kind: "file"}
	output, err := app.downloadHTTP(t.Context(), job, folder)
	if err != nil {
		t.Fatalf("downloadHTTP() error = %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("could not read downloaded test file: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("downloaded file = %q, want %q", got, content)
	}
}

func TestOutputFolderSeparatesDownloadsAndConversions(t *testing.T) {
	base := t.TempDir()
	existingFile := filepath.Join(base, "old-video.mp4")
	if err := os.WriteFile(existingFile, []byte("existing file"), 0600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		job      Job
		category string
	}{
		{name: "video download", job: Job{Kind: "video"}, category: "Vídeos"},
		{name: "audio download", job: Job{Kind: "audio"}, category: "Áudios"},
		{name: "regular file download", job: Job{Kind: "file"}, category: "Arquivos"},
		{name: "mp3 conversion", job: Job{Kind: "convert", Format: "MP3"}, category: "Áudios"},
		{name: "mp4 conversion", job: Job{Kind: "convert", Format: "MP4"}, category: "Vídeos"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := outputFolder(base, test.job)
			if err != nil {
				t.Fatalf("outputFolder() error = %v", err)
			}
			want := filepath.Join(base, test.category)
			if got != want {
				t.Errorf("outputFolder() = %q, want %q", got, want)
			}
			info, err := os.Stat(got)
			if err != nil {
				t.Fatalf("output folder was not created: %v", err)
			}
			if !info.IsDir() {
				t.Errorf("output path %q is not a directory", got)
			}
		})
	}

	if _, err := os.Stat(existingFile); err != nil {
		t.Errorf("existing file was moved or removed: %v", err)
	}
}

func TestOutputFolderRejectsUnknownKindsAndConversionFormats(t *testing.T) {
	for _, job := range []Job{
		{Kind: "unknown"},
		{Kind: "convert", Format: "AVI"},
	} {
		if folder, err := outputFolder(t.TempDir(), job); err == nil {
			t.Errorf("outputFolder(%+v) = %q, want error", job, folder)
		}
	}
}

func TestResolveMediaOutput(t *testing.T) {
	existingPath := filepath.Join(t.TempDir(), "media.mp4")
	if err := os.WriteFile(existingPath, []byte("test media"), 0600); err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(t.TempDir(), "empty.mp4")
	if err := os.WriteFile(emptyPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	directoryPath := t.TempDir()

	tests := []struct {
		name        string
		outputLine  string
		lastError   string
		commandErr  error
		wantPath    string
		wantErrText string
	}{
		{
			name:       "successful process with existing output path",
			outputLine: "OF_FILE:" + existingPath,
			wantPath:   existingPath,
		},
		{
			name:        "failed process with existing output path",
			outputLine:  "OF_FILE:" + existingPath,
			lastError:   "ERROR: controlled yt-dlp failure",
			commandErr:  errors.New("exit status 1"),
			wantErrText: "ERROR: controlled yt-dlp failure",
		},
		{
			name:        "empty output path is rejected",
			wantErrText: "não foi possível confirmar o arquivo de saída",
		},
		{
			name:        "reported output path does not exist",
			outputLine:  "OF_FILE:" + filepath.Join(t.TempDir(), "missing.mp4"),
			wantErrText: "arquivo final não foi encontrado",
		},
		{
			name:       "existing empty file is accepted",
			outputLine: "OF_FILE:" + emptyPath,
			wantPath:   emptyPath,
		},
		{
			name:       "existing directory is accepted",
			outputLine: "OF_FILE:" + directoryPath,
			wantPath:   directoryPath,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outputPath := ""
			if path, ok := parseMediaOutputPath(test.outputLine); ok {
				outputPath = path
			}

			gotPath, err := resolveMediaOutput(outputPath, test.lastError, test.commandErr)
			if test.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrText) {
					t.Fatalf("resolveMediaOutput() error = %v, want it to contain %q", err, test.wantErrText)
				}
				if gotPath != "" {
					t.Errorf("resolveMediaOutput() path = %q, want empty on failure", gotPath)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveMediaOutput() error = %v, want nil", err)
			}
			if gotPath != test.wantPath {
				t.Errorf("resolveMediaOutput() path = %q, want %q", gotPath, test.wantPath)
			}
		})
	}
}

func TestResolveMediaOutputWithCandidates(t *testing.T) {
	folder := t.TempDir()
	candidatePath := filepath.Join(folder, "candidate.mp4")
	if err := os.WriteFile(candidatePath, []byte("test media"), 0600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(folder, "reported.mp4")
	secondCandidatePath := filepath.Join(folder, "another.mp4")
	if err := os.WriteFile(secondCandidatePath, []byte("other media"), 0600); err != nil {
		t.Fatal(err)
	}
	wrongExtensionPath := filepath.Join(folder, "candidate.webm")
	if err := os.WriteFile(wrongExtensionPath, []byte("wrong extension"), 0600); err != nil {
		t.Fatal(err)
	}
	directoryCandidate := filepath.Join(folder, "directory.mp4")
	if err := os.Mkdir(directoryCandidate, 0700); err != nil {
		t.Fatal(err)
	}
	unreadableCandidateStat := func(path string) (os.FileInfo, error) {
		if path == outputPath {
			return nil, os.ErrNotExist
		}
		return nil, errors.New("controlled stat failure")
	}

	tests := []struct {
		name            string
		outputPath      string
		lastError       string
		commandErr      error
		candidates      []string
		stat            func(string) (os.FileInfo, error)
		wantPath        string
		wantFallback    bool
		wantErrContains string
	}{
		{
			name:         "uses the sole regular candidate with matching extension",
			outputPath:   outputPath,
			candidates:   []string{candidatePath},
			wantPath:     candidatePath,
			wantFallback: true,
		},
		{
			name:         "keeps captured path when it exists",
			outputPath:   candidatePath,
			candidates:   []string{secondCandidatePath},
			stat:         os.Stat,
			wantPath:     candidatePath,
			wantFallback: false,
		},
		{
			name:            "does not fallback without candidates",
			outputPath:      outputPath,
			wantErrContains: "arquivo final não foi encontrado",
		},
		{
			name:            "does not choose between multiple candidates",
			outputPath:      outputPath,
			candidates:      []string{candidatePath, secondCandidatePath},
			wantErrContains: "arquivo final não foi encontrado",
		},
		{
			name:            "rejects candidate with a different extension",
			outputPath:      outputPath,
			candidates:      []string{wrongExtensionPath},
			wantErrContains: "arquivo final não foi encontrado",
		},
		{
			name:            "rejects missing candidate",
			outputPath:      outputPath,
			candidates:      []string{filepath.Join(folder, "missing.mp4")},
			wantErrContains: "arquivo final não foi encontrado",
		},
		{
			name:            "rejects candidate that is not a regular file",
			outputPath:      outputPath,
			candidates:      []string{directoryCandidate},
			wantErrContains: "arquivo final não foi encontrado",
		},
		{
			name:            "preserves the existing error when candidate validation fails",
			outputPath:      outputPath,
			candidates:      []string{candidatePath},
			stat:            unreadableCandidateStat,
			wantErrContains: "arquivo final não foi encontrado",
		},
		{
			name:            "does not fallback after process failure",
			outputPath:      outputPath,
			commandErr:      errors.New("controlled failure"),
			candidates:      []string{candidatePath},
			wantErrContains: "yt-dlp",
		},
		{
			name:            "does not fallback when yt-dlp reports an error",
			outputPath:      outputPath,
			lastError:       "controlled failure",
			candidates:      []string{candidatePath},
			wantErrContains: "arquivo final não foi encontrado",
		},
		{
			name:            "does not fallback without a captured output path",
			candidates:      []string{candidatePath},
			wantErrContains: "não foi possível confirmar o arquivo de saída",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stat := test.stat
			if stat == nil {
				stat = os.Stat
			}
			gotPath, gotFallback, err := resolveMediaOutputWithCandidates(
				test.outputPath,
				test.lastError,
				test.commandErr,
				test.candidates,
				stat,
			)
			if test.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrContains) {
					t.Fatalf("resolveMediaOutputWithCandidates() error = %v, want it to contain %q", err, test.wantErrContains)
				}
				if gotFallback {
					t.Errorf("resolveMediaOutputWithCandidates() fallback = true on rejection")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveMediaOutputWithCandidates() error = %v, want nil", err)
			}
			if gotPath != test.wantPath {
				t.Errorf("resolveMediaOutputWithCandidates() path = %q, want %q", gotPath, test.wantPath)
			}
			if gotFallback != test.wantFallback {
				t.Errorf("resolveMediaOutputWithCandidates() fallback = %v, want %v", gotFallback, test.wantFallback)
			}
		})
	}
}

func TestResolveMediaOutputsWithCandidatesPlaylist(t *testing.T) {
	folder := t.TempDir()
	candidateMP4 := filepath.Join(folder, "candidate-video.mp4")
	candidateWebM := filepath.Join(folder, "candidate-audio.webm")
	for _, path := range []string{candidateMP4, candidateWebM} {
		if err := os.WriteFile(path, []byte("controlled media"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	captured := []capturedMediaPath{
		{path: filepath.Join(folder, "reported-video.mp4")},
		{path: filepath.Join(folder, "reported-audio.webm")},
	}
	got, fallbackUsed, err := resolveMediaOutputsWithCandidates(
		captured,
		"",
		nil,
		[]string{candidateWebM, candidateMP4},
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveMediaOutputsWithCandidates() error = %v, want nil", err)
	}
	if !fallbackUsed {
		t.Fatal("resolveMediaOutputsWithCandidates() fallback = false, want true")
	}
	if len(got) != len(captured) {
		t.Fatalf("resolved output count = %d, want %d", len(got), len(captured))
	}
	if filepath.Ext(got[0]) != ".mp4" || filepath.Ext(got[1]) != ".webm" {
		t.Errorf("resolved extensions = [%q, %q], want [.mp4, .webm]", filepath.Ext(got[0]), filepath.Ext(got[1]))
	}
	if filepath.Base(got[0]) != filepath.Base(candidateMP4) || filepath.Base(got[1]) != filepath.Base(candidateWebM) {
		t.Errorf("resolved files = [%q, %q], want the unique candidate set", filepath.Base(got[0]), filepath.Base(got[1]))
	}
}

func TestResolveMediaOutputsWithCandidatesSameExtensionPlaylist(t *testing.T) {
	folder := t.TempDir()
	candidates := []string{
		filepath.Join(folder, "first candidate.mp4"),
		filepath.Join(folder, "second candidate.mp4"),
		filepath.Join(folder, "third candidate.mp4"),
	}
	for _, candidate := range candidates {
		if err := os.WriteFile(candidate, []byte("controlled media"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	captured := []capturedMediaPath{
		{path: filepath.Join(folder, "reported-first.mp4")},
		{path: filepath.Join(folder, "reported-second.mp4")},
		{path: filepath.Join(folder, "reported-third.mp4")},
	}

	got, fallbackUsed, err := resolveMediaOutputsWithCandidates(captured, "", nil, candidates, os.Stat)
	if err != nil {
		t.Fatalf("resolveMediaOutputsWithCandidates() error = %v, want nil", err)
	}
	if !fallbackUsed || len(got) != len(captured) {
		t.Fatalf("fallback=%v resolved count=%d, want true and %d files", fallbackUsed, len(got), len(captured))
	}
	resolvedNames := make(map[string]struct{}, len(got))
	for _, path := range got {
		if filepath.Ext(path) != ".mp4" {
			t.Errorf("resolved extension = %q, want .mp4", filepath.Ext(path))
		}
		resolvedNames[filepath.Base(path)] = struct{}{}
	}
	for _, path := range candidates {
		if _, ok := resolvedNames[filepath.Base(path)]; !ok {
			t.Errorf("candidate %q missing from validated result set", filepath.Base(path))
		}
	}
}

func TestResolveMediaOutputsWithCandidatesKeepsExistingPaths(t *testing.T) {
	folder := t.TempDir()
	paths := []string{
		filepath.Join(folder, "first.mp4"),
		filepath.Join(folder, "second.mp4"),
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("controlled media"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	captured := []capturedMediaPath{{path: paths[0]}, {path: paths[1]}}

	got, fallbackUsed, err := resolveMediaOutputsWithCandidates(captured, "", nil, nil, os.Stat)
	if err != nil {
		t.Fatalf("resolveMediaOutputsWithCandidates() error = %v, want nil", err)
	}
	if fallbackUsed {
		t.Error("resolveMediaOutputsWithCandidates() fallback = true, want false")
	}
	if len(got) != len(paths) || got[0] != paths[0] || got[1] != paths[1] {
		t.Errorf("resolved paths = %v, want %v", got, paths)
	}
}

func TestResolveMediaOutputsWithCandidatesRejectsIncompletePlaylists(t *testing.T) {
	folder := t.TempDir()
	candidateMP4 := filepath.Join(folder, "candidate-video.mp4")
	candidateWebM := filepath.Join(folder, "candidate-audio.webm")
	otherMP4 := filepath.Join(folder, "other-video.mp4")
	thirdMP4 := filepath.Join(folder, "third-video.mp4")
	if err := os.WriteFile(candidateMP4, []byte("controlled media"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidateWebM, []byte("controlled media"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherMP4, []byte("controlled media"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thirdMP4, []byte("controlled media"), 0600); err != nil {
		t.Fatal(err)
	}
	directoryMP4 := filepath.Join(folder, "directory.mp4")
	if err := os.Mkdir(directoryMP4, 0700); err != nil {
		t.Fatal(err)
	}

	captured := []capturedMediaPath{
		{path: filepath.Join(folder, "reported-video.mp4")},
		{path: filepath.Join(folder, "reported-audio.webm")},
	}
	tests := []struct {
		name            string
		candidates      []string
		commandErr      error
		lastError       string
		wantFallback    bool
		wantErrContains string
	}{
		{
			name:            "rejects too few candidates",
			candidates:      []string{candidateMP4},
			wantErrContains: "arquivo final não foi encontrado",
		},
		{
			name:            "rejects too many candidates",
			candidates:      []string{candidateMP4, candidateWebM, thirdMP4},
			wantErrContains: "arquivo final não foi encontrado",
		},
		{
			name:            "rejects extension multiset mismatch",
			candidates:      []string{candidateMP4, otherMP4},
			wantErrContains: "arquivo final não foi encontrado",
		},
		{
			name:            "rejects non-regular candidate",
			candidates:      []string{candidateMP4, directoryMP4},
			wantErrContains: "arquivo final não foi encontrado",
		},
		{
			name:            "rejects process failure",
			candidates:      []string{candidateMP4, candidateWebM},
			commandErr:      errors.New("controlled process failure"),
			wantErrContains: "yt-dlp",
		},
		{
			name:            "rejects reported extractor error",
			candidates:      []string{candidateMP4, candidateWebM},
			lastError:       "controlled extractor failure",
			wantErrContains: "arquivo final não foi encontrado",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, fallbackUsed, err := resolveMediaOutputsWithCandidates(
				captured,
				test.lastError,
				test.commandErr,
				test.candidates,
				os.Stat,
			)
			if err == nil || !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("resolveMediaOutputsWithCandidates() error = %v, want it to contain %q", err, test.wantErrContains)
			}
			if fallbackUsed != test.wantFallback {
				t.Errorf("resolveMediaOutputsWithCandidates() fallback = %v, want %v", fallbackUsed, test.wantFallback)
			}
			if len(got) != 0 {
				t.Errorf("resolveMediaOutputsWithCandidates() paths = %v, want none on rejection", got)
			}
			for _, candidate := range test.candidates {
				if _, statErr := os.Stat(candidate); statErr != nil {
					t.Errorf("candidate changed during rejected validation: %v", statErr)
				}
			}
		})
	}
}

func TestMoveMediaFilesToUniqueCopyPaths(t *testing.T) {
	folder := t.TempDir()
	sourceFolder := filepath.Join(folder, "staging")
	destinationFolder := filepath.Join(folder, "downloads")
	if err := os.Mkdir(sourceFolder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destinationFolder, 0700); err != nil {
		t.Fatal(err)
	}

	existingPath := filepath.Join(destinationFolder, "video.mp4")
	if err := os.WriteFile(existingPath, []byte("keep existing"), 0600); err != nil {
		t.Fatal(err)
	}
	sources := []string{
		filepath.Join(sourceFolder, "video.mp4"),
		filepath.Join(sourceFolder, "other.webm"),
	}
	for _, source := range sources {
		if err := os.WriteFile(source, []byte("new media"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	files, err := moveMediaFilesToUniqueCopyPaths(destinationFolder, sources)
	if err != nil {
		t.Fatalf("moveMediaFilesToUniqueCopyPaths() error = %v", err)
	}
	if len(files) != len(sources) {
		t.Fatalf("moved file count = %d, want %d", len(files), len(sources))
	}
	if filepath.Base(files[0].path) != "video.copia(1).mp4" || !files[0].copyCreated {
		t.Errorf("first destination = %q, copyCreated=%v, want numbered copy", filepath.Base(files[0].path), files[0].copyCreated)
	}
	if filepath.Base(files[1].path) != "other.webm" || files[1].copyCreated {
		t.Errorf("second destination = %q, copyCreated=%v, want original name", filepath.Base(files[1].path), files[1].copyCreated)
	}
	content, err := os.ReadFile(existingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep existing" {
		t.Errorf("existing file content = %q, want unchanged", content)
	}
	for _, file := range files {
		content, err := os.ReadFile(file.path)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "new media" {
			t.Errorf("moved file content = %q, want new media", content)
		}
	}
}

func TestMoveMediaFilesValidatesWholeSetBeforeMoving(t *testing.T) {
	folder := t.TempDir()
	sourceFolder := filepath.Join(folder, "staging")
	destinationFolder := filepath.Join(folder, "downloads")
	if err := os.Mkdir(sourceFolder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destinationFolder, 0700); err != nil {
		t.Fatal(err)
	}
	firstSource := filepath.Join(sourceFolder, "first.mp4")
	if err := os.WriteFile(firstSource, []byte("controlled media"), 0600); err != nil {
		t.Fatal(err)
	}
	missingSource := filepath.Join(sourceFolder, "missing.mp4")

	if _, err := moveMediaFilesToUniqueCopyPaths(destinationFolder, []string{firstSource, missingSource}); err == nil {
		t.Fatal("moveMediaFilesToUniqueCopyPaths() error = nil, want missing source rejection")
	}
	if _, err := os.Stat(firstSource); err != nil {
		t.Errorf("first source was moved before full-set validation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destinationFolder, "first.mp4")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("destination exists before full-set validation, stat error = %v", err)
	}
}

func TestCompletedMediaJobsCreatesOneHistoryItemPerFile(t *testing.T) {
	parent := Job{
		ID:       "playlist-job",
		Title:    "playlist",
		Kind:     "video",
		Format:   "MP4",
		Status:   "downloading",
		Progress: 80,
		Error:    "stale error",
	}
	files := []mediaFinalFile{
		{path: filepath.Join(t.TempDir(), "first video.mp4")},
		{path: filepath.Join(t.TempDir(), "second video.mp4")},
		{path: filepath.Join(t.TempDir(), "third video.mp4")},
	}

	jobs := completedMediaJobs(parent, files)
	if len(jobs) != len(files) {
		t.Fatalf("completed job count = %d, want %d", len(jobs), len(files))
	}
	ids := make(map[string]struct{}, len(jobs))
	for index, job := range jobs {
		if _, duplicate := ids[job.ID]; duplicate {
			t.Errorf("duplicate history job ID %q", job.ID)
		}
		ids[job.ID] = struct{}{}
		if job.ID == "" || job.Title != filepath.Base(files[index].path) || job.FilePath != files[index].path {
			t.Errorf("history job %d does not preserve its file identity: %+v", index, job)
		}
		if job.Status != "completed" || job.Progress != 100 || job.Error != "" {
			t.Errorf("history job %d status fields = %+v, want completed without error", index, job)
		}
	}
	if jobs[0].ID != parent.ID {
		t.Errorf("first history job ID = %q, want original ID %q", jobs[0].ID, parent.ID)
	}
}

func TestReplaceJobWithMediaFilesPreservesHistoryOrder(t *testing.T) {
	parent := Job{ID: "playlist-job", Title: "playlist", Status: "downloading"}
	before := []Job{
		{ID: "newer-job", Title: "newer"},
		parent,
		{ID: "older-job", Title: "older"},
	}
	files := []mediaFinalFile{
		{path: filepath.Join(t.TempDir(), "first.mp4")},
		{path: filepath.Join(t.TempDir(), "second.mp4")},
	}

	got := replaceJobWithMediaFiles(before, parent.ID, files)
	if len(got) != 4 {
		t.Fatalf("history length = %d, want 4", len(got))
	}
	if got[0].ID != "newer-job" || got[3].ID != "older-job" {
		t.Errorf("surrounding history order changed: [%s, ..., %s]", got[0].ID, got[3].ID)
	}
	if got[1].FilePath != files[0].path || got[2].FilePath != files[1].path {
		t.Errorf("playlist entries = [%q, %q], want individual file paths", got[1].FilePath, got[2].FilePath)
	}
	if got[1].ID != parent.ID {
		t.Errorf("first playlist item ID = %q, want parent ID %q", got[1].ID, parent.ID)
	}
	if got[2].ID == parent.ID || got[2].ID == "" {
		t.Errorf("second playlist item ID = %q, want a distinct ID", got[2].ID)
	}
}

func TestMoveToUniqueCopyPath(t *testing.T) {
	folder := t.TempDir()
	tests := []struct {
		name     string
		filename string
		existing []string
		wantName string
		wantCopy bool
	}{
		{
			name:     "keeps original name when available",
			filename: "video.mp4",
			wantName: "video.mp4",
		},
		{
			name:     "uses copy one when original exists",
			filename: "video.mp4",
			existing: []string{"video.mp4"},
			wantName: "video.copia(1).mp4",
			wantCopy: true,
		},
		{
			name:     "increments copy number",
			filename: "video.mp4",
			existing: []string{"video.mp4", "video.copia(1).mp4"},
			wantName: "video.copia(2).mp4",
			wantCopy: true,
		},
		{
			name:     "supports names without extensions",
			filename: "audio",
			existing: []string{"audio"},
			wantName: "audio.copia(1)",
			wantCopy: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testFolder := filepath.Join(folder, strings.ReplaceAll(test.name, " ", "_"))
			if err := os.Mkdir(testFolder, 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range test.existing {
				if err := os.WriteFile(filepath.Join(testFolder, name), []byte("keep original"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			sourcePath := filepath.Join(testFolder, "staged-output")
			if err := os.WriteFile(sourcePath, []byte("new download"), 0600); err != nil {
				t.Fatal(err)
			}
			got, copyCreated, err := moveToUniqueCopyPath(testFolder, test.filename, sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			if copyCreated != test.wantCopy {
				t.Errorf("moveToUniqueCopyPath() copyCreated = %v, want %v", copyCreated, test.wantCopy)
			}
			if filepath.Base(got) != test.wantName {
				t.Errorf("moveToUniqueCopyPath() = %q, want %q", filepath.Base(got), test.wantName)
			}
			downloaded, err := os.ReadFile(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(downloaded) != "new download" {
				t.Errorf("downloaded file content = %q, want new download", downloaded)
			}
			for _, name := range test.existing {
				content, err := os.ReadFile(filepath.Join(testFolder, name))
				if err != nil {
					t.Fatal(err)
				}
				if string(content) != "keep original" {
					t.Errorf("existing file %q was modified", name)
				}
			}
		})
	}
}

func TestParseMediaOutputPath(t *testing.T) {
	pathWithSpaces := filepath.Join(t.TempDir(), "folder with spaces", "media file.mp4")
	tests := []struct {
		name     string
		line     string
		wantPath string
		wantOK   bool
	}{
		{
			name:     "preserves internal spaces in path",
			line:     "OF_FILE:" + pathWithSpaces,
			wantPath: pathWithSpaces,
			wantOK:   true,
		},
		{
			name: "does not recognize leading whitespace before marker",
			line: " OF_FILE:" + pathWithSpaces,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotPath, gotOK := parseMediaOutputPath(test.line)
			if gotOK != test.wantOK {
				t.Fatalf("parseMediaOutputPath() recognized = %v, want %v", gotOK, test.wantOK)
			}
			if gotPath != test.wantPath {
				t.Errorf("parseMediaOutputPath() path = %q, want %q", gotPath, test.wantPath)
			}
		})
	}
}

func TestParseMediaOutputPathKeepsLastRecognizedPath(t *testing.T) {
	lines := []string{
		"OF_FILE:first.mp4",
		"progress: 100%",
		" OF_FILE:ignored.mp4",
		"OF_FILE:last file.mp4",
	}
	var outputPath string
	for _, line := range lines {
		if path, ok := parseMediaOutputPath(line); ok {
			outputPath = path
		}
	}
	if outputPath != "last file.mp4" {
		t.Errorf("processed output path = %q, want %q", outputPath, "last file.mp4")
	}
}

func TestRedactMediaDiagnosticError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private media.mp4")
	err := fmt.Errorf("stat %s: https://example.invalid/file?token=secret-value cookie=session-secret Authorization: Bearer private-credential", path)

	got := redactMediaDiagnosticError(err, path)
	for _, secret := range []string{path, "secret-value", "session-secret", "private-credential", "https://"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted error %q contains sensitive value %q", got, secret)
		}
	}
	for _, expected := range []string{"[caminho redigido]", "[URL redigida]", "cookie= [segredo redigido]"} {
		if !strings.Contains(got, expected) {
			t.Errorf("redacted error %q does not contain %q", got, expected)
		}
	}
}

func TestSanitizeMediaDiagnosticAuthorization(t *testing.T) {
	got := sanitizeMediaDiagnosticMessage("ERROR: Authorization: Bearer private-credential", "")
	if strings.Contains(got, "private-credential") || !strings.Contains(got, "Authorization: [segredo redigido]") {
		t.Errorf("sanitized message %q did not safely redact authorization credentials", got)
	}
}

func TestSanitizeMediaDiagnosticFilesystemPath(t *testing.T) {
	message := `ERROR: failed at C:\Users\private-user\Videos\private.mp4`
	got := sanitizeMediaDiagnosticMessage(message, "")
	for _, secret := range []string{"C:\\Users\\private-user\\Videos\\private.mp4", "private-user"} {
		if strings.Contains(got, secret) {
			t.Errorf("sanitized message %q contains filesystem path value %q", got, secret)
		}
	}
	if !strings.Contains(got, "[caminho redigido]") {
		t.Errorf("sanitized message %q does not redact the filesystem path", got)
	}
}

func TestSanitizeMediaDiagnosticMessageBoundsAndRedacts(t *testing.T) {
	message := "ERROR: token=private-value\n" + strings.Repeat("x", maxDiagnosticErrorLength+50)

	got := sanitizeMediaDiagnosticMessage(message, "")
	if len(got) > maxDiagnosticErrorLength+len("...[truncado]") {
		t.Errorf("sanitized message length = %d, want at most %d", len(got), maxDiagnosticErrorLength+len("...[truncado]"))
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("sanitized message %q contains a line break", got)
	}
	if strings.Contains(got, "private-value") || !strings.Contains(got, "[segredo redigido]") {
		t.Errorf("sanitized message %q did not safely redact the token value", got)
	}
}

func TestCompareMediaFilesReportsAnonymousMatches(t *testing.T) {
	folder := t.TempDir()
	before, err := snapshotMediaFiles(folder)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"private video.mp4": "first controlled test data",
		"another video.mp4": "second controlled test data",
	} {
		if err := os.WriteFile(filepath.Join(folder, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	privateVideoPath := filepath.Join(folder, "private video.mp4")
	anotherVideoPath := filepath.Join(folder, "another video.mp4")
	outputs := []capturedMediaPath{
		{path: privateVideoPath, elapsedMS: 17},
		{path: filepath.Join(t.TempDir(), "private video.mp4"), elapsedMS: 32},
		{path: filepath.Join(folder, "different name.ogv"), elapsedMS: 48},
	}
	observed := map[string]int64{
		filepath.Base(privateVideoPath): 12,
		filepath.Base(anotherVideoPath): 45,
	}
	comparison, err := compareMediaFiles(folder, before, outputs, observed)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.changedCount != 2 {
		t.Errorf("changedCount = %d, want 2", comparison.changedCount)
	}
	if len(comparison.capturedMatches) != len(outputs) {
		t.Fatalf("capturedMatches length = %d, want %d", len(comparison.capturedMatches), len(outputs))
	}
	if got, want := comparison.capturedMatches[0], (mediaPathMatchCounts{
		folder: 2, fullPath: 1, name: 1, ext: 2,
		fullPathFirstSeenMS: 12, nameFirstSeenMS: 12, extFirstSeenMS: 12,
	}); got != want {
		t.Errorf("first capture matches = %+v, want %+v", got, want)
	}
	if got, want := comparison.capturedMatches[1], (mediaPathMatchCounts{
		name: 1, ext: 2,
		fullPathFirstSeenMS: -1, nameFirstSeenMS: 12, extFirstSeenMS: 12,
	}); got != want {
		t.Errorf("second capture matches = %+v, want %+v", got, want)
	}
	if got, want := comparison.capturedMatches[2], (mediaPathMatchCounts{
		folder:              2,
		fullPathFirstSeenMS: -1, nameFirstSeenMS: -1, extFirstSeenMS: -1,
	}); got != want {
		t.Errorf("third capture matches = %+v, want no matches", got)
	}
	if comparison.changedExtensions[".mp4"] != 2 {
		t.Errorf("changedExtensions = %v, want two .mp4 files", comparison.changedExtensions)
	}
	formatted := formatMediaPathMatches(outputs, comparison.capturedMatches)
	for _, privateValue := range []string{"private video", "another video", folder, filepath.Dir(outputs[1].path)} {
		if strings.Contains(formatted, privateValue) {
			t.Errorf("formatted matches %q contain private value %q", formatted, privateValue)
		}
	}
	if !strings.Contains(formatted, "capture=1 folder_match=true name_match=true extension_match=true normalized_path_match=true") ||
		!strings.Contains(formatted, "capture=2 folder_match=false name_match=true extension_match=true normalized_path_match=false") ||
		!strings.Contains(formatted, "capture=3 folder_match=true name_match=false extension_match=false normalized_path_match=false") {
		t.Errorf("formatted matches %q do not contain expected anonymous counts", formatted)
	}
}

func TestObserveMediaFilesRecordsFirstSeenTime(t *testing.T) {
	folder := t.TempDir()
	before, err := snapshotMediaFiles(folder)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now()
	stop := make(chan struct{})
	type observationResult struct {
		files map[string]int64
		err   error
	}
	result := make(chan observationResult, 1)
	go func() {
		files, observeErr := observeMediaFiles(folder, before, startedAt, stop)
		result <- observationResult{files: files, err: observeErr}
	}()

	fileName := "private local test.mp4"
	if err := os.WriteFile(filepath.Join(folder, fileName), []byte("controlled test data"), 0600); err != nil {
		close(stop)
		t.Fatal(err)
	}
	close(stop)
	observation := <-result
	if observation.err != nil {
		t.Fatal(observation.err)
	}
	firstSeenMS, ok := observation.files[fileName]
	if !ok || firstSeenMS < 0 {
		t.Errorf("first-seen time for test file = %d, present = %v; want non-negative time", firstSeenMS, ok)
	}
}

func TestDiagnosticExtensionsDoNotIncludeFileNames(t *testing.T) {
	got := diagnosticExtension(filepath.Join(t.TempDir(), "private title.OGV"))
	if got != ".ogv" {
		t.Errorf("diagnosticExtension() = %q, want %q", got, ".ogv")
	}
	if strings.Contains(got, "private") {
		t.Errorf("diagnostic extension %q contains filename content", got)
	}

	got = diagnosticExtension(filepath.Join(t.TempDir(), "private title.custom-extension"))
	if got != "[outra extensão]" {
		t.Errorf("diagnosticExtension() for unusual extension = %q, want redacted value", got)
	}
}

func TestFormatDiagnosticExtensions(t *testing.T) {
	if got, want := formatDiagnosticExtensions([]string{".mp4", ".ogv"}), "[.mp4,.ogv]"; got != want {
		t.Errorf("formatDiagnosticExtensions() = %q, want %q", got, want)
	}
	if got, want := formatDiagnosticExtensionCounts(map[string]int{".ogv": 1, ".mp4": 2}), "[.mp4:2,.ogv:1]"; got != want {
		t.Errorf("formatDiagnosticExtensionCounts() = %q, want %q", got, want)
	}
}
