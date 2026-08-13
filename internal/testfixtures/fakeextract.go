package testfixtures

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fake extractor harness.
//
// Arker shells out to yt-dlp and gallery-dl with exec.CommandContext(ctx,
// "yt-dlp") — no injectable seam — so the only way to exercise the real
// archiver (its argv, its output-directory scan, its ZIP builder, its
// metadata normalizer, its partial-download branch) offline is to put a fake
// binary of that name earlier on PATH.
//
// The fakes are POSIX shell scripts, deliberately almost empty. All of the
// intelligence lives here in Go: the installer stages the exact files the
// extractor would have written into a temp directory, and the script only
// copies them into place and exits with the requested status. That keeps the
// shell portable between macOS and Linux (cp, cat, basename, sed only) and
// keeps the interesting logic somewhere it can be read and changed safely.
//
// Every installer uses t.Setenv, which means the test cannot be parallel —
// consistent with the rest of this repo, which uses no t.Parallel anywhere.

// defaultYtDlpVersion and defaultGalleryDlVersion are the versions the fakes
// report. Arker logs the extractor version on every job, so a test asserting
// on log content needs a known value.
const (
	defaultYtDlpVersion     = "2026.08.04.234419"
	defaultGalleryDlVersion = "1.32.9"
)

// YtDlpFake configures the fake yt-dlp. The zero value replays the named
// fixture as a clean success.
type YtDlpFake struct {
	// Fixture is the case name, e.g. "youtube_regular". Required unless
	// FailProbe or FailDownload is set with NoInfoJSON.
	Fixture string

	// VideoBytes is the MP4 payload written beside the info JSON. Defaults to
	// a small deterministic filler.
	VideoBytes []byte

	// NoInfoJSON makes the run write the video but not the .info.json
	// sidecar, the shape of an older yt-dlp or a partial run. Arker must
	// treat this as a failure, never as a legacy-but-fine success.
	NoInfoJSON bool

	// NoThumbnail suppresses the poster image. A missing poster is cosmetic
	// and must not fail the archive.
	NoThumbnail bool

	// NoSubtitles suppresses the subtitle tracks even when the fixture has
	// them. Most videos on most platforms have no captions at all, so this
	// is the common case, not an edge case — and it must still fulfill.
	NoSubtitles bool

	// FailProbe makes the "--print title,duration,uploader" accessibility
	// check exit non-zero, the shape of a login wall or a geo-block.
	FailProbe bool

	// FailDownload makes the download run exit non-zero after the probe
	// succeeded, the shape of a mid-download refusal.
	FailDownload bool

	// ExitCode is the status used by FailProbe/FailDownload. Defaults to 1.
	ExitCode int

	// Stderr is what the failing run prints. Defaults to a generic message.
	Stderr string

	// Version is what "--version" reports.
	Version string
}

// GalleryDlFake configures the fake gallery-dl. The zero value replays every
// slide of the named fixture as a clean success.
type GalleryDlFake struct {
	// Fixture is the case name, e.g. "instagram_carousel". Required.
	Fixture string

	// Slides caps how many slides actually land on disk. Zero means all of
	// them. Setting it below the fixture's declared count reproduces the
	// partial-carousel case: gallery-dl exits non-zero having written some
	// files, and Arker keeps the partial archive.
	Slides int

	// ExitCode is gallery-dl's exit status. It is a bitmask, not an enum:
	// 4 is "extraction or download failed", 16 "authentication required",
	// 64 "no extractor supports this URL". Zero means success.
	ExitCode int

	// NoSidecars writes the media files without their JSON sidecars, the
	// shape of a run whose raw metadata is unavailable.
	NoSidecars bool

	// Version is what "--version" reports.
	Version string
}

// InstallFakeYtDlp puts a fake yt-dlp earlier on PATH for the rest of the
// test and returns the directory holding it.
func InstallFakeYtDlp(t *testing.T, cfg YtDlpFake) string {
	t.Helper()

	stage := t.TempDir()
	outDir := filepath.Join(stage, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("create fake yt-dlp staging dir: %v", err)
	}

	version := cfg.Version
	if version == "" {
		version = defaultYtDlpVersion
	}
	writeStageFile(t, stage, "version.txt", []byte(version+"\n"))

	exitCode := cfg.ExitCode
	if exitCode == 0 {
		exitCode = 1
	}
	writeStageFile(t, stage, "exit_code", []byte(fmt.Sprintf("%d\n", exitCode)))

	stderr := cfg.Stderr
	if stderr == "" {
		stderr = "ERROR: [fixture] fake yt-dlp was configured to fail"
	}
	writeStageFile(t, stage, "error.txt", []byte(stderr+"\n"))

	// The probe prints title, duration and uploader on separate lines; the
	// duration probe prints the duration alone. Both are read from the
	// fixture so they agree with the info JSON the download writes.
	title, duration, uploader := "Fixture video", "60", "fixture-uploader"
	if cfg.Fixture != "" {
		c := Lookup(t, cfg.Fixture)
		info := c.InfoJSON(t)
		title = jsonStringField(t, info, "title")
		uploader = jsonStringField(t, info, "uploader")
		duration = jsonNumberField(t, info, "duration")
		if !cfg.NoInfoJSON {
			writeStageFile(t, outDir, "payload.info.json", info)
		}
		// yt-dlp writes "<output base><suffix>" for each subtitle track, e.g.
		// video.en.vtt, exactly like the info JSON and the poster.
		if !cfg.NoSubtitles {
			for _, track := range c.SubtitleTracks(t) {
				writeStageFile(t, outDir, "payload"+track.Suffix, track.Data)
			}
		}
	}
	writeStageFile(t, stage, "print.txt",
		[]byte(strings.Join([]string{title, duration, uploader}, "\n")+"\n"))
	writeStageFile(t, stage, "duration.txt", []byte(duration+"\n"))

	video := cfg.VideoBytes
	if video == nil {
		video = placeholderVideo(cfg.Fixture)
	}
	writeStageFile(t, outDir, "payload.mp4", video)

	if !cfg.NoThumbnail {
		writeStageFile(t, outDir, "payload.jpg", PlaceholderJPEG(t, 640, 360))
	}

	if cfg.FailProbe {
		writeStageFile(t, stage, "probe_fail", nil)
	}
	if cfg.FailDownload {
		writeStageFile(t, stage, "download_fail", nil)
	}

	return installScript(t, "yt-dlp", fmt.Sprintf(ytDlpScript, stage))
}

// InstallFakeGalleryDl puts a fake gallery-dl earlier on PATH for the rest of
// the test and returns the directory holding it.
func InstallFakeGalleryDl(t *testing.T, cfg GalleryDlFake) string {
	t.Helper()

	stage := t.TempDir()
	outDir := filepath.Join(stage, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("create fake gallery-dl staging dir: %v", err)
	}

	version := cfg.Version
	if version == "" {
		version = defaultGalleryDlVersion
	}
	writeStageFile(t, stage, "version.txt", []byte(version+"\n"))
	writeStageFile(t, stage, "exit_code", []byte(fmt.Sprintf("%d\n", cfg.ExitCode)))

	if cfg.Fixture != "" {
		slides := Lookup(t, cfg.Fixture).Sidecars(t)
		if cfg.Slides > 0 && cfg.Slides < len(slides) {
			slides = slides[:cfg.Slides]
		}
		for _, slide := range slides {
			if slide.IsVideo() {
				writeStageFile(t, outDir, slide.MediaName, placeholderVideo(slide.MediaName))
			} else {
				writeStageFile(t, outDir, slide.MediaName, PlaceholderJPEG(t, 480, 600))
			}
			if !cfg.NoSidecars {
				writeStageFile(t, outDir, slide.SidecarName, slide.Data)
			}
		}
	}

	return installScript(t, "gallery-dl", fmt.Sprintf(galleryDlScript, stage))
}

// installScript writes an executable script named after the tool into a fresh
// directory and prepends that directory to PATH for the rest of the test.
func installScript(t *testing.T, name, body string) string {
	t.Helper()
	binDir := t.TempDir()
	path := filepath.Join(binDir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	// Prepend rather than replace: the archivers still resolve real helpers
	// (sh, cp, basename) through the inherited PATH.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return binDir
}

func writeStageFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatalf("stage %s: %v", name, err)
	}
}

// placeholderVideo returns deterministic filler bytes standing in for media.
// Arker never decodes the video, only stores it and records its size, so the
// content only has to be stable and identifiable.
func placeholderVideo(seed string) []byte {
	var buf bytes.Buffer
	buf.WriteString("FIXTURE-MEDIA:" + seed + ":")
	for buf.Len() < 512 {
		buf.WriteByte(byte('a' + buf.Len()%26))
	}
	return buf.Bytes()
}

// PlaceholderJPEG returns a real, decodable JPEG. It has to be a genuine
// image, not filler: the gallery-dl archiver decodes the first still image to
// build a thumbnail, and the yt-dlp archiver decodes the poster yt-dlp wrote.
// Filler bytes would silently take the "no usable cover" branch and stop
// those paths from being tested at all.
func PlaceholderJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	// Two bands, so a crop-anchor regression is visible in the average colour
	// of the result rather than hidden behind a flat fill.
	for y := 0; y < height; y++ {
		c := color.RGBA{R: 0x20, G: 0x60, B: 0xC0, A: 0xFF}
		if y > height/2 {
			c = color.RGBA{R: 0xE0, G: 0x40, B: 0x30, A: 0xFF}
		}
		for x := 0; x < width; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode placeholder JPEG: %v", err)
	}
	return buf.Bytes()
}

func decodeField(t *testing.T, data []byte, key string) interface{} {
	t.Helper()
	var record map[string]interface{}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode fixture JSON: %v", err)
	}
	return record[key]
}

func jsonStringField(t *testing.T, data []byte, key string) string {
	t.Helper()
	value := decodeField(t, data, key)
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.ReplaceAll(text, "\n", " ")
}

func jsonNumberField(t *testing.T, data []byte, key string) string {
	t.Helper()
	value := decodeField(t, data, key)
	switch typed := value.(type) {
	case float64:
		return fmt.Sprintf("%g", typed)
	case string:
		return typed
	}
	return "0"
}

// ytDlpScript is the fake yt-dlp. It recognizes the three ways Arker invokes
// yt-dlp: --version (utils.YtDlpVersion), --print (the accessibility check in
// ytdlp.go and the duration probe in utils.ProbeYtDlpDuration), and the real
// download run, whose -o template tells it where to put the staged output.
const ytDlpScript = `#!/bin/sh
# Fake yt-dlp installed by internal/testfixtures.InstallFakeYtDlp.
STAGE='%s'

print_mode=0
skip_download=0
out_template=''
while [ $# -gt 0 ]; do
	case "$1" in
	--version)
		cat "$STAGE/version.txt"
		exit 0
		;;
	--print)
		print_mode=1
		shift
		;;
	--skip-download)
		skip_download=1
		;;
	-o)
		shift
		out_template="$1"
		;;
	esac
	shift
done

if [ "$print_mode" = 1 ]; then
	if [ -f "$STAGE/probe_fail" ]; then
		cat "$STAGE/error.txt" >&2
		exit "$(cat "$STAGE/exit_code")"
	fi
	if [ "$skip_download" = 1 ]; then
		cat "$STAGE/duration.txt"
	else
		cat "$STAGE/print.txt"
	fi
	exit 0
fi

if [ -f "$STAGE/download_fail" ]; then
	cat "$STAGE/error.txt" >&2
	exit "$(cat "$STAGE/exit_code")"
fi

# Arker passes -o "<tempbase>.%%(ext)s"; strip the template to get the base.
base=$(printf '%%s' "$out_template" | sed 's/\.%%(ext)s$//')
for staged in "$STAGE"/out/*; do
	[ -e "$staged" ] || continue
	name=$(basename "$staged")
	cp "$staged" "$base${name#payload}"
done
exit 0
`

// galleryDlScript is the fake gallery-dl. Arker gives it -D <dir> and expects
// a flat directory of numbered media files with .json sidecars beside them.
// It copies the staged slides in and exits with the configured bitmask, so a
// partial run (files present, non-zero exit) reproduces faithfully.
const galleryDlScript = `#!/bin/sh
# Fake gallery-dl installed by internal/testfixtures.InstallFakeGalleryDl.
STAGE='%s'

dest=''
while [ $# -gt 0 ]; do
	case "$1" in
	--version)
		cat "$STAGE/version.txt"
		exit 0
		;;
	-D)
		shift
		dest="$1"
		;;
	esac
	shift
done

if [ -n "$dest" ]; then
	mkdir -p "$dest"
	for staged in "$STAGE"/out/*; do
		[ -e "$staged" ] || continue
		cp "$staged" "$dest/"
	done
fi
exit "$(cat "$STAGE/exit_code")"
`
