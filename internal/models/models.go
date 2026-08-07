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
	Logs       string `gorm:"type:text"`
	RetryCount int

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

// Thumbnail status values for ArchiveItem.ThumbnailStatus.
const (
	ThumbnailStatusPending     = "pending"
	ThumbnailStatusReady       = "ready"
	ThumbnailStatusUnavailable = "unavailable"
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
