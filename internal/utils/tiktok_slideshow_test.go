package utils

import "testing"

func TestTikTokPhotoPostURL(t *testing.T) {
	cases := map[string]string{
		"https://www.tiktok.com/@rchs.hack.club/video/7607629362342399245":         "https://www.tiktok.com/@rchs.hack.club/photo/7607629362342399245",
		"https://www.tiktok.com/@rchs.hack.club/video/7607629362342399245?lang=en": "https://www.tiktok.com/@rchs.hack.club/photo/7607629362342399245?lang=en",
		"https://www.tiktok.com/@rchs.hack.club/photo/7607629362342399245":         "https://www.tiktok.com/@rchs.hack.club/photo/7607629362342399245",
		"https://vm.tiktok.com/ZMabcdefg/":                                         "https://vm.tiktok.com/ZMabcdefg/",
		"https://www.youtube.com/watch?v=abc":                                      "https://www.youtube.com/watch?v=abc",
	}
	for in, want := range cases {
		if got := TikTokPhotoPostURL(in); got != want {
			t.Errorf("TikTokPhotoPostURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGalleryStandsInForVideoOnlyForTikTokVideoSpellings(t *testing.T) {
	yes := []string{
		"https://www.tiktok.com/@rchs.hack.club/video/7607629362342399245",
		"https://vm.tiktok.com/ZMabcdefg/",
	}
	no := []string{
		"https://www.tiktok.com/@rchs.hack.club/photo/7607629362342399245",
		"https://www.instagram.com/reel/abc/",
		"https://www.youtube.com/watch?v=abc",
	}
	for _, u := range yes {
		if !GalleryStandsInForVideo(u) {
			t.Errorf("GalleryStandsInForVideo(%q) = false", u)
		}
	}
	for _, u := range no {
		if GalleryStandsInForVideo(u) {
			t.Errorf("GalleryStandsInForVideo(%q) = true", u)
		}
	}
}

func TestMediaFetchURLForType(t *testing.T) {
	video := "https://www.tiktok.com/@rchs.hack.club/video/7607629362342399245"
	if got := MediaFetchURLForType(ArchiveTypeGalleryDl, video); got != "https://www.tiktok.com/@rchs.hack.club/photo/7607629362342399245" {
		t.Fatalf("gallery-dl fetch URL = %q", got)
	}
	if got := MediaFetchURLForType(ArchiveTypeYtDlp, video); got != video {
		t.Fatalf("yt-dlp fetch URL = %q, want unchanged", got)
	}
	reel := "https://www.instagram.com/p/abc/"
	if got := MediaFetchURLForType(ArchiveTypeGalleryDl, reel); got != reel {
		t.Fatalf("instagram gallery fetch URL = %q, want unchanged", got)
	}
}
