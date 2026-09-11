package archivers

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"arker/internal/utils"
)

// installCookieRejectingYtDlp fakes yt-dlp with a script that fails whenever
// --cookies is passed (the shape of a throttled or stale Instagram session,
// which answers HTTP 400 to every authenticated request) and succeeds
// anonymously, writing the fixture info JSON next to the -o template. Every
// invocation's arguments are appended to calls so the test can assert which
// runs carried cookies.
func installCookieRejectingYtDlp(t *testing.T) (calls string) {
	t.Helper()
	bin := t.TempDir()
	calls = filepath.Join(bin, "calls.log")
	fixture, err := filepath.Abs("testdata/ytdlp_info.json")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
case " $* " in
  *" --version "*) echo 2026.09.11; exit 0;;
  *" --cookies "*) echo "ERROR: [Instagram] X: Video info extraction failed: HTTP Error 400: Bad Request" >&2; exit 1;;
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
else
  printf 'Video by starthackclub\nNA\nstarthackclub\nen\n'
fi
exit 0
`, calls, fixture)
	if err := os.WriteFile(filepath.Join(bin, "yt-dlp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// When the configured session is rejected but the post is public, the run must
// fall back to anonymous access rather than fail (and then burn a paid
// fallback). The download run must be anonymous too — the whole point is that
// the cookies are what the platform is refusing.
func TestYtDlpArchiveRetriesAnonymouslyWhenCookiesAreRejected(t *testing.T) {
	calls := installCookieRejectingYtDlp(t)
	jar := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(jar, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := utils.InitYtDlpCookies(jar, "", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = utils.InitYtDlpCookies("", "", "") })

	var log bytes.Buffer
	a := &YtDlpArchiver{}
	res, err := a.RefreshVideoMetadata(context.Background(), "https://www.instagram.com/reel/DdJVK8WNOk7/", &log, VideoMedia{Extension: ".mp4", ContentType: "video/mp4", SizeBytes: 1})
	if err != nil {
		t.Fatalf("archive failed despite anonymous access working: %v\nlog:\n%s", err, log.String())
	}
	if len(res.Extras) == 0 && res.Metadata == nil {
		t.Errorf("expected metadata artifacts from the anonymous run; got %+v", res)
	}
	raw, _ := os.ReadFile(calls)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var probes, downloads, cookied int
	for _, l := range lines {
		switch {
		case strings.Contains(l, "--version"):
		case strings.Contains(l, "--print"):
			probes++
			if strings.Contains(l, "--cookies") {
				cookied++
			}
		default:
			downloads++
			if strings.Contains(l, "--cookies") {
				t.Errorf("download run still carried cookies: %s", l)
			}
		}
	}
	if probes != 2 || cookied != 1 || downloads != 1 {
		t.Errorf("calls = %d probes (%d with cookies), %d downloads; want 2/1/1:\n%s", probes, cookied, downloads, raw)
	}
	if !strings.Contains(log.String(), "Anonymous access works") {
		t.Errorf("log does not record the anonymous retry:\n%s", log.String())
	}
}
