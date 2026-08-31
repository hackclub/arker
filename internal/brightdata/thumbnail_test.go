package brightdata

import (
	"bytes"
	"context"
	"io"
	"testing"

	"arker/internal/models"
	"arker/internal/utils"
)

func TestResolveSocialThumbnailDownloadsInstagramPosterOnly(t *testing.T) {
	posterURL := "https://scontent.example/poster.png?sig=secret"
	record := map[string]any{
		"shortcode": "POST123",
		"thumbnail": posterURL,
		"video_url": "https://cdn.example/video.mp4?sig=must-not-download",
	}
	network := newFakeNetwork(record)
	poster := fakePNG(t)
	network.serve(posterURL, poster)
	client, db := newTestClient(t, network)

	thumb, err := client.ResolveSocialThumbnail(context.Background(), "https://www.instagram.com/p/POST123/", utils.ArchiveTypeGalleryDl, io.Discard, db, 99)
	if err != nil {
		t.Fatalf("ResolveSocialThumbnail: %v", err)
	}
	if !bytes.Equal(thumb.Data, poster) || thumb.Kind != models.ThumbnailKindSocialOriginal {
		t.Fatal("provider poster was not preserved exactly")
	}
	if network.requested("https://cdn.example/video.mp4?sig=must-not-download") {
		t.Fatal("thumbnail backfill downloaded archived video media")
	}
	rows := usageRows(t, db)
	if len(rows) != 1 || rows[0].CostUSD != 0.0015 || !rows[0].Success {
		t.Fatalf("usage rows = %+v", rows)
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
}
