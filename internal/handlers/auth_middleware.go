package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"arker/internal/models"
)

// RequireAPIKey middleware validates API key authentication
func RequireAPIKey(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization header required"})
			c.Abort()
			return
		}

		if !strings.HasPrefix(authHeader, "Bearer ") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid authorization format. Use 'Bearer <api_key>'"})
			c.Abort()
			return
		}

		apiKey := strings.TrimPrefix(authHeader, "Bearer ")
		if len(apiKey) < 10 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key format"})
			c.Abort()
			return
		}

		// Extract prefix (everything before the last underscore)
		parts := strings.Split(apiKey, "_")
		if len(parts) < 4 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key format"})
			c.Abort()
			return
		}
		
		keyPrefix := strings.Join(parts[:3], "_") // username_appname_environment

		var dbAPIKey models.APIKey
		if err := db.Where("key_prefix = ? AND is_active = ?", keyPrefix, true).First(&dbAPIKey).Error; err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key"})
			c.Abort()
			return
		}

		// Verify the full key against the stored hash
		if err := bcrypt.CompareHashAndPassword([]byte(dbAPIKey.KeyHash), []byte(apiKey)); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key"})
			c.Abort()
			return
		}

		// Update last used timestamp
		now := time.Now()
		db.Model(&dbAPIKey).Update("last_used_at", &now)

		// Store API key info in context for handlers
		c.Set("api_key", &dbAPIKey)
		c.Next()
	}
}

// GenerateAPIKey creates a new API key with the specified parameters
func GenerateAPIKey(username, appName, environment string) (string, string, error) {
	// Generate random suffix
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", "", err
	}
	randomSuffix := hex.EncodeToString(randomBytes)

	keyPrefix := fmt.Sprintf("%s_%s_%s", username, appName, environment)
	fullKey := fmt.Sprintf("%s_%s", keyPrefix, randomSuffix)

	// Hash the full key for storage
	keyHash, err := bcrypt.GenerateFromPassword([]byte(fullKey), bcrypt.DefaultCost)
	if err != nil {
		return "", "", err
	}

	return fullKey, string(keyHash), nil
}
