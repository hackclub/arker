package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"time"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/kelseyhightower/envconfig"
	"github.com/playwright-community/playwright-go"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"arker/internal/archivers"
	"arker/internal/browsermgr"
	"arker/internal/handlers"
	"arker/internal/models"
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

func healthCheckHandler(db *gorm.DB, bm *browsermgr.Manager) gin.HandlerFunc {
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

		// Check browser manager health
		if !bm.Healthy() {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "unhealthy",
				"error":  "browser unavailable",
			})
			return
		}

		// App is ready
		c.JSON(http.StatusOK, gin.H{
			"status": "healthy",
		})
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

func main() {
	var cfg Config
	err := envconfig.Process("", &cfg)
	if err != nil {
		log.Fatal("Failed to process config:", err)
	}

	// Note: Session secret is now handled after database connection

	db, err := gorm.Open(postgres.Open(cfg.DBURL), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
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

	// Create browser manager with launch options
	launchArgs := []string{
		"--no-sandbox",
		"--disable-setuid-sandbox",
		"--disable-dev-shm-usage",
		"--disable-extensions",
		"--disable-plugins",
		"--disable-images",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
	}
	
	// Add SOCKS5 proxy configuration if SOCKS5_PROXY is set
	if socks5Proxy := os.Getenv("SOCKS5_PROXY"); socks5Proxy != "" {
		launchArgs = append(launchArgs, "--proxy-server="+socks5Proxy)
		log.Printf("Using SOCKS5 proxy for browser: %s", socks5Proxy)
	}
	
	launchOpts := playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
		Args:     launchArgs,
	}
	
	bm, err := browsermgr.New(launchOpts, cfg.MaxWorkers)
	if err != nil {
		log.Fatal("Failed to start browser manager:", err)
	}
	defer bm.Close()

	archiversMap := map[string]archivers.Archiver{
		"mhtml":      &archivers.MHTMLArchiver{BrowserMgr: bm},
		"screenshot": &archivers.ScreenshotArchiver{BrowserMgr: bm},
		"git":        &archivers.GitArchiver{},
		"youtube":    &archivers.YTArchiver{},
	}

	os.MkdirAll(cfg.CachePath, 0755)
	// Resume pending archives on startup and handle stuck jobs from previous run
	var pendingItems []models.ArchiveItem
	db.Where("status = 'pending' AND retry_count < ?", 3).Find(&pendingItems)
	for _, item := range pendingItems {
		var capture models.Capture
		db.First(&capture, item.CaptureID)
		var au models.ArchivedURL
		db.First(&au, capture.ArchivedURLID)
		workers.JobChan <- models.Job{CaptureID: capture.ID, ShortID: capture.ShortID, Type: item.Type, URL: au.Original}
		log.Printf("Resuming pending job: %s %s", capture.ShortID, item.Type)
	}
	
	// Handle jobs that were stuck in "processing" status (likely from server restart)
	var stuckItems []models.ArchiveItem
	db.Where("status = 'processing'").Find(&stuckItems)
	for _, item := range stuckItems {
		// Mark as failed with explanation, they'll be retried if under limit
		failureMsg := "\n--- RECOVERY FROM SERVER RESTART ---\nJob was processing when server restarted\nMarking as failed for automatic retry\n"
		db.Model(&item).Updates(map[string]interface{}{
			"status": "failed", 
			"logs": item.Logs + failureMsg,
		})
		log.Printf("Marked stuck job as failed for retry: ID=%d, Type=%s", item.ID, item.Type)
	}

	for i := 1; i <= cfg.MaxWorkers; i++ {
		go workers.Worker(i, workers.JobChan, storageInstance, db, archiversMap)
	}

	// Start job monitor for stuck job detection and cleanup
	jobMonitor := workers.NewJobMonitor(db)
	jobMonitor.Start()
	defer jobMonitor.Stop()

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
	r.GET("/health", healthCheckHandler(db, bm))
	r.GET("/login", handlers.LoginGet)
	r.POST("/login", func(c *gin.Context) { handlers.LoginPost(c, db) })
	r.GET("/admin/api-keys", func(c *gin.Context) { handlers.ApiKeysGet(c, db) })
	r.POST("/admin/api-keys", func(c *gin.Context) { handlers.ApiKeysCreate(c, db) })
	r.POST("/admin/api-keys/:id/toggle", func(c *gin.Context) { handlers.ApiKeysToggle(c, db) })
	r.DELETE("/admin/api-keys/:id", func(c *gin.Context) { handlers.ApiKeysDelete(c, db) })
	r.POST("/admin/retry-failed", func(c *gin.Context) { handlers.RetryAllFailedJobs(c, db) })
	r.POST("/admin/url/:id/capture", func(c *gin.Context) { handlers.RequestCapture(c, db) })
	r.POST("/admin/archive", func(c *gin.Context) { handlers.AdminArchive(c, db) })
	r.GET("/admin/item/:id/log", func(c *gin.Context) { handlers.GetItemLog(c, db) })
	r.GET("/docs", handlers.DocsGet)
	r.POST("/api/v1/archive", handlers.RequireAPIKey(db), func(c *gin.Context) { handlers.ApiArchive(c, db) })
	r.GET("/api/v1/past-archives", handlers.RequireAPIKey(db), func(c *gin.Context) { handlers.ApiPastArchives(c, db) })
	r.GET("/web/past-archives", func(c *gin.Context) { handlers.WebPastArchives(c, db) })
	r.GET("/logs/:shortid/:type", func(c *gin.Context) { handlers.GetLogs(c, db) })
	r.GET("/archive/:shortid/:type", func(c *gin.Context) { handlers.ServeArchive(c, storageInstance, db) })
	r.GET("/archive/:shortid/mhtml/html", func(c *gin.Context) { handlers.ServeMHTMLAsHTML(c, storageInstance, db) })
	r.Any("/git/*path", func(c *gin.Context) { handlers.GitHandler(c, storageInstance, db, cfg.CachePath) })
	r.GET("/:shortid/:type", func(c *gin.Context) { handlers.DisplayType(c, db) })
	r.GET("/:shortid", func(c *gin.Context) { handlers.DisplayDefault(c, db) })
	r.GET("/", func(c *gin.Context) { handlers.AdminGet(c, db) })

	log.Printf("Starting server on :%s", cfg.Port)
	r.Run(":" + cfg.Port)
}
