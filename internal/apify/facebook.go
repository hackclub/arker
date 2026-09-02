package apify

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// Facebook goes through apify/facebook-posts-scraper, which answers with one
// of two shapes depending on the URL: a video node for /videos/ and /watch
// URLs (videoId, browser_native_*_url, publish_time, reaction_count) and a
// post for permalinks (postId, user, text, media[] typed by __typename).
// facebookPost flattens both so the rest of the flow reads one thing.

type facebookInput struct {
	StartURLs    []facebookStartURL `json:"startUrls"`
	ResultsLimit int                `json:"resultsLimit"`
}

type facebookStartURL struct {
	URL string `json:"url"`
}

func facebookRunInput(targetURL string) facebookInput {
	return facebookInput{StartURLs: []facebookStartURL{{URL: targetURL}}, ResultsLimit: 1}
}

// facebookPost is the platform-neutral reading of either record shape.
type facebookPost struct {
	ID         string
	URL        string
	Title      string
	Text       string
	AuthorName string
	AuthorID   string
	PageName   string
	Date       string
	Duration   *float64
	Likes      *int64
	Comments   *int64
	Shares     *int64
	Views      *int64
	Width      *int64
	Height     *int64
	Poster     string
	Entries    []mediaEntry
}

func (c *Client) archiveFacebook(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	switch {
	case utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeYtDlp):
		return c.facebookVideo(ctx, targetURL, logWriter, db, itemID, shortID)
	case utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeGalleryDl):
		return c.facebookGallery(ctx, targetURL, logWriter, db, itemID, shortID)
	default:
		return archivers.Result{}, fmt.Errorf("no Facebook fallback for archive type %s", itemType)
	}
}

// facebookResolve runs the actor and flattens the record. A /reel/ URL
// sometimes comes back as a post whose video attachment carries no delivery
// URL; the same video addressed as /watch/?v=<id> answers as a video node,
// so that is tried once before giving up.
func (c *Client) facebookResolve(ctx context.Context, usage *models.FallbackUsage, targetURL string, db *gorm.DB, logWriter io.Writer) (facebookPost, map[string]any, error) {
	record, _, err := c.resolveRecord(ctx, db, usage, ActorFacebook, facebookRunInput(targetURL), logWriter, "postId", "videoId", "post_id", "id")
	if err != nil {
		return facebookPost{}, nil, err
	}
	post := parseFacebookRecord(record, logWriter)
	if len(post.Entries) > 0 {
		return post, record, nil
	}
	videoID := facebookVideoIDFromURL(targetURL)
	if videoID == "" {
		videoID = stringField(nestedObject(record, "video"), "id")
	}
	if videoID == "" {
		return post, record, nil
	}
	watchURL := "https://www.facebook.com/watch/?v=" + videoID
	if strings.EqualFold(strings.TrimRight(watchURL, "/"), strings.TrimRight(targetURL, "/")) {
		return post, record, nil
	}
	fmt.Fprintf(logWriter, "Record carries no media URL; re-resolving as %s\n", watchURL)
	usage.Detail = "post shape without media; retried as watch URL"
	c.recordUsage(db, usage)
	retryUsage := &models.FallbackUsage{ArchiveItemID: usage.ArchiveItemID, ShortID: usage.ShortID, URL: watchURL}
	retryRecord, _, err := c.resolveRecord(ctx, db, retryUsage, ActorFacebook, facebookRunInput(watchURL), logWriter, "postId", "videoId", "post_id", "id")
	if err != nil {
		return facebookPost{}, nil, err
	}
	*usage = *retryUsage
	retryPost := parseFacebookRecord(retryRecord, logWriter)
	// The first record often has the text and counts the video node lacks.
	if retryPost.Text == "" {
		retryPost.Text = post.Text
	}
	if retryPost.Title == "" {
		retryPost.Title = post.Title
	}
	if retryPost.AuthorName == "" {
		retryPost.AuthorName = post.AuthorName
	}
	if retryPost.PageName == "" {
		retryPost.PageName = post.PageName
	}
	if retryPost.Likes == nil {
		retryPost.Likes = post.Likes
	}
	if retryPost.Comments == nil {
		retryPost.Comments = post.Comments
	}
	return retryPost, retryRecord, nil
}

var facebookVideoIDPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)/reel/(\d+)`),
	regexp.MustCompile(`(?i)/videos/(?:[^/]+/)?(\d+)`),
	regexp.MustCompile(`(?i)[?&]v=(\d+)`),
}

func facebookVideoIDFromURL(rawURL string) string {
	for _, pattern := range facebookVideoIDPatterns {
		if m := pattern.FindStringSubmatch(rawURL); m != nil {
			return m[1]
		}
	}
	return ""
}

// facebookVideo produces the MP4 for a video permalink.
func (c *Client) facebookVideo(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	usage := &models.FallbackUsage{ArchiveItemID: itemID, ShortID: shortID, URL: targetURL}
	post, record, err := c.facebookResolve(ctx, usage, targetURL, db, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}
	var videoURL string
	for _, entry := range post.Entries {
		if entry.isVideo() {
			videoURL = entry.URL
			break
		}
	}
	if videoURL == "" {
		err := fmt.Errorf("Facebook record for %s has no video URL (%d media attachment(s))", targetURL, len(post.Entries))
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	logFacebookMetadata(logWriter, post)
	return c.archiveVideo(ctx, videoPlan{
		URL:          videoURL,
		Metadata:     facebookVideoMetadata(post, targetURL, videoURL),
		Record:       record,
		ThumbnailURL: post.Poster,
		TempPattern:  "arker-apify-fb-*.mp4",
	}, usage, db, logWriter)
}

// facebookGallery produces a gallery bundle for a photo post or permalink.
// A post that turns out to be exactly one video also gets the normalized
// video contract, which is where the counts and the timestamp live.
func (c *Client) facebookGallery(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	usage := &models.FallbackUsage{ArchiveItemID: itemID, ShortID: shortID, URL: targetURL}
	post, record, err := c.facebookResolve(ctx, usage, targetURL, db, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}
	if len(post.Entries) == 0 {
		err := fmt.Errorf("Facebook record for %s contains no media (a text-only post has nothing to archive)", targetURL)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	logFacebookMetadata(logWriter, post)

	meta := facebookGalleryMetadata(post, targetURL)
	result, err := c.archiveGallery(ctx, galleryPlan{
		Entries:   post.Entries,
		Meta:      meta,
		Record:    record,
		PosterURL: post.Poster,
	}, usage, db, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}
	if len(post.Entries) == 1 && post.Entries[0].isVideo() {
		if err := attachSingleVideoMetadata(&result, meta, record, facebookVideoMetadata(post, targetURL, post.Entries[0].URL), ActorFacebook); err != nil {
			closeResultData(result)
			return archivers.Result{}, fmt.Errorf("failed to build Facebook video metadata: %w", err)
		}
	}
	return result, nil
}

// parseFacebookRecord flattens either record shape.
func parseFacebookRecord(record map[string]any, logWriter io.Writer) facebookPost {
	if stringField(record, "videoId") != "" || nestedObject(record, "videoDeliveryLegacyFields") != nil {
		return parseFacebookVideoNode(record)
	}
	return parseFacebookPostRecord(record, logWriter)
}

func parseFacebookVideoNode(record map[string]any) facebookPost {
	post := facebookPost{
		ID:         firstNonEmptyString(stringField(record, "videoId"), stringField(record, "id")),
		URL:        firstNonEmptyString(stringField(record, "permalink_url"), stringField(record, "url")),
		Title:      stringField(record, "previewTitle"),
		Text:       firstNonEmptyString(stringField(record, "previewDescription"), stringField(record, "previewTitle")),
		AuthorID:   stringField(nestedObject(record, "owner"), "id"),
		AuthorName: stringField(nestedObject(record, "owner"), "name"),
		Date:       timestampString(record["publish_time"]),
		Likes:      facebookReactionCount(record),
		Comments:   intField(record, "total_comment_count"),
		Width:      intField(record, "width"),
		Height:     intField(record, "height"),
		Poster:     facebookStripCTP(stringField(nestedObject(record, "preferred_thumbnail", "image"), "uri")),
	}
	if page := stringField(record, "pageName"); page != "" && page != "reel" && page != "watch" {
		post.PageName = page
	}
	if ms := floatField(record, "playable_duration_in_ms"); ms != nil && *ms > 0 {
		seconds := *ms / 1000
		post.Duration = &seconds
	}
	if post.PageName == "" {
		post.PageName = facebookPageFromURL(post.URL)
	}
	if u := facebookDeliveryURL(record); u != "" {
		post.Entries = []mediaEntry{{URL: u, Type: "Video"}}
	}
	return post
}

// facebookPageFromURL reads the vanity page name out of a permalink such as
// /HackClubHQ/videos/123/, the only place a video node names its page when
// the actor was addressed through a reel or watch URL.
func facebookPageFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) < 2 {
		return ""
	}
	switch segments[1] {
	case "videos", "posts", "photos", "reels":
	default:
		return ""
	}
	page := segments[0]
	if page == "" || strings.Trim(page, "0123456789") == "" {
		return ""
	}
	switch page {
	case "reel", "watch", "share", "photo", "video", "story.php", "permalink.php":
		return ""
	}
	return page
}

func parseFacebookPostRecord(record map[string]any, logWriter io.Writer) facebookPost {
	user := nestedObject(record, "user")
	post := facebookPost{
		ID:         firstNonEmptyString(stringField(record, "postId"), stringField(record, "post_id")),
		URL:        firstNonEmptyString(stringField(record, "url"), stringField(record, "topLevelUrl"), stringField(record, "facebookUrl")),
		Text:       firstNonEmptyString(stringField(record, "text"), stringField(nestedObject(record, "message"), "text")),
		AuthorName: stringField(user, "name"),
		AuthorID:   stringField(user, "id"),
		PageName:   stringField(record, "pageName"),
		Date:       firstNonEmptyString(timestampString(record["time"]), timestampString(record["timestamp"]), timestampString(record["creation_time"])),
		Likes:      intField(record, "likes"),
		Comments:   facebookCommentCount(record),
		Shares:     intField(record, "shares"),
		Views:      intField(record, "viewsCount", "videoPostViewCount"),
	}
	seen := map[string]bool{}
	for i, raw := range nestedList(record, "media") {
		media, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch stringField(media, "__typename") {
		case "Photo":
			u := facebookStripCTP(firstNonEmptyString(stringField(nestedObject(media, "image"), "uri"), stringField(media, "thumbnail")))
			if u == "" || seen[u] {
				continue
			}
			seen[u] = true
			post.Entries = append(post.Entries, mediaEntry{URL: u, Type: "Photo"})
			if post.Poster == "" {
				post.Poster = u
			}
		case "Video":
			u := facebookDeliveryURL(media)
			if u == "" {
				fmt.Fprintf(logWriter, "Attachment %d is a video with no delivery URL in the record; skipping\n", i+1)
				continue
			}
			if seen[u] {
				continue
			}
			seen[u] = true
			post.Entries = append(post.Entries, mediaEntry{URL: u, Type: "Video"})
			if post.Poster == "" {
				post.Poster = facebookStripCTP(firstNonEmptyString(stringField(nestedObject(media, "image"), "uri"), stringField(media, "thumbnail")))
			}
			if post.Duration == nil {
				if ms := floatField(media, "playable_duration_in_ms"); ms != nil && *ms > 0 {
					seconds := *ms / 1000
					post.Duration = &seconds
				}
			}
			if post.Width == nil {
				post.Width, post.Height = intField(media, "width"), intField(media, "height")
			}
		default:
			// The first media[] item is routinely the post's own permalink
			// with no __typename: a page, not a slide.
		}
	}
	return post
}

// facebookDeliveryURL picks the best progressive MP4 a video node offers.
func facebookDeliveryURL(node map[string]any) string {
	delivery := nestedObject(node, "videoDeliveryLegacyFields")
	return firstNonEmptyString(
		stringField(delivery, "browser_native_hd_url"),
		stringField(delivery, "browser_native_sd_url"),
		stringField(node, "video_hd_url"),
		stringField(node, "video_sd_url"),
		stringField(node, "playable_url_quality_hd"),
		stringField(node, "playable_url"),
	)
}

func facebookReactionCount(record map[string]any) *int64 {
	if count := intField(nestedObject(record, "reaction_count"), "count"); count != nil {
		return count
	}
	return intField(record, "likes")
}

func facebookCommentCount(record map[string]any) *int64 {
	if count := intField(record, "total_comment_count", "commentsCount"); count != nil {
		return count
	}
	switch comments := record["comments"].(type) {
	case float64:
		v := int64(comments)
		return &v
	case map[string]any:
		return intField(comments, "total_count", "count")
	case []any:
		v := int64(len(comments))
		return &v
	}
	return nil
}

// facebookStripCTP removes the `ctp=` crop parameter from a CDN image URL.
// With it the CDN serves a 590px rendition; without it, the full size.
func facebookStripCTP(rawURL string) string {
	if rawURL == "" || !strings.Contains(rawURL, "ctp=") {
		return rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := parsed.Query()
	q.Del("ctp")
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

func facebookAuthorHandle(post facebookPost) string {
	return firstNonEmptyString(post.PageName, post.AuthorName, post.AuthorID)
}

func facebookHashtags(post facebookPost) []string {
	return hashtagsFromText(post.Text)
}

func facebookGalleryMetadata(post facebookPost, sourceURL string) *archivers.GalleryMetadata {
	return &archivers.GalleryMetadata{
		SourceURL:   sourceURL,
		Extractor:   "facebook",
		Subcategory: "apify",
		PostID:      post.ID,
		PostURL:     post.URL,
		Author:      facebookAuthorHandle(post),
		AuthorName:  firstNonEmptyString(post.AuthorName, post.PageName),
		Title:       post.Title,
		Description: post.Text,
		Date:        post.Date,
		Likes:       post.Likes,
		Views:       post.Views,
		Comments:    post.Comments,
		Tags:        facebookHashtags(post),
		ToolVersion: "apify:" + strings.ReplaceAll(ActorFacebook, "~", "/"),
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}
}

func facebookVideoMetadata(post facebookPost, sourceURL, videoURL string) *archivers.VideoMetadata {
	title := post.Title
	if title == "" {
		if line, _, _ := strings.Cut(strings.TrimSpace(post.Text), "\n"); strings.TrimSpace(line) != "" {
			title = truncate(strings.TrimSpace(line), 120)
		} else if author := firstNonEmptyString(post.AuthorName, post.PageName); author != "" {
			title = "Video by " + author
		}
	}
	return &archivers.VideoMetadata{
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "facebook",
		Extractor:            "facebook",
		PostID:               post.ID,
		CanonicalURL:         archivers.SanitizeURL(firstNonEmptyString(post.URL, sourceURL), nil),
		Title:                title,
		Description:          post.Text,
		Author:               facebookAuthorHandle(post),
		AuthorID:             post.AuthorID,
		Uploader:             facebookAuthorHandle(post),
		UploaderID:           post.AuthorID,
		Channel:              firstNonEmptyString(post.AuthorName, post.PageName),
		ChannelID:            post.AuthorID,
		PublicationTimestamp: post.Date,
		DurationSeconds:      post.Duration,
		Engagement: archivers.VideoEngagement{
			Views:    post.Views,
			Likes:    post.Likes,
			Comments: post.Comments,
			Reposts:  post.Shares,
		},
		Tags: facebookHashtags(post),
		Media: archivers.VideoMedia{
			Width:        post.Width,
			Height:       post.Height,
			QualityLabel: facebookQualityLabel(videoURL),
		},
	}
}

// facebookQualityLabel names the rendition: "hd" or "sd" from which delivery
// URL was chosen is not in the URL itself, so only the height is reported
// when the record carried one; otherwise nothing.
func facebookQualityLabel(videoURL string) string {
	return videoQualityLabel(videoURL)
}

func logFacebookMetadata(logWriter io.Writer, post facebookPost) {
	if author := firstNonEmptyString(post.AuthorName, post.PageName); author != "" {
		fmt.Fprintf(logWriter, "Author: %s\n", author)
	}
	if post.Title != "" {
		fmt.Fprintf(logWriter, "Title: %s\n", utils.TruncateForLog(post.Title, 300))
	} else if post.Text != "" {
		fmt.Fprintf(logWriter, "Text: %s\n", utils.TruncateForLog(post.Text, 300))
	}
	if post.Date != "" {
		fmt.Fprintf(logWriter, "Posted: %s\n", post.Date)
	}
	if post.Likes != nil {
		fmt.Fprintf(logWriter, "Reactions: %d\n", *post.Likes)
	}
	fmt.Fprintf(logWriter, "Media: %d attachment(s)\n", len(post.Entries))
}
