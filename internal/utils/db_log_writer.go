package utils

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"arker/internal/models"

	"gorm.io/gorm"
)

const MaxArchiveLogChunkBytes = 1024

// DBLogWriter writes logs to database in real-time
type DBLogWriter struct {
	db      *gorm.DB
	itemID  uint
	attempt int
	buffer  []byte
	mutex   sync.Mutex
}

func NewDBLogWriter(db *gorm.DB, itemID uint) *DBLogWriter {
	return NewDBLogWriterForAttempt(db, itemID, 0)
}

func NewDBLogWriterForAttempt(db *gorm.DB, itemID uint, attempt int) *DBLogWriter {
	return &DBLogWriter{
		db:      db,
		itemID:  itemID,
		attempt: attempt,
	}
}

func (w *DBLogWriter) Write(p []byte) (n int, err error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	var firstErr error
	w.buffer = append(w.buffer, p...)

	for {
		flushLen := w.nextFlushLenLocked(false)
		if flushLen == 0 {
			break
		}
		if err := w.flushPrefixLocked(flushLen); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
	}

	return len(p), firstErr
}

func (w *DBLogWriter) Flush() error {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	var firstErr error
	for len(w.buffer) > 0 {
		flushLen := w.nextFlushLenLocked(true)
		if flushLen == 0 {
			flushLen = len(w.buffer)
		}
		if err := w.flushPrefixLocked(flushLen); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
	}
	return firstErr
}

func (w *DBLogWriter) nextFlushLenLocked(force bool) int {
	if len(w.buffer) == 0 {
		return 0
	}

	if newline := bytes.IndexByte(w.buffer, '\n'); newline >= 0 && newline+1 <= MaxArchiveLogChunkBytes {
		return newline + 1
	}

	if len(w.buffer) < MaxArchiveLogChunkBytes && !force {
		return 0
	}

	maxLen := len(w.buffer)
	if maxLen > MaxArchiveLogChunkBytes {
		maxLen = MaxArchiveLogChunkBytes
	}
	if force {
		return validUTF8PrefixLen(w.buffer, maxLen, true)
	}
	return validUTF8PrefixLen(w.buffer, maxLen, false)
}

func (w *DBLogWriter) flushPrefixLocked(n int) error {
	chunkBytes := append([]byte(nil), w.buffer[:n]...)
	chunk := strings.ToValidUTF8(string(chunkBytes), "\uFFFD")
	if err := appendArchiveItemLogChunks(w.db, w.itemID, w.attempt, SplitArchiveLogChunks(chunk)); err != nil {
		return err
	}
	w.buffer = w.buffer[n:]
	return nil
}

func validUTF8PrefixLen(b []byte, maxLen int, force bool) int {
	if maxLen > len(b) {
		maxLen = len(b)
	}
	if maxLen <= 0 {
		return 0
	}

	for n := maxLen; n > 0; n-- {
		if utf8.Valid(b[:n]) {
			return n
		}
	}

	if force || len(b) >= MaxArchiveLogChunkBytes {
		return maxLen
	}
	return 0
}

func (w *DBLogWriter) String() string {
	_ = w.Flush()
	logs, err := ArchiveItemLogString(w.db, w.itemID, "")
	if err != nil {
		return ""
	}
	return logs
}

func AppendArchiveItemLog(db *gorm.DB, itemID uint, attempt int, text string) error {
	return appendArchiveItemLogChunks(db, itemID, attempt, SplitArchiveLogChunks(text))
}

func appendArchiveItemLogChunks(db *gorm.DB, itemID uint, attempt int, chunks []string) error {
	if len(chunks) == 0 {
		return nil
	}

	insertChunks := func(tx *gorm.DB) error {
		for _, chunk := range chunks {
			if len(chunk) > MaxArchiveLogChunkBytes {
				return fmt.Errorf("archive log chunk is %d bytes, max is %d", len(chunk), MaxArchiveLogChunkBytes)
			}
			if err := tx.Create(&models.ArchiveItemLog{
				ArchiveItemID: itemID,
				Attempt:       attempt,
				Chunk:         chunk,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	}

	if len(chunks) == 1 {
		return insertChunks(db)
	}
	return db.Transaction(insertChunks)
}

func SplitArchiveLogChunks(text string) []string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	if text == "" {
		return nil
	}

	var chunks []string
	var b strings.Builder
	for _, r := range text {
		part := string(r)
		if b.Len()+len(part) > MaxArchiveLogChunkBytes {
			chunks = append(chunks, b.String())
			b.Reset()
		}
		b.WriteString(part)
	}
	if b.Len() > 0 {
		chunks = append(chunks, b.String())
	}
	return chunks
}

func ArchiveItemLogString(db *gorm.DB, itemID uint, legacyLogs string) (string, error) {
	if db.Dialector.Name() == "postgres" {
		var result struct {
			ChunkCount int64  `gorm:"column:chunk_count"`
			Logs       string `gorm:"column:logs"`
		}
		if err := db.Raw(`
			SELECT count(*) AS chunk_count, COALESCE(string_agg(chunk, '' ORDER BY id), '') AS logs
			FROM archive_item_logs
			WHERE archive_item_id = ?
		`, itemID).Scan(&result).Error; err != nil {
			return "", err
		}
		if result.ChunkCount == 0 {
			return legacyLogs, nil
		}
		return mergeLegacyAndChunkLogs(legacyLogs, result.Logs), nil
	}

	var rows []models.ArchiveItemLog
	if err := db.Select("chunk").Where("archive_item_id = ?", itemID).Order("id ASC").Find(&rows).Error; err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return legacyLogs, nil
	}

	var b strings.Builder
	for _, row := range rows {
		b.WriteString(row.Chunk)
	}
	return mergeLegacyAndChunkLogs(legacyLogs, b.String()), nil
}

func mergeLegacyAndChunkLogs(legacyLogs, chunkLogs string) string {
	if chunkLogs == "" {
		return legacyLogs
	}
	if legacyLogs == "" {
		return chunkLogs
	}
	if chunkLogs == legacyLogs || strings.HasPrefix(chunkLogs, legacyLogs) {
		return chunkLogs
	}
	if strings.HasPrefix(legacyLogs, chunkLogs) {
		return legacyLogs
	}
	return legacyLogs + chunkLogs
}
