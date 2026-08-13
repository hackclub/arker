package archivers

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mp4HandlerTypes lists the media handlers declared by an MP4 file, one per
// track: "vide" for video, "soun" for audio.
//
// This is how these tests tell a real muxed download from a DASH video stream
// with the audio left behind — the exact failure mode v.redd.it produces when
// nothing merges the two streams. It walks the box tree (moov > trak > mdia >
// hdlr) rather than shelling out to ffprobe, which is not a test dependency.
func mp4HandlerTypes(data []byte) []string {
	var handlers []string

	var walk func(b []byte)
	walk = func(b []byte) {
		for len(b) >= 8 {
			size := int(binary.BigEndian.Uint32(b[:4]))
			boxType := string(b[4:8])
			header := 8
			switch {
			case size == 1: // 64-bit extended size
				if len(b) < 16 {
					return
				}
				size = int(binary.BigEndian.Uint64(b[8:16]))
				header = 16
			case size == 0: // box runs to end of file
				size = len(b)
			}
			if size < header || size > len(b) {
				return
			}
			payload := b[header:size]

			switch boxType {
			case "moov", "trak", "mdia", "minf", "stbl":
				walk(payload)
			case "hdlr":
				// version+flags(4) predefined(4) then the handler type.
				if len(payload) >= 12 {
					handlers = append(handlers, string(payload[8:12]))
				}
			}
			b = b[size:]
		}
	}
	walk(data)
	return handlers
}

func hasHandler(handlers []string, want string) bool {
	for _, handler := range handlers {
		if handler == want {
			return true
		}
	}
	return false
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// zipEntries reads back everything writeGalleryZip produced, so a test can
// assert on the bytes that actually reach storage rather than on the temp dir.
func zipEntries(t *testing.T, dir string, metadataJSON []byte) map[string][]byte {
	t.Helper()

	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)
	if err := writeGalleryZip(zipWriter, dir, metadataJSON, io.Discard); err != nil {
		t.Fatalf("writeGalleryZip: %v", err)
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	reader, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	entries := make(map[string][]byte, len(reader.File))
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("open %s in zip: %v", file.Name, err)
		}
		contents, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s in zip: %v", file.Name, err)
		}
		entries[file.Name] = contents
	}
	return entries
}

// The control for every audio assertion below: the fixtures really do differ,
// so a test that checks for a "soun" track can fail.
func TestMP4HandlerTypesDistinguishesMuxedFromVideoOnly(t *testing.T) {
	muxed := mp4HandlerTypes(readFixture(t, "muxed_video_audio_sample.mp4"))
	if !hasHandler(muxed, "vide") || !hasHandler(muxed, "soun") {
		t.Fatalf("muxed fixture handlers = %v, want both vide and soun", muxed)
	}

	videoOnly := mp4HandlerTypes(readFixture(t, "video_only_sample.mp4"))
	if !hasHandler(videoOnly, "vide") {
		t.Fatalf("video-only fixture handlers = %v, want vide", videoOnly)
	}
	if hasHandler(videoOnly, "soun") {
		t.Fatalf("video-only fixture handlers = %v, want no soun track", videoOnly)
	}
}

// G3c. reddit serves v.redd.it as DASH with the audio in a separate stream, so
// the archive is only a real archive if something muxes them. gallery-dl always
// delegates that to yt-dlp; galleryDlDownloadArgs decides what it hands over.
func TestGalleryDlDownloadArgsEnableRedditYtdlIntegration(t *testing.T) {
	args := galleryDlDownloadArgs("/tmp/out")

	found := false
	for i, arg := range args {
		if arg != "-o" || i+1 >= len(args) {
			continue
		}
		if args[i+1] == "extractor.reddit.videos=ytdl" {
			found = true
		}
		// The default is "dash", which only parses reddit's DASH manifest: if
		// that manifest lists no audio, the archive is silently video-only.
		if args[i+1] == "extractor.reddit.videos=dash" {
			t.Errorf("args pin reddit videos to dash, which cannot recover audio the manifest omits")
		}
	}
	if !found {
		t.Errorf("gallery-dl args %v missing -o extractor.reddit.videos=ytdl", args)
	}
}

// A reddit video post, as gallery-dl lays it out once yt-dlp has merged the
// DASH video and audio streams: the archive must carry both tracks all the way
// into the ZIP, byte for byte, and metadata.json must describe it as video.
func TestRedditVideoArchiveStoresVideoAndAudioBytes(t *testing.T) {
	video := readFixture(t, "muxed_video_audio_sample.mp4")
	sidecar := readFixture(t, "gallery_dl_reddit_video_sidecar.json")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "000.mp4"), video, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "000.mp4.json"), sidecar, 0o600); err != nil {
		t.Fatal(err)
	}

	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}
	metadata := buildGalleryMetadata(dir, "https://www.reddit.com/r/aww/comments/1abc234/otter_figures_out_the_tap/",
		"1.32.9", media, sidecars, io.Discard)

	if metadata.Extractor != "reddit" {
		t.Errorf("extractor = %q, want reddit", metadata.Extractor)
	}
	if metadata.Title != "Otter figures out the tap" {
		t.Errorf("title = %q, want the submission title", metadata.Title)
	}
	if metadata.Author != "example_user" {
		t.Errorf("author = %q, want example_user", metadata.Author)
	}
	if len(metadata.Files) != 1 {
		t.Fatalf("files = %v, want exactly the merged video", metadata.Files)
	}
	file := metadata.Files[0]
	if !file.IsVideo {
		t.Errorf("file %q is_video = false, want true", file.Name)
	}
	if file.Size != int64(len(video)) {
		t.Errorf("file size = %d, want %d (the whole downloaded file)", file.Size, len(video))
	}

	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	entries := zipEntries(t, dir, metadataJSON)

	stored, ok := entries["000.mp4"]
	if !ok {
		t.Fatalf("ZIP entries = %v, want the video", keys2(entries))
	}
	if !bytes.Equal(stored, video) {
		t.Fatalf("stored video differs from the downloaded bytes (%d vs %d bytes)", len(stored), len(video))
	}

	handlers := mp4HandlerTypes(stored)
	if !hasHandler(handlers, "vide") {
		t.Errorf("archived reddit video has no video track (handlers %v)", handlers)
	}
	if !hasHandler(handlers, "soun") {
		t.Errorf("archived reddit video has no audio track (handlers %v): v.redd.it DASH audio was not merged", handlers)
	}

	// The raw submission record travels with it, so the DASH/HLS URLs and
	// has_audio flag stay auditable after the fact.
	raw, ok := entries["000.mp4.json"]
	if !ok {
		t.Fatalf("ZIP entries = %v, want the raw sidecar", keys2(entries))
	}
	if !strings.Contains(string(raw), "\"has_audio\": true") {
		t.Error("raw reddit sidecar lost the reddit_video record")
	}
}

// G3d. Bluesky serves the poster's original upload as an atproto blob, so
// gallery-dl's download is the source file itself: verified live on
// 2026-08-12 against bsky.app/profile/bsky.app/post/3mgdqhzxy7s2u, where the
// blob size declared in the post record (251820) matched the downloaded file
// exactly. This pins that invariant — a stored file shorter than the declared
// blob is a truncated archive, not a complete one.
func TestBlueskyVideoArchiveStoresTheWholeSourceBlob(t *testing.T) {
	video := readFixture(t, "muxed_video_audio_sample.mp4")
	sidecar := readFixture(t, "gallery_dl_bluesky_video_sidecar.json")

	var record map[string]any
	if err := json.Unmarshal(sidecar, &record); err != nil {
		t.Fatalf("parse bluesky fixture: %v", err)
	}
	blob := record["embed"].(map[string]any)["video"].(map[string]any)
	if blob["mimeType"] != "video/mp4" {
		t.Fatalf("fixture blob mimeType = %v, want video/mp4", blob["mimeType"])
	}
	if size, ok := blob["size"].(float64); !ok || int64(size) != 251820 {
		t.Fatalf("fixture blob size = %v, want the 251820 bytes observed live", blob["size"])
	}
	// The committed fixture keeps the real blob size from that live capture;
	// the stand-in media file is a tiny generated clip, so declare its size
	// here instead of shipping a 250KB binary just to make the numbers line up.
	blob["size"] = len(video)
	patched, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001.mp4"), video, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "001.mp4.json"), patched, 0o600); err != nil {
		t.Fatal(err)
	}

	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}
	metadata := buildGalleryMetadata(dir, "https://bsky.app/profile/bsky.app/post/3mgdqhzxy7s2u",
		"1.32.9", media, sidecars, io.Discard)

	if metadata.Extractor != "bluesky" {
		t.Errorf("extractor = %q, want bluesky", metadata.Extractor)
	}
	if metadata.Author != "bsky.app" {
		t.Errorf("author = %q, want the poster handle", metadata.Author)
	}
	if metadata.PostID != "3mgdqhzxy7s2u" {
		t.Errorf("post_id = %q, want 3mgdqhzxy7s2u", metadata.PostID)
	}
	if len(metadata.Files) != 1 || !metadata.Files[0].IsVideo {
		t.Fatalf("files = %+v, want one video", metadata.Files)
	}
	if metadata.Files[0].Size != int64(len(video)) {
		t.Errorf("stored size = %d, want the whole %d-byte source blob", metadata.Files[0].Size, len(video))
	}

	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored := zipEntries(t, dir, metadataJSON)["001.mp4"]
	if !bytes.Equal(stored, video) {
		t.Fatalf("stored blob differs from the download (%d vs %d bytes)", len(stored), len(video))
	}
	// Bluesky hands back the file the author uploaded, so whatever audio it
	// had is in the archive; nothing has to be merged after the fact.
	if handlers := mp4HandlerTypes(stored); !hasHandler(handlers, "soun") {
		t.Errorf("archived bluesky blob lost its audio track (handlers %v)", handlers)
	}
}

// G3b. With cookies, gallery-dl's X/Twitter extractor picks the highest-bitrate
// entry of video_info.variants — a plain MP4 on video.twimg.com, downloaded
// like any other file, with no ytdl module involved (gallery-dl 1.32.9
// extractor/twitter.py:230-256). Verified by code reading and this fixture; no
// live authenticated X request was made.
func TestTwitterVideoArchiveStoresTheMP4Variant(t *testing.T) {
	video := readFixture(t, "muxed_video_audio_sample.mp4")
	sidecar := readFixture(t, "gallery_dl_twitter_video_sidecar.json")

	var record map[string]any
	if err := json.Unmarshal(sidecar, &record); err != nil {
		t.Fatalf("parse twitter fixture: %v", err)
	}
	// The selected variant must be a real MP4 file, not an HLS playlist: a
	// .m3u8 would mean gallery-dl stored a manifest instead of the video.
	sourceURL, _ := record["url"].(string)
	if !strings.HasSuffix(strings.Split(sourceURL, "?")[0], ".mp4") {
		t.Fatalf("variant url = %q, want an .mp4 variant", sourceURL)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001.mp4"), video, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "001.mp4.json"), sidecar, 0o600); err != nil {
		t.Fatal(err)
	}

	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}
	metadata := buildGalleryMetadata(dir, "https://x.com/example_user/status/1234567890123456789",
		"1.32.9", media, sidecars, io.Discard)

	if metadata.Extractor != "twitter" {
		t.Errorf("extractor = %q, want twitter", metadata.Extractor)
	}
	// Post IDs above 2^53 must survive as exact digits.
	if metadata.PostID != "1234567890123456789" {
		t.Errorf("post_id = %q, want 1234567890123456789", metadata.PostID)
	}
	if len(metadata.Files) != 1 || !metadata.Files[0].IsVideo {
		t.Fatalf("files = %+v, want one video", metadata.Files)
	}
	if metadata.Files[0].ContentType != "video/mp4" {
		t.Errorf("content type = %q, want video/mp4", metadata.Files[0].ContentType)
	}

	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored := zipEntries(t, dir, metadataJSON)["001.mp4"]
	if !bytes.Equal(stored, video) {
		t.Fatalf("stored video differs from the download (%d vs %d bytes)", len(stored), len(video))
	}
	handlers := mp4HandlerTypes(stored)
	if !hasHandler(handlers, "vide") || !hasHandler(handlers, "soun") {
		t.Errorf("archived X video handlers = %v, want both vide and soun", handlers)
	}
}

// gallery-dl imports yt-dlp as a Python module for every "ytdl:" URL, so an
// installation where the two live in different environments downloads no video
// at all and reports only a generic exit code. That must read as an actionable
// failure, never as an empty-but-successful archive.
func TestPhraseWatcherDetectsMissingYtdlModule(t *testing.T) {
	var out bytes.Buffer
	watcher := newPhraseWatcher(&out, galleryDlYtdlImportError)

	if watcher.Seen() {
		t.Fatal("watcher reported the phrase before anything was written")
	}
	if _, err := watcher.Write([]byte("[reddit][info] Downloading submission\n")); err != nil {
		t.Fatal(err)
	}
	if watcher.Seen() {
		t.Fatal("watcher matched unrelated output")
	}

	// Split across writes exactly the way pipe reads split a log line.
	if _, err := watcher.Write([]byte("[ytdl][error] Cannot import yt-")); err != nil {
		t.Fatal(err)
	}
	if _, err := watcher.Write([]byte("dlp or youtube-dl\n")); err != nil {
		t.Fatal(err)
	}
	if !watcher.Seen() {
		t.Error("watcher missed the import error split across two writes")
	}

	// Everything still reaches the log untouched.
	want := "[reddit][info] Downloading submission\n[ytdl][error] Cannot import yt-dlp or youtube-dl\n"
	if out.String() != want {
		t.Errorf("passthrough output = %q, want %q", out.String(), want)
	}
}

func TestMissingYtdlMessageNamesTheFix(t *testing.T) {
	for _, want := range []string{"yt-dlp", "same Python environment", "gallery-dl"} {
		if !strings.Contains(galleryDlMissingYtdlMessage, want) {
			t.Errorf("missing-ytdl message %q should mention %q", galleryDlMissingYtdlMessage, want)
		}
	}
}

func keys2(m map[string][]byte) []string {
	result := make([]string, 0, len(m))
	for key := range m {
		result = append(result, key)
	}
	return result
}
