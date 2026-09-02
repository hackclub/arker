package apify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

func TestResolveSocialThumbnailDownloadsInstagramPosterOnly(t *testing.T) {
	record := loadRecord(t, "instagram_reel.json")
	posterURL := instagramImageURL(record)
	videoURL := instagramVideoURL(record)
	if posterURL == "" || videoURL == "" {
		t.Fatalf("fixture lacks poster (%q) or video (%q)", posterURL, videoURL)
	}
	network := newFakeNetwork(actorRun{Actor: ActorInstagram, Items: []map[string]any{record}, CostUSD: 0.0015})
	poster := fakePNG(t)
	network.serve(posterURL, poster)
	client, db := newTestClient(t, network)

	thumb, err := client.ResolveSocialThumbnail(context.Background(), "https://www.instagram.com/reel/DUp5AL8kxuN/", utils.ArchiveTypeYtDlp, io.Discard, db, 99)
	if err != nil {
		t.Fatalf("ResolveSocialThumbnail: %v", err)
	}
	if !bytes.Equal(thumb.Data, poster) || thumb.Kind != models.ThumbnailKindSocialPreview {
		t.Fatal("small provider poster was not retained as a contract-ready preview")
	}
	if network.requested(videoURL) {
		t.Fatal("thumbnail backfill downloaded archived video media")
	}
	rows := usageRows(t, db)
	if len(rows) != 1 || rows[0].CostUSD != 0.0015 || !rows[0].Success || rows[0].ArchiveItemID != 99 {
		t.Fatalf("usage rows = %+v", rows)
	}
}

func TestResolveSocialThumbnailTikTokUsesPlatformCoverWithoutStoreCopies(t *testing.T) {
	record := loadRecord(t, "tiktok_video.json")
	posterURL := tiktokPosterURL(record)
	network := newFakeNetwork(runFor(ActorTikTok, record))
	network.serve(posterURL, fakeJPEG(t))
	client, db := newTestClient(t, network)

	if _, err := client.ResolveSocialThumbnail(context.Background(), "https://www.tiktok.com/@hackclub_latam/video/7653880597500218645", utils.ArchiveTypeYtDlp, io.Discard, db, 1); err != nil {
		t.Fatal(err)
	}
	input := network.inputFor(ActorTikTok)
	for _, key := range []string{"shouldDownloadVideos", "shouldDownloadCovers", "shouldDownloadSlideshowImages"} {
		if on, _ := input[key].(bool); on {
			t.Errorf("poster-only run asked for %s", key)
		}
	}
	for _, requested := range network.requestedURLs() {
		if strings.HasPrefix(requested, apiBase+"/key-value-stores") {
			t.Errorf("poster-only run read a store record: %s", requested)
		}
	}
}

func TestResolveSocialThumbnailUsesFreeYouTubePosterURL(t *testing.T) {
	videoID := "abcdefghijk"
	posterURL := "https://i.ytimg.com/vi/" + videoID + "/maxresdefault.jpg"
	network := newFakeNetwork()
	poster := fakePNG(t)
	network.serve(posterURL, poster)
	client, db := newTestClient(t, network)

	thumb, err := client.ResolveSocialThumbnail(context.Background(), "https://www.youtube.com/watch?v="+videoID, utils.ArchiveTypeYtDlp, io.Discard, db, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(thumb.Data, poster) {
		t.Fatal("YouTube poster was not retained")
	}
	if cost := client.SocialThumbnailCostUSD("https://www.youtube.com/watch?v="+videoID, utils.ArchiveTypeYtDlp); cost != 0 {
		t.Fatalf("YouTube poster cost = %v, want zero", cost)
	}
	if rows := usageRows(t, db); len(rows) != 0 {
		t.Fatalf("free YouTube poster unexpectedly recorded paid usage: %+v", rows)
	}
	if len(network.startedActors()) != 0 {
		t.Fatal("YouTube poster started an actor")
	}
}

func TestResolveSocialThumbnailYouTubeFallsBackToHQDefault(t *testing.T) {
	videoID := "abcdefghijk"
	network := newFakeNetwork()
	network.serveStatus("https://i.ytimg.com/vi/"+videoID+"/maxresdefault.jpg", 404)
	network.serve("https://i.ytimg.com/vi/"+videoID+"/hqdefault.jpg", fakeJPEG(t))
	client, db := newTestClient(t, network)
	if _, err := client.ResolveSocialThumbnail(context.Background(), "https://youtu.be/"+videoID, utils.ArchiveTypeYtDlp, io.Discard, db, 1); err != nil {
		t.Fatal(err)
	}

	// Both gone is conclusive: the backfill should stop retrying.
	network.serveStatus("https://i.ytimg.com/vi/"+videoID+"/hqdefault.jpg", 404)
	_, err := client.ResolveSocialThumbnail(context.Background(), "https://youtu.be/"+videoID, utils.ArchiveTypeYtDlp, io.Discard, db, 1)
	if !errors.Is(err, archivers.ErrSocialThumbnailUnavailable) {
		t.Fatalf("missing poster error = %v; want ErrSocialThumbnailUnavailable", err)
	}
}

// A deleted post is conclusive; a transport failure is not.
func TestResolveSocialThumbnailClassifiesErrors(t *testing.T) {
	network := newFakeNetwork(runFor(ActorInstagram, loadRecord(t, "instagram_error.json")))
	client, db := newTestClient(t, network)
	_, err := client.ResolveSocialThumbnail(context.Background(), "https://www.instagram.com/p/GONE/", utils.ArchiveTypeGalleryDl, io.Discard, db, 1)
	if !errors.Is(err, archivers.ErrSocialThumbnailUnavailable) {
		t.Fatalf("deleted post error = %v; want ErrSocialThumbnailUnavailable", err)
	}

	network = newFakeNetwork(actorRun{Actor: ActorInstagram, StartStatus: 500})
	client, db = newTestClient(t, network)
	_, err = client.ResolveSocialThumbnail(context.Background(), "https://www.instagram.com/p/GONE/", utils.ArchiveTypeGalleryDl, io.Discard, db, 1)
	if err == nil || errors.Is(err, archivers.ErrSocialThumbnailUnavailable) {
		t.Fatalf("API failure error = %v; want a retryable error", err)
	}
}

func TestSupportsSocialThumbnail(t *testing.T) {
	client := &Client{cfg: Config{Token: "k"}}
	for url, want := range map[string]bool{
		"https://www.instagram.com/p/X/":              true,
		"https://www.tiktok.com/@u/photo/1":           true,
		"https://www.facebook.com/NASA/posts/pfbid1/": true,
		"https://www.youtube.com/watch?v=abc123def45": true,
		"https://www.reddit.com/r/aww/comments/a/b/":  false,
	} {
		if got := client.SupportsSocialThumbnail(url, ""); got != want {
			t.Errorf("SupportsSocialThumbnail(%s) = %v; want %v", url, got, want)
		}
		if cost := client.SocialThumbnailCostUSD(url, ""); (cost > 0) != (want && !strings.Contains(url, "youtube")) {
			t.Errorf("SocialThumbnailCostUSD(%s) = %v", url, cost)
		}
	}
	disabled := &Client{}
	if disabled.SupportsSocialThumbnail("https://www.instagram.com/p/X/", "") {
		t.Error("unconfigured client claimed a paid poster path")
	}
	if !disabled.SupportsSocialThumbnail("https://www.youtube.com/watch?v=abc123def45", "") {
		t.Error("YouTube posters are free and should not need a token")
	}
}
