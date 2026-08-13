# Social-archive contract — overnight ship report (2026-08-13)

**Shipped and live.** Production (archive.hackclub.com) runs main `2fca7b1`
(~116 commits tonight across two phases: 6 Opus 5 agent branches + manager
integration), deployed via the normal Coolify flow, zero crash loops.

**Prod matrix v2 (final, live on 2fca7b1):** Reddit ✅ fulfilled via BD rescue
($0.0015, muxed audio verified) · X ✅ native · Pinterest ✅ native (previously
no media item at all) · Facebook video ✅ native · FB photo post ⚠ honest
partial (newly claimed shape) · TikTok ❌ explicit pending your BD KYC (one
billed session, policy-cached) · everything from matrix v1 still green. Total
BD spend: ~$0.23 of $10.

## ⚠️ Morning action for Zach

(5 min, unlocks TikTok bytes) Complete Bright Data KYC:
https://brightdata.com/cp/kyc — their compliance layer gates tiktok.com browser
sessions until then.

## Test it yourself

```
# any social URL: submit, then read the unified result
curl -X POST https://archive.hackclub.com/api/v1/archive \
  -H "Authorization: Bearer <your key>" -H 'Content-Type: application/json' \
  -d '{"url":"https://www.youtube.com/watch?v=jNQXAC9IVRw"}'
curl -H "Authorization: Bearer <key>" https://archive.hackclub.com/api/v1/archive/<short_id>
# find-or-create (canonical identity: youtu.be joins the watch?v= capture)
curl -X POST https://archive.hackclub.com/api/v1/archive/find-or-create ... -d '{"url":"https://youtu.be/jNQXAC9IVRw"}'
# service health
curl https://archive.hackclub.com/health
# admin: /admin/brightdata-usage (spend)
```

`social_post` now carries: status/fulfilled (honest — see below), normalized
post, media[] (Arker-hosted URLs, alt text where the platform provides it),
completeness {state, expected, stored}, transcript {lang, source, text} +
subtitles[] with download URLs, raw_metadata links (sanitized), provenance
(native vs brightdata, attempts, last failure reason, BD ops + cost), warnings,
failure {code, message, retryable}.

## Production platform matrix (verified live tonight, forced fresh captures)

| Platform / shape | Result | Evidence |
|---|---|---|
| YouTube video | ✅ fulfilled, native, complete, **transcript** | 7XGuR |
| YouTube Short | ✅ fulfilled, native, complete, **transcript** | dOuEi |
| Vimeo | ✅ fulfilled, native (player-URL route; main site is login-only upstream) | seHF8 |
| Instagram reel | ✅ fulfilled, native (prod cookies healthy) | FYJMH |
| Instagram carousel | ✅ fulfilled via **Bright Data rescue, 8/8 slides, $0.0015**, provenance surfaced | 7LRGk |
| Bluesky image | ✅ fulfilled, native, complete | vWBbm |
| Flickr photo | ✅ fulfilled, complete (structural single-asset rule) | 271PA |
| Imgur album | ✅ 2/2 files | EikLI |
| Ordinary URL | ✅ unchanged behavior, no social_post | X8Udg |
| TikTok video | ❌ explicit `extractor_failed` — TikTok bot-walls this egress IP (verified from both prod and HQ; same IP). Routing + metadata code is correct and fixture-tested | jIeVJ |
| TikTok photo | routed to gallery-dl (new); same IP wall applies; fixture-tested | — |
| X/Twitter | ❌ explicit `authentication_required` — X serves nothing logged-out and prod has no X cookies (owner decision; adding an X cookie jar would light this up) | TfbTE |
| Reddit | ❌ reddit WAF 403s the `.json` API from prod's IP (verified from prod host). gallery-dl depends on it. Fix path: registered reddit OAuth client-id (owner decision). v.redd.it audio-mux fix is in and fixture-tested for when access returns | JVadH |
| Facebook video shapes | routed to yt-dlp, unit/fixture-tested; no stable anonymous public FB probe was verifiable tonight — expect explicit failures when FB gates | — |

**No false-greens anywhere**: every non-success above is an explicit, coded,
retryable-flagged failure in the API, never a silent mhtml/screenshot-only pass.

## What shipped (the contract, mapped)

1. **Never-false-green fulfillment**: 3-state completeness (complete/partial/
   unknown) recorded per item, derived from extractor counts + structural
   shape; fulfilled requires complete + valid post + servable media +
   retrievable raw metadata. Partial carousels can no longer read green.
   Legacy/pre-feature archives stay explicit (`legacy_archive`).
2. **Find-or-create with canonical identity**: youtu.be/watch?v=/shorts,
   IG url variants, x/twitter/mobile hosts, reddit hosts + redd.it, bluesky,
   vimeo, FB shapes → one identity; advisory-locked on canonical form; newest
   completed wins regardless of age; joins in-flight; creates once (race-tested
   against real Postgres). 78,093 prod rows backfilled in 12.7s at boot.
3. **Free-first + honest paid fallback**: unchanged native-first design;
   fallback provenance now first-class in the API (source, ops, records,
   bytes, snapshot ids, est. cost). Verified live: IG carousel rescue $0.0015.
4. **All media + metadata maximalism (G13)**: subtitles (manual + auto,
   language-bounded) stored as artifacts; plain-text transcript derived
   (rolling-caption dedupe) and inlined; alt text per slide; gallery sidecars
   now sanitized at rest (G12); googlevideo path-segment secrets redacted (G14
   — was leaking client IPs + live signatures in raw metadata on every Shorts
   capture).
5. **Explicit social intent**: every claimed platform shape (incl. TikTok
   photo) is recognized; social_post never silently absent for them.
6. **Stable versioned API**: schema_version 1, all changes additive.
## Costs (auditable ledger: BD-SPEND.md)

Mission Bright Data spend: **$0.0030 of the $10.00 cap** (2 dataset records:
one local-verify carousel, one prod-verify carousel; snapshot ids recorded;
`/admin/brightdata-usage` agrees). Everything else ran the free native path.

## Verification story

- 12/12 Go packages green (incl. new contract suites), gofmt/vet clean,
  `-race` green, Postgres-gated advisory-lock/migration suites green against a
  prod-shaped DB, 50k-row backfill measured before prod ran it on 78k.
- 250+ contract tests over a fake-extractor harness driving the REAL archiver/
  worker/API code with sanitized fixtures for every matrix cell (no network).
- Browser E2E locally + live prod matrix above, with actual nonzero media
  bytes fetched through the API for every green cell.

## Phase 2 (post-1st-ship, same night): Bright Data pathways for blocked platforms

Zach directive ~01:05 ET: "the ones we can't get native extractors for, implement
Bright Data pathways for … basically get it working for everything."

Live-verified results (dev stack, real BD calls, all ledgered):
- **Reddit: WORKS.** Native WAF-blocked -> BD Posts dataset -> packaged-media
  muxed MP4 (h264+AAC verified by ffprobe on the stored artifact), photos[] for
  image/gallery posts, completeness recorded, $0.0015/rescue.
- **X: routing now creates the item, and native gallery-dl (syndication route)
  succeeded anonymously in verification — free path first, BD pathway armed
  behind it** (photos + video variants incl. 4K, direct-download verified).
- **TikTok: metadata pathway works; BYTES BLOCKED at Bright Data's compliance
  layer** — browser sessions refuse tiktok.com navigation pending account-level
  KYC approval (https://brightdata.com/cp/kyc). One-time owner action; the code
  path is built and expected to work unchanged after approval. Refusals now
  abort without retry (a blocked rescue costs one session, not three).
- **Pinterest: pathway shipped** — pins previously got NO media item at all
  (cookie-gated, no fallback); now routed + BD-rescued (i.pinimg originals,
  direct download). Video pins carry a VideoMetadata sidecar.
- **Facebook: pathway shipped on both routes** — video permalinks (yt-dlp item,
  video.fbcdn.net direct) AND newly-claimed photo/post permalink shapes
  (/photo/?fbid=, /photo.php, /<page>/photos/<id>, /<page>/posts/<id> incl.
  pfbid) through the gallery route. FB photo posts were previously entirely
  unclaimed (ordinary-URL treatment).
- Compliance refusals (KYC-gated hosts) now abort without retry AND are cached
  per host for the process lifetime — a blocked TikTok rescue costs one session
  (~$0.03), not three per URL.
- **Vimeo: no BD pathway** — the dataset crawler fails on exactly the DRM class
  native can't get; DRM is cryptographic, not positional. Documented.

## Known limits + recommended follow-ups (owner decisions)

- TikTok needs an unblocked egress (residential proxy or BD-style route) —
  currently explicit-fails from HQ's IP. Routing/metadata ready.
- Reddit needs OAuth client-id credentials for gallery-dl, or an unlocker
  proxy. v.redd.it muxing fix already shipped.
- X needs a cookie jar (deliberate non-goal tonight).
- Vimeo: per-video DRM exists (audio-only DRM observed); such videos fail
  explicitly. Non-DRM Vimeo verified working.
- BD post dataset exposes post-level alt_text that isn't mapped yet (small).
- The `verify-prod-brightdata` worktree/branch and tonight's agent branches
  can be archived once reviewed.
