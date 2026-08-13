package utils

import (
	"strings"
	"testing"
)

// yt-dlp 2026.08.04 refuses vimeo.com URLs without an account, so the fetch has
// to go through the player, which still answers logged out. Verified live on
// 2026-08-12: vimeo.com/76979871 fails with "The Vimeo extractor only works
// when logged-in", player.vimeo.com/video/76979871 returns
// "The New Vimeo Player (You Know, For Videos)", id 76979871, duration 62.
func TestVimeoPlayerURL(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		want      string
		rewritten bool
	}{
		{"main site video", "https://vimeo.com/76979871", "https://player.vimeo.com/video/76979871", true},
		{"with www", "https://www.vimeo.com/76979871", "https://player.vimeo.com/video/76979871", true},
		{"trailing slash", "https://vimeo.com/76979871/", "https://player.vimeo.com/video/76979871", true},
		{"http scheme", "http://vimeo.com/76979871", "https://player.vimeo.com/video/76979871", true},
		{"tracking query dropped", "https://vimeo.com/76979871?fl=pl&fe=sh", "https://player.vimeo.com/video/76979871", true},

		// The unlisted-video token must survive: without it the player 404s.
		{"unlisted hash path", "https://vimeo.com/76979871/a1b2c3d4e5", "https://player.vimeo.com/video/76979871?h=a1b2c3d4e5", true},
		{"unlisted hash query", "https://vimeo.com/76979871?h=a1b2c3d4e5", "https://player.vimeo.com/video/76979871?h=a1b2c3d4e5", true},
		{"all-numeric hash", "https://vimeo.com/76979871/1234567890", "https://player.vimeo.com/video/76979871?h=1234567890", true},

		// Container pages still name one video.
		{"channel video", "https://vimeo.com/channels/staffpicks/76979871", "https://player.vimeo.com/video/76979871", true},
		{"group video", "https://vimeo.com/groups/motion/videos/76979871", "https://player.vimeo.com/video/76979871", true},
		{"album video", "https://vimeo.com/album/1234567/video/76979871", "https://player.vimeo.com/video/76979871", true},

		// Already a player URL: passthrough, no double rewrite.
		{"player passthrough", "https://player.vimeo.com/video/76979871", "https://player.vimeo.com/video/76979871", false},
		{"player passthrough with hash", "https://player.vimeo.com/video/76979871?h=a1b2c3d4e5", "https://player.vimeo.com/video/76979871?h=a1b2c3d4e5", false},

		// Not a plain video page: leave it alone rather than fetch the wrong thing.
		{"user profile", "https://vimeo.com/user12345678", "https://vimeo.com/user12345678", false},
		{"named profile", "https://vimeo.com/staff", "https://vimeo.com/staff", false},
		{"profile videos tab", "https://vimeo.com/staff/videos", "https://vimeo.com/staff/videos", false},
		{"channel index", "https://vimeo.com/channels/staffpicks", "https://vimeo.com/channels/staffpicks", false},
		{"livestream event", "https://vimeo.com/event/1234567", "https://vimeo.com/event/1234567", false},
		{"on demand", "https://vimeo.com/ondemand/somefilm/1234567", "https://vimeo.com/ondemand/somefilm/1234567", false},
		{"home page", "https://vimeo.com/", "https://vimeo.com/", false},

		// Other hosts are none of Vimeo's business.
		{"youtube", "https://www.youtube.com/watch?v=abc", "https://www.youtube.com/watch?v=abc", false},
		{"host substring", "https://notvimeo.com/76979871", "https://notvimeo.com/76979871", false},
		{"vimeo in path", "https://example.com/vimeo.com/76979871", "https://example.com/vimeo.com/76979871", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, rewritten := VimeoPlayerURL(tt.url)
			if got != tt.want || rewritten != tt.rewritten {
				t.Errorf("VimeoPlayerURL(%q) = (%q, %v), want (%q, %v)", tt.url, got, rewritten, tt.want, tt.rewritten)
			}
		})
	}
}

// The rewrite must be invisible to everything except the fetch itself.
func TestYtDlpFetchURLOnlyRewritesVimeo(t *testing.T) {
	if got := YtDlpFetchURL("https://vimeo.com/76979871"); got != "https://player.vimeo.com/video/76979871" {
		t.Errorf("YtDlpFetchURL(vimeo) = %q, want the player URL", got)
	}

	unchanged := []string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ",
		"https://www.instagram.com/reel/ABC123/",
		"https://www.tiktok.com/@someone/video/123",
		"https://fb.watch/abc123/",
		"https://example.com/article",
		"https://player.vimeo.com/video/76979871",
	}
	for _, url := range unchanged {
		if got := YtDlpFetchURL(url); got != url {
			t.Errorf("YtDlpFetchURL(%q) = %q, want it unchanged", url, got)
		}
	}
}

func TestYtDlpRefererArgsForURL(t *testing.T) {
	args := YtDlpRefererArgsForURL("https://player.vimeo.com/video/76979871")
	if len(args) != 2 || args[0] != "--referer" || args[1] != "https://vimeo.com/" {
		t.Errorf("YtDlpRefererArgsForURL(player) = %v, want [--referer https://vimeo.com/]", args)
	}

	for _, url := range []string{
		"https://vimeo.com/76979871",
		"https://www.youtube.com/watch?v=abc",
		"https://example.com",
		"://not a url",
	} {
		if args := YtDlpRefererArgsForURL(url); args != nil {
			t.Errorf("YtDlpRefererArgsForURL(%q) = %v, want nil", url, args)
		}
	}
}

// Rewriting the fetch target must not change what Arker considers archived:
// the URL still routes to yt-dlp and is still recognized as a social post
// under both its original and its player form.
func TestVimeoRewriteKeepsRoutingIdentity(t *testing.T) {
	for _, url := range []string{
		"https://vimeo.com/76979871",
		"https://vimeo.com/76979871/a1b2c3d4e5",
		"https://player.vimeo.com/video/76979871",
	} {
		if !IsVimeoURL(url) {
			t.Errorf("IsVimeoURL(%q) = false, want true", url)
		}
		if !IsSocialMediaPostURL(url) {
			t.Errorf("IsSocialMediaPostURL(%q) = false, want true", url)
		}
		types := strings.Join(GetArchiveTypes(url), ",")
		if !strings.Contains(types, ArchiveTypeYtDlp) {
			t.Errorf("GetArchiveTypes(%q) = %s, want a yt-dlp item", url, types)
		}
	}
}
