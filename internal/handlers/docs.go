package handlers

import (
	"net/http"
	"github.com/gin-gonic/gin"
)

func DocsGet(c *gin.Context) {
	c.HTML(http.StatusOK, "docs.html", gin.H{
		"baseURL": c.Request.Host,
	})
}
