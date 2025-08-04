package workers

import (
	"log/slog"
	"time"
	"gorm.io/gorm"
	"arker/internal/models"
	"arker/internal/utils"
)

// QueueCapture centralizes the logic for creating captures and queueing jobs
func QueueCapture(db *gorm.DB, urlID uint, originalURL string, types []string) (string, error) {
	return QueueCaptureWithAPIKey(db, urlID, originalURL, types, nil)
}

// QueueCaptureWithAPIKey creates a capture with optional API key tracking
func QueueCaptureWithAPIKey(db *gorm.DB, urlID uint, originalURL string, types []string, apiKeyID *uint) (string, error) {
	shortID := utils.GenerateShortID(db)
	capture := models.Capture{
		ArchivedURLID: urlID, 
		Timestamp:     time.Now(), 
		ShortID:       shortID,
		APIKeyID:      apiKeyID,
	}
	if err := db.Create(&capture).Error; err != nil {
		slog.Error("Failed to create capture",
			"url", originalURL,
			"types", types,
			"error", err)
		return "", err
	}
	
	createdItems := 0
	for _, t := range types {
		item := models.ArchiveItem{
			CaptureID:    capture.ID, 
			Type:         t, 
			Status:       "pending",
		}
		if err := db.Create(&item).Error; err != nil {
			slog.Error("Failed to create archive item",
				"short_id", shortID,
				"type", t,
				"error", err)
			return "", err
		}
		createdItems++
	}
	
	slog.Info("Queued new capture",
		"short_id", shortID,
		"url", originalURL,
		"types", types,
		"items_created", createdItems)
	
	return shortID, nil
}

// QueueCaptureForURL creates or finds an ArchivedURL and queues a capture
func QueueCaptureForURL(db *gorm.DB, url string, types []string) (string, error) {
	return QueueCaptureForURLWithAPIKey(db, url, types, nil)
}

// QueueCaptureForURLWithAPIKey creates or finds an ArchivedURL and queues a capture with API key tracking
func QueueCaptureForURLWithAPIKey(db *gorm.DB, url string, types []string, apiKeyID *uint) (string, error) {
	if len(types) == 0 {
		types = utils.GetArchiveTypes(url)
	}
	
	var u models.ArchivedURL
	err := db.Where("original = ?", url).First(&u).Error
	if err == gorm.ErrRecordNotFound {
		u = models.ArchivedURL{Original: url}
		if err = db.Create(&u).Error; err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	
	return QueueCaptureWithAPIKey(db, u.ID, url, types, apiKeyID)
}
