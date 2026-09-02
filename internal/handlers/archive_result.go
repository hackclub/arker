package handlers

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	Status    string `json:"status"`
	Terminal  bool   `json:"terminal"`
	Fulfilled bool   `json:"fulfilled"`
	Platform  string `json:"platform,omitempty"`
	// Completeness says whether Media is the whole post or only what survived.
	// Null until the extractor item reaches a completed state; the status field
	// carries the answer before then.
	Completeness *socialCompleteness `json:"completeness"`
	Post         *normalizedPost     `json:"post"`
	Media        []normalizedMedia   `json:"media"`
	// Subtitles and Transcript are present only when the platform exposed
	// captions. Their absence says nothing about whether the archive is
	// complete: most posts have none, and requiring them would make almost
	// every archive read degraded.
	Subtitles  []socialSubtitle  `json:"subtitles,omitempty"`
	Transcript *socialTranscript `json:"transcript,omitempty"`
	// Music is the soundtrack behind an image post (Instagram carousel,
	// TikTok slideshow). It is not a media card and is not counted by
	// completeness: a three-photo post with a track is three assets and one
	// soundtrack. Same shape as the gallery manifest's music object.
	Music       *galleryManifestMusic `json:"music,omitempty"`
	BundleURL   *string               `json:"bundle_url"`
	RawMetadata []rawMetadataLink     `json:"raw_metadata"`
	Provenance  socialProvenance      `json:"provenance"`
	Warnings    []socialWarning       `json:"warnings"`
	Failure     *socialFailure        `json:"failure"`
}

// socialCompleteness reports how much of the post this archive holds.
//
// Stored is what this API can actually serve, not what the archiver believed it
// wrote, so a recorded verdict that no longer matches the artifact cannot make
// a capture read complete.
type socialCompleteness struct {
	State string `json:"state"`
	// Expected is nil when nothing told Arker how many assets the post has,
	// which is what forces the unknown state.
	Expected       *int  `json:"expected,omitempty"`
	Stored         int   `json:"stored"`
	MissingIndices []int `json:"missing_indices,omitempty"`
}

type normalizedPost struct {
	ID              string   `json:"id,omitempty"`
	URL             string   `json:"url,omitempty"`
	Title           string   `json:"title,omitempty"`
	Text            string   `json:"text,omitempty"`
	PublishedAt     string   `json:"published_at,omitempty"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
	// MediaType is the platform's own delivery format ("short", "video"),
	// carried through from the normalized record so this endpoint and
	// /video/:shortid/manifest never disagree. Absent where the provider
	// names none.
	MediaType  string            `json:"media_type,omitempty"`
	Author     *normalizedAuthor `json:"author,omitempty"`
	Engagement *socialEngagement `json:"engagement,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
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
	// AltText is the poster's own description of this image, where the
	// platform exposes one. Often the only text on an image-only post.
	AltText string `json:"alt_text,omitempty"`
}

type rawMetadataLink struct {
	Provider string `json:"provider"`
	URL      string `json:"url"`
}

// socialSubtitle is one stored caption track. Kind distinguishes a
// human-authored track from speech recognition, which matters to anyone
// quoting the archive.
type socialSubtitle struct {
	Lang      string `json:"lang"`
	Kind      string `json:"kind"`
	Format    string `json:"format"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	URL       string `json:"url"`
}

// socialTranscript is the readable text derived from the best subtitle track.
// The track itself remains the primary record; this is a convenience, and it
// says so when it had to be cut short.
type socialTranscript struct {
	Lang       string `json:"lang"`
	Source     string `json:"source"`
	Characters int    `json:"characters"`
	Truncated  bool   `json:"truncated,omitempty"`
	Text       string `json:"text"`
	URL        string `json:"url"`
}

// socialProvenance records how the artifact was obtained, including what it
// cost and what went wrong on the way. Attempts, the failure reason and the
// fallback ops are additive: a caller that only reads source and mode is
// unaffected.
type socialProvenance struct {
	Source string `json:"source"`
	Mode   string `json:"mode"`
	// Attempts is how many times the job ran (River's attempt counter). Zero
	// means the item has not been picked up yet.
	Attempts int `json:"attempts,omitempty"`
	// LastFailureReason is the most recent failure line from the item's own
	// archive log, sanitized: configured secrets are redacted and every URL is
	// replaced, so a signed CDN link can never leave through this field. Only
	// populated when something actually went wrong, and always best-effort —
	// logs are rotated and an empty string means "not recorded", never "clean".
	LastFailureReason string `json:"last_failure_reason,omitempty"`
	// FallbackOps summarizes the paid fallback work done for this item. Cost
	// is the same figure the top-level cost block reports; the provider's
	// billing dashboard remains the invoice of record.
	FallbackOps []socialFallbackOp `json:"fallback_ops,omitempty"`
}

type socialFallbackOp struct {
	Provider         string `json:"provider"`
	Product          string `json:"product"`
	Operations       int64  `json:"operations"`
	Successes        int64  `json:"successes"`
	Records          int64  `json:"records,omitempty"`
	BytesTransferred int64  `json:"bytes_transferred,omitempty"`
	// OperationIDs are the provider-side operation identifiers (Apify run IDs;
	// Bright Data snapshot IDs for historical rows). snapshot_ids is the
	// historical name of the same field and is kept for existing readers.
	OperationIDs     []string `json:"operation_ids,omitempty"`
	SnapshotIDs      []string `json:"snapshot_ids,omitempty"`
	EstimatedCostUSD float64  `json:"estimated_cost_usd"`
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
	response.SocialPost = buildSocialPost(c, store, db, &canonical, response.SourceURL)
	c.JSON(http.StatusOK, response)
}

func buildArchiveResultCost(db *gorm.DB, items []models.ArchiveItem) (archiveResultCost, error) {
	cost := archiveResultCost{
		Currency:  "USD",
		Breakdown: []archiveResultCostBreakdown{{Provider: "native", Operations: int64(len(items)), CostUSD: 0, Estimated: false}},
		Note:      "Native archive operations are free. Apify costs are the platform-reported run cost; historical Bright Data rows are estimates from configured rates. The provider's billing dashboard is the invoice of record.",
	}
	ids := make([]uint, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
		if item.Status == "completed" && !models.IsFallbackSource(item.Source) {
			cost.Breakdown[0].Successes++
		}
	}
	if len(ids) == 0 {
		return cost, nil
	}
	var rows []struct {
		Provider         string
		Product          string
		Operations       int64
		Successes        int64
		Records          int64
		BytesTransferred int64
		CostUSD          float64
	}
	if err := db.Model(&models.FallbackUsage{}).
		Select("provider", "product", "COUNT(*) AS operations", "COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END), 0) AS successes", "COALESCE(SUM(records), 0) AS records", "COALESCE(SUM(bytes_transferred), 0) AS bytes_transferred", "COALESCE(SUM(cost_usd), 0) AS cost_usd").
		Where("archive_item_id IN ?", ids).Group("provider").Group("product").Order("provider").Order("product").Scan(&rows).Error; err != nil {
		return archiveResultCost{}, err
	}
	for _, row := range rows {
		provider := row.Provider
		if provider == "" {
			provider = models.FallbackProviderBrightData
		}
		// Bright Data costs were rate-based estimates; Apify reports the
		// billed amount for each run.
		estimated := provider == models.FallbackProviderBrightData
		cost.Breakdown = append(cost.Breakdown, archiveResultCostBreakdown{Provider: provider, Product: row.Product, Operations: row.Operations, Successes: row.Successes, Records: row.Records, BytesTransferred: row.BytesTransferred, CostUSD: row.CostUSD, Estimated: estimated})
		cost.TotalUSD += row.CostUSD
		if estimated {
			cost.Estimated = true
		}
	}
	return cost, nil
}

func buildSocialPost(c *gin.Context, store storage.Storage, db *gorm.DB, capture *models.Capture, sourceURL string) *socialPostResult {
	// Recognition uses the centralized helper so every claimed platform shape
	// (including TikTok photo posts) gets an explicit social outcome.
	recognized := utils.IsSocialMediaPostURL(sourceURL)
	if !recognized {
		return nil
	}
	result := &socialPostResult{Status: "pending", Media: []normalizedMedia{}, RawMetadata: []rawMetadataLink{}, Warnings: []socialWarning{}, Provenance: socialProvenance{Source: "native", Mode: "primary"}}
	social := selectSocialItem(capture.ArchiveItems, sourceURL)
	if social == nil {
		result.Status, result.Terminal = "failed", true
		code, message, retryable := "unsupported_url", "No social extractor was available for this recognized URL", false
		if utils.IsGalleryDLURL(sourceURL) && utils.GalleryDLURLRequiresCookies(sourceURL) && !utils.MediaCookiesConfigured() {
			code, message, retryable = "authentication_required", "Provider authentication is required and no credentials or fallback were available", true
		}
		result.Failure = &socialFailure{Code: code, Message: message, Retryable: retryable}
		return result
	}
	if models.IsFallbackSource(social.Source) {
		result.Provenance.Source, result.Provenance.Mode = social.Source, "fallback"
	}
	// Attempts and paid fallback work describe how the archive was obtained, so
	// they are reported whatever the outcome — including for an item that is
	// still running or has failed outright.
	result.Provenance.Attempts = social.RetryCount
	result.Provenance.FallbackOps = fallbackOps(db, social.ID)

	switch social.Status {
	case "processing":
		result.Status = "processing"
	case "failed":
		result.Status, result.Terminal, result.Failure = "failed", true, &socialFailure{Code: "extractor_failed", Message: "The social extractor failed", Retryable: true}
	case "completed":
		result.Terminal = true
	}
	if social.Status != "completed" {
		if social.Status == "failed" || social.RetryCount > 1 {
			result.Provenance.LastFailureReason = lastFailureReason(db, social)
		}
		return result
	}

	// fromArchive is the verdict the archiver wrote into the artifact itself.
	// It is only available for gallery bundles; a video's completeness is
	// structural and comes from the item's own keys.
	var fromArchive *archivers.Completeness
	if utils.ArchiveTypesEqual(social.Type, utils.ArchiveTypeYtDlp) {
		buildVideoSocial(c, store, capture.ShortID, social, result)
	} else {
		fromArchive = buildGallerySocial(c, store, capture.ShortID, social, result)
	}

	completeness := socialCompletenessFor(social, fromArchive, len(result.Media))
	result.Completeness = &completeness

	// Fulfilled is a claim that the whole post is archived and re-readable, so
	// every input has to hold: a valid normalized post, media that is actually
	// servable, a completeness verdict that says nothing is missing, and the
	// provider's raw record still retrievable. Partial and unknown never pass.
	validPost := result.Post != nil && (result.Post.ID != "" || result.Post.URL != "" || result.Post.Title != "" || result.Post.Text != "")
	rawAvailable := len(result.RawMetadata) > 0
	result.Fulfilled = validPost &&
		rawAvailable &&
		len(result.Media) > 0 &&
		completeness.State == archivers.CompletenessComplete

	switch {
	case result.Fulfilled:
		result.Status = "fulfilled"
	case len(result.Media) > 0 || result.Post != nil:
		result.Status = "partial"
	default:
		result.Status = "failed"
	}

	result.Warnings = append(result.Warnings, socialCompletenessWarnings(completeness, validPost, rawAvailable)...)
	if !result.Fulfilled && result.Failure == nil {
		result.Failure = socialFulfillmentFailure(social, completeness, validPost, rawAvailable)
	}
	if !result.Fulfilled {
		result.Provenance.LastFailureReason = lastFailureReason(db, social)
	}
	return result
}

// selectSocialItem picks the archive item that represents this URL's social
// capture.
//
// A capture can hold more than one candidate: a URL that routes to both
// extractors, or a row still carrying the retired "youtube" type beside a newer
// yt-dlp one. Taking the first in load order made the answer depend on row
// ordering, which could surface a failed item while a completed one sat behind
// it. Prefer the type the URL actually routes to, then a completed item, then
// the most recently updated.
func selectSocialItem(items []models.ArchiveItem, sourceURL string) *models.ArchiveItem {
	preferred := preferredSocialType(sourceURL)
	var best *models.ArchiveItem
	bestRank := -1
	for i := range items {
		item := &items[i]
		if !utils.ArchiveTypesEqual(item.Type, utils.ArchiveTypeYtDlp) && !utils.ArchiveTypesEqual(item.Type, utils.ArchiveTypeGalleryDl) {
			continue
		}
		rank := 0
		if preferred != "" && utils.ArchiveTypesEqual(item.Type, preferred) {
			rank += 2
		}
		if item.Status == "completed" {
			rank++
		}
		if best == nil || rank > bestRank || (rank == bestRank && item.UpdatedAt.After(best.UpdatedAt)) {
			best, bestRank = item, rank
		}
	}
	return best
}

// preferredSocialType names the extractor this URL routes to. A URL matching
// both predicates is a video first: the video is the post's primary asset and
// the gallery item is the secondary capture.
func preferredSocialType(sourceURL string) string {
	if utils.IsVideoURL(sourceURL) {
		return utils.ArchiveTypeYtDlp
	}
	if utils.IsGalleryDLURL(sourceURL) {
		return utils.ArchiveTypeGalleryDl
	}
	return ""
}

// socialCompletenessFor reconciles the recorded verdict with what the API can
// actually serve.
//
// The stored column wins when it is set, because it is what the archiver
// observed at capture time. When it is empty the row predates completeness
// tracking, and only a video can be rescued from that: one yt-dlp item is one
// video, so an artifact stored with both sidecars is structurally whole. Every
// other legacy row is unknown — an old gallery bundle carries no evidence that
// it holds the entire post.
func socialCompletenessFor(item *models.ArchiveItem, fromArchive *archivers.Completeness, storedMedia int) socialCompleteness {
	out := socialCompleteness{Stored: storedMedia}
	if fromArchive != nil {
		out.Expected = fromArchive.Expected
		out.MissingIndices = fromArchive.MissingIndices
	}

	state := archivers.NormalizeCompletenessState(item.Completeness)
	if item.Completeness == "" {
		switch {
		case fromArchive != nil && fromArchive.State != "":
			state = archivers.NormalizeCompletenessState(fromArchive.State)
		case isStructurallyCompleteVideo(item):
			state = archivers.CompletenessComplete
		default:
			state = archivers.CompletenessUnknown
		}
	}
	if state == archivers.CompletenessComplete && out.Expected == nil &&
		utils.ArchiveTypesEqual(item.Type, utils.ArchiveTypeYtDlp) {
		one := 1
		out.Expected = &one
	}

	// The recorded verdict describes what the archiver wrote; this describes
	// what a caller can fetch today. If the artifact no longer yields what was
	// promised, the servable count is the honest answer.
	if out.Expected != nil && storedMedia < *out.Expected {
		state = archivers.CompletenessPartial
	} else if storedMedia == 0 && state == archivers.CompletenessComplete {
		state = archivers.CompletenessPartial
	}
	out.State = state
	return out
}

// isStructurallyCompleteVideo reports whether a legacy video row can be trusted
// as whole: --no-playlist caps a yt-dlp item at one video, so the artifact plus
// both sidecars is everything there was to store.
func isStructurallyCompleteVideo(item *models.ArchiveItem) bool {
	return utils.ArchiveTypesEqual(item.Type, utils.ArchiveTypeYtDlp) &&
		item.StorageKey != "" && item.MetadataKey != "" && item.RawMetadataKey != ""
}

func socialCompletenessWarnings(completeness socialCompleteness, validPost, rawAvailable bool) []socialWarning {
	var warnings []socialWarning
	switch completeness.State {
	case archivers.CompletenessPartial:
		warnings = append(warnings, socialWarning{Code: "media_incomplete", Message: describeIncompleteMedia(completeness)})
	case archivers.CompletenessUnknown:
		warnings = append(warnings, socialWarning{Code: "completeness_unknown", Message: "Nothing recorded how many media assets this post has, so this archive cannot be reported as complete"})
	}
	if !rawAvailable {
		warnings = append(warnings, socialWarning{Code: "raw_metadata_unavailable", Message: "The provider's raw metadata record is not stored or is no longer retrievable"})
	}
	if !validPost {
		warnings = append(warnings, socialWarning{Code: "metadata_unavailable", Message: "Structured social metadata is unavailable"})
	}
	return warnings
}

// socialFulfillmentFailure names the most specific reason a completed capture
// still cannot be called fulfilled.
func socialFulfillmentFailure(item *models.ArchiveItem, completeness socialCompleteness, validPost, rawAvailable bool) *socialFailure {
	// Archives captured before structured metadata existed keep their own
	// explicit code: nothing is missing from the download, the record simply
	// predates the contract.
	if item.MetadataKey == "" && utils.ArchiveTypesEqual(item.Type, utils.ArchiveTypeYtDlp) {
		return &socialFailure{Code: "legacy_archive", Message: "This archive predates structured social metadata", Retryable: false}
	}
	switch {
	case !validPost:
		return &socialFailure{Code: "metadata_unavailable", Message: "Structured social metadata is unavailable", Retryable: false}
	case completeness.State == archivers.CompletenessPartial:
		// Worth another capture: the missing assets may still be online.
		return &socialFailure{Code: "media_incomplete", Message: describeIncompleteMedia(completeness), Retryable: true}
	case completeness.State == archivers.CompletenessUnknown:
		// Re-running does not teach Arker the post's asset count, so this is
		// not retryable; it is a permanent statement about what is knowable.
		return &socialFailure{Code: "completeness_unknown", Message: "Nothing recorded how many media assets this post has, so this archive cannot be reported as complete", Retryable: false}
	case !rawAvailable:
		return &socialFailure{Code: "raw_metadata_unavailable", Message: "The provider's raw metadata record is not stored or is no longer retrievable", Retryable: false}
	}
	return &socialFailure{Code: "metadata_unavailable", Message: "Structured social metadata is unavailable", Retryable: false}
}

func describeIncompleteMedia(completeness socialCompleteness) string {
	if completeness.Expected == nil {
		return fmt.Sprintf("The extractor reported a failure after storing %d media asset(s); part of this post is missing", completeness.Stored)
	}
	message := fmt.Sprintf("%d of %d media assets from this post are stored", completeness.Stored, *completeness.Expected)
	if len(completeness.MissingIndices) > 0 {
		indices := make([]string, 0, len(completeness.MissingIndices))
		for _, index := range completeness.MissingIndices {
			indices = append(indices, strconv.Itoa(index))
		}
		message += "; missing " + strings.Join(indices, ", ")
	}
	return message
}

// failureReasonURL matches any URL in a log line. Signed CDN links carry
// credentials in their query string and archive logs are full of them, so every
// URL is replaced rather than trying to tell the safe ones apart. The source
// URL is already a top-level field, so nothing is lost.
var failureReasonURL = regexp.MustCompile(`(?i)\bhttps?://[^\s'"<>)]+`)

// failureReasonMarkers are the substrings that mark a log line as describing a
// failure. Best-effort by design: extractors have no structured error output,
// so this reads their human text.
var failureReasonMarkers = []string{"error", "failed", "failure", "timed out", "timeout", "denied", "refused", "unsupported", "unavailable"}

// failureReasonChunkLimit bounds how much of an archive log is read. Logs are
// stored as ~1KB chunks and a verbose yt-dlp run writes hundreds of them, but
// only the end is needed: reading the whole log on every poll of an unfinished
// archive would pull megabytes out of the database for one line of output.
const failureReasonChunkLimit = 24

// lastFailureReason returns the most recent failure line from an item's archive
// log, sanitized for external display. Returns empty when nothing matched,
// which means "not recorded" — never "the capture was clean".
func lastFailureReason(db *gorm.DB, item *models.ArchiveItem) string {
	if db == nil || item == nil {
		return ""
	}
	logs := recentArchiveLogTail(db, item)
	if logs == "" {
		return ""
	}
	lines := strings.Split(logs, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		for _, marker := range failureReasonMarkers {
			if strings.Contains(lower, marker) {
				return sanitizeFailureReason(line)
			}
		}
	}
	return ""
}

// recentArchiveLogTail returns the last few log chunks for an item, in order.
// Rows written before chunked logs existed keep their whole log in the item's
// own column, which is already loaded, so that needs no query at all.
func recentArchiveLogTail(db *gorm.DB, item *models.ArchiveItem) string {
	var chunks []string
	if err := db.Model(&models.ArchiveItemLog{}).
		Where("archive_item_id = ?", item.ID).
		Order("id DESC").Limit(failureReasonChunkLimit).
		Pluck("chunk", &chunks).Error; err != nil {
		return item.Logs
	}
	if len(chunks) == 0 {
		return item.Logs
	}
	var tail strings.Builder
	for i := len(chunks) - 1; i >= 0; i-- {
		tail.WriteString(chunks[i])
	}
	logs := tail.String()
	// A chunk boundary can fall mid-line, so when the window is full drop
	// everything before the first newline rather than risk reporting a
	// fragment that starts halfway through a word.
	if len(chunks) == failureReasonChunkLimit {
		if newline := strings.Index(logs, "\n"); newline >= 0 {
			logs = logs[newline+1:]
		}
	}
	return logs
}

func sanitizeFailureReason(line string) string {
	line = utils.RedactSecrets(line, utils.MediaProxyRedactionSecrets())
	line = failureReasonURL.ReplaceAllString(line, "[url]")
	return utils.TruncateForLog(strings.Join(strings.Fields(line), " "), 300)
}

// fallbackOps summarizes the paid fallback work recorded against one item.
// Rows are few (an actor run per attempt, or a scrape plus a download), so
// they are aggregated in Go rather than in SQL: it keeps operation ID
// collection dialect-independent and the ordering deterministic.
func fallbackOps(db *gorm.DB, itemID uint) []socialFallbackOp {
	if db == nil || itemID == 0 {
		return nil
	}
	var rows []models.FallbackUsage
	if err := db.Where("archive_item_id = ?", itemID).Order("id").Limit(200).Find(&rows).Error; err != nil || len(rows) == 0 {
		return nil
	}
	byProduct := make(map[string]*socialFallbackOp, len(rows))
	seenSnapshot := make(map[string]bool, len(rows))
	for _, row := range rows {
		provider := row.Provider
		if provider == "" {
			provider = models.FallbackProviderBrightData
		}
		key := provider + "\x00" + row.Product
		op, ok := byProduct[key]
		if !ok {
			op = &socialFallbackOp{Provider: provider, Product: row.Product}
			byProduct[key] = op
		}
		op.Operations++
		if row.Success {
			op.Successes++
		}
		op.Records += int64(row.Records)
		op.BytesTransferred += row.BytesTransferred
		// Failed operations are billable too, so their cost counts.
		op.EstimatedCostUSD += row.CostUSD
		if row.OperationID != "" && !seenSnapshot[key+"\x00"+row.OperationID] {
			seenSnapshot[key+"\x00"+row.OperationID] = true
			op.OperationIDs = append(op.OperationIDs, row.OperationID)
			op.SnapshotIDs = op.OperationIDs
		}
	}
	ops := make([]socialFallbackOp, 0, len(byProduct))
	for _, op := range byProduct {
		ops = append(ops, *op)
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Provider != ops[j].Provider {
			return ops[i].Provider < ops[j].Provider
		}
		return ops[i].Product < ops[j].Product
	})
	return ops
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
	out.Post = &normalizedPost{ID: meta.PostID, URL: meta.CanonicalURL, Title: meta.Title, Text: meta.Description, PublishedAt: meta.PublicationTimestamp, DurationSeconds: meta.DurationSeconds, MediaType: meta.MediaType, Author: author, Engagement: eng, Tags: meta.Tags}
	if len(out.Media) > 0 {
		out.Media[0].ContentType = meta.Media.ContentType
		out.Media[0].SizeBytes = meta.Media.SizeBytes
		out.Media[0].Width = meta.Media.Width
		out.Media[0].Height = meta.Media.Height
		out.Media[0].DurationSeconds = meta.DurationSeconds
		out.Media[0].Quality = meta.Media.QualityLabel
	}
	for _, track := range meta.Subtitles {
		if track.Lang == "" {
			continue
		}
		out.Subtitles = append(out.Subtitles, socialSubtitle{
			Lang: track.Lang, Kind: track.Kind, Format: track.Format, SizeBytes: track.SizeBytes,
			URL: fullPath(c, fmt.Sprintf("video/%s/subtitle/%s", shortID, url.PathEscape(subtitleRequestName(track)))),
		})
	}
	if meta.Transcript != nil && meta.Transcript.Text != "" {
		out.Transcript = &socialTranscript{
			Lang: meta.Transcript.Lang, Source: meta.Transcript.Source,
			Characters: len([]rune(meta.Transcript.Text)), Truncated: meta.Transcript.Truncated,
			Text: meta.Transcript.Text,
			URL:  fullPath(c, fmt.Sprintf("video/%s/transcript", shortID)),
		}
	}

	// Fulfilled promises the raw provider record is still there, so check the
	// object rather than trusting the key column. A storage error counts as not
	// retrievable: the claim has to be provable, and failing closed only costs
	// a warning.
	if item.RawMetadataKey != "" && objectRetrievable(store, item.RawMetadataKey) {
		provider := meta.Provider
		if provider == "" {
			provider = "yt-dlp"
		}
		out.RawMetadata = append(out.RawMetadata, rawMetadataLink{provider, fullPath(c, fmt.Sprintf("video/%s/raw", shortID))})
	}
}

func objectRetrievable(store storage.Storage, key string) bool {
	if store == nil || key == "" {
		return false
	}
	exists, err := store.Exists(key)
	return err == nil && exists
}

// buildGallerySocial fills in the post, its media and its raw metadata links,
// and returns the completeness verdict the archiver wrote into the bundle. A
// nil return means the bundle predates completeness tracking or could not be
// opened, which the caller resolves as unknown.
func buildGallerySocial(c *gin.Context, store storage.Storage, shortID string, item *models.ArchiveItem, out *socialPostResult) *archivers.Completeness {
	zipReader, cleanup, ok := openGalleryZipData(store, item)
	if !ok {
		return nil
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
	if meta.Likes != nil || meta.Views != nil || meta.Comments != nil {
		eng = &socialEngagement{Views: meta.Views, Likes: meta.Likes, Comments: meta.Comments}
	}
	out.Post = &normalizedPost{ID: meta.PostID, URL: meta.PostURL, Title: meta.Title, Text: meta.Description, PublishedAt: meta.Date, Author: author, Engagement: eng, Tags: meta.Tags}
	fileMetadata := make(map[string]archivers.GalleryFile, len(meta.Files))
	for _, f := range meta.Files {
		fileMetadata[f.Name] = f
	}
	rawAvailable := false
	var audioEntry *zip.File
	for _, f := range zipReader.File {
		if f.Name == galleryMetadataFilename || strings.HasSuffix(f.Name, ".json") {
			if f.Name != galleryMetadataFilename {
				rawAvailable = true
			}
			continue
		}
		if archivers.GalleryAudioFilename(f.Name) {
			audioEntry = f
			continue
		}
		ct := galleryZipFileContentType(f)
		media := normalizedMedia{Index: len(out.Media), Type: galleryMediaKind(ct), URL: fullPath(c, fmt.Sprintf("gallery/%s/file/%s", shortID, url.PathEscape(f.Name))), Filename: f.Name, ContentType: ct, SizeBytes: int64(f.UncompressedSize64)}
		if details, ok := fileMetadata[f.Name]; ok {
			if details.Width > 0 {
				width := int64(details.Width)
				media.Width = &width
			}
			if details.Height > 0 {
				height := int64(details.Height)
				media.Height = &height
			}
			if details.DurationSeconds != nil && *details.DurationSeconds > 0 {
				duration := *details.DurationSeconds
				media.DurationSeconds = &duration
			}
			media.AltText = details.AltText
		}
		out.Media = append(out.Media, media)
	}
	out.Music = galleryManifestMusicFor(c, shortID, meta.Music, audioEntry)
	bundle := fullPath(c, fmt.Sprintf("archive/%s/%s", shortID, utils.ArchiveTypeGalleryDl))
	out.BundleURL = &bundle
	provider := "gallery-dl"
	for _, f := range zipReader.File {
		if p := fallbackRawRecordProvider(f.Name); p != "" {
			provider = p
			break
		}
	}
	if rawAvailable {
		out.RawMetadata = append(out.RawMetadata, rawMetadataLink{provider, fullPath(c, fmt.Sprintf("gallery/%s/raw", shortID))})
	}
	return meta.Completeness
}

func fullPath(c *gin.Context, path string) string {
	return utils.BuildFullURL(c, strings.TrimPrefix(path, "/"))
}
