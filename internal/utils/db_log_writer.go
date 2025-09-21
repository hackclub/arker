package utils

import (
	"arker/internal/models"
	"gorm.io/gorm"
	"strings"
	"sync"
)

// DBLogWriter writes logs to database in real-time
type DBLogWriter struct {
	db     *gorm.DB
	itemID uint
	buffer strings.Builder
	mutex  sync.Mutex
}

func NewDBLogWriter(db *gorm.DB, itemID uint) *DBLogWriter {
	return &DBLogWriter{
		db:     db,
		itemID: itemID,
	}
}

func (w *DBLogWriter) Write(p []byte) (n int, err error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	// Write to buffer
	n, err = w.buffer.Write(p)
	if err != nil {
		return n, err
	}

	// Update database with current log content
	w.db.Model(&models.ArchiveItem{}).Where("id = ?", w.itemID).Update("logs", w.buffer.String())

	return n, nil
}

func (w *DBLogWriter) String() string {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.buffer.String()
}
