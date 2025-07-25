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
	ArchivedURLID uint
	Timestamp     time.Time
	ShortID       string `gorm:"unique"`
	APIKeyID      *uint     `gorm:"nullable"`
	APIKey        *APIKey   `gorm:"foreignKey:APIKeyID"`
	ArchiveItems  []ArchiveItem `gorm:"foreignKey:CaptureID"`
}

// ArchiveItem represents a specific type of archive (screenshot, mhtml, etc.)
type ArchiveItem struct {
	gorm.Model
	CaptureID  uint
	Type       string // mhtml, screenshot, git, youtube
	Status     string // pending, processing, completed, failed
	StorageKey string
	Extension  string // .webp, .mhtml, .tar.zst, .mp4, etc.
	Logs       string `gorm:"type:text"`
	RetryCount int
}

// Job represents a job in the queue
type Job struct {
	CaptureID uint
	ShortID   string
	Type      string
	URL       string
}
