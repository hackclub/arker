package brightdata

import (
	"errors"
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
