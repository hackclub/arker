package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"arker/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func ApiKeysGet(c *gin.Context, db *gorm.DB) {
	if !RequireLogin(c) {
		return
	}

	var apiKeys []models.APIKey
	// Only get non-deleted API keys (GORM automatically excludes soft-deleted records)
	db.Order("created_at DESC").Find(&apiKeys)
	c.HTML(http.StatusOK, "api_keys.html", gin.H{"apiKeys": apiKeys})
}

func ApiKeysCreate(c *gin.Context, db *gorm.DB) {
	if !RequireLogin(c) {
		return
	}

	var req struct {
		Username    string `json:"username"`
		AppName     string `json:"app_name"`
		Environment string `json:"environment"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	// Validate input
	if req.Username == "" || req.AppName == "" || req.Environment == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "All fields are required"})
		return
	}

	// Sanitize inputs (replace spaces with hyphens, convert to lowercase)
	req.Username = strings.ToLower(strings.ReplaceAll(req.Username, " ", "-"))
	req.AppName = strings.ToLower(strings.ReplaceAll(req.AppName, " ", "-"))
	req.Environment = strings.ToLower(strings.ReplaceAll(req.Environment, " ", "-"))

	// Generate API key
	fullKey, keyHash, err := GenerateAPIKey(req.Username, req.AppName, req.Environment)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate API key"})
		return
	}

	keyPrefix := req.Username + "_" + req.AppName + "_" + req.Environment

	// Check if key prefix already exists
	var existingKey models.APIKey
	if err := db.Where("key_prefix = ?", keyPrefix).First(&existingKey).Error; err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "API key with this combination already exists"})
		return
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error checking existing keys"})
		return
	}

	// Create API key record
	apiKey := models.APIKey{
		Username:    req.Username,
		AppName:     req.AppName,
		Environment: req.Environment,
		KeyHash:     keyHash,
		KeyPrefix:   keyPrefix,
		IsActive:    true,
	}

	if err := db.Create(&apiKey).Error; err != nil {
		// Check if it's a uniqueness constraint violation
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
			c.JSON(http.StatusConflict, gin.H{"error": "API key with this combination already exists"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create API key"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":      apiKey.ID,
		"api_key": fullKey, // Only shown once during creation
		"prefix":  keyPrefix,
	})
}

func ApiKeysToggle(c *gin.Context, db *gorm.DB) {
	if !RequireLogin(c) {
		return
	}

	id := c.Param("id")
	keyID, err := strconv.ParseUint(id, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid key ID"})
		return
	}

	var apiKey models.APIKey
	if err := db.First(&apiKey, keyID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return
	}

	// Toggle active status
	apiKey.IsActive = !apiKey.IsActive
	if err := db.Save(&apiKey).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update API key"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":        apiKey.ID,
		"is_active": apiKey.IsActive,
	})
}

func ApiKeysDelete(c *gin.Context, db *gorm.DB) {
	if !RequireLogin(c) {
		return
	}

	id := c.Param("id")
	keyID, err := strconv.ParseUint(id, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid key ID"})
		return
	}

	// Use Unscoped() to perform a hard delete (permanently remove from database)
	// instead of soft delete which just sets deleted_at timestamp
	if err := db.Unscoped().Delete(&models.APIKey{}, keyID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete API key"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "API key deleted"})
}
