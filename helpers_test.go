package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckURL(t *testing.T) {
	cases := []struct {
		url  string
		good bool
	}{
		{"https://example.org/video", true},
		{"http://example.org/file.zip", true},
		{"ftp://example.org/file.zip", false},
		{"javascript:alert(1)", false},
		{"https://name:pass@example.org/a", false},
		{"sem protocolo", false},
	}
	for _, c := range cases {
		_, err := checkURL(c.url)
		if (err == nil) != c.good {
			t.Errorf("checkURL(%q) gerou %v, esperado good=%v", c.url, err, c.good)
		}
	}
}

func TestSafeName(t *testing.T) {
	for _, name := range []string{"../../abc.exe", "a\\b/c.txt", "CON", "a|b"} {
		got := safeName(name)
		if got == "" || strings.ContainsAny(got, "/\\:|?") {
			t.Fatalf("nome incorreto: %q -> %q", name, got)
		}
	}
}

func TestUniquePath(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "file.mp3")
	if err := os.WriteFile(base, []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	got := uniquePath(dir, "file.mp3")
	if got == base {
		t.Fatalf("sobrescreveria o arquivo anterior: %q", got)
	}
}
