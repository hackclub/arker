package brightdata

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"arker/internal/archivers"
	"arker/internal/models"
)

func TestRefreshStoredVideoMetadataUsesDatasetsWithoutDownloadingMedia(t *testing.T) {
	tests := []struct {
		name, fixture, targetURL, platform string
	}{
		{"instagram", "instagram_reel.json", "https://www.instagram.com/reel/DPAid-WDi67/", "instagram"},
		{"tiktok", "tiktok_post.json", tiktokVideoURL, "tiktok"},
		{"facebook", "facebook_video_post.json", facebookVideoURL, "facebook"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := loadRecords(t, test.fixture)[0]
			network := newFakeNetwork(record)
			client, db := newTestClient(t, network)
			result, err := client.RefreshStoredVideoMetadata(context.Background(), test.targetURL, io.Discard, db, 0, archivers.VideoMedia{Extension: ".mp4", SizeBytes: 987654})
			if err != nil {
				t.Fatalf("RefreshStoredVideoMetadata: %v", err)
			}
			if result.Data != nil {
				t.Fatal("metadata-only repair returned media bytes")
			}
			if result.Metadata == nil || result.RawMetadata == nil {
				t.Fatalf("missing sidecars: %+v", result)
			}
			meta := videoMetadataFromSidecar(t, result.Metadata)
			if meta.Platform != test.platform || meta.Media.SizeBytes != 987654 {
				t.Errorf("metadata platform/size = %q/%d", meta.Platform, meta.Media.SizeBytes)
			}
			for _, requested := range network.requestedURLs() {
				if !strings.HasPrefix(requested, apiBase) {
					t.Errorf("metadata-only repair downloaded media: %s", requested)
				}
			}
			var usage []models.BrightDataUsage
			if err := db.Find(&usage).Error; err != nil {
				t.Fatal(err)
			}
			if len(usage) != 1 || !usage[0].Success || usage[0].Product != "web_scraper" {
				t.Errorf("usage = %+v", usage)
			}
		})
	}
}

type youtubeMetadataPage struct {
	response string
	closed   bool
}

func (p *youtubeMetadataPage) Evaluate(string, ...any) (any, error) { return p.response, nil }
func (p *youtubeMetadataPage) Close()                               { p.closed = true }

func TestRefreshStoredYouTubeMetadataDoesNotFetchVideo(t *testing.T) {
	raw, err := os.ReadFile("testdata/youtube_innertube.json")
	if err != nil {
		t.Fatal(err)
	}
	info := youtubeMediaInfo{
		OK: true, URL: "https://video.example/signed", ContentLength: 123456,
		MimeType: "video/mp4", QualityLabel: "360p", FormatID: "18",
		Title: "Archived title", Author: "Archived channel", LengthSeconds: 212,
		Width: 640, Height: 360, FPS: 30, Raw: json.RawMessage(raw),
	}
	response, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	page := &youtubeMetadataPage{response: string(response)}
	client, db := newTestClient(t, newFakeNetwork(map[string]any{}))
	client.openBrowser = func(context.Context, string, string, io.Writer) (browserSession, error) { return page, nil }

	result, err := client.RefreshStoredVideoMetadata(context.Background(), "https://youtu.be/dQw4w9WgXcQ", io.Discard, db, 0, archivers.VideoMedia{Extension: ".mp4", SizeBytes: 555})
	if err != nil {
		t.Fatalf("RefreshStoredVideoMetadata: %v", err)
	}
	if result.Data != nil || !page.closed {
		t.Fatalf("media returned or browser left open: data=%v closed=%v", result.Data != nil, page.closed)
	}
	meta := videoMetadataFromSidecar(t, result.Metadata)
	if meta.Title != "Archived title" || meta.Media.SizeBytes != 555 {
		t.Errorf("metadata = %+v", meta)
	}
	if meta.Media.FormatID != "" || meta.Media.Width != nil || meta.Media.Height != nil {
		t.Errorf("current provider delivery facts leaked into historical media: %+v", meta.Media)
	}
	var usage models.BrightDataUsage
	if err := db.First(&usage).Error; err != nil {
		t.Fatal(err)
	}
	if !usage.Success || usage.Product != "browser_api" || usage.BytesTransferred != browserPageOverheadBytes {
		t.Errorf("usage = %+v", usage)
	}
}
