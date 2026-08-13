package archivers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// youtubeAutoVTT is the shape YouTube's automatic captions actually take: a
// rolling two-line window where each cue repeats the previous line and appends
// a few words, with per-word timing tags threaded through the text.
const youtubeAutoVTT = `WEBVTT
Kind: captions
Language: en

00:00:00.030 --> 00:00:02.669 align:start position:0%
all right so here we are<00:00:01.290><c> in</c><00:00:01.530><c> front</c><00:00:01.860><c> of</c><00:00:02.100><c> the</c>

00:00:02.669 --> 00:00:02.679 align:start position:0%
all right so here we are in front of the

00:00:02.679 --> 00:00:04.860 align:start position:0%
all right so here we are in front of the
elephants<00:00:03.300><c> the</c><00:00:03.600><c> cool</c><00:00:04.200><c> thing</c>

00:00:04.860 --> 00:00:04.870 align:start position:0%
elephants the cool thing

00:00:04.870 --> 00:00:08.000 align:start position:0%
elephants the cool thing
about these guys is that they have really
`

func TestTranscriptFromVTTCollapsesRollingCaptions(t *testing.T) {
	got, truncated := TranscriptFromVTT(youtubeAutoVTT)
	if truncated {
		t.Error("a short track reported truncation")
	}

	want := "all right so here we are in front of the\nelephants the cool thing\nabout these guys is that they have really"
	if got != want {
		t.Fatalf("transcript =\n%q\nwant\n%q", got, want)
	}
	// The specific failure this guards: the rolling window saying everything
	// two or three times.
	if strings.Count(got, "all right so here we are") != 1 {
		t.Errorf("the opening line survived %d times, want 1:\n%s", strings.Count(got, "all right so here we are"), got)
	}
	if strings.Contains(got, "<") || strings.Contains(got, "-->") {
		t.Errorf("markup or timings leaked into the transcript:\n%s", got)
	}
}

func TestTranscriptFromVTTHandlesManualCuesAndEntities(t *testing.T) {
	manual := `WEBVTT

1
00:00:01.000 --> 00:00:03.000
Tom &amp; Jerry said &quot;hello&quot;

2
00:00:03.000 --> 00:00:05.000
&lt;not a tag&gt;

3
00:00:05.000 --> 00:00:07.000
Tom &amp; Jerry said &quot;hello&quot;
`
	got, _ := TranscriptFromVTT(manual)
	want := "Tom & Jerry said \"hello\"\n<not a tag>\nTom & Jerry said \"hello\""
	if got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
	// Cue numbers are structure, not speech.
	if strings.Contains(got, "\n1\n") || strings.HasPrefix(got, "1\n") {
		t.Errorf("cue identifiers leaked into the transcript: %q", got)
	}
	// Only *consecutive* repeats are collapsed; a genuinely repeated line
	// later in the video is part of what was said.
	if strings.Count(got, "Tom & Jerry") != 2 {
		t.Errorf("a non-consecutive repeat was wrongly collapsed: %q", got)
	}
}

func TestTranscriptFromVTTEmptyInput(t *testing.T) {
	for _, input := range []string{"", "WEBVTT\n", "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\n\n"} {
		if got, _ := TranscriptFromVTT(input); got != "" {
			t.Errorf("TranscriptFromVTT(%q) = %q, want empty", input, got)
		}
	}
}

func TestTranscriptFromVTTTruncatesAtTheCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for i := 0; i < 40000; i++ {
		b.WriteString("00:00:01.000 --> 00:00:02.000\n")
		// Distinct lines, or the dedupe would collapse them.
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 40))
		b.WriteString(strconv.Itoa(i))
		b.WriteString("\n\n")
	}
	got, truncated := TranscriptFromVTT(b.String())
	if !truncated {
		t.Fatal("an oversized track did not report truncation")
	}
	if len(got) > MaxTranscriptBytes {
		t.Fatalf("transcript is %d bytes, want at most %d", len(got), MaxTranscriptBytes)
	}
}

func TestBuildTranscriptPrefersOriginalLanguageThenManual(t *testing.T) {
	contents := map[string]string{
		".sub.en.vtt": "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nenglish auto\n",
		".sub.de.vtt": "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\ndeutscher text\n",
	}
	tracks := []SubtitleTrack{
		{Lang: "en", Kind: SubtitleKindAuto, Format: "vtt", ArtifactSuffix: ".sub.en.vtt"},
		{Lang: "de", Kind: SubtitleKindManual, Format: "vtt", ArtifactSuffix: ".sub.de.vtt"},
	}

	// The video's own language wins: it is the primary record, and English is
	// only the fallback for readability.
	got := BuildTranscript(tracks, contents, "de")
	if got == nil || got.Lang != "de" || got.Source != SubtitleKindManual {
		t.Fatalf("transcript = %+v, want the German manual track", got)
	}
	if got.Text != "deutscher text" {
		t.Errorf("text = %q", got.Text)
	}

	// With no detected language, English is the fallback.
	if got := BuildTranscript(tracks, contents, ""); got == nil || got.Lang != "en" {
		t.Fatalf("transcript = %+v, want the English track", got)
	}

	// A manual track beats an automatic one for the same language.
	same := []SubtitleTrack{
		{Lang: "en", Kind: SubtitleKindAuto, Format: "vtt", ArtifactSuffix: ".sub.en.auto.vtt"},
		{Lang: "en-US", Kind: SubtitleKindManual, Format: "vtt", ArtifactSuffix: ".sub.en-US.vtt"},
	}
	sameContents := map[string]string{
		".sub.en.auto.vtt": "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nrecognized\n",
		".sub.en-US.vtt":   "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nreviewed\n",
	}
	if got := BuildTranscript(same, sameContents, "en"); got == nil || got.Source != SubtitleKindManual {
		t.Fatalf("transcript = %+v, want the manual track", got)
	}
}

// A post with no captions is the normal case, and it must produce nothing at
// all rather than an empty transcript that looks like a failed derivation.
func TestBuildTranscriptWithoutTracks(t *testing.T) {
	if got := BuildTranscript(nil, nil, "en"); got != nil {
		t.Fatalf("transcript = %+v, want nil", got)
	}
	tracks := []SubtitleTrack{{Lang: "en", Kind: SubtitleKindAuto, Format: "vtt", ArtifactSuffix: ".sub.en.vtt"}}
	if got := BuildTranscript(tracks, map[string]string{".sub.en.vtt": "WEBVTT\n"}, "en"); got != nil {
		t.Fatalf("transcript = %+v, want nil for a track with no cues", got)
	}
}

func TestSubtitleLangFromFilename(t *testing.T) {
	base := "/tmp/arker-video-123/video"
	for name, want := range map[string]string{
		"/tmp/arker-video-123/video.en.vtt":    "en",
		"/tmp/arker-video-123/video.en-US.vtt": "en-US",
		"/tmp/arker-video-123/video.pt-BR.srt": "pt-BR",
	} {
		if got := SubtitleLangFromFilename(name, base); got != want {
			t.Errorf("SubtitleLangFromFilename(%q) = %q, want %q", name, got, want)
		}
	}
}

// The storage keys have to round-trip: the archiver names a track by suffix and
// only the worker knows the item's key base.
func TestSetSubtitleStorageKeys(t *testing.T) {
	metadata := &VideoMetadata{
		SchemaVersion: VideoMetadataSchemaVersion,
		Title:         "Fixture",
		Subtitles: []SubtitleTrack{
			{Lang: "en", Kind: SubtitleKindAuto, Format: "vtt", ArtifactSuffix: ".sub.en.vtt", SizeBytes: 440},
		},
		Transcript: &Transcript{Lang: "en", Source: SubtitleKindAuto, Text: "hello"},
	}
	encoded, err := MarshalVideoMetadata(metadata)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	updated, err := SetSubtitleStorageKeys(encoded, map[string]string{".sub.en.vtt": "abc12/yt-dlp-9f2c.sub.en.vtt"})
	if err != nil {
		t.Fatalf("SetSubtitleStorageKeys: %v", err)
	}
	var decoded VideoMetadata
	if err := json.Unmarshal(updated, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Subtitles[0].StorageKey != "abc12/yt-dlp-9f2c.sub.en.vtt" {
		t.Fatalf("storage key = %q", decoded.Subtitles[0].StorageKey)
	}
	// Nothing else may be disturbed by the round trip.
	if decoded.Title != "Fixture" || decoded.Transcript == nil || decoded.Transcript.Text != "hello" {
		t.Fatalf("round trip lost data: %+v", decoded)
	}

	// An archive with no subtitles is returned byte-for-byte.
	plain, _ := MarshalVideoMetadata(&VideoMetadata{SchemaVersion: "1", Title: "No captions"})
	same, err := SetSubtitleStorageKeys(plain, map[string]string{".sub.en.vtt": "x"})
	if err != nil || string(same) != string(plain) {
		t.Fatalf("metadata without subtitles was rewritten: %v / %s", err, same)
	}
}

// The real thing: the English caption track YouTube serves for jNQXAC9IVRw,
// captured live during development. A synthetic fixture only proves the parser
// handles what its author imagined.
func TestTranscriptFromRealYouTubeCaptions(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "youtube_captions.en.vtt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, truncated := TranscriptFromVTT(string(raw))
	if truncated {
		t.Error("a 440-byte track reported truncation")
	}

	want := strings.Join([]string{
		"All right, so here we are, in front of the",
		"elephants",
		"the cool thing about these guys is that they",
		"have really...",
		"really really long trunks",
		"and that's cool",
		"(baaaaaaaaaaahhh!!)",
		"and that's pretty much all there is to",
		"say",
	}, "\n")
	if got != want {
		t.Fatalf("transcript =\n%q\nwant\n%q", got, want)
	}
	// The header block describes the track, not the speech.
	if strings.Contains(got, "WEBVTT") || strings.Contains(got, "Kind:") || strings.Contains(got, "Language:") {
		t.Errorf("VTT header leaked into the transcript:\n%s", got)
	}
}

// A language listed as both automatic and manual is written by yt-dlp as the
// manual track, and the recorded kind must say so on every run — ranging over a
// Go map would decide this at random.
func TestSubtitleKindsPreferManualDeterministically(t *testing.T) {
	info := []byte(`{"subtitles":{"en":[{"ext":"vtt"}]},"automatic_captions":{"en":[{"ext":"vtt"}],"de":[{"ext":"vtt"}]}}`)
	for i := 0; i < 50; i++ {
		kinds := subtitleKindsFromInfo(info)
		if kinds["en"] != SubtitleKindManual {
			t.Fatalf("run %d: en = %q, want manual", i, kinds["en"])
		}
		if kinds["de"] != SubtitleKindAuto {
			t.Fatalf("run %d: de = %q, want auto", i, kinds["de"])
		}
	}
	// An unparseable record yields no claims rather than wrong ones.
	if kinds := subtitleKindsFromInfo([]byte("not json")); len(kinds) != 0 {
		t.Fatalf("kinds = %v, want none", kinds)
	}
}
