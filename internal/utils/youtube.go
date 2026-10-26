package utils

import "strings"

// YouTube shows some public videos to logged-out viewers only behind "Please
// sign in. This helps protect our community." (seen 2026-10). The gate is
// per video, not per IP, and every logged-out player client yt-dlp offers
// hits it except the embedded player's, which serves the full format ladder.
const youtubeEmbeddedPlayerClient = "youtube:player_client=web_embedded"

// YtDlpYouTubeLoginGateArgs returns the extra yt-dlp arguments that get past
// YouTube's logged-out sign-in gate, or nil when url is not YouTube or no
// ERROR line in output asks for a sign-in. Any other failure (a removed video,
// a geo block) gets nil so it is not retried for nothing.
func YtDlpYouTubeLoginGateArgs(url string, output ...string) []string {
	if !IsYouTubeURL(url) {
		return nil
	}
	for _, out := range output {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "ERROR:") && strings.Contains(strings.ToLower(line), "sign in") {
				return []string{"--extractor-args", youtubeEmbeddedPlayerClient}
			}
		}
	}
	return nil
}
