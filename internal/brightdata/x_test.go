package brightdata

import (
	"context"
	"io"
	"strings"
	"testing"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

const (
	xPhotoPostURL = "https://x.com/BarackObama/status/266031293945503744"
	xVideoPostURL = "https://x.com/SpaceX/status/2057952539417461045"
)

func TestArchiveXPhotoPost(t *testing.T) {
	record := loadRecords(t, "x_post.json")[0]
	network := newFakeNetwork(record)
	image := fakePNG(t)
	network.serve("https://pbs.twimg.com/media/A7EiDWcCYAAZT1D.jpg", image)

	client, db := newTestClient(t, network)
	result, err := client.archiveX(context.Background(), xPhotoPostURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 11, "xph01")
	if err != nil {
		t.Fatalf("archiveX: %v", err)
	}
	if result.Extension != ".zip" || result.Source != models.ArchiveSourceBrightData {
		t.Errorf("result = %s / %q", result.Extension, result.Source)
	}
	if result.Completeness != archivers.CompletenessComplete {
		t.Errorf("completeness = %q; want complete", result.Completeness)
	}

	reader := resultZip(t, result)
	if got := zipEntry(t, reader, "001.jpg"); len(got) != len(image) {
		t.Errorf("stored image is %d bytes; want %d", len(got), len(image))
	}
	meta := galleryMetadataFromZip(t, reader)
	if meta.Extractor != "twitter" {
		t.Errorf("extractor = %q", meta.Extractor)
	}
	if meta.Author != "BarackObama" || meta.AuthorName != "Barack Obama" {
		t.Errorf("author = %q / %q", meta.Author, meta.AuthorName)
	}
	if meta.Description != "Four more years." {
		t.Errorf("text = %q", meta.Description)
	}
	if meta.PostID != "266031293945503744" {
		t.Errorf("post id = %q", meta.PostID)
	}
	if meta.Likes == nil || *meta.Likes != 458086 {
		t.Errorf("likes = %v; want 458086", meta.Likes)
	}
	if meta.Completeness == nil || meta.Completeness.State != archivers.CompletenessComplete {
		t.Errorf("bundle completeness = %+v", meta.Completeness)
	}
	if result.Thumbnail == nil {
		t.Error("no thumbnail from a photo post")
	}
	// A photo post is not a video: it must not claim the video contract.
	if result.Metadata != nil {
		t.Error("photo post carries video metadata")
	}

	rows := usageRows(t, db)
	if len(rows) != 1 || !rows[0].Success || rows[0].DatasetID != DatasetXPosts {
		t.Fatalf("usage rows = %+v", rows)
	}
}

func TestArchiveXVideoPost(t *testing.T) {
	record := loadRecords(t, "x_video_post.json")[0]
	videoURL := xMediaEntries(record)[0].URL
	video := fakeMP4(2048)
	network := newFakeNetwork(record)
	network.serve(videoURL, video)

	client, db := newTestClient(t, network)
	result, err := client.archiveX(context.Background(), xVideoPostURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 12, "xvd01")
	if err != nil {
		t.Fatalf("archiveX: %v", err)
	}

	reader := resultZip(t, result)
	if got := zipEntry(t, reader, "001.mp4"); len(got) != len(video) {
		t.Errorf("stored video is %d bytes; want %d", len(got), len(video))
	}

	meta := videoMetadataFromSidecar(t, result.Metadata)
	if meta.Platform != "x" || meta.Extractor != "twitter" {
		t.Errorf("platform/extractor = %q/%q", meta.Platform, meta.Extractor)
	}
	if meta.PostID != "2057952539417461045" {
		t.Errorf("post id = %q", meta.PostID)
	}
	if meta.PublicationTimestamp != "2026-05-22T22:31:34Z" {
		t.Errorf("publication timestamp = %q", meta.PublicationTimestamp)
	}
	if meta.Author != "SpaceX" || meta.Uploader != "SpaceX" {
		t.Errorf("author = %q / uploader = %q", meta.Author, meta.Uploader)
	}
	// duration is reported in milliseconds.
	if meta.DurationSeconds == nil || *meta.DurationSeconds != 51.21 {
		t.Errorf("duration = %v; want 51.21s from 51210ms", meta.DurationSeconds)
	}
	if got := intValue(meta.Engagement.Views); got != 1941838 {
		t.Errorf("views = %d", got)
	}
	if got := intValue(meta.Engagement.Likes); got != 31837 {
		t.Errorf("likes = %d", got)
	}
	if got := intValue(meta.Engagement.Comments); got != 1092 {
		t.Errorf("replies = %d", got)
	}
	if got := intValue(meta.Engagement.Reposts); got != 5927 {
		t.Errorf("reposts = %d", got)
	}
	if meta.Media.QualityLabel != "3840x2160" {
		t.Errorf("quality label = %q; want 3840x2160", meta.Media.QualityLabel)
	}
	if meta.Media.SizeBytes != int64(len(video)) {
		t.Errorf("media size = %d; want %d", meta.Media.SizeBytes, len(video))
	}

	// twimg URLs carry a load-bearing tag= parameter: it identifies the media
	// variant, and a download that strips it gets a different file or a 404.
	if !network.requested(videoURL) {
		t.Fatalf("video was not downloaded from %q (requests: %v)", videoURL, network.requestedURLs())
	}
	if mustParseQuery(t, videoURL).Get("tag") == "" {
		t.Fatal("fixture lost its tag= parameter; the test no longer proves anything")
	}
}

// The videos field has been seen as a list of {video_url, duration} objects.
// Provider records change shape between post types and over time, so the
// reader accepts the plausible variants rather than trusting one sample.
func TestXVideoEntriesDefensiveShapes(t *testing.T) {
	const base = "https://video.twimg.com/amplify_video/1/vid/avc1/"
	cases := []struct {
		name   string
		videos any
		want   []string
	}{
		{
			name:   "objects with video_url",
			videos: []any{map[string]any{"video_url": base + "1280x720/a.mp4", "duration": float64(51210)}},
			want:   []string{base + "1280x720/a.mp4"},
		},
		{
			name:   "plain URL strings",
			videos: []any{base + "1280x720/a.mp4"},
			want:   []string{base + "1280x720/a.mp4"},
		},
		{
			name:   "objects keyed url",
			videos: []any{map[string]any{"url": base + "1280x720/a.mp4"}},
			want:   []string{base + "1280x720/a.mp4"},
		},
		{
			name: "resolution variants collapse to the largest",
			videos: []any{
				map[string]any{"video_url": base + "640x360/a.mp4"},
				map[string]any{"video_url": base + "3840x2160/a.mp4"},
				map[string]any{"video_url": base + "1280x720/a.mp4"},
			},
			want: []string{base + "3840x2160/a.mp4"},
		},
		{
			name: "two different videos stay two entries",
			videos: []any{
				map[string]any{"video_url": "https://video.twimg.com/amplify_video/1/vid/avc1/1280x720/a.mp4"},
				map[string]any{"video_url": "https://video.twimg.com/amplify_video/2/vid/avc1/1280x720/b.mp4"},
			},
			want: []string{
				"https://video.twimg.com/amplify_video/1/vid/avc1/1280x720/a.mp4",
				"https://video.twimg.com/amplify_video/2/vid/avc1/1280x720/b.mp4",
			},
		},
		{
			name:   "unrecognized resolution is kept, not dropped",
			videos: []any{"https://video.twimg.com/ext_tw_video/1/pu/vid/opaque.mp4"},
			want:   []string{"https://video.twimg.com/ext_tw_video/1/pu/vid/opaque.mp4"},
		},
		{
			name:   "null videos",
			videos: nil,
			want:   nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entries := xMediaEntries(map[string]any{"videos": c.videos})
			if len(entries) != len(c.want) {
				t.Fatalf("entries = %d (%v); want %d", len(entries), entries, len(c.want))
			}
			for i, want := range c.want {
				if entries[i].URL != want {
					t.Errorf("entry %d = %q; want %q", i, entries[i].URL, want)
				}
				if !entries[i].isVideo() {
					t.Errorf("entry %d is not marked as a video", i)
				}
			}
		})
	}
}

// A tweet's own media is archived; media belonging to a quoted tweet or an
// external link preview is not — counting it would archive someone else's post
// and corrupt this one's completeness count.
func TestXMediaEntriesExcludeForeignMedia(t *testing.T) {
	record := map[string]any{
		"photos":              []any{"https://pbs.twimg.com/media/own.jpg"},
		"external_image_urls": []any{"https://example.com/link-preview.jpg"},
		"external_video_urls": []any{"https://example.com/link-preview.mp4"},
		"quoted_post":         map[string]any{"photos": []any{"https://pbs.twimg.com/media/quoted.jpg"}},
	}
	entries := xMediaEntries(record)
	if len(entries) != 1 || entries[0].URL != "https://pbs.twimg.com/media/own.jpg" {
		t.Fatalf("entries = %v; want only the tweet's own photo", entries)
	}
}

func TestArchiveXMediaDownloadFailureRecordsUsage(t *testing.T) {
	record := loadRecords(t, "x_post.json")[0]
	// The CDN is told about nothing, so every download is refused.
	client, db := newTestClient(t, newFakeNetwork(record))

	_, err := client.archiveX(context.Background(), xPhotoPostURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 13, "xerr1")
	if err == nil {
		t.Fatal("expected an error when no media could be downloaded")
	}
	rows := usageRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d; want 1", len(rows))
	}
	if rows[0].Success {
		t.Error("a rescue that stored nothing was recorded as a success")
	}
	if !strings.Contains(rows[0].Detail, "media download") {
		t.Errorf("usage detail does not name the failure: %q", rows[0].Detail)
	}
	// The dataset record was still collected, so the spend is still counted.
	if rows[0].Records != 1 || rows[0].CostUSD != 0.0015 {
		t.Errorf("billable collection not recorded: records=%d cost=%v", rows[0].Records, rows[0].CostUSD)
	}
}
