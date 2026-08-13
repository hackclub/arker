package utils

import "testing"

// canonicalCase is one spelling and the identity it must collapse to. Cases are
// grouped by platform so the table doubles as the documentation of what
// CanonicalizeArchiveURL claims to recognize.
type canonicalCase struct {
	name string
	in   string
	want string
}

var canonicalCases = []canonicalCase{
	// ---- YouTube -----------------------------------------------------------
	{"youtube watch", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
	{"youtube bare host", "https://youtube.com/watch?v=dQw4w9WgXcQ", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
	{"youtube mobile host", "https://m.youtube.com/watch?v=dQw4w9WgXcQ&app=desktop", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
	{"youtube http scheme", "http://www.youtube.com/watch?v=dQw4w9WgXcQ", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
	{"youtube uppercase host", "https://WWW.YouTube.com/watch?v=dQw4w9WgXcQ", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
	{"youtube default port", "https://www.youtube.com:443/watch?v=dQw4w9WgXcQ", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
	{"youtube tracking params", "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=42s&feature=share&pp=ygUJcmljaw%3D%3D&ab_channel=Rick", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
	{"youtube fragment", "https://www.youtube.com/watch?v=dQw4w9WgXcQ#t=30", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
	{"youtu.be short link", "https://youtu.be/dQw4w9WgXcQ", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
	{"youtu.be with si", "https://youtu.be/dQw4w9WgXcQ?si=Xy_9zQ&t=15", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
	{"youtube shorts", "https://www.youtube.com/shorts/abc123XYZ_-?si=track", "https://www.youtube.com/shorts/abc123XYZ_-"},
	{"youtube live", "https://youtube.com/live/abc123XYZ_-?feature=share", "https://www.youtube.com/live/abc123XYZ_-"},
	{"youtube playlist context kept", "https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PLabc&index=3", "https://www.youtube.com/watch?index=3&list=PLabc&v=dQw4w9WgXcQ"},
	{"youtube param order irrelevant", "https://www.youtube.com/watch?list=PLabc&v=dQw4w9WgXcQ&index=3", "https://www.youtube.com/watch?index=3&list=PLabc&v=dQw4w9WgXcQ"},

	// ---- Instagram ---------------------------------------------------------
	{"instagram reel", "https://www.instagram.com/reel/CxYz-123_/", "https://www.instagram.com/reel/CxYz-123_/"},
	{"instagram reel no slash", "https://instagram.com/reel/CxYz-123_", "https://www.instagram.com/reel/CxYz-123_/"},
	{"instagram reels plural", "https://www.instagram.com/reels/CxYz-123_/", "https://www.instagram.com/reel/CxYz-123_/"},
	{"instagram igsh", "https://www.instagram.com/reel/CxYz-123_/?igsh=MWx0dg%3D%3D&utm_source=ig_web_copy_link", "https://www.instagram.com/reel/CxYz-123_/"},
	{"instagram mobile host", "https://m.instagram.com/p/CxYz-123_/", "https://www.instagram.com/p/CxYz-123_/"},
	{"instagram username prefix", "https://www.instagram.com/therock/p/CxYz-123_/", "https://www.instagram.com/p/CxYz-123_/"},
	{"instagram tv", "https://www.instagram.com/tv/CxYz-123_/?igshid=abc", "https://www.instagram.com/tv/CxYz-123_/"},

	// ---- X / Twitter -------------------------------------------------------
	{"x status", "https://x.com/jack/status/20", "https://x.com/i/web/status/20"},
	{"twitter status", "https://twitter.com/jack/status/20", "https://x.com/i/web/status/20"},
	{"twitter mobile", "https://mobile.twitter.com/jack/status/20?s=20&t=abcDEF", "https://x.com/i/web/status/20"},
	{"twitter statuses", "https://twitter.com/jack/statuses/20", "https://x.com/i/web/status/20"},
	{"x i web status", "https://x.com/i/web/status/20", "https://x.com/i/web/status/20"},
	{"x photo lightbox", "https://x.com/jack/status/20/photo/1", "https://x.com/i/web/status/20"},
	{"x video lightbox", "https://twitter.com/jack/status/20/video/1?ref_src=twsrc%5Etfw", "https://x.com/i/web/status/20"},

	// ---- TikTok ------------------------------------------------------------
	{"tiktok video", "https://www.tiktok.com/@user.name/video/7412345678901234567", "https://www.tiktok.com/@user.name/video/7412345678901234567"},
	{"tiktok share params", "https://www.tiktok.com/@User.Name/video/7412345678901234567?is_from_webapp=1&sender_device=pc&web_id=12345", "https://www.tiktok.com/@user.name/video/7412345678901234567"},
	{"tiktok photo post", "https://www.tiktok.com/@user/photo/7412345678901234567", "https://www.tiktok.com/@user/photo/7412345678901234567"},
	{"tiktok mobile host", "https://m.tiktok.com/@user/video/7412345678901234567", "https://www.tiktok.com/@user/video/7412345678901234567"},

	// ---- Reddit ------------------------------------------------------------
	{"reddit post with slug", "https://www.reddit.com/r/golang/comments/1abc23/some_title_here/", "https://www.reddit.com/comments/1abc23/"},
	{"reddit post without slug", "https://old.reddit.com/r/golang/comments/1abc23/", "https://www.reddit.com/comments/1abc23/"},
	{"reddit new host", "https://new.reddit.com/r/golang/comments/1abc23/some_title_here", "https://www.reddit.com/comments/1abc23/"},
	{"reddit mobile host", "https://m.reddit.com/r/golang/comments/1abc23/some_title_here/?utm_source=share&utm_medium=web2x&context=3", "https://www.reddit.com/comments/1abc23/"},
	{"reddit id only path", "https://www.reddit.com/comments/1abc23", "https://www.reddit.com/comments/1abc23/"},
	{"reddit short link", "https://redd.it/1abc23", "https://www.reddit.com/comments/1abc23/"},
	{"reddit comment legacy shape", "https://www.reddit.com/r/golang/comments/1abc23/some_title/def456/", "https://www.reddit.com/comments/1abc23/comment/def456/"},
	{"reddit comment current shape", "https://www.reddit.com/r/golang/comments/1abc23/comment/def456/?context=3", "https://www.reddit.com/comments/1abc23/comment/def456/"},

	// ---- Bluesky -----------------------------------------------------------
	{"bluesky handle", "https://bsky.app/profile/Alice.bsky.social/post/3kabc123", "https://bsky.app/profile/alice.bsky.social/post/3kabc123"},
	{"bluesky did preserved", "https://bsky.app/profile/did:plc:AbC123/post/3kabc123", "https://bsky.app/profile/did:plc:AbC123/post/3kabc123"},

	// ---- Vimeo -------------------------------------------------------------
	{"vimeo plain", "https://vimeo.com/123456789", "https://vimeo.com/123456789"},
	{"vimeo www", "https://www.vimeo.com/123456789?share=copy", "https://vimeo.com/123456789"},
	{"vimeo unlisted hash", "https://vimeo.com/123456789/abcdef1234", "https://vimeo.com/123456789/abcdef1234"},
	{"vimeo player", "https://player.vimeo.com/video/123456789?autoplay=1&title=0&byline=0", "https://vimeo.com/123456789"},
	{"vimeo player hash to path", "https://player.vimeo.com/video/123456789?h=abcdef1234&badge=0", "https://vimeo.com/123456789/abcdef1234"},
	{"vimeo channel", "https://vimeo.com/channels/staffpicks/123456789", "https://vimeo.com/123456789"},
	{"vimeo group", "https://vimeo.com/groups/motion/videos/123456789", "https://vimeo.com/123456789"},

	// ---- Facebook ----------------------------------------------------------
	{"facebook watch", "https://www.facebook.com/watch/?v=1234567890", "https://www.facebook.com/watch/?v=1234567890"},
	{"facebook watch no slash", "https://www.facebook.com/watch?v=1234567890&mibextid=abc", "https://www.facebook.com/watch/?v=1234567890"},
	{"facebook video.php", "https://web.facebook.com/video.php?v=1234567890", "https://www.facebook.com/watch/?v=1234567890"},
	{"facebook page video", "https://www.facebook.com/SomePage/videos/1234567890/?rdid=xyz", "https://www.facebook.com/somepage/videos/1234567890/"},
	{"facebook page video with slug", "https://m.facebook.com/SomePage/videos/a-nice-title/1234567890/", "https://www.facebook.com/somepage/videos/1234567890/"},
	{"facebook reel", "https://www.facebook.com/reel/1234567890?mibextid=zz", "https://www.facebook.com/reel/1234567890/"},
	{"facebook page reel", "https://www.facebook.com/SomePage/reel/1234567890/", "https://www.facebook.com/reel/1234567890/"},
	{"facebook cft tracking", "https://www.facebook.com/reel/1234567890/?__cft__[0]=AZX&__tn__=-R", "https://www.facebook.com/reel/1234567890/"},
	{"fb.watch opaque", "https://fb.watch/aBc-1_2/?mibextid=zz", "https://fb.watch/aBc-1_2/"},
}

// unchangedCases must come back byte-for-byte identical. Ordinary URLs are the
// bulk of what Arker archives and canonicalization must be invisible to them;
// the recognized-host-but-unrecognized-shape entries pin the "be conservative"
// rule.
var unchangedCases = []canonicalCase{
	{"ordinary url", "https://example.com/page?b=2&a=1", ""},
	{"ordinary url trailing slash", "https://example.com/page/", ""},
	{"ordinary url with tracking", "https://example.com/page?utm_source=x", ""},
	{"ordinary uppercase host", "https://Example.COM/Page", ""},
	{"git url", "https://github.com/hackclub/arker", ""},
	{"itch url", "https://someone.itch.io/game", ""},
	{"empty", "", ""},
	{"not a url", "not a url", ""},
	{"non http scheme", "ftp://youtube.com/watch?v=dQw4w9WgXcQ", ""},
	{"nonstandard port", "https://www.youtube.com:8443/watch?v=dQw4w9WgXcQ", ""},
	{"youtube channel", "https://www.youtube.com/@rickastley", ""},
	{"youtube playlist page", "https://www.youtube.com/playlist?list=PLabc", ""},
	{"youtube embed", "https://www.youtube.com/embed/dQw4w9WgXcQ", ""},
	{"youtube nocookie", "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ", ""},
	{"youtube music", "https://music.youtube.com/watch?v=dQw4w9WgXcQ", ""},
	{"instagram profile", "https://www.instagram.com/therock/", ""},
	{"instagram stories", "https://www.instagram.com/stories/therock/123456/", ""},
	{"instagram post subpage", "https://www.instagram.com/p/CxYz-123_/liked_by/", ""},
	{"x profile", "https://x.com/jack", ""},
	{"x status analytics", "https://x.com/jack/status/20/analytics", ""},
	{"tiktok short vm", "https://vm.tiktok.com/ZMabc123/", ""},
	{"tiktok short vt", "https://vt.tiktok.com/ZSabc123/", ""},
	{"tiktok short t", "https://www.tiktok.com/t/ZTabc123/", ""},
	{"tiktok profile", "https://www.tiktok.com/@user", ""},
	{"reddit share link", "https://www.reddit.com/r/golang/s/AbCdEf1234", ""},
	{"reddit subreddit", "https://www.reddit.com/r/golang/", ""},
	{"reddit image host", "https://i.redd.it/abcdef123.jpg", ""},
	{"reddit video host", "https://v.redd.it/abcdef123", ""},
	{"reddit preview host", "https://preview.redd.it/abcdef123.jpg?width=640", ""},
	{"bluesky profile", "https://bsky.app/profile/alice.bsky.social", ""},
	{"vimeo ondemand", "https://vimeo.com/ondemand/somefilm", ""},
	{"vimeo user page", "https://vimeo.com/user12345678", ""},
	{"facebook photo post", "https://www.facebook.com/photo?fbid=123&set=a.456", ""},
	{"facebook story", "https://www.facebook.com/story.php?story_fbid=123&id=456", ""},
	{"facebook profile", "https://www.facebook.com/SomePage", ""},
}

func TestCanonicalizeArchiveURL(t *testing.T) {
	for _, tc := range canonicalCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalizeArchiveURL(tc.in); got != tc.want {
				t.Errorf("CanonicalizeArchiveURL(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalizeArchiveURLLeavesUnrecognizedUntouched(t *testing.T) {
	for _, tc := range unchangedCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalizeArchiveURL(tc.in); got != tc.in {
				t.Errorf("CanonicalizeArchiveURL(%q) = %q, want the input unchanged", tc.in, got)
			}
		})
	}
}

// TestCanonicalizeArchiveURLIsIdempotent guards the property the database
// depends on: the stored canonical_url of a row must not drift when the same
// value is canonicalized again by a later lookup or backfill pass.
func TestCanonicalizeArchiveURLIsIdempotent(t *testing.T) {
	for _, tc := range append(append([]canonicalCase{}, canonicalCases...), unchangedCases...) {
		t.Run(tc.name, func(t *testing.T) {
			once := CanonicalizeArchiveURL(tc.in)
			if twice := CanonicalizeArchiveURL(once); twice != once {
				t.Errorf("not idempotent for %q:\nonce  %q\ntwice %q", tc.in, once, twice)
			}
		})
	}
}

// TestCanonicalizeArchiveURLUnifiesSpellings states the point of the whole file
// in the terms find-or-create cares about: these groups must each collapse to
// one identity.
func TestCanonicalizeArchiveURLUnifiesSpellings(t *testing.T) {
	groups := map[string][]string{
		"youtube video": {
			"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			"https://youtube.com/watch?v=dQw4w9WgXcQ&feature=youtu.be",
			"https://m.youtube.com/watch?v=dQw4w9WgXcQ",
			"https://youtu.be/dQw4w9WgXcQ?si=abcDEF",
			"http://www.youtube.com/watch?v=dQw4w9WgXcQ&t=120",
		},
		"instagram reel": {
			"https://www.instagram.com/reel/CxYz-123_/",
			"https://instagram.com/reel/CxYz-123_",
			"https://www.instagram.com/reels/CxYz-123_/",
			"https://m.instagram.com/reel/CxYz-123_/?igsh=abc%3D%3D",
			"https://www.instagram.com/zachlatta/reel/CxYz-123_/",
		},
		"x status": {
			"https://x.com/jack/status/20",
			"https://twitter.com/jack/status/20?s=46&t=xyz",
			"https://mobile.twitter.com/Jack/status/20",
			"https://x.com/i/web/status/20",
			"https://x.com/jack/status/20/photo/1",
		},
		"reddit post": {
			"https://www.reddit.com/r/golang/comments/1abc23/some_title/",
			"https://old.reddit.com/r/golang/comments/1abc23/",
			"https://www.reddit.com/comments/1abc23",
			"https://redd.it/1abc23",
		},
		"vimeo unlisted": {
			"https://vimeo.com/123456789/abcdef1234",
			"https://player.vimeo.com/video/123456789?h=abcdef1234",
			"https://www.vimeo.com/123456789/abcdef1234?share=copy",
		},
		"facebook watch": {
			"https://www.facebook.com/watch/?v=1234567890",
			"https://www.facebook.com/watch?v=1234567890&mibextid=abc",
			"https://m.facebook.com/watch/?v=1234567890",
			"https://web.facebook.com/video.php?v=1234567890",
		},
	}
	for name, spellings := range groups {
		t.Run(name, func(t *testing.T) {
			want := CanonicalizeArchiveURL(spellings[0])
			for _, spelling := range spellings[1:] {
				if got := CanonicalizeArchiveURL(spelling); got != want {
					t.Errorf("%q canonicalized to %q, want %q (same as %q)", spelling, got, want, spellings[0])
				}
			}
		})
	}
}

// TestCanonicalizeArchiveURLKeepsDistinctPostsDistinct is the safety half of the
// contract. A collision here means find-or-create hands back an archive of the
// wrong post, which is worse than never deduping at all.
func TestCanonicalizeArchiveURLKeepsDistinctPostsDistinct(t *testing.T) {
	distinct := [][2]string{
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "https://www.youtube.com/watch?v=oHg5SJYRHA0"},
		{"https://www.youtube.com/watch?v=abc123XYZ_-", "https://www.youtube.com/shorts/abc123XYZ_-"},
		{"https://www.youtube.com/shorts/abc123XYZ_-", "https://www.youtube.com/live/abc123XYZ_-"},
		{"https://www.instagram.com/p/CxYz-123_/", "https://www.instagram.com/reel/CxYz-123_/"},
		{"https://www.instagram.com/p/CxYz-123_/", "https://www.instagram.com/tv/CxYz-123_/"},
		{"https://x.com/jack/status/20", "https://x.com/jack/status/21"},
		{"https://www.tiktok.com/@user/video/74123", "https://www.tiktok.com/@user/photo/74123"},
		{"https://www.tiktok.com/@user/video/74123", "https://www.tiktok.com/@other/video/74123"},
		{"https://www.reddit.com/r/golang/comments/1abc23/", "https://www.reddit.com/r/golang/comments/1abc23/slug/def456/"},
		{"https://vimeo.com/123456789", "https://vimeo.com/123456789/abcdef1234"},
		{"https://bsky.app/profile/alice.bsky.social/post/3kabc", "https://bsky.app/profile/bob.bsky.social/post/3kabc"},
		{"https://www.facebook.com/reel/123", "https://www.facebook.com/watch/?v=123"},
		{"https://www.facebook.com/pagea/videos/123/", "https://www.facebook.com/pageb/videos/123/"},
	}
	for _, pair := range distinct {
		a, b := CanonicalizeArchiveURL(pair[0]), CanonicalizeArchiveURL(pair[1])
		if a == b {
			t.Errorf("%q and %q both canonicalized to %q", pair[0], pair[1], a)
		}
	}
}

// TestCanonicalizeArchiveURLKeepsUnknownParams pins the conservative rule: an
// unrecognized parameter is load-bearing until proven otherwise.
func TestCanonicalizeArchiveURLKeepsUnknownParams(t *testing.T) {
	cases := []canonicalCase{
		{"unknown youtube param", "https://www.youtube.com/watch?v=dQw4w9WgXcQ&mystery=1", "https://www.youtube.com/watch?mystery=1&v=dQw4w9WgXcQ"},
		{"instagram carousel index", "https://www.instagram.com/p/CxYz-123_/?img_index=2", "https://www.instagram.com/p/CxYz-123_/?img_index=2"},
		{"instagram language", "https://www.instagram.com/p/CxYz-123_/?hl=ja", "https://www.instagram.com/p/CxYz-123_/?hl=ja"},
		{"reddit comment sort", "https://www.reddit.com/r/golang/comments/1abc23/slug/?sort=new", "https://www.reddit.com/comments/1abc23/?sort=new"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalizeArchiveURL(tc.in); got != tc.want {
				t.Errorf("CanonicalizeArchiveURL(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}
