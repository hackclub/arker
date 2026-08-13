// Package testfixtures holds the sanitized extractor outputs behind Arker's
// social-archive contract tests, plus the fake yt-dlp/gallery-dl harness that
// replays them.
//
// It exists so the contract tests can drive the *real* archiver code — the
// download loop, the ZIP builder, the metadata normalizer, the completeness
// check — with no network and no dependence on a platform staying reachable.
// A fixture is a recording of what an extractor actually emits; a test that
// asserts against one is asserting against the real world, not an idealized
// schema.
//
// This is a test-only package by convention (nothing outside _test.go files
// imports it), in the same spirit as net/http/httptest.
//
// See docs/testing-contract.md for the sanitization rules and for how to
// regenerate the corpus.
package testfixtures

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Origin records how a fixture was obtained. It is not decoration: a live
// capture proves the shape is real today, while a constructed one is only as
// good as the documentation behind it, and a reviewer needs to tell them
// apart without digging.
type Origin string

const (
	// OriginLive is a sanitized recording of a real anonymous run of the
	// locally installed extractor.
	OriginLive Origin = "live"
	// OriginConstructed is reconstructed from the field shapes Arker's own
	// mapping code documents, because the platform is unreachable
	// anonymously or is off-limits by policy (Instagram).
	OriginConstructed Origin = "constructed"
)

// Tool names, matching the archive type each fixture feeds.
const (
	ToolYtDlp     = "yt-dlp"
	ToolGalleryDl = "gallery-dl"
)

// Case is one platform/post-type entry of the contract matrix in
// docs/social-contract/CONTRACT-GAPS.md.
type Case struct {
	// Name is the fixture directory or file stem under testdata.
	Name string
	// Platform and PostType describe the matrix row this fixture pins.
	Platform string
	PostType string
	// Tool is the extractor Arker routes this URL to.
	Tool string
	// URL is a URL of the shape Arker's router recognizes for this case.
	// Tests use it so routing and fixture stay in agreement.
	URL string
	// Origin is how the fixture was obtained.
	Origin Origin
	// DeclaredMedia is the number of media files the post declares. For
	// gallery-dl fixtures this is the sidecar "count" key, which is what a
	// completeness check has to compare the downloaded file count against.
	// Zero means the extractor declares no count for this site.
	DeclaredMedia int
	// Note records anything load-bearing about the case.
	Note string
}

// cases is the contract matrix. Adding a platform means adding a row here and
// the matching fixture files; the tests iterate this list, so a new row is
// covered everywhere at once.
var cases = []Case{
	{
		Name: "youtube_regular", Platform: "youtube", PostType: "regular video",
		Tool: ToolYtDlp, URL: "https://www.youtube.com/watch?v=aqz-KE-bpKQ",
		Origin: OriginLive, DeclaredMedia: 1,
	},
	{
		Name: "youtube_shorts", Platform: "youtube", PostType: "shorts",
		Tool: ToolYtDlp, URL: "https://www.youtube.com/shorts/Ujmx5PsT1IY",
		Origin: OriginLive, DeclaredMedia: 1,
	},
	{
		Name: "vimeo_video", Platform: "vimeo", PostType: "video",
		Tool: ToolYtDlp, URL: "https://vimeo.com/76979871",
		Origin: OriginConstructed, DeclaredMedia: 1,
		Note: "yt-dlp's Vimeo extractor now requires credentials, so this cannot be captured anonymously",
	},
	{
		Name: "instagram_reel", Platform: "instagram", PostType: "reel",
		Tool: ToolYtDlp, URL: "https://www.instagram.com/reel/DbktPO1Eopi/",
		Origin: OriginConstructed, DeclaredMedia: 1,
		Note: "Arker never makes live Instagram requests from a dev machine",
	},
	{
		Name: "tiktok_video", Platform: "tiktok", PostType: "video",
		Tool: ToolYtDlp, URL: "https://www.tiktok.com/@arkerfixture/video/7106594312292453675",
		Origin: OriginConstructed, DeclaredMedia: 1,
		Note: "TikTok serves a bot challenge to anonymous yt-dlp runs",
	},
	{
		Name: "facebook_video", Platform: "facebook", PostType: "video",
		Tool: ToolYtDlp, URL: "https://www.facebook.com/watch/?v=1004422781373452",
		Origin: OriginConstructed, DeclaredMedia: 1,
		Note: "Facebook video is behind a login wall",
	},

	{
		Name: "instagram_image", Platform: "instagram", PostType: "single image",
		Tool: ToolGalleryDl, URL: "https://www.instagram.com/p/DbkSINGLEim/",
		Origin: OriginConstructed, DeclaredMedia: 1,
	},
	{
		Name: "instagram_carousel", Platform: "instagram", PostType: "10-slide carousel with video slide",
		Tool: ToolGalleryDl, URL: "https://www.instagram.com/p/DbktPO1Eopi/",
		Origin: OriginConstructed, DeclaredMedia: 10,
		Note: "slide 4 is a video; this is the G1 partial-download fixture",
	},
	{
		Name: "tiktok_photo", Platform: "tiktok", PostType: "photo post",
		Tool: ToolGalleryDl, URL: "https://www.tiktok.com/@arkerfixture/photo/7301234567890123456",
		Origin: OriginConstructed, DeclaredMedia: 3,
		Note: "G3a: not routed to gallery-dl yet",
	},
	{
		Name: "x_image", Platform: "x", PostType: "image post",
		Tool: ToolGalleryDl, URL: "https://x.com/arkerfixture/status/1929384756102938112",
		Origin: OriginConstructed, DeclaredMedia: 1,
		Note: "gallery-dl serves nothing for x.com without a cookie jar",
	},
	{
		Name: "x_video", Platform: "x", PostType: "video post",
		Tool: ToolGalleryDl, URL: "https://x.com/arkerfixture/status/1929384756102938113",
		Origin: OriginConstructed, DeclaredMedia: 1,
		Note: "G3b: proves the video bytes are stored, not a poster frame",
	},
	{
		Name: "reddit_image", Platform: "reddit", PostType: "image submission",
		Tool: ToolGalleryDl, URL: "https://www.reddit.com/r/aww/comments/1abcxyz/a_public_reddit_submission/",
		Origin: OriginConstructed, DeclaredMedia: 1,
		Note: "reddit.com blocks the JSON API from this network",
	},
	{
		Name: "reddit_gallery", Platform: "reddit", PostType: "gallery submission",
		Tool: ToolGalleryDl, URL: "https://www.reddit.com/r/aww/comments/1galxyz/a_three_image_reddit_gallery/",
		Origin: OriginConstructed, DeclaredMedia: 3,
	},
	{
		Name: "reddit_video", Platform: "reddit", PostType: "v.redd.it video",
		Tool: ToolGalleryDl, URL: "https://www.reddit.com/r/aww/comments/1vidxyz/a_vreddit_video_submission/",
		Origin: OriginConstructed, DeclaredMedia: 1,
		Note: "G3c: DASH video with a separate audio stream",
	},
	{
		Name: "bluesky_image", Platform: "bluesky", PostType: "image post",
		Tool: ToolGalleryDl, URL: "https://bsky.app/profile/bsky.app/post/3msqpuobiwk2t",
		Origin: OriginLive, DeclaredMedia: 1,
	},
	{
		Name: "bluesky_video", Platform: "bluesky", PostType: "video post",
		Tool: ToolGalleryDl, URL: "https://bsky.app/profile/bsky.app/post/3mk4lzkrnk22d",
		Origin: OriginLive, DeclaredMedia: 1,
		Note: "G3d: proves the video blob downloads",
	},
	{
		Name: "imgur_album", Platform: "imgur", PostType: "album",
		Tool: ToolGalleryDl, URL: "https://imgur.com/a/zJjxIyO",
		Origin: OriginConstructed, DeclaredMedia: 3,
	},
	{
		Name: "flickr_photo", Platform: "flickr", PostType: "photo",
		Tool: ToolGalleryDl, URL: "https://www.flickr.com/photos/45218841@N07/55459534522/",
		Origin: OriginLive, DeclaredMedia: 1,
	},
}

// Cases returns every matrix entry.
func Cases() []Case {
	out := make([]Case, len(cases))
	copy(out, cases)
	return out
}

// CasesForTool returns the matrix entries routed to one extractor.
func CasesForTool(tool string) []Case {
	var out []Case
	for _, c := range cases {
		if c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

// Lookup returns the named case.
func Lookup(t *testing.T, name string) Case {
	t.Helper()
	for _, c := range cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no fixture case named %q", name)
	return Case{}
}

// root locates the testdata directory from this source file rather than the
// working directory, so a test in any package can load fixtures without
// knowing where it was run from.
func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate testfixtures package directory")
	}
	return filepath.Join(filepath.Dir(file), "testdata")
}

// InfoJSON returns the sanitized yt-dlp info JSON for a yt-dlp case.
func (c Case) InfoJSON(t *testing.T) []byte {
	t.Helper()
	if c.Tool != ToolYtDlp {
		t.Fatalf("case %s is a %s fixture, not yt-dlp", c.Name, c.Tool)
	}
	path := filepath.Join(root(t), "ytdlp", c.Name+".info.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return data
}

// Sidecar is one gallery-dl output pair: the media file gallery-dl would have
// written and the JSON metadata sidecar it writes beside it.
type Sidecar struct {
	// MediaName is the flat numeric name Arker's gallery-dl flags produce,
	// e.g. "001.jpg" (gallery_dl.go passes -f "{num:>03}.{extension}").
	MediaName string
	// SidecarName is always MediaName + ".json".
	SidecarName string
	// Data is the sanitized sidecar JSON.
	Data []byte
}

// IsVideo reports whether this slide is a video, by extension.
func (s Sidecar) IsVideo() bool {
	switch strings.ToLower(filepath.Ext(s.MediaName)) {
	case ".mp4", ".webm", ".mov", ".m4v":
		return true
	}
	return false
}

// Sidecars returns a gallery-dl case's slides in slide order.
func (c Case) Sidecars(t *testing.T) []Sidecar {
	t.Helper()
	if c.Tool != ToolGalleryDl {
		t.Fatalf("case %s is a %s fixture, not gallery-dl", c.Name, c.Tool)
	}
	dir := filepath.Join(root(t), "gallerydl", c.Name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixture dir %s: %v", dir, err)
	}
	var out []Sidecar
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read sidecar %s: %v", entry.Name(), err)
		}
		out = append(out, Sidecar{
			MediaName:   strings.TrimSuffix(entry.Name(), ".json"),
			SidecarName: entry.Name(),
			Data:        data,
		})
	}
	// Slide order is the archive's order, so sort by name: 001 before 010.
	sort.Slice(out, func(i, j int) bool { return out[i].MediaName < out[j].MediaName })
	if len(out) == 0 {
		t.Fatalf("fixture %s has no sidecars", c.Name)
	}
	return out
}
