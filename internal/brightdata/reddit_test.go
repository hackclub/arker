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

const redditPostURL = "https://www.reddit.com/r/aww/comments/1vjt9lo/meet_roxy_my_9_week_old_miniature_dachshund/"

// The fixture's videos[] is the same clip at six resolutions. Storing the
// first would archive a 392p copy of a 1920p video, and storing all six would
// claim the post holds six videos.
func TestRedditVideoEntriesPickHighestResolution(t *testing.T) {
	record := loadRecords(t, "reddit_post.json")[0]

	entries := redditMediaEntries(record)
	if len(entries) != 1 {
		t.Fatalf("entries = %d; want 1 (six resolutions of one video)", len(entries))
	}
	if !strings.Contains(entries[0].URL, "m2-res_1920p.mp4") {
		t.Errorf("chose %q; want the 1920p variant", entries[0].URL)
	}
	if !entries[0].isVideo() || entries[0].extension() != ".mp4" {
		t.Errorf("entry is not a video: %+v", entries[0])
	}
}

func TestArchiveRedditVideoPost(t *testing.T) {
	record := loadRecords(t, "reddit_post.json")[0]
	network := newFakeNetwork(record)
	video := fakeMP4(4096)
	videoURL := redditMediaEntries(record)[0].URL
	network.serve(videoURL, video)

	client, db := newTestClient(t, network)
	var log strings.Builder
	result, err := client.archiveReddit(context.Background(), redditPostURL, utils.ArchiveTypeGalleryDl, &log, db, 7, "abc12")
	if err != nil {
		t.Fatalf("archiveReddit: %v", err)
	}

	if result.Extension != ".zip" || result.ContentType != "application/zip" {
		t.Errorf("result is %s/%s; want a gallery ZIP", result.Extension, result.ContentType)
	}
	if result.Source != models.ArchiveSourceBrightData {
		t.Errorf("source = %q; want brightdata", result.Source)
	}
	if result.Completeness != archivers.CompletenessComplete {
		t.Errorf("completeness = %q; want complete", result.Completeness)
	}

	reader := resultZip(t, result)
	if got := zipEntry(t, reader, "001.mp4"); len(got) != len(video) {
		t.Errorf("stored video is %d bytes; want %d", len(got), len(video))
	}
	meta := galleryMetadataFromZip(t, reader)
	if meta.Extractor != "reddit" {
		t.Errorf("extractor = %q", meta.Extractor)
	}
	if meta.Author != "Wake_The_Riot" {
		t.Errorf("author = %q", meta.Author)
	}
	if meta.AuthorName != "r/aww" {
		t.Errorf("community = %q; want r/aww", meta.AuthorName)
	}
	if meta.Likes == nil || *meta.Likes != 10686 {
		t.Errorf("upvotes = %v; want 10686", meta.Likes)
	}
	if meta.Completeness == nil || meta.Completeness.State != archivers.CompletenessComplete {
		t.Fatalf("bundle completeness = %+v; want complete", meta.Completeness)
	}
	if meta.Completeness.Expected == nil || *meta.Completeness.Expected != 1 || meta.Completeness.Stored != 1 {
		t.Errorf("completeness counts = %+v; want 1 of 1", meta.Completeness)
	}

	// A single-video post also carries the normalized video contract, which is
	// the only place engagement and the publication timestamp can live.
	video1 := videoMetadataFromSidecar(t, result.Metadata)
	if video1.Platform != "reddit" || video1.PostID != "t3_1vjt9lo" {
		t.Errorf("normalized metadata = %q/%q", video1.Platform, video1.PostID)
	}
	if video1.PublicationTimestamp != "2026-08-09T15:48:37Z" {
		t.Errorf("publication timestamp = %q", video1.PublicationTimestamp)
	}
	if got := intValue(video1.Engagement.Likes); got != 10686 {
		t.Errorf("likes = %d; want 10686", got)
	}
	if got := intValue(video1.Engagement.Comments); got != 84 {
		t.Errorf("comments = %d; want 84", got)
	}
	if video1.Media.SizeBytes != int64(len(video)) {
		t.Errorf("media size = %d; want %d", video1.Media.SizeBytes, len(video))
	}
	if video1.Media.QualityLabel != "1920p" {
		t.Errorf("quality label = %q; want 1920p", video1.Media.QualityLabel)
	}
	if video1.Provenance != models.ArchiveSourceBrightData {
		t.Errorf("provenance = %q", video1.Provenance)
	}
	if video1.Channel != "r/aww" {
		t.Errorf("channel = %q; want r/aww", video1.Channel)
	}

	// Signed CDN parameters must not survive into either stored record.
	assertNoSignedParamsAtRest(t, "reddit raw metadata sidecar", result.RawMetadata.Data)
	assertNoSignedParamsAtRest(t, "reddit bundle brightdata.json", zipEntry(t, reader, "brightdata.json"))

	rows := usageRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d; want 1", len(rows))
	}
	row := rows[0]
	if row.Product != "web_scraper" || row.DatasetID != DatasetRedditPosts {
		t.Errorf("usage row = %s/%s", row.Product, row.DatasetID)
	}
	if !row.Success || row.Records != 1 {
		t.Errorf("usage row success=%v records=%d", row.Success, row.Records)
	}
	if row.CostUSD != 0.0015 {
		t.Errorf("cost = %v; want 0.0015 (1 record x $0.0015)", row.CostUSD)
	}
	if row.ShortID != "abc12" || row.ArchiveItemID != 7 {
		t.Errorf("usage row not attributed to the item: %+v", row)
	}
}

// A gallery that loses a slide is archived, but it is archived as partial: the
// record said how many images the post has, so the gap is knowable.
func TestArchiveRedditGalleryReportsMissingSlides(t *testing.T) {
	image := fakePNG(t)
	record := map[string]any{
		"post_id":        "t3_gallery",
		"url":            "https://www.reddit.com/r/pics/comments/gallery/",
		"user_posted":    "someone",
		"title":          "Four pictures",
		"community_name": "pics",
		"date_posted":    "2026-08-01T10:00:00.000Z",
		"num_upvotes":    float64(12),
		"num_comments":   float64(3),
		"photos": []any{
			"https://i.redd.it/one.jpg",
			"https://i.redd.it/two.jpg",
			"https://i.redd.it/three.jpg",
			"https://i.redd.it/four.jpg",
		},
	}
	network := newFakeNetwork(record)
	for _, u := range []string{"https://i.redd.it/one.jpg", "https://i.redd.it/two.jpg", "https://i.redd.it/four.jpg"} {
		network.serve(u, image)
	}

	client, db := newTestClient(t, network)
	result, err := client.archiveReddit(context.Background(), "https://www.reddit.com/r/pics/comments/gallery/", utils.ArchiveTypeGalleryDl, io.Discard, db, 1, "gal01")
	if err != nil {
		t.Fatalf("archiveReddit: %v", err)
	}
	if result.Completeness != archivers.CompletenessPartial {
		t.Errorf("completeness = %q; want partial", result.Completeness)
	}
	if result.Metadata != nil {
		t.Error("a multi-image post must not claim single-video metadata")
	}

	reader := resultZip(t, result)
	meta := galleryMetadataFromZip(t, reader)
	if meta.FileCount != 3 {
		t.Errorf("stored %d files; want 3", meta.FileCount)
	}
	if meta.Completeness == nil || meta.Completeness.Expected == nil || *meta.Completeness.Expected != 4 {
		t.Fatalf("expected count = %+v; want 4", meta.Completeness)
	}
	if len(meta.Completeness.MissingIndices) != 1 || meta.Completeness.MissingIndices[0] != 3 {
		t.Errorf("missing indices = %v; want [3]", meta.Completeness.MissingIndices)
	}
	// A thumbnail still comes from the slides that did arrive.
	if result.Thumbnail == nil {
		t.Error("no thumbnail from a gallery of real images")
	}
}

// A dataset failure has to leave an explicit failure and a usage row, because
// a triggered collection can be billable whether or not it produced anything.
func TestArchiveRedditDatasetFailureRecordsUsage(t *testing.T) {
	network := newFakeNetwork()
	network.triggerStatus = 500

	client, db := newTestClient(t, network)
	_, err := client.archiveReddit(context.Background(), redditPostURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 3, "fail1")
	if err == nil {
		t.Fatal("expected an error when the dataset trigger fails")
	}

	rows := usageRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d; want 1", len(rows))
	}
	if rows[0].Success {
		t.Error("failed dataset trigger recorded as a success")
	}
	if !strings.Contains(rows[0].Detail, "trigger failed") {
		t.Errorf("usage detail does not name the failure: %q", rows[0].Detail)
	}
	if rows[0].URL != redditPostURL {
		t.Errorf("usage row URL = %q", rows[0].URL)
	}
}

// A text-only post has nothing to archive, and must say so rather than
// producing an empty bundle.
func TestArchiveRedditTextPostFailsExplicitly(t *testing.T) {
	record := map[string]any{
		"post_id":     "t3_text",
		"url":         "https://www.reddit.com/r/askreddit/comments/text/",
		"user_posted": "someone",
		"title":       "What is your favourite dog breed?",
	}
	client, db := newTestClient(t, newFakeNetwork(record))

	_, err := client.archiveReddit(context.Background(), "https://www.reddit.com/r/askreddit/comments/text/", utils.ArchiveTypeGalleryDl, io.Discard, db, 4, "txt01")
	if err == nil || !strings.Contains(err.Error(), "no media") {
		t.Fatalf("error = %v; want an explicit no-media failure", err)
	}
	rows := usageRows(t, db)
	if len(rows) != 1 || rows[0].Success {
		t.Fatalf("usage rows = %+v; want one unsuccessful row", rows)
	}
}

func TestArchiveRedditRejectsVideoItemType(t *testing.T) {
	client, db := newTestClient(t, newFakeNetwork())
	if _, err := client.archiveReddit(context.Background(), redditPostURL, utils.ArchiveTypeYtDlp, io.Discard, db, 1, "x"); err == nil {
		t.Fatal("Reddit accepted a yt-dlp item; routing never creates one")
	}
	if rows := usageRows(t, db); len(rows) != 0 {
		t.Errorf("spent money on an unroutable item type: %+v", rows)
	}
}
