package utils

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestEstimateYtDlpTimeoutScalesWithDuration(t *testing.T) {
	duration := 2499 * time.Second
	got := EstimateYtDlpTimeout(duration)
	want := 72*time.Minute + 28*time.Second + 500*time.Millisecond

	if got != want {
		t.Fatalf("EstimateYtDlpTimeout(%s) = %s, want %s", duration, got, want)
	}
}

func TestEstimateYtDlpTimeoutClampsShortVideos(t *testing.T) {
	got := EstimateYtDlpTimeout(30 * time.Second)
	want := 15 * time.Minute

	if got != want {
		t.Fatalf("EstimateYtDlpTimeout(30s) = %s, want %s", got, want)
	}
}

func TestEstimateYtDlpTimeoutClampsVeryLongVideos(t *testing.T) {
	got := EstimateYtDlpTimeout(6 * time.Hour)
	want := 3 * time.Hour

	if got != want {
		t.Fatalf("EstimateYtDlpTimeout(6h) = %s, want %s", got, want)
	}
}

func TestEstimateYtDlpTimeoutFallsBackWhenDurationUnknown(t *testing.T) {
	got := EstimateYtDlpTimeout(0)
	want := DefaultTimeoutConfig().YtDlpTimeout

	if got != want {
		t.Fatalf("EstimateYtDlpTimeout(0) = %s, want %s", got, want)
	}
}

func TestTimeoutForArchiveJobUsesProbedYoutubeDuration(t *testing.T) {
	var logs bytes.Buffer
	duration := 2499 * time.Second

	got := timeoutForArchiveJob(context.Background(), "youtube", "https://www.youtube.com/watch?v=8gaSCj1-6ck", &logs, func(context.Context, string) (time.Duration, error) {
		return duration, nil
	})
	want := EstimateYtDlpTimeout(duration)

	if got != want {
		t.Fatalf("timeoutForArchiveJob(youtube) = %s, want %s", got, want)
	}
	if !bytes.Contains(logs.Bytes(), []byte("Detected video duration 41m39s, using dynamic timeout 1h12m29s")) {
		t.Fatalf("timeoutForArchiveJob did not log dynamic timeout decision: %q", logs.String())
	}
}

func TestTimeoutForArchiveJobFallsBackWhenYoutubeProbeFails(t *testing.T) {
	var logs bytes.Buffer

	got := timeoutForArchiveJob(context.Background(), "youtube", "https://www.youtube.com/watch?v=8gaSCj1-6ck", &logs, func(context.Context, string) (time.Duration, error) {
		return 0, errors.New("probe failed")
	})
	want := DefaultTimeoutConfig().YtDlpTimeout

	if got != want {
		t.Fatalf("timeoutForArchiveJob(youtube probe failure) = %s, want %s", got, want)
	}
	if !bytes.Contains(logs.Bytes(), []byte("Could not determine video duration, using fallback timeout 1h0m0s")) {
		t.Fatalf("timeoutForArchiveJob did not log fallback timeout decision: %q", logs.String())
	}
}

func TestTimeoutForArchiveJobDoesNotProbeNonYoutubeJobs(t *testing.T) {
	probed := false

	got := timeoutForArchiveJob(context.Background(), "git", "https://github.com/hackclub/dns", nil, func(context.Context, string) (time.Duration, error) {
		probed = true
		return 0, nil
	})
	want := DefaultTimeoutConfig().GitCloneTimeout

	if got != want {
		t.Fatalf("timeoutForArchiveJob(git) = %s, want %s", got, want)
	}
	if probed {
		t.Fatal("timeoutForArchiveJob probed duration for non-youtube job")
	}
}

func TestParseYtDlpDuration(t *testing.T) {
	got, err := parseYtDlpDuration([]byte("\n2499.5\n"))
	if err != nil {
		t.Fatalf("parseYtDlpDuration returned error: %v", err)
	}
	want := 2499500 * time.Millisecond
	if got != want {
		t.Fatalf("parseYtDlpDuration = %s, want %s", got, want)
	}
}

func TestParseYtDlpDurationRejectsInvalidOutput(t *testing.T) {
	_, err := parseYtDlpDuration([]byte("NA\n"))
	if err == nil {
		t.Fatal("parseYtDlpDuration accepted invalid output")
	}
}
