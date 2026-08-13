# Bright Data spend ledger — social-contract mission (12h window ending ~2026-08-13 10:50 ET)

Hard cap: $10.00 USD cumulative NEW test spend initiated by this mission.
Rule: every deliberate paid call gets a row BEFORE the money is considered spent.
Organic production fallback spend from ordinary user traffic is not mission spend,
but is monitored via GET /admin/brightdata-usage during the window.

| # | UTC time | Purpose | Product | Dataset/zone | Request/snapshot id | Est. cost | Running total |
|---|---|---|---|---|---|---|---|
| 1 | 2026-08-13 ~04:25 | Local integration E2E: IG reel fallback verification (dev stack, no cookies, native fails => BD rescue) | web_scraper (reels dataset trigger) | gd_lyclm20il4r5helnj | n/a — native path succeeded first (anonymous reel extraction); NO BD ops triggered | $0.00 actual | $0.00 |

| 2 | 2026-08-13 ~04:35 | Local E2E: IG /p/ carousel fallback verification (no cookies => native fails => BD posts dataset rescue) | web_scraper (posts dataset) | gd_lk5ns7kz21pck8jpis | sd_msr0mayeg7u9dzzaq | $0.0015 actual (1 op, 1 record, success) | $0.0015 |
| 3 | 2026-08-13 ~05:50 | Prod matrix probes: IG reel DPAid-WDi67 + IG carousel DYZavjKE9i- (prod has cookies; BD triggers only if native fails) | web_scraper (if triggered) | reels/posts datasets | reel: native (no BD); carousel 7LRGk: 1 op, snapshot in prod bright_data_usages | $0.0015 actual | $0.0030 |
| 4 | 2026-08-13 ~05:10 | Phase-2 discovery: 1 trigger each on TikTok Posts (gd_lu702nij2f790tmv9h), Reddit Posts (gd_lvz8ah06191smkebj4), X Posts (gd_lwxkxvnf1cynvib9co) to learn schemas + CDN URL properties | web_scraper x3 | see purpose | sd_msr25mbhtpnzp8zos / sd_msr25mi41408hhlc4e / sd_msr25msl2ce5c4dsy3 | $0.0045 actual (3 records) | $0.0075 |
| 5 | 2026-08-13 ~05:25 | X video-tweet record shape discovery (SpaceX Starship liftoff tweet) | web_scraper | gd_lwxkxvnf1cynvib9co | sd_msr2ajy91198xmx5yg | $0.0015 actual (1 record) | $0.0090 |
| 6 | 2026-08-13 ~05:40 | Everything-sweep discovery: TikTok photo posts via keyword dataset, Pinterest posts discover, FB page posts (limits <=5 each) + follow-up single-record schema probes | web_scraper | gd_lilwhto81z415d9mdl, gd_lk0sjs4d21kdr7cnlv, gd_lkaxegm826bjpoo9m5, gd_lyclm1571iy3mv57zw, gd_lxk88z3v1ketji4pn | pinterest sd_msr2ik6pn2vyaf9e6 (3 rec), fb-pages sd_msr2ikn62mhaz0uk1w (5), tiktok-kw sd_msr2ix1q1nrlc7mb54 (6), fb-post sd_msr2*fbpost (1), vimeo probe (error rec) | ~$0.024 actual (~16 records) | ~$0.033 |
| 7 | 2026-08-13 ~02:15 ET | Live verification of F pathways via dev stack: X photo (Obama), X video (SpaceX), Reddit video (roxy), TikTok video (@tiktok) | web_scraper x4 + browser_api x1 (TikTok) | posts datasets + browser zone | X photo+video: NATIVE (gallery-dl syndication works once item exists — $0); Reddit rescue: 1 record $0.0015 (muxed h264+aac verified); TikTok: dataset ok but browser sessions BLOCKED by BD compliance policy (KYC required) — 3 billed session-attempts ~$0.0838 (policy discovery) | ~$0.0853 actual | ~$0.118 |
| 8 | 2026-08-13 ~06:40 ET | Verify compliance-abort fix: 1 TikTok rescue attempt should bill exactly one session | web_scraper + browser_api x1 | tiktok posts + browser zone | 1 session as designed; in-session retries eliminated; per-host cache added after | ~$0.0838 (pre-cache attempt) | ~$0.20 |
| 9 | 2026-08-13 ~07:05 ET | Live verify G pathways via dev stack: Pinterest pin, FB video, FB photo post | web_scraper x3 | pinterest + fb posts datasets | ALL THREE NATIVE (routing gates unlocked free paths; BD armed but unspent). FB photo post = honest partial (completeness_unknown; fallback only fires on native ERROR — filed follow-up) | $0.00 actual | ~$0.20 |

Planned spend (subject to remaining budget):
- End-of-run fallback-path verification: 1-2 deliberate rescues (dataset trigger
  ~$0.0015/record; browser session ~$8.40/GB → a YouTube itag-18 rescue ≈ $0.05-0.25).
  Expected total ≤ $0.60.
