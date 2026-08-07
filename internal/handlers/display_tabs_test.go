package handlers

import (
	"testing"

	"arker/internal/models"
	"arker/internal/utils"
)

func items(pairs ...string) []models.ArchiveItem {
	result := make([]models.ArchiveItem, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		result = append(result, models.ArchiveItem{Type: pairs[i], Status: pairs[i+1]})
	}
	return result
}

// The bug this fixes: an Instagram photo post got a yt-dlp item that always
// failed, and the viewer opened that tab by preference, so every visitor landed
// on a red "Archive Failed" page while a good screenshot sat one tab over.
func TestSelectDefaultTypePrefersCompletedOverPreferred(t *testing.T) {
	tests := []struct {
		name       string
		items      []models.ArchiveItem
		preference []string
		want       string
	}{
		{
			name:       "skips a failed preferred type for a completed one",
			items:      items("yt-dlp", "failed", "mhtml", "completed", "screenshot", "completed"),
			preference: []string{utils.ArchiveTypeYtDlp, utils.ArchiveTypeMHTML, utils.ArchiveTypeScreenshot},
			want:       utils.ArchiveTypeMHTML,
		},
		{
			name:       "uses the preferred type when it completed",
			items:      items("gallery-dl", "completed", "mhtml", "completed"),
			preference: []string{utils.ArchiveTypeGalleryDl, utils.ArchiveTypeMHTML},
			want:       utils.ArchiveTypeGalleryDl,
		},
		{
			name:       "falls back to preference order when nothing completed",
			items:      items("mhtml", "failed", "gallery-dl", "pending"),
			preference: []string{utils.ArchiveTypeGalleryDl, utils.ArchiveTypeMHTML},
			want:       utils.ArchiveTypeGalleryDl,
		},
		{
			name:       "falls back to any completed item outside the preference list",
			items:      items("itch", "completed"),
			preference: []string{utils.ArchiveTypeGalleryDl, utils.ArchiveTypeMHTML},
			want:       utils.ArchiveTypeItch,
		},
		{
			name:       "returns empty for a capture with no items",
			items:      nil,
			preference: []string{utils.ArchiveTypeMHTML},
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectDefaultType(tt.items, tt.preference); got != tt.want {
				t.Errorf("selectDefaultType = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDefaultTypePreferenceRoutesByURLKind(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"instagram post leads with gallery-dl", "https://www.instagram.com/p/ABC123/", utils.ArchiveTypeGalleryDl},
		{"instagram reel leads with yt-dlp", "https://www.instagram.com/reel/ABC123/", utils.ArchiveTypeYtDlp},
		{"youtube leads with yt-dlp", "https://www.youtube.com/watch?v=123", utils.ArchiveTypeYtDlp},
		{"git repo leads with git", "https://github.com/user/repo", utils.ArchiveTypeGit},
		{"itch leads with itch", "https://someone.itch.io/game", utils.ArchiveTypeItch},
		{"plain site leads with mhtml", "https://example.com", utils.ArchiveTypeMHTML},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preference := defaultTypePreference(tt.url)
			if len(preference) == 0 || preference[0] != tt.want {
				t.Errorf("defaultTypePreference(%q) = %v, want it to lead with %q", tt.url, preference, tt.want)
			}
		})
	}
}

func TestBuildTabsOrdersByPreferenceAndMarksActive(t *testing.T) {
	archiveItems := items(
		"mhtml", "completed",
		"screenshot", "completed",
		"gallery-dl", "failed",
	)
	preference := defaultTypePreference("https://www.instagram.com/p/ABC123/")

	tabs := buildTabs(archiveItems, preference, "screenshot")

	if len(tabs) != 3 {
		t.Fatalf("got %d tabs, want 3", len(tabs))
	}
	if tabs[0].URLType != utils.ArchiveTypeGalleryDl {
		t.Errorf("tabs[0] = %q, want gallery-dl first for an Instagram post", tabs[0].URLType)
	}
	if tabs[0].DisplayName != "Media" {
		t.Errorf("gallery-dl display name = %q, want Media", tabs[0].DisplayName)
	}
	// mhtml is stored as "mhtml" but linked as "web".
	if tabs[1].URLType != "web" || tabs[1].DisplayName != "Web" {
		t.Errorf("tabs[1] = %+v, want the mhtml item exposed as web/Web", tabs[1])
	}
	if !tabs[2].IsActive {
		t.Errorf("tabs[2] = %+v, want the screenshot tab marked active", tabs[2])
	}
	if tabs[0].IsActive || tabs[1].IsActive {
		t.Error("only the current tab may be marked active")
	}
}

// A capture may hold a type the preference list does not mention; it must still
// get a tab rather than disappearing from the viewer.
func TestBuildTabsIncludesUnlistedTypes(t *testing.T) {
	tabs := buildTabs(items("git", "completed"), []string{utils.ArchiveTypeMHTML}, "git")
	if len(tabs) != 1 || tabs[0].URLType != utils.ArchiveTypeGit {
		t.Fatalf("tabs = %+v, want a single git tab", tabs)
	}
}

// Permalinks handed out before the rename use /{shortid}/youtube and must keep
// resolving to the yt-dlp item forever.
func TestURLTypeMappingResolvesLegacyNames(t *testing.T) {
	if got := urlTypeToInternalType("youtube"); got != utils.ArchiveTypeYtDlp {
		t.Errorf("urlTypeToInternalType(youtube) = %q, want yt-dlp", got)
	}
	if got := urlTypeToInternalType("web"); got != utils.ArchiveTypeMHTML {
		t.Errorf("urlTypeToInternalType(web) = %q, want mhtml", got)
	}
	if got := urlTypeToInternalType("gallery-dl"); got != utils.ArchiveTypeGalleryDl {
		t.Errorf("urlTypeToInternalType(gallery-dl) = %q, want gallery-dl", got)
	}
	if got := internalTypeToURLType(utils.ArchiveTypeMHTML); got != "web" {
		t.Errorf("internalTypeToURLType(mhtml) = %q, want web", got)
	}
}
