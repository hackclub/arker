package models

import (
	"time"

	"gorm.io/gorm"
)

// User represents an authenticated user
type User struct {
	gorm.Model
	Username     string `gorm:"unique"`
	PasswordHash string
}

// Config represents persistent application configuration
type Config struct {
	gorm.Model
	Key   string `gorm:"unique;not null"`
	Value string `gorm:"not null"`
}

// APIKey represents an API key for authentication
type APIKey struct {
	gorm.Model
	Username    string `gorm:"not null"`
	AppName     string `gorm:"not null"`
	Environment string `gorm:"not null"`
	KeyHash     string `gorm:"not null"`
	KeyPrefix   string `gorm:"unique;not null"`
	IsActive    bool   `gorm:"default:true"`
	LastUsedAt  *time.Time
}

// ArchivedURL represents a URL that has been archived
type ArchivedURL struct {
	gorm.Model
	Original string `gorm:"unique"`
	// CanonicalURL is the platform-canonical identity of Original
	// (utils.CanonicalizeArchiveURL): one value shared by every spelling of the
	// same social post, and equal to Original for ordinary URLs. Find-or-create
	// and capture aliasing look up and lock on this; Original is what gets
	// stored, displayed, and handed to the archivers, and is never rewritten.
	//
	// Deliberately indexed but NOT unique. Several rows share one identity by
	// design — each spelling a caller submits keeps its own row so the URL they
	// sent is the URL they see — and a unique constraint would also have to be
	// added to a production table whose existing rows already collide.
	CanonicalURL string `gorm:"index"`
	Captures     []Capture
}

// Capture represents a snapshot of an archived URL at a specific time
type Capture struct {
	gorm.Model
	ArchivedURLID uint        `gorm:"index"`
	ArchivedURL   ArchivedURL `gorm:"foreignKey:ArchivedURLID"`
	Timestamp     time.Time
	ShortID       string        `gorm:"unique"`
	APIKeyID      *uint         `gorm:"nullable"`
	APIKey        *APIKey       `gorm:"foreignKey:APIKeyID"`
	ArchiveItems  []ArchiveItem `gorm:"foreignKey:CaptureID"`
	// AliasOfID points at the canonical capture this capture is an alias of.
	// An alias capture has its own short ID, timestamp, and API key
	// (provenance), but owns no archive items and enqueued no jobs; serving
	// resolves it to the canonical capture with a visible redirect. Aliases
	// always point directly at a canonical capture, never at another alias.
	AliasOfID *uint    `gorm:"index"`
	AliasOf   *Capture `gorm:"foreignKey:AliasOfID"`
}

// ArchiveItem represents a specific type of archive (screenshot, mhtml, etc.)
type ArchiveItem struct {
	gorm.Model
	CaptureID  uint   `gorm:"index:idx_archive_items_capture_type,priority:1"`
	Type       string `gorm:"index:idx_archive_items_capture_type,priority:2"` // mhtml, screenshot, git, youtube
	Status     string // pending, processing, completed, failed
	StorageKey string
	Extension  string // .webp, .mhtml, .tar.zst, .mp4, etc.
	FileSize   int64  // file size in bytes
	// MetadataKey points at a stable normalized JSON sidecar. RawMetadataKey
	// points at the sanitized extractor/provider record used to build it. Both
	// are empty for archive types without sidecars and for older video captures;
	// an empty key must never be treated as inferred metadata.
	MetadataKey    string
	RawMetadataKey string
	Logs           string `gorm:"type:text"`
	RetryCount     int
	// Completeness records whether this item stored every obtainable source
	// asset for the post: "complete", "partial", or "unknown". It is only
	// written by archivers that can speak to it (the social extractors); every
	// other type leaves it empty.
	//
	// Empty means the row predates completeness tracking, so it must be read as
	// unknown and never as complete. Deliberately un-indexed: the column is read
	// through an already-loaded capture, and AutoMigrate adding a bare nullable
	// column to archive_items is a metadata-only change on existing prod rows.
	Completeness string
	// Source records which flow produced the stored artifact: "" or "native"
	// for the regular archivers, "apify" (historically "brightdata") when the
	// paid fallback rescued a failed native run. Provenance matters here: fallback artifacts
	// can differ in fidelity (e.g. YouTube capped at the progressive stream),
	// so reviews and audits need to find them without parsing logs.
	Source string `gorm:"index"`

	// Thumbnail is a derived preview image for this item, stored as its own
	// object. It is deliberately not an ArchiveItem of its own: archive types
	// are permanent, user-facing identifiers that render as viewer tabs, and a
	// thumbnail is neither.
	//
	// ThumbnailStatus distinguishes "not attempted yet" from "generated" and
	// from "this source can never produce one". Without that last state the
	// lazy generator would re-enqueue an impossible job on every page view.
	ThumbnailKey    string // storage key; empty until generated
	ThumbnailWidth  int
	ThumbnailHeight int
	ThumbnailStatus string `gorm:"index"` // "" | pending | ready | unavailable
	// ThumbnailKind records what the bytes actually are. Legacy rows are empty;
	// that is intentionally distinct from social_original so the bulk backfill
	// can replace the old 480x270 derivatives once, resume safely after a
	// restart, and leave already-correct posters alone.
	ThumbnailKind string `gorm:"index"` // "" | page_preview | social_original | social_fallback
}

// Archive item source values for ArchiveItem.Source.
const (
	ArchiveSourceNative = "native"
	// ArchiveSourceApify marks artifacts produced by the Apify fallback.
	ArchiveSourceApify = "apify"
	// ArchiveSourceBrightData marks artifacts produced by the retired Bright
	// Data fallback. No new rows carry it; readers must keep honoring it.
	ArchiveSourceBrightData = "brightdata"
)

// IsFallbackSource reports whether an ArchiveItem.Source value names a paid
// fallback provider rather than the native flow. Fallback artifacts can differ
// in fidelity from native ones, so reviews, reuse and backfills treat every
// provider alike here.
func IsFallbackSource(source string) bool {
	return source == ArchiveSourceApify || source == ArchiveSourceBrightData
}

// Thumbnail status values for ArchiveItem.ThumbnailStatus.
const (
	ThumbnailStatusPending     = "pending"
	ThumbnailStatusReady       = "ready"
	ThumbnailStatusUnavailable = "unavailable"
)

// Thumbnail kind values for ArchiveItem.ThumbnailKind.
const (
	ThumbnailKindPagePreview    = "page_preview"
	ThumbnailKindSocialOriginal = "social_original"
	// SocialPreview is a compact, uncropped derivative of the platform's
	// authored poster/still. Its aspect ratio is the source's; its longest side
	// is bounded for row rendering.
	ThumbnailKindSocialPreview = "social_preview"
	// SocialFallback means a backfill made a conclusive attempt but the post no
	// longer exposes a real poster. Any existing legacy thumbnail stays in
	// place; otherwise the capture falls back to its screenshot sibling.
	ThumbnailKindSocialFallback = "social_fallback"
)

// FallbackUsage records one billable operation performed by a paid fallback
// provider, so operators can see exactly what the fallback is costing without
// leaving Arker. One archive item can accumulate several rows: one actor run
// per attempt, or a scrape plus a media download.
//
// Rows are written for failures too, with Success=false, because a failed
// attempt can still be billable and silent spend is the thing this table
// exists to prevent.
//
// Historical Bright Data rows (Provider "brightdata") were copied in from the
// retired bright_data_usages table: Product is the Bright Data product,
// ResourceID the dataset ID and OperationID the snapshot ID. CostUSD for those
// rows is the rate-based estimate the old client computed. Apify rows carry
// the actor ID, the run ID and the platform-reported usageTotalUsd.
type FallbackUsage struct {
	gorm.Model
	ArchiveItemID uint   `gorm:"index"`
	ShortID       string `gorm:"index"`
	URL           string
	// Provider is the paid service: "apify" or (historical) "brightdata".
	Provider string `gorm:"index"`
	// Product is the provider's unit of work: an Apify actor ID such as
	// "clockworks/tiktok-video-scraper", or a Bright Data product name.
	Product string `gorm:"index"`
	// ResourceID identifies the provider-side resource the run drew on (an
	// Apify actor run's default key-value store, a Bright Data dataset).
	ResourceID string
	// OperationID identifies the provider-side operation (an Apify run ID, a
	// Bright Data snapshot ID).
	OperationID string
	// Records is the number of dataset items returned.
	Records int
	// BytesTransferred is the media payload pulled from the provider itself
	// (bytes served out of an Apify key-value store), not from platform CDNs.
	BytesTransferred int64
	CostUSD          float64
	Success          bool
	Detail           string
}

// Fallback provider values for FallbackUsage.Provider.
const (
	FallbackProviderApify      = "apify"
	FallbackProviderBrightData = "brightdata"
)

// ArchiveItemLog stores immutable log chunks for an archive item.
type ArchiveItemLog struct {
	ID            uint        `gorm:"primaryKey"`
	ArchiveItemID uint        `gorm:"not null"`
	ArchiveItem   ArchiveItem `gorm:"constraint:OnDelete:CASCADE;"`
	Attempt       int         `gorm:"not null;default:0"`
	Chunk         string      `gorm:"type:text;not null"`
	CreatedAt     time.Time   `gorm:"not null"`
}
