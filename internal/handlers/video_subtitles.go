package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
)

// maxSubtitleServeSize bounds what is buffered to answer one subtitle request.
// The archiver refuses to store anything near this, so it is a backstop against
// a hand-written object rather than a real limit.
const maxSubtitleServeSize = 8 * 1024 * 1024

// subtitleRequestName is the path segment identifying one track: its language
// and format, which is exactly what distinguishes the tracks of one video.
func subtitleRequestName(track archivers.SubtitleTrack) string {
	format := track.Format
	if format == "" {
		format = "vtt"
	}
	return track.Lang + "." + format
}

// videoSubtitleTracks loads the caption tracks recorded in an item's normalized
// metadata. Returns nil for every archive without them, which is most of them.
func videoSubtitleTracks(store storage.Storage, item *models.ArchiveItem) ([]archivers.SubtitleTrack, *archivers.Transcript) {
	if item == nil || item.Status != "completed" || item.MetadataKey == "" {
		return nil, nil
	}
	raw, err := readStoredJSON(store, item.MetadataKey, maxVideoMetadataSize)
	if err != nil {
		return nil, nil
	}
	var meta archivers.VideoMetadata
	if json.Unmarshal(raw, &meta) != nil {
		return nil, nil
	}
	return meta.Subtitles, meta.Transcript
}

// subtitleStorageKey resolves where a track was stored.
//
// The recorded key is authoritative. Deriving one from the item's key base is
// the fallback for an archive written before the key was recorded, and is safe
// because the suffix comes from the metadata rather than from the request.
func subtitleStorageKey(item *models.ArchiveItem, track archivers.SubtitleTrack) string {
	if track.StorageKey != "" {
		return track.StorageKey
	}
	if item.StorageKey == "" || item.Extension == "" || track.ArtifactSuffix == "" {
		return ""
	}
	return strings.TrimSuffix(item.StorageKey, item.Extension) + track.ArtifactSuffix
}

// ServeVideoSubtitle streams one stored caption track.
//
// The requested name is only ever compared against the tracks the archive
// itself recorded, so no request can name an object that this capture does not
// own.
func ServeVideoSubtitle(c *gin.Context, store storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	if redirectIfAlias(c, db, shortID) {
		return
	}
	item, ok := findVideoItem(c, db, shortID)
	if !ok {
		return
	}
	tracks, _ := videoSubtitleTracks(store, &item)
	requested := strings.TrimPrefix(c.Param("name"), "/")
	for _, track := range tracks {
		if !strings.EqualFold(subtitleRequestName(track), requested) {
			continue
		}
		key := subtitleStorageKey(&item, track)
		if key == "" {
			break
		}
		data, err := readStoredJSONRaw(store, key, maxSubtitleServeSize)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "subtitle track temporarily unavailable"})
			return
		}
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Disposition", fmt.Sprintf("inline; filename=%q", shortID+"."+requested))
		c.Data(http.StatusOK, subtitleServeContentType(track.Format), data)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "subtitle track not found for this archive"})
}

// ServeVideoTranscript returns the derived plain-text transcript.
func ServeVideoTranscript(c *gin.Context, store storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	if redirectIfAlias(c, db, shortID) {
		return
	}
	item, ok := findVideoItem(c, db, shortID)
	if !ok {
		return
	}
	_, transcript := videoSubtitleTracks(store, &item)
	if transcript == nil || transcript.Text == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "no transcript is available for this archive"})
		return
	}
	c.Header("X-Content-Type-Options", "nosniff")
	// The language and whether it came from speech recognition travel with the
	// text, so a caller saving the file keeps the provenance.
	c.Header("X-Transcript-Language", transcript.Lang)
	c.Header("X-Transcript-Source", transcript.Source)
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(transcript.Text))
}

func subtitleServeContentType(format string) string {
	switch strings.ToLower(format) {
	case "vtt":
		return "text/vtt; charset=utf-8"
	case "srt":
		return "application/x-subrip; charset=utf-8"
	case "ttml":
		return "application/ttml+xml"
	case "json3":
		return "application/json"
	default:
		return "text/plain; charset=utf-8"
	}
}

// readStoredJSONRaw reads a bounded object without requiring it to be JSON.
func readStoredJSONRaw(store storage.Storage, key string, limit int64) ([]byte, error) {
	reader, err := store.Reader(key)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, limit))
}
