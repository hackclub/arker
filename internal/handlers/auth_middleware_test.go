package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func TestRequireLoginMiddlewareBlocksAnonymous(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	store := cookie.NewStore([]byte("test-secret"))
	r.Use(sessions.Sessions("session", store))

	// A login route so the redirect target exists.
	r.GET("/login", func(c *gin.Context) { c.String(http.StatusOK, "login") })
	r.GET("/set-session", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("user_id", uint(1))
		if err := session.Save(); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.String(http.StatusOK, "set")
	})

	grp := r.Group("/admin", RequireLoginMiddleware())
	grp.GET("/secret", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// Unauthenticated: expect a redirect to /login (302), not 200.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/secret", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("anonymous request: got %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Fatalf("redirect location = %q, want /login", loc)
	}

	// Authenticated: the grouped handler should run.
	loginW := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodGet, "/set-session", nil)
	r.ServeHTTP(loginW, loginReq)
	if loginW.Code != http.StatusOK {
		t.Fatalf("set session: got %d, want 200", loginW.Code)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/secret", nil)
	for _, cookie := range loginW.Result().Cookies() {
		req.AddCookie(cookie)
	}
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated request: got %d, want 200", w.Code)
	}
	if body := w.Body.String(); body != "ok" {
		t.Fatalf("authenticated response body = %q, want ok", body)
	}
}
