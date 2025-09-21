package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/kelseyhightower/envconfig"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
	"log"
	"log/slog"
	"os"
	"time"

	"arker/internal/archivers"
	"arker/internal/handlers"
	"arker/internal/models"
	"arker/internal/monitoring"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"riverqueue.com/riverui"

	"arker/internal/storage"
	"arker/internal/utils"
	"arker/internal/workers"
)

type Config struct {
	DBURL         string `envconfig:"DB_URL" default:"host=localhost user=user password=pass dbname=arker port=5432 sslmode=disable"`
	StoragePath   string `envconfig:"STORAGE_PATH" default:"./storage"`
	CachePath     string `envconfig:"CACHE_PATH" default:"./cache"`
	MaxWorkers    int    `envconfig:"MAX_WORKERS" default:"5"`
	Port          string `envconfig:"PORT" default:"8080"`
	SessionSecret string `envconfig:"SESSION_SECRET"`
	AdminUsername string `envconfig:"ADMIN_USERNAME" default:"admin"`
	AdminPassword string `envconfig:"ADMIN_PASSWORD" default:"admin"`
	LoginText     string `envconfig:"LOGIN_TEXT"`
	
	// S3 Configuration
	StorageType      string `envconfig:"STORAGE_TYPE" default:"filesystem"` // "filesystem" or "s3"
	S3Endpoint       string `envconfig:"S3_ENDPOINT"`                       // For S3-compatible services
	S3Region         string `envconfig:"S3_REGION" default:"us-east-1"`
	S3AccessKeyID    string `envconfig:"S3_ACCESS_KEY_ID"`
	S3SecretKey      string `envconfig:"S3_SECRET_ACCESS_KEY"`
	S3Bucket         string `envconfig:"S3_BUCKET"`
	S3Prefix         string `envconfig:"S3_PREFIX"`                         // Optional prefix for all keys
	S3ForcePathStyle bool   `envconfig:"S3_FORCE_PATH_STYLE" default:"false"` // Required for MinIO
	S3TempDir        string `envconfig:"S3_TEMP_DIR" default:"/tmp"`         // Temp directory for upload buffering
}

// CustomErrorHandler implements the River ErrorHandler interface
type CustomErrorHandler struct{}

func (h *CustomErrorHandler) HandleError(ctx context.Context, job *rivertype.JobRow, err error) *river.ErrorHandlerResult {
	slog.Error("Job error",
		"job_id", job.ID,
		"kind", job.Kind,
		"attempt", job.Attempt,
		"error", err)
	return nil // Let River handle the retry
}

func (h *CustomErrorHandler) HandlePanic(ctx context.Context, job *rivertype.JobRow, panicErr any, trace string) *river.ErrorHandlerResult {
	slog.Error("Job panicked",
		"job_id", job.ID,
		"kind", job.Kind,
		"attempt", job.Attempt,
		"panic", panicErr,
		"trace", trace)
	return nil // Let River handle the retry
}

func generateRandomSecret() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		log.Fatalf("Failed to generate random session secret: %v", err)
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

func main() {
	// Initialize structured logging
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level:     slog.LevelInfo,
		AddSource: false,
	})))

	var cfg Config
	err := envconfig.Process("", &cfg)
	if err != nil {
		slog.Error("Failed to process environment variables", "error", err)
		log.Fatalf("Failed to process config: %v", err)
	}

	slog.Info("Starting Arker archive server",
		"max_workers", cfg.MaxWorkers,
		"storage_path", cfg.StoragePath,
		"cache_path", cfg.CachePath)

	// Note: Session secret is now handled after database connection

	// Create shared pgx pool for both River and GORM
	pgxConfig, err := pgxpool.ParseConfig(cfg.DBURL)
	if err != nil {
		log.Fatalf("Failed to parse database URL: %v", err)
	}
	dbPool, err := pgxpool.NewWithConfig(context.Background(), pgxConfig)
	if err != nil {
		log.Fatalf("Failed to create database pool: %v", err)
	}
	defer dbPool.Close()

	// Create GORM DB using the shared pgx pool
	sqlDB := stdlib.OpenDBFromPool(dbPool)
	db, err := gorm.Open(postgres.New(postgres.Config{
		Conn: sqlDB,
	}), &gorm.Config{})
	if err != nil {
		log.Fatalf("Failed to initialize GORM with shared pool: %v", err)
	}

	// Auto-migrate database models
	if err := db.AutoMigrate(&models.User{}, &models.APIKey{}, &models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.Config{}); err != nil {
		slog.Error("AutoMigrate failed with detailed error", "error", err, "error_type", fmt.Sprintf("%T", err), "error_string", err.Error())
		slog.Info("Continuing startup despite AutoMigrate error")
	}

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
			log.Fatalf("Failed to get/create session secret: %v", err)
		}
		sessionSecret = dbSecret
		if dbSecret == generatedSecret {
			log.Println("Generated new session secret and stored in database")
		} else {
			log.Println("Using existing session secret from database")
		}
	}

	// Initialize storage backend
	var baseStorage storage.SeekableStorage
	var storageErr error
	
	switch cfg.StorageType {
	case "s3":
		log.Printf("Initializing S3 storage (bucket: %s, region: %s)", cfg.S3Bucket, cfg.S3Region)
		if cfg.S3Bucket == "" {
			log.Fatal("S3_BUCKET environment variable is required when using S3 storage")
		}
		
		s3Config := storage.S3Config{
			Endpoint:        cfg.S3Endpoint,
			Region:          cfg.S3Region,
			AccessKeyID:     cfg.S3AccessKeyID,
			SecretAccessKey: cfg.S3SecretKey,
			Bucket:          cfg.S3Bucket,
			Prefix:          cfg.S3Prefix,
			ForcePathStyle:  cfg.S3ForcePathStyle,
			TempDir:         cfg.S3TempDir,
		}
		
		baseStorage, storageErr = storage.NewS3Storage(context.Background(), s3Config)
		if storageErr != nil {
			log.Fatalf("Failed to initialize S3 storage: %v", storageErr)
		}
		
	default: // "filesystem"
		log.Printf("Initializing filesystem storage (path: %s)", cfg.StoragePath)
		baseStorage = storage.NewFSStorage(cfg.StoragePath)
	}
	
	storageInstance := storage.NewZSTDStorage(baseStorage)

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
		log.Fatalf("Failed to create River migrator: %v", err)
	}
	if _, err := migrator.Migrate(context.Background(), rivermigrate.DirectionUp, nil); err != nil {
		log.Fatalf("Failed to run River migrations: %v", err)
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
			"high_priority":    {MaxWorkers: max(2, cfg.MaxWorkers/2)}, // At least 2 workers, or half of total workers
		},
		Workers:              riverWorkers,
		JobTimeout:           30 * time.Minute, // Kill jobs running longer than 30 minutes
		RescueStuckJobsAfter: 35 * time.Minute, // Rescue stuck jobs after 35 minutes (must be >= JobTimeout)
		ErrorHandler:         &CustomErrorHandler{},
	})
	if err != nil {
		log.Fatalf("Failed to create River client: %v", err)
	}

	// Start River client
	if err := riverClient.Start(context.Background()); err != nil {
		log.Fatalf("Failed to start River client: %v", err)
	}
	defer riverClient.Stop(context.Background())

	// Setup River UI
	riverUIServer, err := riverui.NewServer(&riverui.ServerOpts{
		Client: riverClient,
		DB:     dbPool,
		Logger: slog.Default(),
		Prefix: "/queue",
	})
	if err != nil {
		log.Fatalf("Failed to create River UI server: %v", err)
	}

	// Start River UI server
	if err := riverUIServer.Start(context.Background()); err != nil {
		log.Fatalf("Failed to start River UI server: %v", err)
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
	r.GET("/health", handlers.HealthCheckHandler(db))
	r.GET("/metrics/browser", handlers.BrowserMetricsHandler())
	r.GET("/status/browser", handlers.BrowserStatusHandler())
	r.GET("/login", func(c *gin.Context) { handlers.LoginGet(c, cfg.LoginText) })
	r.POST("/login", func(c *gin.Context) { handlers.LoginPost(c, db, cfg.LoginText) })
	r.GET("/admin/api-keys", func(c *gin.Context) { handlers.ApiKeysGet(c, db) })
	r.POST("/admin/api-keys", func(c *gin.Context) { handlers.ApiKeysCreate(c, db) })
	r.POST("/admin/api-keys/:id/toggle", func(c *gin.Context) { handlers.ApiKeysToggle(c, db) })
	r.DELETE("/admin/api-keys/:id", func(c *gin.Context) { handlers.ApiKeysDelete(c, db) })
	r.POST("/admin/retry-failed", func(c *gin.Context) { handlers.RetryAllFailedJobs(c, db, riverClient) })
	r.POST("/admin/url/:id/capture", func(c *gin.Context) { handlers.RequestCapture(c, db, riverClient) })
	r.POST("/admin/archive", func(c *gin.Context) { handlers.AdminArchive(c, db, riverClient) })
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
	r.POST("/api/v1/archive", handlers.RequireAPIKey(db), func(c *gin.Context) { handlers.ApiArchive(c, db, riverClient) })
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
