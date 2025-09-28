package handlers

import (
	"arker/internal/storage"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ServeItchDebug is a minimal debug version to test basic functionality
func ServeItchDebug(c *gin.Context, storageInstance storage.Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	
	c.JSON(http.StatusOK, gin.H{
		"debug": "basic test",
		"shortid": shortID,
		"timestamp": fmt.Sprintf("%d", 1759084600),
	})
}
