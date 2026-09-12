package archivers

import (
	"errors"
	"testing"
)

func TestClassifyYtDlpFailure(t *testing.T) {
	base := errors.New("yt-dlp cannot access video: exit status 1")
	cases := []struct {
		name, output string
		unavailable  bool
	}{
		{"dead livestream", "ERROR: [youtube] aTe_x3MWhbw: This live stream recording is not available.", true},
		{"upcoming live", "ERROR: [youtube] 15FGNbtNbj8: This live event will begin in a few moments.", true},
		{"removed", "ERROR: [youtube] abc: Video unavailable", true},
		{"instagram 400", "ERROR: [Instagram] DPjQ_rxE0Sh: Video info extraction failed: HTTP Error 400: Bad Request", false},
		{"bot check", "ERROR: [youtube] abc: Sign in to confirm you're not a bot", false},
		{"marker without error line", "WARNING: Video unavailable in some regions", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyYtDlpFailure(base, tc.output)
			if errors.Is(got, ErrContentUnavailable) != tc.unavailable {
				t.Fatalf("unavailable = %v, want %v (%v)", !tc.unavailable, tc.unavailable, got)
			}
			if !errors.Is(got, base) && got != base && !tc.unavailable {
				t.Fatalf("original error not preserved: %v", got)
			}
		})
	}
}

func TestContentGone(t *testing.T) {
	cases := map[string]bool{
		"content unavailable at the source: Video unavailable: yt-dlp cannot access video":              true,
		"content unavailable at the source: This video has been removed: exit status 1":                 true,
		"content unavailable at the source: This live stream recording is not available: exit status 1": true,
		"content unavailable at the source: This live event will begin in: exit status 1":               false,
		"content unavailable at the source: This live event has ended: exit status 1":                   false,
		"content unavailable at the source: Private video: exit status 1":                               false,
		"Video unavailable without the classification prefix":                                           false,
		"yt-dlp cannot access video: exit status 1":                                                     false,
	}
	for reason, want := range cases {
		if got := ContentGone(reason); got != want {
			t.Errorf("ContentGone(%q) = %v, want %v", reason, got, want)
		}
	}
}
