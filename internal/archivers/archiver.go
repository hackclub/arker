package archivers

import (
	"io"
	"gorm.io/gorm"
)

// Archiver interface
type Archiver interface {
	Archive(url string, logWriter io.Writer, db *gorm.DB, itemID uint) (data io.Reader, extension string, contentType string, cleanup func(), err error)
}
