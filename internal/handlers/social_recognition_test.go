package handlers

import (
	"testing"

	"arker/internal/storage"
	"arker/internal/utils"
)

// The contract's fifth promise: a recognized social post must never quietly
// succeed as an MHTML/screenshot-only archive. buildSocialPost decides that
// from utils.IsSocialMediaPostURL, so every claimed post shape has to come back
// with a social_post object — a failed one when no media item exists — while
// ordinary URLs keep returning none at all.
func TestApiArchiveResultRecognizesClaimedSocialShapes(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	r := resultRouter(db, store)

	// Page-only captures: the media item is missing entirely, which is exactly
	// the state that used to read as a plain successful web archive.
	pageOnly := map[string]string{"mhtml": "completed", "screenshot": "completed"}

	social := []struct {
		shortID string
		url     string
	}{
		// The gap that started this: TikTok photo posts matched no archiver.
		{"tkph1", "https://www.tiktok.com/@someone/photo/7412345678901234567"},
		{"tkvm1", "https://vm.tiktok.com/ZMabcdefg/"},
		{"tkvd1", "https://www.tiktok.com/@someone/video/7412345678901234567"},
		{"rddt1", "https://www.reddit.com/r/aww/comments/1abc234/title/"},
		{"vrdd1", "https://v.redd.it/examplevideoid"},
		{"bsky1", "https://bsky.app/profile/bsky.app/post/3mgdqhzxy7s2u"},
		{"vmeo1", "https://vimeo.com/76979871"},
		{"xstt1", "https://x.com/someone/status/1234567890123456789"},
		{"ytsh1", "https://www.youtube.com/shorts/abc123"},
		{"fbrl1", "https://www.facebook.com/reel/1234567890"},
	}
	for _, tc := range social {
		createVideoCapture(t, db, tc.shortID, tc.url, pageOnly)
		code, body := getResult(t, r, tc.shortID)
		if code != 200 {
			t.Fatalf("%s: status = %d", tc.url, code)
		}
		post, ok := body["social_post"].(map[string]any)
		if !ok {
			t.Errorf("%s: social_post = %#v, want a failed object, not null", tc.url, body["social_post"])
			continue
		}
		if post["status"] != "failed" || post["terminal"] != true {
			t.Errorf("%s: social_post = %#v, want an explicit terminal failure", tc.url, post)
		}
		if post["fulfilled"] != false {
			t.Errorf("%s: fulfilled = %#v, want false", tc.url, post["fulfilled"])
		}
		failure, ok := post["failure"].(map[string]any)
		if !ok || failure["code"] == "" {
			t.Errorf("%s: failure = %#v, want a machine-readable code", tc.url, post["failure"])
		}
	}

	// Ordinary URLs must be untouched: no social_post, no behavior change.
	for _, tc := range []struct {
		shortID string
		url     string
	}{
		{"page1", "https://example.com/article"},
		{"gith1", "https://github.com/hackclub/arker"},
		{"fbph1", "https://www.facebook.com/photo/?fbid=123"},
		{"tkpr1", "https://www.tiktok.com/@someone"},
	} {
		createVideoCapture(t, db, tc.shortID, tc.url, pageOnly)
		code, body := getResult(t, r, tc.shortID)
		if code != 200 {
			t.Fatalf("%s: status = %d", tc.url, code)
		}
		if body["social_post"] != nil {
			t.Errorf("%s: social_post = %#v, want null for an ordinary URL", tc.url, body["social_post"])
		}
	}
}

// A login-only site with no cookie jar gets no gallery-dl item on purpose. The
// API has to say why rather than reporting a generic miss.
func TestApiArchiveResultReportsAuthRequiredForCookielessSites(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	r := resultRouter(db, store)

	// Pin the cookie state instead of inheriting whatever the developer has
	// configured: the answer depends on it.
	if _, err := utils.InitYtDlpCookies("", "", t.TempDir()); err != nil {
		t.Fatalf("clear media cookies: %v", err)
	}

	createVideoCapture(t, db, "xauth", "https://x.com/someone/status/1234567890123456789",
		map[string]string{"mhtml": "completed", "screenshot": "completed"})
	_, body := getResult(t, r, "xauth")
	post, ok := body["social_post"].(map[string]any)
	if !ok {
		t.Fatalf("social_post = %#v, want an object", body["social_post"])
	}
	failure, ok := post["failure"].(map[string]any)
	if !ok {
		t.Fatalf("failure = %#v, want an object", post["failure"])
	}
	if failure["code"] != "authentication_required" {
		t.Errorf("failure code = %v, want authentication_required", failure["code"])
	}
	if failure["retryable"] != true {
		t.Errorf("retryable = %v, want true: cookies can be supplied later", failure["retryable"])
	}
}
