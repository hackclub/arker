package archivers

import (
	"os"
	"testing"
)

// A TikTok photo post reached through its /video/ URL: yt-dlp offers only the
// soundtrack. Storing that as the post's archive is the wrong artifact.
func TestAudioOnlySlideshowReasonDetectsTikTokSlideshow(t *testing.T) {
	raw, err := os.ReadFile("testdata/ytdlp_tiktok_slideshow_info.json")
	if err != nil {
		t.Fatal(err)
	}
	if reason := audioOnlySlideshowReason(raw); reason == "" {
		t.Fatal("slideshow record read as a video")
	}
}

func TestAudioOnlySlideshowReasonLeavesVideosAlone(t *testing.T) {
	raw, err := os.ReadFile("testdata/ytdlp_info.json")
	if err != nil {
		t.Fatal(err)
	}
	if reason := audioOnlySlideshowReason(raw); reason != "" {
		t.Fatalf("video record read as a slideshow: %s", reason)
	}
	// Audio-only from another extractor is still a legitimate archive.
	other := []byte(`{"extractor_key":"SoundCloud","vcodec":"none","formats":[{"vcodec":"none"}]}`)
	if reason := audioOnlySlideshowReason(other); reason != "" {
		t.Fatalf("non-TikTok audio read as a slideshow: %s", reason)
	}
	// A TikTok record with any video stream is a video.
	video := []byte(`{"extractor_key":"TikTok","vcodec":"none","formats":[{"vcodec":"none"},{"vcodec":"h264"}]}`)
	if reason := audioOnlySlideshowReason(video); reason != "" {
		t.Fatalf("TikTok record with a video format read as a slideshow: %s", reason)
	}
	if reason := audioOnlySlideshowReason([]byte("not json")); reason != "" {
		t.Fatalf("garbage read as a slideshow: %s", reason)
	}
}
