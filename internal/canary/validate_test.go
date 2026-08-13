package canary

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

func TestValidateRoutingAcceptsCatalogProbes(t *testing.T) {
	for _, probe := range DefaultProbes() {
		if !probe.DefaultEnabled {
			continue // cookies-required and hostile-platform probes are opt-in
		}
		if got := ValidateRouting(probe); !got.Passed {
			t.Errorf("probe %s does not route to %s: %s", probe.Key(), probe.ExpectedType, got.FailureReason)
		}
	}
}

func TestValidateRoutingRejectsUnroutedURL(t *testing.T) {
	probe := videoProbe()
	probe.URL = "https://example.com/not-a-post"
	got := ValidateRouting(probe)
	if got.Passed {
		t.Fatal("expected a routing failure for a non-social URL")
	}
	if got.FailureStage != StageRouting {
		t.Errorf("failure stage = %q, want %q", got.FailureStage, StageRouting)
	}
}

func TestValidateArchiveVideoPasses(t *testing.T) {
	store := storage.NewMemoryStorage()
	item := videoFixture(t, store, nil)

	got := ValidateArchive(videoProbe(), item, store, Spend{}, false)
	if !got.Passed {
		t.Fatalf("expected pass, got failure at %s: %s", got.FailureStage, got.FailureReason)
	}
	if got.StageReached != StagePassed {
		t.Errorf("stage reached = %q, want %q", got.StageReached, StagePassed)
	}
	if got.MediaBytes != 4096 {
		t.Errorf("media bytes = %d, want 4096", got.MediaBytes)
	}
	if got.Provenance != models.ArchiveSourceNative {
		t.Errorf("provenance = %q, want native", got.Provenance)
	}
}

// Each case removes exactly one thing the contract requires, and asserts the
// probe fails at the stage that owns it. Together they are the reason a canary
// failure is actionable: the stage says where to look.
func TestValidateArchiveVideoFailureStages(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(meta *archivers.VideoMetadata, item *models.ArchiveItem)
		wantStage   string
		wantReasonC string
	}{
		{
			name:        "item not completed",
			mutate:      func(_ *archivers.VideoMetadata, item *models.ArchiveItem) { item.Status = "failed" },
			wantStage:   StageItemCompleted,
			wantReasonC: "not completed",
		},
		{
			name:        "no storage key",
			mutate:      func(_ *archivers.VideoMetadata, item *models.ArchiveItem) { item.StorageKey = "" },
			wantStage:   StageItemCompleted,
			wantReasonC: "no storage key",
		},
		{
			name:        "normalized metadata missing",
			mutate:      func(_ *archivers.VideoMetadata, item *models.ArchiveItem) { item.MetadataKey = "" },
			wantStage:   StageMetadata,
			wantReasonC: "metadata_key is empty",
		},
		{
			name:        "metadata identifies no post",
			mutate:      func(meta *archivers.VideoMetadata, _ *models.ArchiveItem) { meta.PostID, meta.CanonicalURL = "", "" },
			wantStage:   StageMetadata,
			wantReasonC: "identifies no post",
		},
		{
			name: "metadata has no title or description",
			mutate: func(meta *archivers.VideoMetadata, _ *models.ArchiveItem) {
				meta.Title, meta.Description = "", ""
			},
			wantStage:   StageMetadata,
			wantReasonC: "neither a title nor a description",
		},
		{
			name: "metadata archived_at is not RFC3339",
			mutate: func(meta *archivers.VideoMetadata, _ *models.ArchiveItem) {
				meta.ArchivedAt = "yesterday"
			},
			wantStage:   StageMetadata,
			wantReasonC: "not RFC3339",
		},
		{
			name: "media is not video",
			mutate: func(meta *archivers.VideoMetadata, _ *models.ArchiveItem) {
				meta.Media.ContentType = "image/jpeg"
			},
			wantStage:   StageMedia,
			wantReasonC: "expected a video/* asset",
		},
		{
			name:        "raw metadata missing",
			mutate:      func(_ *archivers.VideoMetadata, item *models.ArchiveItem) { item.RawMetadataKey = "" },
			wantStage:   StageRawMetadata,
			wantReasonC: "raw_metadata_key is empty",
		},
		{
			name: "provenance is paid fallback",
			mutate: func(_ *archivers.VideoMetadata, item *models.ArchiveItem) {
				item.Source = models.ArchiveSourceBrightData
			},
			wantStage:   StageProvenance,
			wantReasonC: "never trigger paid fallback",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := storage.NewMemoryStorage()
			item := videoFixture(t, store, tc.mutate)

			got := ValidateArchive(videoProbe(), item, store, Spend{}, false)
			if got.Passed {
				t.Fatalf("expected failure at %s, but the probe passed", tc.wantStage)
			}
			if got.FailureStage != tc.wantStage {
				t.Errorf("failure stage = %q, want %q (reason: %s)", got.FailureStage, tc.wantStage, got.FailureReason)
			}
			if !strings.Contains(got.FailureReason, tc.wantReasonC) {
				t.Errorf("failure reason %q does not mention %q", got.FailureReason, tc.wantReasonC)
			}
		})
	}
}

func TestValidateArchiveRejectsUndersizedMedia(t *testing.T) {
	store := storage.NewMemoryStorage()
	item := videoFixture(t, store, nil)
	probe := videoProbe()
	probe.MinMediaBytes = 1 << 20 // a real video would clear this; the fixture will not

	got := ValidateArchive(probe, item, store, Spend{}, false)
	if got.Passed {
		t.Fatal("expected a media-size failure")
	}
	if got.FailureStage != StageMedia {
		t.Errorf("failure stage = %q, want %q", got.FailureStage, StageMedia)
	}
	if !strings.Contains(got.FailureReason, "false green") {
		t.Errorf("reason %q should explain why a small artifact is a false green", got.FailureReason)
	}
}

// A canary that somehow billed must never read green, even when everything
// else about the archive is perfect.
func TestValidateArchiveFailsOnRecordedSpend(t *testing.T) {
	store := storage.NewMemoryStorage()
	item := videoFixture(t, store, nil)

	got := ValidateArchive(videoProbe(), item, store, Spend{Operations: 1, CostUSD: 0.0015}, false)
	if got.Passed {
		t.Fatal("expected a provenance failure when spend was recorded")
	}
	if got.FailureStage != StageProvenance {
		t.Errorf("failure stage = %q, want %q", got.FailureStage, StageProvenance)
	}
	if !strings.Contains(got.FailureReason, "never spend money") {
		t.Errorf("reason %q should say canaries must not spend", got.FailureReason)
	}

	// ...unless paid probes were explicitly opted into for this sweep.
	if allowed := ValidateArchive(videoProbe(), item, store, Spend{Operations: 1, CostUSD: 0.0015}, true); !allowed.Passed {
		t.Errorf("paid-allowed sweep should tolerate recorded spend, got %s: %s", allowed.FailureStage, allowed.FailureReason)
	}
}

func TestValidateArchiveGalleryPasses(t *testing.T) {
	store := storage.NewMemoryStorage()
	item := galleryFixture(t, store, defaultGalleryMeta(), defaultGalleryFiles())

	got := ValidateArchive(galleryProbe(), item, store, Spend{}, false)
	if !got.Passed {
		t.Fatalf("expected pass, got failure at %s: %s", got.FailureStage, got.FailureReason)
	}
	if got.MediaCount != 2 {
		t.Errorf("media count = %d, want 2", got.MediaCount)
	}
	if got.ContentType != "image/jpeg" {
		t.Errorf("content type = %q, want image/jpeg", got.ContentType)
	}
}

// The partial-carousel case: metadata says ten files, the bundle holds two.
// This is the exact false-green the contract calls out, so it must fail.
func TestValidateArchiveGalleryDetectsPartialDownload(t *testing.T) {
	store := storage.NewMemoryStorage()
	meta := defaultGalleryMeta()
	meta.FileCount = 10
	item := galleryFixture(t, store, meta, defaultGalleryFiles())

	got := ValidateArchive(galleryProbe(), item, store, Spend{}, false)
	if got.Passed {
		t.Fatal("expected a partial-download failure")
	}
	if got.FailureStage != StageMetadata {
		t.Errorf("failure stage = %q, want %q", got.FailureStage, StageMetadata)
	}
	if !strings.Contains(got.FailureReason, "partial download") {
		t.Errorf("reason %q should name the partial download", got.FailureReason)
	}
}

func TestValidateArchiveGalleryRequiresMediaAndSidecars(t *testing.T) {
	t.Run("no media files", func(t *testing.T) {
		store := storage.NewMemoryStorage()
		meta := defaultGalleryMeta()
		meta.FileCount, meta.Files = 0, nil
		item := galleryFixture(t, store, meta, []galleryFile{{name: "001.jpg.json", data: []byte(`{"id":"1"}`)}})

		got := ValidateArchive(galleryProbe(), item, store, Spend{}, false)
		if got.Passed || got.FailureStage != StageMedia {
			t.Fatalf("want a media failure, got passed=%v stage=%s reason=%s", got.Passed, got.FailureStage, got.FailureReason)
		}
	})

	t.Run("no raw sidecars", func(t *testing.T) {
		store := storage.NewMemoryStorage()
		meta := defaultGalleryMeta()
		meta.FileCount, meta.Files = 1, meta.Files[:1]
		item := galleryFixture(t, store, meta, []galleryFile{{name: "001.jpg", data: bytes.Repeat([]byte("a"), 2048)}})

		got := ValidateArchive(galleryProbe(), item, store, Spend{}, false)
		if got.Passed || got.FailureStage != StageRawMetadata {
			t.Fatalf("want a raw-metadata failure, got passed=%v stage=%s reason=%s", got.Passed, got.FailureStage, got.FailureReason)
		}
	})

	t.Run("no normalized metadata", func(t *testing.T) {
		store := storage.NewMemoryStorage()
		item := galleryFixture(t, store, archivers.GalleryMetadata{}, defaultGalleryFiles())

		got := ValidateArchive(galleryProbe(), item, store, Spend{}, false)
		if got.Passed || got.FailureStage != StageMetadata {
			t.Fatalf("want a metadata failure, got passed=%v stage=%s reason=%s", got.Passed, got.FailureStage, got.FailureReason)
		}
	})
}

func TestValidateArchiveMissingItem(t *testing.T) {
	got := ValidateArchive(videoProbe(), nil, storage.NewMemoryStorage(), Spend{}, false)
	if got.Passed || got.FailureStage != StageItemCompleted {
		t.Fatalf("want an item_completed failure, got passed=%v stage=%s", got.Passed, got.FailureStage)
	}
}

func TestValidateArchiveUnexpectedType(t *testing.T) {
	store := storage.NewMemoryStorage()
	item := videoFixture(t, store, nil)
	item.Type = utils.ArchiveTypeMHTML

	got := ValidateArchive(videoProbe(), item, store, Spend{}, false)
	if got.Passed || !strings.Contains(got.FailureReason, "unexpected archive type") {
		t.Fatalf("want an unexpected-type failure, got passed=%v reason=%s", got.Passed, got.FailureReason)
	}
}

// A completed item whose bytes are gone from storage is a false green of its
// own: the row says archived, the object does not exist.
func TestValidateArchiveUnreadableMedia(t *testing.T) {
	store := storage.NewMemoryStorage()
	item := videoFixture(t, store, nil)
	item.StorageKey = "abc12/missing.mp4"

	got := ValidateArchive(videoProbe(), item, store, Spend{}, false)
	if got.Passed || got.FailureStage != StageMedia {
		t.Fatalf("want a media failure, got passed=%v stage=%s", got.Passed, got.FailureStage)
	}
}

func TestWithinTolerance(t *testing.T) {
	if !withinTolerance(1000, 1000, 0.05) {
		t.Error("identical sizes should be within tolerance")
	}
	if !withinTolerance(1000, 1010, 0.05) {
		t.Error("1% difference should be within a 5% tolerance")
	}
	if withinTolerance(1000, 2000, 0.05) {
		t.Error("100% difference should be outside a 5% tolerance")
	}
}

func TestVideoMetadataProblemAcceptsSaneRecord(t *testing.T) {
	meta := archivers.VideoMetadata{
		SchemaVersion: "1", PostID: "x", Title: "t",
		Media:      archivers.VideoMedia{ContentType: "video/mp4"},
		ArchivedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if problem := videoMetadataProblem(meta); problem != "" {
		t.Errorf("sane metadata reported a problem: %s", problem)
	}
}

// The completeness check that does not rely on the extractor's own accounting:
// a 4-image post that archived 2 images has perfectly self-consistent
// metadata, and must still fail.
func TestValidateArchiveGalleryDetectsMissingAssets(t *testing.T) {
	store := storage.NewMemoryStorage()
	meta := defaultGalleryMeta() // consistent: claims 2 files, bundle holds 2
	item := galleryFixture(t, store, meta, defaultGalleryFiles())

	probe := galleryProbe()
	probe.MinMediaCount = 4 // but the real post has four

	got := ValidateArchive(probe, item, store, Spend{}, false)
	if got.Passed {
		t.Fatal("a 2-of-4 gallery passed validation")
	}
	if got.FailureStage != StageMedia {
		t.Errorf("failure stage = %q, want %q", got.FailureStage, StageMedia)
	}
	if !strings.Contains(got.FailureReason, "partial download") {
		t.Errorf("reason %q should name the partial download", got.FailureReason)
	}

	// The same bundle passes when the post really does have two assets.
	probe.MinMediaCount = 2
	if ok := ValidateArchive(probe, item, store, Spend{}, false); !ok.Passed {
		t.Errorf("complete gallery failed at %s: %s", ok.FailureStage, ok.FailureReason)
	}
}

// Every default-enabled probe must declare how many assets its post has,
// otherwise the completeness check silently does nothing for that slot.
func TestDefaultProbesDeclareExpectedMediaCount(t *testing.T) {
	for _, probe := range DefaultProbes() {
		if probe.DefaultEnabled && probe.MinMediaCount <= 0 {
			t.Errorf("probe %s has no MinMediaCount, so a partial archive would pass", probe.Key())
		}
	}
}
