package workers

import (
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/utils"
)

func newQueueTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.APIKey{}, &models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.Config{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedCapture creates an ArchivedURL (if needed) and a canonical capture with
// the given item type/status pairs.
func seedCapture(t *testing.T, db *gorm.DB, url, shortID string, age time.Duration, items map[string]string) models.Capture {
	t.Helper()
	var u models.ArchivedURL
	if err := db.Where("original = ?", url).First(&u).Error; err != nil {
		u = models.ArchivedURL{Original: url}
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("create url: %v", err)
		}
	}
	capture := models.Capture{
		ArchivedURLID: u.ID,
		Timestamp:     time.Now().Add(-age),
		ShortID:       shortID,
	}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatalf("create capture: %v", err)
	}
	for typ, status := range items {
		item := models.ArchiveItem{CaptureID: capture.ID, Type: typ, Status: status}
		if err := db.Create(&item).Error; err != nil {
			t.Fatalf("create item: %v", err)
		}
	}
	return capture
}

func TestCreateCaptureAliasesFreshCapture(t *testing.T) {
	db := newQueueTestDB(t)
	url := "https://example.com/page"
	canonical := seedCapture(t, db, url, "canon", time.Hour, map[string]string{"mhtml": "completed", "screenshot": "completed"})

	shortID, aliasOf, createdItems, err := createCapture(db, url, []string{"mhtml", "screenshot"}, nil, false)
	if err != nil {
		t.Fatalf("createCapture: %v", err)
	}
	if aliasOf == nil {
		t.Fatal("expected an alias, got a full capture")
	}
	if aliasOf.ID != canonical.ID {
		t.Fatalf("aliased to capture %d, want %d", aliasOf.ID, canonical.ID)
	}
	if createdItems != 0 {
		t.Fatalf("alias created %d items, want 0", createdItems)
	}

	var created models.Capture
	if err := db.Where("short_id = ?", shortID).First(&created).Error; err != nil {
		t.Fatalf("load created capture: %v", err)
	}
	if created.AliasOfID == nil || *created.AliasOfID != canonical.ID {
		t.Fatalf("created capture alias_of_id = %v, want %d", created.AliasOfID, canonical.ID)
	}
	if created.ShortID == canonical.ShortID {
		t.Fatal("alias must get its own distinct short ID")
	}

	var itemCount int64
	db.Model(&models.ArchiveItem{}).Where("capture_id = ?", created.ID).Count(&itemCount)
	if itemCount != 0 {
		t.Fatalf("alias owns %d items, want 0", itemCount)
	}
}

func TestCreateCaptureForceBypassesAlias(t *testing.T) {
	db := newQueueTestDB(t)
	url := "https://example.com/page"
	seedCapture(t, db, url, "canon", time.Hour, map[string]string{"mhtml": "completed", "screenshot": "completed"})

	_, aliasOf, createdItems, err := createCapture(db, url, []string{"mhtml", "screenshot"}, nil, true)
	if err != nil {
		t.Fatalf("createCapture: %v", err)
	}
	if aliasOf != nil {
		t.Fatal("force=true must never alias")
	}
	if createdItems != 2 {
		t.Fatalf("created %d items, want 2", createdItems)
	}
}

func TestCreateCaptureNoAliasWhenTypeFailed(t *testing.T) {
	db := newQueueTestDB(t)
	url := "https://example.com/page"
	seedCapture(t, db, url, "canon", time.Hour, map[string]string{"mhtml": "failed", "screenshot": "completed"})

	_, aliasOf, _, err := createCapture(db, url, []string{"mhtml", "screenshot"}, nil, false)
	if err != nil {
		t.Fatalf("createCapture: %v", err)
	}
	if aliasOf != nil {
		t.Fatal("must not alias to a capture with a failed requested type")
	}
}

func TestCreateCaptureNoAliasWhenTypeMissing(t *testing.T) {
	db := newQueueTestDB(t)
	url := "https://example.com/page"
	seedCapture(t, db, url, "canon", time.Hour, map[string]string{"mhtml": "completed"})

	_, aliasOf, _, err := createCapture(db, url, []string{"mhtml", "screenshot"}, nil, false)
	if err != nil {
		t.Fatalf("createCapture: %v", err)
	}
	if aliasOf != nil {
		t.Fatal("must not alias to a capture missing a requested type")
	}
}

func TestCreateCaptureNoAliasWhenStale(t *testing.T) {
	db := newQueueTestDB(t)
	url := "https://example.com/page"
	seedCapture(t, db, url, "canon", 25*time.Hour, map[string]string{"mhtml": "completed", "screenshot": "completed"})

	_, aliasOf, _, err := createCapture(db, url, []string{"mhtml", "screenshot"}, nil, false)
	if err != nil {
		t.Fatalf("createCapture: %v", err)
	}
	if aliasOf != nil {
		t.Fatal("must not alias outside the freshness window")
	}
}

func TestCreateCaptureRespectsConfiguredWindow(t *testing.T) {
	db := newQueueTestDB(t)
	url := "https://example.com/page"
	seedCapture(t, db, url, "canon", 2*time.Hour, map[string]string{"mhtml": "completed", "screenshot": "completed"})

	if err := db.Create(&models.Config{Key: utils.CaptureFreshnessWindowKey, Value: "1h"}).Error; err != nil {
		t.Fatalf("set config: %v", err)
	}

	_, aliasOf, _, err := createCapture(db, url, []string{"mhtml", "screenshot"}, nil, false)
	if err != nil {
		t.Fatalf("createCapture: %v", err)
	}
	if aliasOf != nil {
		t.Fatal("2h-old capture must not alias under a 1h window")
	}
}

func TestCreateCaptureAliasingDisabledByZeroWindow(t *testing.T) {
	db := newQueueTestDB(t)
	url := "https://example.com/page"
	seedCapture(t, db, url, "canon", time.Minute, map[string]string{"mhtml": "completed", "screenshot": "completed"})

	if err := db.Create(&models.Config{Key: utils.CaptureFreshnessWindowKey, Value: "0s"}).Error; err != nil {
		t.Fatalf("set config: %v", err)
	}

	_, aliasOf, _, err := createCapture(db, url, []string{"mhtml", "screenshot"}, nil, false)
	if err != nil {
		t.Fatalf("createCapture: %v", err)
	}
	if aliasOf != nil {
		t.Fatal("a zero window must disable aliasing")
	}
}

func TestCreateCaptureNeverAliasesToAnAlias(t *testing.T) {
	db := newQueueTestDB(t)
	url := "https://example.com/page"
	canonical := seedCapture(t, db, url, "canon", 2*time.Hour, map[string]string{"mhtml": "completed", "screenshot": "completed"})

	// First submission becomes an alias of canonical.
	_, aliasOf, _, err := createCapture(db, url, []string{"mhtml", "screenshot"}, nil, false)
	if err != nil {
		t.Fatalf("first createCapture: %v", err)
	}
	if aliasOf == nil || aliasOf.ID != canonical.ID {
		t.Fatal("first submission should alias to canonical")
	}

	// Second submission must also point at canonical, not at the newer alias.
	_, aliasOf2, _, err := createCapture(db, url, []string{"mhtml", "screenshot"}, nil, false)
	if err != nil {
		t.Fatalf("second createCapture: %v", err)
	}
	if aliasOf2 == nil || aliasOf2.ID != canonical.ID {
		t.Fatalf("second submission aliased to %v, want canonical %d", aliasOf2, canonical.ID)
	}
}

func TestCreateCaptureAliasAllowsPendingItems(t *testing.T) {
	db := newQueueTestDB(t)
	url := "https://example.com/page"
	canonical := seedCapture(t, db, url, "canon", time.Minute, map[string]string{"mhtml": "pending", "screenshot": "processing"})

	_, aliasOf, _, err := createCapture(db, url, []string{"mhtml", "screenshot"}, nil, false)
	if err != nil {
		t.Fatalf("createCapture: %v", err)
	}
	if aliasOf == nil || aliasOf.ID != canonical.ID {
		t.Fatal("in-flight (pending/processing) items should still allow aliasing")
	}
}
