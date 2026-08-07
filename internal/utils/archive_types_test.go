package utils

import "testing"

func TestNormalizeArchiveType(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"legacy youtube maps to yt-dlp", "youtube", ArchiveTypeYtDlp},
		{"legacy name is case-insensitive", "YouTube", ArchiveTypeYtDlp},
		{"legacy name tolerates whitespace", " youtube ", ArchiveTypeYtDlp},
		{"canonical yt-dlp is unchanged", "yt-dlp", ArchiveTypeYtDlp},
		{"gallery-dl is unchanged", "gallery-dl", ArchiveTypeGalleryDl},
		{"mhtml is unchanged", "mhtml", ArchiveTypeMHTML},
		{"unknown values pass through for the caller to reject", "bogus", "bogus"},
		{"empty stays empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeArchiveType(tt.input); got != tt.want {
				t.Errorf("NormalizeArchiveType(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeArchiveTypesDropsCollapsedDuplicates(t *testing.T) {
	got := NormalizeArchiveTypes([]string{"mhtml", "youtube", "yt-dlp", "screenshot"})
	want := []string{"mhtml", ArchiveTypeYtDlp, "screenshot"}

	if len(got) != len(want) {
		t.Fatalf("NormalizeArchiveTypes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NormalizeArchiveTypes = %v, want %v", got, want)
		}
	}
}

func TestNormalizeArchiveTypesPreservesNil(t *testing.T) {
	// QueueCapture distinguishes "caller asked for nothing" (derive types from
	// the URL) from "caller asked for an empty set", so nil must stay nil.
	if got := NormalizeArchiveTypes(nil); got != nil {
		t.Errorf("NormalizeArchiveTypes(nil) = %v, want nil", got)
	}
}

func TestIsValidArchiveType(t *testing.T) {
	valid := []string{"mhtml", "screenshot", "git", "yt-dlp", "gallery-dl", "itch", "youtube"}
	for _, archiveType := range valid {
		if !IsValidArchiveType(archiveType) {
			t.Errorf("IsValidArchiveType(%q) = false, want true", archiveType)
		}
	}

	invalid := []string{"", "video", "gallery", "ytdlp", "gallerydl", "bogus"}
	for _, archiveType := range invalid {
		if IsValidArchiveType(archiveType) {
			t.Errorf("IsValidArchiveType(%q) = true, want false", archiveType)
		}
	}
}

// Every legacy alias must resolve to a type the system can actually run, or the
// startup migration would rename rows onto a type with no archiver.
func TestLegacyAliasesResolveToRunnableTypes(t *testing.T) {
	for legacy, canonical := range LegacyArchiveTypeAliases() {
		if !IsValidArchiveType(canonical) {
			t.Errorf("legacy alias %q maps to unknown type %q", legacy, canonical)
		}
	}
}
