package archivers

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Live smoke test for the metadata-only refresh flow; gated behind an env var
// so CI and normal test runs never touch the network.
func TestRefreshVideoMetadataLive(t *testing.T) {
	if os.Getenv("ARKER_LIVE_YTDLP_TEST") == "" {
		t.Skip("set ARKER_LIVE_YTDLP_TEST=1 to run the live yt-dlp refresh test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	url := os.Getenv("ARKER_LIVE_YTDLP_URL")
	if url == "" {
		url = "https://www.youtube.com/watch?v=jNQXAC9IVRw"
	}
	a := &YtDlpArchiver{}
	result, err := a.RefreshVideoMetadata(ctx, url, os.Stderr, VideoMedia{
		Extension: ".mp4",
		SizeBytes: 1234567,
	})
	if err != nil {
		t.Fatalf("RefreshVideoMetadata: %v", err)
	}
	if result.Data != nil {
		t.Fatal("refresh returned media data; it must not download")
	}
	if result.Metadata == nil || result.RawMetadata == nil {
		t.Fatal("refresh returned no sidecars")
	}
	var meta VideoMetadata
	if err := json.Unmarshal(result.Metadata.Data, &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta.Title == "" || meta.Media.SizeBytes != 1234567 || meta.Media.Extension != ".mp4" {
		t.Fatalf("metadata does not describe the stored media: title=%q media=%+v", meta.Title, meta.Media)
	}
	t.Logf("title=%q views=%v thumb=%v subs=%d", meta.Title, meta.Engagement.Views, result.Thumbnail != nil, len(meta.Subtitles))
}
