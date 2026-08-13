package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/utils"
)

func postFindOrCreate(t *testing.T, r http.Handler, key, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/archive/find-or-create", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	var parsed map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return w.Code, parsed
}

func seedCompletedCapture(t *testing.T, db *gorm.DB, url, shortID string, age time.Duration, types ...string) {
	t.Helper()
	u := models.ArchivedURL{Original: url, CanonicalURL: utils.CanonicalizeArchiveURL(url)}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	capture := models.Capture{ArchivedURLID: u.ID, Timestamp: time.Now().Add(-age), ShortID: shortID}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatal(err)
	}
	for _, typ := range types {
		if err := db.Create(&models.ArchiveItem{CaptureID: capture.ID, Type: typ, Status: "completed"}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

// TestApiFindOrCreateMatchesAcrossSpellingsEndToEnd drives the whole stack:
// a two-year-old archive stored under a youtu.be share link answers a request
// made with the watch URL, with no types specified so the archive types are
// derived from the URL exactly as a real caller would get them.
func TestApiFindOrCreateMatchesAcrossSpellingsEndToEnd(t *testing.T) {
	r, db, key := newFindOrCreateHandlerTest(t)
	seedCompletedCapture(t, db, "https://youtu.be/dQw4w9WgXcQ?si=Tr4ck1ng", "ytcanon",
		2*365*24*time.Hour, "mhtml", "screenshot", "yt-dlp")

	code, body := postFindOrCreate(t, r, key, `{"url":"https://www.youtube.com/watch?v=dQw4w9WgXcQ"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %#v", code, body)
	}
	if body["action"] != "found" || body["short_id"] != "ytcanon" || body["status"] != "completed" {
		t.Fatalf("body = %#v, want the archive stored under the other spelling", body)
	}

	var captures int64
	db.Model(&models.Capture{}).Count(&captures)
	if captures != 1 {
		t.Fatalf("capture count = %d, want 1 (no re-archive)", captures)
	}
}

// TestApiFindOrCreateOrdinaryURLCompatibility is the compatibility guarantee:
// ordinary URLs are never rewritten, so near-miss spellings stay distinct and
// each starts its own capture exactly as before.
func TestApiFindOrCreateOrdinaryURLCompatibility(t *testing.T) {
	r, db, key := newFindOrCreateHandlerTest(t)
	seedCompletedCapture(t, db, "https://example.com/article?id=7", "ordone", time.Hour, "mhtml")

	code, body := postFindOrCreate(t, r, key, `{"url":"https://example.com/article?id=7","types":["mhtml"]}`)
	if code != http.StatusOK || body["action"] != "found" || body["short_id"] != "ordone" {
		t.Fatalf("exact ordinary URL: status %d, body %#v", code, body)
	}

	code, body = postFindOrCreate(t, r, key, `{"url":"https://example.com/article?id=7&ref=x","types":["mhtml"]}`)
	if code != http.StatusAccepted || body["action"] != "created" {
		t.Fatalf("different ordinary URL: status %d, body %#v, want a new capture", code, body)
	}

	var row models.ArchivedURL
	if err := db.Where("original = ?", "https://example.com/article?id=7&ref=x").First(&row).Error; err != nil {
		t.Fatalf("the submitted ordinary URL must be stored verbatim: %v", err)
	}
	if row.CanonicalURL != row.Original {
		t.Fatalf("ordinary URL was rewritten: original %q, canonical %q", row.Original, row.CanonicalURL)
	}
}

// TestRedirectIfAliasResolvesAcrossIdentityRows: keying capture reuse on the
// canonical identity newly allows an alias whose ArchivedURL row differs from
// the canonical capture's — someone submits the watch URL and gets aliased to
// an archive stored under the youtu.be spelling. Alias resolution keys purely
// on capture ID, so this works; the test exists to keep it that way.
func TestRedirectIfAliasResolvesAcrossIdentityRows(t *testing.T) {
	db := newAliasTestDB(t)

	canonicalRow := models.ArchivedURL{
		Original:     "https://youtu.be/dQw4w9WgXcQ?si=abc",
		CanonicalURL: utils.CanonicalizeArchiveURL("https://youtu.be/dQw4w9WgXcQ?si=abc"),
	}
	if err := db.Create(&canonicalRow).Error; err != nil {
		t.Fatal(err)
	}
	canonicalCapture := models.Capture{ArchivedURLID: canonicalRow.ID, Timestamp: time.Now(), ShortID: "ytreal"}
	if err := db.Create(&canonicalCapture).Error; err != nil {
		t.Fatal(err)
	}

	// The alias hangs off a *different* row: the spelling the caller submitted.
	aliasRow := models.ArchivedURL{
		Original:     "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		CanonicalURL: canonicalRow.CanonicalURL,
	}
	if err := db.Create(&aliasRow).Error; err != nil {
		t.Fatal(err)
	}
	alias := models.Capture{ArchivedURLID: aliasRow.ID, Timestamp: time.Now(), ShortID: "ytalia", AliasOfID: &canonicalCapture.ID}
	if err := db.Create(&alias).Error; err != nil {
		t.Fatal(err)
	}

	w := performAliasRequest(db, "/ytalia/screenshot")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want a redirect", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/ytreal/screenshot" {
		t.Fatalf("Location = %q, want /ytreal/screenshot", got)
	}
}
