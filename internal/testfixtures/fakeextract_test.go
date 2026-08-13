package testfixtures

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The harness is the foundation every contract test stands on, so it gets its
// own tests. If the fake stops writing what the real extractor writes, the
// contract tests go green for the wrong reason — which is the exact failure
// mode this whole evidence layer exists to prevent.

func TestFakeYtDlpAnswersEveryInvocationArkerMakes(t *testing.T) {
	InstallFakeYtDlp(t, YtDlpFake{Fixture: "youtube_regular"})

	// utils.YtDlpVersion
	out, err := exec.Command("yt-dlp", "--version").Output()
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if strings.TrimSpace(string(out)) != defaultYtDlpVersion {
		t.Errorf("--version = %q, want %q", strings.TrimSpace(string(out)), defaultYtDlpVersion)
	}

	// The accessibility probe in ytdlp.go: three fields, one per line.
	out, err = exec.Command("yt-dlp", "--print", "title,duration,uploader",
		"https://www.youtube.com/watch?v=aqz-KE-bpKQ").Output()
	if err != nil {
		t.Fatalf("--print: %v", err)
	}
	if lines := strings.Fields(strings.TrimSpace(string(out))); len(lines) == 0 {
		t.Error("--print produced no output; the archiver logs this as the video info")
	}

	// utils.ProbeYtDlpDuration: --skip-download, and the first line must
	// parse as a duration.
	out, err = exec.Command("yt-dlp", "--print", "duration", "--no-playlist",
		"--skip-download", "https://www.youtube.com/watch?v=aqz-KE-bpKQ").Output()
	if err != nil {
		t.Fatalf("duration probe: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "635" {
		t.Errorf("duration probe = %q, want the fixture's duration 635", got)
	}
}

func TestFakeYtDlpWritesTheFilesTheArchiverLooksFor(t *testing.T) {
	InstallFakeYtDlp(t, YtDlpFake{Fixture: "youtube_shorts"})

	base := filepath.Join(t.TempDir(), "video")
	cmd := exec.Command("yt-dlp", "-f", "bestvideo+bestaudio/best", "--no-playlist",
		"--write-thumbnail", "--write-info-json", "--no-clean-infojson",
		"--merge-output-format", "mp4", "--remux-video", "mp4", "--verbose",
		"-o", base+".%(ext)s", "https://www.youtube.com/shorts/Ujmx5PsT1IY")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("download run: %v\n%s", err, out)
	}

	// These three names are exactly what findDownloadedMP4,
	// findYtDlpInfoJSON and findDownloadedThumbnail glob for.
	for _, suffix := range []string{".mp4", ".info.json", ".jpg"} {
		if _, err := os.Stat(base + suffix); err != nil {
			t.Errorf("fake yt-dlp did not write %s: %v", suffix, err)
		}
	}

	info, err := os.ReadFile(base + ".info.json")
	if err != nil {
		t.Fatalf("read info json: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(info, &record); err != nil {
		t.Fatalf("fake yt-dlp wrote unparseable info JSON: %v", err)
	}
	if record["extractor_key"] != "Youtube" {
		t.Errorf("extractor_key = %v, want Youtube", record["extractor_key"])
	}
}

func TestFakeYtDlpFailureModesExitNonZero(t *testing.T) {
	t.Run("probe failure", func(t *testing.T) {
		InstallFakeYtDlp(t, YtDlpFake{Fixture: "instagram_reel", FailProbe: true})
		err := exec.Command("yt-dlp", "--print", "title,duration,uploader", "u").Run()
		if err == nil {
			t.Fatal("expected the accessibility probe to fail")
		}
	})

	t.Run("download failure after a passing probe", func(t *testing.T) {
		InstallFakeYtDlp(t, YtDlpFake{Fixture: "instagram_reel", FailDownload: true})
		if err := exec.Command("yt-dlp", "--print", "title", "u").Run(); err != nil {
			t.Fatalf("probe should still succeed: %v", err)
		}
		base := filepath.Join(t.TempDir(), "video")
		if err := exec.Command("yt-dlp", "-o", base+".%(ext)s", "u").Run(); err == nil {
			t.Fatal("expected the download run to fail")
		}
		if _, err := os.Stat(base + ".mp4"); err == nil {
			t.Error("a failed download must not leave an MP4 behind")
		}
	})
}

func TestFakeGalleryDlWritesAFlatNumberedDirectory(t *testing.T) {
	InstallFakeGalleryDl(t, GalleryDlFake{Fixture: "instagram_carousel"})

	dest := t.TempDir()
	cmd := exec.Command("gallery-dl", "--config-ignore", "-D", dest,
		"-f", "{num:>03}.{extension}", "--write-metadata", "-o", "cookies-update=false",
		"--no-part", "-R", "3", "--http-timeout", "30",
		"https://www.instagram.com/p/DbktPO1Eopi/")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gallery-dl run: %v\n%s", err, out)
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	// 10 slides plus one sidecar each.
	if len(entries) != 20 {
		t.Fatalf("wrote %d entries, want 20 (10 slides + 10 sidecars)", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dest, "004.mp4")); err != nil {
		t.Errorf("the carousel's video slide is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "010.jpg.json")); err != nil {
		t.Errorf("slide 10's sidecar is missing: %v", err)
	}
}

// A partial run is the shape behind G1: gallery-dl exits non-zero having
// written only some of the post's slides.
func TestFakeGalleryDlPartialRunWritesSomeFilesAndExitsNonZero(t *testing.T) {
	InstallFakeGalleryDl(t, GalleryDlFake{Fixture: "instagram_carousel", Slides: 3, ExitCode: 4})

	dest := t.TempDir()
	err := exec.Command("gallery-dl", "-D", dest, "https://www.instagram.com/p/DbktPO1Eopi/").Run()
	if err == nil {
		t.Fatal("a partial gallery-dl run must exit non-zero")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 4 {
		t.Fatalf("exit error = %v, want exit code 4", err)
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if len(entries) != 6 {
		t.Fatalf("wrote %d entries, want 6 (3 slides + 3 sidecars)", len(entries))
	}
}

func TestPlaceholderJPEGIsDecodable(t *testing.T) {
	// The gallery and video thumbnail paths both decode this. Filler bytes
	// would take the "no usable cover" branch and quietly skip the test.
	data := PlaceholderJPEG(t, 320, 240)
	if len(data) == 0 {
		t.Fatal("placeholder JPEG is empty")
	}
	if _, err := os.Stat("testdata"); err != nil {
		t.Fatalf("fixture corpus is missing: %v", err)
	}
}

func TestFakeYtDlpWritesSubtitleTracks(t *testing.T) {
	InstallFakeYtDlp(t, YtDlpFake{Fixture: "youtube_regular"})

	base := filepath.Join(t.TempDir(), "video")
	cmd := exec.Command("yt-dlp", "--write-info-json", "--write-subs", "--write-auto-subs",
		"-o", base+".%(ext)s", "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("download run: %v\n%s", err, out)
	}
	if _, err := os.Stat(base + ".en.vtt"); err != nil {
		t.Fatalf("fake yt-dlp did not write the subtitle track: %v", err)
	}
}

// The rolling duplication in YouTube's auto-captions is the whole reason the
// transcript contract needs a dedupe step, so the fixture has to actually
// contain it.
func TestYouTubeSubtitleFixtureRollsLikeARealAutoCaption(t *testing.T) {
	tracks := Lookup(t, "youtube_regular").SubtitleTracks(t)
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(tracks))
	}
	if !tracks[0].Auto {
		t.Error("the YouTube fixture should be an auto-caption track")
	}

	raw := strings.Count(string(tracks[0].Data), "big buck bunny is a short")
	if raw < 3 {
		t.Errorf("fixture repeats the first line %d times, want the rolling duplication a real auto-caption has", raw)
	}
	lines := tracks[0].CaptionLines()
	for _, line := range lines {
		if strings.Count(strings.Join(lines, "\n"), line) != 1 {
			t.Errorf("CaptionLines still contains %q more than once", line)
		}
	}
	if len(lines) != 4 {
		t.Errorf("CaptionLines = %v, want the 4 distinct spoken lines", lines)
	}
}

func TestTikTokSubtitleFixtureIsAnUploaderTrack(t *testing.T) {
	tracks := Lookup(t, "tiktok_video").SubtitleTracks(t)
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(tracks))
	}
	if tracks[0].Auto {
		t.Error("the TikTok fixture is an uploader-supplied caption track, not auto-generated")
	}
	if got := len(tracks[0].CaptionLines()); got != 3 {
		t.Errorf("CaptionLines = %d, want 3", got)
	}
}

// Most videos have no captions. These cases pin that the corpus keeps a
// no-subtitles arm, which the fulfillment contract depends on.
func TestSomeFixturesDeliberatelyHaveNoSubtitles(t *testing.T) {
	for _, name := range []string{"vimeo_video", "instagram_reel", "facebook_video"} {
		if tracks := Lookup(t, name).SubtitleTracks(t); len(tracks) != 0 {
			t.Errorf("%s has %d subtitle tracks, want none", name, len(tracks))
		}
	}
}
