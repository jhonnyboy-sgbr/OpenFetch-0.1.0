package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const cookieSecret = "cookie-value-must-not-leak"

func validCookieJar() []byte {
	return []byte("# Netscape HTTP Cookie File\n.youtube.com\tTRUE\t/\tTRUE\t1893456000\tSID\t" + cookieSecret + "\n.example.com\tTRUE\t/\tTRUE\t1893456000\tOTHER\tother-secret\n")
}

func writeValidCookieJar(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(path, validCookieJar(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCookieSelectionCancelRemoveAndInvalidSelection(t *testing.T) {
	a := &App{}
	path := writeValidCookieJar(t, t.TempDir())
	selected, err := a.applyCookiesSelection(path)
	if err != nil || !selected {
		t.Fatalf("selection = (%t, %v), want selected", selected, err)
	}
	if !a.GetState().CookiesSelected {
		t.Fatal("cookies should be selected")
	}
	selected, err = a.applyCookiesSelection("")
	if err != nil || selected {
		t.Fatalf("cancel selection = (%t, %v), want no change", selected, err)
	}
	if a.currentCookiesPath() != path {
		t.Fatal("cancelled selection changed the current selection")
	}

	invalidPath := filepath.Join(t.TempDir(), "invalid.txt")
	if err := os.WriteFile(invalidPath, []byte("not a Netscape cookie jar"), 0600); err != nil {
		t.Fatal(err)
	}
	if selected, err := a.applyCookiesSelection(invalidPath); err == nil || selected {
		t.Fatalf("invalid selection = (%t, %v), want validation error", selected, err)
	}
	if a.currentCookiesPath() != path {
		t.Fatal("invalid selection replaced the valid selection")
	}

	a.RemoveCookiesFile()
	if a.GetState().CookiesSelected || a.currentCookiesPath() != "" {
		t.Fatal("removing cookies did not clear the selection")
	}
}

func TestCookieValidationAndYouTubeDomainFiltering(t *testing.T) {
	filtered, err := filterYouTubeCookieJar(validCookieJar())
	if err != nil {
		t.Fatalf("filterYouTubeCookieJar() error = %v", err)
	}
	output := string(filtered)
	if !strings.Contains(output, cookieSecret) || !strings.Contains(output, ".youtube.com") {
		t.Fatal("filtered jar did not preserve the valid YouTube cookie")
	}
	if strings.Contains(output, "OTHER") || strings.Contains(output, "other-secret") {
		t.Fatal("filtered jar retained a non-YouTube cookie")
	}

	tests := []struct {
		name string
		data []byte
	}{
		{name: "missing fields", data: []byte(".youtube.com\tTRUE\t/\n")},
		{name: "invalid booleans", data: []byte(".youtube.com\tMAYBE\t/\tTRUE\t1\tSID\tvalue\n")},
		{name: "invalid expiry", data: []byte(".youtube.com\tTRUE\t/\tTRUE\tnever\tSID\tvalue\n")},
		{name: "no YouTube domains", data: []byte(".example.com\tTRUE\t/\tTRUE\t1\tSID\tvalue\n")},
		{name: "non-YouTube suffix", data: []byte(".notyoutube.com\tTRUE\t/\tTRUE\t1\tSID\tvalue\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := filterYouTubeCookieJar(test.data); err == nil {
				t.Fatal("filterYouTubeCookieJar() accepted invalid cookie data")
			}
		})
	}
}

func TestCookieFileMustBeRegularAndReadable(t *testing.T) {
	if _, err := readYouTubeCookies(t.TempDir()); err == nil {
		t.Fatal("directory was accepted as a cookie file")
	}
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, validCookieJar(), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := readYouTubeCookies(path)
	if err != nil {
		t.Fatalf("readYouTubeCookies() error = %v", err)
	}
	if !strings.Contains(string(data), cookieSecret) {
		t.Fatal("valid cookie data was not read")
	}
}

func TestCookieCopyIsFilteredProtectedAndCleaned(t *testing.T) {
	sourceDir := t.TempDir()
	sourcePath := writeValidCookieJar(t, sourceDir)
	original, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cookie-cache")

	for _, outcome := range []string{"success", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			tempPath, cleanup, err := createYouTubeCookieCopy(sourcePath, cacheDir)
			if err != nil {
				t.Fatalf("createYouTubeCookieCopy() error = %v", err)
			}
			if !cookieCopyPermissionsPrivate(tempPath) {
				t.Error("temporary cookie permissions are not restricted to the current user")
			}
			tempData, err := os.ReadFile(tempPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(tempData), "other-secret") {
				t.Fatal("temporary copy retained a non-YouTube cookie")
			}
			if strings.Contains(tempPath, cookieSecret) {
				t.Fatal("temporary path unexpectedly contains cookie data")
			}
			cleanup()
			if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
				t.Fatalf("temporary copy remains after %s cleanup: %v", outcome, err)
			}
		})
	}
	after, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("original cookie file was changed")
	}
}

func TestStaleCookieCopiesAreRemoved(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "openfetch-cookies-stale.txt")
	recent := filepath.Join(dir, "openfetch-cookies-recent.txt")
	unrelated := filepath.Join(dir, "keep.txt")
	for _, path := range []string{stale, recent, unrelated} {
		if err := os.WriteFile(path, []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if err := os.Chtimes(stale, now.Add(-25*time.Hour), now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleCookieCopies(dir, now); err != nil {
		t.Fatalf("cleanupStaleCookieCopies() error = %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale cookie copy was not removed")
	}
	for _, path := range []string{recent, unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("non-stale file %q was removed: %v", filepath.Base(path), err)
		}
	}
}

func TestCookieSelectionIsNotPersistedAndQueuedJobsCaptureIt(t *testing.T) {
	a := &App{configPath: filepath.Join(t.TempDir(), "state.json"), jobCookies: make(map[string]string)}
	path := writeValidCookieJar(t, t.TempDir())
	if _, err := a.applyCookiesSelection(path); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.captureCookiesForJobLocked("queued-job", "video")
	a.saveLocked()
	a.selectedCookiesPath = ""
	a.mu.Unlock()
	if got := a.selectedCookiesForJob("queued-job"); got != path {
		t.Fatal("queued job did not retain the selected cookie path")
	}
	a.releaseJobCookies("queued-job")
	if got := a.selectedCookiesForJob("queued-job"); got != "" {
		t.Fatal("released job retained a cookie path")
	}

	stateBytes, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshotBytes, err := json.Marshal(a.GetState())
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{stateBytes, snapshotBytes} {
		if strings.Contains(string(data), path) || strings.Contains(string(data), cookieSecret) {
			t.Fatal("cookie path or contents leaked into persisted or frontend state")
		}
	}
}

func TestQueuedCookieFileBecomingUnavailableFailsSafely(t *testing.T) {
	a := &App{jobCookies: make(map[string]string)}
	path := writeValidCookieJar(t, t.TempDir())
	if _, err := a.applyCookiesSelection(path); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.captureCookiesForJobLocked("queued-job", "video")
	a.mu.Unlock()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, _, err := createYouTubeCookieCopy(a.selectedCookiesForJob("queued-job"), t.TempDir())
	if err == nil || strings.Contains(err.Error(), path) || strings.Contains(err.Error(), cookieSecret) {
		t.Fatalf("unavailable cookie error was missing or exposed sensitive data: %v", err)
	}
}

func TestCookieArgumentsPreserveNoCookieFlowAndAddCookieJar(t *testing.T) {
	probeBase := []string{"--flat-playlist", "--no-warnings", "--dump-single-json", "--skip-download"}
	probeWithout := buildProbeArgs("", "")
	if strings.Join(probeWithout, "\x00") != strings.Join(probeBase, "\x00") {
		t.Fatalf("probe args without cookies changed: %v", probeWithout)
	}
	probeWith := buildProbeArgs("", "temporary-cookie-file")
	assertCookieArgument(t, probeWith, "temporary-cookie-file")

	job := Job{Kind: "video", Quality: "best"}
	mediaWithout := buildMediaArgs(job, "staging", "", "", "")
	if containsArgument(mediaWithout, "--cookies") {
		t.Fatal("download args included cookies when none were selected")
	}
	mediaWith := buildMediaArgs(job, "staging", "", "", "temporary-cookie-file")
	assertCookieArgument(t, mediaWith, "temporary-cookie-file")

	audioWith := buildMediaArgs(Job{Kind: "audio"}, "staging", "", "", "temporary-cookie-file")
	assertCookieArgument(t, audioWith, "temporary-cookie-file")
}

func assertCookieArgument(t *testing.T, args []string, path string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--cookies" && args[i+1] == path {
			return
		}
	}
	t.Fatalf("arguments did not contain --cookies with temporary path: %v", args)
}

func containsArgument(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}

func TestCookieErrorsDoNotExposeSensitiveValues(t *testing.T) {
	tempPath := filepath.Join(t.TempDir(), "openfetch-cookies-sensitive.txt")
	raw := "ERROR: Sign in to confirm you're not a bot " + tempPath + " " + cookieSecret
	message := normalizedCookieDownloadError(raw)
	if strings.Contains(message, tempPath) || strings.Contains(message, cookieSecret) {
		t.Fatal("normalized error exposed temporary path or cookie data")
	}
}
