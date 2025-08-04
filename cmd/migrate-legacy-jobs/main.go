package main

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/kelseyhightower/envconfig"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/workers"
)

type Config struct {
	DBURL string `envconfig:"DB_URL" default:"host=localhost user=user password=pass dbname=arker port=5432 sslmode=disable"`
}

func main() {
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		log.Fatal("Failed to load config:", err)
	}

	log.Println("Starting legacy job migration...")

	// Create shared pgx pool for both River and GORM
	pgxConfig, err := pgxpool.ParseConfig(cfg.DBURL)
	if err != nil {
		log.Fatal("Failed to parse database URL:", err)
	}
	dbPool, err := pgxpool.NewWithConfig(context.Background(), pgxConfig)
	if err != nil {
		log.Fatal("Failed to create database pool:", err)
	}
	defer dbPool.Close()

	// Create GORM DB using the shared pgx pool
	sqlDB := stdlib.OpenDBFromPool(dbPool)
	db, err := gorm.Open(postgres.New(postgres.Config{
		Conn: sqlDB,
	}), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to initialize GORM with shared pool:", err)
	}

	// Create River client
	riverClient, err := river.NewClient(riverpgxv5.New(dbPool), &river.Config{
		Workers: river.NewWorkers(),
	})
	if err != nil {
		log.Fatal("Failed to create River client:", err)
	}

	// Find all pending archive items with their captures and URLs
	var pendingItems []models.ArchiveItem
	result := db.Joins("JOIN captures ON archive_items.capture_id = captures.id").
		Joins("JOIN archived_urls ON captures.archived_url_id = archived_urls.id").
		Select("archive_items.*, captures.short_id, archived_urls.original").
		Where("archive_items.status = ?", "pending").
		Find(&pendingItems)
	if result.Error != nil {
		log.Fatal("Failed to find pending items:", result.Error)
	}

	log.Printf("Found %d pending archive items to migrate", len(pendingItems))

	migrated := 0
	skipped := 0

	for _, item := range pendingItems {
		// Get URL and short_id for this item
		var capture models.Capture
		if err := db.Preload("ArchivedURL").Where("id = ?", item.CaptureID).First(&capture).Error; err != nil {
			log.Printf("Skipping item %d: failed to get capture: %v", item.ID, err)
			skipped++
			continue
		}

		if capture.ArchivedURL.Original == "" {
			log.Printf("Skipping item %d: missing URL", item.ID)
			skipped++
			continue
		}

		// Create River job for this item
		args := workers.ArchiveJobArgs{
			CaptureID: item.CaptureID,
			ShortID:   capture.ShortID,
			Type:      item.Type,
			URL:       capture.ArchivedURL.Original,
		}

		opts := &river.InsertOpts{
			MaxAttempts: 3,
			Tags:        []string{"archive", item.Type, "migrated"},
			UniqueOpts: river.UniqueOpts{
				ByArgs:   true,
				ByPeriod: 1 * time.Minute,
			},
		}

		ctx := context.Background()
		_, err := riverClient.Insert(ctx, args, opts)
		if err != nil {
			log.Printf("Failed to enqueue job for item %d: %v", item.ID, err)
			skipped++
			continue
		}

		migrated++
	}

	log.Printf("Migration completed: %d jobs migrated, %d skipped", migrated, skipped)
}
