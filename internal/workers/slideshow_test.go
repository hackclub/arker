package workers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

const slideshowVideoURL = "https://www.tiktok.com/@rchs.hack.club/video/7607629362342399245"

func TestRedirectSlideshowToGalleryRetypesTheItem(t *testing.T) {
	db := newWorkerTestDB(t)
	u := models.ArchivedURL{Original: slideshowVideoURL}
	db.Create(&u)
	capture := models.Capture{ArchivedURLID: u.ID, Timestamp: time.Now(), ShortID: "ANx6Y"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: utils.ArchiveTypeYtDlp, Status: "processing", RetryCount: 1}
	db.Create(&item)

	cause := errors.New("post is a photo slideshow, not a video: TikTok offered only the soundtrack")
	args := ArchiveJobArgs{ShortID: "ANx6Y", Type: utils.ArchiveTypeYtDlp, URL: slideshowVideoURL}
	// No River client in a bare test context: the hand-off re-types the item
	// and then reports that it could not queue the gallery job.
	err := redirectSlideshowToGallery(context.Background(), db, &item, args, cause)
	if err == nil || !strings.Contains(err.Error(), "River client") {
		t.Fatalf("err = %v, want a missing-client error", err)
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Type != utils.ArchiveTypeGalleryDl || got.Status != "pending" {
		t.Fatalf("item = type %q status %q, want gallery-dl pending", got.Type, got.Status)
	}
	logs, err := utils.ArchiveItemLogString(db, item.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "/photo/7607629362342399245") {
		t.Fatalf("item log does not name the photo URL: %s", logs)
	}
	var count int64
	db.Model(&models.ArchiveItem{}).Where("capture_id = ?", capture.ID).Count(&count)
	if count != 1 {
		t.Fatalf("capture has %d items, want the one re-typed item", count)
	}
}

func TestRedirectSlideshowToGalleryFailsTheVideoRowWhenGalleryExists(t *testing.T) {
	db := newWorkerTestDB(t)
	u := models.ArchivedURL{Original: slideshowVideoURL}
	db.Create(&u)
	capture := models.Capture{ArchivedURLID: u.ID, Timestamp: time.Now(), ShortID: "ANx6Y"}
	db.Create(&capture)
	gallery := models.ArchiveItem{CaptureID: capture.ID, Type: utils.ArchiveTypeGalleryDl, Status: "completed"}
	db.Create(&gallery)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: utils.ArchiveTypeYtDlp, Status: "processing"}
	db.Create(&item)

	err := redirectSlideshowToGallery(context.Background(), db, &item, ArchiveJobArgs{ShortID: "ANx6Y", Type: utils.ArchiveTypeYtDlp, URL: slideshowVideoURL}, archivers.ErrPhotoSlideshow)
	if err != nil {
		t.Fatal(err)
	}
	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Type != utils.ArchiveTypeYtDlp || got.Status != "failed" {
		t.Fatalf("item = type %q status %q, want yt-dlp failed", got.Type, got.Status)
	}
}

// The gallery item that replaced the video item is the capture of the post:
// an auto-detected request for the /video/ URL must find it rather than
// archive the same post again on every submission.
func TestFindOrCreateAcceptsGalleryInPlaceOfVideoForTikTokVideoURL(t *testing.T) {
	db := newQueueTestDB(t)
	capture := seedCapture(t, db, slideshowVideoURL, "ANx6Y", 24*time.Hour, map[string]string{
		"mhtml":      "completed",
		"screenshot": "completed",
	})
	item := models.ArchiveItem{
		CaptureID:    capture.ID,
		Type:         utils.ArchiveTypeGalleryDl,
		Status:       "completed",
		StorageKey:   "ANx6Y/gallery-dl-abc.zip",
		Completeness: archivers.CompletenessComplete,
	}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}

	got, err := FindOrCreateCapture(t.Context(), db, nil, slideshowVideoURL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != FindOrCreateFound || got.ShortID != "ANx6Y" {
		t.Fatalf("result = %+v, want the existing capture", got)
	}
	if n := countCaptures(t, db); n != 1 {
		t.Fatalf("capture count = %d, want 1", n)
	}

	// A gallery item never stands in for a video on a URL whose spelling
	// names the kind: an Instagram reel with only a gallery item is not the
	// reel.
	reel := "https://www.instagram.com/reel/abc123/"
	reelCapture := seedCapture(t, db, reel, "reel1", 24*time.Hour, map[string]string{"mhtml": "completed"})
	db.Create(&models.ArchiveItem{CaptureID: reelCapture.ID, Type: utils.ArchiveTypeGalleryDl, Status: "completed", StorageKey: "k", Completeness: archivers.CompletenessComplete})
	if !captureCoversTypes(&models.Capture{ArchivedURL: models.ArchivedURL{Original: reel}, ArchiveItems: []models.ArchiveItem{{Type: utils.ArchiveTypeGalleryDl, Status: "completed"}}}, []string{utils.ArchiveTypeYtDlp}) == false {
		t.Fatal("a gallery item covered the yt-dlp type for a reel")
	}
}
