package archivers

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Shaped like gallery-dl 1.32's TikTok extractor output: the audio file is
// numbered 000 with type "audio", and every file carries the post's "music".
const tiktokSlideSidecar = `{
	"category": "tiktok", "subcategory": "post", "id": "7400000000000000001",
	"num": 1, "extension": "jpg", "type": "image", "title": "three photos",
	"imagePost": {"images": [{"imageURL": {}}, {"imageURL": {}}]},
	"music": {"id": "7300000000000000009", "title": "original sound - someone",
	          "authorName": "someone", "original": true, "duration": 42, "playUrl": "https://sf16.tiktokcdn.com/obj/x.mp3"}
}`

const tiktokAudioSidecar = `{
	"category": "tiktok", "subcategory": "post", "id": "7400000000000000001",
	"num": 0, "extension": "mp3", "type": "audio",
	"music": {"id": "7300000000000000009", "title": "original sound - someone",
	          "authorName": "someone", "original": true, "duration": 42}
}`

// Shaped like gallery-dl's Instagram _extract_audio output: audio numbered
// after the last slide, audio_url only on the audio file, attribution on all.
const instagramAudioSlideSidecar = `{
	"category": "instagram", "subcategory": "post", "post_shortcode": "ABC", "count": 3,
	"num": 1, "extension": "jpg", "audio_title": "Song", "audio_artist": ["A", "B"],
	"audio_user": {"username": "artist"}, "audio_duration": 30.5
}`

const instagramAudioFileSidecar = `{
	"category": "instagram", "subcategory": "post", "post_shortcode": "ABC", "count": 3,
	"num": 3, "extension": "m4a", "audio_url": "https://cdn.example/x.m4a",
	"audio_title": "Song", "audio_artist": ["A", "B"], "audio_user": {"username": "artist"}, "audio_duration": 30.5
}`

func TestSeparateGalleryAudioMovesTikTokTrackOutOfSlides(t *testing.T) {
	dir := writeGalleryFixture(t, map[string]string{
		"000.mp3":      "ID3",
		"000.mp3.json": tiktokAudioSidecar,
		"001.jpg":      "a",
		"001.jpg.json": tiktokSlideSidecar,
		"002.jpg":      "b",
		"002.jpg.json": tiktokSlideSidecar,
	})
	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	slides, audioName, audioSidecar, err := separateGalleryAudio(dir, media, sidecars, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(slides) != 2 || slides[0] != "001.jpg" || slides[1] != "002.jpg" {
		t.Fatalf("slides = %v, want the two photos", slides)
	}
	if audioName != "audio.mp3" || audioSidecar != "audio.mp3.json" {
		t.Fatalf("audio = %q/%q, want audio.mp3 with sidecar", audioName, audioSidecar)
	}
	for _, name := range []string{"audio.mp3", "audio.mp3.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s not on disk: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "000.mp3")); !os.IsNotExist(err) {
		t.Errorf("000.mp3 still present")
	}
	if sidecars["audio.mp3"] != "audio.mp3.json" || sidecars["000.mp3"] != "" {
		t.Errorf("sidecar map not updated: %v", sidecars)
	}

	// Completeness is about slides: TikTok publishes no count, so 2 stored
	// with a clean run is complete, and the audio never counts.
	got := galleryCompleteness(dir, slides, sidecars, audioName != "", false, io.Discard)
	if got.State != CompletenessComplete || got.Stored != 2 || got.Expected == nil || *got.Expected != 2 {
		t.Errorf("completeness = %+v, want complete 2/2 from imagePost.images", got)
	}

	music := resolveGalleryMusic(context.Background(), dir, audioName, audioSidecar, slides, sidecars, io.Discard)
	if music == nil {
		t.Fatal("music = nil")
	}
	if music.Status != GalleryMusicStored || music.File != "audio.mp3" || music.ContentType != "audio/mpeg" || music.Size != 3 {
		t.Errorf("music = %+v", *music)
	}
	if music.Title != "original sound - someone" || music.Artist != "someone" || music.ID != "7300000000000000009" {
		t.Errorf("attribution = %+v", *music)
	}
	if music.Original == nil || !*music.Original || music.DurationSeconds == nil || *music.DurationSeconds != 42 {
		t.Errorf("original/duration = %+v", *music)
	}
}

func TestInstagramAudioIsExcludedFromExpectedCount(t *testing.T) {
	// gallery-dl's Instagram count includes the audio file: a 2-photo post
	// with a track reports count=3 and writes 001, 002 and 003.m4a.
	dir := writeGalleryFixture(t, map[string]string{
		"001.jpg":      "a",
		"001.jpg.json": instagramAudioSlideSidecar,
		"002.jpg":      "b",
		"002.jpg.json": instagramAudioSlideSidecar,
		"003.m4a":      "m4a",
		"003.m4a.json": instagramAudioFileSidecar,
	})
	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	slides, audioName, audioSidecar, err := separateGalleryAudio(dir, media, sidecars, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(slides) != 2 || audioName != "audio.m4a" {
		t.Fatalf("slides = %v audio = %q", slides, audioName)
	}
	got := galleryCompleteness(dir, slides, sidecars, true, false, io.Discard)
	if got.State != CompletenessComplete || got.Expected == nil || *got.Expected != 2 || got.Stored != 2 {
		t.Fatalf("completeness = %+v, want complete 2/2 (count minus the audio)", got)
	}

	music := resolveGalleryMusic(context.Background(), dir, audioName, audioSidecar, slides, sidecars, io.Discard)
	if music == nil || music.Title != "Song" || music.Artist != "A, B" || music.Status != GalleryMusicStored {
		t.Fatalf("music = %+v", music)
	}
}

func TestInstagramAudioNotDownloadedIsMetadataOnly(t *testing.T) {
	// The track was described on every slide but the audio file itself was
	// not written (licensed track with no downloadable URL): count still
	// includes it, so expected is count-1 only when audio really is stored.
	dir := writeGalleryFixture(t, map[string]string{
		"001.jpg":      "a",
		"001.jpg.json": instagramAudioSlideSidecar,
		"002.jpg":      "b",
		"002.jpg.json": instagramAudioSlideSidecar,
	})
	media, sidecars, _ := collectGalleryFiles(dir)
	slides, audioName, audioSidecar, err := separateGalleryAudio(dir, media, sidecars, io.Discard)
	if err != nil || audioName != "" {
		t.Fatalf("audio = %q err = %v", audioName, err)
	}
	music := resolveGalleryMusic(context.Background(), dir, audioName, audioSidecar, slides, sidecars, io.Discard)
	if music == nil || music.Status != GalleryMusicMetadataOnly || music.Title != "Song" || music.File != "" {
		t.Fatalf("music = %+v", music)
	}
	if music.MetadataFile != "001.jpg.json" {
		t.Errorf("metadata_file = %q", music.MetadataFile)
	}
}

func TestGalleryMusicFromSidecarShapes(t *testing.T) {
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(`{"audio_title":"T","audio_artist":"","audio_user":{"username":"u"}}`), &raw); err != nil {
		t.Fatal(err)
	}
	if m := galleryMusicFromSidecar(raw); m == nil || m.Artist != "u" {
		t.Errorf("IG original audio should attribute to audio_user: %+v", m)
	}
	if m := galleryMusicFromSidecar(map[string]interface{}{"music": map[string]interface{}{"title": ""}}); m != nil {
		t.Errorf("empty music object should be nil, got %+v", m)
	}
	if m := galleryMusicFromSidecar(map[string]interface{}{"category": "instagram"}); m != nil {
		t.Errorf("no music keys should be nil, got %+v", m)
	}
}

func TestGalleryAudioFilename(t *testing.T) {
	for name, want := range map[string]bool{
		"audio.mp3": true, "audio.m4a": true, "audio.mp3.json": true,
		"001.jpg": false, "000.mp3": false, "audio": true, "metadata.json": false,
	} {
		if got := GalleryAudioFilename(name); got != want {
			t.Errorf("GalleryAudioFilename(%q) = %v", name, got)
		}
	}
}
