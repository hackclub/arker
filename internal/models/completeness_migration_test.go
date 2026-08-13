package models

import (
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// legacyArchiveItem is archive_items as it stood before completeness tracking.
// It exists so the migration can be exercised against a table that genuinely
// lacks the column, rather than one AutoMigrate created with it already there.
type legacyArchiveItem struct {
	gorm.Model
	CaptureID      uint
	Type           string
	Status         string
	StorageKey     string
	Extension      string
	FileSize       int64
	MetadataKey    string
	RawMetadataKey string
	Logs           string `gorm:"type:text"`
	RetryCount     int
	Source         string
}

func (legacyArchiveItem) TableName() string { return "archive_items" }

// The completeness column is additive: AutoMigrate has to add it to a populated
// table without touching the rows already there, and adding it twice has to be
// a no-op. Existing rows must come back with an empty value — which the API
// reads as unknown, never as complete.
func TestCompletenessColumnIsAdditiveOverExistingRows(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:completeness-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	if err := db.AutoMigrate(&legacyArchiveItem{}); err != nil {
		t.Fatalf("migrate the pre-change schema: %v", err)
	}
	before := legacyArchiveItem{
		Model:      gorm.Model{CreatedAt: time.Now(), UpdatedAt: time.Now()},
		CaptureID:  7,
		Type:       "gallery-dl",
		Status:     "completed",
		StorageKey: "abc12/gallery-dl-9f2c.zip",
		Extension:  ".zip",
		FileSize:   4096,
		Logs:       "captured before completeness existed",
		RetryCount: 1,
		Source:     "native",
	}
	if err := db.Create(&before).Error; err != nil {
		t.Fatalf("insert a pre-change row: %v", err)
	}

	// Running it twice proves the migration is idempotent, which is what a
	// restart or a rolling deploy will actually do.
	for attempt := 1; attempt <= 2; attempt++ {
		if err := db.AutoMigrate(&ArchiveItem{}); err != nil {
			t.Fatalf("AutoMigrate attempt %d: %v", attempt, err)
		}
	}

	var after ArchiveItem
	if err := db.First(&after, before.ID).Error; err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if after.Completeness != "" {
		t.Errorf("completeness = %q, want empty so the row reads as legacy-unknown", after.Completeness)
	}
	if after.StorageKey != before.StorageKey || after.FileSize != before.FileSize ||
		after.Type != before.Type || after.Status != before.Status ||
		after.RetryCount != before.RetryCount || after.Source != before.Source ||
		after.Logs != before.Logs {
		t.Errorf("the migration altered an existing row: %+v", after)
	}

	// And the column is writable afterwards, so new captures can record a
	// verdict on the same table.
	if err := db.Model(&ArchiveItem{}).Where("id = ?", before.ID).Update("completeness", "partial").Error; err != nil {
		t.Fatalf("write the new column: %v", err)
	}
	var updated ArchiveItem
	db.First(&updated, before.ID)
	if updated.Completeness != "partial" {
		t.Errorf("completeness = %q after update, want partial", updated.Completeness)
	}
}
