package archivers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"arker/internal/testfixtures"
)

// G13: transcripts, subtitles and alt text.
//
// The contract Zach added is "give Arker a post, get everything within
// reason", with YouTube the priority. Everything a platform will hand over
// about a post is part of the archive, and the words spoken in a video are
// the single largest piece of it that Arker currently drops on the floor.
//
// These assertions are written against the contract as stated, not against
// any implementation. Field names below are the obvious ones; the manager
// reconciles them with the implementing branch at integration, so a rename is
// a one-line change here rather than a rewrite.
//
// Everything that already holds today is asserted unguarded. Only the parts
// that need the feature are behind t.Skip.

// normalizedMetadataMap runs the real archiver and decodes its normalized
// metadata sidecar into a map, so a test can assert on fields VideoMetadata
// does not carry yet.
func normalizedMetadataMap(t *testing.T, fake testfixtures.YtDlpFake, url string) map[string]any {
	t.Helper()
	testfixtures.InstallFakeYtDlp(t, fake)
	var log strings.Builder
	archiver := &YtDlpArchiver{}
	result, err := archiver.Archive(t.Context(), url, &log, nil, 1)
	if err != nil {
		t.Fatalf("yt-dlp archive failed: %v\nlog:\n%s", err, log.String())
	}
	if closer, ok := result.Data.(io.Closer); ok {
		_, _ = io.Copy(io.Discard, result.Data)
		_ = closer.Close()
	}
	var raw map[string]any
	if err := json.Unmarshal(result.Metadata.Data, &raw); err != nil {
		t.Fatalf("decode normalized metadata: %v", err)
	}
	return raw
}

// TestYtDlpAsksForSubtitles pins the argv half of G13. Arker cannot store a
// transcript it never asked yt-dlp to download.
func TestYtDlpAsksForSubtitles(t *testing.T) {
	args := ytDlpDownloadArgs("/tmp/out.%(ext)s")
	joined := strings.Join(args, " ")

	// Already true and load-bearing for the transcript work: the info JSON is
	// where the available subtitle languages are listed.
	if !strings.Contains(joined, "--write-info-json") {
		t.Error("yt-dlp args no longer request the info JSON")
	}

	t.Skip("contract-pending: G13 — yt-dlp is not asked for subtitles yet; enable at integration")

	for _, want := range []string{"--write-subs", "--write-auto-subs"} {
		if !strings.Contains(joined, want) {
			t.Errorf("yt-dlp args %v are missing %q", args, want)
		}
	}
	// Auto-captions exist for almost every YouTube video in dozens of
	// machine-translated variants. Downloading all of them turns one archive
	// into a hundred files, so the language set has to be bounded.
	if !strings.Contains(joined, "--sub-langs") {
		t.Errorf("yt-dlp args %v do not bound --sub-langs; auto-captions come in ~100 languages", args)
	}
}

// TestFulfilledYouTubeExposesTranscriptAndSubtitles is the core of G13.
func TestFulfilledYouTubeExposesTranscriptAndSubtitles(t *testing.T) {
	for _, name := range []string{"youtube_regular", "youtube_shorts"} {
		t.Run(name, func(t *testing.T) {
			c := testfixtures.Lookup(t, name)
			tracks := c.SubtitleTracks(t)
			if len(tracks) == 0 {
				t.Fatalf("fixture %s has no subtitle track to assert against", name)
			}
			meta := normalizedMetadataMap(t, testfixtures.YtDlpFake{Fixture: name}, c.URL)

			// Holds today: the archive itself is sound.
			if meta["post_id"] == "" || meta["title"] == "" {
				t.Fatalf("normalized metadata is not a valid post: %v", meta)
			}

			t.Skip("contract-pending: G13 — transcripts are not extracted yet; enable at integration " +
				"(field names transcript/subtitles are provisional, manager reconciles)")

			transcript, ok := meta["transcript"].(map[string]any)
			if !ok {
				t.Fatal("G13: normalized metadata carries no transcript for a captioned YouTube video")
			}
			text, _ := transcript["text"].(string)
			if strings.TrimSpace(text) == "" {
				t.Fatal("G13: transcript text is empty")
			}
			if lang, _ := transcript["language"].(string); lang != "en" {
				t.Errorf("G13: transcript language = %q, want en", lang)
			}
			// The provenance of the words matters: an auto-caption is a
			// machine guess and a viewer needs to know that.
			if source, _ := transcript["source"].(string); source != "auto" {
				t.Errorf("G13: transcript source = %q, want auto for a YouTube auto-caption", source)
			}

			// Every spoken line appears, in order, exactly once. YouTube's
			// auto-captions repeat the previous line in each cue, so a naive
			// concatenation doubles the transcript.
			for _, line := range tracks[0].CaptionLines() {
				if !strings.Contains(text, line) {
					t.Errorf("G13: transcript is missing caption line %q", line)
				}
				if strings.Count(text, line) != 1 {
					t.Errorf("G13: transcript repeats %q %d times; auto-caption rolling duplication was not collapsed",
						line, strings.Count(text, line))
				}
			}

			// The VTT itself stays retrievable: a transcript is derived, and
			// the timed original is the evidence behind it.
			subtitles, ok := meta["subtitles"].([]any)
			if !ok || len(subtitles) == 0 {
				t.Fatal("G13: normalized metadata links no subtitle artifact")
			}
			first, _ := subtitles[0].(map[string]any)
			if first["language"] != "en" {
				t.Errorf("G13: subtitle language = %v, want en", first["language"])
			}
			if url, _ := first["url"].(string); url == "" {
				t.Error("G13: subtitle entry has no Arker-hosted URL")
			}
		})
	}
}

// TestUploaderSuppliedCaptionsAreNotLabelledAuto keeps the two provenances
// distinct: TikTok's fixture is a real caption track, not a machine guess.
func TestUploaderSuppliedCaptionsAreNotLabelledAuto(t *testing.T) {
	c := testfixtures.Lookup(t, "tiktok_video")
	meta := normalizedMetadataMap(t, testfixtures.YtDlpFake{Fixture: c.Name}, c.URL)
	if meta["post_id"] == "" {
		t.Fatal("normalized metadata is not a valid post")
	}

	t.Skip("contract-pending: G13 — transcripts are not extracted yet; enable at integration")

	transcript, ok := meta["transcript"].(map[string]any)
	if !ok {
		t.Fatal("G13: no transcript for a captioned TikTok video")
	}
	if source, _ := transcript["source"].(string); source != "uploader" {
		t.Errorf("G13: transcript source = %q, want uploader for a supplied caption track", source)
	}
}

// TestAbsentSubtitlesNeverBlockFulfillment is the most important half of G13.
//
// Most videos on most platforms have no captions at all. If "has a transcript"
// leaks into the fulfillment test, Arker starts reporting perfectly complete
// archives as unfulfilled — the same false-negative that contract #1's
// "fulfilled only if complete" rule exists to avoid in the other direction.
func TestAbsentSubtitlesNeverBlockFulfillment(t *testing.T) {
	for _, name := range []string{"vimeo_video", "instagram_reel", "facebook_video"} {
		t.Run(name, func(t *testing.T) {
			c := testfixtures.Lookup(t, name)
			if tracks := c.SubtitleTracks(t); len(tracks) != 0 {
				t.Fatalf("%s is supposed to be the no-subtitles case", name)
			}
			meta := normalizedMetadataMap(t, testfixtures.YtDlpFake{Fixture: name}, c.URL)

			// Holds today and must keep holding: a caption-less video is a
			// complete archive.
			if meta["post_id"] == "" || meta["media"] == nil {
				t.Fatalf("a video with no captions must still produce a full archive: %v", meta)
			}

			t.Skip("contract-pending: G13 — transcript fields do not exist yet; enable at integration")

			if _, present := meta["transcript"]; present {
				t.Error("G13: a video with no captions must not carry an empty transcript object")
			}
			if subs, present := meta["subtitles"]; present {
				if list, ok := subs.([]any); ok && len(list) != 0 {
					t.Errorf("G13: expected no subtitle entries, got %v", list)
				}
			}
		})
	}
}

// TestSubtitleDownloadFailureNeverFailsTheArchive: captions are a bonus, and
// yt-dlp treats a subtitle fetch failure as a warning. Arker must too.
func TestSubtitleDownloadFailureNeverFailsTheArchive(t *testing.T) {
	c := testfixtures.Lookup(t, "youtube_regular")
	// The fixture has a track; NoSubtitles reproduces the run where yt-dlp
	// asked for it and got nothing back.
	testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: c.Name, NoSubtitles: true})
	artifact, meta, _, _ := runYtDlpArchive(t, c.URL)

	if len(artifact) == 0 || meta.PostID == "" {
		t.Fatal("a failed subtitle fetch must not cost the archive its media or metadata")
	}
}

// TestBlueskyAltTextIsSurfaced is the gallery half of G13.
//
// Alt text is the author's own description of the image. On Bluesky it is the
// only text attached to many posts, and gallery-dl already hands it over in
// two places: embed.images[].alt and a flattened "description". Arker reads
// "text" for the caption and deliberately ignores "description" to avoid
// mistaking alt text for the post body — which is right, but it means the alt
// text is currently discarded rather than surfaced in its own field.
func TestBlueskyAltTextIsSurfaced(t *testing.T) {
	c := testfixtures.Lookup(t, "bluesky_image")
	slides := c.Sidecars(t)

	// The fixture must actually carry alt text or this proves nothing.
	var sidecar map[string]any
	if err := json.Unmarshal(slides[0].Data, &sidecar); err != nil {
		t.Fatalf("decode fixture sidecar: %v", err)
	}
	altText, _ := sidecar["description"].(string)
	if strings.TrimSpace(altText) == "" {
		t.Fatal("the bluesky_image fixture no longer carries alt text")
	}

	testfixtures.InstallFakeGalleryDl(t, testfixtures.GalleryDlFake{Fixture: c.Name})
	data, _, err := runGalleryArchive(t, c.URL)
	if err != nil {
		t.Fatalf("gallery archive failed: %v", err)
	}

	meta := galleryManifest(t, data)
	// Holds today, and is the regression this must not undo: the post body
	// comes from "text", never from the alt text.
	if meta.Description == altText {
		t.Fatal("alt text was mistaken for the post caption; that regression is already guarded upstream")
	}
	if !strings.Contains(meta.Description, "v1.130 is live") {
		t.Errorf("caption = %q, want the post body from the text field", meta.Description)
	}

	t.Skip("contract-pending: G13 — alt text is not surfaced yet; enable at integration " +
		"(field name files[].alt_text is provisional, manager reconciles)")

	raw := manifestRaw(t, data)
	files, ok := raw["files"].([]any)
	if !ok || len(files) == 0 {
		t.Fatal("manifest lists no files")
	}
	first, _ := files[0].(map[string]any)
	got, _ := first["alt_text"].(string)
	if got != altText {
		t.Errorf("G13: files[0].alt_text = %q, want the author's alt text %q", got, altText)
	}
}

// TestCarouselAltTextIsPerSlide: alt text belongs to a slide, not a post. A
// ten-slide carousel can describe each image differently, so one post-level
// field would lose nine of them.
func TestCarouselAltTextIsPerSlide(t *testing.T) {
	c := testfixtures.Lookup(t, "instagram_carousel")
	testfixtures.InstallFakeGalleryDl(t, testfixtures.GalleryDlFake{Fixture: c.Name})
	data, _, err := runGalleryArchive(t, c.URL)
	if err != nil {
		t.Fatalf("gallery archive failed: %v", err)
	}
	if meta := galleryManifest(t, data); len(meta.Files) != 10 {
		t.Fatalf("files = %d, want 10", len(meta.Files))
	}

	t.Skip("contract-pending: G13 — per-slide alt text is not surfaced yet; enable at integration")

	raw := manifestRaw(t, data)
	files, _ := raw["files"].([]any)
	for i, entry := range files {
		file, _ := entry.(map[string]any)
		if _, present := file["alt_text"]; !present {
			t.Errorf("G13: slide %d has no alt_text field (absent alt text should be an empty string, not a missing key)", i+1)
		}
	}
}

// TestSubtitleArtifactsAreStoredUnderTheItemKeyBase pins where the VTT lands.
// Storage keys are nonce-suffixed under one keyBase per item so the bucket
// never needs an overwrite; a subtitle artifact has to follow that rule.
func TestSubtitleArtifactsAreStoredUnderTheItemKeyBase(t *testing.T) {
	c := testfixtures.Lookup(t, "youtube_regular")
	testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: c.Name})
	artifact, _, _, _ := runYtDlpArchive(t, c.URL)
	if len(artifact) == 0 {
		t.Fatal("no artifact")
	}

	t.Skip("contract-pending: G13 — subtitle artifacts are not stored yet; enable at integration " +
		"(worker-level assertion: saveArchiveResult should write keyBase + \".en.vtt\" and record it on the item)")
}

// TestGalleryZipIsUnchangedByAltText guards compatibility: adding alt text to
// the manifest must not disturb the bundle layout that existing viewers and
// the /gallery endpoints already depend on.
func TestGalleryZipIsUnchangedByAltText(t *testing.T) {
	c := testfixtures.Lookup(t, "bluesky_image")
	testfixtures.InstallFakeGalleryDl(t, testfixtures.GalleryDlFake{Fixture: c.Name})
	data, _, err := runGalleryArchive(t, c.URL)
	if err != nil {
		t.Fatalf("gallery archive failed: %v", err)
	}

	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open ZIP: %v", err)
	}
	if reader.File[0].Name != galleryMetadataFilename {
		t.Errorf("first ZIP entry = %q, want %q", reader.File[0].Name, galleryMetadataFilename)
	}
	names := zipNames(t, data)
	for _, want := range []string{"001.jpg", "001.jpg.json", galleryMetadataFilename} {
		if !contains(names, want) {
			t.Errorf("ZIP is missing %s; entries: %v", want, names)
		}
	}
}
