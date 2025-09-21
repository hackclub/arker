package handlers

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

func DocsGet(c *gin.Context) {
	c.HTML(http.StatusOK, "docs.html", gin.H{
		"baseURL": c.Request.Host,
	})
}
