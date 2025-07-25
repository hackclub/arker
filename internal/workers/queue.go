package workers

import (
	"time"
	"gorm.io/gorm"
	"arker/internal/models"
	"arker/internal/utils"
)

// QueueCapture centralizes the logic for creating captures and queueing jobs
func QueueCapture(db *gorm.DB, urlID uint, originalURL string, types []string) (string, error) {
	shortID := utils.GenerateShortID(db)
	capture := models.Capture{
		ArchivedURLID: urlID, 
		Timestamp:     time.Now(), 
		ShortID:       shortID,
	}
	if err := db.Create(&capture).Error; err != nil {
		return "", err
	}
	
	for _, t := range types {
		item := models.ArchiveItem{
			CaptureID: capture.ID, 
			Type:      t, 
			Status:    "pending",
		}
		if err := db.Create(&item).Error; err != nil {
			return "", err
		}
		
		JobChan <- models.Job{
			CaptureID: capture.ID, 
			ShortID:   shortID, 
			Type:      t, 
			URL:       originalURL,
		}
	}
	
	return shortID, nil
}

// QueueCaptureForURL creates or finds an ArchivedURL and queues a capture
func QueueCaptureForURL(db *gorm.DB, url string, types []string) (string, error) {
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
	
	return QueueCapture(db, u.ID, url, types)
}
