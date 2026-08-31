package archivers

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const galleryProbeFixtureOutput = `{
  "streams": [
    {"codec_name":"h264","codec_type":"video","width":576,"height":1024,"r_frame_rate":"30/1","bit_rate":"800000"},
    {"codec_name":"aac","codec_type":"audio","r_frame_rate":"0/0","bit_rate":"96000"}
  ],
  "format": {"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"18.994000","bit_rate":"900000"}
}`

// installFileCheckingFFprobe fakes ffprobe for path-based probing. It asserts
// the final argument is a readable file (the contract ProbeVideoFile relies
// on: a real file, so ffprobe can seek to a trailing moov atom) and then
// prints the canned probe result. The fake is a local process only.
func installFileCheckingFFprobe(t *testing.T, output string) {
	t.Helper()
	bin := t.TempDir()
	response := filepath.Join(bin, "probe.json")
	if err := os.WriteFile(response, []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
for last; do :; done
[ -r "$last" ] || exit 90
cat %q
`, response)
	if err := os.WriteFile(filepath.Join(bin, "ffprobe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A gallery bundle is zipped into an immutable stored object, so archive time
// is the only moment a video slide's duration and dimensions can be read off
// the actual bytes. This pins that they are, that probe values win, and that
// stills are left alone.
func TestProbeGalleryVideoFilesReadsIntrinsicsOffTheBytes(t *testing.T) {
	installFileCheckingFFprobe(t, galleryProbeFixtureOutput)
	dir := t.TempDir()
	for _, name := range []string{"001.mp4", "002.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("media-bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files := []GalleryFile{
		// Provider said 500x500; the probe of the stored bytes must win.
		{Name: "001.mp4", IsVideo: true, ContentType: "video/mp4", Width: 500, Height: 500},
		{Name: "002.jpg", ContentType: "image/jpeg", Width: 640, Height: 480},
	}
	ProbeGalleryVideoFiles(context.Background(), dir, files, io.Discard)

	video := files[0]
	if video.DurationSeconds == nil || *video.DurationSeconds != 18.994 {
		t.Errorf("video duration_seconds = %v; want 18.994 from the probe", video.DurationSeconds)
	}
	if video.Width != 576 || video.Height != 1024 {
		t.Errorf("video dimensions = %dx%d; want the probed 576x1024", video.Width, video.Height)
	}
	still := files[1]
	if still.DurationSeconds != nil || still.Width != 640 || still.Height != 480 {
		t.Errorf("still image was modified by the video probe: %+v", still)
	}
}

// A probe failure must leave the entry as the provider described it — probing
// is an enrichment, never a reason to lose or distort a slide.
func TestProbeGalleryVideoFilesKeepsProviderFactsOnProbeFailure(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ffprobe"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001.mp4"), []byte("media-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := []GalleryFile{{Name: "001.mp4", IsVideo: true, Width: 500, Height: 500}}
	ProbeGalleryVideoFiles(context.Background(), dir, files, io.Discard)
	if files[0].DurationSeconds != nil || files[0].Width != 500 || files[0].Height != 500 {
		t.Errorf("failed probe altered provider facts: %+v", files[0])
	}
}
