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

const pinterestPinURL = "https://www.pinterest.com/pin/1123648175807513296"

func TestArchivePinterestImagePin(t *testing.T) {
	record := loadRecords(t, "pinterest_posts.json")[0]
	network := newFakeNetwork(record)
	image := fakeJPEG(t)
	network.serve("https://i.pinimg.com/originals/99/d9/39/99d939a31478dd6cb9a6859113e204d3.jpg", image)

	client, db := newTestClient(t, network)
	result, err := client.archivePinterest(context.Background(), pinterestPinURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 21, "pin01")
	if err != nil {
		t.Fatalf("archivePinterest: %v", err)
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
	if meta.Extractor != "pinterest" || meta.Subcategory != "brightdata" {
		t.Errorf("extractor/subcategory = %q/%q", meta.Extractor, meta.Subcategory)
	}
	if meta.PostID != "1123648175807513296" {
		t.Errorf("post id = %q", meta.PostID)
	}
	if meta.Author != "mrjavidah" {
		t.Errorf("author = %q", meta.Author)
	}
	if meta.Date != "2026-07-26T05:17:26.000Z" {
		t.Errorf("date = %q", meta.Date)
	}
	// image_video_url and attached_files[0] are the same asset. Counting it
	// twice would archive it twice and report a whole pin as half-archived the
	// moment one copy failed.
	if meta.FileCount != 1 || len(meta.Files) != 1 {
		t.Errorf("file count = %d (%v); want the pin's single asset", meta.FileCount, zipNames(reader))
	}
	if meta.Completeness == nil || meta.Completeness.State != archivers.CompletenessComplete {
		t.Errorf("bundle completeness = %+v", meta.Completeness)
	}
	if result.Thumbnail == nil {
		t.Error("no thumbnail from an image pin")
	}
	// An image pin is not a video: it must not claim the video contract.
	if result.Metadata != nil {
		t.Error("image pin carries video metadata")
	}

	rows := usageRows(t, db)
	if len(rows) != 1 || !rows[0].Success || rows[0].DatasetID != DatasetPinterestPosts {
		t.Fatalf("usage rows = %+v", rows)
	}
	if rows[0].Records != 1 || rows[0].CostUSD != 0.0015 {
		t.Errorf("cost not recorded: records=%d cost=%v", rows[0].Records, rows[0].CostUSD)
	}
}

// A pin carrying a caption and a title keeps both: the caption is the only text
// most image pins publish.
func TestArchivePinterestKeepsPinText(t *testing.T) {
	record := loadRecords(t, "pinterest_posts.json")[2]
	network := newFakeNetwork(record)
	network.serve("https://i.pinimg.com/originals/00/b4/30/00b430230be36bc423d4a7c13cbc51f8.png", fakePNG(t))

	client, db := newTestClient(t, network)
	var log strings.Builder
	result, err := client.archivePinterest(context.Background(), "https://www.pinterest.com/pin/716776096987585345", utils.ArchiveTypeGalleryDl, &log, db, 22, "pin02")
	if err != nil {
		t.Fatalf("archivePinterest: %v", err)
	}

	reader := resultZip(t, result)
	meta := galleryMetadataFromZip(t, reader)
	if meta.Title != "Whispers of Nature✨" {
		t.Errorf("title = %q", meta.Title)
	}
	if !strings.HasPrefix(meta.Description, "Nature has a way of reminding us") {
		t.Errorf("description = %q", meta.Description)
	}
	if len(meta.Tags) != 8 {
		t.Errorf("tags = %v; want the pin's 8 interest labels", meta.Tags)
	}
	if !strings.Contains(log.String(), "Author: drkhaani786") {
		t.Errorf("log does not name the author:\n%s", log.String())
	}
	// The stored raw record is the provider's, sanitized on the way in.
	if raw := zipEntry(t, reader, "brightdata.json"); !strings.Contains(string(raw), "716776096987585345") {
		t.Error("bundle does not carry the raw Bright Data record")
	}
}

// A video pin additionally gets the normalized video contract: GalleryMetadata
// has nowhere to put the engagement counts or the publication timestamp.
func TestArchivePinterestVideoPinGetsVideoMetadata(t *testing.T) {
	record := map[string]any{
		"url":             "https://www.pinterest.com/pin/9001",
		"post_id":         "9001",
		"title":           "A short clip",
		"user_name":       "someone",
		"user_id":         "42",
		"date_posted":     "2026-07-26T05:17:26.000Z",
		"likes":           float64(12),
		"comments_num":    float64(3),
		"post_type":       "video",
		"video_length":    float64(15),
		"image_video_url": "https://v1.pinimg.com/videos/mc/720p/aa/bb/cc/clip.mp4",
	}
	video := fakeMP4(4096)
	network := newFakeNetwork(record)
	network.serve("https://v1.pinimg.com/videos/mc/720p/aa/bb/cc/clip.mp4", video)

	client, db := newTestClient(t, network)
	result, err := client.archivePinterest(context.Background(), "https://www.pinterest.com/pin/9001", utils.ArchiveTypeGalleryDl, io.Discard, db, 23, "pin03")
	if err != nil {
		t.Fatalf("archivePinterest: %v", err)
	}

	reader := resultZip(t, result)
	if got := zipEntry(t, reader, "001.mp4"); len(got) != len(video) {
		t.Errorf("stored video is %d bytes; want %d", len(got), len(video))
	}

	meta := videoMetadataFromSidecar(t, result.Metadata)
	if meta.Platform != "pinterest" || meta.Extractor != "pinterest" {
		t.Errorf("platform/extractor = %q/%q", meta.Platform, meta.Extractor)
	}
	if meta.PostID != "9001" || meta.Title != "A short clip" {
		t.Errorf("post id/title = %q/%q", meta.PostID, meta.Title)
	}
	if meta.PublicationTimestamp != "2026-07-26T05:17:26Z" {
		t.Errorf("publication timestamp = %q", meta.PublicationTimestamp)
	}
	if got := intValue(meta.Engagement.Likes); got != 12 {
		t.Errorf("likes = %d", got)
	}
	if got := intValue(meta.Engagement.Comments); got != 3 {
		t.Errorf("comments = %d", got)
	}
	if meta.Media.SizeBytes != int64(len(video)) {
		t.Errorf("media size = %d; want %d", meta.Media.SizeBytes, len(video))
	}
	if meta.Media.QualityLabel != "720p" {
		t.Errorf("quality label = %q; want 720p", meta.Media.QualityLabel)
	}
	// video_length's unit is unverified (every verified record is an image pin
	// carrying 0), so the archive does not claim a duration rather than risk
	// one that is wrong by 1000x.
	if meta.DurationSeconds != nil {
		t.Errorf("duration = %v; want none until the unit is verified", *meta.DurationSeconds)
	}
	if result.RawMetadata == nil {
		t.Error("video pin has no raw record sidecar")
	}
}

func TestPinterestMediaEntries(t *testing.T) {
	cases := []struct {
		name      string
		record    map[string]any
		wantURLs  []string
		wantVideo []bool
	}{
		{
			name: "image_video_url and attached_files are one asset",
			record: map[string]any{
				"post_type":       "image",
				"image_video_url": "https://i.pinimg.com/originals/a/b/c.jpg",
				"attached_files":  []any{"https://i.pinimg.com/originals/a/b/c.jpg"},
			},
			wantURLs:  []string{"https://i.pinimg.com/originals/a/b/c.jpg"},
			wantVideo: []bool{false},
		},
		{
			name: "a distinct attached file is a second asset",
			record: map[string]any{
				"post_type":       "image",
				"image_video_url": "https://i.pinimg.com/originals/a/b/c.jpg",
				"attached_files":  []any{"https://i.pinimg.com/originals/d/e/f.jpg"},
			},
			wantURLs:  []string{"https://i.pinimg.com/originals/a/b/c.jpg", "https://i.pinimg.com/originals/d/e/f.jpg"},
			wantVideo: []bool{false, false},
		},
		{
			name: "a video pin's poster stays a photo",
			record: map[string]any{
				"post_type":       "video",
				"image_video_url": "https://v1.pinimg.com/videos/mc/720p/a/b/clip.mp4",
				"attached_files":  []any{"https://i.pinimg.com/originals/a/b/poster.jpg"},
			},
			wantURLs:  []string{"https://v1.pinimg.com/videos/mc/720p/a/b/clip.mp4", "https://i.pinimg.com/originals/a/b/poster.jpg"},
			wantVideo: []bool{true, false},
		},
		{
			name: "objects in attached_files are read too",
			record: map[string]any{
				"post_type":      "image",
				"attached_files": []any{map[string]any{"url": "https://i.pinimg.com/originals/a/b/c.jpg"}},
			},
			wantURLs:  []string{"https://i.pinimg.com/originals/a/b/c.jpg"},
			wantVideo: []bool{false},
		},
		{
			name:     "a record with no media yields nothing",
			record:   map[string]any{"post_type": "image"},
			wantURLs: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entries := pinterestMediaEntries(c.record)
			if len(entries) != len(c.wantURLs) {
				t.Fatalf("entries = %v; want %v", entries, c.wantURLs)
			}
			for i, want := range c.wantURLs {
				if entries[i].URL != want {
					t.Errorf("entry %d = %q; want %q", i, entries[i].URL, want)
				}
				if entries[i].isVideo() != c.wantVideo[i] {
					t.Errorf("entry %d isVideo = %v; want %v", i, entries[i].isVideo(), c.wantVideo[i])
				}
			}
		})
	}
}

// A pin whose asset cannot be downloaded fails explicitly, and the collection
// it already paid for is still recorded as spend.
func TestArchivePinterestMediaDownloadFailureRecordsUsage(t *testing.T) {
	record := loadRecords(t, "pinterest_posts.json")[0]
	client, db := newTestClient(t, newFakeNetwork(record))

	_, err := client.archivePinterest(context.Background(), pinterestPinURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 24, "pin04")
	if err == nil {
		t.Fatal("expected an error when the pin's image could not be downloaded")
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
	if rows[0].Records != 1 || rows[0].CostUSD != 0.0015 {
		t.Errorf("billable collection not recorded: records=%d cost=%v", rows[0].Records, rows[0].CostUSD)
	}
}

// A record with no media at all is an explicit failure rather than an empty
// bundle that reads as a successful archive.
func TestArchivePinterestWithoutMediaFails(t *testing.T) {
	record := map[string]any{"url": pinterestPinURL, "post_id": "1", "post_type": "story"}
	client, db := newTestClient(t, newFakeNetwork(record))

	_, err := client.archivePinterest(context.Background(), pinterestPinURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 25, "pin05")
	if err == nil {
		t.Fatal("expected an error for a record with no media")
	}
	if !strings.Contains(err.Error(), "story") {
		t.Errorf("error does not name the pin type: %v", err)
	}
	rows := usageRows(t, db)
	if len(rows) != 1 || rows[0].Success {
		t.Fatalf("usage rows = %+v; want one unsuccessful row", rows)
	}
}

// Pinterest routes to gallery-dl only. A yt-dlp item asking for a pin is a
// routing bug, and paying for a dataset record would hide it.
func TestArchivePinterestRejectsWrongArchiveType(t *testing.T) {
	network := newFakeNetwork(loadRecords(t, "pinterest_posts.json")[0])
	client, db := newTestClient(t, network)

	_, err := client.archivePinterest(context.Background(), pinterestPinURL, utils.ArchiveTypeYtDlp, io.Discard, db, 26, "pin06")
	if err == nil {
		t.Fatal("expected an error for a yt-dlp item")
	}
	if rows := usageRows(t, db); len(rows) != 0 {
		t.Errorf("money was spent on a wrongly-routed item: %+v", rows)
	}
}
