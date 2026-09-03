package utils

import "testing"

func TestExtractGitRepoURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "Tangled root",
			in:   "https://tangled.org/dunkirk.sh/akami",
			want: "https://tangled.org/dunkirk.sh/akami",
		},
		{
			name: "Tangled at-handle blob",
			in:   "https://tangled.org/@dunkirk.sh/akami/blob/main/README.md?code=true#L10",
			want: "https://tangled.org/@dunkirk.sh/akami",
		},
		{
			name: "Tangled commit",
			in:   "https://tangled.org/dunkirk.sh/akami/commit/45f6aaec",
			want: "https://tangled.org/dunkirk.sh/akami",
		},
		{
			name: "Tangled encoded branch path",
			in:   "https://tangled.org/recaptime.dev/knot-docker-nest/blob/recaptime-dev%2Fmain/readme.md",
			want: "https://tangled.org/recaptime.dev/knot-docker-nest",
		},
		{
			name: "GitHub tree",
			in:   "https://github.com/hackclub/arker/tree/main/internal",
			want: "https://github.com/hackclub/arker",
		},
		{
			name: "unknown forge keeps query",
			in:   "https://git.example.com/team/repo.git?token=value#readme",
			want: "https://git.example.com/team/repo.git?token=value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractGitRepoURL(tt.in); got != tt.want {
				t.Fatalf("ExtractGitRepoURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExtractRepoNameFromForgeBrowserURL(t *testing.T) {
	if got := ExtractRepoName("https://tangled.org/@dunkirk.sh/akami/blob/main/README.md"); got != "akami" {
		t.Fatalf("ExtractRepoName() = %q, want akami", got)
	}
}
