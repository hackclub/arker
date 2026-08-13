package utils

import (
	"regexp"
	"strings"
	"sync"
)

var (
	subtitleLangsMu sync.RWMutex
	subtitleLangs   string
)

// englishSubtitleLangs are the codes worth requesting for English.
//
// These are exact codes, not patterns, and that is the whole point. yt-dlp
// matches each --sub-langs entry as an anchored regex against the available
// track codes, and YouTube names machine-translated auto-caption tracks
// "<target>-<source>". A pattern like "en.*" therefore matches "en-de"
// ("English from German") as readily as "en-US", and a video with translations
// from several source languages turns one subtitle fetch into a dozen — which
// is how a capture earns an HTTP 429 partway through and loses the track it
// actually wanted. Verified live against yt-dlp 2026.08.04.
var englishSubtitleLangs = []string{"en", "en-US", "en-GB"}

// originalTrackPattern matches yt-dlp's "-orig" suffix. Those tracks are always
// the untranslated original, whatever the language, so they are safe to request
// as a pattern. Most listings contain none at all.
const originalTrackPattern = ".*-orig"

// excludeLiveChat drops YouTube's live_chat "subtitle" track, which is a replay
// of the chat room rather than speech and can be enormous on a long stream.
const excludeLiveChat = "-live_chat"

// subtitleLangCode accepts an ISO-639 style code, optionally with a region or
// script subtag. Anything else is ignored rather than passed to yt-dlp, so a
// surprising probe result can never inject an argument.
var subtitleLangCode = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// InitYtDlpSubtitleLangs sets an operator override for the subtitle language
// filter. The value is passed to yt-dlp's --sub-langs verbatim, so it can use
// the full pattern syntax ("all,-live_chat" to hoard everything). Empty keeps
// the computed default.
func InitYtDlpSubtitleLangs(langs string) string {
	value := strings.TrimSpace(langs)
	subtitleLangsMu.Lock()
	subtitleLangs = value
	subtitleLangsMu.Unlock()
	return value
}

// YtDlpSubtitleLangsOverride returns the configured override, or empty.
func YtDlpSubtitleLangsOverride() string {
	subtitleLangsMu.RLock()
	defer subtitleLangsMu.RUnlock()
	return subtitleLangs
}

// YtDlpSubtitleLangs builds the --sub-langs filter for a video whose detected
// language is detectedLang (empty when the extractor did not report one).
//
// The contract is the post's own language plus English: the original track is
// the primary record, and English is the one most callers can read. Machine
// translations into 150 other languages are neither.
func YtDlpSubtitleLangs(detectedLang string) string {
	if override := YtDlpSubtitleLangsOverride(); override != "" {
		return override
	}

	langs := make([]string, 0, len(englishSubtitleLangs)+3)
	seen := make(map[string]bool)
	add := func(code string) {
		if code == "" || seen[code] {
			return
		}
		seen[code] = true
		langs = append(langs, code)
	}

	if detected := NormalizeSubtitleLang(detectedLang); detected != "" {
		add(detected)
		// A regional original ("pt-BR") is still the original when the bare
		// code is what the extractor reported, and vice versa.
		if base, _, found := strings.Cut(detected, "-"); found {
			add(base)
		}
	}
	for _, code := range englishSubtitleLangs {
		add(code)
	}
	add(originalTrackPattern)
	langs = append(langs, excludeLiveChat)
	return strings.Join(langs, ",")
}

// NormalizeSubtitleLang validates a language code reported by an extractor.
// yt-dlp prints "NA" for an absent field, and an unrecognized value must not
// reach the command line.
func NormalizeSubtitleLang(lang string) string {
	trimmed := strings.TrimSpace(lang)
	switch strings.ToLower(trimmed) {
	case "", "na", "n/a", "none", "null":
		return ""
	}
	if !subtitleLangCode.MatchString(trimmed) {
		return ""
	}
	return trimmed
}

// YtDlpSubtitleArgs returns the arguments that make yt-dlp write subtitle
// tracks beside the video.
//
// VTT is requested first because it is what every browser plays natively and
// what Arker's transcript derivation reads; "best" is the fallback so a track
// offered in some other format is still captured rather than skipped.
func YtDlpSubtitleArgs(detectedLang string) []string {
	return []string{
		"--write-subs",
		"--write-auto-subs",
		"--sub-langs", YtDlpSubtitleLangs(detectedLang),
		"--sub-format", "vtt/best",
	}
}
