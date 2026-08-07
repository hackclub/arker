package archivers

import (
	"context"
	"io"

	"gorm.io/gorm"
)

// Thumbnail is an encoded preview image produced alongside the main artifact.
//
// It carries bytes rather than an io.Reader on purpose. Thumbnails are small
// (tens of KB), and the main artifact's reader is already a live process or a
// goroutine writing into an io.Pipe whose close semantics are delicate. A
// second reader with the same lifetime rules would be a second way to leak a
// blocked goroutine for no benefit.
type Thumbnail struct {
	Data   []byte
	Width  int
	Height int
}

// Result is what an archiver produces for one item.
//
// This is a struct rather than a list of return values so that adding a derived
// artifact -- a thumbnail today, extracted metadata or a transcript later -- does
// not churn the signature of every archiver again.
type Result struct {
	// Data is the archived content. The worker owns closing it if it is also an
	// io.Closer; see saveArchiveData.
	Data      io.Reader
	Extension string
	// ContentType is advisory. Serving derives its own content type from the
	// archive type and extension, since that is what survives a restart.
	ContentType string
	// Bundle is the Playwright bundle for browser-based archivers. It must be
	// returned even on the error paths so the worker can always clean it up.
	Bundle *PWBundle
	// Thumbnail is optional. A nil thumbnail is not an error: most archive
	// types cannot produce one cheaply, and no caller may treat its absence as
	// a failure.
	Thumbnail *Thumbnail
}

// Archiver captures a URL into a single stored artifact.
type Archiver interface {
	Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (Result, error)
}
