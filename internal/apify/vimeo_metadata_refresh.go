package apify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
)

const maxVimeoOEmbedBytes = 2 << 20

var vimeoVideoPathPattern = regexp.MustCompile(`^/(?:video/)?(\d+)(?:/|$)`)

type vimeoOEmbedRecord struct {
	Title       string `json:"title"`
	AuthorName  string `json:"author_name"`
	Description string `json:"description"`
	UploadDate  string `json:"upload_date"`
	VideoID     int64  `json:"video_id"`
	Duration    int64  `json:"duration"`
	Width       int64  `json:"width"`
	Height      int64  `json:"height"`
}

func isVimeoMetadataURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(strings.TrimPrefix(parsed.Hostname(), "www."), "vimeo.com") {
		return false
	}
	return vimeoVideoPathPattern.MatchString(parsed.EscapedPath())
}

func vimeoOEmbedURL(targetURL string) string {
	return "https://vimeo.com/api/oembed.json?url=" + url.QueryEscape(targetURL)
}

func (c *Client) refreshStoredVimeoMetadata(ctx context.Context, targetURL string, logWriter io.Writer, media archivers.VideoMedia) (archivers.Result, error) {
	requestURL := vimeoOEmbedURL(targetURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return archivers.Result{}, fmt.Errorf("build Vimeo oEmbed request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return archivers.Result{}, fmt.Errorf("fetch Vimeo oEmbed metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return archivers.Result{}, fmt.Errorf("Vimeo oEmbed returned %s", resp.Status)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxVimeoOEmbedBytes+1))
	var record vimeoOEmbedRecord
	if err := decoder.Decode(&record); err != nil {
		return archivers.Result{}, fmt.Errorf("decode Vimeo oEmbed metadata: %w", err)
	}
	if record.Title == "" || record.AuthorName == "" {
		return archivers.Result{}, fmt.Errorf("Vimeo oEmbed omitted title or author")
	}
	publication := ""
	if parsed, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(record.UploadDate)); err == nil {
		publication = parsed.UTC().Format(time.RFC3339)
	}
	if media.Extension == "" {
		media.Extension = ".mp4"
	}
	if media.ContentType == "" {
		media.ContentType = "video/mp4"
	}
	if media.Width == nil && record.Width > 0 {
		value := record.Width
		media.Width = &value
	}
	if media.Height == nil && record.Height > 0 {
		value := record.Height
		media.Height = &value
	}
	var duration *float64
	if record.Duration > 0 {
		value := float64(record.Duration)
		duration = &value
	}
	postID := ""
	if record.VideoID > 0 {
		postID = fmt.Sprint(record.VideoID)
	}
	metadataJSON, err := archivers.MarshalVideoMetadata(&archivers.VideoMetadata{
		SchemaVersion:        archivers.VideoMetadataSchemaVersion,
		SourceURL:            archivers.SanitizeURL(targetURL, nil),
		Platform:             "vimeo",
		Extractor:            "vimeo_oembed",
		PostID:               postID,
		CanonicalURL:         archivers.SanitizeURL(targetURL, nil),
		Title:                record.Title,
		Description:          record.Description,
		Author:               record.AuthorName,
		Uploader:             record.AuthorName,
		Channel:              record.AuthorName,
		PublicationTimestamp: publication,
		DurationSeconds:      duration,
		Media:                media,
		ArchivedAt:           time.Now().UTC().Format(time.RFC3339),
		Provenance:           "native",
		Provider:             "vimeo_oembed",
	})
	if err != nil {
		return archivers.Result{}, err
	}
	rawJSON, err := json.MarshalIndent(map[string]interface{}{
		"provider":    "vimeo_oembed",
		"video_id":    record.VideoID,
		"title":       record.Title,
		"author_name": record.AuthorName,
		"description": record.Description,
		"upload_date": record.UploadDate,
		"duration":    record.Duration,
		"width":       record.Width,
		"height":      record.Height,
	}, "", "  ")
	if err != nil {
		return archivers.Result{}, fmt.Errorf("encode Vimeo oEmbed metadata: %w", err)
	}
	fmt.Fprintf(logWriter, "Recovered historical post metadata from Vimeo's official oEmbed endpoint\n")
	return archivers.Result{
		Extension: media.Extension, ContentType: media.ContentType,
		Source: models.ArchiveSourceNative, Metadata: &archivers.Sidecar{Data: metadataJSON}, RawMetadata: &archivers.Sidecar{Data: rawJSON},
		Completeness: archivers.CompletenessComplete,
	}, nil
}
