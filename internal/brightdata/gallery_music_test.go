package brightdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"arker/internal/archivers"
)

// A TikTok Posts record's music block, as the dataset returns it (lowercase
// keys, play URL on the IP-locked CDN).
func tiktokMusicRecord() map[string]any {
	return map[string]any{
		"post_id": "7400000000000000001",
		"music": map[string]any{
			"id":         "7300000000000000009",
			"title":      "original sound - someone",
			"authorname": "someone",
			"original":   true,
			"playurl":    "https://sf16-ies-music.tiktokcdn.com/obj/x.mp3",
		},
		"original_sound": "original sound - someone",
	}
}

func TestTikTokAudioReadsMusicBlock(t *testing.T) {
	audio := tiktokAudio(tiktokMusicRecord())
	if audio == nil {
		t.Fatal("audio = nil")
	}
	if audio.URL != "https://sf16-ies-music.tiktokcdn.com/obj/x.mp3" || audio.entry().extension() != ".mp3" {
		t.Errorf("url/ext = %q %q", audio.URL, audio.entry().extension())
	}
	if audio.Music.Title != "original sound - someone" || audio.Music.Artist != "someone" || audio.Music.ID != "7300000000000000009" {
		t.Errorf("attribution = %+v", audio.Music)
	}
	if audio.Music.Original == nil || !*audio.Music.Original {
		t.Errorf("original = %v", audio.Music.Original)
	}
	if tiktokAudio(map[string]any{"post_id": "x"}) != nil {
		t.Error("record without music should yield nil")
	}
	if tiktokVideoMusic(tiktokMusicRecord()) == nil {
		t.Error("video attribution should be derived from the same block")
	}
}

func TestInstagramAudioNullIsNoMusic(t *testing.T) {
	// Every observed Instagram Posts record carries audio: null, audio_url: null.
	if instagramAudio(map[string]any{"audio": nil, "audio_url": nil}) != nil {
		t.Error("null audio should yield nil")
	}
	audio := instagramAudio(map[string]any{"audio": map[string]any{"title": "Song", "artist": "A"}, "audio_url": nil})
	if audio == nil || audio.Music.Title != "Song" || audio.Music.Artist != "A" || audio.URL != "" {
		t.Errorf("audio = %+v", audio)
	}
}

// The soundtrack is stored as audio.mp3 beside the slides, never counted as a
// slide, and reported in metadata.json's music block.
func TestBuildGalleryArchiveStoresSoundtrackOutsideTheCount(t *testing.T) {
	client, _ := newTestClient(t, newFakeNetwork())
	image := fakeJPEG(t)
	mp3 := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), make([]byte, 64)...)
	entries := []mediaEntry{{URL: "https://cdn.example/a.jpg", Type: "Photo"}}
	record := tiktokMusicRecord()
	meta := &archivers.GalleryMetadata{SourceURL: "https://example.com/post", Extractor: "tiktok"}

	fetch := func(ctx context.Context, entry mediaEntry, dest string) (int64, error) {
		if entry.isAudio() {
			return int64(len(mp3)), os.WriteFile(dest, mp3, 0o644)
		}
		return int64(len(image)), os.WriteFile(dest, image, 0o644)
	}
	result, completeness, totalBytes, err := client.buildGalleryArchive(context.Background(), entries, tiktokAudio(record), meta, record, fetch, io.Discard)
	if err != nil {
		t.Fatalf("buildGalleryArchive: %v", err)
	}
	if completeness.State != archivers.CompletenessComplete || completeness.Stored != 1 || *completeness.Expected != 1 {
		t.Errorf("completeness = %+v; the audio must not be a slide", completeness)
	}
	if totalBytes != int64(len(image)+len(mp3)) {
		t.Errorf("bytes = %d", totalBytes)
	}
	if meta.FileCount != 1 || len(meta.Files) != 1 {
		t.Errorf("files = %+v", meta.Files)
	}
	if meta.Music == nil || meta.Music.Status != archivers.GalleryMusicStored || meta.Music.File != "audio.mp3" || meta.Music.ContentType != "audio/mpeg" {
		t.Fatalf("music = %+v", meta.Music)
	}

	reader := resultZip(t, result)
	names := strings.Join(zipNames(reader), " ")
	if !strings.Contains(names, "audio.mp3") || !strings.Contains(names, "001.jpg") {
		t.Errorf("zip = %s", names)
	}
	var stored archivers.GalleryMetadata
	if err := json.Unmarshal(zipEntry(t, reader, "metadata.json"), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Music == nil || stored.Music.Title != "original sound - someone" {
		t.Errorf("metadata.json music = %+v", stored.Music)
	}
}

// A soundtrack the CDN refuses degrades to attribution; the slideshow itself
// still reads complete.
func TestBuildGalleryArchiveSoundtrackFailureIsMetadataOnly(t *testing.T) {
	client, _ := newTestClient(t, newFakeNetwork())
	image := fakeJPEG(t)
	entries := []mediaEntry{{URL: "https://cdn.example/a.jpg", Type: "Photo"}}
	record := tiktokMusicRecord()
	meta := &archivers.GalleryMetadata{SourceURL: "https://example.com/post"}

	fetch := func(ctx context.Context, entry mediaEntry, dest string) (int64, error) {
		if entry.isAudio() {
			return 0, fmt.Errorf("status 403")
		}
		return int64(len(image)), os.WriteFile(dest, image, 0o644)
	}
	result, completeness, _, err := client.buildGalleryArchive(context.Background(), entries, tiktokAudio(record), meta, record, fetch, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if completeness.State != archivers.CompletenessComplete {
		t.Errorf("completeness = %+v", completeness)
	}
	if meta.Music == nil || meta.Music.Status != archivers.GalleryMusicMetadataOnly || meta.Music.File != "" || meta.Music.Title == "" {
		t.Errorf("music = %+v", meta.Music)
	}
	if names := strings.Join(zipNames(resultZip(t, result)), " "); strings.Contains(names, "audio") {
		t.Errorf("zip should hold no audio entry: %s", names)
	}
}
