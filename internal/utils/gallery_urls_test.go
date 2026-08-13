package utils

import "testing"

func TestIsGalleryDLURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		// Instagram feed posts: the case that motivated the archiver.
		{"instagram post", "https://www.instagram.com/p/DbktPO1Eopi/", true},
		{"instagram post with carousel index", "https://www.instagram.com/p/DbktPO1Eopi/?img_index=1", true},
		{"instagram post without www", "https://instagram.com/p/ABC123/", true},
		{"instagram tv", "https://www.instagram.com/tv/ABC123/", true},

		// Other post-shaped URLs across supported hosts.
		{"x status", "https://x.com/someone/status/1234567890", true},
		{"twitter status", "https://twitter.com/someone/status/1234567890", true},
		{"reddit submission", "https://www.reddit.com/r/pics/comments/abc123/title/", true},
		{"redd.it short link", "https://redd.it/abc123", true},
		{"tumblr post", "https://example.tumblr.com/post/1234567890", true},
		{"bluesky post", "https://bsky.app/profile/a.bsky.social/post/abc", true},
		{"flickr photo", "https://www.flickr.com/photos/someone/123456/", true},
		{"imgur album", "https://imgur.com/a/Kn9lB", true},
		{"deviantart art", "https://www.deviantart.com/someone/art/Title-123", true},
		{"artstation artwork", "https://www.artstation.com/artwork/abc123", true},
		{"pixiv artwork", "https://www.pixiv.net/artworks/12345678", true},
		{"pinterest pin", "https://www.pinterest.com/pin/123456789/", true},

		// Reels are yt-dlp's job; routing them here would double Instagram
		// traffic for no gain.
		{"instagram reel", "https://www.instagram.com/reel/DPAid-WDi67/", false},

		// Profile, feed, and tag URLs are not single posts. gallery-dl would
		// happily download an entire account from these.
		{"instagram profile", "https://www.instagram.com/someone/", false},
		{"instagram home", "https://www.instagram.com/", false},
		{"instagram stories", "https://www.instagram.com/stories/someone/", false},
		{"x profile", "https://x.com/someone", false},
		{"reddit subreddit", "https://www.reddit.com/r/pics/", false},
		{"imgur home", "https://imgur.com/", false},

		// Unsupported hosts.
		{"youtube video", "https://www.youtube.com/watch?v=123", false},
		{"tiktok video", "https://www.tiktok.com/@someone/video/123", false},
		{"regular website", "https://example.com/p/whatever", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsGalleryDLURL(tt.url); got != tt.want {
				t.Errorf("IsGalleryDLURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

// An Instagram feed post must never be classified as a video URL: that is what
// gave it a yt-dlp item guaranteed to fail with "There is no video in this post".
func TestInstagramPhotoPostsAreNotVideoURLs(t *testing.T) {
	photoPosts := []string{
		"https://www.instagram.com/p/DbktPO1Eopi/",
		"https://www.instagram.com/p/ABC123/?img_index=3",
		"https://www.instagram.com/tv/ABC123/",
	}
	for _, url := range photoPosts {
		if IsVideoURL(url) {
			t.Errorf("IsVideoURL(%q) = true, want false", url)
		}
		if !IsInstagramPhotoPostURL(url) {
			t.Errorf("IsInstagramPhotoPostURL(%q) = false, want true", url)
		}
		if !IsInstagramURL(url) {
			t.Errorf("IsInstagramURL(%q) = false, want true", url)
		}
	}

	// Reels keep working exactly as before.
	reel := "https://www.instagram.com/reel/DPAid-WDi67/"
	if !IsVideoURL(reel) {
		t.Errorf("IsVideoURL(%q) = false, want true", reel)
	}
	if IsInstagramPhotoPostURL(reel) {
		t.Errorf("IsInstagramPhotoPostURL(%q) = true, want false", reel)
	}
}

// No URL should ever get both a yt-dlp and a gallery-dl item: one of the two is
// guaranteed to fail, and a failing tab is what this change set out to remove.
func TestArchiveTypesNeverPairYtDlpWithGalleryDL(t *testing.T) {
	urls := []string{
		"https://www.instagram.com/p/ABC123/",
		"https://www.instagram.com/reel/ABC123/",
		"https://www.instagram.com/tv/ABC123/",
		"https://x.com/someone/status/123",
		"https://www.youtube.com/watch?v=123",
		"https://www.tiktok.com/@someone/video/123",
		"https://www.reddit.com/r/pics/comments/abc/title/",
		"https://example.com",
	}

	for _, url := range urls {
		types := GetArchiveTypes(url)
		var hasYtDlp, hasGalleryDl bool
		for _, archiveType := range types {
			switch archiveType {
			case ArchiveTypeYtDlp:
				hasYtDlp = true
			case ArchiveTypeGalleryDl:
				hasGalleryDl = true
			}
		}
		if hasYtDlp && hasGalleryDl {
			t.Errorf("GetArchiveTypes(%q) = %v, want at most one of yt-dlp/gallery-dl", url, types)
		}
	}
}

// Host matching must be on the parsed hostname, not a substring of the whole
// URL: "x.com" is a substring of netflix.com, linux.com, phoenix.com...
func TestIsGalleryDLURLDoesNotMatchHostSubstrings(t *testing.T) {
	shouldNotMatch := []string{
		"https://www.netflix.com/title/status/80100172",
		"https://linux.com/news/status/1234",
		"https://phoenix.com/a/gallery/thing",
		"https://notinstagram.com/p/ABC123/",
		"https://instagram.com.evil.example/p/ABC123/",
		"https://vsco.com/media/abc",
	}
	for _, url := range shouldNotMatch {
		if IsGalleryDLURL(url) {
			t.Errorf("IsGalleryDLURL(%q) = true, want false (host substring must not match)", url)
		}
	}

	// Real subdomains of a listed host still match.
	shouldMatch := []string{
		"https://www.instagram.com/p/ABC123/",
		"https://mobile.x.com/someone/status/123",
	}
	for _, url := range shouldMatch {
		if !IsGalleryDLURL(url) {
			t.Errorf("IsGalleryDLURL(%q) = false, want true", url)
		}
	}
}

// The path shape must be matched against the URL path, not the query string,
// so a "/p/" parked in a query parameter cannot route an unrelated page here.
func TestIsGalleryDLURLIgnoresQueryString(t *testing.T) {
	if IsGalleryDLURL("https://www.instagram.com/someone/?next=/p/ABC123/") {
		t.Error("a /p/ in the query string must not make a profile URL match")
	}
	// But a real post URL with a query string still matches.
	if !IsGalleryDLURL("https://www.instagram.com/p/ABC123/?img_index=2") {
		t.Error("a post URL with a query string must still match")
	}
}

// A bare domain is a homepage, not a post, even for host-only entries.
func TestIsGalleryDLURLRejectsBareHosts(t *testing.T) {
	for _, url := range []string{"https://redd.it/", "https://redd.it", "https://imgur.com/"} {
		if IsGalleryDLURL(url) {
			t.Errorf("IsGalleryDLURL(%q) = true, want false for a bare host", url)
		}
	}
	if !IsGalleryDLURL("https://redd.it/abc123") {
		t.Error("a redd.it short link must match")
	}
}

// A login-only URL without cookies is only worth an archive item when the
// Bright Data fallback gives the guaranteed-to-fail native run a real path to
// success. Coverage is the fallback client's own answer, per URL and archive
// type: a login-only site it does not cover stays excluded.
func TestShouldCreateGalleryDLItemWithBrightDataFallback(t *testing.T) {
	if MediaCookiesConfigured() {
		t.Skip("test requires no cookie jar configured")
	}

	const igPost = "https://www.instagram.com/p/ABC123/"
	const xPost = "https://x.com/user/status/123"
	const pinterestPin = "https://www.pinterest.com/pin/1234567890/"

	SetBrightDataMediaFallback(nil)
	t.Cleanup(func() { SetBrightDataMediaFallback(nil) })
	for _, rawURL := range []string{igPost, xPost, pinterestPin} {
		if ShouldCreateGalleryDLItem(rawURL) {
			t.Errorf("cookie-less item created for %s with no fallback configured", rawURL)
		}
	}

	// Stands in for the real client's coverage table (brightdata.Client.
	// SupportsFallback), which covers Instagram and X but not Pinterest.
	SetBrightDataMediaFallback(func(rawURL, itemType string) bool {
		return itemType == ArchiveTypeGalleryDl && (IsInstagramURL(rawURL) || IsXPostURL(rawURL))
	})
	if !ShouldCreateGalleryDLItem(igPost) {
		t.Error("cookie-less Instagram item not created despite Bright Data fallback")
	}
	if !ShouldCreateGalleryDLItem(xPost) {
		t.Error("cookie-less X item not created despite Bright Data fallback")
	}
	if ShouldCreateGalleryDLItem(pinterestPin) {
		t.Error("cookie-less Pinterest item created; the fallback does not cover Pinterest")
	}
}

// The Bright Data fallback dispatches on these, so a URL that matches the
// wrong one buys the wrong dataset — or a subreddit feed instead of a post.
func TestIsRedditPostURL(t *testing.T) {
	cases := map[string]bool{
		"https://www.reddit.com/r/aww/comments/1vjt9lo/meet_roxy/": true,
		"https://reddit.com/r/aww/comments/abc/":                   true,
		"https://old.reddit.com/r/aww/comments/abc/title/":         true,
		"https://redd.it/1vjt9lo":                                  true,
		"https://www.reddit.com/r/aww/":                            false,
		"https://www.reddit.com/":                                  false,
		"https://www.reddit.com/user/someone":                      false,
		"https://redd.it/":                                         false,
		"https://notreddit.com/r/aww/comments/abc/":                false,
		"https://example.com/?ref=reddit.com/comments/abc":         false,
	}
	for rawURL, want := range cases {
		if got := IsRedditPostURL(rawURL); got != want {
			t.Errorf("IsRedditPostURL(%s) = %v; want %v", rawURL, got, want)
		}
	}
}

func TestIsXPostURL(t *testing.T) {
	cases := map[string]bool{
		"https://x.com/SpaceX/status/2057952539417461045":       true,
		"https://twitter.com/BarackObama/status/266031293945":   true,
		"https://mobile.twitter.com/someone/status/1":           true,
		"https://www.x.com/someone/status/1?s=20":               true,
		"https://x.com/someone":                                 false,
		"https://x.com/":                                        false,
		"https://netflix.com/status/1":                          false,
		"https://example.com/?u=https://x.com/someone/status/1": false,
	}
	for rawURL, want := range cases {
		if got := IsXPostURL(rawURL); got != want {
			t.Errorf("IsXPostURL(%s) = %v; want %v", rawURL, got, want)
		}
	}
}
