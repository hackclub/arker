package main

import (
	"log"
	"os"
	"strconv"
	"time"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
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

func main() {
	dsn := os.Getenv("DB_URL")
	if dsn == "" {
		dsn = "host=localhost user=user password=pass dbname=arker port=5432 sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	db.AutoMigrate(&models.User{}, &models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{})

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
		hashed, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
		if err != nil {
			log.Fatal(err)
		}
		user = models.User{Username: "admin", PasswordHash: string(hashed)}
		db.Create(&user)
		log.Println("Created default admin user: admin/admin")
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

	storagePath := os.Getenv("STORAGE_PATH")
	if storagePath == "" {
		storagePath = "./storage"
	}
	storageInstance := storage.NewFSStorage(storagePath)

	cachePath := os.Getenv("CACHE_PATH")
	if cachePath == "" {
		cachePath = "./cache"
	}
	os.MkdirAll(cachePath, 0755)

	maxWorkers, _ := strconv.Atoi(os.Getenv("MAX_WORKERS"))
	if maxWorkers <= 0 {
		maxWorkers = 5
	}
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

	for i := 1; i <= maxWorkers; i++ {
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
	store := cookie.NewStore([]byte("secret-key-change-in-production"))
	r.Use(sessions.Sessions("session", store))

	// Setup routes
	r.GET("/login", handlers.LoginGet)
	r.POST("/login", func(c *gin.Context) { handlers.LoginPost(c, db) })
	r.GET("/admin", func(c *gin.Context) { handlers.AdminGet(c, db) })
	r.POST("/admin/url/:id/capture", func(c *gin.Context) { handlers.RequestCapture(c, db) })
	r.GET("/admin/item/:id/log", func(c *gin.Context) { handlers.GetItemLog(c, db) })
	r.POST("/api/v1/archive", func(c *gin.Context) { handlers.ApiArchive(c, db) })
	r.GET("/api/v1/past-archives", func(c *gin.Context) { handlers.ApiPastArchives(c, db) })
	r.GET("/:shortid", func(c *gin.Context) { handlers.DisplayGet(c, db) })
	r.GET("/logs/:shortid/:type", func(c *gin.Context) { handlers.GetLogs(c, db) })
	r.GET("/archive/:shortid/:type", func(c *gin.Context) { handlers.ServeArchive(c, storageInstance, db) })
	r.GET("/archive/:shortid/mhtml/html", func(c *gin.Context) { handlers.ServeMHTMLAsHTML(c, storageInstance, db) })
	r.Any("/git/*path", func(c *gin.Context) { handlers.GitHandler(c, storageInstance, db, cachePath) })

	log.Println("Starting server on :8080")
	r.Run(":8080")
}
