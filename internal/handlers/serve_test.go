package handlers

import "testing"

func TestContentTypeForArchiveUsesYoutubeExtension(t *testing.T) {
	tests := []struct {
		name       string
		extension  string
		wantType   string
		wantAttach bool
	}{
		{
			name:      "mp4",
			extension: ".mp4",
			wantType:  "video/mp4",
		},
		{
			name:      "webm",
			extension: ".webm",
			wantType:  "video/webm",
		},
		{
			name:      "uppercase webm",
			extension: ".WEBM",
			wantType:  "video/webm",
		},
		{
			name:      "unknown video extension defaults to mp4",
			extension: "",
			wantType:  "video/mp4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotAttach := contentTypeForArchive("youtube", tt.extension)
			if gotType != tt.wantType {
				t.Fatalf("contentTypeForArchive(youtube, %q) type = %q, want %q", tt.extension, gotType, tt.wantType)
			}
			if gotAttach != tt.wantAttach {
				t.Fatalf("contentTypeForArchive(youtube, %q) attach = %t, want %t", tt.extension, gotAttach, tt.wantAttach)
			}
		})
	}
}

func TestContentTypeForArchiveKeepsDownloadsAttached(t *testing.T) {
	tests := []struct {
		typ      string
		wantType string
	}{
		{"mhtml", "multipart/related"},
		{"git", "application/x-tar"},
		{"itch", "application/zip"},
		{"unknown", "application/octet-stream"},
	}

	for _, tt := range tests {
		gotType, gotAttach := contentTypeForArchive(tt.typ, "")
		if gotType != tt.wantType {
			t.Fatalf("contentTypeForArchive(%q) type = %q, want %q", tt.typ, gotType, tt.wantType)
		}
		if !gotAttach {
			t.Fatalf("contentTypeForArchive(%q) attach = false, want true", tt.typ)
		}
	}
}
