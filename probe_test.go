package main

import "testing"

func TestParseProbeMetadataVideo(t *testing.T) {
	metadata, err := parseProbeMetadata([]byte(`{"_type":"video","title":"A video","thumbnail":"https://example.invalid/thumb.jpg","duration":42}`))
	if err != nil {
		t.Fatalf("parseProbeMetadata() error = %v", err)
	}
	if metadata.Type != "video" || metadata.Title != "A video" || metadata.Duration != 42 {
		t.Errorf("parsed video metadata = %+v", metadata)
	}
	if metadata.Entries != nil {
		t.Errorf("video entries = %v, want nil", metadata.Entries)
	}
}

func TestParseProbeMetadataPlaylist(t *testing.T) {
	metadata, err := parseProbeMetadata([]byte(`{"_type":"playlist","title":"A playlist","entries":[{"id":"first"},{"id":"second"},{"id":"third"}]}`))
	if err != nil {
		t.Fatalf("parseProbeMetadata() error = %v", err)
	}
	if metadata.Type != "playlist" || metadata.Title != "A playlist" || len(metadata.Entries) != 3 {
		t.Errorf("parsed playlist metadata = %+v", metadata)
	}
}

func TestParseProbeMetadataRejectsInvalidJSON(t *testing.T) {
	if _, err := parseProbeMetadata([]byte(`{"_type":`)); err == nil {
		t.Fatal("parseProbeMetadata() error = nil, want invalid JSON error")
	}
}

func TestIsAuthenticationRequired(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{name: "bot verification", stderr: "Sign in to confirm you're not a bot", want: true},
		{name: "authentication required", stderr: "Authentication is required", want: true},
		{name: "login required", stderr: "Login required", want: true},
		{name: "unrelated failure", stderr: "Unable to download webpage", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isAuthenticationRequired([]byte(test.stderr)); got != test.want {
				t.Errorf("isAuthenticationRequired() = %v, want %v", got, test.want)
			}
		})
	}
}
