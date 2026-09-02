package apify

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"arker/internal/archivers"
	"arker/internal/utils"
)

// Each platform flow runs end to end through ArchiveFallback against the
// fake network: the actor run, the record selection, the media downloads,
// the artifact layout, the sanitized raw record and the usage row.

func archive(t *testing.T, network *fakeNetwork, targetURL, itemType string) (archivers.Result, string, *fakeNetwork, []string) {
	t.Helper()
	client, db := newTestClient(t, network)
	var log strings.Builder
	result, err := client.ArchiveFallback(context.Background(), targetURL, itemType, &log, db, 7)
	if err != nil {
		t.Fatalf("ArchiveFallback(%s): %v\nlog:\n%s", targetURL, err, log.String())
	}
	rows := usageRows(t, db)
	var details []string
	for _, row := range rows {
		if !row.Success {
			t.Errorf("usage row not marked successful: %+v", row)
		}
		if row.ArchiveItemID != 7 || row.Provider != "apify" || row.Product == "" {
			t.Errorf("usage row not attributed: %+v", row)
		}
		details = append(details, row.Detail)
	}
	return result, log.String(), network, details
}

func archiveFails(t *testing.T, network *fakeNetwork, targetURL, itemType string) error {
	t.Helper()
	client, db := newTestClient(t, network)
	_, err := client.ArchiveFallback(context.Background(), targetURL, itemType, io.Discard, db, 7)
	if err == nil {
		t.Fatalf("ArchiveFallback(%s) succeeded; want failure", targetURL)
	}
	for _, row := range usageRows(t, db) {
		if row.Success {
			t.Errorf("failed rescue marked its usage row successful: %+v", row)
		}
		if row.Detail == "" {
			t.Errorf("failed usage row has no detail: %+v", row)
		}
	}
	return err
}

func assertVideoResult(t *testing.T, result archivers.Result, video []byte) archivers.VideoMetadata {
	t.Helper()
	if result.Extension != ".mp4" || result.ContentType != "video/mp4" || result.Source != "apify" {
		t.Errorf("result = %s %s %s", result.Extension, result.ContentType, result.Source)
	}
	if result.Completeness != archivers.CompletenessComplete {
		t.Errorf("completeness = %q", result.Completeness)
	}
	if data := readResult(t, result); len(data) != len(video) {
		t.Errorf("stored %d bytes; want %d", len(data), len(video))
	}
	if result.RawMetadata == nil {
		t.Fatal("no raw metadata sidecar")
	}
	assertNoSignedParamsAtRest(t, "raw sidecar", result.RawMetadata.Data)
	meta := videoMetadataFromSidecar(t, result.Metadata)
	if meta.SchemaVersion != archivers.VideoMetadataSchemaVersion || meta.Provenance != "apify" || !strings.HasPrefix(meta.Provider, "apify:") {
		t.Errorf("provenance = %+v", meta)
	}
	if meta.Media.SizeBytes != int64(len(video)) || meta.Media.Extension != ".mp4" {
		t.Errorf("media = %+v", meta.Media)
	}
	if meta.PostID == "" || meta.Title == "" || meta.Author == "" || meta.CanonicalURL == "" {
		t.Errorf("metadata incomplete: %+v", meta)
	}
	if meta.PublicationTimestamp == "" && meta.Extractor != "youtube:oembed" {
		t.Errorf("metadata has no publication timestamp: %+v", meta)
	}
	return meta
}

func assertGalleryResult(t *testing.T, result archivers.Result, wantFiles int) (archivers.GalleryMetadata, *zip.Reader) {
	t.Helper()
	if result.Extension != ".zip" || result.Source != "apify" {
		t.Errorf("result = %s %s", result.Extension, result.Source)
	}
	reader := resultZip(t, result)
	if reader.File[0].Name != "metadata.json" {
		t.Errorf("first entry = %q; want metadata.json", reader.File[0].Name)
	}
	assertNoSignedParamsAtRest(t, "bundle "+rawRecordName, zipEntry(t, reader, rawRecordName))
	meta := galleryMetadataFromZip(t, reader)
	if meta.FileCount != wantFiles || len(meta.Files) != wantFiles {
		t.Errorf("files = %d (%v); want %d", meta.FileCount, zipNames(reader), wantFiles)
	}
	if result.Completeness != archivers.CompletenessComplete || meta.Completeness == nil || meta.Completeness.Stored != wantFiles {
		t.Errorf("completeness = %q / %+v", result.Completeness, meta.Completeness)
	}
	if meta.PostID == "" || meta.Author == "" || meta.Date == "" || meta.Subcategory != "apify" || !strings.HasPrefix(meta.ToolVersion, "apify:") {
		t.Errorf("metadata incomplete: %+v", meta)
	}
	if result.Thumbnail == nil {
		t.Error("gallery has no thumbnail")
	}
	return meta, reader
}

// ---- Instagram ----

func TestInstagramReelArchivesVideoWithMusic(t *testing.T) {
	record := loadRecord(t, "instagram_reel.json")
	network := newFakeNetwork(actorRun{Actor: ActorInstagram, Items: []map[string]any{record}, CostUSD: 0.0021})
	video := fakeMP4(4096)
	network.serve(instagramVideoURL(record), video)
	network.serve(stringField(record, "thumbnail_url"), fakeJPEG(t))

	result, _, network, _ := archive(t, network, "https://www.instagram.com/reel/DUp5AL8kxuN/?igsh=abc", utils.ArchiveTypeYtDlp)
	meta := assertVideoResult(t, result, video)
	if meta.Platform != "instagram" || meta.PostID != "DUp5AL8kxuN" || meta.Uploader == "" {
		t.Errorf("metadata = %+v", meta)
	}
	if meta.Music == nil || meta.Music.Title == "" {
		t.Errorf("reel soundtrack attribution missing: %+v", meta.Music)
	}
	if meta.Engagement.Likes == nil || meta.Engagement.Views == nil {
		t.Errorf("engagement = %+v", meta.Engagement)
	}
	if result.Thumbnail == nil {
		t.Error("reel has no poster thumbnail")
	}
	input := network.inputFor(ActorInstagram)
	if urls, _ := input["postUrls"].([]any); len(urls) != 1 || urls[0] != "https://www.instagram.com/p/DUp5AL8kxuN/" {
		t.Errorf("actor input = %v; want the canonical post URL", input)
	}
}

func TestInstagramCarouselArchivesSlidesAndSoundtrack(t *testing.T) {
	record := loadRecord(t, "instagram_carousel.json")
	network := newFakeNetwork(runFor(ActorInstagram, record))
	entries := instagramMediaEntries(record)
	if len(entries) != 5 {
		t.Fatalf("fixture resolves %d slides; want 5", len(entries))
	}
	videos := 0
	for _, entry := range entries {
		if entry.isVideo() {
			videos++
			network.serve(entry.URL, fakeMP4(1024))
		} else {
			network.serve(entry.URL, fakeJPEG(t))
		}
	}
	if videos == 0 {
		t.Fatal("fixture carousel has no video slide")
	}
	audio := instagramAudio(record)
	mp3 := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), make([]byte, 64)...)
	network.serve(audio.URL, mp3)

	result, _, _, _ := archive(t, network, "https://www.instagram.com/p/DckIgOqFIr5/?img_index=1", utils.ArchiveTypeGalleryDl)
	meta, _ := assertGalleryResult(t, result, 5)
	if meta.Extractor != "instagram" || meta.PostID != "DckIgOqFIr5" {
		t.Errorf("metadata = %+v", meta)
	}
	if meta.Music == nil || meta.Music.Status != archivers.GalleryMusicStored || meta.Music.File != "audio.mp3" || meta.Music.Artist != "grgr_playlist" {
		t.Errorf("music = %+v", meta.Music)
	}
	videoFiles := 0
	for _, file := range meta.Files {
		if file.IsVideo {
			videoFiles++
		}
	}
	if videoFiles != videos {
		t.Errorf("stored %d video slides; want %d", videoFiles, videos)
	}
	// A mixed carousel is not one video: no video contract is attached.
	if result.Metadata != nil {
		t.Error("multi-slide gallery attached a video metadata sidecar")
	}
}

func TestInstagramPhotoPostArchivesOneStill(t *testing.T) {
	record := loadRecord(t, "instagram_photo.json")
	network := newFakeNetwork(runFor(ActorInstagram, record))
	entries := instagramMediaEntries(record)
	if len(entries) != 1 || entries[0].isVideo() {
		t.Fatalf("entries = %+v", entries)
	}
	network.serve(entries[0].URL, fakeJPEG(t))
	result, _, _, _ := archive(t, network, "https://www.instagram.com/p/DcOX3hWFiey/", utils.ArchiveTypeGalleryDl)
	meta, _ := assertGalleryResult(t, result, 1)
	if meta.Music != nil {
		t.Errorf("still post reports music: %+v", meta.Music)
	}
	if meta.Description == "" || meta.Likes == nil {
		t.Errorf("metadata = %+v", meta)
	}
}

func TestInstagramDeletedPostIsNotFound(t *testing.T) {
	network := newFakeNetwork(runFor(ActorInstagram, loadRecord(t, "instagram_error.json")))
	err := archiveFails(t, network, "https://www.instagram.com/p/GONE/", utils.ArchiveTypeGalleryDl)
	if !errors.Is(err, errNotFound) {
		t.Errorf("error = %v; want errNotFound", err)
	}
	if len(network.requestedURLs()) > 4 {
		t.Errorf("failed record still tried downloads: %v", network.requestedURLs())
	}
}

func TestInstagramSlideRefusalIsPartial(t *testing.T) {
	record := loadRecord(t, "instagram_carousel.json")
	network := newFakeNetwork(runFor(ActorInstagram, record))
	entries := instagramMediaEntries(record)
	for i, entry := range entries {
		if i == 0 {
			continue // the first slide stays 403
		}
		if entry.isVideo() {
			network.serve(entry.URL, fakeMP4(1024))
		} else {
			network.serve(entry.URL, fakeJPEG(t))
		}
	}
	client, db := newTestClient(t, network)
	result, err := client.ArchiveFallback(context.Background(), "https://www.instagram.com/p/DckIgOqFIr5/", utils.ArchiveTypeGalleryDl, io.Discard, db, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResultData(result)
	meta := galleryMetadataFromZip(t, resultZip(t, result))
	if result.Completeness != archivers.CompletenessPartial || meta.Completeness == nil || meta.Completeness.Stored != 4 || meta.Completeness.Expected == nil || *meta.Completeness.Expected != 5 {
		t.Errorf("completeness = %q / %+v", result.Completeness, meta.Completeness)
	}
	if len(meta.Completeness.MissingIndices) != 1 || meta.Completeness.MissingIndices[0] != 1 {
		t.Errorf("missing indices = %v", meta.Completeness.MissingIndices)
	}
	rows := usageRows(t, db)
	if len(rows) != 1 || !rows[0].Success || !strings.Contains(rows[0].Detail, "4 file(s)") {
		t.Errorf("usage = %+v", rows)
	}
}

// ---- TikTok ----

func TestTikTokVideoDownloadsFromTheStore(t *testing.T) {
	record := loadRecord(t, "tiktok_video.json")
	network := newFakeNetwork(runFor(ActorTikTok, record))
	video := fakeMP4(8192)
	storeURL := tiktokVideoURL(record)
	if !isApifyHost(storeURL) {
		t.Fatalf("fixture video URL is not a store record: %s", storeURL)
	}
	network.serve(storeURL, video)
	network.serve(tiktokPosterURL(record), fakeJPEG(t))

	result, _, network, _ := archive(t, network, "https://www.tiktok.com/@hackclub_latam/video/7653880597500218645", utils.ArchiveTypeYtDlp)
	meta := assertVideoResult(t, result, video)
	if meta.Platform != "tiktok" || meta.PostID != "7653880597500218645" || meta.Author != "hackclub_latam" || meta.Channel != "Macondo" {
		t.Errorf("metadata = %+v", meta)
	}
	if meta.Music == nil || len(meta.Tags) == 0 || meta.Engagement.Views == nil {
		t.Errorf("music/tags/engagement = %+v %v %+v", meta.Music, meta.Tags, meta.Engagement)
	}
	input := network.inputFor(ActorTikTok)
	if on, _ := input["shouldDownloadVideos"].(bool); !on {
		t.Errorf("video run did not ask the actor to store the MP4: %v", input)
	}
}

func TestTikTokSlideshowFallsBackToStoredCopies(t *testing.T) {
	record := loadRecord(t, "tiktok_slideshow.json")
	network := newFakeNetwork(runFor(ActorTikTok, record))
	entries, alternates := tiktokImageEntries(record)
	if len(entries) != 3 || len(alternates) != 3 {
		t.Fatalf("entries = %d, alternates = %d", len(entries), len(alternates))
	}
	// The platform CDN serves the first slide; the rest come from the store.
	network.serve(entries[0].URL, fakeJPEG(t))
	for _, entry := range entries[1:] {
		network.serve(alternates[entry.URL], fakeJPEG(t))
	}
	network.serve(tiktokAudio(record).URL, append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), make([]byte, 32)...))

	result, log, _, _ := archive(t, network, "https://www.tiktok.com/@dct_deepco/photo/7656194593955859720", utils.ArchiveTypeGalleryDl)
	meta, _ := assertGalleryResult(t, result, 3)
	if meta.Extractor != "tiktok" || meta.Music == nil || meta.Music.Status != archivers.GalleryMusicStored {
		t.Errorf("metadata = %+v music = %+v", meta, meta.Music)
	}
	if strings.Count(log, "using the actor's stored copy") != 2 {
		t.Errorf("log does not show two store fallbacks:\n%s", log)
	}
}

func TestTikTokSlideshowOnVideoRouteIsRejected(t *testing.T) {
	network := newFakeNetwork(runFor(ActorTikTok, loadRecord(t, "tiktok_slideshow.json")))
	err := archiveFails(t, network, "https://www.tiktok.com/@dct_deepco/video/7656194593955859720", utils.ArchiveTypeYtDlp)
	if !strings.Contains(err.Error(), "photo slideshow") {
		t.Errorf("error = %v", err)
	}
}

func TestTikTokDeletedPostIsNotFound(t *testing.T) {
	network := newFakeNetwork(runFor(ActorTikTok, loadRecord(t, "tiktok_error.json")))
	if err := archiveFails(t, network, "https://www.tiktok.com/@x/video/1", utils.ArchiveTypeYtDlp); !errors.Is(err, errNotFound) {
		t.Errorf("error = %v; want errNotFound", err)
	}
}

// ---- YouTube ----

func TestYouTubeDownloadsFromTheStoreWithScrapedFacts(t *testing.T) {
	download := loadRecord(t, "youtube_download.json")
	facts := loadRecord(t, "youtube_scrape.json")
	network := newFakeNetwork(
		actorRun{Actor: ActorYouTube, Items: []map[string]any{download}, CostUSD: 0.00525, Pending: true},
		actorRun{Actor: ActorYouTubeMetadata, Items: []map[string]any{facts}, CostUSD: 0.0005},
	)
	video := fakeMP4(3000)
	network.serve(stringField(nestedObject(download, "output"), "url"), video)
	network.serve(stringField(facts, "thumbnailUrl"), fakeJPEG(t))

	result, log, network, _ := archive(t, network, "https://youtu.be/iik25wqIuFo?t=3", utils.ArchiveTypeYtDlp)
	meta := assertVideoResult(t, result, video)
	if meta.Platform != "youtube" || meta.PostID != "iik25wqIuFo" || meta.Channel != "Hack Club" || meta.Uploader != "HackClubHQ" || meta.ChannelID == "" {
		t.Errorf("metadata = %+v", meta)
	}
	if meta.DurationSeconds == nil || *meta.DurationSeconds != 7 || meta.Media.QualityLabel != "1080p" {
		t.Errorf("duration/quality = %v %q", meta.DurationSeconds, meta.Media.QualityLabel)
	}
	if runs := network.startedRuns(); runs[0].polls < 2 {
		t.Errorf("pending run polled %d time(s); want the client to wait for it\n%s", runs[0].polls, log)
	}
	if got := network.startedActors(); len(got) != 2 {
		t.Errorf("actors = %v", got)
	}
	input := network.inputFor(ActorYouTube)
	if urls, _ := input["startUrls"].([]any); len(urls) != 1 || urls[0] != "https://www.youtube.com/watch?v=iik25wqIuFo" {
		t.Errorf("downloader input = %v", input)
	}
}

func TestYouTubeFallsBackToOEmbedWhenScraperFails(t *testing.T) {
	download := loadRecord(t, "youtube_download.json")
	network := newFakeNetwork(
		runFor(ActorYouTube, download),
		actorRun{Actor: ActorYouTubeMetadata, Status: "FAILED", StatusMessage: "Actor crashed"},
	)
	video := fakeMP4(3000)
	network.serve(stringField(nestedObject(download, "output"), "url"), video)
	network.serve("https://www.youtube.com/oembed?format=json&url=https%3A%2F%2Fwww.youtube.com%2Fwatch%3Fv%3Diik25wqIuFo",
		[]byte(`{"title":"oEmbed title","author_name":"Some Channel","author_url":"https://www.youtube.com/@some","thumbnail_url":"https://i.ytimg.com/vi/iik25wqIuFo/hqdefault.jpg"}`))

	client, db := newTestClient(t, network)
	result, err := client.ArchiveFallback(context.Background(), "https://www.youtube.com/watch?v=iik25wqIuFo", utils.ArchiveTypeYtDlp, io.Discard, db, 7)
	if err != nil {
		t.Fatal(err)
	}
	meta := assertVideoResult(t, result, video)
	if meta.Title != "oEmbed title" || meta.Channel != "Some Channel" || meta.Extractor != "youtube:oembed" {
		t.Errorf("metadata = %+v", meta)
	}
	rows := usageRows(t, db)
	if len(rows) != 2 {
		t.Fatalf("usage = %+v", rows)
	}
}

func TestYouTubeUnavailableVideoIsNotFound(t *testing.T) {
	network := newFakeNetwork(runFor(ActorYouTube), runFor(ActorYouTubeMetadata))
	if err := archiveFails(t, network, "https://www.youtube.com/watch?v=abcdefghijk", utils.ArchiveTypeYtDlp); !errors.Is(err, errNotFound) {
		t.Errorf("error = %v; want errNotFound", err)
	}
}

// ---- Facebook ----

func TestFacebookVideoArchivesDeliveryURL(t *testing.T) {
	record := loadRecord(t, "facebook_video.json")
	post := parseFacebookRecord(record, io.Discard)
	if len(post.Entries) != 1 || !post.Entries[0].isVideo() {
		t.Fatalf("fixture parsed as %+v", post)
	}
	network := newFakeNetwork(runFor(ActorFacebook, record))
	video := realMP4(t, true)
	network.serve(post.Entries[0].URL, video)

	result, log, _, _ := archive(t, network, "https://www.facebook.com/HackClubHQ/videos/1847881915262822/", utils.ArchiveTypeYtDlp)
	meta := assertVideoResult(t, result, video)
	if meta.Platform != "facebook" || meta.PostID != "1847881915262822" || meta.Author != "HackClubHQ" || meta.Engagement.Likes == nil || meta.DurationSeconds == nil {
		t.Errorf("metadata = %+v", meta)
	}
	// The video node carries no poster; the row preview is the first frame.
	if result.Thumbnail == nil || len(result.Thumbnail.Data) == 0 || result.Thumbnail.Width == 0 {
		t.Errorf("no poster frame extracted from the video\n%s", log)
	}
}

// Facebook sometimes hands out an ISP-local delivery node that only has an
// AAAA record; without an IPv6 route the download cannot even start, so the
// client resolves again for another node and books the wasted run honestly.
func TestFacebookIPv6OnlyDeliveryHostIsReResolved(t *testing.T) {
	record := loadRecord(t, "facebook_video.json")
	post := parseFacebookRecord(record, io.Discard)
	network := newFakeNetwork(runFor(ActorFacebook, record), runFor(ActorFacebook, record))
	video := realMP4(t, false)
	network.serve(post.Entries[0].URL, video)
	client, db := newTestClient(t, network)
	lookups := 0
	client.resolveHost = func(_ context.Context, host string) ([]net.IP, error) {
		lookups++
		if lookups == 1 {
			return []net.IP{net.ParseIP("2600:6c7f:10:3:face:b00c:0:358e")}, nil
		}
		return []net.IP{net.IPv4(203, 0, 113, 1)}, nil
	}
	var log strings.Builder
	result, err := client.ArchiveFallback(context.Background(), "https://www.facebook.com/HackClubHQ/videos/1847881915262822/", utils.ArchiveTypeYtDlp, &log, db, 7)
	if err != nil {
		t.Fatalf("ArchiveFallback: %v\n%s", err, log.String())
	}
	assertVideoResult(t, result, video)
	if len(network.startedRuns()) != 2 {
		t.Fatalf("actor runs = %d, want 2", len(network.startedRuns()))
	}
	if !strings.Contains(log.String(), "IPv6-only and unreachable") {
		t.Errorf("log does not explain the re-resolution:\n%s", log.String())
	}
	rows := usageRows(t, db)
	if len(rows) != 2 || rows[0].Success || !strings.Contains(rows[0].Detail, "unreachable (IPv6-only)") || !rows[1].Success {
		t.Errorf("usage rows = %+v", rows)
	}
}

// With no IPv6 route and every run landing on an IPv6-only node, the client
// gives up after a bounded number of paid runs instead of looping.
func TestFacebookIPv6OnlyDeliveryHostGivesUpAfterRetries(t *testing.T) {
	record := loadRecord(t, "facebook_video.json")
	network := newFakeNetwork(runFor(ActorFacebook, record), runFor(ActorFacebook, record), runFor(ActorFacebook, record), runFor(ActorFacebook, record))
	client, db := newTestClient(t, network)
	client.resolveHost = func(_ context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("2600:6c7f:10:3:face:b00c:0:358e")}, nil
	}
	_, err := client.ArchiveFallback(context.Background(), "https://www.facebook.com/HackClubHQ/videos/1847881915262822/", utils.ArchiveTypeYtDlp, io.Discard, db, 7)
	if err == nil || !strings.Contains(err.Error(), "IPv6-only") {
		t.Fatalf("err = %v", err)
	}
	if len(network.startedRuns()) != 1+facebookHostRetries {
		t.Errorf("actor runs = %d, want %d", len(network.startedRuns()), 1+facebookHostRetries)
	}
	for _, row := range usageRows(t, db) {
		if row.Success || row.Detail == "" {
			t.Errorf("usage row = %+v", row)
		}
	}
}

func TestFacebookReelPostShapeRetriesAsWatchURL(t *testing.T) {
	first := loadRecord(t, "facebook_reel_post.json")
	second := loadRecord(t, "facebook_reel_video.json")
	network := newFakeNetwork(runFor(ActorFacebook, first), runFor(ActorFacebook, second))
	post := parseFacebookRecord(second, io.Discard)
	video := fakeMP4(2500)
	network.serve(post.Entries[0].URL, video)

	client, db := newTestClient(t, network)
	result, err := client.ArchiveFallback(context.Background(), "https://www.facebook.com/reel/1121155692481577/", utils.ArchiveTypeYtDlp, io.Discard, db, 7)
	if err != nil {
		t.Fatal(err)
	}
	meta := assertVideoResult(t, result, video)
	if meta.PostID != "1121155692481577" {
		t.Errorf("metadata = %+v", meta)
	}
	runs := network.startedRuns()
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	urls, _ := runs[1].input["startUrls"].([]any)
	if len(urls) != 1 || urls[0].(map[string]any)["url"] != "https://www.facebook.com/watch/?v=1121155692481577" {
		t.Errorf("retry input = %v", runs[1].input)
	}
	rows := usageRows(t, db)
	if len(rows) != 2 || !rows[1].Success {
		t.Errorf("usage = %+v", rows)
	}
}

func TestFacebookPostArchivesPhotosAtFullSize(t *testing.T) {
	record := loadRecord(t, "facebook_post.json")
	post := parseFacebookRecord(record, io.Discard)
	if len(post.Entries) != 5 {
		t.Fatalf("fixture parsed %d entries; want 5", len(post.Entries))
	}
	network := newFakeNetwork(runFor(ActorFacebook, record))
	for _, entry := range post.Entries {
		if strings.Contains(entry.URL, "ctp=") {
			t.Errorf("entry still carries the crop parameter: %s", entry.URL)
		}
		network.serve(entry.URL, fakeJPEG(t))
	}
	result, _, _, _ := archive(t, network, "https://www.facebook.com/HackClubHQ/posts/pfbid02kCzvfAri8zqTBvGg4qjBVfbhwtTkHuts4grQGnR7BVsSZvgyCN6Q8dYbypk335CWl", utils.ArchiveTypeGalleryDl)
	meta, _ := assertGalleryResult(t, result, 5)
	if meta.Extractor != "facebook" || meta.PostID != "1697801959051254" || meta.AuthorName != "Hack Club" || meta.Likes == nil {
		t.Errorf("metadata = %+v", meta)
	}
}

func TestFacebookMissingPostIsNotFound(t *testing.T) {
	network := newFakeNetwork(runFor(ActorFacebook, loadRecord(t, "facebook_error.json")))
	if err := archiveFails(t, network, "https://www.facebook.com/reel/1240519287786269", utils.ArchiveTypeYtDlp); !errors.Is(err, errNotFound) {
		t.Errorf("error = %v; want errNotFound", err)
	}
}

// ---- Reddit ----

func TestRedditGalleryArchivesSlides(t *testing.T) {
	record := loadRecord(t, "reddit_text.json")
	entries := redditMediaEntries(record)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	network := newFakeNetwork(runFor(ActorReddit, record))
	for _, entry := range entries {
		network.serve(entry.URL, fakeJPEG(t))
	}
	result, _, _, _ := archive(t, network, "https://www.reddit.com/r/aww/comments/1vtkudd/x/", utils.ArchiveTypeGalleryDl)
	meta, _ := assertGalleryResult(t, result, 2)
	if meta.Extractor != "reddit" || meta.PostID != "1vtkudd" || meta.Author != "" && meta.AuthorName != "r/aww" || meta.Likes == nil || meta.Title == "" {
		t.Errorf("metadata = %+v", meta)
	}
}

func TestRedditVideoMuxesAudioTrack(t *testing.T) {
	video := realMP4(t, false)
	audio := realAudioMP4(t)
	record := loadRecord(t, "reddit_video.json")
	fallback := stringField(nestedObject(record, "media", "reddit_video"), "fallback_url")
	audioURL, err := redditAudioURL(fallback)
	if err != nil || audioURL != "https://v.redd.it/lhfsdmqdqchh1/CMAF_AUDIO_128.mp4" {
		t.Fatalf("audio URL = %q, %v", audioURL, err)
	}
	network := newFakeNetwork(runFor(ActorReddit, record))
	network.serve(fallback, video)
	network.serve(audioURL, audio)
	for _, image := range stringSlice(record, "images") {
		network.serve(image, fakeJPEG(t))
	}

	result, _, _, _ := archive(t, network, "https://www.reddit.com/r/aww/comments/1vf8x6i/x/", utils.ArchiveTypeGalleryDl)
	meta, bundle := assertGalleryResult(t, result, 1)
	if !meta.Files[0].IsVideo {
		t.Errorf("stored file = %+v", meta.Files[0])
	}
	if result.Metadata == nil {
		t.Fatal("single video gallery has no video contract")
	}
	vm := videoMetadataFromSidecar(t, result.Metadata)
	if vm.Platform != "reddit" || vm.DurationSeconds == nil || vm.Media.SizeBytes == 0 {
		t.Errorf("video metadata = %+v", vm)
	}
	mp4 := zipEntry(t, bundle, meta.Files[0].Name)
	if !hasAudioTrack(t, mp4) {
		t.Error("stored Reddit video has no audio track")
	}
}

func TestRedditVideoWithoutAudioIsStoredDirectly(t *testing.T) {
	record := loadRecord(t, "reddit_video.json")
	nestedObject(record, "media", "reddit_video")["has_audio"] = false
	fallback := stringField(nestedObject(record, "media", "reddit_video"), "fallback_url")
	network := newFakeNetwork(runFor(ActorReddit, record))
	video := fakeMP4(2222)
	network.serve(fallback, video)
	network.serve(redditPosterURL(record), fakeJPEG(t))

	result, _, network, _ := archive(t, network, "https://www.reddit.com/r/aww/comments/1vf8x6i/x/", utils.ArchiveTypeGalleryDl)
	meta, bundle := assertGalleryResult(t, result, 1)
	if got := zipEntry(t, bundle, meta.Files[0].Name); len(got) != len(video) {
		t.Errorf("stored %d bytes; want %d", len(got), len(video))
	}
	if network.requested("https://v.redd.it/lhfsdmqdqchh1/CMAF_AUDIO_128.mp4") {
		t.Error("fetched an audio track the record says does not exist")
	}
}

// ---- X ----

func TestXPhotosSkipMockRowsAndRequestOriginals(t *testing.T) {
	records := loadRecords(t, "x_photos.json")
	if len(records) != 2 || stringField(records[1], "type") != "mock_tweet" {
		t.Fatalf("fixture should carry a mock row: %v", records)
	}
	entries := xMediaEntries(records[0])
	if len(entries) == 0 {
		t.Fatal("fixture tweet has no media")
	}
	network := newFakeNetwork(runFor(ActorX, records[1], records[0]))
	for _, entry := range entries {
		if q := mustParseQuery(t, entry.URL); q.Get("name") != "orig" {
			t.Errorf("photo not requested at original size: %s", entry.URL)
		}
		network.serve(entry.URL, fakeJPEG(t))
	}
	result, _, network, _ := archive(t, network, "https://x.com/icyelectronics/status/2063159442393248203", utils.ArchiveTypeGalleryDl)
	meta, _ := assertGalleryResult(t, result, len(entries))
	if meta.Extractor != "twitter" || meta.PostID != "2063159442393248203" || meta.Author != "icyelectronics" {
		t.Errorf("metadata = %+v", meta)
	}
	if ids, _ := network.inputFor(ActorX)["tweetIDs"].([]any); len(ids) != 1 || ids[0] != "2063159442393248203" {
		t.Errorf("actor input = %v", network.inputFor(ActorX))
	}
}

func TestXVideoTakesHighestBitrateVariant(t *testing.T) {
	records := loadRecords(t, "x_video.json")
	entries := xMediaEntries(records[0])
	if len(entries) != 1 || !entries[0].isVideo() || !strings.Contains(entries[0].URL, "1280x720") {
		t.Fatalf("entries = %+v", entries)
	}
	network := newFakeNetwork(runFor(ActorX, records...))
	video := fakeMP4(4444)
	network.serve(entries[0].URL, video)
	network.serve(xPosterURL(records[0]), fakeJPEG(t))

	result, _, _, _ := archive(t, network, "https://twitter.com/NASA/status/2094884934510956691", utils.ArchiveTypeGalleryDl)
	assertGalleryResult(t, result, 1)
	vm := videoMetadataFromSidecar(t, result.Metadata)
	if vm.Platform != "twitter" || vm.Author != "NASA" || vm.DurationSeconds == nil || vm.Engagement.Views == nil || vm.Media.QualityLabel != "1280x720" {
		t.Errorf("video metadata = %+v", vm)
	}
	if vm.Media.SizeBytes != int64(len(video)) {
		t.Errorf("size = %d", vm.Media.SizeBytes)
	}
}

func TestXMissingTweetIsNotFound(t *testing.T) {
	records := loadRecords(t, "x_photos.json")
	network := newFakeNetwork(runFor(ActorX, records[1]))
	if err := archiveFails(t, network, "https://x.com/u/status/999", utils.ArchiveTypeGalleryDl); !errors.Is(err, errNotFound) {
		t.Errorf("error = %v; want errNotFound", err)
	}
}

// ---- Pinterest ----

// Pay-per-event actors report $0 for a few seconds after a run finishes.
// The ledger row is corrected in the background once the charge lands, and
// only its cost column, so the Success/Detail the archive finalized stand.
func TestRunCostSettlesAfterTheRunFinishes(t *testing.T) {
	record := loadRecord(t, "pinterest_image.json")
	scripted := runFor(ActorPinterest, record)
	scripted.SettledCostUSD = 0.00195
	network := newFakeNetwork(scripted)
	network.serve(pinterestMediaEntries(record)[0].URL, fakeJPEG(t))
	client, db := newTestClient(t, network)
	client.costSettleDelays = []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}
	var log strings.Builder
	if _, err := client.ArchiveFallback(context.Background(), "https://www.pinterest.com/pin/2181499817705551/", utils.ArchiveTypeGalleryDl, &log, db, 7); err != nil {
		t.Fatalf("ArchiveFallback: %v", err)
	}
	client.Close()
	rows := usageRows(t, db)
	if len(rows) != 1 || rows[0].CostUSD != 0.00195 || !rows[0].Success || !strings.HasPrefix(rows[0].Detail, "gallery 1 file(s)") {
		t.Errorf("usage rows = %+v", rows)
	}
	if !strings.Contains(log.String(), "cost not yet reported") {
		t.Errorf("log does not mention settlement:\n%s", log.String())
	}
}

func TestPinterestImagePinArchivesOriginal(t *testing.T) {
	record := loadRecord(t, "pinterest_image.json")
	network := newFakeNetwork(runFor(ActorPinterest, record))
	entries := pinterestMediaEntries(record)
	if len(entries) != 1 || !strings.Contains(entries[0].URL, "/originals/") {
		t.Fatalf("entries = %+v", entries)
	}
	network.serve(entries[0].URL, fakeJPEG(t))
	result, _, _, _ := archive(t, network, "https://www.pinterest.com/pin/2181499817705551/", utils.ArchiveTypeGalleryDl)
	meta, _ := assertGalleryResult(t, result, 1)
	if meta.Extractor != "pinterest" || meta.Title == "" {
		t.Errorf("metadata = %+v", meta)
	}
}

func TestPinterestVideoPinPrefersProgressiveMP4(t *testing.T) {
	record := loadRecord(t, "pinterest_video.json")
	network := newFakeNetwork(runFor(ActorPinterest, record))
	progressive := pinterestProgressiveURL(pinterestVideoURL(record))
	if progressive != "https://v1.pinimg.com/videos/mc/720p/c3/ba/ec/c3baec1477f8a6756b2bd791c355237e.mp4" {
		t.Fatalf("progressive URL = %q", progressive)
	}
	video := fakeMP4(6000)
	network.serve(progressive, video)
	network.serve(pinterestImageURL(record), fakeJPEG(t))

	result, _, network, _ := archive(t, network, "https://www.pinterest.com/pin/31103053674023753/", utils.ArchiveTypeGalleryDl)
	meta, _ := assertGalleryResult(t, result, 1)
	if !meta.Files[0].IsVideo {
		t.Errorf("file = %+v", meta.Files[0])
	}
	vm := videoMetadataFromSidecar(t, result.Metadata)
	if vm.Platform != "pinterest" || vm.Uploader != "biodiv0106" || vm.Media.SizeBytes != int64(len(video)) {
		t.Errorf("video metadata = %+v", vm)
	}
	for _, requested := range network.requestedURLs() {
		if strings.HasSuffix(requested, ".m3u8") {
			t.Errorf("HLS manifest fetched although the progressive rendition worked: %s", requested)
		}
	}
}

// ---- helpers ----

func realAudioMP4(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dest := t.TempDir() + "/audio.mp4"
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=0.5", "-c:a", "aac", dest).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot render audio: %v: %s", err, out)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func hasAudioTrack(t *testing.T, mp4 []byte) bool {
	t.Helper()
	path := t.TempDir() + "/check.mp4"
	if err := os.WriteFile(path, mp4, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a", "-show_entries", "stream=codec_type", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Skipf("ffprobe unavailable: %v", err)
	}
	return strings.Contains(string(out), "audio")
}
