package apify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"arker/internal/archivers"
	"arker/internal/utils"
)

// A slideshow behind a /video/ URL has no video to buy; the worker hands it
// to gallery-dl instead. The video fallback must not spend on it.
func TestFallbackSkippedWhenNativeFindsPhotoSlideshow(t *testing.T) {
	db := newTestDB(t)
	backend := &fakeBackend{supports: true}
	native := fmt.Errorf("%w: TikTok offered only the soundtrack", archivers.ErrPhotoSlideshow)
	arch := &FallbackArchiver{Primary: &fakePrimary{err: native}, Type: utils.ArchiveTypeYtDlp, Backend: backend}

	var log strings.Builder
	_, err := arch.Archive(context.Background(), "https://www.tiktok.com/@rchs.hack.club/video/7607629362342399245", &log, db, 1)
	if !errors.Is(err, archivers.ErrPhotoSlideshow) {
		t.Fatalf("err = %v, want ErrPhotoSlideshow", err)
	}
	if backend.called {
		t.Fatal("Apify fallback ran for a photo slideshow")
	}
	if !strings.Contains(log.String(), "photo slideshow") {
		t.Fatalf("log does not explain the skip: %s", log.String())
	}
}

// When the native run fails for an unrelated reason (a bot wall) and the paid
// record reveals the slideshow, the sentinel must survive the fallback's
// error wrapping so the worker can still hand the post to gallery-dl.
func TestFallbackSlideshowVerdictReachesTheWorker(t *testing.T) {
	db := newTestDB(t)
	backend := &fakeBackend{supports: true, err: fmt.Errorf("%w: TikTok post is a photo slideshow (isSlideshow)", archivers.ErrPhotoSlideshow)}
	arch := &FallbackArchiver{Primary: &fakePrimary{err: errors.New("yt-dlp cannot access video: bot check")}, Type: utils.ArchiveTypeYtDlp, Backend: backend}

	var log strings.Builder
	_, err := arch.Archive(context.Background(), "https://www.tiktok.com/@rchs.hack.club/video/7607629362342399245", &log, db, 1)
	if !backend.called {
		t.Fatal("fallback did not run")
	}
	if !errors.Is(err, archivers.ErrPhotoSlideshow) {
		t.Fatalf("err = %v, want ErrPhotoSlideshow to survive wrapping", err)
	}
}
