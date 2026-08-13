package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

type archiveResultResponse struct {
	SchemaVersion    string              `json:"schema_version"`
	ShortID          string              `json:"short_id"`
	CanonicalShortID string              `json:"canonical_short_id"`
	SourceURL        string              `json:"source_url"`
	ArchiveURL       string              `json:"archive_url"`
	SubmittedAt      string              `json:"submitted_at"`
	CaptureDone      bool                `json:"capture_done"`
	Items            []archiveResultItem `json:"items"`
	Cost             archiveResultCost   `json:"cost"`
	SocialPost       *socialPostResult   `json:"social_post"`
}

type archiveResultCost struct {
	Currency  string                       `json:"currency"`
	TotalUSD  float64                      `json:"total_usd"`
	Estimated bool                         `json:"estimated"`
	Breakdown []archiveResultCostBreakdown `json:"breakdown"`
	Note      string                       `json:"note"`
}

type archiveResultCostBreakdown struct {
	Provider         string  `json:"provider"`
	Product          string  `json:"product,omitempty"`
	Operations       int64   `json:"operations"`
	Successes        int64   `json:"successes"`
	Records          int64   `json:"records,omitempty"`
	BytesTransferred int64   `json:"bytes_transferred,omitempty"`
	CostUSD          float64 `json:"cost_usd"`
	Estimated        bool    `json:"estimated"`
}

type archiveResultItem struct {
	Type      string `json:"type"`
	Status    string `json:"status"`
	URL       string `json:"url,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

type socialPostResult struct {
	Status      string            `json:"status"`
	Terminal    bool              `json:"terminal"`
	Fulfilled   bool              `json:"fulfilled"`
	Platform    string            `json:"platform,omitempty"`
	Post        *normalizedPost   `json:"post"`
	Media       []normalizedMedia `json:"media"`
	BundleURL   *string           `json:"bundle_url"`
	RawMetadata []rawMetadataLink `json:"raw_metadata"`
	Provenance  socialProvenance  `json:"provenance"`
	Warnings    []socialWarning   `json:"warnings"`
	Failure     *socialFailure    `json:"failure"`
}

type normalizedPost struct {
	ID              string            `json:"id,omitempty"`
	URL             string            `json:"url,omitempty"`
	Title           string            `json:"title,omitempty"`
	Text            string            `json:"text,omitempty"`
	PublishedAt     string            `json:"published_at,omitempty"`
	DurationSeconds *float64          `json:"duration_seconds,omitempty"`
	Author          *normalizedAuthor `json:"author,omitempty"`
	Engagement      *socialEngagement `json:"engagement,omitempty"`
	Tags            []string          `json:"tags,omitempty"`
}

type normalizedAuthor struct {
	ID          string `json:"id,omitempty"`
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type socialEngagement struct {
	Views    *int64 `json:"views,omitempty"`
	Likes    *int64 `json:"likes,omitempty"`
	Comments *int64 `json:"comments,omitempty"`
	Reposts  *int64 `json:"reposts,omitempty"`
}

type normalizedMedia struct {
	Index           int      `json:"index"`
	Type            string   `json:"type"`
	URL             string   `json:"url"`
	Filename        string   `json:"filename,omitempty"`
	ContentType     string   `json:"content_type,omitempty"`
	SizeBytes       int64    `json:"size_bytes,omitempty"`
	Width           *int64   `json:"width,omitempty"`
	Height          *int64   `json:"height,omitempty"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
	Quality         string   `json:"quality,omitempty"`
}

type rawMetadataLink struct {
	Provider string `json:"provider"`
	URL      string `json:"url"`
}

type socialProvenance struct {
	Source string `json:"source"`
	Mode   string `json:"mode"`
}
type socialWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type socialFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// ApiArchiveResult returns one provider-neutral representation of a capture.
// Aliases are resolved without redirecting so callers retain both identifiers.
func ApiArchiveResult(c *gin.Context, store storage.Storage, db *gorm.DB) {
	requested := c.Param("shortid")
	var requestedCapture models.Capture
	if err := db.Preload("ArchivedURL").Where("short_id = ?", requested).First(&requestedCapture).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "archive not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}
	canonical := requestedCapture
	if requestedCapture.AliasOfID != nil {
		canonical = models.Capture{}
		if err := db.Preload("ArchivedURL").Preload("ArchiveItems").First(&canonical, *requestedCapture.AliasOfID).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
			return
		}
	} else if err := db.Preload("ArchiveItems").First(&canonical, requestedCapture.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	items := make([]archiveResultItem, 0, len(canonical.ArchiveItems))
	done := true
	for _, item := range canonical.ArchiveItems {
		typ := utils.NormalizeArchiveType(item.Type)
		out := archiveResultItem{Type: typ, Status: item.Status}
		if item.Status == "completed" && item.StorageKey != "" {
			out.URL = fullPath(c, fmt.Sprintf("archive/%s/%s", canonical.ShortID, typ))
			out.SizeBytes = item.FileSize
		}
		if item.Status != "completed" && item.Status != "failed" {
			done = false
		}
		items = append(items, out)
	}

	response := archiveResultResponse{
		SchemaVersion: "1", ShortID: requested, CanonicalShortID: canonical.ShortID,
		SourceURL:  requestedCapture.ArchivedURL.Original,
		ArchiveURL: fullPath(c, canonical.ShortID), SubmittedAt: requestedCapture.Timestamp.UTC().Format("2006-01-02T15:04:05Z07:00"),
		CaptureDone: done, Items: items,
	}
	cost, err := buildArchiveResultCost(db, canonical.ArchiveItems)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}
	response.Cost = cost
	response.SocialPost = buildSocialPost(c, store, &canonical, response.SourceURL)
	c.JSON(http.StatusOK, response)
}

func buildArchiveResultCost(db *gorm.DB, items []models.ArchiveItem) (archiveResultCost, error) {
	cost := archiveResultCost{
		Currency:  "USD",
		Breakdown: []archiveResultCostBreakdown{{Provider: "native", Operations: int64(len(items)), CostUSD: 0, Estimated: false}},
		Note:      "Native archive operations are free. Bright Data costs are estimates computed from configured rates; the Bright Data dashboard is the invoice of record.",
	}
	ids := make([]uint, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
		if item.Status == "completed" {
			cost.Breakdown[0].Successes++
		}
	}
	if len(ids) == 0 {
		return cost, nil
	}
	var rows []struct {
		Product          string
		Operations       int64
		Successes        int64
		Records          int64
		BytesTransferred int64
		CostUSD          float64
	}
	if err := db.Model(&models.BrightDataUsage{}).
		Select("product", "COUNT(*) AS operations", "COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END), 0) AS successes", "COALESCE(SUM(records), 0) AS records", "COALESCE(SUM(bytes_transferred), 0) AS bytes_transferred", "COALESCE(SUM(cost_usd), 0) AS cost_usd").
		Where("archive_item_id IN ?", ids).Group("product").Order("product").Scan(&rows).Error; err != nil {
		return archiveResultCost{}, err
	}
	for _, row := range rows {
		cost.Breakdown = append(cost.Breakdown, archiveResultCostBreakdown{Provider: "brightdata", Product: row.Product, Operations: row.Operations, Successes: row.Successes, Records: row.Records, BytesTransferred: row.BytesTransferred, CostUSD: row.CostUSD, Estimated: true})
		cost.TotalUSD += row.CostUSD
		cost.Estimated = true
	}
	return cost, nil
}

func buildSocialPost(c *gin.Context, store storage.Storage, capture *models.Capture, sourceURL string) *socialPostResult {
	recognized := utils.IsVideoURL(sourceURL) || utils.IsGalleryDLURL(sourceURL)
	if !recognized {
		return nil
	}
	result := &socialPostResult{Status: "pending", Media: []normalizedMedia{}, RawMetadata: []rawMetadataLink{}, Warnings: []socialWarning{}, Provenance: socialProvenance{Source: "native", Mode: "primary"}}
	var social *models.ArchiveItem
	for i := range capture.ArchiveItems {
		if utils.ArchiveTypesEqual(capture.ArchiveItems[i].Type, utils.ArchiveTypeYtDlp) || utils.ArchiveTypesEqual(capture.ArchiveItems[i].Type, utils.ArchiveTypeGalleryDl) {
			social = &capture.ArchiveItems[i]
			break
		}
	}
	if social == nil {
		result.Status, result.Terminal = "failed", true
		code, message, retryable := "unsupported_url", "No social extractor was available for this recognized URL", false
		if utils.IsGalleryDLURL(sourceURL) && utils.GalleryDLURLRequiresCookies(sourceURL) && !utils.MediaCookiesConfigured() {
			code, message, retryable = "authentication_required", "Provider authentication is required and no credentials or fallback were available", true
		}
		result.Failure = &socialFailure{Code: code, Message: message, Retryable: retryable}
		return result
	}
	if social.Source == models.ArchiveSourceBrightData {
		result.Provenance = socialProvenance{Source: "brightdata", Mode: "fallback"}
	}
	switch social.Status {
	case "processing":
		result.Status = "processing"
	case "failed":
		result.Status, result.Terminal, result.Failure = "failed", true, &socialFailure{Code: "extractor_failed", Message: "The social extractor failed", Retryable: true}
	case "completed":
		result.Terminal = true
	}
	if social.Status != "completed" {
		return result
	}

	if utils.ArchiveTypesEqual(social.Type, utils.ArchiveTypeYtDlp) {
		buildVideoSocial(c, store, capture.ShortID, social, result)
	} else {
		buildGallerySocial(c, store, capture.ShortID, social, result)
	}
	validPost := result.Post != nil && (result.Post.ID != "" || result.Post.URL != "" || result.Post.Title != "" || result.Post.Text != "")
	result.Fulfilled = validPost && len(result.Media) > 0
	if result.Fulfilled {
		result.Status = "fulfilled"
	} else if len(result.Media) > 0 || result.Post != nil {
		result.Status = "partial"
	} else {
		result.Status = "failed"
	}
	if !result.Fulfilled && result.Failure == nil {
		code, msg := "metadata_unavailable", "Structured social metadata is unavailable"
		if social.MetadataKey == "" && utils.ArchiveTypesEqual(social.Type, utils.ArchiveTypeYtDlp) {
			code, msg = "legacy_archive", "This archive predates structured social metadata"
		}
		result.Failure = &socialFailure{Code: code, Message: msg, Retryable: false}
	}
	return result
}

func buildVideoSocial(c *gin.Context, store storage.Storage, shortID string, item *models.ArchiveItem, out *socialPostResult) {
	if item.StorageKey != "" {
		media := normalizedMedia{Index: 0, Type: "video", URL: fullPath(c, fmt.Sprintf("archive/%s/%s", shortID, utils.ArchiveTypeYtDlp)), Filename: filepath.Base(item.StorageKey), SizeBytes: item.FileSize}
		out.Media = append(out.Media, media)
	}
	if item.MetadataKey == "" {
		return
	}
	raw, err := readStoredJSON(store, item.MetadataKey, maxVideoMetadataSize)
	if err != nil {
		return
	}
	var meta archivers.VideoMetadata
	if json.Unmarshal(raw, &meta) != nil {
		return
	}
	out.Platform = meta.Platform
	author := &normalizedAuthor{ID: meta.AuthorID, Username: meta.Uploader, DisplayName: meta.Author}
	if author.ID == "" && author.Username == "" && author.DisplayName == "" {
		author = nil
	}
	eng := &socialEngagement{Views: meta.Engagement.Views, Likes: meta.Engagement.Likes, Comments: meta.Engagement.Comments, Reposts: meta.Engagement.Reposts}
	if eng.Views == nil && eng.Likes == nil && eng.Comments == nil && eng.Reposts == nil {
		eng = nil
	}
	out.Post = &normalizedPost{ID: meta.PostID, URL: meta.CanonicalURL, Title: meta.Title, Text: meta.Description, PublishedAt: meta.PublicationTimestamp, DurationSeconds: meta.DurationSeconds, Author: author, Engagement: eng, Tags: meta.Tags}
	if len(out.Media) > 0 {
		out.Media[0].ContentType = meta.Media.ContentType
		out.Media[0].SizeBytes = meta.Media.SizeBytes
		out.Media[0].Width = meta.Media.Width
		out.Media[0].Height = meta.Media.Height
		out.Media[0].DurationSeconds = meta.DurationSeconds
		out.Media[0].Quality = meta.Media.QualityLabel
	}
	if item.RawMetadataKey != "" {
		provider := meta.Provider
		if provider == "" {
			provider = "yt-dlp"
		}
		out.RawMetadata = append(out.RawMetadata, rawMetadataLink{provider, fullPath(c, fmt.Sprintf("video/%s/raw", shortID))})
	}
}

func buildGallerySocial(c *gin.Context, store storage.Storage, shortID string, item *models.ArchiveItem, out *socialPostResult) {
	zipReader, cleanup, ok := openGalleryZipData(store, item)
	if !ok {
		return
	}
	defer cleanup()
	var meta archivers.GalleryMetadata
	for _, f := range zipReader.File {
		if f.Name == galleryMetadataFilename {
			r, e := f.Open()
			if e == nil {
				_ = json.NewDecoder(r).Decode(&meta)
				r.Close()
			}
			break
		}
	}
	out.Platform = meta.Extractor
	author := &normalizedAuthor{Username: meta.Author, DisplayName: meta.AuthorName}
	if author.Username == "" && author.DisplayName == "" {
		author = nil
	}
	var eng *socialEngagement
	if meta.Likes != nil {
		eng = &socialEngagement{Likes: meta.Likes}
	}
	out.Post = &normalizedPost{ID: meta.PostID, URL: meta.PostURL, Title: meta.Title, Text: meta.Description, PublishedAt: meta.Date, Author: author, Engagement: eng, Tags: meta.Tags}
	fileMetadata := make(map[string]archivers.GalleryFile, len(meta.Files))
	for _, f := range meta.Files {
		fileMetadata[f.Name] = f
	}
	rawAvailable := false
	for _, f := range zipReader.File {
		if f.Name == galleryMetadataFilename || strings.HasSuffix(f.Name, ".json") {
			if f.Name != galleryMetadataFilename {
				rawAvailable = true
			}
			continue
		}
		ct := galleryFileContentType(f.Name)
		typ := "other"
		if strings.HasPrefix(ct, "image/") {
			typ = "image"
		} else if strings.HasPrefix(ct, "video/") {
			typ = "video"
		} else if strings.HasPrefix(ct, "audio/") {
			typ = "audio"
		}
		media := normalizedMedia{Index: len(out.Media), Type: typ, URL: fullPath(c, fmt.Sprintf("gallery/%s/file/%s", shortID, url.PathEscape(f.Name))), Filename: f.Name, ContentType: ct, SizeBytes: int64(f.UncompressedSize64)}
		if details, ok := fileMetadata[f.Name]; ok {
			if details.Width > 0 {
				width := int64(details.Width)
				media.Width = &width
			}
			if details.Height > 0 {
				height := int64(details.Height)
				media.Height = &height
			}
		}
		out.Media = append(out.Media, media)
	}
	bundle := fullPath(c, fmt.Sprintf("archive/%s/%s", shortID, utils.ArchiveTypeGalleryDl))
	out.BundleURL = &bundle
	provider := "gallery-dl"
	for _, f := range zipReader.File {
		if f.Name == "brightdata.json" {
			provider = "brightdata"
			break
		}
	}
	if rawAvailable {
		out.RawMetadata = append(out.RawMetadata, rawMetadataLink{provider, fullPath(c, fmt.Sprintf("gallery/%s/raw", shortID))})
	}
}

func fullPath(c *gin.Context, path string) string {
	return utils.BuildFullURL(c, strings.TrimPrefix(path, "/"))
}
