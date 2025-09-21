package archivers

import (
	"context"
	"gorm.io/gorm"
	"io"
)

// Archiver interface
type Archiver interface {
	Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (data io.Reader, extension string, contentType string, bundle *PWBundle, err error)
}
