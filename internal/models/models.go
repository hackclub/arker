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

// CanaryRun is the durable history of one production canary probe: a real
// archive of a known-good public URL, validated against the social archive
// contract end to end.
//
// One row is written per probe attempt, including the failed and aborted ones.
// The row is created before the archive starts (so a crash mid-probe leaves
// evidence rather than silence) and updated when the probe finishes; a row with
// no FinishedAt is a probe that never reported back.
//
// StageReached names how far the probe got, and FailureStage/FailureReason name
// exactly where and why it stopped. That pairing is the whole point of the
// table: "youtube/video failed" is not actionable, "youtube/video reached
// item_completed then failed raw_metadata: raw metadata key is empty" is.
type CanaryRun struct {
	gorm.Model
	// Platform and PostType together identify the probe slot (the health view
	// is the newest row per pair); ProbeKey is their stable "platform/post_type"
	// spelling as configured.
	ProbeKey string `gorm:"index"`
	Platform string `gorm:"index"`
	PostType string `gorm:"index"`
	URL      string
	// Trigger records who asked: "schedule" or "manual".
	Trigger     string `gorm:"index"`
	ArchiveType string
	StartedAt   time.Time `gorm:"index"`
	FinishedAt  *time.Time
	DurationMS  int64
	// StageReached is the last stage that succeeded (or "passed").
	StageReached  string
	Passed        bool `gorm:"index"`
	FailureStage  string
	FailureReason string
	// ShortID is the capture this probe created, empty if it never got that far.
	// It is the join key to captures/archive_items and to bright_data_usages.
	ShortID     string `gorm:"index"`
	MediaBytes  int64
	MediaCount  int
	ContentType string
	// Provenance mirrors ArchiveItem.Source ("native"/"brightdata"). A canary
	// that reports anything but native means the paid guard was bypassed.
	Provenance string
	// CostUSD is the Bright Data spend attributed to this probe's capture. It is
	// zero for every run of a native-only canary, which is the default and the
	// only configuration that can be scheduled without opting in to paid probes.
	CostUSD     float64
	PaidAllowed bool
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
