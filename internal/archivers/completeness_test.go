package archivers

import (
	"io"
	"testing"
)

func intPtr(n int) *int { return &n }

func TestCompletenessFromCounts(t *testing.T) {
	tests := []struct {
		name      string
		expected  *int
		stored    int
		runFailed bool
		want      string
	}{
		// A known count is the only thing that decides the state, so a run that
		// exits non-zero for an unrelated reason but stored every asset is
		// complete rather than being punished for the exit code.
		{"all slides stored despite an exit error", intPtr(3), 3, true, CompletenessComplete},
		{"all slides stored, clean exit", intPtr(3), 3, false, CompletenessComplete},
		{"more stored than promised", intPtr(2), 3, false, CompletenessComplete},
		{"three of ten", intPtr(10), 3, false, CompletenessPartial},
		{"nothing of ten", intPtr(10), 0, false, CompletenessPartial},
		// No count and a failed run: something was definitely lost, which is
		// more specific than unknown.
		{"no count, run failed", nil, 2, true, CompletenessPartial},
		// No count and a clean run is the honest unknown: "all of them" is
		// unprovable, so it must not read as complete.
		{"no count, clean run", nil, 2, false, CompletenessUnknown},
		{"zero count is not a count", intPtr(0), 2, false, CompletenessUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CompletenessFromCounts(tt.expected, tt.stored, tt.runFailed)
			if got.State != tt.want {
				t.Errorf("state = %q, want %q", got.State, tt.want)
			}
			if got.Stored != tt.stored {
				t.Errorf("stored = %d, want %d", got.Stored, tt.stored)
			}
			if tt.want == CompletenessUnknown && got.Expected != nil {
				t.Errorf("expected = %v, want nil for an unknown count", *got.Expected)
			}
		})
	}
}

// An unrecognized or empty stored value must never read as complete: an old row
// carries no evidence that its capture was whole.
func TestNormalizeCompletenessStateFailsClosed(t *testing.T) {
	for _, state := range []string{CompletenessComplete, CompletenessPartial, CompletenessUnknown} {
		if got := NormalizeCompletenessState(state); got != state {
			t.Errorf("NormalizeCompletenessState(%q) = %q", state, got)
		}
	}
	for _, state := range []string{"", "COMPLETE", "done", "fulfilled", "partial "} {
		if got := NormalizeCompletenessState(state); got != CompletenessUnknown {
			t.Errorf("NormalizeCompletenessState(%q) = %q, want unknown", state, got)
		}
	}
}

func TestGalleryCompletenessDetectsPartialCarousel(t *testing.T) {
	// gallery-dl downloaded slides 1 and 3 of a 3-slide post and exited
	// non-zero. Slide 2 left neither media nor sidecar behind.
	dir := writeGalleryFixture(t, map[string]string{
		"001.jpg":      "a",
		"001.jpg.json": instagramCarouselSidecar,
		"003.jpg":      "c",
		"003.jpg.json": instagramCarouselSidecar,
	})

	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}
	got := galleryCompleteness(dir, media, sidecars, false, true, io.Discard)

	if got.State != CompletenessPartial {
		t.Fatalf("state = %q, want partial", got.State)
	}
	if got.Expected == nil || *got.Expected != 3 {
		t.Fatalf("expected = %v, want 3 (the sidecar's count)", got.Expected)
	}
	if got.Stored != 2 {
		t.Errorf("stored = %d, want 2", got.Stored)
	}
	if len(got.MissingIndices) != 1 || got.MissingIndices[0] != 2 {
		t.Errorf("missing = %v, want [2] derived from the gap in the slide numbers", got.MissingIndices)
	}
}

// The count is merged into every file's dict, so losing slide 1 must not lose
// the ability to tell that anything is missing.
func TestGalleryCompletenessReadsCountFromAnySurvivingSidecar(t *testing.T) {
	dir := writeGalleryFixture(t, map[string]string{
		"002.jpg":      "b",
		"002.jpg.json": instagramCarouselSidecar,
	})

	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}
	got := galleryCompleteness(dir, media, sidecars, false, true, io.Discard)

	if got.State != CompletenessPartial || got.Expected == nil || *got.Expected != 3 {
		t.Fatalf("completeness = %+v, want partial with expected 3", got)
	}
	if len(got.MissingIndices) != 2 || got.MissingIndices[0] != 1 || got.MissingIndices[1] != 3 {
		t.Errorf("missing = %v, want [1 3]", got.MissingIndices)
	}
}

func TestGalleryCompletenessCompleteRunWithExitError(t *testing.T) {
	// Every slide of the 3-slide post is here, but gallery-dl still exited
	// non-zero (a failed thumbnail fetch, a retried request). The archive is
	// complete: the count is what decides.
	dir := writeGalleryFixture(t, map[string]string{
		"001.jpg": "a", "001.jpg.json": instagramCarouselSidecar,
		"002.jpg": "b", "002.jpg.json": instagramCarouselSidecar,
		"003.jpg": "c", "003.jpg.json": instagramCarouselSidecar,
	})

	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}
	got := galleryCompleteness(dir, media, sidecars, false, true, io.Discard)

	if got.State != CompletenessComplete {
		t.Fatalf("state = %q, want complete (all 3 of 3 stored)", got.State)
	}
	if len(got.MissingIndices) != 0 {
		t.Errorf("missing = %v, want none", got.MissingIndices)
	}
}

// Most extractors do not report a file count. That is unknown, not complete —
// the whole point is that Arker cannot prove it saw everything.
func TestGalleryCompletenessUnknownWithoutACount(t *testing.T) {
	dir := writeGalleryFixture(t, map[string]string{
		"001.jpg":      "a",
		"001.jpg.json": `{"category": "flickr", "width": 100, "height": 100}`,
	})

	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}
	got := galleryCompleteness(dir, media, sidecars, false, false, io.Discard)

	if got.State != CompletenessUnknown {
		t.Fatalf("state = %q, want unknown", got.State)
	}
	if got.Expected != nil {
		t.Errorf("expected = %v, want nil", *got.Expected)
	}
	if got.Stored != 1 {
		t.Errorf("stored = %d, want 1", got.Stored)
	}
}

// A clean run with no count still cannot claim completeness, but a failed one
// can at least say something was lost.
func TestGalleryCompletenessFailedRunWithoutACountIsPartial(t *testing.T) {
	dir := writeGalleryFixture(t, map[string]string{"001.jpg": "a"})
	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}
	if got := galleryCompleteness(dir, media, sidecars, false, true, io.Discard); got.State != CompletenessPartial {
		t.Fatalf("state = %q, want partial", got.State)
	}
}

func TestGalleryExpectedCountPlatformKeysAndGuards(t *testing.T) {
	tests := []struct {
		name    string
		sidecar string
		want    *int
	}{
		{"gallery-dl count", `{"count": 4}`, intPtr(4)},
		{"imgur nested album image_count", `{"album": {"image_count": 5}}`, intPtr(5)},
		{"pixiv page_count", `{"page_count": 7}`, intPtr(7)},
		{"instagram carousel_media_count", `{"carousel_media_count": 2}`, intPtr(2)},
		{"no count key", `{"width": 100}`, nil},
		{"zero is not a count", `{"count": 0}`, nil},
		{"negative is not a count", `{"count": -3}`, nil},
		// A site that reuses one of these names for a follower or view count
		// would otherwise mark every capture partial forever.
		{"implausible value is rejected", `{"count": 250000}`, nil},
		{"non-numeric value", `{"count": "many"}`, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeGalleryFixture(t, map[string]string{
				"001.jpg":      "a",
				"001.jpg.json": tt.sidecar,
			})
			media, sidecars, err := collectGalleryFiles(dir)
			if err != nil {
				t.Fatalf("collectGalleryFiles: %v", err)
			}
			got := galleryExpectedCount(dir, media, sidecars, io.Discard)
			switch {
			case tt.want == nil && got != nil:
				t.Errorf("expected count = %d, want none", *got)
			case tt.want != nil && got == nil:
				t.Errorf("expected count = none, want %d", *tt.want)
			case tt.want != nil && *got != *tt.want:
				t.Errorf("expected count = %d, want %d", *got, *tt.want)
			}
		})
	}
}

// Missing-index reporting depends on the archiver's numeric filenames. A layout
// without them must report "cannot tell" rather than claiming every slide is
// gone.
func TestGalleryMissingIndices(t *testing.T) {
	if got := galleryMissingIndices(4, []string{"001.jpg", "003.mp4"}); len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Errorf("missing = %v, want [2 4]", got)
	}
	if got := galleryMissingIndices(2, []string{"IMG_4021.jpg", "photo.jpg"}); got != nil {
		t.Errorf("missing = %v, want nil for unnumbered filenames", got)
	}
	if got := galleryMissingIndices(1, []string{"001.jpg"}); got != nil {
		t.Errorf("missing = %v, want nil when nothing is missing", got)
	}
}

// The completeness record has to survive in the archive itself, not only in the
// database, or a restored bucket loses the only proof of what was captured.
func TestBuildGalleryMetadataCarriesCompletenessIntoTheArchive(t *testing.T) {
	dir := writeGalleryFixture(t, map[string]string{
		"001.jpg":      "a",
		"001.jpg.json": instagramCarouselSidecar,
	})
	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}

	meta := buildGalleryMetadata(dir, "https://www.instagram.com/p/DbktPO1Eopi/", "1.32.9", media, sidecars, io.Discard)
	completeness := galleryCompleteness(dir, media, sidecars, false, true, io.Discard)
	meta.Completeness = &completeness

	if meta.FileCount != 1 {
		t.Fatalf("FileCount = %d, want 1", meta.FileCount)
	}
	if meta.Completeness == nil || meta.Completeness.State != CompletenessPartial {
		t.Fatalf("completeness = %+v, want partial", meta.Completeness)
	}
	if meta.Completeness.Expected == nil || *meta.Completeness.Expected != 3 {
		t.Fatalf("expected = %v, want 3 while file_count is 1", meta.Completeness.Expected)
	}
}
