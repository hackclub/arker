package brightdata

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"arker/internal/archivers"
)

// galleryAudio is a post's soundtrack as a provider record describes it: the
// attribution Arker will store, plus the CDN URL to fetch the bytes from when
// the record offers one.
type galleryAudio struct {
	URL   string
	Music archivers.GalleryMusic
}

// audioEntry turns the soundtrack into a media entry for the platform's
// fetcher, so TikTok's IP-locked audio CDN goes through the same browser
// fallback its stills use.
func (a *galleryAudio) entry() mediaEntry {
	return mediaEntry{URL: a.URL, Type: "Audio"}
}

// tiktokAudio reads the sound off a TikTok Posts record. The dataset carries
// it as a "music" object ({id, title, authorname, original, playurl, ...}) and
// separately as an "original_sound" label.
func tiktokAudio(record map[string]any) *galleryAudio {
	music, ok := record["music"].(map[string]any)
	if !ok {
		return nil
	}
	audio := &galleryAudio{
		URL: stringField(music, "playurl", "playUrl", "play_url"),
		Music: archivers.GalleryMusic{
			Status: archivers.GalleryMusicMetadataOnly,
			Title:  stringField(music, "title"),
			Artist: stringField(music, "authorname", "authorName", "author"),
			ID:     stringField(music, "id"),
		},
	}
	if original, ok := music["original"].(bool); ok {
		audio.Music.Original = &original
	}
	if duration := floatField(music, "duration"); duration != nil && *duration > 0 {
		audio.Music.DurationSeconds = duration
	}
	if audio.URL == "" && audio.Music.Title == "" && audio.Music.ID == "" {
		return nil
	}
	return audio
}

// instagramAudio reads the sound off an Instagram Posts record. The dataset
// declares "audio" and "audio_url" fields; every record observed so far has
// both null, so this is written to the declared shape (a string or an object
// with title/artist keys) and simply yields nothing when they are empty.
func instagramAudio(record map[string]any) *galleryAudio {
	audio := &galleryAudio{
		URL:   stringField(record, "audio_url"),
		Music: archivers.GalleryMusic{Status: archivers.GalleryMusicMetadataOnly},
	}
	switch typed := record["audio"].(type) {
	case string:
		audio.Music.Title = strings.TrimSpace(typed)
	case map[string]any:
		audio.Music.Title = stringField(typed, "title", "audio_title", "name", "original_audio_title")
		audio.Music.Artist = stringField(typed, "artist", "display_artist", "audio_artist", "author", "ig_artist")
		audio.Music.ID = stringField(typed, "id", "audio_id", "audio_asset_id")
		if audio.URL == "" {
			audio.URL = stringField(typed, "url", "audio_url", "progressive_download_url")
		}
	}
	if audio.URL == "" && audio.Music.Title == "" && audio.Music.Artist == "" {
		return nil
	}
	return audio
}

// fetchGalleryAudio stores the soundtrack next to the slides as audio.<ext>
// and fills in meta.Music. A failed audio download degrades to metadata_only
// rather than failing the post: the slides are the post, the track is its
// soundtrack, and completeness is a claim about slides.
func fetchGalleryAudio(ctx context.Context, dir string, audio *galleryAudio, meta *archivers.GalleryMetadata, fetch mediaFetcher, logWriter io.Writer) int64 {
	if audio == nil {
		return 0
	}
	music := audio.Music
	music.Status = archivers.GalleryMusicMetadataOnly
	meta.Music = &music
	if audio.URL == "" {
		fmt.Fprintf(logWriter, "Music: %s (record offers no audio URL; attribution only)\n", galleryMusicLabel(&music))
		return 0
	}

	entry := audio.entry()
	name := "audio" + entry.extension()
	path := filepath.Join(dir, name)
	fmt.Fprintf(logWriter, "Downloading %s (soundtrack)...\n", name)
	size, err := fetch(ctx, entry, path)
	if err == nil {
		err = rejectHTMLDocument(path)
	}
	if err != nil {
		removeFile(path)
		fmt.Fprintf(logWriter, "Music: %s (audio download failed: %v; attribution only)\n", galleryMusicLabel(&music), err)
		return 0
	}
	canonicalName, contentType, err := canonicalizeDownloadedGalleryMedia(dir, name, logWriter)
	if err != nil {
		removeFile(path)
		fmt.Fprintf(logWriter, "Music: %s (could not inspect audio: %v; attribution only)\n", galleryMusicLabel(&music), err)
		return 0
	}
	if !strings.HasPrefix(contentType, "audio/") {
		// A soundtrack that sniffs as something else is not a soundtrack; do
		// not store an unknown blob under that name.
		removeFile(filepath.Join(dir, canonicalName))
		fmt.Fprintf(logWriter, "Music: %s (download was %s, not audio; attribution only)\n", galleryMusicLabel(&music), contentType)
		return 0
	}
	music.Status = archivers.GalleryMusicStored
	music.File = canonicalName
	music.ContentType = contentType
	music.Size = size
	if music.DurationSeconds == nil {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if probe, err := archivers.ProbeVideoFile(probeCtx, filepath.Join(dir, canonicalName)); err == nil && probe.DurationSeconds != nil {
			music.DurationSeconds = probe.DurationSeconds
		}
		cancel()
	}
	meta.Music = &music
	fmt.Fprintf(logWriter, "Music: %s (stored as %s, %d bytes)\n", galleryMusicLabel(&music), canonicalName, size)
	return size
}

func galleryMusicLabel(music *archivers.GalleryMusic) string {
	switch {
	case music.Title != "" && music.Artist != "":
		return music.Title + " — " + music.Artist
	case music.Title != "":
		return music.Title
	case music.Artist != "":
		return music.Artist
	default:
		return "(untitled)"
	}
}

