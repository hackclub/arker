package utils

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// configureMediaCookies points the media tools at a throwaway cookie jar for
// the duration of one test, so the cookie-gated routes (Instagram feed posts,
// X, Pinterest) can be pinned in both states instead of depending on whatever
// the developer happens to have configured.
func configureMediaCookies(t *testing.T, configured bool) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := InitYtDlpCookies("", "", t.TempDir()); err != nil {
			t.Fatalf("failed to reset media cookies: %v", err)
		}
	})

	if !configured {
		if _, err := InitYtDlpCookies("", "", t.TempDir()); err != nil {
			t.Fatalf("failed to clear media cookies: %v", err)
		}
		return
	}

	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitYtDlpCookies(path, "", t.TempDir()); err != nil {
		t.Fatalf("failed to configure media cookies: %v", err)
	}
}

func archiveTypeSet(types []string) string {
	sorted := append([]string(nil), types...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

func assertArchiveTypes(t *testing.T, url string, want ...string) {
	t.Helper()
	got := GetArchiveTypes(url)
	if archiveTypeSet(got) != archiveTypeSet(want) {
		t.Errorf("GetArchiveTypes(%q) = %v, want %v", url, got, want)
	}
}

// Every claimed platform/post-type in the social contract, pinned to the exact
// archive item set it must produce. A row that changes here is a change to
// what Arker promises to store, not an implementation detail.
//
// Cookie-gated sites appear twice (with and without a cookie jar) because
// ShouldCreateGalleryDLItem deliberately refuses to queue a guaranteed-failed
// login-only capture.
func TestArchiveTypeMatrixWithoutCookies(t *testing.T) {
	configureMediaCookies(t, false)
	SetBrightDataMediaFallback(nil)
	t.Cleanup(func() { SetBrightDataMediaFallback(nil) })

	base := []string{ArchiveTypeMHTML, ArchiveTypeScreenshot}
	withYtDlp := append(append([]string{}, base...), ArchiveTypeYtDlp)
	withGallery := append(append([]string{}, base...), ArchiveTypeGalleryDl)

	tests := []struct {
		platform string
		url      string
		want     []string
	}{
		// --- yt-dlp: single-video post types ---
		{"youtube regular", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", withYtDlp},
		{"youtube short link", "https://youtu.be/dQw4w9WgXcQ", withYtDlp},
		{"youtube shorts", "https://www.youtube.com/shorts/abc123", withYtDlp},
		{"youtube mobile", "https://m.youtube.com/watch?v=abc123", withYtDlp},
		{"vimeo", "https://vimeo.com/76979871", withYtDlp},
		{"vimeo unlisted hash", "https://vimeo.com/76979871/a1b2c3d4e5", withYtDlp},
		{"vimeo player", "https://player.vimeo.com/video/76979871", withYtDlp},
		{"instagram reel", "https://www.instagram.com/reel/DPAid-WDi67/", withYtDlp},
		{"tiktok video", "https://www.tiktok.com/@someone/video/7412345678901234567", withYtDlp},
		{"tiktok vm short link", "https://vm.tiktok.com/ZMabcdefg/", withYtDlp},
		{"tiktok vt short link", "https://vt.tiktok.com/ZSabcdefg/", withYtDlp},
		{"tiktok t short link", "https://www.tiktok.com/t/ZTabcdefg/", withYtDlp},
		{"facebook reel", "https://www.facebook.com/reel/1234567890", withYtDlp},
		{"facebook videos", "https://www.facebook.com/page/videos/1234567890/", withYtDlp},
		{"facebook watch", "https://www.facebook.com/watch/?v=1234567890", withYtDlp},
		{"fb.watch short link", "https://fb.watch/abc123/", withYtDlp},

		// --- gallery-dl: post types that work logged out ---
		{"tiktok photo post", "https://www.tiktok.com/@someone/photo/7412345678901234567", withGallery},
		{"tiktok share photo post", "https://www.tiktok.com/share/photo/7412345678901234567", withGallery},
		{"reddit comments", "https://www.reddit.com/r/aww/comments/abc123/title/", withGallery},
		{"redd.it short link", "https://redd.it/abc123", withGallery},
		{"v.redd.it video", "https://v.redd.it/abc123xyz", withGallery},
		{"bluesky post", "https://bsky.app/profile/bsky.app/post/3mgdqhzxy7s2u", withGallery},
		{"tumblr post", "https://staff.tumblr.com/post/1234567890", withGallery},
		{"flickr photo", "https://www.flickr.com/photos/someone/1234567890/", withGallery},
		{"imgur album", "https://imgur.com/a/Kn9lB", withGallery},
		{"imgur gallery", "https://imgur.com/gallery/abc123", withGallery},
		{"deviantart art", "https://www.deviantart.com/someone/art/Title-123456", withGallery},
		{"artstation artwork", "https://www.artstation.com/artwork/abc123", withGallery},
		{"pixiv artwork", "https://www.pixiv.net/artworks/12345678", withGallery},
		{"newgrounds art", "https://www.newgrounds.com/art/view/someone/title", withGallery},
		{"vsco media", "https://vsco.co/someone/media/abc123", withGallery},

		// --- login-only: no cookie jar means no guaranteed-failed item ---
		{"instagram feed post", "https://www.instagram.com/p/DbktPO1Eopi/", base},
		{"instagram tv", "https://www.instagram.com/tv/ABC123/", base},
		{"x status", "https://x.com/someone/status/1234567890123456789", base},
		{"twitter status", "https://twitter.com/someone/status/1234567890123456789", base},
		{"pinterest pin", "https://www.pinterest.com/pin/1234567890/", base},

		// --- ordinary URL behavior must not change ---
		{"github repo", "https://github.com/hackclub/arker", append(append([]string{}, base...), ArchiveTypeGit)},
		{"gitlab repo", "https://gitlab.com/group/project", append(append([]string{}, base...), ArchiveTypeGit)},
		{"bare .git URL", "https://example.com/thing.git", append(append([]string{}, base...), ArchiveTypeGit)},
		{"itch game", "https://someone.itch.io/some-game", append(append([]string{}, base...), ArchiveTypeItch)},
		{"plain page", "https://example.com/article", base},
		{"plain page with /p/ path", "https://example.com/p/whatever", base},
		{"facebook photo post (unclaimed)", "https://www.facebook.com/photo/?fbid=123", base},
		{"tiktok profile", "https://www.tiktok.com/@someone", base},
		{"reddit subreddit", "https://www.reddit.com/r/aww/", base},
	}

	for _, tt := range tests {
		t.Run(tt.platform, func(t *testing.T) {
			assertArchiveTypes(t, tt.url, tt.want...)
		})
	}
}

// With a cookie jar the login-only sites become archivable, and nothing else
// about routing changes.
func TestArchiveTypeMatrixWithCookies(t *testing.T) {
	configureMediaCookies(t, true)

	base := []string{ArchiveTypeMHTML, ArchiveTypeScreenshot}
	withGallery := append(append([]string{}, base...), ArchiveTypeGalleryDl)

	tests := []struct {
		platform string
		url      string
		want     []string
	}{
		{"instagram feed post", "https://www.instagram.com/p/DbktPO1Eopi/", withGallery},
		{"instagram tv", "https://www.instagram.com/tv/ABC123/", withGallery},
		{"x status", "https://x.com/someone/status/1234567890123456789", withGallery},
		{"twitter status", "https://twitter.com/someone/status/1234567890123456789", withGallery},
		{"pinterest pin", "https://www.pinterest.com/pin/1234567890/", withGallery},

		// Unchanged by cookies.
		{"instagram reel", "https://www.instagram.com/reel/DPAid-WDi67/", append(append([]string{}, base...), ArchiveTypeYtDlp)},
		{"tiktok photo post", "https://www.tiktok.com/@someone/photo/7412345678901234567", withGallery},
		{"plain page", "https://example.com/article", base},
	}

	for _, tt := range tests {
		t.Run(tt.platform, func(t *testing.T) {
			assertArchiveTypes(t, tt.url, tt.want...)
		})
	}
}

// A login-only site with no cookie jar gets its media item back when the
// Bright Data fallback covers it: the native run still fails first, but it is
// now followed by one that can succeed, so the item is worth creating. This is
// the routing half of the fallback contract — the client's coverage decides,
// not a list kept here.
func TestArchiveTypeMatrixWithoutCookiesButWithBrightDataFallback(t *testing.T) {
	configureMediaCookies(t, false)
	// Stands in for brightdata.Client.SupportsFallback, which covers Instagram
	// and X for gallery items but not Pinterest.
	SetBrightDataMediaFallback(func(rawURL, itemType string) bool {
		return itemType == ArchiveTypeGalleryDl && (IsInstagramURL(rawURL) || IsXPostURL(rawURL))
	})
	t.Cleanup(func() { SetBrightDataMediaFallback(nil) })

	base := []string{ArchiveTypeMHTML, ArchiveTypeScreenshot}
	withGallery := append(append([]string{}, base...), ArchiveTypeGalleryDl)

	tests := []struct {
		platform string
		url      string
		want     []string
	}{
		{"x status", "https://x.com/someone/status/1234567890123456789", withGallery},
		{"twitter status", "https://twitter.com/someone/status/1234567890123456789", withGallery},
		{"instagram feed post", "https://www.instagram.com/p/DbktPO1Eopi/", withGallery},

		// Not covered by the fallback: still no item, because the native run
		// cannot succeed and nothing follows it.
		{"pinterest pin", "https://www.pinterest.com/pin/1234567890/", base},

		// Anonymous sites are unaffected either way.
		{"reddit comments", "https://www.reddit.com/r/aww/comments/abc123/title/", withGallery},
		{"instagram reel", "https://www.instagram.com/reel/DPAid-WDi67/", append(append([]string{}, base...), ArchiveTypeYtDlp)},
		{"plain page", "https://example.com/article", base},
	}

	for _, tt := range tests {
		t.Run(tt.platform, func(t *testing.T) {
			assertArchiveTypes(t, tt.url, tt.want...)
		})
	}
}

// Recognition must cover every shape the routing table claims. A recognized
// post that produced no media item gets an explicit failure from the API; an
// unrecognized one silently reads as an ordinary page capture, which is
// exactly the failure mode TikTok photo posts used to have.
func TestIsSocialMediaPostURLCoversEveryClaimedShape(t *testing.T) {
	configureMediaCookies(t, false)

	recognized := []string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ",
		"https://www.youtube.com/shorts/abc123",
		"https://vimeo.com/76979871",
		"https://player.vimeo.com/video/76979871",
		"https://www.instagram.com/reel/DPAid-WDi67/",
		"https://www.instagram.com/p/DbktPO1Eopi/",
		"https://www.instagram.com/tv/ABC123/",
		"https://www.tiktok.com/@someone/video/7412345678901234567",
		"https://www.tiktok.com/@someone/photo/7412345678901234567",
		"https://www.tiktok.com/share/photo/7412345678901234567",
		"https://vm.tiktok.com/ZMabcdefg/",
		"https://vt.tiktok.com/ZSabcdefg/",
		"https://www.tiktok.com/t/ZTabcdefg/",
		"https://www.facebook.com/reel/1234567890",
		"https://www.facebook.com/page/videos/1234567890/",
		"https://www.facebook.com/watch/?v=1234567890",
		"https://fb.watch/abc123/",
		"https://x.com/someone/status/1234567890123456789",
		"https://twitter.com/someone/status/1234567890123456789",
		"https://www.reddit.com/r/aww/comments/abc123/title/",
		"https://redd.it/abc123",
		"https://v.redd.it/abc123xyz",
		"https://bsky.app/profile/bsky.app/post/3mgdqhzxy7s2u",
		"https://www.pinterest.com/pin/1234567890/",
		"https://staff.tumblr.com/post/1234567890",
		"https://www.flickr.com/photos/someone/1234567890/",
		"https://imgur.com/a/Kn9lB",
		"https://www.deviantart.com/someone/art/Title-123456",
		"https://www.artstation.com/artwork/abc123",
		"https://www.pixiv.net/artworks/12345678",
		"https://www.newgrounds.com/art/view/someone/title",
		"https://vsco.co/someone/media/abc123",
	}
	for _, url := range recognized {
		if !IsSocialMediaPostURL(url) {
			t.Errorf("IsSocialMediaPostURL(%q) = false, want true", url)
		}
	}

	// Ordinary URLs must keep ordinary behavior: recognizing one of these
	// would put a social_post object on every plain web archive.
	ordinary := []string{
		"https://example.com/article",
		"https://github.com/hackclub/arker",
		"https://someone.itch.io/some-game",
		"https://www.facebook.com/photo/?fbid=123",
		"https://www.tiktok.com/@someone",
		"https://www.reddit.com/r/aww/",
		"https://www.instagram.com/someone/",
		"https://x.com/someone",
		"https://imgur.com/",
		"https://notinstagram.com/p/ABC123/",
	}
	for _, url := range ordinary {
		if IsSocialMediaPostURL(url) {
			t.Errorf("IsSocialMediaPostURL(%q) = true, want false", url)
		}
	}
}

// Recognition and routing are two answers to the same question and must never
// disagree: anything that gets a media item must be recognized as social.
func TestEveryMediaRoutedURLIsRecognizedAsSocial(t *testing.T) {
	for _, cookies := range []bool{false, true} {
		configureMediaCookies(t, cookies)
		for _, url := range []string{
			"https://www.youtube.com/watch?v=abc",
			"https://vimeo.com/76979871",
			"https://www.instagram.com/reel/ABC123/",
			"https://www.instagram.com/p/ABC123/",
			"https://www.tiktok.com/@someone/video/123",
			"https://www.tiktok.com/@someone/photo/123",
			"https://vm.tiktok.com/ZMabcdefg/",
			"https://x.com/someone/status/123",
			"https://www.reddit.com/r/aww/comments/abc/title/",
			"https://bsky.app/profile/a.bsky.social/post/abc",
			"https://example.com/article",
			"https://github.com/hackclub/arker",
		} {
			hasMediaItem := false
			for _, archiveType := range GetArchiveTypes(url) {
				if archiveType == ArchiveTypeYtDlp || archiveType == ArchiveTypeGalleryDl {
					hasMediaItem = true
				}
			}
			if hasMediaItem && !IsSocialMediaPostURL(url) {
				t.Errorf("GetArchiveTypes(%q) creates a media item but IsSocialMediaPostURL is false (cookies=%v)", url, cookies)
			}
		}
	}
}

// TikTok photo posts must go to gallery-dl and never to yt-dlp, which cannot
// download a slideshow at all.
func TestTikTokPhotoPostsRouteToGalleryDL(t *testing.T) {
	photoPosts := []string{
		"https://www.tiktok.com/@someone/photo/7412345678901234567",
		"https://www.tiktok.com/@someone/photo/7412345678901234567?is_from_webapp=1",
		"https://www.tiktok.com/share/photo/7412345678901234567",
		"https://m.tiktok.com/@someone/photo/7412345678901234567",
	}
	for _, url := range photoPosts {
		if !IsTikTokPhotoPostURL(url) {
			t.Errorf("IsTikTokPhotoPostURL(%q) = false, want true", url)
		}
		if !IsGalleryDLURL(url) {
			t.Errorf("IsGalleryDLURL(%q) = false, want true", url)
		}
		if IsVideoURL(url) {
			t.Errorf("IsVideoURL(%q) = true, want false (yt-dlp cannot fetch a slideshow)", url)
		}
	}

	// Video posts and short links keep their existing yt-dlp route.
	for _, url := range []string{
		"https://www.tiktok.com/@someone/video/7412345678901234567",
		"https://vm.tiktok.com/ZMabcdefg/",
		"https://vt.tiktok.com/ZSabcdefg/",
		"https://www.tiktok.com/t/ZTabcdefg/",
	} {
		if IsTikTokPhotoPostURL(url) {
			t.Errorf("IsTikTokPhotoPostURL(%q) = true, want false", url)
		}
		if !IsVideoURL(url) {
			t.Errorf("IsVideoURL(%q) = false, want true", url)
		}
		if IsGalleryDLURL(url) {
			t.Errorf("IsGalleryDLURL(%q) = true, want false (would pair two media items)", url)
		}
	}

	// A "/photo/" on another host must not route to TikTok's entry.
	for _, url := range []string{
		"https://example.com/@someone/photo/123",
		"https://nottiktok.com/@someone/photo/123",
	} {
		if IsTikTokPhotoPostURL(url) {
			t.Errorf("IsTikTokPhotoPostURL(%q) = true, want false", url)
		}
	}
}

// Short links are the one recognized TikTok shape whose post type is unknown
// until the redirect resolves. They stay on yt-dlp; the contract requirement
// is that a photo-post short link fails loudly, which means it must still be
// recognized as social so the API reports a failed social_post.
func TestTikTokShortLinksStayOnYtDlpAndStayRecognized(t *testing.T) {
	shortLinks := []string{
		"https://vm.tiktok.com/ZMabcdefg/",
		"https://vt.tiktok.com/ZSabcdefg/",
		"https://www.tiktok.com/t/ZTabcdefg/",
		"http://vm.tiktok.com/ZMabcdefg",
	}
	for _, url := range shortLinks {
		if !IsTikTokShortLinkURL(url) {
			t.Errorf("IsTikTokShortLinkURL(%q) = false, want true", url)
		}
		if !IsSocialMediaPostURL(url) {
			t.Errorf("IsSocialMediaPostURL(%q) = false, want true", url)
		}
		types := GetArchiveTypes(url)
		found := false
		for _, archiveType := range types {
			if archiveType == ArchiveTypeYtDlp {
				found = true
			}
			if archiveType == ArchiveTypeGalleryDl {
				t.Errorf("GetArchiveTypes(%q) = %v, want no gallery-dl item", url, types)
			}
		}
		if !found {
			t.Errorf("GetArchiveTypes(%q) = %v, want a yt-dlp item", url, types)
		}
	}

	for _, url := range []string{
		"https://www.tiktok.com/@someone/video/123",
		"https://www.tiktok.com/@someone/photo/123",
		"https://www.tiktok.com/@someone",
		"https://example.com/t/abc",
	} {
		if IsTikTokShortLinkURL(url) {
			t.Errorf("IsTikTokShortLinkURL(%q) = true, want false", url)
		}
	}
}
