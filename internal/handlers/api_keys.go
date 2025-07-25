package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"arker/internal/models"
)

func ApiKeysGet(c *gin.Context, db *gorm.DB) {
	if !RequireLogin(c) {
		return
	}

	var apiKeys []models.APIKey
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

	// Validate environment
	if req.Environment != "dev" && req.Environment != "prod" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Environment must be 'dev' or 'prod'"})
		return
	}

	// Sanitize inputs (replace spaces with hyphens, convert to lowercase)
	req.Username = strings.ToLower(strings.ReplaceAll(req.Username, " ", "-"))
	req.AppName = strings.ToLower(strings.ReplaceAll(req.AppName, " ", "-"))

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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create API key"})
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

	if err := db.Delete(&models.APIKey{}, keyID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete API key"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "API key deleted"})
}
