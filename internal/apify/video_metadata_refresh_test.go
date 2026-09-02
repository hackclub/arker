package apify

import (
	"context"
	"io"
	"strings"
	"testing"

	"arker/internal/archivers"
)

func TestRefreshStoredVideoMetadataUsesRecordsWithoutDownloadingMedia(t *testing.T) {
	tests := []struct {
		name, fixture, actor, targetURL, platform string
	}{
		{"instagram", "instagram_reel.json", ActorInstagram, "https://www.instagram.com/reel/DUp5AL8kxuN/", "instagram"},
		{"tiktok", "tiktok_video.json", ActorTikTok, "https://www.tiktok.com/@hackclub_latam/video/7653880597500218645", "tiktok"},
		{"facebook", "facebook_video.json", ActorFacebook, "https://www.facebook.com/HackClubHQ/videos/1847881915262822/", "facebook"},
		{"youtube", "youtube_scrape.json", ActorYouTubeMetadata, "https://youtu.be/Px0L-_8_9fw", "youtube"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := loadRecord(t, test.fixture)
			network := newFakeNetwork(actorRun{Actor: test.actor, Items: []map[string]any{record}, CostUSD: 0.004})
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
			if meta.Media.Width != nil || meta.Media.Height != nil || meta.Media.FormatID != "" {
				t.Errorf("current provider delivery facts leaked into historical media: %+v", meta.Media)
			}
			if meta.Title == "" || meta.Provenance != "apify" {
				t.Errorf("metadata = %+v", meta)
			}
			for _, requested := range network.requestedURLs() {
				if !strings.HasPrefix(requested, apiBase) {
					t.Errorf("metadata-only repair downloaded media: %s", requested)
				}
			}
			if actors := network.startedActors(); len(actors) != 1 || actors[0] != test.actor {
				t.Errorf("actors started = %v; want only %s", actors, test.actor)
			}
			assertNoSignedParamsAtRest(t, "raw sidecar", result.RawMetadata.Data)
			rows := usageRows(t, db)
			if len(rows) != 1 || !rows[0].Success || rows[0].CostUSD != 0.004 || rows[0].Detail != "metadata-only historical repair" {
				t.Errorf("usage = %+v", rows)
			}
		})
	}
}

// A TikTok refresh must not ask the actor to copy the video into its store:
// that is the expensive part of a full run and the bytes are already held.
func TestRefreshStoredTikTokMetadataDoesNotRequestDownloads(t *testing.T) {
	network := newFakeNetwork(runFor(ActorTikTok, loadRecord(t, "tiktok_video.json")))
	client, db := newTestClient(t, network)
	if _, err := client.RefreshStoredVideoMetadata(context.Background(), "https://www.tiktok.com/@hackclub_latam/video/7653880597500218645", io.Discard, db, 0, archivers.VideoMedia{}); err != nil {
		t.Fatal(err)
	}
	input := network.inputFor(ActorTikTok)
	for _, key := range []string{"shouldDownloadVideos", "shouldDownloadCovers", "shouldDownloadSlideshowImages"} {
		if on, _ := input[key].(bool); on {
			t.Errorf("metadata-only refresh asked for %s", key)
		}
	}
}

func TestRefreshStoredYouTubeMetadataFallsBackToOEmbed(t *testing.T) {
	network := newFakeNetwork(actorRun{Actor: ActorYouTubeMetadata, Status: "FAILED", StatusMessage: "out of memory"})
	network.serve("https://www.youtube.com/oembed?format=json&url=https%3A%2F%2Fwww.youtube.com%2Fwatch%3Fv%3DdQw4w9WgXcQ",
		[]byte(`{"title":"Archived title","author_name":"Archived channel","author_url":"https://www.youtube.com/@archived","thumbnail_url":"https://i.ytimg.com/vi/dQw4w9WgXcQ/hqdefault.jpg"}`))
	client, db := newTestClient(t, network)

	result, err := client.RefreshStoredVideoMetadata(context.Background(), "https://youtu.be/dQw4w9WgXcQ", io.Discard, db, 0, archivers.VideoMedia{Extension: ".mp4", SizeBytes: 555})
	if err != nil {
		t.Fatalf("RefreshStoredVideoMetadata: %v", err)
	}
	meta := videoMetadataFromSidecar(t, result.Metadata)
	if meta.Title != "Archived title" || meta.Author != "Archived channel" || meta.Media.SizeBytes != 555 || meta.Extractor != "youtube:oembed" {
		t.Errorf("metadata = %+v", meta)
	}
	for _, actor := range network.startedActors() {
		if actor == ActorYouTube {
			t.Fatal("metadata refresh started the YouTube downloader")
		}
	}
	rows := usageRows(t, db)
	if len(rows) != 2 || rows[0].Success || !rows[1].Success {
		t.Errorf("usage = %+v", rows)
	}
}

func TestSupportsStoredVideoMetadata(t *testing.T) {
	client := &Client{cfg: Config{Token: "k"}}
	for url, want := range map[string]bool{
		"https://www.instagram.com/reel/X/":            true,
		"https://www.tiktok.com/@u/video/1":            true,
		"https://www.facebook.com/reel/1/":             true,
		"https://www.youtube.com/watch?v=abc123def45":  true,
		"https://www.youtube.com/":                     false,
		"https://www.reddit.com/r/aww/comments/abc/x/": false,
		"https://www.pinterest.com/pin/1/":             false,
	} {
		if got := client.SupportsStoredVideoMetadata(url); got != want {
			t.Errorf("SupportsStoredVideoMetadata(%s) = %v; want %v", url, got, want)
		}
	}
	if (&Client{}).SupportsStoredVideoMetadata("https://www.instagram.com/reel/X/") {
		t.Error("unconfigured client claimed a metadata-only path")
	}
}
