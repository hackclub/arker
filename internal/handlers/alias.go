package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/models"
)

// resolveCaptureForMachineEndpoint follows an alias without issuing an HTTP
// redirect. Machine-facing contracts (manifests, raw metadata, thumbnails)
// promise that the URL the caller was given answers directly and forever.
// Viewer/download routes retain redirectIfAlias below so humans still land on
// the canonical capture page.
func resolveCaptureForMachineEndpoint(db *gorm.DB, shortID string) (models.Capture, error) {
	var capture models.Capture
	if err := db.Where("short_id = ?", shortID).First(&capture).Error; err != nil {
		return models.Capture{}, err
	}
	if capture.AliasOfID == nil {
		return capture, nil
	}
	var canonical models.Capture
	if err := db.First(&canonical, *capture.AliasOfID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return models.Capture{}, gorm.ErrRecordNotFound
		}
		return models.Capture{}, err
	}
	return canonical, nil
}

// redirectIfAlias resolves capture aliases for handlers addressed by short ID.
//
// When shortID names an alias capture (alias_of_id set), it issues a visible
// 302 to the same path with the canonical capture's short ID substituted and
// returns true; the caller must stop handling the request. A visible redirect
// is preferred over silent resolution so the timestamp a user sees always
// matches the URL they land on.
//
// When the capture does not exist or is canonical, it returns false and the
// caller proceeds exactly as before (including its own 404 handling).
func redirectIfAlias(c *gin.Context, db *gorm.DB, shortID string) bool {
	var capture models.Capture
	if err := db.Select("id", "alias_of_id").Where("short_id = ?", shortID).First(&capture).Error; err != nil {
		return false // let the caller produce its usual not-found response
	}
	if capture.AliasOfID == nil {
		return false
	}

	var canonical models.Capture
	if err := db.Select("id", "short_id").First(&canonical, *capture.AliasOfID).Error; err != nil {
		// Should not happen (aliases always point at an existing canonical
		// capture); fall through to the caller rather than break the request.
		return false
	}

	target := replaceShortIDSegment(c.Request.URL.Path, shortID, canonical.ShortID)
	if c.Request.URL.RawQuery != "" {
		target += "?" + c.Request.URL.RawQuery
	}
	c.Redirect(http.StatusFound, target)
	return true
}

// replaceShortIDSegment swaps the first path segment equal to shortID for the
// canonical short ID, leaving every other segment (route prefixes, types,
// file paths) untouched.
func replaceShortIDSegment(path, shortID, canonicalShortID string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment == shortID {
			segments[i] = canonicalShortID
			break
		}
	}
	return strings.Join(segments, "/")
}
