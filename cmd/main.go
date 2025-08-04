package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/kelseyhightower/envconfig"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"riverqueue.com/riverui"
	"arker/internal/archivers"
	"arker/internal/handlers"
	"arker/internal/models"
	"arker/internal/monitoring"

	"arker/internal/storage"
	"arker/internal/utils"
	"arker/internal/workers"
)

type Config struct {
	DBURL          string `envconfig:"DB_URL" default:"host=localhost user=user password=pass dbname=arker port=5432 sslmode=disable"`
	StoragePath    string `envconfig:"STORAGE_PATH" default:"./storage"`
	CachePath      string `envconfig:"CACHE_PATH" default:"./cache"`
	MaxWorkers     int    `envconfig:"MAX_WORKERS" default:"5"`
	Port           string `envconfig:"PORT" default:"8080"`
	SessionSecret  string `envconfig:"SESSION_SECRET"`
	AdminUsername  string `envconfig:"ADMIN_USERNAME" default:"admin"`
	AdminPassword  string `envconfig:"ADMIN_PASSWORD" default:"admin"`
	LoginText      string `envconfig:"LOGIN_TEXT"`
}

func generateRandomSecret() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		log.Fatal("Failed to generate random session secret:", err)
	}
	return hex.EncodeToString(bytes)
}

// getOrCreateConfigValue retrieves a config value from database or creates it with a default
func getOrCreateConfigValue(db *gorm.DB, key string, defaultValue string) (string, error) {
	var config models.Config
	if err := db.Where("key = ?", key).First(&config).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			// Create new config entry
			config = models.Config{Key: key, Value: defaultValue}
			if err := db.Create(&config).Error; err != nil {
				return "", err
			}
			log.Printf("Created new config entry: %s", key)
			return defaultValue, nil
		}
		return "", err
	}
	return config.Value, nil
}

func healthCheckHandler(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check database connectivity
		sqlDB, err := db.DB()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "unhealthy",
				"error":  "database connection failed",
			})
			return
		}

		if err := sqlDB.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "unhealthy",
				"error":  "database ping failed",
			})
			return
		}

		// Browser health is checked per-job, no global browser manager
		// App is ready
		c.JSON(http.StatusOK, gin.H{
			"status": "healthy",
		})
	}
}

func browserMetricsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		monitor := monitoring.GetGlobalMonitor()
		metrics := monitor.GetMetrics()
		
		c.JSON(http.StatusOK, metrics)
	}
}

func browserStatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		monitor := monitoring.GetGlobalMonitor()
		metrics := monitor.GetMetrics()
		
		status := "healthy"
		if metrics.LeakDetected {
			status = "leak_detected"
		}
		
		response := gin.H{
			"status":                status,
			"chrome_process_count":  metrics.ChromeProcessCount,
			"total_goroutines":      metrics.TotalGoroutines,
			"launch_close_balance":  metrics.PlaywrightLaunches - metrics.PlaywrightCloses,
			"create_cleanup_balance": metrics.BrowserCreations - metrics.BrowserCleanups,
			"leak_detected":         metrics.LeakDetected,
			"leak_reason":           metrics.LeakReason,
			"last_updated":          metrics.LastUpdated,
		}
		
		if metrics.LeakDetected {
			c.JSON(http.StatusServiceUnavailable, response)
		} else {
			c.JSON(http.StatusOK, response)
		}
	}
}

func populateFileSizes(db *gorm.DB, storage storage.Storage) {
	var items []models.ArchiveItem
	// Find completed archive items that don't have file size set
	db.Where("status = ? AND (file_size = 0 OR file_size IS NULL) AND storage_key != ''", "completed").Find(&items)
	
	if len(items) == 0 {
		return
	}
	
	log.Printf("Populating file sizes for %d existing archives...", len(items))
	updated := 0
	
	for _, item := range items {
		if item.StorageKey == "" {
			continue
		}
		
		size, err := storage.Size(item.StorageKey)
		if err != nil {
			log.Printf("Warning: Could not get size for %s: %v", item.StorageKey, err)
			continue
		}
		
		if size > 0 {
			db.Model(&item).Update("file_size", size)
			updated++
		}
	}
	
	if updated > 0 {
		log.Printf("Updated file sizes for %d archives", updated)
	}
}

// cleanupOrphanedPendingJobs finds archive items marked as pending but without corresponding River jobs
func cleanupOrphanedPendingJobs(ctx context.Context, riverClient *river.Client[pgx.Tx], db *gorm.DB) error {
	// Get all pending archive items
	var pendingItems []models.ArchiveItem
	if err := db.Where("status = 'pending'").Find(&pendingItems).Error; err != nil {
		return err
	}

	if len(pendingItems) == 0 {
		slog.Info("No pending jobs found during startup cleanup")
		return nil
	}

	slog.Info("Checking for orphaned pending jobs", "pending_count", len(pendingItems))

	// Get all current jobs from River with archive tags
	params := river.NewJobListParams().
		Kinds("archive").
		First(1000) // Adjust if you expect more than 1000 jobs
	
	jobs, err := riverClient.JobList(ctx, params)
	if err != nil {
		return err
	}

	// Create a map of River job args for quick lookup
	riverJobMap := make(map[string]bool)
	for _, job := range jobs.Jobs {
		// Try to unmarshal the job args to get the job details
		if job.Kind == "archive" {
			// Parse the job args as ArchiveJobArgs
			var args workers.ArchiveJobArgs
			if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
				slog.Warn("Failed to unmarshal job args", "job_id", job.ID, "error", err)
				continue
			}
			// Create a key based on capture_id and type to match with pending items
			key := fmt.Sprintf("%d-%s", args.CaptureID, args.Type)
			riverJobMap[key] = true
		}
	}

	orphanedCount := 0
	for _, item := range pendingItems {
		// Create the same key format
		key := fmt.Sprintf("%d-%s", item.CaptureID, item.Type)
		
		if !riverJobMap[key] {
			// This pending item doesn't have a corresponding River job
			currentTime := time.Now()
			item.Status = "failed"
			item.Logs = fmt.Sprintf("Startup cleanup at %s: Job was pending but not found in River queue (likely lost due to restart)\n%s", 
				currentTime.Format("2006-01-02 15:04:05"), item.Logs)
			
			if err := db.Save(&item).Error; err != nil {
				slog.Error("Failed to mark orphaned job as failed", 
					"item_id", item.ID, 
					"capture_id", item.CaptureID, 
					"type", item.Type, 
					"error", err)
				continue
			}
			
			orphanedCount++
			slog.Info("Marked orphaned pending job as failed", 
				"item_id", item.ID, 
				"capture_id", item.CaptureID, 
				"type", item.Type)
		}
	}

	if orphanedCount > 0 {
		slog.Info("Startup cleanup completed", "orphaned_jobs_marked_failed", orphanedCount)
	} else {
		slog.Info("No orphaned pending jobs found - all pending jobs have corresponding River jobs")
	}

	return nil
}

func main() {
	// Initialize structured logging
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		AddSource: false,
	})))
	
	var cfg Config
	err := envconfig.Process("", &cfg)
	if err != nil {
		slog.Error("Failed to process environment variables", "error", err)
		log.Fatal("Failed to process config:", err)
	}
	
	slog.Info("Starting Arker archive server",
		"max_workers", cfg.MaxWorkers,
		"storage_path", cfg.StoragePath,
		"cache_path", cfg.CachePath)

	// Note: Session secret is now handled after database connection

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
	
	db.AutoMigrate(&models.User{}, &models.APIKey{}, &models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.Config{})

	// Get or generate session secret from database (overrides environment variable if not set)
	var sessionSecret string
	if cfg.SessionSecret != "" {
		// Use environment variable if provided
		sessionSecret = cfg.SessionSecret
		log.Println("Using session secret from environment variable")
	} else {
		// Get or create session secret in database
		generatedSecret := generateRandomSecret()
		dbSecret, err := getOrCreateConfigValue(db, "session_secret", generatedSecret)
		if err != nil {
			log.Fatal("Failed to get/create session secret:", err)
		}
		sessionSecret = dbSecret
		if dbSecret == generatedSecret {
			log.Println("Generated new session secret and stored in database")
		} else {
			log.Println("Using existing session secret from database")
		}
	}

	fsStorage := storage.NewFSStorage(cfg.StoragePath)
	storageInstance := storage.NewZSTDStorage(fsStorage)
	
	// Populate file sizes for existing archives
	populateFileSizes(db, storageInstance)

	// Perform health checks on startup
	log.Println("Performing startup health checks...")
	healthConfig := utils.DefaultHealthCheckConfig()
	if err := utils.RunHealthChecks(healthConfig); err != nil {
		log.Printf("Health check warning: %v", err)
		// Continue startup even if health checks fail (non-critical)
	} else {
		log.Println("All health checks passed")
	}
	


	// Default user
	var user models.User
	if db.First(&user).Error == gorm.ErrRecordNotFound {
		hashed, err := bcrypt.GenerateFromPassword([]byte(cfg.AdminPassword), bcrypt.DefaultCost)
		if err != nil {
			log.Fatal(err)
		}
		user = models.User{Username: cfg.AdminUsername, PasswordHash: string(hashed)}
		db.Create(&user)
		log.Printf("Created default admin user: %s/%s", cfg.AdminUsername, cfg.AdminPassword)
	}

	// No shared browser manager - each job creates its own browser instance
	archiversMap := map[string]archivers.Archiver{
		"mhtml":      &archivers.MHTMLArchiver{},
		"screenshot": &archivers.ScreenshotArchiver{},
		"git":        &archivers.GitArchiver{},
		"youtube":    &archivers.YTArchiver{},
	}

	os.MkdirAll(cfg.CachePath, 0755)

	// Initialize browser monitoring
	monitor := monitoring.GetGlobalMonitor()
	slog.Info("Browser monitoring initialized")
	

	
	// Initialize River job queue
	slog.Info("Starting River job queue", "worker_count", cfg.MaxWorkers)
	
	// Run River migrations using shared pool
	migrator, err := rivermigrate.New(riverpgxv5.New(dbPool), nil)
	if err != nil {
		log.Fatal("Failed to create River migrator:", err)
	}
	if _, err := migrator.Migrate(context.Background(), rivermigrate.DirectionUp, nil); err != nil {
		log.Fatal("Failed to run River migrations:", err)
	}
	slog.Info("River migrations completed successfully")

	// Create worker registry
	riverWorkers := river.NewWorkers()
	archiveWorker := workers.NewArchiveWorker(storageInstance, db, archiversMap)
	river.AddWorker(riverWorkers, archiveWorker)

	// Create River client with configuration
	riverClient, err := river.NewClient(riverpgxv5.New(dbPool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.MaxWorkers},
		},
		Workers: riverWorkers,
	})
	if err != nil {
		log.Fatal("Failed to create River client:", err)
	}

	// Start River client
	if err := riverClient.Start(context.Background()); err != nil {
		log.Fatal("Failed to start River client:", err)
	}
	defer riverClient.Stop(context.Background())

	// Create River queue manager for API handlers
	riverQueueManager := workers.NewRiverQueueManager(riverClient, db)

	// Clean up orphaned pending jobs (jobs that are marked pending but not in River)
	if err := cleanupOrphanedPendingJobs(context.Background(), riverClient, db); err != nil {
		slog.Error("Failed to cleanup orphaned pending jobs", "error", err)
	}

	// Setup River UI
	riverUIServer, err := riverui.NewServer(&riverui.ServerOpts{
		Client: riverClient,
		DB:     dbPool,
		Logger: slog.Default(),
		Prefix: "/queue",
	})
	if err != nil {
		log.Fatal("Failed to create River UI server:", err)
	}

	// Start River UI server
	if err := riverUIServer.Start(context.Background()); err != nil {
		log.Fatal("Failed to start River UI server:", err)
	}
	
	// Log initial browser status
	monitor.LogCurrentStatus()

	slog.Info("River job queue started successfully")

	// Start log cleanup routine
	go func() {
		for {
			time.Sleep(24 * time.Hour)
			result := db.Model(&models.ArchiveItem{}).Where("status = ? AND updated_at < ?", "completed", time.Now().Add(-30*24*time.Hour)).Update("logs", "")
			if result.RowsAffected > 0 {
				log.Printf("Cleaned up logs for %d completed archives older than 30 days", result.RowsAffected)
			}
		}
	}()

	r := gin.Default()
	r.LoadHTMLGlob("templates/*.html")
	store := cookie.NewStore([]byte(sessionSecret))
	r.Use(sessions.Sessions("session", store))

	// Setup routes
	r.GET("/health", healthCheckHandler(db))
	r.GET("/metrics/browser", browserMetricsHandler())
	r.GET("/status/browser", browserStatusHandler())
	r.GET("/login", func(c *gin.Context) { handlers.LoginGet(c, cfg.LoginText) })
	r.POST("/login", func(c *gin.Context) { handlers.LoginPost(c, db, cfg.LoginText) })
	r.GET("/admin/api-keys", func(c *gin.Context) { handlers.ApiKeysGet(c, db) })
	r.POST("/admin/api-keys", func(c *gin.Context) { handlers.ApiKeysCreate(c, db) })
	r.POST("/admin/api-keys/:id/toggle", func(c *gin.Context) { handlers.ApiKeysToggle(c, db) })
	r.DELETE("/admin/api-keys/:id", func(c *gin.Context) { handlers.ApiKeysDelete(c, db) })
	r.POST("/admin/retry-failed", func(c *gin.Context) { handlers.RetryAllFailedJobs(c, db, riverQueueManager) })
	r.POST("/admin/url/:id/capture", func(c *gin.Context) { handlers.RequestCapture(c, db, riverQueueManager) })
	r.POST("/admin/archive", func(c *gin.Context) { handlers.AdminArchive(c, db, riverQueueManager) })
	r.GET("/admin/item/:id/log", func(c *gin.Context) { handlers.GetItemLog(c, db) })
	// Create protected River UI routes
	r.GET("/queue", func(c *gin.Context) {
		if !handlers.RequireLogin(c) {
			return
		}
		riverUIServer.ServeHTTP(c.Writer, c.Request)
	})
	r.Any("/queue/*path", func(c *gin.Context) {
		if !handlers.RequireLogin(c) {
			return
		}
		riverUIServer.ServeHTTP(c.Writer, c.Request)
	})
	r.GET("/docs", handlers.DocsGet)
	r.POST("/api/v1/archive", handlers.RequireAPIKey(db), func(c *gin.Context) { handlers.ApiArchive(c, db, riverQueueManager) })
	r.GET("/api/v1/past-archives", handlers.RequireAPIKey(db), func(c *gin.Context) { handlers.ApiPastArchives(c, db) })
	r.GET("/web/past-archives", func(c *gin.Context) { handlers.WebPastArchives(c, db) })
	r.GET("/logs/:shortid/:type", func(c *gin.Context) { handlers.GetLogs(c, db) })
	r.GET("/archive/:shortid/:type", func(c *gin.Context) { handlers.ServeArchive(c, storageInstance, db) })
	r.GET("/archive/:shortid/mhtml/html", func(c *gin.Context) { handlers.ServeMHTMLAsHTML(c, storageInstance, db) })
	r.Any("/git/*path", func(c *gin.Context) { handlers.GitHandler(c, storageInstance, db, cfg.CachePath) })
	r.GET("/:shortid/:type", func(c *gin.Context) { handlers.DisplayType(c, db) })
	r.GET("/:shortid", func(c *gin.Context) { handlers.DisplayDefault(c, db) })
	r.GET("/", func(c *gin.Context) { handlers.AdminGet(c, db) })

	slog.Info("Starting HTTP server", "port", cfg.Port, "address", "0.0.0.0:"+cfg.Port)
	r.Run("0.0.0.0:" + cfg.Port)
}
