package utils

import (
	"net/url"
	"strings"
)

// Canonical archive identity.
//
// Arker stores and displays whatever URL the caller submitted, but "has this
// post already been archived?" cannot be answered by string equality:
// https://youtu.be/dQw4w9WgXcQ?si=abc, https://www.youtube.com/watch?v=dQw4w9WgXcQ
// and https://m.youtube.com/watch?v=dQw4w9WgXcQ&t=42s are one video with three
// spellings. CanonicalizeArchiveURL maps every spelling of a post on a
// *recognized* social platform onto a single identity string, which
// find-or-create uses as its lookup key and as its advisory-lock key.
//
// Two rules keep this from breaking things:
//
//  1. Recognized platforms only. An unrecognized host is returned byte-for-byte
//     unchanged. Ordinary URLs are the majority of what Arker archives, and a
//     generic "normalize any URL" pass — sorting query strings, dropping
//     trailing slashes, folding www — silently merges distinct pages on sites we
//     know nothing about. A shop's ?variant=, a docs site's ?version=, a
//     paginated ?page= are all load-bearing somewhere.
//
//  2. Conservative query handling. A parameter is dropped only when it appears
//     in an explicit per-platform deny list of referrer/share/playback junk;
//     anything unrecognized is kept. The two failure modes are not symmetric.
//     Keeping a tracking parameter costs a missed dedupe, which is exactly
//     today's behavior and therefore not a regression. Dropping a load-bearing
//     parameter merges two different posts into one identity and serves the
//     wrong archive — a correctness bug that is invisible until someone reads
//     the result.
//
// Canonical output is not required to be fetchable, only stable: it is an
// identity key, never the URL handed to an archiver. In practice every form
// produced here does resolve to the post, which makes debugging far easier.
// CanonicalizeArchiveURL is pure, deterministic, and idempotent
// (canonicalize(canonicalize(x)) == canonicalize(x)); it performs no I/O, so
// short links whose target is only knowable by following a redirect
// (vm.tiktok.com, reddit /s/, fb.watch) keep their opaque identity by design.

// trackingParams are query parameters that identify the referrer, the sharing
// surface, or an ad click on every platform that uses them. None of them can
// change which post a URL points at.
var trackingParams = map[string]bool{
	"fbclid":   true,
	"gclid":    true,
	"gclsrc":   true,
	"dclid":    true,
	"msclkid":  true,
	"yclid":    true,
	"twclid":   true,
	"mc_cid":   true,
	"mc_eid":   true,
	"igshid":   true,
	"igsh":     true,
	"ref_src":  true,
	"ref_url":  true,
	"share_id": true,
	"_ga":      true,
	"_gl":      true,
}

// trackingPrefixes catches families of generated parameters that cannot be
// enumerated: utm_* (analytics), __cft__[0]/__tn__ (Facebook's click tokens).
var trackingPrefixes = []string{"utm_", "__"}

// CanonicalizeArchiveURL returns a stable identity string for rawURL.
//
// For a post on a recognized social platform it returns the canonical form of
// that post. For everything else — including a recognized host in an
// unrecognized shape, such as a YouTube channel page — it returns rawURL
// unchanged.
func CanonicalizeArchiveURL(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return rawURL
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return rawURL
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		// Scheme-relative and scheme-less inputs park the host in Path, where
		// none of the matchers below can see it. Leaving them alone is correct:
		// callers validate URLs before they get here.
		return rawURL
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return rawURL
	}
	// A social host on a non-default port is not the platform, it is something
	// pretending to be it (or a local proxy). Rebuilding it as https://host/...
	// would quietly repoint the identity at the real site.
	if port := parsed.Port(); port != "" && port != "80" && port != "443" {
		return rawURL
	}
	bare := strings.TrimPrefix(host, "www.")
	segs := splitPathSegments(parsed.Path)
	query := parsed.Query()

	// The fragment is deliberately dropped for recognized platforms: it is
	// never sent to the server, so it cannot change what an archiver captures.
	var canonical string
	switch {
	case isYouTubeHost(bare):
		canonical = canonicalYouTube(bare, segs, query)
	case isInstagramHost(bare):
		canonical = canonicalInstagram(segs, query)
	case isTwitterHost(bare):
		canonical = canonicalTwitter(segs, query)
	case isTikTokHost(bare):
		canonical = canonicalTikTok(segs, query)
	case isRedditHost(bare):
		canonical = canonicalReddit(bare, segs, query)
	case isBlueskyHost(bare):
		canonical = canonicalBluesky(segs, query)
	case isVimeoHost(bare):
		canonical = canonicalVimeo(bare, segs, query)
	case isFacebookHost(bare):
		canonical = canonicalFacebook(bare, segs, query)
	}
	if canonical == "" {
		return rawURL
	}
	return canonical
}

// ---------------------------------------------------------------------------
// YouTube
// ---------------------------------------------------------------------------

func isYouTubeHost(bare string) bool {
	return bare == "youtube.com" || bare == "m.youtube.com" || bare == "youtu.be"
}

// youTubeDropParams: si and feature tag the share surface, t/start/end are
// playback offsets into a video Arker archives in full, app=desktop is the m.
// site's redirect flag, ab_channel is an attribution echo of the uploader, and
// pp is the opaque player-context blob share sheets attach. None of them select
// a video; the video ID does. list/index survive on purpose — a watch page
// opened inside a playlist renders differently, and unlike the rest it is not
// obviously discardable.
var youTubeDropParams = map[string]bool{
	"si":           true,
	"feature":      true,
	"t":            true,
	"start":        true,
	"end":          true,
	"app":          true,
	"ab_channel":   true,
	"pp":           true,
	"themeRefresh": true,
}

// canonicalYouTube folds youtu.be share links into the watch shape, because
// youtu.be/X is nothing but an alias of watch?v=X. Shorts and live keep their
// own shape: they are distinct pages with distinct chrome, and collapsing them
// into /watch would make find-or-create hand back an archive of a different
// page than the one that was asked for.
func canonicalYouTube(bare string, segs []string, query url.Values) string {
	dropParams(query, youTubeDropParams)

	if bare == "youtu.be" {
		if len(segs) != 1 || !isIDLike(segs[0]) {
			return ""
		}
		query.Set("v", segs[0])
		return buildURL("www.youtube.com", "/watch", query)
	}

	if len(segs) == 0 {
		return ""
	}
	switch segs[0] {
	case "watch":
		if len(segs) != 1 {
			return ""
		}
		if v := query.Get("v"); isIDLike(v) {
			return buildURL("www.youtube.com", "/watch", query)
		}
	case "shorts", "live":
		if len(segs) == 2 && isIDLike(segs[1]) {
			return buildURL("www.youtube.com", "/"+segs[0]+"/"+segs[1], query)
		}
	}
	// Channels, playlists, /embed/, /@handle, youtube-nocookie: not a single
	// post shape Arker claims, so identity stays the raw string.
	return ""
}

// ---------------------------------------------------------------------------
// Instagram
// ---------------------------------------------------------------------------

func isInstagramHost(bare string) bool {
	return bare == "instagram.com" || bare == "m.instagram.com"
}

// hl (interface language) and img_index (which carousel slide opens) are
// deliberately absent: both change what a rendered capture looks like, so they
// stay part of the identity.
var instagramDropParams = map[string]bool{
	"igsh":       true,
	"igshid":     true,
	"share_url":  true,
	"story_type": true,
}

// canonicalInstagram normalizes /reels/ to /reel/ (Instagram redirects one to
// the other) but keeps /p/, /reel/ and /tv/ apart. Instagram does serve a reel
// under both /p/CODE and /reel/CODE, yet Arker routes them to different
// archivers — /p/ to gallery-dl, /reel/ to yt-dlp — so folding them together
// would let a find-or-create for one shape answer with the other's artifact.
//
// The shortcode is located by scanning for the kind segment rather than by
// position, because Instagram also serves /{username}/p/{code}/.
func canonicalInstagram(segs []string, query url.Values) string {
	dropParams(query, instagramDropParams)
	for i, seg := range segs {
		kind := seg
		switch kind {
		case "reels":
			kind = "reel"
		case "p", "reel", "tv":
		default:
			continue
		}
		// Exactly one segment may follow: /p/CODE/embed and /p/CODE/liked_by
		// are different pages, not different spellings of the post.
		if i+2 != len(segs) || !isIDLike(segs[i+1]) {
			return ""
		}
		return buildURL("www.instagram.com", "/"+kind+"/"+segs[i+1]+"/", query)
	}
	return ""
}

// ---------------------------------------------------------------------------
// X / Twitter
// ---------------------------------------------------------------------------

func isTwitterHost(bare string) bool {
	switch bare {
	case "x.com", "mobile.x.com", "m.x.com",
		"twitter.com", "mobile.twitter.com", "m.twitter.com":
		return true
	}
	return false
}

// s and t are the share-sheet's surface and signature; cxt is the notification
// context blob.
var twitterDropParams = map[string]bool{"s": true, "t": true, "cxt": true, "ref_component": true}

// canonicalTwitter keys on the status ID alone and emits the username-free
// /i/web/status/ form. Usernames change — a handle rename rewrites every
// permalink to a post without changing the post — and /i/web/status/ID is
// already how X itself addresses a status when it does not know the author.
func canonicalTwitter(segs []string, query url.Values) string {
	dropParams(query, twitterDropParams)
	for i, seg := range segs {
		if seg != "status" && seg != "statuses" {
			continue
		}
		if i+1 >= len(segs) || !isNumeric(segs[i+1]) {
			return ""
		}
		// /photo/1 and /video/1 are the lightbox over the same post; anything
		// else after the ID (/analytics, /retweets, /likes) is a different page.
		switch rest := segs[i+2:]; {
		case len(rest) == 0:
		case len(rest) == 2 && (rest[0] == "photo" || rest[0] == "video") && isNumeric(rest[1]):
		default:
			return ""
		}
		return buildURL("x.com", "/i/web/status/"+segs[i+1], query)
	}
	return ""
}

// ---------------------------------------------------------------------------
// TikTok
// ---------------------------------------------------------------------------

// isTikTokHost deliberately excludes vm./vt./ short-link hosts: their path is an
// opaque redirect token, and resolving it needs a network round trip that
// canonicalization must not make.
func isTikTokHost(bare string) bool {
	return bare == "tiktok.com" || bare == "m.tiktok.com"
}

var tiktokDropParams = map[string]bool{
	"is_from_webapp": true, "sender_device": true, "sender_web_id": true,
	"web_id": true, "_r": true, "_t": true, "_d": true, "is_copy_url": true,
	"refer": true, "referer_url": true, "referer_video_id": true,
	"share_app_id": true, "share_link_id": true, "share_item_id": true,
	"tt_from": true, "u_code": true, "source": true, "social_sharing": true,
	"timestamp": true, "user_id": true, "enter_from": true, "enter_method": true,
	"checksum": true, "q": true,
}

// canonicalTikTok keeps the @username in the identity. The numeric ID alone
// identifies the item, but TikTok 404s a mismatched username rather than
// redirecting, so a URL's username is part of what makes it resolvable.
// Usernames are case-insensitive, so they are lowercased.
func canonicalTikTok(segs []string, query url.Values) string {
	dropParams(query, tiktokDropParams)
	// /t/XXXX is another opaque short link.
	if len(segs) >= 1 && segs[0] == "t" {
		return ""
	}
	if len(segs) != 3 || !strings.HasPrefix(segs[0], "@") {
		return ""
	}
	user := strings.ToLower(segs[0])
	if len(user) < 2 || !isHandleLike(user[1:]) {
		return ""
	}
	kind := segs[1]
	if kind != "video" && kind != "photo" {
		return ""
	}
	if !isNumeric(segs[2]) {
		return ""
	}
	return buildURL("www.tiktok.com", "/"+user+"/"+kind+"/"+segs[2], query)
}

// ---------------------------------------------------------------------------
// Reddit
// ---------------------------------------------------------------------------

func isRedditHost(bare string) bool {
	if bare == "redd.it" {
		return true
	}
	switch bare {
	case "reddit.com", "old.reddit.com", "new.reddit.com", "m.reddit.com",
		"np.reddit.com", "i.reddit.com", "sh.reddit.com", "amp.reddit.com":
		return true
	}
	// i.redd.it and v.redd.it are media hosts whose path is a filename, not a
	// post ID, and preview.redd.it is a signed thumbnail. They must never be
	// folded into a post identity, so they are not recognized here.
	return false
}

var redditDropParams = map[string]bool{
	"share_id": true, "ref": true, "ref_source": true, "ref_campaign": true,
	"correlation_id": true, "post_fullname": true, "rdt": true, "rdtk": true,
	"context": true, "chainedPosts": true, "$deep_link": true,
	"$original_url": true, "$3p": true, "_branch_match_id": true,
	"_branch_referrer": true, "share_source": true,
}

// canonicalReddit drops the subreddit from the identity. The post ID is
// globally unique and a post lives in exactly one subreddit, but a redd.it
// short link carries only the ID — so a subreddit-bearing identity could never
// match https://redd.it/abc123 against
// https://www.reddit.com/r/golang/comments/abc123/some_title/. The
// subreddit-free /comments/{id}/ form is Reddit's own ID-only permalink.
// The title slug is dropped for the same reason: it is decoration Reddit
// regenerates and omits at will.
func canonicalReddit(bare string, segs []string, query url.Values) string {
	dropParams(query, redditDropParams)

	if bare == "redd.it" {
		if len(segs) != 1 || !isIDLike(segs[0]) {
			return ""
		}
		return buildURL("www.reddit.com", "/comments/"+segs[0]+"/", query)
	}

	var postID string
	var rest []string
	switch {
	case len(segs) >= 4 && segs[0] == "r" && segs[2] == "comments":
		postID, rest = segs[3], segs[4:]
	case len(segs) >= 2 && segs[0] == "comments":
		postID, rest = segs[1], segs[2:]
	default:
		// /r/{sub}/s/{token} share links are opaque redirects; user pages,
		// subreddit fronts and media hosts are not posts.
		return ""
	}
	if !isIDLike(postID) {
		return ""
	}
	// Both the legacy /{slug}/{commentID} and the current /comment/{commentID}
	// permalink shapes put the comment ID in the same position. A comment
	// permalink is a different capture target than its post, so it keeps its
	// own identity.
	if len(rest) >= 2 && isIDLike(rest[1]) {
		return buildURL("www.reddit.com", "/comments/"+postID+"/comment/"+rest[1]+"/", query)
	}
	if len(rest) > 1 {
		return ""
	}
	return buildURL("www.reddit.com", "/comments/"+postID+"/", query)
}

// ---------------------------------------------------------------------------
// Bluesky
// ---------------------------------------------------------------------------

func isBlueskyHost(bare string) bool { return bare == "bsky.app" }

// canonicalBluesky lowercases a handle (handles are domain names, so
// case-insensitive) but never a DID, whose method-specific identifier is
// case-sensitive. A post addressed by handle and the same post addressed by DID
// stay separate identities: mapping between them requires a directory lookup,
// which is I/O this function must not do.
func canonicalBluesky(segs []string, query url.Values) string {
	dropParams(query, nil)
	if len(segs) != 4 || segs[0] != "profile" || segs[2] != "post" {
		return ""
	}
	actor := segs[1]
	if !strings.HasPrefix(strings.ToLower(actor), "did:") {
		actor = strings.ToLower(actor)
	}
	if !isHandleLike(actor) || !isIDLike(segs[3]) {
		return ""
	}
	return buildURL("bsky.app", "/profile/"+actor+"/post/"+segs[3], query)
}

// ---------------------------------------------------------------------------
// Vimeo
// ---------------------------------------------------------------------------

func isVimeoHost(bare string) bool {
	return bare == "vimeo.com" || bare == "player.vimeo.com"
}

// Player chrome only: none of these change which video loads.
var vimeoDropParams = map[string]bool{
	"share": true, "fl": true, "fe": true, "autoplay": true, "muted": true,
	"loop": true, "title": true, "byline": true, "portrait": true,
	"color": true, "badge": true, "app_id": true, "dnt": true,
	"transparent": true, "controls": true, "autopause": true, "referrer": true,
}

// canonicalVimeo preserves the unlisted-video hash, which is the one Vimeo
// parameter that is genuinely load-bearing: vimeo.com/123456789/abcdef1234 and
// player.vimeo.com/video/123456789?h=abcdef1234 are the same private video, and
// the same ID without the hash is a different (403) resource.
func canonicalVimeo(bare string, segs []string, query url.Values) string {
	dropParams(query, vimeoDropParams)

	var id, hash string
	switch {
	case bare == "player.vimeo.com":
		if len(segs) != 2 || segs[0] != "video" || !isNumeric(segs[1]) {
			return ""
		}
		id = segs[1]
	case len(segs) == 1 && isNumeric(segs[0]):
		id = segs[0]
	case len(segs) == 2 && isNumeric(segs[0]) && isIDLike(segs[1]):
		id, hash = segs[0], segs[1]
	case len(segs) == 3 && segs[0] == "channels" && isNumeric(segs[2]):
		id = segs[2]
	case len(segs) == 4 && segs[0] == "groups" && segs[2] == "videos" && isNumeric(segs[3]):
		id = segs[3]
	default:
		// /ondemand/, /user123/, /manage/ and staff-pick listing pages are not
		// single-video shapes.
		return ""
	}
	// The player carries the hash in ?h=; the web URL carries it as a path
	// segment. Normalize onto the path form so both spellings agree.
	if h := query.Get("h"); h != "" {
		if hash == "" && isIDLike(h) {
			hash = h
		}
		query.Del("h")
	}
	path := "/" + id
	if hash != "" {
		path += "/" + hash
	}
	return buildURL("vimeo.com", path, query)
}

// ---------------------------------------------------------------------------
// Facebook
// ---------------------------------------------------------------------------

func isFacebookHost(bare string) bool {
	switch bare {
	case "facebook.com", "m.facebook.com", "web.facebook.com",
		"mbasic.facebook.com", "fb.watch":
		return true
	}
	return false
}

var facebookDropParams = map[string]bool{
	"mibextid": true, "rdid": true, "share_url": true, "extid": true,
	"idorvanity": true, "sfnsn": true, "refsrc": true, "_rdr": true,
	"hc_ref": true, "comment_tracking": true, "notif_id": true,
	"notif_t": true, "av": true, "eav": true, "paipv": true, "rc": true,
}

// canonicalFacebook covers only the video shapes Arker claims to route
// (url_utils.go: /reel/, /videos/, /watch, fb.watch). Photo posts, permalinks
// and story.php are ordinary URLs here and stay untouched.
//
// fb.watch IDs stay opaque: mapping one to its underlying video ID means
// following a redirect, which is I/O. Normalizing the wrapper (scheme, host,
// tracking params) still collapses the share-sheet spellings of one link.
func canonicalFacebook(bare string, segs []string, query url.Values) string {
	dropParams(query, facebookDropParams)

	if bare == "fb.watch" {
		if len(segs) != 1 || !isIDLike(segs[0]) {
			return ""
		}
		return buildURL("fb.watch", "/"+segs[0]+"/", query)
	}

	if len(segs) == 0 {
		// /watch?v=ID with no path segments is not reachable; /watch/ is.
		return ""
	}

	// /watch/?v=ID, /watch?v=ID, /watch/live/?v=ID, /video.php?v=ID
	if segs[0] == "watch" || segs[0] == "video.php" {
		if v := query.Get("v"); isNumeric(v) {
			// Set collapses a repeated ?v= to one value so the identity cannot
			// depend on parameter order.
			query.Set("v", v)
			return buildURL("www.facebook.com", "/watch/", query)
		}
		return ""
	}

	// /reel/{id} and /{page}/reel/{id}: reel IDs are globally unique, so the
	// page segment is redundant and only adds a second spelling.
	for i, seg := range segs {
		if seg == "reel" && i+2 == len(segs) && isNumeric(segs[i+1]) {
			return buildURL("www.facebook.com", "/reel/"+segs[i+1]+"/", query)
		}
	}

	// /{page}/videos/{id} and /{page}/videos/{title-slug}/{id}. The page
	// segment is kept — unlike reels, a bare /videos/{id} is not a valid
	// address — and lowercased, since Facebook usernames are case-insensitive.
	for i, seg := range segs {
		if seg != "videos" || i == 0 {
			continue
		}
		page := strings.ToLower(segs[0])
		if !isHandleLike(page) {
			return ""
		}
		id := segs[len(segs)-1]
		if len(segs) < i+2 || !isNumeric(id) {
			return ""
		}
		return buildURL("www.facebook.com", "/"+page+"/videos/"+id+"/", query)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// splitPathSegments returns the non-empty, percent-decoded segments of a URL
// path.
func splitPathSegments(path string) []string {
	parts := strings.Split(path, "/")
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			segs = append(segs, p)
		}
	}
	return segs
}

// dropParams removes the shared tracking parameters plus a platform's own deny
// list. Everything else survives: see the conservative-handling note at the top
// of this file.
func dropParams(query url.Values, platform map[string]bool) {
	for key := range query {
		if platform[key] || trackingParams[key] {
			delete(query, key)
			continue
		}
		lower := strings.ToLower(key)
		if trackingParams[lower] {
			delete(query, key)
			continue
		}
		for _, prefix := range trackingPrefixes {
			if strings.HasPrefix(lower, prefix) {
				delete(query, key)
				break
			}
		}
	}
}

// buildURL assembles the canonical string. Query parameters are sorted by
// url.Values.Encode, so two spellings that differ only in parameter order land
// on the same identity.
func buildURL(host, path string, query url.Values) string {
	out := "https://" + host + path
	if len(query) > 0 {
		out += "?" + query.Encode()
	}
	return out
}

// isIDLike reports whether s is a plausible opaque platform identifier. Every
// shortcode, video ID and rkey Arker canonicalizes is drawn from this alphabet;
// anything else means the URL was not the shape it looked like, and the
// conservative answer is to leave it alone rather than build an identity around
// characters that would need escaping.
func isIDLike(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isHandleLike allows the dots and colons that appear in Bluesky handles
// (alice.bsky.social) and DIDs (did:plc:xyz), alongside the ID alphabet.
func isHandleLike(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}
