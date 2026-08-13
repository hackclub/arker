package brightdata

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A Bright Data compliance refusal is permanent: retrying it only bills more
// guaranteed failures (verified live 2026-08-13, ~$0.08 across three retried
// sessions against a KYC-gated TikTok target).
func TestCompliancePolicyErrorsAreRecognized(t *testing.T) {
	blocked := errors.New("playwright: Protocol error (Page.navigate): Forbidden: target site requires special permission. You are trying to access a target site which is not permitted by our compliance policy. (proxy_error)")
	if !compliancePolicyError(blocked) {
		t.Fatal("KYC refusal not recognized as a policy error")
	}
	for _, transient := range []error{
		errors.New("No Peer Found (no_peers)"),
		errors.New("net::ERR_TIMED_OUT"),
		nil,
	} {
		if compliancePolicyError(transient) {
			t.Fatalf("%v misclassified as a policy error", transient)
		}
	}
}

// A policy-blocked host must not open (and bill) further sessions this
// process lifetime: River's retries would otherwise buy the same refusal
// three times per URL.
func TestPolicyBlockedHostSkipsSessions(t *testing.T) {
	c := &Client{}
	c.markPolicyBlocked("www.tiktok.com")
	opened := 0
	c.openBrowser = func(ctx context.Context, country, pageURL string, logWriter io.Writer) (browserSession, error) {
		opened++
		return nil, errors.New("should not be reached")
	}
	var log strings.Builder
	_, err := c.fetchThroughBrowser(context.Background(), browserFetchRequest{
		PageURL:   "https://www.tiktok.com/@user/video/1",
		MediaURLs: []string{"https://v16.example/video.mp4"},
		LogWriter: &log,
	})
	if err == nil || !strings.Contains(err.Error(), "KYC") {
		t.Fatalf("expected a KYC-labelled refusal, got %v", err)
	}
	if opened != 0 {
		t.Fatalf("a blocked host opened %d sessions; the cache exists to make this 0", opened)
	}
	if c.isPolicyBlocked("www.youtube.com") {
		t.Fatal("unrelated host must not be blocked")
	}
}
