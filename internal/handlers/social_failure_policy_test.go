package handlers

import (
	"testing"

	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

func TestSocialFailurePolicies(t *testing.T) {
	tests := []struct {
		code, diagnostic, category, reason, action string
	}{
		{"authentication_required", "", "access_required", "authentication_required", "configure_source_access"},
		{"content_unavailable", "content unavailable at the source: Private video", "access_required", "private_content", "configure_source_access"},
		{"content_unavailable", "content unavailable at the source: This live event will begin in", "source_unavailable", "live_not_ready", "wait_for_source_change"},
		{"content_unavailable", "content unavailable at the source: Offline.", "source_unavailable", "live_not_ready", "wait_for_source_change"},
		{"content_unavailable", "content unavailable at the source: This live stream recording is not available", "source_unavailable", "recording_unavailable", "wait_for_source_change"},
		{"content_unavailable", "content unavailable at the source: Video unavailable", "source_unavailable", "source_unavailable", "wait_for_source_change"},
		{"content_unavailable", "content unavailable at the source: This video has been removed", "source_unavailable", "removed_content", "wait_for_source_change"},
		{"extractor_failed", "Apify fallback declined after 2 recent paid failures (last: post not found: failed_to_fetch_post_details)", "capture_failed", "fallback_budget_exhausted", "investigate"},
		{"extractor_failed", "post not found: failed_to_fetch_post_details", "capture_failed", "availability_unconfirmed", "investigate"},
		{"extractor_failed", "", "capture_failed", "extraction_failed", "investigate"},
		{"unsupported_url", "", "unsupported", "unsupported_url", "do_not_retry"},
		{"legacy_archive", "", "artifact_incomplete", "legacy_archive", "repair_archive"},
		{"media_incomplete", "", "artifact_incomplete", "media_incomplete", "repair_archive"},
		{"metadata_unavailable", "", "artifact_incomplete", "metadata_unavailable", "repair_archive"},
		{"completeness_unknown", "", "artifact_incomplete", "completeness_unknown", "repair_archive"},
		{"raw_metadata_unavailable", "", "artifact_incomplete", "raw_metadata_unavailable", "repair_archive"},
		{"future_failure", "", "capture_failed", "future_failure", "investigate"},
	}
	for _, tc := range tests {
		t.Run(tc.reason+"/"+tc.code, func(t *testing.T) {
			f := &socialFailure{Code: tc.code, Message: "human-readable text can change", Retryable: true}
			setSocialFailurePolicy(f, tc.diagnostic)
			if f.Category != tc.category || f.Reason != tc.reason || f.Retry.Action != tc.action || f.Retry.Automatic {
				t.Fatalf("policy = %#v", f)
			}
			if !f.Retryable || f.Message != "human-readable text can change" {
				t.Fatalf("legacy fields were changed: %#v", f)
			}
		})
	}
}

func TestArchiveGETExposesFailurePolicyWithoutLogParsing(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "pol01", "https://www.youtube.com/watch?v=15FGNbtNbj8", map[string]string{"yt-dlp": "failed"})
	var item models.ArchiveItem
	db.Where("type = ?", "yt-dlp").First(&item)
	if err := utils.AppendArchiveItemLog(db, item.ID, 3,
		"Final attempt failed after 3 tries: content unavailable at the source: This live event will begin in: yt-dlp cannot access video"); err != nil {
		t.Fatal(err)
	}
	_, body := getResult(t, resultRouter(db, store), "pol01")
	social := socialOf(t, body)
	if social["terminal"] != true || social["fulfilled"] != false || social["status"] != "failed" {
		t.Fatalf("lifecycle changed: %#v", social)
	}
	failure := social["failure"].(map[string]any)
	retry := failure["retry"].(map[string]any)
	if failure["code"] != "content_unavailable" || failure["category"] != "source_unavailable" ||
		failure["reason"] != "live_not_ready" || retry["action"] != "wait_for_source_change" || retry["automatic"] != false {
		t.Fatalf("failure contract = %#v", failure)
	}
	// Compatibility flag says possible later, but does not authorize a retry loop.
	if failure["retryable"] != true {
		t.Fatalf("legacy compatibility lost: %#v", failure)
	}
}

func TestIncompleteArchiveAlsoExposesFailurePolicy(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	// A completed item without structured sidecars is not a fulfilled post.
	createVideoCapture(t, db, "old01", "https://www.youtube.com/watch?v=legacy", map[string]string{"yt-dlp": "completed"})
	_, body := getResult(t, resultRouter(db, store), "old01")
	social := socialOf(t, body)
	if social["fulfilled"] != false || social["terminal"] != true {
		t.Fatalf("incomplete archive lifecycle = %#v", social)
	}
	failure, ok := social["failure"].(map[string]any)
	if !ok {
		t.Fatalf("missing failure: %#v", social)
	}
	retry := failure["retry"].(map[string]any)
	if failure["category"] != "artifact_incomplete" || retry["action"] != "repair_archive" || retry["automatic"] != false {
		t.Fatalf("incomplete archive policy = %#v", failure)
	}
}
