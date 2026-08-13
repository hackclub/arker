package brightdata

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"arker/internal/archivers"
)

func TestBestVideoVariants(t *testing.T) {
	cases := []struct {
		name string
		urls []string
		want []string
	}{
		{
			name: "reddit packaged media resolutions",
			urls: []string{
				"https://packaged-media.redd.it/abc/pb/m2-res_392p.mp4?s=x",
				"https://packaged-media.redd.it/abc/pb/m2-res_1920p.mp4?s=x",
				"https://packaged-media.redd.it/abc/pb/m2-res_640p.mp4?s=x",
			},
			want: []string{"https://packaged-media.redd.it/abc/pb/m2-res_1920p.mp4?s=x"},
		},
		{
			name: "twitter dimension segments",
			urls: []string{
				"https://video.twimg.com/amplify_video/1/vid/avc1/640x360/a.mp4?tag=27",
				"https://video.twimg.com/amplify_video/1/vid/avc1/3840x2160/b.mp4?tag=27",
			},
			want: []string{"https://video.twimg.com/amplify_video/1/vid/avc1/3840x2160/b.mp4?tag=27"},
		},
		{
			name: "different posts stay separate",
			urls: []string{
				"https://packaged-media.redd.it/abc/pb/m2-res_480p.mp4",
				"https://packaged-media.redd.it/xyz/pb/m2-res_480p.mp4",
			},
			want: []string{
				"https://packaged-media.redd.it/abc/pb/m2-res_480p.mp4",
				"https://packaged-media.redd.it/xyz/pb/m2-res_480p.mp4",
			},
		},
		{
			name: "no resolution marker keeps every URL",
			urls: []string{"https://cdn.example/a.mp4", "https://cdn.example/b.mp4"},
			want: []string{"https://cdn.example/a.mp4", "https://cdn.example/b.mp4"},
		},
		{
			name: "first-seen order is preserved",
			urls: []string{
				"https://cdn.example/second/1080p/v.mp4",
				"https://packaged-media.redd.it/abc/pb/m2-res_480p.mp4",
			},
			want: []string{
				"https://cdn.example/second/1080p/v.mp4",
				"https://packaged-media.redd.it/abc/pb/m2-res_480p.mp4",
			},
		},
		{name: "empty", urls: nil, want: nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bestVideoVariants(c.urls)
			if len(got) != len(c.want) {
				t.Fatalf("got %v; want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("entry %d = %q; want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestVideoQualityLabel(t *testing.T) {
	cases := map[string]string{
		"https://packaged-media.redd.it/abc/pb/m2-res_1080p.mp4?s=x":        "1080p",
		"https://video.twimg.com/amplify_video/1/vid/avc1/1280x720/a.mp4":   "1280x720",
		"https://cdn.example/no-resolution-here.mp4":                        "",
		"https://v16-webapp-prime.us.tiktok.com/video/tos/useast8/x/?a=1":   "",
		"https://packaged-media.redd.it/abc/pb/m2-res_392p.mp4?m=1&v=2&s=3": "392p",
	}
	for rawURL, want := range cases {
		if got := videoQualityLabel(rawURL); got != want {
			t.Errorf("videoQualityLabel(%s) = %q; want %q", rawURL, got, want)
		}
	}
}

func TestStringsFromFieldShapes(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{"string list", []any{"https://cdn/a.jpg", "https://cdn/b.jpg"}, []string{"https://cdn/a.jpg", "https://cdn/b.jpg"}},
		{"object list", []any{map[string]any{"url": "https://cdn/a.jpg"}}, []string{"https://cdn/a.jpg"}},
		{"single string", "https://cdn/a.jpg", []string{"https://cdn/a.jpg"}},
		{"single object", map[string]any{"url": "https://cdn/a.jpg"}, []string{"https://cdn/a.jpg"}},
		{"mixed list", []any{"https://cdn/a.jpg", map[string]any{"url": "https://cdn/b.jpg"}}, []string{"https://cdn/a.jpg", "https://cdn/b.jpg"}},
		{"null", nil, nil},
		{"objects without a known key", []any{map[string]any{"height": float64(1)}}, nil},
		{"blank strings", []any{"", "  "}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stringsFromField(map[string]any{"media": c.value}, "media", "url", "video_url")
			if len(got) != len(c.want) {
				t.Fatalf("got %v; want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("entry %d = %q; want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// The bundle is laid out exactly like the native gallery-dl artifact:
// metadata.json first so a streaming reader finds it without scanning, the raw
// provider record beside it, and media stored uncompressed.
func TestBuildGalleryArchiveLayout(t *testing.T) {
	client, _ := newTestClient(t, newFakeNetwork())
	image := fakePNG(t)
	entries := []mediaEntry{
		{URL: "https://cdn.example/a.jpg", Type: "Photo"},
		{URL: "https://cdn.example/b.jpg", Type: "Photo"},
	}
	meta := &archivers.GalleryMetadata{SourceURL: "https://example.com/post", Extractor: "test"}
	record := map[string]any{"signed": "https://packaged-media.redd.it/a/pb/m2-res_480p.mp4?s=" + syntheticSecret}

	fetch := func(ctx context.Context, entry mediaEntry, dest string) (int64, error) {
		return int64(len(image)), os.WriteFile(dest, image, 0o644)
	}
	result, completeness, totalBytes, err := client.buildGalleryArchive(context.Background(), entries, meta, record, fetch, io.Discard)
	if err != nil {
		t.Fatalf("buildGalleryArchive: %v", err)
	}
	if completeness.State != archivers.CompletenessComplete || totalBytes != int64(2*len(image)) {
		t.Errorf("completeness = %+v, bytes = %d", completeness, totalBytes)
	}

	reader := resultZip(t, result)
	if reader.File[0].Name != "metadata.json" {
		t.Errorf("first entry = %q; want metadata.json", reader.File[0].Name)
	}
	names := strings.Join(zipNames(reader), " ")
	for _, want := range []string{"metadata.json", "brightdata.json", "001.jpg", "002.jpg"} {
		if !strings.Contains(names, want) {
			t.Errorf("zip is missing %s (has %s)", want, names)
		}
	}
	assertNoSignedParamsAtRest(t, "bundle brightdata.json", zipEntry(t, reader, "brightdata.json"))
}

// Nothing downloading at all is a failed rescue, not an empty archive.
func TestBuildGalleryArchiveFailsWhenNothingDownloads(t *testing.T) {
	client, _ := newTestClient(t, newFakeNetwork())
	entries := []mediaEntry{{URL: "https://cdn.example/a.jpg", Type: "Photo"}}
	meta := &archivers.GalleryMetadata{SourceURL: "https://example.com/post"}

	fetch := func(ctx context.Context, entry mediaEntry, dest string) (int64, error) {
		return 0, fmt.Errorf("refused")
	}
	_, _, _, err := client.buildGalleryArchive(context.Background(), entries, meta, map[string]any{}, fetch, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "all 1 media download") {
		t.Fatalf("error = %v; want an all-downloads-failed failure", err)
	}
}

// Provider records mix numeric types within one record: TikTok returns
// share_count as the string "99" beside digg_count as the number 1807. A
// reader that only accepts JSON numbers drops the string ones silently, and
// the archive then reports a post with no shares rather than reporting an
// error.
func TestIntFieldAcceptsNumericStrings(t *testing.T) {
	record := map[string]any{
		"digg_count":  float64(1807),
		"share_count": "99",
		"padded":      " 42 ",
		"approximate": "1.2K",
		"empty":       "",
		"object":      map[string]any{"n": 1},
	}
	cases := map[string]int64{
		"digg_count":  1807,
		"share_count": 99,
		"padded":      42,
	}
	for key, want := range cases {
		got := intField(record, key)
		if got == nil || *got != want {
			t.Errorf("intField(%s) = %v; want %d", key, got, want)
		}
	}
	// A rounded display string is not a count this can honestly report, and
	// neither is anything that is not a number at all.
	for _, key := range []string{"approximate", "empty", "object", "missing"} {
		if got := intField(record, key); got != nil {
			t.Errorf("intField(%s) = %d; want nil", key, *got)
		}
	}
	// The first key that yields a value wins, in order.
	if got := intField(record, "missing", "share_count", "digg_count"); got == nil || *got != 99 {
		t.Errorf("key precedence broken: %v", got)
	}
}
