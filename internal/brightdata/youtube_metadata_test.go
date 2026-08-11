package brightdata

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"arker/internal/archivers"
)

func TestBuildBrightDataYouTubeArtifactsFromFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/youtube_innertube.json")
	if err != nil {
		t.Fatal(err)
	}
	info := &youtubeMediaInfo{
		OK:               true,
		URL:              "https://rr.example.googlevideo.com/videoplayback?signature=secret-signature&ip=10.20.30.40",
		ContentLength:    1048576,
		MimeType:         `video/mp4; codecs="avc1.42001E, mp4a.40.2"`,
		QualityLabel:     "360p",
		Title:            "Never Gonna Give You Up",
		Author:           "Rick Astley",
		LengthSeconds:    212,
		ViewCount:        "1600000000",
		ShortDescription: "The official video caption.",
		ThumbnailURL:     "https://i.ytimg.com/vi/dQw4w9WgXcQ/hqdefault.jpg",
		FormatID:         "18",
		Width:            640,
		Height:           360,
		FPS:              30,
		Raw:              json.RawMessage(raw),
	}

	metadataSidecar, rawSidecar, err := buildBrightDataYouTubeArtifacts(
		info,
		"https://youtu.be/dQw4w9WgXcQ",
		"dQw4w9WgXcQ",
		1048576,
		time.Date(2026, 8, 11, 22, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("buildBrightDataYouTubeArtifacts: %v", err)
	}

	var metadata archivers.VideoMetadata
	if err := json.Unmarshal(metadataSidecar.Data, &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if metadata.Platform != "youtube" || metadata.Extractor != "youtube:innertube" || metadata.PostID != "dQw4w9WgXcQ" {
		t.Errorf("identity fields = %+v", metadata)
	}
	if metadata.ChannelID != "UCuAXFkgsw1L7xaCfnd5JJOw" || metadata.PublicationTimestamp != "2009-10-25T00:00:00Z" {
		t.Errorf("provider fields = channel %q published %q", metadata.ChannelID, metadata.PublicationTimestamp)
	}
	if metadata.Engagement.Views == nil || *metadata.Engagement.Views != 1600000000 {
		t.Errorf("views = %+v", metadata.Engagement.Views)
	}
	if metadata.Media.FormatID != "18" || metadata.Media.SizeBytes != 1048576 || metadata.Media.Width == nil || *metadata.Media.Width != 640 {
		t.Errorf("media = %+v", metadata.Media)
	}
	if metadata.Provenance != "brightdata" || metadata.Provider != "brightdata_browser_api" {
		t.Errorf("provenance = %q provider = %q", metadata.Provenance, metadata.Provider)
	}

	sanitized := string(rawSidecar.Data)
	for _, secret := range []string{"secret-signature", "10.20.30.40", "1999999999", "private-visitor-id"} {
		if strings.Contains(sanitized, secret) {
			t.Errorf("sanitized Innertube response leaked %q: %s", secret, sanitized)
		}
	}
	if !strings.Contains(sanitized, "Never Gonna Give You Up") || !strings.Contains(sanitized, "[REDACTED]") {
		t.Errorf("sanitized Innertube response lost useful data or redaction marker: %s", sanitized)
	}
}
