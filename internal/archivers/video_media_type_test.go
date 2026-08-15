package archivers

import (
	"encoding/json"
	"testing"

	"arker/internal/testfixtures"
)

// TestNativeYtDlpNormalizesProviderDeliveryFormat pins the delivery format —
// a YouTube Short versus an ordinary video — into the normalized metadata the
// manifest serves. A consumer picking a viewer-retention curve cannot infer
// this from duration and aspect ratio without guessing, and the raw record is
// a second request away.
//
// The assertion is a passthrough of the provider's own term rather than a
// vocabulary of Arker's invention: whatever `/video/:shortid/raw` reports as
// `media_type` is what the manifest reports. A provider that does not name a
// delivery format (Instagram) must leave the field absent rather than have one
// guessed for it.
func TestNativeYtDlpNormalizesProviderDeliveryFormat(t *testing.T) {
	for _, c := range testfixtures.CasesForTool(testfixtures.ToolYtDlp) {
		t.Run(c.Name, func(t *testing.T) {
			var provider map[string]interface{}
			if err := json.Unmarshal(c.InfoJSON(t), &provider); err != nil {
				t.Fatalf("decode fixture info JSON: %v", err)
			}
			want, _ := provider["media_type"].(string)

			testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: c.Name})
			_, meta, raw, _ := runYtDlpArchive(t, c.URL)

			if meta.MediaType != want {
				t.Errorf("media_type = %q, want the provider's own %q", meta.MediaType, want)
			}

			// The normalized value must agree with the raw record the API
			// serves beside it; two endpoints disagreeing about the same fact
			// is worse than one of them staying silent.
			var rawRecord map[string]interface{}
			if err := json.Unmarshal(raw, &rawRecord); err != nil {
				t.Fatalf("decode raw metadata: %v", err)
			}
			rawValue, _ := rawRecord["media_type"].(string)
			if rawValue != meta.MediaType {
				t.Errorf("media_type = %q but raw record reports %q", meta.MediaType, rawValue)
			}
		})
	}
}

// TestYouTubeShortsAndRegularVideosAreDistinguishable is the case the pipeline
// actually depends on: the two YouTube post types must be told apart from the
// normalized record alone.
func TestYouTubeShortsAndRegularVideosAreDistinguishable(t *testing.T) {
	shorts := testfixtures.Lookup(t, "youtube_shorts")
	testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: shorts.Name})
	_, shortsMeta, _, _ := runYtDlpArchive(t, shorts.URL)
	if shortsMeta.MediaType != "short" {
		t.Errorf("Shorts media_type = %q, want short", shortsMeta.MediaType)
	}
	// canonical_url rewrites /shorts/X into /watch?v=X, so source_url is the
	// only place the submitted URL shape survives. Both must keep behaving
	// this way for a consumer to have a second, independent signal.
	if shortsMeta.SourceURL != shorts.URL {
		t.Errorf("source_url = %q, want the submitted %q", shortsMeta.SourceURL, shorts.URL)
	}

	regular := testfixtures.Lookup(t, "youtube_regular")
	testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: regular.Name})
	_, regularMeta, _, _ := runYtDlpArchive(t, regular.URL)
	if regularMeta.MediaType != "video" {
		t.Errorf("long-form media_type = %q, want video", regularMeta.MediaType)
	}
}

// TestBackfillVideoMetadataPreservesDeliveryFormat guards the post-storage
// probe pass. ffprobe reads intrinsic facts out of the stored bytes and
// rewrites the sidecar; a delivery format is not one of those facts, and a
// round trip that silently dropped it would leave every newly captured Short
// looking like a legacy archive.
func TestBackfillVideoMetadataPreservesDeliveryFormat(t *testing.T) {
	duration := 60.0
	original, err := MarshalVideoMetadata(&VideoMetadata{
		SchemaVersion:   VideoMetadataSchemaVersion,
		SourceURL:       "https://www.youtube.com/shorts/wAGwnFMBdHk",
		Platform:        "youtube",
		MediaType:       "short",
		DurationSeconds: &duration,
		Media:           VideoMedia{Extension: ".mp4", ContentType: "video/mp4"},
	})
	if err != nil {
		t.Fatal(err)
	}

	probedDuration := 59.921
	width, height := int64(1080), int64(1468)
	updated, err := BackfillVideoMetadata(original, VideoProbe{
		DurationSeconds: &probedDuration,
		Width:           &width,
		Height:          &height,
	})
	if err != nil {
		t.Fatalf("BackfillVideoMetadata: %v", err)
	}
	var meta VideoMetadata
	if err := json.Unmarshal(updated, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.MediaType != "short" {
		t.Errorf("media_type after probe backfill = %q, want short", meta.MediaType)
	}
	if meta.DurationSeconds == nil || *meta.DurationSeconds != probedDuration {
		t.Errorf("probe backfill stopped applying: duration = %v", meta.DurationSeconds)
	}
}

// TestLegacyMetadataWithoutDeliveryFormatRoundTrips proves the field is
// genuinely additive. A sidecar written before this existed must survive every
// rewrite path unchanged and simply carry no delivery format, rather than
// erroring or gaining an invented one.
func TestLegacyMetadataWithoutDeliveryFormatRoundTrips(t *testing.T) {
	legacy := []byte(`{"schema_version":"1","source_url":"https://www.youtube.com/shorts/wAGwnFMBdHk","platform":"youtube","engagement":{},"media":{"extension":".mp4","content_type":"video/mp4","size_bytes":8}}`)

	updated, err := BackfillVideoMetadata(legacy, VideoProbe{})
	if err != nil {
		t.Fatalf("BackfillVideoMetadata on a legacy sidecar: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(updated, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, present := decoded["media_type"]; present {
		t.Errorf("legacy sidecar gained a media_type it never captured: %s", updated)
	}
}
