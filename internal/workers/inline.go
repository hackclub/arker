package workers

import (
	"context"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
)

// CreateCaptureRows creates the ArchivedURL (if needed), the capture, and one
// pending archive item per type, and returns the new short ID. It is
// QueueCapture's transaction without the queueing: no River jobs are enqueued
// and no alias is ever created (force semantics).
//
// It exists for callers that must run the archive themselves rather than hand
// it to the shared worker pool — today that is the production canary runner,
// which has to supply its own archiver map so a probe can never reach a paid
// fallback. Sharing this function keeps canary captures byte-identical in shape
// to real ones: same identity rows, same short ID generator, same item states.
func CreateCaptureRows(db *gorm.DB, url string, types []string, apiKeyID *uint) (string, error) {
	shortID, _, _, err := createCapture(db, url, types, apiKeyID, true)
	return shortID, err
}

// RunArchiveItemInline runs one archive item synchronously through the exact
// code path the River worker uses (processArchiveJob): same timeout selection,
// same DB log writer, same storage keys and nonce, same sidecar-before-completed
// ordering in saveArchiveResult.
//
// The archiver map is a parameter rather than the worker's own so a caller can
// deliberately restrict which archivers are reachable. The canary runner passes
// a native-only map; there is no way for a run started here to spend money that
// the caller did not hand it the means to spend.
func RunArchiveItemInline(ctx context.Context, args ArchiveJobArgs, item *models.ArchiveItem, store storage.Storage, db *gorm.DB, archiversMap map[string]archivers.Archiver) error {
	return processArchiveJob(ctx, args, item, store, db, archiversMap)
}
