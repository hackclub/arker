package archivers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// GalleryMusic is the sound attached to a post: the track an Instagram photo
// carousel plays or the audio behind a TikTok slideshow. It is part of the
// post as published, so it is captured alongside the slides — the audio file
// itself when the platform lets it be fetched, and the track's attribution
// either way.
//
// The audio is never a slide. It is not counted in FileCount or completeness,
// and it is not listed in Files; a post with music and three photos is a
// three-asset post with a soundtrack, not a four-asset one.
type GalleryMusic struct {
	// Status says whether the audio bytes are in the ZIP. "stored" means File
	// names them; "metadata_only" means the platform described the track but
	// the audio could not be downloaded (licensed tracks are frequently
	// served with no downloadable URL).
	Status string `json:"status"`
	Title  string `json:"title,omitempty"`
	Artist string `json:"artist,omitempty"`
	// ID is the platform's identifier for the sound (Instagram audio asset id,
	// TikTok music id). Stable across posts that reuse the same sound.
	ID string `json:"id,omitempty"`
	// Original is true when the platform marks the sound as the poster's own
	// recording rather than a library track. Nil when the platform did not say.
	Original        *bool    `json:"original,omitempty"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
	// File is the audio entry inside the ZIP when Status is "stored".
	File        string `json:"file,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
	// MetadataFile is the provider sidecar that described the track.
	MetadataFile string `json:"metadata_file,omitempty"`
}

const (
	GalleryMusicStored       = "stored"
	GalleryMusicMetadataOnly = "metadata_only"
)

// galleryAudioBasename is the ZIP entry name for a post's audio track, with
// the extension gallery-dl or the provider gave it (audio.mp3, audio.m4a).
// It is deliberately not numbered: slide numbers are the post's swipe order
// and the audio has no position in it.
const galleryAudioBasename = "audio"

// GalleryAudioFilename reports whether a ZIP entry is the post's audio track.
func GalleryAudioFilename(name string) bool {
	base := strings.TrimSuffix(name, ".json")
	return strings.TrimSuffix(base, filepath.Ext(base)) == galleryAudioBasename
}

// gallerySidecarIsAudio reads the extractor's own word on whether a file is
// the audio track. gallery-dl's TikTok extractor labels it type="audio"; its
// Instagram extractor gives the audio file, and only the audio file, an
// audio_url. The byte sniff cannot be trusted alone here: Instagram serves
// its tracks as .m4a whose ISO BMFF brand reads as an ordinary MP4.
func gallerySidecarIsAudio(raw map[string]interface{}) bool {
	if raw == nil {
		return false
	}
	if strings.EqualFold(galleryString(raw, "type"), "audio") {
		return true
	}
	if galleryString(raw, "audio_url") != "" {
		return true
	}
	return false
}

// separateGalleryAudio moves the post's audio track out of the slide list.
//
// gallery-dl writes the track like any other file — TikTok numbers it 000,
// Instagram numbers it after the last slide — so left alone it would be
// counted as a slide, break the expected-count comparison, and be rendered as
// a broken image. It is renamed to audio.<ext> (sidecar along with it) and
// returned separately. Returns the remaining slides, the audio filename ("" if
// none), and its sidecar name.
func separateGalleryAudio(dir string, media []string, sidecars map[string]string, logWriter io.Writer) ([]string, string, string, error) {
	slides := make([]string, 0, len(media))
	audioName, audioSidecar := "", ""
	for _, name := range media {
		sidecar := sidecars[name]
		var raw map[string]interface{}
		if sidecar != "" {
			raw = readGalleryJSON(filepath.Join(dir, sidecar), logWriter)
		}
		isAudio := gallerySidecarIsAudio(raw) || strings.HasPrefix(galleryContentType(name), "audio/")
		if !isAudio {
			slides = append(slides, name)
			continue
		}
		if audioName != "" {
			// A post has one soundtrack. A second audio file is unexpected;
			// keep it in the bundle but do not pretend it is a slide either.
			fmt.Fprintf(logWriter, "Ignoring additional audio file %s (a post has one soundtrack)\n", name)
			continue
		}
		ext := filepath.Ext(name)
		if raw != nil && strings.EqualFold(galleryString(raw, "extension"), "m4a") {
			ext = ".m4a"
		}
		target := galleryAudioBasename + ext
		if target != name {
			if err := galleryRenameAvailable(dir, name, target); err != nil {
				return nil, "", "", err
			}
			if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, target)); err != nil {
				return nil, "", "", fmt.Errorf("rename %s to %s: %w", name, target, err)
			}
			if sidecar != "" {
				if err := os.Rename(filepath.Join(dir, sidecar), filepath.Join(dir, target+".json")); err != nil {
					return nil, "", "", fmt.Errorf("rename %s to %s: %w", sidecar, target+".json", err)
				}
				delete(sidecars, name)
				sidecars[target] = target + ".json"
				sidecar = target + ".json"
			}
			fmt.Fprintf(logWriter, "Audio track %s stored as %s\n", name, target)
		}
		audioName, audioSidecar = target, sidecar
	}
	sort.Strings(slides)
	return slides, audioName, audioSidecar, nil
}

// galleryMusicFromSidecar reads the track attribution out of an extractor
// record. gallery-dl merges post-level fields into every file's dict, so the
// slide sidecars carry it whether or not the audio downloaded.
//
// Shapes handled:
//   - TikTok: a "music" object {id, title, authorName, original, duration}
//   - Instagram: flat audio_title / audio_artist / audio_user /
//     audio_duration, written by gallery-dl's _extract_audio
//   - Historical Bright Data TikTok bundles: a "music" object with lowercase
//     keys {id, title, authorname, original, playurl}. Apify bundles do not
//     go through here; the builder sets GalleryMusic directly.
func galleryMusicFromSidecar(raw map[string]interface{}) *GalleryMusic {
	if raw == nil {
		return nil
	}
	if music := galleryObject(raw, "music"); music != nil {
		result := &GalleryMusic{
			Title:  galleryString(music, "title"),
			Artist: galleryString(music, "authorName", "authorname", "author", "artist"),
			ID:     galleryString(music, "id", "music_id"),
		}
		if original, ok := music["original"].(bool); ok {
			result.Original = &original
		}
		if duration := galleryFloat(music, "duration"); duration != nil && *duration > 0 {
			result.DurationSeconds = duration
		}
		if result.Title != "" || result.Artist != "" || result.ID != "" {
			return result
		}
	}
	if title := galleryString(raw, "audio_title"); title != "" || galleryString(raw, "audio_artist") != "" {
		result := &GalleryMusic{
			Title:  title,
			Artist: galleryArtist(raw, "audio_artist"),
		}
		if result.Artist == "" {
			// A poster's own recording has no display artist; the account it
			// belongs to is the attribution.
			if user := galleryObject(raw, "audio_user"); user != nil {
				result.Artist = galleryString(user, "username", "full_name")
			} else {
				result.Artist = galleryString(raw, "audio_user")
			}
		}
		if duration := galleryFloat(raw, "audio_duration"); duration != nil && *duration > 0 {
			result.DurationSeconds = duration
		}
		return result
	}
	return nil
}

// galleryArtist reads an artist field that may be a string or, for a track
// with several credited artists, a list of strings.
func galleryArtist(raw map[string]interface{}, key string) string {
	if single := galleryString(raw, key); single != "" {
		return single
	}
	return strings.Join(galleryStrings(raw, key), ", ")
}

// galleryFloat reads a number that may be an integer or a float from the top
// level of a decoded record.
func galleryFloat(raw map[string]interface{}, keys ...string) *float64 {
	for _, key := range keys {
		switch value := raw[key].(type) {
		case json.Number:
			if f, err := value.Float64(); err == nil {
				return &f
			}
		case float64:
			f := value
			return &f
		}
	}
	return nil
}

// resolveGalleryMusic assembles the normalized music record for a native
// gallery-dl capture: attribution from the sidecars, bytes from the audio file
// if one was downloaded. Nil when the post carries no sound.
func resolveGalleryMusic(ctx context.Context, dir, audioName, audioSidecar string, slides []string, sidecars map[string]string, logWriter io.Writer) *GalleryMusic {
	var music *GalleryMusic
	if audioSidecar != "" {
		music = galleryMusicFromSidecar(readGalleryJSON(filepath.Join(dir, audioSidecar), logWriter))
	}
	if music == nil {
		for _, name := range slides {
			sidecar, ok := sidecars[name]
			if !ok {
				continue
			}
			if music = galleryMusicFromSidecar(readGalleryJSON(filepath.Join(dir, sidecar), logWriter)); music != nil {
				music.MetadataFile = sidecar
				break
			}
		}
	} else {
		music.MetadataFile = audioSidecar
	}

	if audioName == "" {
		if music != nil {
			music.Status = GalleryMusicMetadataOnly
		}
		return music
	}
	if music == nil {
		music = &GalleryMusic{}
	}
	music.Status = GalleryMusicStored
	music.File = audioName
	music.ContentType = galleryContentType(audioName)
	if info, err := os.Stat(filepath.Join(dir, audioName)); err == nil {
		music.Size = info.Size()
	}
	if music.DurationSeconds == nil {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		probe, err := ProbeVideoFile(probeCtx, filepath.Join(dir, audioName))
		cancel()
		if err == nil && probe.DurationSeconds != nil {
			music.DurationSeconds = probe.DurationSeconds
		}
	}
	return music
}

// logGalleryMusic writes the soundtrack outcome where an operator reads it.
func logGalleryMusic(logWriter io.Writer, music *GalleryMusic) {
	if music == nil {
		fmt.Fprintf(logWriter, "Music: none attached to this post\n")
		return
	}
	attribution := music.Title
	if music.Artist != "" {
		attribution = fmt.Sprintf("%s — %s", music.Title, music.Artist)
	}
	if attribution == "" {
		attribution = "(untitled)"
	}
	switch music.Status {
	case GalleryMusicStored:
		fmt.Fprintf(logWriter, "Music: %s (stored as %s, %d bytes)\n", attribution, music.File, music.Size)
	default:
		fmt.Fprintf(logWriter, "Music: %s (metadata only; audio not downloadable)\n", attribution)
	}
}
