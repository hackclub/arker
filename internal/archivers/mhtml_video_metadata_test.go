package archivers

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBuildCapturedHTMLVideoArtifactsRecoversSchemaFacts(t *testing.T) {
	html := []byte(`<html><head>
<title>Fallback title - YouTube</title>
<meta property="og:title" content="Turn coding hours into rewards, no catch.">
<meta property="og:description" content="A captured description">
<meta property="og:image" content="https://images.example/post.jpg?x=1&amp;y=2">
<link rel="canonical" href="https://www.youtube.com/shorts/jAUZFBlZmiE">
<meta itemprop="datePublished" content="2026-07-15T05:55:09-07:00">
<meta itemprop="duration" content="PT1M2.5S">
<span itemprop="author"><link itemprop="name" content="Miles K"></span>
</head></html>`)
	metadataSidecar, rawSidecar, err := BuildCapturedHTMLVideoArtifacts(html, "https://youtube.com/shorts/jAUZFBlZmiE", VideoMedia{Extension: ".mp4", SizeBytes: 42}, time.Date(2026, 7, 16, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var metadata VideoMetadata
	if err := json.Unmarshal(metadataSidecar.Data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Title != "Turn coding hours into rewards, no catch." || metadata.Channel != "Miles K" {
		t.Errorf("title/channel = %q/%q", metadata.Title, metadata.Channel)
	}
	if metadata.PublicationTimestamp != "2026-07-15T12:55:09Z" {
		t.Errorf("publication timestamp = %q", metadata.PublicationTimestamp)
	}
	if metadata.DurationSeconds == nil || *metadata.DurationSeconds != 62.5 {
		t.Errorf("duration = %v", metadata.DurationSeconds)
	}
	if metadata.Provider != "captured_mhtml" || metadata.Media.SizeBytes != 42 {
		t.Errorf("provider/media = %q/%+v", metadata.Provider, metadata.Media)
	}
	if got := CapturedHTMLSocialImageURL(html); got != "https://images.example/post.jpg?x=1&y=2" {
		t.Errorf("captured social image URL = %q", got)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(rawSidecar.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["capture_source"] != "mhtml" {
		t.Errorf("raw sidecar = %+v", raw)
	}
}

func TestBuildCapturedHTMLVideoArtifactsRejectsGenericPage(t *testing.T) {
	if _, _, err := BuildCapturedHTMLVideoArtifacts([]byte(`<html><title>Log in</title></html>`), "https://instagram.com/reel/gone/", VideoMedia{}, time.Now()); err == nil {
		t.Fatal("generic page should not become video metadata")
	}
}
