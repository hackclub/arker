package archivers

import (
	"context"
	"errors"
	"io"

	"gorm.io/gorm"
)

// ErrSocialThumbnailUnavailable means a successful, conclusive lookup found no
// provider-authored poster. Backfill workers preserve any legacy preview and
// mark the group attempted instead of retrying this permanent condition.
var ErrSocialThumbnailUnavailable = errors.New("social thumbnail unavailable")

// Thumbnail is an encoded preview image produced alongside the main artifact.
//
// It carries bytes rather than an io.Reader on purpose. Derived screenshot
// previews are tens of KB and original social posters are explicitly bounded
// by the thumbnail package. The main artifact's reader is already a live
// process or a goroutine writing into an io.Pipe whose close semantics are
// delicate; a second reader with the same lifetime rules would be a second way
// to leak a blocked goroutine for no benefit.
type Thumbnail struct {
	Data   []byte
	Width  int
	Height int
	// Kind describes whether Data is a provider-authored social image or an
	// Arker-derived page preview. It is stored with the archive item so legacy
	// social derivatives can be identified and backfilled exactly once.
	Kind string
}

// Sidecar is a small durable artifact stored beside the primary archive.
// Video captures use one for normalized metadata and one for the sanitized raw
// extractor/provider record. Sidecars are bytes because JSON metadata is small
// and must remain available after an archiver's temp directory is removed.
type Sidecar struct {
	Data []byte
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
	// Source identifies which flow produced the artifact when it matters for
	// provenance (see models.ArchiveItem.Source). Empty means the item's
	// regular native archiver.
	Source string
	// Metadata is the stable, normalized machine-readable record for an
	// artifact. RawMetadata preserves the sanitized provider-native record for
	// auditability. They are optional for archive types that do not use separate
	// sidecars and for legacy test archivers.
	Metadata    *Sidecar
	RawMetadata *Sidecar
	// Extras are additional durable artifacts stored beside the main one under
	// the same key base — subtitle tracks today. They are bytes for the same
	// reason sidecars are: they are small, and they must outlive the archiver's
	// temp directory, which is swept the moment Archive returns.
	Extras []ExtraArtifact
	// Completeness is the archiver's claim about whether it stored every
	// obtainable source asset: one of CompletenessComplete, CompletenessPartial
	// or CompletenessUnknown. Empty means the archiver does not speak to
	// completeness (mhtml, screenshot, git, itch), and is stored as-is so those
	// types stay distinguishable from a social capture that answered "unknown".
	Completeness string
}

// ExtraArtifact is one additional stored object belonging to an archive item.
//
// NameSuffix is appended to the item's storage key base, so an artifact's
// location is derivable from the item and recorded in the normalized metadata.
// An extra is never load-bearing: failing to store one must not fail or degrade
// the archive it belongs to.
type ExtraArtifact struct {
	NameSuffix  string
	ContentType string
	Data        []byte
}

// Archiver captures a URL into a single stored artifact.
type Archiver interface {
	Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (Result, error)
}

// VideoMetadataRefresher is implemented by video archivers that can rebuild an
// item's sidecars — normalized metadata, sanitized raw record, caption tracks,
// poster — without downloading the media again. media describes the stored
// artifact the refreshed record must keep describing. The returned Result
// carries no Data: the caller reuses the bytes that are already in storage.
type VideoMetadataRefresher interface {
	RefreshVideoMetadata(ctx context.Context, url string, logWriter io.Writer, media VideoMedia) (Result, error)
}

// SocialThumbnailRefresher fetches only a video's provider-authored poster.
// Unlike VideoMetadataRefresher it does not download media, captions, or
// sidecars, which makes it safe to run gradually across the historical corpus.
type SocialThumbnailRefresher interface {
	RefreshSocialThumbnail(ctx context.Context, url string, logWriter io.Writer) (*Thumbnail, error)
}
