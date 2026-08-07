package utils

import (
	"log/slog"
	"time"

	"gorm.io/gorm"

	"arker/internal/models"
)

// CaptureFreshnessWindowKey is the configs-table key holding the alias
// freshness window as a Go duration string (e.g. "24h"). A zero or negative
// duration disables capture aliasing entirely.
const CaptureFreshnessWindowKey = "capture_freshness_window"

// DefaultCaptureFreshnessWindow is used when the config row is missing or
// unparsable.
const DefaultCaptureFreshnessWindow = 24 * time.Hour

// GetOrCreateConfigValue retrieves a config value from the database, creating
// the row with defaultValue when it does not exist yet. Two callers racing to
// create the same key are both fine: the loser of the unique-constraint race
// re-reads the winner's row.
func GetOrCreateConfigValue(db *gorm.DB, key string, defaultValue string) (string, error) {
	var config models.Config
	if err := db.Where("key = ?", key).First(&config).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			config = models.Config{Key: key, Value: defaultValue}
			if createErr := db.Create(&config).Error; createErr != nil {
				// Lost a concurrent-create race (or the insert failed for
				// another reason): the authoritative row may exist now.
				if err := db.Where("key = ?", key).First(&config).Error; err != nil {
					return "", createErr
				}
				return config.Value, nil
			}
			return defaultValue, nil
		}
		return "", err
	}
	return config.Value, nil
}

// CaptureFreshnessWindow returns how recent a prior capture of the same URL
// must be for a new submission to become an alias of it instead of triggering
// a full re-archive. Tunable at runtime via the configs table without a
// deploy.
func CaptureFreshnessWindow(db *gorm.DB) time.Duration {
	value, err := GetOrCreateConfigValue(db, CaptureFreshnessWindowKey, DefaultCaptureFreshnessWindow.String())
	if err != nil {
		slog.Error("Failed to read capture freshness window config; using default",
			"key", CaptureFreshnessWindowKey, "default", DefaultCaptureFreshnessWindow, "error", err)
		return DefaultCaptureFreshnessWindow
	}
	window, err := time.ParseDuration(value)
	if err != nil {
		slog.Error("Invalid capture freshness window config; using default",
			"key", CaptureFreshnessWindowKey, "value", value, "default", DefaultCaptureFreshnessWindow, "error", err)
		return DefaultCaptureFreshnessWindow
	}
	return window
}
