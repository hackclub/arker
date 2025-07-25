package handlers

import (
	"net/http"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"arker/internal/models"
)

func LoginGet(c *gin.Context) {
	c.HTML(http.StatusOK, "login.html", nil)
}

func LoginPost(c *gin.Context, db *gorm.DB) {
	username := c.PostForm("username")
	password := c.PostForm("password")
	var user models.User
	if err := db.Where("username = ?", username).First(&user).Error; err != nil {
		c.HTML(http.StatusBadRequest, "login.html", gin.H{"error": "Invalid credentials"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		c.HTML(http.StatusBadRequest, "login.html", gin.H{"error": "Invalid credentials"})
		return
	}
	session := sessions.Default(c)
	session.Set("user_id", user.ID)
	session.Save()
	c.Redirect(http.StatusFound, "/admin")
}

func RequireLogin(c *gin.Context) bool {
	session := sessions.Default(c)
	if session.Get("user_id") == nil {
		c.Redirect(http.StatusFound, "/login")
		return false
	}
	return true
}
