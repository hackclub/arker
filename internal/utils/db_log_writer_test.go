package utils

import (
	"fmt"
	"strings"
	"testing"

	"arker/internal/models"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newLogTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchiveItem{}, &models.ArchiveItemLog{}); err != nil {
		t.Fatalf("migrate sqlite db: %v", err)
	}
	return db
}

func createLogTestItem(t *testing.T, db *gorm.DB, logs string) models.ArchiveItem {
	t.Helper()
	item := models.ArchiveItem{Type: "mhtml", Status: "processing", Logs: logs}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("create archive item: %v", err)
	}
	return item
}

func TestDBLogWriterReconstructsExactLogs(t *testing.T) {
	db := newLogTestDB(t)
	item := createLogTestItem(t, db, "")

	writer := NewDBLogWriter(db, item.ID)
	parts := []string{
		"Starting archive\n",
		strings.Repeat("x", MaxArchiveLogChunkBytes+200),
		"\nDone ✓\n",
	}
	for _, part := range parts {
		if _, err := writer.Write([]byte(part)); err != nil {
			t.Fatalf("write log part: %v", err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("flush logs: %v", err)
	}

	got, err := ArchiveItemLogString(db, item.ID, "")
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	want := strings.Join(parts, "")
	if got != want {
		t.Fatalf("logs mismatch: got %q want %q", got, want)
	}

	var chunks []models.ArchiveItemLog
	if err := db.Where("archive_item_id = ?", item.ID).Order("id ASC").Find(&chunks).Error; err != nil {
		t.Fatalf("load chunks: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for _, chunk := range chunks {
		if len(chunk.Chunk) > MaxArchiveLogChunkBytes {
			t.Fatalf("chunk %d has %d bytes, max %d", chunk.ID, len(chunk.Chunk), MaxArchiveLogChunkBytes)
		}
	}
}

func TestDBLogWriterPreservesSplitUTF8AcrossWritesAndChunkBoundaries(t *testing.T) {
	db := newLogTestDB(t)
	item := createLogTestItem(t, db, "")

	writer := NewDBLogWriter(db, item.ID)
	prefix := strings.Repeat("a", MaxArchiveLogChunkBytes-1)
	checkmark := []byte("✓")
	writes := [][]byte{
		[]byte(prefix),
		checkmark[:1],
		checkmark[1:],
		[]byte("\n"),
	}
	for _, write := range writes {
		if _, err := writer.Write(write); err != nil {
			t.Fatalf("write log bytes: %v", err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("flush logs: %v", err)
	}

	got, err := ArchiveItemLogString(db, item.ID, "")
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	want := prefix + "✓\n"
	if got != want {
		t.Fatalf("logs mismatch: got %q want %q", got, want)
	}

	var chunks []models.ArchiveItemLog
	if err := db.Where("archive_item_id = ?", item.ID).Order("id ASC").Find(&chunks).Error; err != nil {
		t.Fatalf("load chunks: %v", err)
	}
	for _, chunk := range chunks {
		if len(chunk.Chunk) > MaxArchiveLogChunkBytes {
			t.Fatalf("chunk %d has %d bytes, max %d", chunk.ID, len(chunk.Chunk), MaxArchiveLogChunkBytes)
		}
	}
}

func TestDBLogWriterFlushPersistsTrailingPartialChunk(t *testing.T) {
	db := newLogTestDB(t)
	item := createLogTestItem(t, db, "")

	writer := NewDBLogWriter(db, item.ID)
	if _, err := writer.Write([]byte("partial without newline")); err != nil {
		t.Fatalf("write partial log: %v", err)
	}

	got, err := ArchiveItemLogString(db, item.ID, "")
	if err != nil {
		t.Fatalf("read logs before flush: %v", err)
	}
	if got != "" {
		t.Fatalf("expected no persisted logs before flush, got %q", got)
	}

	if err := writer.Flush(); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	got, err = ArchiveItemLogString(db, item.ID, "")
	if err != nil {
		t.Fatalf("read logs after flush: %v", err)
	}
	if got != "partial without newline" {
		t.Fatalf("logs mismatch after flush: got %q", got)
	}
}

func TestDBLogWriterAttemptsAppendWithoutOverwriting(t *testing.T) {
	db := newLogTestDB(t)
	item := createLogTestItem(t, db, "")

	first := NewDBLogWriterForAttempt(db, item.ID, 1)
	if _, err := first.Write([]byte("attempt one\n")); err != nil {
		t.Fatalf("write first attempt: %v", err)
	}
	if err := first.Flush(); err != nil {
		t.Fatalf("flush first attempt: %v", err)
	}

	second := NewDBLogWriterForAttempt(db, item.ID, 2)
	if _, err := second.Write([]byte("attempt two\n")); err != nil {
		t.Fatalf("write second attempt: %v", err)
	}
	if err := second.Flush(); err != nil {
		t.Fatalf("flush second attempt: %v", err)
	}

	got, err := ArchiveItemLogString(db, item.ID, "")
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if got != "attempt one\nattempt two\n" {
		t.Fatalf("attempt logs mismatch: got %q", got)
	}

	var attempts []int
	if err := db.Model(&models.ArchiveItemLog{}).Where("archive_item_id = ?", item.ID).Order("id ASC").Pluck("attempt", &attempts).Error; err != nil {
		t.Fatalf("load attempts: %v", err)
	}
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("unexpected attempts: %#v", attempts)
	}
}

func TestArchiveItemLogStringFallsBackToLegacyLogs(t *testing.T) {
	db := newLogTestDB(t)
	item := createLogTestItem(t, db, "legacy-only logs")

	got, err := ArchiveItemLogString(db, item.ID, item.Logs)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if got != item.Logs {
		t.Fatalf("legacy fallback mismatch: got %q want %q", got, item.Logs)
	}
}

func TestArchiveItemLogStringPreservesLegacyLogsWhenChunksAlsoExist(t *testing.T) {
	db := newLogTestDB(t)
	item := createLogTestItem(t, db, "legacy\n")

	if err := AppendArchiveItemLog(db, item.ID, 1, "new chunk\n"); err != nil {
		t.Fatalf("append chunk log: %v", err)
	}

	got, err := ArchiveItemLogString(db, item.ID, item.Logs)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if got != "legacy\nnew chunk\n" {
		t.Fatalf("merged logs mismatch: got %q", got)
	}
}

func TestArchiveItemLogStringAvoidsDuplicatingBackfilledLegacyLogs(t *testing.T) {
	db := newLogTestDB(t)
	item := createLogTestItem(t, db, "legacy\n")

	if err := AppendArchiveItemLog(db, item.ID, 0, "legacy\n"); err != nil {
		t.Fatalf("append backfilled chunk log: %v", err)
	}

	got, err := ArchiveItemLogString(db, item.ID, item.Logs)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if got != "legacy\n" {
		t.Fatalf("expected no duplicate legacy logs, got %q", got)
	}
}

func TestBackfillLegacyArchiveItemLogs(t *testing.T) {
	db := newLogTestDB(t)
	legacy := "legacy\n" + strings.Repeat("z", MaxArchiveLogChunkBytes+10)
	item := createLogTestItem(t, db, legacy)

	if err := BackfillLegacyArchiveItemLogs(db); err != nil {
		t.Fatalf("backfill legacy logs: %v", err)
	}

	var reloaded models.ArchiveItem
	if err := db.First(&reloaded, item.ID).Error; err != nil {
		t.Fatalf("reload item: %v", err)
	}
	if reloaded.Logs != "" {
		t.Fatalf("legacy logs were not cleared: %q", reloaded.Logs)
	}

	got, err := ArchiveItemLogString(db, item.ID, reloaded.Logs)
	if err != nil {
		t.Fatalf("read backfilled logs: %v", err)
	}
	if got != legacy {
		t.Fatalf("backfilled logs mismatch: got %q want %q", got, legacy)
	}
}
