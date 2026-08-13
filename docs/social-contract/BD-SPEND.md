# Bright Data spend ledger — social-contract mission (12h window ending ~2026-08-13 10:50 ET)

Hard cap: $10.00 USD cumulative NEW test spend initiated by this mission.
Rule: every deliberate paid call gets a row BEFORE the money is considered spent.
Organic production fallback spend from ordinary user traffic is not mission spend,
but is monitored via GET /admin/brightdata-usage during the window.

| # | UTC time | Purpose | Product | Dataset/zone | Request/snapshot id | Est. cost | Running total |
|---|---|---|---|---|---|---|---|
| 1 | 2026-08-13 ~04:25 | Local integration E2E: IG reel fallback verification (dev stack, no cookies, native fails => BD rescue) | web_scraper (reels dataset trigger) | gd_lyclm20il4r5helnj | n/a — native path succeeded first (anonymous reel extraction); NO BD ops triggered | $0.00 actual | $0.00 |

| 2 | 2026-08-13 ~04:35 | Local E2E: IG /p/ carousel fallback verification (no cookies => native fails => BD posts dataset rescue) | web_scraper (posts dataset) | gd_lk5ns7kz21pck8jpis | sd_msr0mayeg7u9dzzaq | $0.0015 actual (1 op, 1 record, success) | $0.0015 |
| 3 | 2026-08-13 ~05:50 | Prod matrix probes: IG reel DPAid-WDi67 + IG carousel DYZavjKE9i- (prod has cookies; BD triggers only if native fails) | web_scraper (if triggered) | reels/posts datasets | (recorded post-run) | est $0.00-0.02 | tbd |

Planned spend (subject to remaining budget):
- End-of-run fallback-path verification: 1-2 deliberate rescues (dataset trigger
  ~$0.0015/record; browser session ~$8.40/GB → a YouTube itag-18 rescue ≈ $0.05-0.25).
  Expected total ≤ $0.60.
