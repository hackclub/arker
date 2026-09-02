package archivers

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// BuildArchivedProbeVideoArtifacts recovers the narrow facts printed by the
// successful yt-dlp accessibility probe that preceded an old media download.
// Those immutable capture logs are a last-resort historical source when the
// sibling MHTML and both current metadata providers can no longer see a
// deleted post. Diagnostic output is never copied into the sidecar.
func BuildArchivedProbeVideoArtifacts(logData, sourceURL string, media VideoMedia, archivedAt time.Time) (*Sidecar, *Sidecar, error) {
	title, channel, duration := archivedProbeVideoFacts(logData)
	factCount := 0
	for _, present := range []bool{title != "", channel != "", duration != nil} {
		if present {
			factCount++
		}
	}
	if factCount < 2 {
		return nil, nil, fmt.Errorf("capture log contains fewer than two usable video facts")
	}
	if media.Extension == "" {
		media.Extension = ".mp4"
	}
	if media.ContentType == "" {
		media.ContentType = "video/mp4"
	}
	safeURL := SanitizeURL(sourceURL, nil)
	metadataJSON, err := MarshalVideoMetadata(&VideoMetadata{
		SchemaVersion:   VideoMetadataSchemaVersion,
		SourceURL:       safeURL,
		Platform:        capturedVideoPlatform(sourceURL),
		Extractor:       "yt-dlp_probe_log",
		CanonicalURL:    safeURL,
		Title:           title,
		Author:          channel,
		Uploader:        channel,
		Channel:         channel,
		DurationSeconds: duration,
		Media:           media,
		ArchivedAt:      archivedAt.UTC().Format(time.RFC3339),
		Provenance:      "native",
		Provider:        "captured_probe_log",
	})
	if err != nil {
		return nil, nil, err
	}
	rawJSON, err := json.MarshalIndent(map[string]interface{}{
		"capture_source":   "yt-dlp_probe_log",
		"title":            title,
		"channel":          channel,
		"duration_seconds": duration,
	}, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("encode captured probe facts: %w", err)
	}
	return &Sidecar{Data: metadataJSON}, &Sidecar{Data: rawJSON}, nil
}

func archivedProbeVideoFacts(logData string) (string, string, *float64) {
	const marker = "Video info:"
	var bestTitle, bestChannel string
	var bestDuration *float64
	for searchAt := 0; searchAt < len(logData); {
		relative := strings.Index(logData[searchAt:], marker)
		if relative < 0 {
			break
		}
		start := searchAt + relative + len(marker)
		searchAt = start
		block := logData[start:]
		if next := strings.Index(block, marker); next >= 0 {
			block = block[:next]
		}
		lines := strings.Split(strings.ReplaceAll(block, "\r\n", "\n"), "\n")
		sawProbeLine := false
		for durationIndex, line := range lines {
			if durationIndex == 0 {
				continue
			}
			line = strings.TrimSpace(line)
			if line == "" {
				if sawProbeLine {
					break
				}
				continue
			}
			if archivedProbeTerminator(line) {
				break
			}
			sawProbeLine = true
			seconds, err := strconv.ParseFloat(line, 64)
			if err != nil || seconds <= 0 {
				continue
			}
			title := cleanArchivedProbeValue(lines[durationIndex-1])
			if title == "" || archivedProbeDiagnostic(title) {
				continue
			}
			channel := ""
			for _, candidate := range lines[durationIndex+1:] {
				candidate = cleanArchivedProbeValue(candidate)
				if candidate == "" {
					continue
				}
				if archivedProbeDiagnostic(candidate) {
					break
				}
				channel = candidate
				break
			}
			value := seconds
			bestTitle, bestChannel, bestDuration = title, channel, &value
			break
		}
	}
	return bestTitle, bestChannel, bestDuration
}

func archivedProbeTerminator(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{"starting yt-dlp", "video download", "failed to", "successfully ", "testing video"} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func cleanArchivedProbeValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 4096 {
		value = value[:4096]
	}
	switch strings.ToLower(value) {
	case "", "na", "n/a", "none", "null", "unknown":
		return ""
	default:
		return value
	}
}

func archivedProbeDiagnostic(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{"warning:", "error:", "[debug]", "[warning]", "starting ", "testing ", "yt-dlp "} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
