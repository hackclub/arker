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
	Captures []Capture
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
	// for the regular archivers, "brightdata" when the Bright Data fallback
	// rescued a failed native run. Provenance matters here: fallback artifacts
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
}

// Archive item source values for ArchiveItem.Source.
const (
	ArchiveSourceNative     = "native"
	ArchiveSourceBrightData = "brightdata"
)

// Thumbnail status values for ArchiveItem.ThumbnailStatus.
const (
	ThumbnailStatusPending     = "pending"
	ThumbnailStatusReady       = "ready"
	ThumbnailStatusUnavailable = "unavailable"
)

// BrightDataUsage records one billable Bright Data operation performed by the
// fallback archiver, so operators can see exactly what the fallback is costing
// without leaving Arker. One archive item can accumulate several rows: a
// dataset trigger per attempt, or a scrape plus a browser session.
//
// CostUSD is an estimate computed from configured rates (the API key in use
// cannot read Bright Data's billing endpoints); the true invoice lives in the
// Bright Data dashboard. Rows are written for failures too, with Success=false,
// because a failed attempt can still be billable and silent spend is the thing
// this table exists to prevent.
type BrightDataUsage struct {
	gorm.Model
	ArchiveItemID uint   `gorm:"index"`
	ShortID       string `gorm:"index"`
	URL           string
	// Product is the Bright Data product used: "web_scraper" (dataset trigger)
	// or "browser_api" (remote browser session).
	Product    string `gorm:"index"`
	DatasetID  string
	SnapshotID string
	// Records is the number of dataset records returned (web_scraper only).
	Records int
	// BytesTransferred is the measured media payload plus a fixed page-load
	// overhead estimate (browser_api only).
	BytesTransferred int64
	CostUSD          float64
	Success          bool
	Detail           string
}

// ArchiveItemLog stores immutable log chunks for an archive item.
type ArchiveItemLog struct {
	ID            uint        `gorm:"primaryKey"`
	ArchiveItemID uint        `gorm:"not null"`
	ArchiveItem   ArchiveItem `gorm:"constraint:OnDelete:CASCADE;"`
	Attempt       int         `gorm:"not null;default:0"`
	Chunk         string      `gorm:"type:text;not null"`
	CreatedAt     time.Time   `gorm:"not null"`
}
