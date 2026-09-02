package archivers

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type capturedHTMLVideoFacts struct {
	Title, Description, Channel, Published, Canonical, Duration, Image string
}

// CapturedHTMLSocialImageURL returns the provider-authored social preview URL
// embedded in a captured page. The caller must resolve that URL against an
// MHTML part rather than the live network: signatures expire, while the
// snapshot's matching image bytes are immutable.
func CapturedHTMLSocialImageURL(htmlData []byte) string {
	imageURL, _ := CapturedHTMLSocialImage(htmlData)
	return imageURL
}

// CapturedHTMLSocialImage returns both the preview and the root document's
// canonical URL so callers can prove the image belongs to the requested post
// rather than an Instagram/YouTube login or error page.
func CapturedHTMLSocialImage(htmlData []byte) (imageURL, canonicalURL string) {
	doc, err := html.Parse(strings.NewReader(string(htmlData)))
	if err != nil {
		return "", ""
	}
	facts := extractCapturedHTMLVideoFacts(doc)
	return facts.Image, facts.Canonical
}

// BuildCapturedHTMLVideoArtifacts recovers provider-authored post facts from
// the structured metadata embedded in a sibling MHTML snapshot. It is the
// last free source for a historical video after the live extractor fails: the
// page was captured at the same time as the immutable media and can preserve
// title, author, publication time, and duration even after a post is deleted.
// Only those narrow facts are copied; signed media URLs and the rest of the
// page never enter the new raw sidecar.
func BuildCapturedHTMLVideoArtifacts(htmlData []byte, sourceURL string, media VideoMedia, archivedAt time.Time) (*Sidecar, *Sidecar, error) {
	return buildCapturedHTMLVideoArtifacts(htmlData, sourceURL, media, archivedAt, false)
}

// BuildCapturedHTMLVideoArtifactsAllowingDescription is the final fallback
// for an official video page whose stronger live metadata sources have all
// failed. It accepts title plus description only when the captured canonical
// URL proves that the page is the requested post, rather than an error page.
func BuildCapturedHTMLVideoArtifactsAllowingDescription(htmlData []byte, sourceURL string, media VideoMedia, archivedAt time.Time) (*Sidecar, *Sidecar, error) {
	return buildCapturedHTMLVideoArtifacts(htmlData, sourceURL, media, archivedAt, true)
}

func buildCapturedHTMLVideoArtifacts(htmlData []byte, sourceURL string, media VideoMedia, archivedAt time.Time, allowDescription bool) (*Sidecar, *Sidecar, error) {
	doc, err := html.Parse(strings.NewReader(string(htmlData)))
	if err != nil {
		return nil, nil, fmt.Errorf("parse captured HTML: %w", err)
	}
	facts := extractCapturedHTMLVideoFacts(doc)
	platform := capturedVideoPlatform(sourceURL)
	if platform == "instagram" {
		channel, published := capturedInstagramAttribution(facts.Title, facts.Description)
		if facts.Channel == "" {
			facts.Channel = channel
		}
		if facts.Published == "" {
			facts.Published = published
		}
	} else if platform == "tiktok" && facts.Channel == "" {
		facts.Channel = capturedTikTokChannel(facts.Title)
	}
	if platform == "tiktok" && facts.Published == "" {
		// TikTok's provider-assigned video snowflake carries its creation
		// second. Prefer the resolved canonical URL because vm.tiktok.com
		// share links do not contain the video ID themselves.
		facts.Published = TikTokPublicationTimestamp(facts.Canonical)
		if facts.Published == "" {
			facts.Published = TikTokPublicationTimestamp(sourceURL)
		}
	}
	duration := parseISODurationSeconds(facts.Duration)
	published := normalizeCapturedDate(facts.Published)

	// Two independent post facts keeps a generic login/error page from being
	// promoted to video metadata merely because it has a <title> element.
	factCount := 0
	for _, present := range []bool{facts.Title != "", facts.Channel != "", published != "", duration != nil} {
		if present {
			factCount++
		}
	}
	if allowDescription && facts.Description != "" && capturedCanonicalMatches(sourceURL, facts.Canonical) {
		factCount++
	}
	if factCount < 2 {
		return nil, nil, fmt.Errorf("captured HTML contains fewer than two usable video facts")
	}

	if media.Extension == "" {
		media.Extension = ".mp4"
	}
	if media.ContentType == "" {
		media.ContentType = "video/mp4"
	}
	safeSourceURL := SanitizeURL(sourceURL, nil)
	canonical := SanitizeURL(facts.Canonical, nil)
	if canonical == "" {
		canonical = safeSourceURL
	}
	metadataJSON, err := MarshalVideoMetadata(&VideoMetadata{
		SchemaVersion:        VideoMetadataSchemaVersion,
		SourceURL:            safeSourceURL,
		Platform:             platform,
		Extractor:            "captured_html",
		CanonicalURL:         canonical,
		Title:                facts.Title,
		Description:          facts.Description,
		Author:               facts.Channel,
		Uploader:             facts.Channel,
		Channel:              facts.Channel,
		PublicationTimestamp: published,
		DurationSeconds:      duration,
		Media:                media,
		ArchivedAt:           archivedAt.UTC().Format(time.RFC3339),
		Provenance:           "native",
		Provider:             "captured_mhtml",
	})
	if err != nil {
		return nil, nil, err
	}
	rawJSON, err := json.MarshalIndent(map[string]interface{}{
		"capture_source":        "mhtml",
		"title":                 facts.Title,
		"description":           facts.Description,
		"channel":               facts.Channel,
		"publication_timestamp": published,
		"duration_seconds":      duration,
		"canonical_url":         canonical,
	}, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("encode captured HTML facts: %w", err)
	}
	return &Sidecar{Data: metadataJSON}, &Sidecar{Data: rawJSON}, nil
}

func capturedCanonicalMatches(sourceURL, capturedURL string) bool {
	source, sourceErr := url.Parse(strings.TrimSpace(sourceURL))
	captured, capturedErr := url.Parse(strings.TrimSpace(capturedURL))
	if sourceErr != nil || capturedErr != nil {
		return false
	}
	sourceHost := strings.TrimPrefix(strings.ToLower(source.Hostname()), "www.")
	capturedHost := strings.TrimPrefix(strings.ToLower(captured.Hostname()), "www.")
	return sourceHost != "" && sourceHost == capturedHost && strings.TrimRight(source.EscapedPath(), "/") == strings.TrimRight(captured.EscapedPath(), "/")
}

func extractCapturedHTMLVideoFacts(root *html.Node) capturedHTMLVideoFacts {
	var facts capturedHTMLVideoFacts
	var walk func(*html.Node, bool)
	walk = func(node *html.Node, inAuthor bool) {
		if node.Type == html.ElementNode {
			attrs := htmlAttributes(node)
			itemProp := strings.ToLower(attrs["itemprop"])
			inAuthor = inAuthor || itemProp == "author"
			content := strings.TrimSpace(attrs["content"])
			property := strings.ToLower(attrs["property"])
			name := strings.ToLower(attrs["name"])
			switch {
			case property == "og:title" && content != "":
				facts.Title = content
			case facts.Title == "" && (name == "title" || itemProp == "name") && content != "" && !inAuthor:
				facts.Title = content
			}
			if facts.Description == "" && content != "" && (property == "og:description" || name == "description") {
				facts.Description = content
			}
			if content != "" && (property == "og:image" || (facts.Image == "" && name == "twitter:image")) {
				facts.Image = content
			}
			if facts.Channel == "" && content != "" && ((inAuthor && itemProp == "name") || name == "author" || property == "article:author") {
				facts.Channel = content
			}
			// Some captured YouTube pages omit the schema.org author name but
			// retain the provider-authored channel handle in the sibling author
			// URL. The handle is an explicit attribution, not an inference from
			// the video ID or title.
			if facts.Channel == "" && inAuthor && itemProp == "url" {
				facts.Channel = capturedAuthorHandle(attrs["href"])
			}
			if facts.Published == "" && content != "" && (itemProp == "datepublished" || itemProp == "uploaddate" || property == "article:published_time") {
				facts.Published = content
			}
			if facts.Duration == "" && content != "" && itemProp == "duration" {
				facts.Duration = content
			}
			if facts.Canonical == "" && strings.EqualFold(attrs["rel"], "canonical") {
				facts.Canonical = strings.TrimSpace(attrs["href"])
			}
			if node.Data == "title" && facts.Title == "" {
				facts.Title = strings.TrimSpace(nodeText(node))
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, inAuthor)
		}
	}
	walk(root, false)
	return facts
}

func capturedAuthorHandle(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	for _, part := range strings.Split(strings.Trim(parsed.Path, "/"), "/") {
		if strings.HasPrefix(part, "@") && len(part) > 1 {
			return strings.TrimPrefix(part, "@")
		}
	}
	return ""
}

func htmlAttributes(node *html.Node) map[string]string {
	attrs := make(map[string]string, len(node.Attr))
	for _, attr := range node.Attr {
		attrs[strings.ToLower(attr.Key)] = attr.Val
	}
	return attrs
}

func nodeText(node *html.Node) string {
	var out strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			out.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return out.String()
}

var isoDurationPattern = regexp.MustCompile(`(?i)^PT(?:(\d+(?:\.\d+)?)H)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)S)?$`)

var (
	instagramAttributionPattern = regexp.MustCompile(`(?i)(?:^|[-\x{2013}\x{2014}]\s*)(@?[a-z0-9._]+)\s+on\s+((?:January|February|March|April|May|June|July|August|September|October|November|December)\s+\d{1,2},\s+\d{4})`)
	instagramTitleAuthorPattern = regexp.MustCompile(`(?i)^(.+?)\s+on\s+Instagram\s*:`)
	tikTokTitleAuthorPattern    = regexp.MustCompile(`(?i)^(.+?)\s+on\s+TikTok\s*$`)
	tikTokProfileTitlePattern   = regexp.MustCompile(`(?i)^.+?\s+\(@?([a-z0-9._]+)\)\s*\|\s*TikTok\s*$`)
)

// Instagram's captured Open Graph description carries an explicit provider
// attribution even when its schema.org author/date nodes are absent, for
// example "handle on July 14, 2026: ...". Recovering those two authored facts
// turns a snapshot that is otherwise only a caption into useful historical
// metadata without guessing from the shortcode or today's live page.
func capturedInstagramAttribution(title, description string) (channel, published string) {
	if match := instagramAttributionPattern.FindStringSubmatch(strings.TrimSpace(description)); len(match) == 3 {
		channel = strings.TrimPrefix(strings.TrimSpace(match[1]), "@")
		published = strings.TrimSpace(match[2])
	}
	if channel == "" {
		if match := instagramTitleAuthorPattern.FindStringSubmatch(strings.TrimSpace(title)); len(match) == 2 {
			channel = strings.TrimSpace(match[1])
		}
	}
	return channel, published
}

func capturedTikTokChannel(title string) string {
	if match := tikTokProfileTitlePattern.FindStringSubmatch(strings.TrimSpace(title)); len(match) == 2 {
		return strings.TrimPrefix(strings.TrimSpace(match[1]), "@")
	}
	match := tikTokTitleAuthorPattern.FindStringSubmatch(strings.TrimSpace(title))
	if len(match) != 2 {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(match[1]), "@")
}

// TikTokPublicationTimestamp decodes the creation second that TikTok embeds
// in the high 32 bits of its provider-assigned video snowflake. This is only
// accepted for a TikTok /video/<decimal-id> URL and for plausible platform-era
// dates, so profile paths and unrelated numeric routes can never become a
// publication timestamp.
func TikTokPublicationTimestamp(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	if host != "tiktok.com" && !strings.HasSuffix(host, ".tiktok.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if !strings.EqualFold(parts[i], "video") {
			continue
		}
		id, err := strconv.ParseUint(parts[i+1], 10, 64)
		if err != nil {
			return ""
		}
		created := time.Unix(int64(id>>32), 0).UTC()
		platformLaunch := time.Date(2016, time.September, 1, 0, 0, 0, 0, time.UTC)
		if created.Before(platformLaunch) || created.After(time.Now().UTC().Add(24*time.Hour)) {
			return ""
		}
		return created.Format(time.RFC3339)
	}
	return ""
}

func parseISODurationSeconds(value string) *float64 {
	match := isoDurationPattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return nil
	}
	seconds := 0.0
	for i, multiplier := range []float64{3600, 60, 1} {
		if match[i+1] == "" {
			continue
		}
		part, err := strconv.ParseFloat(match[i+1], 64)
		if err != nil {
			return nil
		}
		seconds += part * multiplier
	}
	if seconds <= 0 {
		return nil
	}
	return &seconds
}

func normalizeCapturedDate(value string) string {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02", "2006-01-02 15:04:05", "January 2, 2006"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func capturedVideoPlatform(sourceURL string) string {
	parsed, err := url.Parse(sourceURL)
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	switch {
	case strings.Contains(host, "youtu"):
		return "youtube"
	case strings.Contains(host, "instagram"):
		return "instagram"
	case strings.Contains(host, "tiktok"):
		return "tiktok"
	case strings.Contains(host, "vimeo"):
		return "vimeo"
	case strings.Contains(host, "facebook") || host == "fb.watch":
		return "facebook"
	default:
		return strings.TrimSuffix(host, ".com")
	}
}
