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

func TestBuildCapturedHTMLVideoArtifactsRecoversAuthorFromCapturedHandleURL(t *testing.T) {
	htmlData := []byte(`<html><head>
<meta property="og:title" content="Motion Sensor">
<meta itemprop="duration" content="PT12S">
<span itemprop="author"><link itemprop="url" href="https://www.youtube.com/@l.a.c.l.u.s.t.r"><link itemprop="name" content=""></span>
</head></html>`)
	metadataSidecar, _, err := BuildCapturedHTMLVideoArtifacts(htmlData, "https://www.youtube.com/watch?v=fixture", VideoMedia{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var metadata VideoMetadata
	if err := json.Unmarshal(metadataSidecar.Data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Channel != "l.a.c.l.u.s.t.r" {
		t.Fatalf("channel = %q", metadata.Channel)
	}
}

func TestBuildCapturedHTMLVideoArtifactsRejectsGenericPage(t *testing.T) {
	if _, _, err := BuildCapturedHTMLVideoArtifacts([]byte(`<html><title>Log in</title></html>`), "https://instagram.com/reel/gone/", VideoMedia{}, time.Now()); err == nil {
		t.Fatal("generic page should not become video metadata")
	}
}

func TestBuildCapturedHTMLVideoArtifactsRecoversInstagramOpenGraphAttribution(t *testing.T) {
	htmlData := []byte(`<html><head>
<meta property="og:title" content="Hari | College Admissions on Instagram: &quot;Build an app&quot;">
<meta property="og:description" content="collegewith_hari on July 14, 2026: &quot;Build an app&quot;.">
<link rel="canonical" href="https://www.instagram.com/reel/DazFFu2RdSk/">
</head></html>`)
	metadataSidecar, _, err := BuildCapturedHTMLVideoArtifacts(htmlData, "https://www.instagram.com/reel/DazFFu2RdSk/", VideoMedia{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var metadata VideoMetadata
	if err := json.Unmarshal(metadataSidecar.Data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Channel != "collegewith_hari" || metadata.PublicationTimestamp != "2026-07-14T00:00:00Z" {
		t.Fatalf("Instagram attribution = channel %q, date %q", metadata.Channel, metadata.PublicationTimestamp)
	}
}

func TestCapturedInstagramAttributionHandlesEngagementPrefix(t *testing.T) {
	channel, published := capturedInstagramAttribution("Display Name on Instagram: caption", "1,234 likes, 8 comments - user.name on May 9, 2026: caption")
	if channel != "user.name" || published != "May 9, 2026" {
		t.Fatalf("attribution = %q / %q", channel, published)
	}
}

func TestBuildCapturedHTMLVideoArtifactsRecoversTikTokOpenGraphAuthor(t *testing.T) {
	htmlData := []byte(`<html><head><meta property="og:title" content="Hiktron on TikTok"><meta property="og:description" content="A captured caption"><link rel="canonical" href="https://www.tiktok.com/@hiktron/video/7519606597786537223"></head></html>`)
	metadataSidecar, _, err := BuildCapturedHTMLVideoArtifacts(htmlData, "https://www.tiktok.com/@hiktron/video/7519606597786537223", VideoMedia{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var metadata VideoMetadata
	if err := json.Unmarshal(metadataSidecar.Data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Title != "Hiktron on TikTok" || metadata.Channel != "Hiktron" {
		t.Fatalf("TikTok title/channel = %q/%q", metadata.Title, metadata.Channel)
	}
}

func TestCapturedTikTokChannelHandlesProfileTitle(t *testing.T) {
	if got := capturedTikTokChannel("55gms (@55gms_com) | TikTok"); got != "55gms_com" {
		t.Fatalf("profile channel = %q", got)
	}
}

func TestBuildCapturedHTMLVideoArtifactsAllowsDescriptionWithMatchingCanonical(t *testing.T) {
	htmlData := []byte(`<html><head><meta property="og:title" content="Cedric Hutchings - Sprig"><meta property="og:description" content="The console where every player is creator."><link rel="canonical" href="https://vimeo.com/770625302"></head></html>`)
	metadataSidecar, _, err := BuildCapturedHTMLVideoArtifactsAllowingDescription(htmlData, "https://vimeo.com/770625302?share=copy", VideoMedia{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var metadata VideoMetadata
	if err := json.Unmarshal(metadataSidecar.Data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Title != "Cedric Hutchings - Sprig" || metadata.Description != "The console where every player is creator." {
		t.Fatalf("Vimeo metadata = %+v", metadata)
	}
	if _, _, err := BuildCapturedHTMLVideoArtifactsAllowingDescription([]byte(`<html><head><meta property="og:title" content="Log in"><meta property="og:description" content="Join Vimeo"><link rel="canonical" href="https://vimeo.com/log_in"></head></html>`), "https://vimeo.com/770625302", VideoMedia{}, time.Now()); err == nil {
		t.Fatal("wrong-page canonical should not make sparse metadata available")
	}
}
