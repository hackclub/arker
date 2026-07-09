package handlers

import (
	"arker/internal/models"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"net/http"
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
			"error":      "Invalid credentials",
			"login_text": loginText,
		})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		c.HTML(http.StatusBadRequest, "login.html", gin.H{
			"error":      "Invalid credentials",
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

// RequireLoginMiddleware is the route-group form of RequireLogin. Attach it to a
// Gin group so every route under it requires an authenticated session; on failure
// RequireLogin issues the redirect and this aborts the chain.
func RequireLoginMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authenticated := RequireLogin(c)
		if !authenticated {
			c.Abort()
			return
		}
		c.Next()
	}
}
