package archivers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"arker/internal/utils"
)

// VideoMetadataSchemaVersion is the public normalized video/post metadata
// contract. Additive fields do not require a new version; incompatible meaning
// or shape changes do.
const VideoMetadataSchemaVersion = "1"

// VideoMetadata is Arker's provider-neutral description of an archived video.
// The provider's sanitized raw record is stored separately so this shape can
// remain stable when yt-dlp or an upstream API changes.
type VideoMetadata struct {
	SchemaVersion        string   `json:"schema_version"`
	SourceURL            string   `json:"source_url"`
	Platform             string   `json:"platform,omitempty"`
	Extractor            string   `json:"extractor,omitempty"`
	PostID               string   `json:"post_id,omitempty"`
	CanonicalURL         string   `json:"canonical_url,omitempty"`
	Title                string   `json:"title,omitempty"`
	Description          string   `json:"description,omitempty"`
	Author               string   `json:"author,omitempty"`
	AuthorID             string   `json:"author_id,omitempty"`
	Uploader             string   `json:"uploader,omitempty"`
	UploaderID           string   `json:"uploader_id,omitempty"`
	Channel              string   `json:"channel,omitempty"`
	ChannelID            string   `json:"channel_id,omitempty"`
	PublicationTimestamp string   `json:"publication_timestamp,omitempty"`
	DurationSeconds      *float64 `json:"duration_seconds,omitempty"`
	// MediaType is the platform's own name for the delivery format of the
	// post — "short" for a YouTube Short, "video" for an ordinary one. It is a
	// passthrough of the provider's term rather than a vocabulary of Arker's
	// invention, so it always matches what /video/:shortid/raw reports.
	//
	// It is absent, never guessed, when the provider names no delivery format
	// (Instagram, TikTok and Facebook records carry none) and on archives
	// captured before this field existed. Duration and aspect ratio are not a
	// substitute: a 60-second 9:16 upload can be either.
	MediaType  string          `json:"media_type,omitempty"`
	Engagement VideoEngagement `json:"engagement"`
	Tags       []string        `json:"tags,omitempty"`
	// Music is the track behind the video when the platform names one (a
	// TikTok's sound, a Reel's audio). The audio is already inside the video
	// file, so this is attribution, not a second asset.
	Music *VideoMusic `json:"music,omitempty"`
	Media VideoMedia  `json:"media"`
	// Subtitles lists the stored caption tracks, and Transcript is the readable
	// text derived from the best of them. Both are additive and both are
	// absent for most posts: a platform that exposes no captions is a fact
	// about the post, not a failure of the capture, and must never affect
	// whether the archive reads fulfilled.
	Subtitles    []SubtitleTrack `json:"subtitles,omitempty"`
	Transcript   *Transcript     `json:"transcript,omitempty"`
	YtDlpVersion string          `json:"yt_dlp_version,omitempty"`
	ArchivedAt   string          `json:"archived_at"`
	Provenance   string          `json:"provenance"`
	Provider     string          `json:"provider,omitempty"`
}

// VideoMusic is the attribution of a video's soundtrack.
type VideoMusic struct {
	Title    string `json:"title,omitempty"`
	Artist   string `json:"artist,omitempty"`
	Album    string `json:"album,omitempty"`
	ID       string `json:"id,omitempty"`
	Original *bool  `json:"original,omitempty"`
}

// videoMusicFromYtDlp reads yt-dlp's track/artist(s)/album fields. TikTok's
// extractor fills them from the post's music record; most other extractors
// leave them empty, in which case the post has no declared soundtrack.
func videoMusicFromYtDlp(raw map[string]interface{}) *VideoMusic {
	music := &VideoMusic{
		Title:  videoString(raw, "track"),
		Artist: firstVideoString(strings.Join(videoStrings(raw, "artists"), ", "), videoString(raw, "artist", "creator")),
		Album:  videoString(raw, "album"),
	}
	if music.Title == "" {
		return nil
	}
	return music
}

// VideoEngagement holds counts without treating a missing value as zero.
type VideoEngagement struct {
	Views    *int64 `json:"views,omitempty"`
	Likes    *int64 `json:"likes,omitempty"`
	Comments *int64 `json:"comments,omitempty"`
	Reposts  *int64 `json:"reposts,omitempty"`
}

// VideoMedia describes the stored media file, plus provider format details
// when they are available.
type VideoMedia struct {
	FormatID     string   `json:"format_id,omitempty"`
	Format       string   `json:"format,omitempty"`
	Extension    string   `json:"extension"`
	ContentType  string   `json:"content_type"`
	SizeBytes    int64    `json:"size_bytes"`
	Width        *int64   `json:"width,omitempty"`
	Height       *int64   `json:"height,omitempty"`
	FPS          *float64 `json:"fps,omitempty"`
	VideoCodec   string   `json:"video_codec,omitempty"`
	AudioCodec   string   `json:"audio_codec,omitempty"`
	BitrateKbps  *float64 `json:"bitrate_kbps,omitempty"`
	QualityLabel string   `json:"quality_label,omitempty"`
}

// BuildYtDlpVideoArtifacts normalizes yt-dlp's full info JSON and returns the
// sanitized raw JSON that is safe to persist and expose. The caller supplies
// the final, post-processed media facts because yt-dlp's info record can refer
// to a pre-remux format or estimated size.
func BuildYtDlpVideoArtifacts(rawJSON []byte, sourceURL, toolVersion string, media VideoMedia, archivedAt time.Time) (*VideoMetadata, []byte, error) {
	raw, err := decodeJSONObject(rawJSON)
	if err != nil {
		return nil, nil, fmt.Errorf("decode yt-dlp info JSON: %w", err)
	}

	if media.Extension == "" {
		media.Extension = ".mp4"
	}
	if media.ContentType == "" {
		media.ContentType = "video/mp4"
	}
	media.FormatID = firstVideoString(media.FormatID, videoString(raw, "format_id"))
	media.Format = firstVideoString(media.Format, videoString(raw, "format"))
	media.Width = firstVideoInt(media.Width, videoInt(raw, "width"))
	media.Height = firstVideoInt(media.Height, videoInt(raw, "height"))
	media.FPS = firstVideoFloat(media.FPS, videoFloat(raw, "fps"))
	media.VideoCodec = firstVideoString(media.VideoCodec, videoString(raw, "vcodec"))
	media.AudioCodec = firstVideoString(media.AudioCodec, videoString(raw, "acodec"))
	media.BitrateKbps = firstVideoFloat(media.BitrateKbps, videoFloat(raw, "tbr"))
	media.QualityLabel = firstVideoString(media.QualityLabel, videoString(raw, "resolution", "format_note"))

	extractor := videoString(raw, "extractor")
	platform := strings.ToLower(videoString(raw, "extractor_key"))
	if platform == "" {
		platform = strings.ToLower(strings.Split(extractor, ":")[0])
	}
	if extractor == "" {
		extractor = platform
	}

	redactionSecrets := utils.YtDlpProxyRedactionSecrets()
	safeSourceURL := SanitizeURL(sourceURL, redactionSecrets)
	metadata := &VideoMetadata{
		SchemaVersion:        VideoMetadataSchemaVersion,
		SourceURL:            safeSourceURL,
		Platform:             platform,
		Extractor:            extractor,
		PostID:               videoString(raw, "id", "display_id"),
		CanonicalURL:         SanitizeURL(firstVideoString(videoString(raw, "webpage_url", "original_url"), safeSourceURL), redactionSecrets),
		Title:                videoString(raw, "title", "fulltitle"),
		Description:          videoString(raw, "description", "caption"),
		Author:               videoString(raw, "creator", "artist"),
		AuthorID:             videoString(raw, "creator_id", "artist_id"),
		Uploader:             videoString(raw, "uploader"),
		UploaderID:           videoString(raw, "uploader_id"),
		Channel:              videoString(raw, "channel"),
		ChannelID:            videoString(raw, "channel_id"),
		PublicationTimestamp: videoPublicationTimestamp(raw),
		DurationSeconds:      videoFloat(raw, "duration"),
		MediaType:            videoString(raw, "media_type"),
		Engagement: VideoEngagement{
			Views:    videoInt(raw, "view_count"),
			Likes:    videoInt(raw, "like_count"),
			Comments: videoInt(raw, "comment_count"),
			Reposts:  videoInt(raw, "repost_count"),
		},
		Tags:         videoStrings(raw, "tags", "categories"),
		Music:        videoMusicFromYtDlp(raw),
		Media:        media,
		YtDlpVersion: toolVersion,
		ArchivedAt:   archivedAt.UTC().Format(time.RFC3339),
		Provenance:   "native",
		Provider:     "yt-dlp",
	}
	if metadata.Author == "" {
		metadata.Author = firstVideoString(metadata.Uploader, metadata.Channel)
	}
	if metadata.AuthorID == "" {
		metadata.AuthorID = firstVideoString(metadata.UploaderID, metadata.ChannelID)
	}

	sanitized, err := SanitizeJSON(rawJSON, redactionSecrets)
	if err != nil {
		return nil, nil, fmt.Errorf("sanitize yt-dlp info JSON: %w", err)
	}
	return metadata, sanitized, nil
}

// SanitizeURL strips embedded credentials and redacts sensitive query
// parameters before a URL is stored in normalized metadata.
func SanitizeURL(rawURL string, secrets []string) string {
	return sanitizeJSONString(rawURL, secrets)
}

// MarshalVideoMetadata encodes the normalized contract consistently for all
// native and fallback video paths.
func MarshalVideoMetadata(metadata *VideoMetadata) ([]byte, error) {
	if metadata == nil {
		return nil, fmt.Errorf("video metadata is nil")
	}
	return json.MarshalIndent(metadata, "", "  ")
}

// SetSubtitleStorageKeys records where each subtitle track was actually stored.
//
// The archiver knows a track's language and its key suffix, but only the worker
// knows the item's key base, so the sidecar is finished after the extras are
// written. Tracks are matched by suffix; a metadata record with no subtitles is
// returned untouched.
// A track whose artifact did not make it into storage is dropped rather than
// left pointing at nothing: the metadata has to describe what the archive
// actually holds, and a listed-but-absent track is exactly the kind of quiet
// lie this work exists to remove.
func SetSubtitleStorageKeys(metadataJSON []byte, keysBySuffix map[string]string) ([]byte, error) {
	var metadata VideoMetadata
	if err := json.Unmarshal(metadataJSON, &metadata); err != nil {
		// Not a video record (a gallery bundle has no subtitles), so there is
		// nothing to reconcile.
		return metadataJSON, nil
	}
	if len(metadata.Subtitles) == 0 {
		return metadataJSON, nil
	}

	stored := metadata.Subtitles[:0]
	for _, track := range metadata.Subtitles {
		key, ok := keysBySuffix[track.ArtifactSuffix]
		if !ok {
			continue
		}
		track.StorageKey = key
		stored = append(stored, track)
	}
	metadata.Subtitles = stored
	// The transcript is derived from a track; without any stored track there is
	// nothing left for a reader to check it against.
	if len(metadata.Subtitles) == 0 {
		metadata.Subtitles = nil
		metadata.Transcript = nil
	}
	return MarshalVideoMetadata(&metadata)
}

// SanitizeJSON recursively redacts sensitive keys and credential-bearing URL
// components before a provider record is persisted or exposed by the API.
func SanitizeJSON(rawJSON []byte, secrets []string) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(rawJSON))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	sanitized := sanitizeJSONValue(value, secrets)
	return json.MarshalIndent(sanitized, "", "  ")
}

func sanitizeJSONValue(value interface{}, secrets []string) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			if sensitiveJSONKey(key) {
				result[key] = utils.RedactPlaceholder
				continue
			}
			result[key] = sanitizeJSONValue(child, secrets)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(typed))
		for i, child := range typed {
			result[i] = sanitizeJSONValue(child, secrets)
		}
		return result
	case string:
		return sanitizeJSONString(typed, secrets)
	default:
		return value
	}
}

func sensitiveJSONKey(key string) bool {
	normalized := strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(strings.ToLower(key))
	for _, fragment := range []string{
		"authorization", "cookie", "password", "passwd", "proxy", "credential", "secret",
		"api_key", "apikey", "access_token", "refresh_token", "session_token", "session_id", "visitor_data", "visitor_id", "visitordata", "visitorid",
	} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return normalized == "token" || strings.HasSuffix(normalized, "_token") || strings.HasPrefix(normalized, "token_")
}

func sanitizeJSONString(value string, secrets []string) string {
	value = utils.RedactSecrets(value, secrets)
	if value == utils.RedactPlaceholder {
		return value
	}

	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return value
	}
	parsed.User = nil
	query := parsed.Query()
	changed := false
	for key := range query {
		if signedMediaHost(parsed.Hostname()) || sensitiveURLParameter(key) {
			query.Set(key, utils.RedactPlaceholder)
			changed = true
		}
	}
	if changed {
		parsed.RawQuery = query.Encode()
	}

	// Signed media hosts do not only sign via the query string. googlevideo's
	// HLS manifest URLs carry expire/ei/ip/sig/lsig as ALTERNATING PATH
	// SEGMENTS (/expire/1786.../ei/czt9.../ip/198.51.100.23/.../sig/AE0s...),
	// so a query-only pass stores the viewer's client IP and a live URL
	// signature in raw metadata (G14). Walk the path pairwise and redact the
	// value after any segment that names a sensitive parameter. Scoped to
	// signed media hosts so ordinary URLs never have their paths rewritten.
	if signedMediaHost(parsed.Hostname()) {
		segments := strings.Split(parsed.EscapedPath(), "/")
		pathChanged := false
		for i := 0; i+1 < len(segments); i++ {
			if segments[i] != "" && sensitiveURLParameter(segments[i]) && segments[i+1] != "" {
				segments[i+1] = utils.RedactPlaceholder
				pathChanged = true
				i++ // the redacted value cannot itself be a key
			}
		}
		if pathChanged {
			escaped := strings.Join(segments, "/")
			if unescaped, err := url.PathUnescape(escaped); err == nil {
				parsed.Path = unescaped
				parsed.RawPath = escaped
			} else {
				parsed.Path = escaped
				parsed.RawPath = ""
			}
		}
	}
	return parsed.String()
}

// signedMediaHostSuffixes are the media CDNs whose URLs carry expiring
// credentials. Every query parameter on these hosts is redacted before a raw
// record is stored, because the parameter names are provider-specific and
// undocumented: Reddit signs with s=/e=/v=, TikTok with policy/signature/tk/l.
// Guessing which of those is the secret is exactly the kind of judgement that
// leaves one credential at rest, so on a signed host none of them survive.
var signedMediaHostSuffixes = []string{
	"googlevideo.com",
	"cdninstagram.com",
	"fbcdn.net",
	// Reddit: packaged-media/v/preview/i.redd.it plus its styles host.
	"redd.it",
	"redditmedia.com",
	// TikTok's image and video CDNs.
	"tiktokcdn.com",
	"tiktokcdn-us.com",
	"tiktokv.com",
}

// tiktokMediaSubdomain matches TikTok's regional media hosts
// (v16-webapp-prime.us.tiktok.com, p16-common-sign.us.tiktok.com), which sign
// like the CDNs above but share the tiktok.com domain with ordinary post URLs.
// Post URLs must keep their query strings, so the match is on the media
// subdomain shape rather than the domain.
var tiktokMediaSubdomain = regexp.MustCompile(`^[vp]\d+[a-z0-9-]*\.`)

func signedMediaHost(host string) bool {
	host = strings.ToLower(host)
	for _, suffix := range signedMediaHostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return strings.HasSuffix(host, ".tiktok.com") && tiktokMediaSubdomain.MatchString(host)
}

func sensitiveURLParameter(key string) bool {
	normalized := strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(key))
	for _, fragment := range []string{
		"signature", "credential", "security_token", "authorization", "password", "passwd", "cookie", "policy",
		"key_pair", "token", "expire", "hmac", "jwt",
	} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	if normalized == "ip" || strings.HasSuffix(normalized, "_ip") {
		return true
	}
	switch normalized {
	case "sig", "lsig", "auth", "api_key", "apikey", "key", "ipbits", "hdntl", "hdnea", "hdnts", "pot",
		// Path-segment session tokens on signed media hosts (G14): "ei" is
		// googlevideo's per-viewer session identifier; spc/vprv/bui are opaque
		// per-session blobs riding the same manifest paths.
		"ei", "spc", "vprv", "bui":
		return true
	default:
		return false
	}
}

func decodeJSONObject(rawJSON []byte) (map[string]interface{}, error) {
	decoder := json.NewDecoder(bytes.NewReader(rawJSON))
	decoder.UseNumber()
	var value map[string]interface{}
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("JSON value is not an object")
	}
	return value, nil
}

func videoString(raw map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		switch value := raw[key].(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		case json.Number:
			return value.String()
		}
	}
	return ""
}

func videoFloat(raw map[string]interface{}, keys ...string) *float64 {
	for _, key := range keys {
		switch value := raw[key].(type) {
		case json.Number:
			if number, err := value.Float64(); err == nil {
				return &number
			}
		case float64:
			number := value
			return &number
		case string:
			if number, err := strconv.ParseFloat(value, 64); err == nil {
				return &number
			}
		}
	}
	return nil
}

func videoInt(raw map[string]interface{}, keys ...string) *int64 {
	for _, key := range keys {
		switch value := raw[key].(type) {
		case json.Number:
			if number, err := value.Int64(); err == nil {
				return &number
			}
		case float64:
			number := int64(value)
			return &number
		case string:
			if number, err := strconv.ParseInt(value, 10, 64); err == nil {
				return &number
			}
		}
	}
	return nil
}

func videoStrings(raw map[string]interface{}, keys ...string) []string {
	for _, key := range keys {
		values, ok := raw[key].([]interface{})
		if !ok {
			continue
		}
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				result = append(result, strings.TrimSpace(text))
			}
		}
		if len(result) > 0 {
			return result
		}
	}
	return nil
}

func videoPublicationTimestamp(raw map[string]interface{}) string {
	for _, key := range []string{"timestamp", "release_timestamp"} {
		if seconds := videoInt(raw, key); seconds != nil && *seconds > 0 {
			return time.Unix(*seconds, 0).UTC().Format(time.RFC3339)
		}
	}
	for _, key := range []string{"upload_date", "release_date"} {
		value := videoString(raw, key)
		for _, layout := range []string{"20060102", "2006-01-02", time.RFC3339} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed.UTC().Format(time.RFC3339)
			}
		}
	}
	return ""
}

func firstVideoString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstVideoInt(values ...*int64) *int64 {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstVideoFloat(values ...*float64) *float64 {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
