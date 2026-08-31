package brightdata

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

const (
	facebookVideoURL     = "https://www.facebook.com/NASA/videos/1617067833466601/"
	facebookPhotoPostURL = "https://www.facebook.com/NASA/posts/pfbid02gMKqvktZAj1QZf2cj2Pidqe7Pop6zry7gNrzvBM4mFmha7jxY5TrNhfZh2xjJ9eSl"
)

// facebookVideoRecord is the live per-post record for a video permalink.
func facebookVideoRecord(t *testing.T) map[string]any {
	t.Helper()
	return loadRecords(t, "facebook_video_post.json")[0]
}

// facebookRecordFromPageListing picks one record out of the page-listing
// fixture, which carries the photo-attachment and reel shapes.
func facebookRecordFromPageListing(t *testing.T, index int) map[string]any {
	t.Helper()
	return loadRecords(t, "facebook_page_posts.json")[index]
}

func TestArchiveFacebookVideoPermalink(t *testing.T) {
	record := facebookVideoRecord(t)
	entries := facebookMediaEntries(record, io.Discard)
	if len(entries) != 1 || !entries[0].isVideo() {
		t.Fatalf("fixture entries = %v; want one video", entries)
	}
	video := fakeMP4(4096)
	network := newFakeNetwork(record)
	network.serve(entries[0].URL, video)
	network.serve(facebookPosterURL(record), fakePNG(t))

	client, db := newTestClient(t, network)
	var log strings.Builder
	result, err := client.archiveFacebook(context.Background(), facebookVideoURL, utils.ArchiveTypeYtDlp, &log, db, 31, "fbv01")
	if err != nil {
		t.Fatalf("archiveFacebook: %v", err)
	}
	if result.Extension != ".mp4" || result.Source != models.ArchiveSourceBrightData {
		t.Errorf("result = %s / %q", result.Extension, result.Source)
	}
	if result.Completeness != archivers.CompletenessComplete {
		t.Errorf("completeness = %q; want complete", result.Completeness)
	}
	if got := readResult(t, result); len(got) != len(video) {
		t.Errorf("stored video is %d bytes; want %d", len(got), len(video))
	}
	if result.Thumbnail == nil {
		t.Error("no thumbnail from the video's poster image")
	}

	meta := videoMetadataFromSidecar(t, result.Metadata)
	if meta.Platform != "facebook" || meta.Extractor != "facebook" {
		t.Errorf("platform/extractor = %q/%q", meta.Platform, meta.Extractor)
	}
	if meta.PostID != "1617067833466601" {
		t.Errorf("post id = %q", meta.PostID)
	}
	if meta.Title != "2026 Total Solar Eclipse" {
		t.Errorf("title = %q", meta.Title)
	}
	if meta.PublicationTimestamp != "2026-08-12T17:02:19Z" {
		t.Errorf("publication timestamp = %q", meta.PublicationTimestamp)
	}
	if meta.Uploader != "NASA" {
		t.Errorf("uploader = %q", meta.Uploader)
	}
	if !strings.HasPrefix(meta.Author, "NASA - National Aeronautics") {
		t.Errorf("author = %q", meta.Author)
	}
	if got := intValue(meta.Engagement.Views); got != 521328 {
		t.Errorf("views = %d; want video_view_count 521328", got)
	}
	if got := intValue(meta.Engagement.Likes); got != 23555 {
		t.Errorf("likes = %d", got)
	}
	if got := intValue(meta.Engagement.Comments); got != 9764 {
		t.Errorf("comments = %d", got)
	}
	if got := intValue(meta.Engagement.Reposts); got != 0 {
		t.Errorf("shares = %d", got)
	}
	if meta.Media.SizeBytes != int64(len(video)) {
		t.Errorf("media size = %d; want %d", meta.Media.SizeBytes, len(video))
	}
	if meta.DurationSeconds != nil {
		t.Errorf("duration = %v; want none while video_length's unit is unverified", *meta.DurationSeconds)
	}

	// fbcdn URLs are signed. Every query parameter on that host is redacted
	// before the record is stored, so the archive cannot hand back a live
	// credential.
	if result.RawMetadata == nil {
		t.Fatal("no raw record sidecar")
	}
	if strings.Contains(string(result.RawMetadata.Data), "oe=SYNTHETIC") {
		t.Error("raw record stores signed fbcdn URL parameters at rest")
	}

	rows := usageRows(t, db)
	if len(rows) != 1 || !rows[0].Success || rows[0].DatasetID != DatasetFacebookPosts {
		t.Fatalf("usage rows = %+v", rows)
	}
	if rows[0].Records != 1 || rows[0].CostUSD != 0.0015 {
		t.Errorf("cost not recorded: records=%d cost=%v", rows[0].Records, rows[0].CostUSD)
	}
	if !strings.Contains(log.String(), "Page: NASA") {
		t.Errorf("log does not name the page:\n%s", log.String())
	}
}

// A post permalink is the gallery route: it holds whatever the poster attached,
// which is usually a photo.
func TestArchiveFacebookPhotoPost(t *testing.T) {
	record := facebookRecordFromPageListing(t, 2)
	entries := facebookMediaEntries(record, io.Discard)
	if len(entries) != 1 || entries[0].isVideo() {
		t.Fatalf("fixture entries = %v; want one photo", entries)
	}
	image := fakePNG(t)
	network := newFakeNetwork(record)
	network.serve(entries[0].URL, image)

	client, db := newTestClient(t, network)
	result, err := client.archiveFacebook(context.Background(), facebookPhotoPostURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 32, "fbp01")
	if err != nil {
		t.Fatalf("archiveFacebook: %v", err)
	}
	if result.Extension != ".zip" || result.Completeness != archivers.CompletenessComplete {
		t.Errorf("result = %s / %q", result.Extension, result.Completeness)
	}

	reader := resultZip(t, result)
	if got := zipEntry(t, reader, "001.png"); len(got) != len(image) {
		t.Errorf("stored image is %d bytes; want %d", len(got), len(image))
	}
	meta := galleryMetadataFromZip(t, reader)
	if meta.Extractor != "facebook" || meta.Subcategory != "brightdata" {
		t.Errorf("extractor/subcategory = %q/%q", meta.Extractor, meta.Subcategory)
	}
	if meta.PostID != "1600685561426814" {
		t.Errorf("post id = %q", meta.PostID)
	}
	if !strings.HasPrefix(meta.Description, "Tomorrow, a total solar eclipse") {
		t.Errorf("description = %q", meta.Description)
	}
	if meta.Likes == nil || *meta.Likes != 2980 {
		t.Errorf("likes = %v; want the record's own count", meta.Likes)
	}
	if result.Thumbnail == nil {
		t.Error("no thumbnail from a photo post")
	}
	// A photo post is not a video: it must not claim the video contract.
	if result.Metadata != nil {
		t.Error("photo post carries video metadata")
	}
	// The bundle's raw record is sanitized on the way in: it is downloadable,
	// so a signed CDN URL inside it would be a credential stored at rest.
	if raw := zipEntry(t, reader, "brightdata.json"); strings.Contains(string(raw), "oe=SYNTHETIC") {
		t.Error("bundle stores signed fbcdn URL parameters at rest")
	}

	if rows := usageRows(t, db); len(rows) != 1 || !rows[0].Success {
		t.Fatalf("usage rows = %+v", rows)
	}
}

// A /posts/ permalink can wrap a video. It arrives on the gallery route, so
// the bundle holds the video and the normalized video contract describes it.
func TestArchiveFacebookPostPermalinkWrappingAVideo(t *testing.T) {
	record := facebookRecordFromPageListing(t, 3)
	entries := facebookMediaEntries(record, io.Discard)
	if len(entries) != 1 || !entries[0].isVideo() {
		t.Fatalf("fixture entries = %v; want one video", entries)
	}
	video := fakeMP4(2048)
	poster := fakePNG(t)
	network := newFakeNetwork(record)
	network.serve(entries[0].URL, video)
	network.serve(facebookPosterURL(record), poster)

	client, db := newTestClient(t, network)
	result, err := client.archiveFacebook(context.Background(), "https://www.facebook.com/NASA/posts/pfbid0video/", utils.ArchiveTypeGalleryDl, io.Discard, db, 33, "fbp02")
	if err != nil {
		t.Fatalf("archiveFacebook: %v", err)
	}

	reader := resultZip(t, result)
	if got := zipEntry(t, reader, "001.mp4"); len(got) != len(video) {
		t.Errorf("stored video is %d bytes; want %d", len(got), len(video))
	}
	if result.Thumbnail == nil || !bytes.Equal(result.Thumbnail.Data, poster) {
		t.Error("video post did not retain its published attachment thumbnail")
	}
	meta := videoMetadataFromSidecar(t, result.Metadata)
	if meta.Platform != "facebook" {
		t.Errorf("platform = %q", meta.Platform)
	}
	if meta.PostID != "1600530538108983" {
		t.Errorf("post id = %q", meta.PostID)
	}
	if got := intValue(meta.Engagement.Views); got != 29319 {
		t.Errorf("views = %d", got)
	}
	if meta.Media.SizeBytes != int64(len(video)) {
		t.Errorf("media size = %d", meta.Media.SizeBytes)
	}
}

// The attachment reader is where a wrong field is not a near miss: a video
// attachment's url is the post's page, and an audio attachment is the video's
// own DASH audio stream.
func TestFacebookMediaEntries(t *testing.T) {
	t.Run("video attachments use video_url, never the page link", func(t *testing.T) {
		record := facebookVideoRecord(t)
		entries := facebookMediaEntries(record, io.Discard)
		if len(entries) != 1 {
			t.Fatalf("entries = %v; want only the video", entries)
		}
		if !strings.Contains(entries[0].URL, "video.fmel3-1.fna.fbcdn.net") {
			t.Errorf("entry URL = %q; want the fbcdn video asset", entries[0].URL)
		}
		if strings.Contains(entries[0].URL, "www.facebook.com") {
			t.Error("the post's page link was taken as its video")
		}
	})

	t.Run("the DASH audio attachment is not a second asset", func(t *testing.T) {
		record := facebookVideoRecord(t)
		attachments, _ := record["attachments"].([]any)
		if len(attachments) != 2 {
			t.Fatalf("fixture has %d attachments; expected the video plus its audio stream", len(attachments))
		}
		var log strings.Builder
		entries := facebookMediaEntries(record, &log)
		if len(entries) != 1 {
			t.Errorf("entries = %v; the audio stream was counted as media", entries)
		}
		if !strings.Contains(log.String(), "audio") {
			t.Errorf("log does not say what was skipped:\n%s", log.String())
		}
	})

	t.Run("a text-only post yields nothing", func(t *testing.T) {
		record := facebookRecordFromPageListing(t, 0)
		if entries := facebookMediaEntries(record, io.Discard); len(entries) != 0 {
			t.Errorf("entries = %v; want none for a post with no attachments", entries)
		}
	})

	t.Run("post_image is the fallback when there are no attachments", func(t *testing.T) {
		record := map[string]any{"post_image": "https://scontent.xx.fbcdn.net/v/photo.jpg"}
		entries := facebookMediaEntries(record, io.Discard)
		if len(entries) != 1 || entries[0].URL != "https://scontent.xx.fbcdn.net/v/photo.jpg" || entries[0].isVideo() {
			t.Errorf("entries = %v; want the post image as a photo", entries)
		}
	})

	t.Run("attachment classes are read case-insensitively", func(t *testing.T) {
		record := map[string]any{"attachments": []any{
			map[string]any{"type": "Photo", "url": "https://scontent.xx.fbcdn.net/a.jpg"},
			map[string]any{"type": "video", "video_url": "https://video.xx.fbcdn.net/b.mp4"},
		}}
		entries := facebookMediaEntries(record, io.Discard)
		if len(entries) != 2 || entries[0].isVideo() || !entries[1].isVideo() {
			t.Fatalf("entries = %v; want a photo then a video", entries)
		}
	})

	t.Run("a video attachment with no media URL is skipped, not guessed", func(t *testing.T) {
		record := map[string]any{"attachments": []any{
			map[string]any{"type": "video", "url": "https://www.facebook.com/NASA/videos/1/", "video_url": nil},
		}}
		var log strings.Builder
		if entries := facebookMediaEntries(record, &log); len(entries) != 0 {
			t.Errorf("entries = %v; want none rather than the page link", entries)
		}
		if !strings.Contains(log.String(), "no downloadable URL") {
			t.Errorf("log does not explain the skip:\n%s", log.String())
		}
	})
}

// `likes` and the num_likes_type breakdown disagree in real records, so the
// headline number is preferred and the breakdown is only summed in its absence.
func TestFacebookLikeCount(t *testing.T) {
	cases := []struct {
		name   string
		record map[string]any
		want   int64
	}{
		{
			name:   "headline likes win over the breakdown",
			record: map[string]any{"likes": float64(2980), "num_likes_type": map[string]any{"type": "Like", "num": float64(2468)}},
			want:   2980,
		},
		{
			name: "a breakdown list is summed when likes is absent",
			record: map[string]any{"num_likes_type": []any{
				map[string]any{"type": "Like", "num": float64(10)},
				map[string]any{"type": "Love", "num": float64(5)},
			}},
			want: 15,
		},
		{
			name:   "a single breakdown object is read too",
			record: map[string]any{"num_likes_type": map[string]any{"type": "Like", "num": float64(7)}},
			want:   7,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := facebookLikeCount(c.record); got == nil || *got != c.want {
				t.Errorf("facebookLikeCount = %v; want %d", got, c.want)
			}
		})
	}
	if got := facebookLikeCount(map[string]any{}); got != nil {
		t.Errorf("facebookLikeCount = %v; want nil rather than a fabricated zero", *got)
	}
}

// A CDN that answers 200 with a login wall must not be stored as the post's
// photo: the bundle would look whole and hold a web page.
func TestArchiveFacebookRejectsAnHTMLPageServedAsMedia(t *testing.T) {
	record := facebookRecordFromPageListing(t, 2)
	entries := facebookMediaEntries(record, io.Discard)
	network := newFakeNetwork(record)
	network.serve(entries[0].URL, []byte("<!DOCTYPE html><html><head><title>Log in to Facebook</title></head></html>"))

	client, db := newTestClient(t, network)
	var log strings.Builder
	_, err := client.archiveFacebook(context.Background(), facebookPhotoPostURL, utils.ArchiveTypeGalleryDl, &log, db, 34, "fbp03")
	if err == nil {
		t.Fatal("expected an error when the only asset was an HTML document")
	}
	if !strings.Contains(log.String(), "HTML document") {
		t.Errorf("log does not name the rejection:\n%s", log.String())
	}
	rows := usageRows(t, db)
	if len(rows) != 1 || rows[0].Success {
		t.Fatalf("usage rows = %+v; want one unsuccessful row", rows)
	}
}

// A video permalink whose record holds no video fails explicitly rather than
// storing whatever else the post had.
func TestArchiveFacebookVideoWithoutVideoFails(t *testing.T) {
	record := facebookRecordFromPageListing(t, 2) // a photo post
	client, db := newTestClient(t, newFakeNetwork(record))

	_, err := client.archiveFacebook(context.Background(), facebookVideoURL, utils.ArchiveTypeYtDlp, io.Discard, db, 35, "fbv02")
	if err == nil {
		t.Fatal("expected an error for a record with no video")
	}
	if !strings.Contains(err.Error(), "no video URL") {
		t.Errorf("error does not name the cause: %v", err)
	}
	rows := usageRows(t, db)
	if len(rows) != 1 || rows[0].Success {
		t.Fatalf("usage rows = %+v; want one unsuccessful row", rows)
	}
	if rows[0].Records != 1 || rows[0].CostUSD != 0.0015 {
		t.Errorf("billable collection not recorded: records=%d cost=%v", rows[0].Records, rows[0].CostUSD)
	}
}

func TestArchiveFacebookTextOnlyPostFails(t *testing.T) {
	record := facebookRecordFromPageListing(t, 0)
	client, db := newTestClient(t, newFakeNetwork(record))

	_, err := client.archiveFacebook(context.Background(), facebookPhotoPostURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 36, "fbp04")
	if err == nil {
		t.Fatal("expected an error for a post with no media")
	}
	if !strings.Contains(err.Error(), "no media") {
		t.Errorf("error does not name the cause: %v", err)
	}
	if rows := usageRows(t, db); len(rows) != 1 || rows[0].Success {
		t.Fatalf("usage rows = %+v; want one unsuccessful row", rows)
	}
}

func TestArchiveFacebookRejectsWrongArchiveType(t *testing.T) {
	client, db := newTestClient(t, newFakeNetwork(facebookVideoRecord(t)))

	_, err := client.archiveFacebook(context.Background(), facebookVideoURL, utils.ArchiveTypeMHTML, io.Discard, db, 37, "fbv03")
	if err == nil {
		t.Fatal("expected an error for an mhtml item")
	}
	if rows := usageRows(t, db); len(rows) != 0 {
		t.Errorf("money was spent on a wrongly-routed item: %+v", rows)
	}
}

// The public path: a native failure on a Facebook photo post, the coverage
// check, the dispatch, and the bundle — the combination nothing else covers,
// because the per-platform tests call the flows directly.
func TestFallbackArchiverRescuesFacebookPhotoPost(t *testing.T) {
	record := facebookRecordFromPageListing(t, 2)
	network := newFakeNetwork(record)
	network.serve(facebookMediaEntries(record, io.Discard)[0].URL, fakePNG(t))

	client, db := newTestClient(t, network)
	capture := models.Capture{ShortID: "fb001"}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatalf("create capture: %v", err)
	}
	item := models.ArchiveItem{CaptureID: capture.ID, Type: utils.ArchiveTypeGalleryDl, Status: "processing"}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("create item: %v", err)
	}

	arch := WithFallback(&fakePrimary{err: errFacebookNativeRefused}, utils.ArchiveTypeGalleryDl, client)
	var log strings.Builder
	result, err := arch.Archive(context.Background(), facebookPhotoPostURL, &log, db, item.ID)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.Source != models.ArchiveSourceBrightData || result.Extension != ".zip" {
		t.Errorf("result = %q %s; want a brightdata ZIP", result.Source, result.Extension)
	}
	readResult(t, result)

	rows := usageRows(t, db)
	if len(rows) != 1 || !rows[0].Success {
		t.Fatalf("usage rows = %+v", rows)
	}
	if rows[0].ShortID != "fb001" || rows[0].ArchiveItemID != item.ID {
		t.Errorf("usage row not attributed to the capture: %+v", rows[0])
	}
}

var errFacebookNativeRefused = &facebookNativeError{}

type facebookNativeError struct{}

func (e *facebookNativeError) Error() string { return "gallery-dl: HTTP 403 (login required)" }
