# Agent G brief — Pinterest + Facebook BD pathways (phase 2b)

Spawned after agent F (TikTok/Reddit/X) integrates; base = manager branch with F's
work so fallback.go / capability mechanisms extend rather than collide.

## Verified facts (manager live discovery, 2026-08-13 ~01:30 ET)

**Pinterest** — dataset `gd_lk0sjs4d21pck...` CORRECT ID: `gd_lk0sjs4d21kdr7cnlv`
(Pinterest - Posts). Record: url, post_id, date_posted, user_name/user_url/user_id,
likes, comments_num, categories, hashtags, post_type, video_length,
`image_video_url` (media), `attached_files`. i.pinimg.com/originals URLs download
DIRECTLY from our IP (verified 206 + JPEG). Fixture:
docs/social-contract/brightdata_pinterest_posts.json (3 records incl. shapes).
Routing note: pinterest.com/pin/ is requiresCookies today with no fallback ⇒ no
media item created; extend the BD-capability carve-out (from F's generalization)
so pins get their gallery-dl item when the Pinterest pathway is enabled.

**Facebook** — per-post dataset `gd_lyclm1571iy3mv57zw` (Facebook - Posts by post
URL). Record: attachments[] objects {type: "video"|"photo", url (page link for
video / scontent image for photo), video_url (video.fbcdn.net /o1/v/ asset),
thumbnail_url}, video_title, video_view_count, num_comments, num_shares,
num_likes_type{...}, page_name, date_posted. BOTH media classes download
DIRECTLY from our IP (verified 206 + real MP4 / JPEG bytes). Fixtures:
brightdata_facebook_video_post.json (per-post, video) and
brightdata_facebook_page_posts.json (page listing shapes, photo attachment).
Routing notes: (a) facebook.com video shapes (reel//videos//watch/fb.watch)
already create yt-dlp items — SupportsFallback should cover them for the yt-dlp
type; (b) FACEBOOK PHOTO POSTS are currently unclaimed — add routing claims for
facebook.com/photo/, /photo.php, /<page>/photos/<id>, /posts/ permalinks
(gallery-dl type + BD fallback; native FB gallery-dl is auth-gated so the
BD-capability carve-out applies like X/Pinterest).

**Vimeo** — NO BD pathway: the Vimeo Videos dataset crawler errored on the
DRM-class video (exactly the class native cannot get); DRM is cryptographic,
not positional. Do not implement; document in AGENT.md's matrix notes.

## Guardrails
Same as F: no live BD calls (fixtures only), no pushes, no prod, deterministic
offline tests (mock Backend + fixtures), gofmt/vet/full-test green, small commits.
