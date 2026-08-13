package utils

import (
	"strings"
	"testing"
)

// The filter must never contain an open wildcard on a language prefix. yt-dlp
// matches each entry as an anchored regex, and YouTube names translated
// auto-captions "<target>-<source>", so "en.*" matches "en-de" ("English from
// German") too. Verified live: on a video offering ~150 translations that
// pattern fetched three tracks and earned an HTTP 429 partway through.
func TestSubtitleLangsExcludeTranslatedTracks(t *testing.T) {
	t.Cleanup(func() { InitYtDlpSubtitleLangs("") })
	InitYtDlpSubtitleLangs("")

	got := YtDlpSubtitleLangs("")
	for _, entry := range strings.Split(got, ",") {
		if strings.HasPrefix(entry, "en.") || entry == "en.*" {
			t.Fatalf("filter %q contains an open English wildcard, which also matches en-de translations", got)
		}
	}
	if !strings.Contains(got, "en") {
		t.Fatalf("filter = %q, want English", got)
	}
	if !strings.Contains(got, "-live_chat") {
		t.Fatalf("filter = %q, want the live chat replay excluded", got)
	}
}

func TestSubtitleLangsIncludeTheVideosOwnLanguage(t *testing.T) {
	t.Cleanup(func() { InitYtDlpSubtitleLangs("") })
	InitYtDlpSubtitleLangs("")

	got := YtDlpSubtitleLangs("de")
	entries := strings.Split(got, ",")
	if entries[0] != "de" {
		t.Fatalf("filter = %q, want the video's own language first", got)
	}
	if !strings.Contains(got, "en") {
		t.Fatalf("filter = %q, want English as well", got)
	}

	// A regional original contributes its base code too, since either spelling
	// may be what the track is listed under.
	regional := YtDlpSubtitleLangs("pt-BR")
	if !strings.Contains(regional, "pt-BR") || !strings.Contains(regional, "pt,") {
		t.Fatalf("filter = %q, want both pt-BR and pt", regional)
	}

	// English videos must not list English twice.
	english := YtDlpSubtitleLangs("en")
	if strings.Count(english, "en,") > strings.Count(YtDlpSubtitleLangs(""), "en,") {
		t.Fatalf("filter = %q duplicates English", english)
	}
}

// A garbage or absent language must never reach the command line.
func TestNormalizeSubtitleLangRejectsJunk(t *testing.T) {
	for _, input := range []string{"", "NA", "na", "none", "null", "n/a", "not a language", "--proxy", "e", "en;rm -rf"} {
		if got := NormalizeSubtitleLang(input); got != "" {
			t.Errorf("NormalizeSubtitleLang(%q) = %q, want empty", input, got)
		}
	}
	for _, input := range []string{"en", "de", "pt-BR", "zh-Hans", "fil"} {
		if got := NormalizeSubtitleLang(input); got != input {
			t.Errorf("NormalizeSubtitleLang(%q) = %q, want it kept", input, got)
		}
	}
}

func TestSubtitleLangsOverride(t *testing.T) {
	t.Cleanup(func() { InitYtDlpSubtitleLangs("") })
	InitYtDlpSubtitleLangs("all,-live_chat")
	if got := YtDlpSubtitleLangs("de"); got != "all,-live_chat" {
		t.Fatalf("override = %q, want it used verbatim", got)
	}
	InitYtDlpSubtitleLangs("")
	if got := YtDlpSubtitleLangs("de"); got == "all,-live_chat" {
		t.Fatal("clearing the override did not restore the computed default")
	}
}

func TestSubtitleArgsRequestBothManualAndAutomatic(t *testing.T) {
	t.Cleanup(func() { InitYtDlpSubtitleLangs("") })
	InitYtDlpSubtitleLangs("")
	joined := strings.Join(YtDlpSubtitleArgs("en"), " ")
	for _, want := range []string{"--write-subs", "--write-auto-subs", "--sub-langs", "--sub-format vtt/best"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
}
