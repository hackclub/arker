package handlers

import (
	"archive/tar"
	"io"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"github.com/gin-gonic/gin"

	"gorm.io/gorm"
	"arker/internal/models"
	"arker/internal/storage"
)

var cacheMutex = sync.Mutex{}

func GitHandler(c *gin.Context, storage storage.Storage, db *gorm.DB, cacheRoot string) {
	path := c.Param("path") // e.g., /hc139d/info/refs?service=git-upload-pack
	if path == "" {
		c.Status(http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(path[1:], "/", 2)
	if len(parts) < 1 {
		c.Status(http.StatusBadRequest)
		return
	}
	shortID := parts[0]
	var capture models.Capture
	if db.Where("short_id = ?", shortID).First(&capture).Error != nil {
		c.Status(http.StatusNotFound)
		return
	}
	var item models.ArchiveItem
	if db.Where("capture_id = ? AND type = ?", capture.ID, "git").First(&item).Error != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if item.Status != "completed" {
		c.Status(http.StatusNotFound)
		return
	}
	targetDir := filepath.Join(cacheRoot, shortID)
	cacheMutex.Lock()
	_, err := os.Stat(targetDir)
	if os.IsNotExist(err) {
		if err := unpackGit(item.StorageKey, targetDir, storage); err != nil {
			cacheMutex.Unlock()
			log.Printf("Unpack error: %v", err)
			c.Status(http.StatusInternalServerError)
			return
		}
	}
	cacheMutex.Unlock()
	
	env := append(os.Environ(),
		"GIT_PROJECT_ROOT="+cacheRoot,
		"GIT_HTTP_EXPORT_ALL=true",
		"PATH_INFO="+path,
		"QUERY_STRING="+c.Request.URL.RawQuery,
		"REQUEST_METHOD="+c.Request.Method,
		"CONTENT_TYPE="+c.GetHeader("Content-Type"),
	)
	h := &cgi.Handler{
		Path: "/usr/bin/git",
		Args: []string{"http-backend"},
		Env:  env,
	}
	h.ServeHTTP(c.Writer, c.Request)
}

func unpackGit(key string, targetDir string, storage storage.Storage) error {
	r, err := storage.Reader(key)
	if err != nil {
		return err
	}
	defer r.Close()
	tr := tar.NewReader(r)
	if err = os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		tpath := filepath.Join(targetDir, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(tpath, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(tpath), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(tpath, os.O_CREATE|os.O_RDWR, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err = io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}
