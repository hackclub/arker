package utils

import (
	"fmt"
	"log/slog"

	"arker/internal/models"

	"gorm.io/gorm"
)

func ConfigureArchiveItemLogSchema(db *gorm.DB) error {
	if db.Dialector.Name() != "postgres" {
		return nil
	}

	if err := db.Exec(`
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1
		FROM pg_constraint
		WHERE conname = 'archive_item_logs_chunk_max_bytes'
		  AND conrelid = 'archive_item_logs'::regclass
	) THEN
		ALTER TABLE archive_item_logs
			ADD CONSTRAINT archive_item_logs_chunk_max_bytes
			CHECK (octet_length(chunk) <= 1024);
	END IF;
END $$;
`).Error; err != nil {
		return fmt.Errorf("add archive_item_logs chunk constraint: %w", err)
	}

	if err := db.Exec(`ALTER TABLE archive_item_logs ALTER COLUMN chunk SET STORAGE PLAIN`).Error; err != nil {
		return fmt.Errorf("set archive_item_logs.chunk storage plain: %w", err)
	}

	if err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_archive_item_logs_item_id_id ON archive_item_logs (archive_item_id, id)`).Error; err != nil {
		return fmt.Errorf("create archive_item_logs item/id index: %w", err)
	}

	return nil
}

func BackfillLegacyArchiveItemLogs(db *gorm.DB) error {
	const batchSize = 100

	for {
		var items []models.ArchiveItem
		if err := db.Select("id", "logs").Where("COALESCE(logs, '') <> ''").Limit(batchSize).Find(&items).Error; err != nil {
			return fmt.Errorf("load legacy archive logs: %w", err)
		}
		if len(items) == 0 {
			return nil
		}

		for _, item := range items {
			if err := backfillLegacyArchiveItemLog(db, item.ID, item.Logs); err != nil {
				return err
			}
		}
	}
}

func backfillLegacyArchiveItemLog(db *gorm.DB, itemID uint, legacyLogs string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var existing int64
		if err := tx.Model(&models.ArchiveItemLog{}).Where("archive_item_id = ?", itemID).Count(&existing).Error; err != nil {
			return fmt.Errorf("count existing archive log chunks for item %d: %w", itemID, err)
		}

		if existing == 0 {
			for _, chunk := range SplitArchiveLogChunks(legacyLogs) {
				if err := tx.Create(&models.ArchiveItemLog{
					ArchiveItemID: itemID,
					Attempt:       0,
					Chunk:         chunk,
				}).Error; err != nil {
					return fmt.Errorf("insert archive log chunk for item %d: %w", itemID, err)
				}
			}
		} else {
			reconstructed, err := ArchiveItemLogString(tx, itemID, "")
			if err != nil {
				return fmt.Errorf("reconstruct archive log chunks for item %d: %w", itemID, err)
			}
			if reconstructed != legacyLogs {
				return fmt.Errorf("item %d has legacy logs and non-matching existing archive_item_logs chunks", itemID)
			}
		}

		reconstructed, err := ArchiveItemLogString(tx, itemID, "")
		if err != nil {
			return fmt.Errorf("verify archive log chunks for item %d: %w", itemID, err)
		}
		if len(reconstructed) != len(legacyLogs) || reconstructed != legacyLogs {
			return fmt.Errorf("archive log backfill verification failed for item %d", itemID)
		}

		result := tx.Model(&models.ArchiveItem{}).Where("id = ?", itemID).Update("logs", "")
		if result.Error != nil {
			return fmt.Errorf("clear legacy archive logs for item %d: %w", itemID, result.Error)
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("clear legacy archive logs for item %d updated %d rows", itemID, result.RowsAffected)
		}

		slog.Debug("Backfilled legacy archive item logs", "archive_item_id", itemID, "bytes", len(legacyLogs))
		return nil
	})
}
