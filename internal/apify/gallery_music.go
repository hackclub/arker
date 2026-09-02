package apify

import (
	"context"
	"fmt"
	"io"
	"os"
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
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	probe, probeErr := archivers.ProbeVideoFile(probeCtx, filepath.Join(dir, canonicalName))
	cancel()
	if contentType == "video/mp4" && probeErr == nil && probe.VideoCodec == "" && probe.AudioCodec != "" {
		// Instagram serves licensed tracks as an MP4 container with only an
		// audio stream and a generic "isom" brand, which byte-sniffs as video.
		// The streams are the fact: no video stream means it is a soundtrack.
		renamed := strings.TrimSuffix(canonicalName, filepath.Ext(canonicalName)) + ".m4a"
		if err := os.Rename(filepath.Join(dir, canonicalName), filepath.Join(dir, renamed)); err == nil {
			canonicalName = renamed
		}
		contentType = "audio/mp4"
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
	if music.DurationSeconds == nil && probeErr == nil && probe.DurationSeconds != nil {
		music.DurationSeconds = probe.DurationSeconds
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
