package handlers

import (
	"net/http"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"arker/internal/models"
)

func LoginGet(c *gin.Context, loginText string) {
	c.HTML(http.StatusOK, "login.html", gin.H{
		"login_text": loginText,
	})
}

func LoginPost(c *gin.Context, db *gorm.DB, loginText string) {
	username := c.PostForm("username")
	password := c.PostForm("password")
	var user models.User
	if err := db.Where("username = ?", username).First(&user).Error; err != nil {
		c.HTML(http.StatusBadRequest, "login.html", gin.H{
			"error": "Invalid credentials",
			"login_text": loginText,
		})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		c.HTML(http.StatusBadRequest, "login.html", gin.H{
			"error": "Invalid credentials",
			"login_text": loginText,
		})
		return
	}
	session := sessions.Default(c)
	session.Set("user_id", user.ID)
	session.Save()
	c.Redirect(http.StatusFound, "/")
}

func RequireLogin(c *gin.Context) bool {
	session := sessions.Default(c)
	if session.Get("user_id") == nil {
		c.Redirect(http.StatusFound, "/login")
		return false
	}
	return true
}
