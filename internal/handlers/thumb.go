package handlers

import (
	"crypto/sha256"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/thumbnail"
	"arker/internal/utils"
	"arker/internal/workers"
)

// ServeThumbnail serves the preview image for a capture.
//
// It always returns an image. When no thumbnail exists yet it enqueues
// generation and serves a placeholder with a short cache lifetime, so the next
// page load picks up the real one. Generating inline instead would put a
// multi-hundred-megabyte image decode on the request path, and one dashboard
// render requesting hundreds of thumbnails at once would take the process down.
func ServeThumbnail(c *gin.Context, store storage.Storage, db *gorm.DB, riverClient *river.Client[pgx.Tx]) {
	shortID := c.Param("shortid")
	requestedType := strings.TrimPrefix(c.Param("type"), "/")
	// Alias captures own no items; redirect to the canonical capture's
	// thumbnail so <img> tags pointing at an alias short ID still render.
	if redirectIfAlias(c, db, shortID) {
		return
	}

	var capture models.Capture
	if err := db.Where("short_id = ?", shortID).Preload("ArchiveItems").First(&capture).Error; err != nil {
		// Still an image: a broken <img> in a list of hundreds is worse than a
		// neutral placeholder, and the caller asked for a picture.
		serveThumbnailPlaceholder(c, shortID, "")
		return
	}

	var archivedURL models.ArchivedURL
	db.First(&archivedURL, capture.ArchivedURLID)

	items := capture.ArchiveItems
	if requestedType != "" {
		internal := urlTypeToInternalType(requestedType)
		filtered := make([]models.ArchiveItem, 0, 1)
		for _, item := range items {
			if utils.NormalizeArchiveType(item.Type) == internal {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}

	preference := defaultTypePreference(archivedURL.Original)
	ready, candidate := selectThumbnailItem(items, preference)

	if ready != nil {
		if serveStoredThumbnail(c, store, *ready) {
			return
		}
		// The row points at an object the store cannot produce. Fall through to
		// the placeholder and let the candidate path re-queue it.
	}

	if candidate != nil && riverClient != nil {
		if err := workers.EnqueueThumbnail(c.Request.Context(), riverClient, shortID, candidate.Type); err != nil {
			// Uniqueness violations are the expected case under load, not a fault.
			c.Header("X-Thumbnail-Queued", "error")
		} else {
			c.Header("X-Thumbnail-Queued", "1")
		}
	}

	serveThumbnailPlaceholder(c, shortID, archivedURL.Original)
}

// selectThumbnailItem picks which archive item's thumbnail represents the
// capture, and which item should generate one if none is ready.
//
// Preference order is the same one the viewer uses to choose a default tab, so
// the preview matches what a visitor sees when they follow the link.
func selectThumbnailItem(items []models.ArchiveItem, preference []string) (ready, candidate *models.ArchiveItem) {
	byType := make(map[string][]models.ArchiveItem, len(items))
	for _, item := range items {
		t := utils.NormalizeArchiveType(item.Type)
		byType[t] = append(byType[t], item)
	}

	ordered := make([]models.ArchiveItem, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, t := range preference {
		if group, ok := byType[t]; ok && !seen[t] {
			seen[t] = true
			ordered = append(ordered, group...)
		}
	}
	for _, item := range items {
		if !seen[utils.NormalizeArchiveType(item.Type)] {
			ordered = append(ordered, item)
		}
	}

	for i := range ordered {
		item := ordered[i]
		if item.ThumbnailStatus == models.ThumbnailStatusReady && item.ThumbnailKey != "" {
			if ready == nil {
				ready = &ordered[i]
			}
			continue
		}
		if candidate != nil {
			continue
		}
		if item.Status != "completed" || item.StorageKey == "" {
			continue
		}
		if item.ThumbnailStatus == models.ThumbnailStatusUnavailable {
			continue
		}
		if !thumbnail.CanDeriveFromArchive(item.Type) {
			continue
		}
		candidate = &ordered[i]
	}
	return ready, candidate
}

// serveStoredThumbnail streams a generated thumbnail. It reports whether it
// handled the response.
func serveStoredThumbnail(c *gin.Context, store storage.Storage, item models.ArchiveItem) bool {
	// Thumbnail keys carry an upload nonce and are never rewritten in place, so
	// a matching ETag is a guarantee the bytes are unchanged. Check this before
	// touching storage: a 304 should not cost a read.
	etag := `"` + item.ThumbnailKey + `"`
	if match := c.GetHeader("If-None-Match"); match != "" && strings.Contains(match, etag) {
		c.Header("ETag", etag)
		c.Header("Cache-Control", "public, max-age=86400")
		c.Status(http.StatusNotModified)
		return true
	}

	reader, err := store.Reader(item.ThumbnailKey)
	if err != nil {
		return false
	}
	defer reader.Close()

	// Buffer rather than stream. Thumbnails are tens of KB by construction, and
	// reading first means a storage error still falls back to the placeholder
	// instead of truncating a response we already committed to -- and it lets
	// us send a Content-Length, which caches and HEAD callers expect.
	data, err := io.ReadAll(reader)
	if err != nil || len(data) == 0 {
		return false
	}

	c.Header("ETag", etag)
	c.Header("Cache-Control", "public, max-age=86400")
	c.Header("Content-Length", strconv.Itoa(len(data)))
	c.Data(http.StatusOK, thumbnail.ContentType, data)
	return true
}

// serveThumbnailPlaceholder renders a deterministic stand-in image.
//
// Short max-age on purpose: this response means "not generated yet", and a
// normal refresh should pick up the real thumbnail once the queued job lands.
func serveThumbnailPlaceholder(c *gin.Context, shortID, originalURL string) {
	c.Header("Cache-Control", "public, max-age=60")
	c.Header("Content-Type", "image/svg+xml; charset=utf-8")
	c.Status(http.StatusOK)
	c.Writer.Write([]byte(placeholderSVG(shortID, originalURL)))
}

// placeholderSVG builds a neutral tile carrying the archive's hostname. The hue
// is derived from the short ID so the same archive always looks the same and
// adjacent rows in a list stay visually distinguishable.
func placeholderSVG(shortID, originalURL string) string {
	sum := sha256.Sum256([]byte(shortID))
	hue := int(sum[0]) * 360 / 256

	label := hostLabel(originalURL)
	if label == "" {
		label = shortID
	}
	if len(label) > 28 {
		label = label[:27] + "\u2026"
	}

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="No preview available">`+
		`<rect width="%d" height="%d" fill="hsl(%d,32%%,92%%)"/>`+
		`<text x="50%%" y="50%%" font-family="system-ui,-apple-system,Segoe UI,sans-serif" font-size="28" fill="hsl(%d,28%%,38%%)" text-anchor="middle" dominant-baseline="middle">%s</text>`+
		`</svg>`,
		thumbnail.Width, thumbnail.Height, thumbnail.Width, thumbnail.Height,
		thumbnail.Width, thumbnail.Height, hue,
		hue, html.EscapeString(label))
}

// hostLabel extracts a display hostname from an archived URL.
func hostLabel(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimPrefix(u.Host, "www.")
}

// ThumbnailURL builds the absolute URL for a capture's preview image.
//
// Absolute because the consumers are other services rendering archive cards in
// their own pages, where a root-relative path would resolve against the wrong
// host.
func ThumbnailURL(c *gin.Context, shortID string) string {
	scheme := "http"
	if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if c.Request != nil && c.Request.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/thumb/%s", scheme, c.Request.Host, shortID)
}
