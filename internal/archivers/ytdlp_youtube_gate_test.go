package archivers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arker/internal/utils"
)

// installLoginGatedYtDlp fakes yt-dlp for a video YouTube shows logged-out
// viewers only behind a sign-in gate: every run fails with failure unless the
// embedded player client is requested (or embedWorks is false, when it fails
// too). Runs that succeed write the fixture info JSON next to -o, like the
// real tool. Every invocation's arguments are appended to calls.
func installLoginGatedYtDlp(t *testing.T, failure string, embedWorks bool) (calls string) {
	t.Helper()
	bin := t.TempDir()
	calls = filepath.Join(bin, "calls.log")
	fixture, err := filepath.Abs("testdata/ytdlp_info.json")
	if err != nil {
		t.Fatal(err)
	}
	embedCase := ""
	if embedWorks {
		embedCase = `  *"player_client=web_embedded"*) ;;`
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
case " $* " in
  *" --version "*) echo 2026.10.21; exit 0;;
%s
  *) echo %q >&2; exit 1;;
esac
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
if [ -n "$out" ]; then
  base=${out%%.%%(ext)s}
  cp %q "$base.info.json"
  printf 'video' > "$base.mp4"
else
  printf 'Gated video\n50\nuploader\nen\n'
fi
exit 0
`, calls, embedCase, failure, fixture)
	if err := os.WriteFile(filepath.Join(bin, "yt-dlp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

const youtubeSignInGate = "ERROR: [youtube] 3gAHSy1L-Q8: Please sign in. This helps protect our community. Use --cookies-from-browser or --cookies for the authentication."

func readCalls(t *testing.T, calls string) []string {
	t.Helper()
	raw, _ := os.ReadFile(calls)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l != "" && !strings.Contains(l, "--version") {
			out = append(out, l)
		}
	}
	return out
}

func refreshMetadata(url string, log *bytes.Buffer) (Result, error) {
	a := &YtDlpArchiver{}
	return a.RefreshVideoMetadata(context.Background(), url, log, VideoMedia{Extension: ".mp4", ContentType: "video/mp4", SizeBytes: 1})
}

// A public YouTube video behind the logged-out sign-in gate must be fetched
// through the embedded player instead of failing over to a paid fallback, and
// the download must use the same client the successful probe did.
func TestYtDlpArchiveRetriesYouTubeLoginGateViaEmbeddedPlayer(t *testing.T) {
	calls := installLoginGatedYtDlp(t, youtubeSignInGate, true)

	var log bytes.Buffer
	res, err := refreshMetadata("https://www.youtube.com/watch?v=3gAHSy1L-Q8", &log)
	if err != nil {
		t.Fatalf("archive failed despite the embedded player working: %v\nlog:\n%s", err, log.String())
	}
	if res.Metadata == nil {
		t.Errorf("expected metadata from the embedded-player run; got %+v", res)
	}
	lines := readCalls(t, calls)
	if len(lines) != 3 {
		t.Fatalf("calls = %d, want probe, embedded probe, download:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if strings.Contains(lines[0], "player_client") {
		t.Errorf("first probe already asked for the embedded player: %s", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.Contains(l, "--extractor-args youtube:player_client=web_embedded") {
			t.Errorf("run is missing the embedded player client: %s", l)
		}
	}
	if !strings.Contains(log.String(), "continuing via the embedded player client") {
		t.Errorf("log does not record the embedded-player retry:\n%s", log.String())
	}
}

// The gate retry runs after the anonymous retry, looks at both failures, and
// keeps the download anonymous.
func TestYtDlpArchiveYouTubeLoginGateWithCookiesStaysAnonymous(t *testing.T) {
	calls := installLoginGatedYtDlp(t, youtubeSignInGate, true)
	jar := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(jar, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := utils.InitYtDlpCookies(jar, "", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = utils.InitYtDlpCookies("", "", "") })

	var log bytes.Buffer
	if _, err := refreshMetadata("https://youtu.be/qyKpKty1v_c", &log); err != nil {
		t.Fatalf("archive failed: %v\nlog:\n%s", err, log.String())
	}
	lines := readCalls(t, calls)
	if len(lines) != 4 {
		t.Fatalf("calls = %d, want cookied probe, anonymous probe, embedded probe, download:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "--cookies") {
		t.Errorf("first probe did not carry cookies: %s", lines[0])
	}
	for _, l := range lines[1:] {
		if strings.Contains(l, "--cookies") {
			t.Errorf("run after the cookied probe still carried cookies: %s", l)
		}
	}
}

// Failures that are not the sign-in gate, and sign-in walls on other sites,
// are not retried through the YouTube client.
func TestYtDlpArchiveDoesNotRetryOtherFailuresViaEmbeddedPlayer(t *testing.T) {
	cases := []struct {
		name, url, failure string
	}{
		{"youtube removed", "https://www.youtube.com/watch?v=3gAHSy1L-Q8", "ERROR: [youtube] 3gAHSy1L-Q8: Video unavailable. This video has been removed by the uploader"},
		{"instagram login", "https://www.instagram.com/reel/DdJVK8WNOk7/", "ERROR: [Instagram] DdJVK8WNOk7: Requested content is not available, rate-limit reached or login required. Use --cookies to sign in"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := installLoginGatedYtDlp(t, tc.failure, true)
			var log bytes.Buffer
			if _, err := refreshMetadata(tc.url, &log); err == nil {
				t.Fatal("archive succeeded; want the original failure")
			}
			if lines := readCalls(t, calls); len(lines) != 1 {
				t.Errorf("calls = %d, want only the original probe:\n%s", len(lines), strings.Join(lines, "\n"))
			}
		})
	}
}

// When the embedded player is gated too, the original access error is
// returned unchanged so the paid fallback still gets its chance.
func TestYtDlpArchiveKeepsOriginalErrorWhenEmbeddedPlayerFails(t *testing.T) {
	calls := installLoginGatedYtDlp(t, youtubeSignInGate, false)
	var log bytes.Buffer
	_, err := refreshMetadata("https://www.youtube.com/watch?v=3gAHSy1L-Q8", &log)
	if err == nil || !strings.Contains(err.Error(), "yt-dlp cannot access video") {
		t.Fatalf("err = %v, want the original access failure", err)
	}
	if lines := readCalls(t, calls); len(lines) != 2 {
		t.Errorf("calls = %d, want probe and embedded probe:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// Live check against two public videos YouTube gated for logged-out viewers
// (2026-10-26). Downloads them; skipped unless ARKER_LIVE_YTDLP_TEST is set.
func TestYtDlpArchiveYouTubeLoginGateLive(t *testing.T) {
	if os.Getenv("ARKER_LIVE_YTDLP_TEST") == "" {
		t.Skip("set ARKER_LIVE_YTDLP_TEST=1 to run the live yt-dlp login-gate test")
	}
	for _, url := range []string{
		"https://www.youtube.com/watch?v=3gAHSy1L-Q8",
		"https://www.youtube.com/watch?v=qyKpKty1v_c",
	} {
		t.Run(url, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			var log bytes.Buffer
			res, err := (&YtDlpArchiver{}).Archive(ctx, url, &log, nil, 0)
			if err != nil {
				t.Fatalf("Archive: %v\nlog:\n%s", err, log.String())
			}
			if res.Data == nil || res.Metadata == nil {
				t.Fatalf("missing media or metadata: %+v", res)
			}
			n, _ := io.Copy(io.Discard, res.Data)
			if c, ok := res.Data.(io.Closer); ok {
				_ = c.Close()
			}
			if n == 0 {
				t.Fatal("archived video is empty")
			}
			if !strings.Contains(log.String(), "continuing via the embedded player client") {
				t.Logf("embedded player retry not needed this time:\n%s", log.String())
			}
			t.Logf("%s: %d bytes, metadata %s", url, n, res.Metadata.Data)
		})
	}
}
