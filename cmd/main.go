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

	// Generate random session secret if not provided
	if cfg.SessionSecret == "" {
		cfg.SessionSecret = generateRandomSecret()
		log.Println("Generated random session secret (consider setting SESSION_SECRET environment variable)")
	}

	db, err := gorm.Open(postgres.Open(cfg.DBURL), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	db.AutoMigrate(&models.User{}, &models.APIKey{}, &models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{})

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

	pw, err := playwright.Run()
	if err != nil {
		log.Fatal("Failed to start Playwright:", err)
	}
	defer pw.Stop()
	
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
		Args: []string{
			"--no-sandbox",
			"--disable-setuid-sandbox",
			"--disable-dev-shm-usage",
			"--disable-extensions",
			"--disable-plugins",
			"--disable-images",
			"--disable-background-timer-throttling",
			"--disable-backgrounding-occluded-windows",
			"--disable-renderer-backgrounding",
		},
	})
	if err != nil {
		log.Fatal("Failed to launch Chromium:", err)
	}
	defer browser.Close()

	archiversMap := map[string]archivers.Archiver{
		"mhtml":      &archivers.MHTMLArchiver{Browser: browser},
		"screenshot": &archivers.ScreenshotArchiver{Browser: browser},
		"git":        &archivers.GitArchiver{},
		"youtube":    &archivers.YTArchiver{},
	}

	os.MkdirAll(cfg.CachePath, 0755)
	// Resume pending archives on startup
	var pendingItems []models.ArchiveItem
	db.Where("status IN (?, ?) AND retry_count < ?", "pending", "processing", 3).Find(&pendingItems)
	for _, item := range pendingItems {
		var capture models.Capture
		db.First(&capture, item.CaptureID)
		var au models.ArchivedURL
		db.First(&au, capture.ArchivedURLID)
		workers.JobChan <- models.Job{CaptureID: capture.ID, ShortID: capture.ShortID, Type: item.Type, URL: au.Original}
		log.Printf("Resuming pending job: %s %s", capture.ShortID, item.Type)
	}

	for i := 1; i <= cfg.MaxWorkers; i++ {
		go workers.Worker(i, workers.JobChan, storageInstance, db, archiversMap)
	}

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
	store := cookie.NewStore([]byte(cfg.SessionSecret))
	r.Use(sessions.Sessions("session", store))

	// Setup routes
	r.GET("/health", healthCheckHandler(db))
	r.GET("/login", handlers.LoginGet)
	r.POST("/login", func(c *gin.Context) { handlers.LoginPost(c, db) })
	r.GET("/admin/api-keys", func(c *gin.Context) { handlers.ApiKeysGet(c, db) })
	r.POST("/admin/api-keys", func(c *gin.Context) { handlers.ApiKeysCreate(c, db) })
	r.POST("/admin/api-keys/:id/toggle", func(c *gin.Context) { handlers.ApiKeysToggle(c, db) })
	r.DELETE("/admin/api-keys/:id", func(c *gin.Context) { handlers.ApiKeysDelete(c, db) })
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
