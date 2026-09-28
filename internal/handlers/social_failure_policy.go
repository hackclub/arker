package handlers

import (
	"strings"

	"arker/internal/archivers"
)

// setSocialFailurePolicy is the consumer-facing policy, separate from the
// historical coarse retryable flag. Conservative defaults avoid turning an
// unknown extractor failure into either a deletion claim or paid retry loop.
// It only publishes enums; diagnostic text/URLs never enter these fields.
func setSocialFailurePolicy(f *socialFailure, diagnostic string) {
	f.Category, f.Reason = "capture_failed", f.Code
	f.Retry = socialFailureRetry{Action: "investigate"}
	switch f.Code {
	case "authentication_required":
		f.Category, f.Reason = "access_required", "authentication_required"
		f.Retry.Action = "configure_source_access"
	case "unsupported_url":
		f.Category = "unsupported"
		f.Retry.Action = "do_not_retry"
	case "content_unavailable":
		f.Category, f.Reason = "source_unavailable", "source_unavailable"
		f.Retry.Action = "wait_for_source_change"
		switch {
		case strings.Contains(diagnostic, "This live stream recording is not available"):
			f.Reason = "recording_unavailable"
		case strings.Contains(diagnostic, "This live event will begin in"),
			strings.Contains(diagnostic, "This live event has ended"),
			strings.Contains(diagnostic, "Offline."):
			f.Reason = "live_not_ready"
		case strings.Contains(diagnostic, "Private video"):
			f.Category, f.Reason = "access_required", "private_content"
			f.Retry.Action = "configure_source_access"
		case strings.Contains(diagnostic, "This video has been removed"),
			strings.Contains(diagnostic, "This video is no longer available"):
			f.Reason = "removed_content"
		}
	case "extractor_failed":
		f.Reason = "extraction_failed"
		switch {
		case strings.Contains(diagnostic, "Apify fallback declined after"):
			f.Reason = "fallback_budget_exhausted"
		case strings.Contains(diagnostic, "failed_to_fetch_post_details"):
			// This is an inconclusive provider response, not proof of deletion.
			f.Reason = "availability_unconfirmed"
		case strings.Contains(diagnostic, archivers.ErrSourceAccessRequired.Error()):
			f.Category, f.Reason = "access_required", "authentication_required"
			f.Retry.Action = "configure_source_access"
		}
	case "legacy_archive", "metadata_unavailable", "media_incomplete",
		"completeness_unknown", "raw_metadata_unavailable":
		f.Category = "artifact_incomplete"
		f.Retry.Action = "repair_archive"
	}
}
