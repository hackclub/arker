package main

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"image/png"
	"io"
	"log"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/http/cgi"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/go-git/go-git/v5"
	"github.com/HugoSmits86/nativewebp"
	"github.com/playwright-community/playwright-go"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/html"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Models
type User struct {
	gorm.Model
	Username     string `gorm:"unique"`
	PasswordHash string
}

type ArchivedURL struct {
	gorm.Model
	Original string `gorm:"unique"`
	Captures []Capture
}

type Capture struct {
	gorm.Model
	ArchivedURLID uint
	Timestamp     time.Time
	ShortID       string `gorm:"unique"`
	ArchiveItems  []ArchiveItem `gorm:"foreignKey:CaptureID"`
}

type ArchiveItem struct {
	gorm.Model
	CaptureID  uint
	Type       string // mhtml, screenshot, git, youtube
	Status     string // pending, processing, completed, failed
	StorageKey string
	Extension  string // .webp, .mhtml, .tar.zst, .mp4, etc.
	Logs       string `gorm:"type:text"`
	RetryCount int
}

// Job for queue
type Job struct {
	CaptureID uint
	ShortID   string
	Type      string
	URL       string
}

// Storage interface (modular for future S3)
type Storage interface {
	Writer(key string) (io.WriteCloser, error)
	Reader(key string) (io.ReadCloser, error)
	Exists(key string) (bool, error)
}

// FSStorage impl
type FSStorage struct {
	baseDir string
}

func NewFSStorage(baseDir string) *FSStorage {
	return &FSStorage{baseDir: baseDir}
}

func (s *FSStorage) Writer(key string) (io.WriteCloser, error) {
	path := filepath.Join(s.baseDir, key)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	return os.Create(path)
}

func (s *FSStorage) Reader(key string) (io.ReadCloser, error) {
	path := filepath.Join(s.baseDir, key)
	return os.Open(path)
}

func (s *FSStorage) Exists(key string) (bool, error) {
	path := filepath.Join(s.baseDir, key)
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// DBLogWriter writes logs to database in real-time
type DBLogWriter struct {
	db     *gorm.DB
	itemID uint
	buffer strings.Builder
	mutex  sync.Mutex
}

func NewDBLogWriter(db *gorm.DB, itemID uint) *DBLogWriter {
	return &DBLogWriter{
		db:     db,
		itemID: itemID,
	}
}

func (w *DBLogWriter) Write(p []byte) (n int, err error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	
	// Write to buffer
	n, err = w.buffer.Write(p)
	if err != nil {
		return n, err
	}
	
	// Update database with current log content
	w.db.Model(&ArchiveItem{}).Where("id = ?", w.itemID).Update("logs", w.buffer.String())
	
	return n, nil
}

func (w *DBLogWriter) String() string {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.buffer.String()
}

// Archiver interface
type Archiver interface {
	Archive(url string, logWriter io.Writer, db *gorm.DB, itemID uint) (data io.Reader, extension string, contentType string, cleanup func(), err error)
}

// MHTMLArchiver
type MHTMLArchiver struct {
	browser playwright.Browser
}

func (a *MHTMLArchiver) Archive(url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, func(), error) {
	fmt.Fprintf(logWriter, "Starting MHTML archive for: %s\n", url)
	
	page, err := a.browser.NewPage()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create browser page: %v\n", err)
		return nil, "", "", nil, err
	}
	cleanup := func() { page.Close() }

	// Log console messages and errors
	page.On("console", func(msg playwright.ConsoleMessage) {
		fmt.Fprintf(logWriter, "Console [%s]: %s\n", msg.Type(), msg.Text())
	})
	page.On("pageerror", func(err error) {
		fmt.Fprintf(logWriter, "Page error: %v\n", err)
	})

	fmt.Fprintf(logWriter, "Navigating to URL...\n")
	if _, err = page.Goto(url, playwright.PageGotoOptions{
		Timeout: playwright.Float(30000),
	}); err != nil {
		fmt.Fprintf(logWriter, "Failed to navigate to URL: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}

	// Wait for document.readyState === 'complete'
	fmt.Fprintf(logWriter, "Waiting for page to load completely...\n")
	if _, err = page.WaitForFunction("document.readyState === 'complete'", playwright.PageWaitForFunctionOptions{
		Timeout: playwright.Float(30000),
	}); err != nil {
		fmt.Fprintf(logWriter, "Page failed to load completely: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}
	fmt.Fprintf(logWriter, "Page loaded successfully\n")

	fmt.Fprintf(logWriter, "Creating CDP session for MHTML capture...\n")
	session, err := page.Context().NewCDPSession(page)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create CDP session: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}

	fmt.Fprintf(logWriter, "Capturing MHTML snapshot...\n")
	result, err := session.Send("Page.captureSnapshot", map[string]interface{}{"format": "mhtml"})
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to capture MHTML snapshot: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}

	dataStr := result.(map[string]interface{})["data"].(string)
	fmt.Fprintf(logWriter, "MHTML archive completed successfully, size: %d bytes\n", len(dataStr))
	return strings.NewReader(dataStr), ".mhtml", "application/x-mhtml", cleanup, nil
}

// ScreenshotArchiver
type ScreenshotArchiver struct {
	browser playwright.Browser
}

func (a *ScreenshotArchiver) Archive(url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, func(), error) {
	fmt.Fprintf(logWriter, "Starting screenshot archive for: %s\n", url)
	
	page, err := a.browser.NewPage(playwright.BrowserNewPageOptions{
		Viewport: &playwright.Size{
			Width:  1500,
			Height: 1080,
		},
		DeviceScaleFactor: playwright.Float(2.0), // Retina quality
	})
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create browser page: %v\n", err)
		return nil, "", "", nil, err
	}
	cleanup := func() { page.Close() }

	// Log console messages and errors
	page.On("console", func(msg playwright.ConsoleMessage) {
		fmt.Fprintf(logWriter, "Console [%s]: %s\n", msg.Type(), msg.Text())
	})
	page.On("pageerror", func(err error) {
		fmt.Fprintf(logWriter, "Page error: %v\n", err)
	})

	fmt.Fprintf(logWriter, "Navigating to URL...\n")
	if _, err = page.Goto(url, playwright.PageGotoOptions{
		Timeout: playwright.Float(30000),
	}); err != nil {
		fmt.Fprintf(logWriter, "Failed to navigate to URL: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}

	// Wait for document.readyState === 'complete'
	fmt.Fprintf(logWriter, "Waiting for page to load completely...\n")
	if _, err = page.WaitForFunction("document.readyState === 'complete'", playwright.PageWaitForFunctionOptions{
		Timeout: playwright.Float(30000),
	}); err != nil {
		fmt.Fprintf(logWriter, "Page failed to load completely: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}
	fmt.Fprintf(logWriter, "Page loaded successfully\n")

	fmt.Fprintf(logWriter, "Taking full-page screenshot...\n")
	data, err := page.Screenshot(playwright.PageScreenshotOptions{
		FullPage: playwright.Bool(true),
		Type:     (*playwright.ScreenshotType)(playwright.String("png")),
	})
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to take screenshot: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}

	// Convert PNG to WebP
	fmt.Fprintf(logWriter, "Screenshot captured, size: %d bytes. Converting to WebP...\n", len(data))
	
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to decode PNG: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}
	
	fmt.Fprintf(logWriter, "Image decoded, bounds: %v\n", img.Bounds())

	var webpBuf bytes.Buffer
	err = nativewebp.Encode(&webpBuf, img, nil)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to encode WebP: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}

	fmt.Fprintf(logWriter, "Screenshot archive completed successfully, WebP size: %d bytes\n", webpBuf.Len())
	
	return bytes.NewReader(webpBuf.Bytes()), ".webp", "image/webp", cleanup, nil
}

// GitArchiver
type GitArchiver struct{}

func (a *GitArchiver) Archive(url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, func(), error) {
	fmt.Fprintf(logWriter, "Starting git archive for: %s\n", url)
	
	tempDir, err := os.MkdirTemp("", "git-archive-")
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create temp directory: %v\n", err)
		return nil, "", "", nil, err
	}
	cleanup := func() { os.RemoveAll(tempDir) }

	fmt.Fprintf(logWriter, "Cloning repository to: %s\n", tempDir)
	_, err = git.PlainClone(tempDir, true, &git.CloneOptions{
		URL:      url,
		Progress: logWriter,
	})
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to clone repository: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}
	fmt.Fprintf(logWriter, "Repository cloned successfully\n")

	pr, pw := io.Pipe()

	go func() {
		defer pw.Close()
		tw := tar.NewWriter(pw)
		defer tw.Close()
		
		fmt.Fprintf(logWriter, "Creating tar archive...\n")
		if err := addDirToTar(tw, tempDir, ""); err != nil {
			fmt.Fprintf(logWriter, "Failed to create tar archive: %v\n", err)
			pw.CloseWithError(err)
			return
		}
		fmt.Fprintf(logWriter, "Git archive completed successfully\n")
	}()

	return pr, ".tar", "application/x-tar", cleanup, nil
}

// YTArchiver (streams directly from yt-dlp stdout)
type YTArchiver struct{}

// PartData represents a multipart section
type PartData struct {
	Header map[string][]string
	Data   []byte
}

func (a *YTArchiver) Archive(url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, func(), error) {
	fmt.Fprintf(logWriter, "Starting YouTube archive for: %s\n", url)
	
	// First, test if yt-dlp can access the video
	fmt.Fprintf(logWriter, "Testing video accessibility with yt-dlp...\n")
	testCmd := exec.Command("yt-dlp", "--print", "title,duration,uploader", url)
	testOutput, err := testCmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(logWriter, "yt-dlp test failed: %v\nOutput: %s\n", err, string(testOutput))
		return nil, "", "", nil, fmt.Errorf("yt-dlp cannot access video: %v", err)
	}
	fmt.Fprintf(logWriter, "Video info:\n%s\n", string(testOutput))
	
	cmd := exec.Command("yt-dlp", "-f", "bestvideo+bestaudio/best", "--no-playlist", "--no-write-thumbnail", "--verbose", "-o", "-", url)
	pr, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create stdout pipe: %v\n", err)
		return nil, "", "", nil, err
	}
	
	// Create a pipe for stderr so we can capture and forward logs
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create stderr pipe: %v\n", err)
		return nil, "", "", nil, err
	}
	
	// Start capturing stderr in a goroutine
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				logWriter.Write(buf[:n])
				// Also write to stdout for immediate debugging
				os.Stdout.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}()
	
	fmt.Fprintf(logWriter, "Starting yt-dlp download process...\n")
	if err = cmd.Start(); err != nil {
		fmt.Fprintf(logWriter, "Failed to start yt-dlp: %v\n", err)
		return nil, "", "", nil, err
	}
	cleanup := func() { 
		cmd.Process.Kill()
		cmd.Wait() 
	}
	fmt.Fprintf(logWriter, "YouTube download started successfully\n")
	return pr, ".mp4", "video/mp4", cleanup, nil
}

// MHTMLConverter handles MHTML to HTML conversion  
type MHTMLConverter struct{}

// Convert converts MHTML content to HTML
func (c *MHTMLConverter) Convert(input io.Reader, output io.Writer) error {
	// Read the input into memory first
	data, err := io.ReadAll(input)
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}

	// Parse as mail message
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to read mail message: %w", err)
	}

	// Get content type and boundary
	contentType := msg.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("failed to parse media type: %w", err)
	}

	if !strings.HasPrefix(mediaType, "multipart/related") {
		return fmt.Errorf("not a multipart/related message, got: %s", mediaType)
	}

	boundary := params["boundary"]
	if boundary == "" {
		return fmt.Errorf("no boundary found in content type")
	}

	// Parse multipart content
	mr := multipart.NewReader(msg.Body, boundary)
	
	parts := make(map[string]*PartData)
	var htmlPart *PartData

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read multipart: %w", err)
		}

		partContentType := part.Header.Get("Content-Type")
		contentID := part.Header.Get("Content-ID")
		contentLocation := part.Header.Get("Content-Location")

		// Read part data
		partData, err := io.ReadAll(part)
		if err != nil {
			return fmt.Errorf("failed to read part data: %w", err)
		}

		pd := &PartData{
			Header: map[string][]string(part.Header),
			Data:   partData,
		}

		// Store by Content-ID
		if contentID != "" {
			cid := strings.Trim(contentID, "<>")
			parts[cid] = pd
		}

		// Store by Content-Location
		if contentLocation != "" {
			parts[contentLocation] = pd
			
			// Also store without "cid:" prefix for easier lookup
			if strings.HasPrefix(contentLocation, "cid:") {
				cidKey := strings.TrimPrefix(contentLocation, "cid:")
				parts[cidKey] = pd
			}
		}

		// Check if this is the HTML part
		if htmlPart == nil && strings.HasPrefix(partContentType, "text/html") {
			htmlPart = pd
		}
	}

	if htmlPart == nil {
		return fmt.Errorf("no HTML part found")
	}



	// Decode the HTML content
	htmlContent, err := c.decodePart(htmlPart)
	if err != nil {
		return fmt.Errorf("failed to decode HTML part: %w", err)
	}

	// Parse and modify HTML to embed resources
	doc, err := html.Parse(bytes.NewReader(htmlContent))
	if err != nil {
		return fmt.Errorf("failed to parse HTML: %w", err)
	}

	// Walk the HTML tree and replace cid: references
	c.walkHTML(doc, parts)

	// Render the modified HTML
	return html.Render(output, doc)
}

// decodePart decodes a multipart based on its transfer encoding
func (c *MHTMLConverter) decodePart(partData *PartData) ([]byte, error) {
	transferEncoding := c.getHeader(partData.Header, "Content-Transfer-Encoding")
	
	switch strings.ToLower(transferEncoding) {
	case "quoted-printable":
		reader := quotedprintable.NewReader(bytes.NewReader(partData.Data))
		return io.ReadAll(reader)
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(partData.Data), "\n", ""))
		return decoded, err
	default:
		return partData.Data, nil
	}
}

// getHeader gets a header value from the header map
func (c *MHTMLConverter) getHeader(header map[string][]string, key string) string {
	if values, ok := header[key]; ok && len(values) > 0 {
		return values[0]
	}
	return ""
}

// walkHTML walks the HTML tree and replaces cid: references with data URLs
func (c *MHTMLConverter) walkHTML(n *html.Node, parts map[string]*PartData) {
	if n.Type == html.ElementNode {
		for i := range n.Attr {
			attr := &n.Attr[i]
			if (attr.Key == "href" || attr.Key == "src") && strings.HasPrefix(attr.Val, "cid:") {
				cid := strings.TrimPrefix(attr.Val, "cid:")
				if partData, ok := parts[cid]; ok {
					data, err := c.decodePart(partData)
					if err == nil {
						contentType := c.getHeader(partData.Header, "Content-Type")
						if contentType == "" {
							contentType = "application/octet-stream"
						}
						dataURL := fmt.Sprintf("data:%s;base64,%s", contentType, base64.StdEncoding.EncodeToString(data))
						attr.Val = dataURL
					}
				}
			}
		}
	}

	// Recursively walk children
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		c.walkHTML(child, parts)
	}
}

// Helper to tar dir streaming
func addDirToTar(tw *tar.Writer, dir string, prefix string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	
	fis, err := f.Readdir(-1)
	if err != nil {
		return err
	}
	
	for _, fi := range fis {
		curPath := filepath.Join(dir, fi.Name())
		if fi.IsDir() {
			if err = tw.WriteHeader(&tar.Header{
				Name:     prefix + fi.Name() + "/",
				Size:     0,
				Mode:     int64(fi.Mode()),
				ModTime:  fi.ModTime(),
				Typeflag: tar.TypeDir,
			}); err != nil {
				return err
			}
			if err = addDirToTar(tw, curPath, prefix+fi.Name()+"/"); err != nil {
				return err
			}
		} else {
			if err = tw.WriteHeader(&tar.Header{
				Name:     prefix + fi.Name(),
				Size:     fi.Size(),
				Mode:     int64(fi.Mode()),
				ModTime:  fi.ModTime(),
				Typeflag: tar.TypeReg,
			}); err != nil {
				return err
			}
			ff, err := os.Open(curPath)
			if err != nil {
				return err
			}
			if _, err = io.Copy(tw, ff); err != nil {
				ff.Close()
				return err
			}
			ff.Close()
		}
	}
	return nil
}

// Queue channel
var jobChan = make(chan Job, 100)

// Worker
func worker(id int, jobChan <-chan Job, storage Storage, db *gorm.DB, archivers map[string]Archiver) {
	for job := range jobChan {
		err := processJob(job, storage, db, archivers)
		if err != nil {
			log.Printf("Worker %d failed job %s %s: %v", id, job.ShortID, job.Type, err)
			db.Model(&ArchiveItem{}).Where("capture_id = ? AND type = ?", job.CaptureID, job.Type).Update("status", "failed")
		} else {
			log.Printf("Worker %d completed %s %s", id, job.ShortID, job.Type)
		}
	}
}

// Process job (streams to zstd/FS)
func processJob(job Job, storage Storage, db *gorm.DB, archivers map[string]Archiver) error {
	var item ArchiveItem
	if err := db.Where("capture_id = ? AND type = ?", job.CaptureID, job.Type).First(&item).Error; err != nil {
		return err
	}
	
	// Check retry limit
	if item.RetryCount >= 3 {
		db.Model(&item).Update("status", "failed")
		return fmt.Errorf("max retries exceeded for %s %s", job.ShortID, job.Type)
	}
	
	// Update status to processing and increment retry count
	db.Model(&item).Updates(map[string]interface{}{
		"status":      "processing",
		"retry_count": gorm.Expr("retry_count + 1"),
	})
	
	arch, ok := archivers[job.Type]
	if !ok {
		return fmt.Errorf("unknown archiver %s", job.Type)
	}
	
	dbLogWriter := NewDBLogWriter(db, item.ID)
	log.Printf("Starting archive job: %s %s", job.ShortID, job.Type)
	data, ext, _, cleanup, err := arch.Archive(job.URL, dbLogWriter, db, item.ID)
	log.Printf("Archive job returned: %s %s, error: %v", job.ShortID, job.Type, err)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		// Store final logs and mark as failed
		db.Model(&item).Updates(map[string]interface{}{
			"status": "failed",
			"logs":   dbLogWriter.String(),
		})
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	
	key := fmt.Sprintf("%s/%s%s.zst", job.ShortID, job.Type, ext)
	w, err := storage.Writer(key)
	if err != nil {
		return err
	}
	defer w.Close()
	
	zw, err := zstd.NewWriter(w)
	if err != nil {
		return err
	}
	defer zw.Close()
	
	if _, err = io.Copy(zw, data); err != nil {
		return err
	}
	
	// Mark as completed and store final logs
	db.Model(&item).Updates(map[string]interface{}{
		"status":      "completed",
		"storage_key": key,
		"extension":   ext,
		"logs":        dbLogWriter.String(),
	})
	return nil
}

// Simple MHTML to HTML extraction (bypasses full parsing)


func decodePartData(partData *PartData) ([]byte, error) {
	te := ""
	if values, ok := partData.Header["Content-Transfer-Encoding"]; ok && len(values) > 0 {
		te = values[0]
	}
	switch strings.ToLower(te) {
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(bytes.NewReader(partData.Data)))
	case "base64":
		return base64.StdEncoding.DecodeString(strings.ReplaceAll(string(partData.Data), "\n", ""))
	default:
		return partData.Data, nil
	}
}

// Get archive types based on URL patterns
func getArchiveTypes(url string) []string {
	types := []string{"mhtml", "screenshot"}
	lowerURL := strings.ToLower(url)
	
	// Add YouTube archiver for YouTube URLs
	if strings.Contains(lowerURL, "youtube.com") || strings.Contains(lowerURL, "youtu.be") {
		types = append(types, "youtube")
	}
	
	// Add Git archiver for Git repository URLs
	if strings.HasSuffix(lowerURL, ".git") ||
		strings.Contains(lowerURL, "github.com/") ||
		strings.Contains(lowerURL, "gitlab.com/") ||
		strings.Contains(lowerURL, "bitbucket.org/") ||
		strings.Contains(lowerURL, "codeberg.org/") ||
		strings.Contains(lowerURL, "git.") {
		types = append(types, "git")
	}
	
	return types
}

// Generate short ID
func generateShortID(db *gorm.DB) string {
	alphabet := []rune("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	for {
		var sb strings.Builder
		for i := 0; i < 8; i++ {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			sb.WriteRune(alphabet[n.Int64()])
		}
		id := sb.String()
		var count int64
		db.Model(&Capture{}).Where("short_id = ?", id).Count(&count)
		if count == 0 {
			return id
		}
	}
}

// Handlers...

func loginGet(c *gin.Context) {
	c.HTML(http.StatusOK, "login.html", nil)
}

func loginPost(c *gin.Context, db *gorm.DB) {
	username := c.PostForm("username")
	password := c.PostForm("password")
	var user User
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

func requireLogin(c *gin.Context) bool {
	session := sessions.Default(c)
	if session.Get("user_id") == nil {
		c.Redirect(http.StatusFound, "/login")
		return false
	}
	return true
}

func adminGet(c *gin.Context, db *gorm.DB) {
	if !requireLogin(c) {
		return
	}
	var urls []ArchivedURL
	db.Preload("Captures.ArchiveItems").Preload("Captures", func(db *gorm.DB) *gorm.DB {
		return db.Order("created_at DESC")
	}).Order("updated_at DESC").Find(&urls)
	c.HTML(http.StatusOK, "admin.html", gin.H{"urls": urls})
}

func requestCapture(c *gin.Context, db *gorm.DB) {
	if !requireLogin(c) {
		return
	}
	id := c.Param("id")
	var u ArchivedURL
	if db.First(&u, id).Error != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid URL ID"})
		return
	}
	types := getArchiveTypes(u.Original)
	shortID := generateShortID(db)
	capture := Capture{ArchivedURLID: u.ID, Timestamp: time.Now(), ShortID: shortID}
	db.Create(&capture)
	for _, t := range types {
		item := ArchiveItem{CaptureID: capture.ID, Type: t, Status: "pending"}
		db.Create(&item)
		jobChan <- Job{CaptureID: capture.ID, ShortID: shortID, Type: t, URL: u.Original}
	}
	c.JSON(http.StatusOK, gin.H{"short_id": shortID})
}

func apiArchive(c *gin.Context, db *gorm.DB) {
	var req struct {
		URL   string   `json:"url"`
		Types []string `json:"types"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	if len(req.Types) == 0 {
		req.Types = getArchiveTypes(req.URL)
	}
	var u ArchivedURL
	err := db.Where("original = ?", req.URL).First(&u).Error
	if err == gorm.ErrRecordNotFound {
		u = ArchivedURL{Original: req.URL}
		if err = db.Create(&u).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "DB error"})
			return
		}
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "DB error"})
		return
	}
	shortID := generateShortID(db)
	capture := Capture{ArchivedURLID: u.ID, Timestamp: time.Now(), ShortID: shortID}
	db.Create(&capture)
	for _, t := range req.Types {
		item := ArchiveItem{CaptureID: capture.ID, Type: t, Status: "pending"}
		db.Create(&item)
		jobChan <- Job{CaptureID: capture.ID, ShortID: shortID, Type: t, URL: req.URL}
	}
	c.JSON(http.StatusOK, gin.H{"short_id": shortID})
}

func displayGet(c *gin.Context, db *gorm.DB) {
	shortID := c.Param("shortid")
	var capture Capture
	if err := db.Where("short_id = ?", shortID).Preload("ArchiveItems").First(&capture).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.HTML(http.StatusOK, "display.html", gin.H{
		"domain":   "arker.hackclub.com", // Or c.Request.Host
		"date":     capture.Timestamp.Format(time.RFC1123),
		"archives": capture.ArchiveItems,
		"short_id": shortID,
		"host":     c.Request.Host,
	})
}

// generateArchiveFilename creates a descriptive filename for archive downloads
func generateArchiveFilename(capture Capture, archivedURL ArchivedURL, extension string) string {
	// Format: YYYY-MM-DD_downcased_url.extension
	date := capture.CreatedAt.Format("2006-01-02")
	
	// Clean and downcase the URL
	url := strings.ToLower(archivedURL.Original)
	// Remove protocol
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	// Remove www
	url = strings.TrimPrefix(url, "www.")
	// Replace problematic characters with underscores
	url = strings.NewReplacer(
		"/", "_",
		"?", "_",
		"&", "_",
		"=", "_",
		"#", "_",
		":", "_",
		";", "_",
		" ", "_",
		"+", "_",
		"%", "_",
		".", "_",
	).Replace(url)
	// Remove trailing underscores and slashes
	url = strings.Trim(url, "_/")
	// Limit length to avoid filesystem issues
	if len(url) > 50 {
		url = url[:50]
	}
	
	// Remove leading dot from extension if present
	extension = strings.TrimPrefix(extension, ".")
	
	return fmt.Sprintf("%s_%s.%s", date, url, extension)
}

func serveArchive(c *gin.Context, storage Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	typ := c.Param("type")
	var item ArchiveItem
	var capture Capture
	var archivedURL ArchivedURL
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, typ).
		First(&item).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if err := db.Where("short_id = ?", shortID).First(&capture).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if err := db.Where("id = ?", capture.ArchivedURLID).First(&archivedURL).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if item.Status != "completed" {
		c.Status(http.StatusNotFound)
		return
	}
	r, err := storage.Reader(item.StorageKey)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer r.Close()
	zr, err := zstd.NewReader(r)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer zr.Close()
	var ct string
	attach := false
	switch typ {
	case "mhtml":
		ct = "multipart/related" // Original MHTML content type for downloads
		attach = true
	case "screenshot":
		ct = "image/webp"
	case "youtube":
		ct = "video/mp4"
	case "git":
		ct = "application/zstd"
		attach = true
	default:
		ct = "application/octet-stream"
		attach = true
	}
	c.Header("Content-Type", ct)
	if attach {
		filename := generateArchiveFilename(capture, archivedURL, item.Extension)
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	}
	io.Copy(c.Writer, zr)
}

func serveMHTMLAsHTML(c *gin.Context, storage Storage, db *gorm.DB) {
	shortID := c.Param("shortid")
	var item ArchiveItem
	if err := db.Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("captures.short_id = ? AND archive_items.type = ?", shortID, "mhtml").
		First(&item).Error; err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if item.Status != "completed" {
		c.Status(http.StatusNotFound)
		return
	}
	
	r, err := storage.Reader(item.StorageKey)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer r.Close()
	
	zr, err := zstd.NewReader(r)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer zr.Close()
	
	c.Header("Content-Type", "text/html")
	
	log.Printf("Converting MHTML to HTML for %s", shortID)
	
	// Use the working MHTML converter
	converter := &MHTMLConverter{}
	if err := converter.Convert(zr, c.Writer); err != nil {
		log.Printf("MHTML conversion error: %v", err)
		c.String(http.StatusInternalServerError, "MHTML conversion failed: %v", err)
		return
	}
}

var cacheMutex = sync.Mutex{}

func gitHandler(c *gin.Context, storage Storage, db *gorm.DB, cacheRoot string) {
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
	var capture Capture
	if db.Where("short_id = ?", shortID).First(&capture).Error != nil {
		c.Status(http.StatusNotFound)
		return
	}
	var item ArchiveItem
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

func unpackGit(key string, targetDir string, storage Storage) error {
	r, err := storage.Reader(key)
	if err != nil {
		return err
	}
	defer r.Close()
	zr, err := zstd.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
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

func main() {
	dsn := os.Getenv("DB_URL")
	if dsn == "" {
		dsn = "host=localhost user=user password=pass dbname=arker port=5432 sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	db.AutoMigrate(&User{}, &ArchivedURL{}, &Capture{}, &ArchiveItem{})

	// Default user
	var user User
	if db.First(&user).Error == gorm.ErrRecordNotFound {
		hashed, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
		if err != nil {
			log.Fatal(err)
		}
		user = User{Username: "admin", PasswordHash: string(hashed)}
		db.Create(&user)
		log.Println("Created default admin user: admin/admin")
	}

	pw, err := playwright.Run()
	if err != nil {
		log.Fatal("Failed to start Playwright:", err)
	}
	defer pw.Stop()
	
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
		Args: []string{
			"--no-sandbox",
			"--disable-setuid-sandbox",
			"--disable-dev-shm-usage",
			"--disable-extensions",
			"--disable-plugins",
			"--disable-images",
			"--disable-background-timer-throttling",
			"--disable-backgrounding-occluded-windows",
			"--disable-renderer-backgrounding",
		},
	})
	if err != nil {
		log.Fatal("Failed to launch Chromium:", err)
	}
	defer browser.Close()

	archivers := map[string]Archiver{
		"mhtml":      &MHTMLArchiver{browser},
		"screenshot": &ScreenshotArchiver{browser},
		"git":        &GitArchiver{},
		"youtube":    &YTArchiver{},
	}

	storagePath := os.Getenv("STORAGE_PATH")
	if storagePath == "" {
		storagePath = "./storage"
	}
	storage := NewFSStorage(storagePath)

	cachePath := os.Getenv("CACHE_PATH")
	if cachePath == "" {
		cachePath = "./cache"
	}
	os.MkdirAll(cachePath, 0755)

	maxWorkers, _ := strconv.Atoi(os.Getenv("MAX_WORKERS"))
	if maxWorkers <= 0 {
		maxWorkers = 5
	}
	// Resume pending archives on startup
	var pendingItems []ArchiveItem
	db.Where("status IN (?, ?) AND retry_count < ?", "pending", "processing", 3).Find(&pendingItems)
	for _, item := range pendingItems {
		var capture Capture
		db.First(&capture, item.CaptureID)
		var au ArchivedURL
		db.First(&au, capture.ArchivedURLID)
		jobChan <- Job{CaptureID: capture.ID, ShortID: capture.ShortID, Type: item.Type, URL: au.Original}
		log.Printf("Resuming pending job: %s %s", capture.ShortID, item.Type)
	}

	for i := 1; i <= maxWorkers; i++ {
		go worker(i, jobChan, storage, db, archivers)
	}

	// Start log cleanup routine
	go func() {
		for {
			time.Sleep(24 * time.Hour)
			result := db.Model(&ArchiveItem{}).Where("status = ? AND updated_at < ?", "completed", time.Now().Add(-30*24*time.Hour)).Update("logs", "")
			if result.RowsAffected > 0 {
				log.Printf("Cleaned up logs for %d completed archives older than 30 days", result.RowsAffected)
			}
		}
	}()

	r := gin.Default()
	r.LoadHTMLGlob("templates/*.html")
	store := cookie.NewStore([]byte("secret-key-change-in-production"))
	r.Use(sessions.Sessions("session", store))

	r.GET("/login", loginGet)
	r.POST("/login", func(c *gin.Context) { loginPost(c, db) })
	r.GET("/admin", func(c *gin.Context) { adminGet(c, db) })
	r.POST("/admin/url/:id/capture", func(c *gin.Context) { requestCapture(c, db) })
	r.GET("/admin/item/:id/log", func(c *gin.Context) {
		if !requireLogin(c) { return }
		id := c.Param("id")
		var item ArchiveItem
		if db.First(&item, id).Error != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"logs": item.Logs})
	})
	r.POST("/api/v1/archive", func(c *gin.Context) { apiArchive(c, db) })
	r.GET("/:shortid", func(c *gin.Context) { displayGet(c, db) })
	r.GET("/archive/:shortid/:type", func(c *gin.Context) { serveArchive(c, storage, db) })
	r.GET("/archive/:shortid/mhtml/html", func(c *gin.Context) { serveMHTMLAsHTML(c, storage, db) })
	r.Any("/git/*path", func(c *gin.Context) { gitHandler(c, storage, db, cachePath) })

	log.Println("Starting server on :8080")
	r.Run(":8080")
}
