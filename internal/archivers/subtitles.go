package archivers

import (
	"html"
	"path/filepath"
	"regexp"
	"strings"
)

// Subtitle kinds. A manual track was written or reviewed by a person; an
// automatic one is the platform's speech recognition. The difference matters to
// anyone quoting an archive, so it is recorded rather than flattened.
const (
	SubtitleKindManual = "manual"
	SubtitleKindAuto   = "auto"
)

// MaxTranscriptBytes caps the derived plain-text transcript. A multi-hour
// stream can produce megabytes of captions, and the transcript is embedded in
// the normalized metadata sidecar and the API response; the subtitle artifact
// itself is always stored in full.
const MaxTranscriptBytes = 1 << 20

// SubtitleTrack describes one stored subtitle artifact.
type SubtitleTrack struct {
	Lang   string `json:"lang"`
	Kind   string `json:"kind"`
	Format string `json:"format"`
	// ArtifactSuffix is appended to the archive item's storage key base to form
	// this track's own key. It is recorded so a reader can locate the track
	// without guessing at the naming scheme.
	ArtifactSuffix string `json:"artifact_suffix"`
	StorageKey     string `json:"storage_key,omitempty"`
	SizeBytes      int64  `json:"size_bytes"`
}

// Transcript is the readable text derived from the best available subtitle
// track. It is derived, not authoritative: the stored subtitle artifact is the
// primary record and keeps its timings.
type Transcript struct {
	Lang   string `json:"lang"`
	Source string `json:"source"`
	Text   string `json:"text"`
	// Truncated reports that the text was cut at MaxTranscriptBytes, so a
	// caller never mistakes a clipped transcript for a complete one.
	Truncated bool `json:"truncated,omitempty"`
}

// subtitleTagPattern strips VTT inline markup. Auto-captions interleave
// per-word timing tags (<00:00:01.290>) and cue spans (<c>, </c>) through the
// text, which are meaningless once the timings are gone.
var subtitleTagPattern = regexp.MustCompile(`<[^>]*>`)

// cueSettingsPattern strips the positioning settings that follow a timing line.
var cueTimingPattern = regexp.MustCompile(`-->`)

// SubtitleLangFromFilename reads the language code out of a subtitle filename
// written by yt-dlp, which names tracks "<base>.<lang>.<ext>".
func SubtitleLangFromFilename(path, base string) string {
	name := filepath.Base(path)
	name = strings.TrimPrefix(name, filepath.Base(base))
	name = strings.TrimPrefix(name, ".")
	// Drop the extension, leaving the language code.
	if dot := strings.LastIndex(name, "."); dot > 0 {
		name = name[:dot]
	}
	return name
}

// TranscriptFromVTT turns a WebVTT track into readable plain text.
//
// The work is in the repetition. YouTube's automatic captions are a rolling
// two-line display: each cue repeats the tail of the previous one and appends a
// few new words, so a naive concatenation says everything two or three times.
// Consecutive duplicate lines are dropped, and a line that merely extends the
// previous one replaces it, which is what turns the rolling window back into a
// sentence.
// Returns the text and whether it was cut at MaxTranscriptBytes.
func TranscriptFromVTT(vtt string) (string, bool) {
	var lines []string
	var builder strings.Builder
	truncated := false

	for _, rawLine := range strings.Split(strings.ReplaceAll(vtt, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || cueTimingPattern.MatchString(line) {
			continue
		}
		// Header and block markers carry no speech.
		if strings.HasPrefix(line, "WEBVTT") || strings.HasPrefix(line, "NOTE") ||
			strings.HasPrefix(line, "STYLE") || strings.HasPrefix(line, "REGION") {
			continue
		}
		if isVTTMetadataLine(line) {
			continue
		}

		line = subtitleTagPattern.ReplaceAllString(line, "")
		line = html.UnescapeString(line)
		line = strings.TrimSpace(strings.Join(strings.Fields(line), " "))
		if line == "" {
			continue
		}

		if len(lines) > 0 {
			previous := lines[len(lines)-1]
			if line == previous {
				continue
			}
			// The rolling window: the same words again plus a few more.
			if strings.HasPrefix(line, previous) {
				lines[len(lines)-1] = line
				continue
			}
		}
		lines = append(lines, line)
	}

	for i, line := range lines {
		if builder.Len()+len(line)+1 > MaxTranscriptBytes {
			truncated = true
			break
		}
		if i > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(line)
	}
	return builder.String(), truncated
}

// isVTTMetadataLine matches the header fields YouTube writes above the first
// cue ("Kind: captions", "Language: en") and bare numeric cue identifiers.
func isVTTMetadataLine(line string) bool {
	for _, prefix := range []string{"Kind:", "Language:", "X-TIMESTAMP-MAP"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	if len(line) > 8 {
		return false
	}
	for _, r := range line {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(line) > 0
}

// BuildTranscript derives the readable transcript from the best available
// track: the video's own language when it was captured, then English, then
// whatever else there is. A manual track always beats an automatic one for the
// same language because it is the more faithful record.
func BuildTranscript(tracks []SubtitleTrack, contents map[string]string, preferredLang string) *Transcript {
	best := -1
	bestScore := -1
	for i, track := range tracks {
		if !strings.EqualFold(track.Format, "vtt") {
			continue
		}
		score := 0
		if preferredLang != "" && languageMatches(track.Lang, preferredLang) {
			score += 4
		}
		if languageMatches(track.Lang, "en") {
			score += 2
		}
		if track.Kind == SubtitleKindManual {
			score++
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return nil
	}

	text, truncated := TranscriptFromVTT(contents[tracks[best].ArtifactSuffix])
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return &Transcript{Lang: tracks[best].Lang, Source: tracks[best].Kind, Text: text, Truncated: truncated}
}

// languageMatches compares codes by their base language, so "en-US" satisfies a
// request for "en".
func languageMatches(code, want string) bool {
	code, _, _ = strings.Cut(strings.ToLower(code), "-")
	want, _, _ = strings.Cut(strings.ToLower(want), "-")
	return code != "" && code == want
}

// subtitleKindSources lists the info-record fields that name caption tracks, in
// increasing order of authority. Order is the point: a language can appear in
// both maps, and yt-dlp writes the manual track when it does, so manual must be
// applied last. Ranging over a Go map here would decide it at random and make
// the same archive report different provenance on different runs.
var subtitleKindSources = []struct {
	field string
	kind  string
}{
	{"automatic_captions", SubtitleKindAuto},
	{"subtitles", SubtitleKindManual},
}

// SubtitleArtifactSuffix names a track's stored object relative to the archive
// item's key base. Manual and automatic tracks for one language cannot collide
// because yt-dlp writes only one file per language, preferring the manual one.
func SubtitleArtifactSuffix(lang, format string) string {
	if format == "" {
		format = "vtt"
	}
	return ".sub." + lang + "." + format
}
