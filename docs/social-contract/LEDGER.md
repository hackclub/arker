# Integration Ledger — social-archive contract run (2026-08-12 overnight)

Manager branch: `fulfill-social-contract` (base origin/main @ 814fd21).
Rule: fetch origin/main before every integration; agents never push; manager never pushes in this run.

## Inherited from tonight's pre-existing Codex agents (already on origin/main)
- 3a9b44b unified social archive result API — reviewed by manager (gaps G1/G2/G7/G11 filed)
- 1d25462 find-or-create endpoint — reviewed (gap G5 filed)
- ffd704e cost estimates in result API — reviewed, OK
- 0200a75 + 814fd21 HTTPS public URLs — OK
- ad7870f viewer fix — landed on main; do not duplicate

## Worker branches (Opus 5 agents, worktrees off origin/main)
| Branch | Agent | Scope | State |
|---|---|---|---|
| agent/fulfillment-completeness | A 078db3e9 | G1 G2 G7 G11 + G13 transcripts | working (timebox 02:00 ET) |
| agent/platform-routing | B 4cd989f0 | G3a-e + recognition | MERGED + manager Vimeo DRM followup (80f2f26) |
| agent/contract-tests | C eabdff17 | fixtures + harness + 250 tests | MERGED; found G14 (fixed by manager ed08670) |
| agent/canonical-identity | E 3a66266a | G5 + races + AutoMigrate finding | MERGED + dedupe reconcile; postgres/race suites pass |

## Integrated into fulfill-social-contract
- Platform routing, contract testing, canonical identity, and fulfillment completeness branches were manager-reviewed and integrated.

## SHIPPED
- 2026-08-13 05:04 UTC (~01:04 ET... corrected: ~05:04Z): pushed fulfill-social-contract -> main, 814fd21..0c476b5 (73 commits, fast-forward, no force). All listed agent branches + manager fixes (G12, G14, Vimeo DRM, structural-single, IG alt key). Local gates at push: fmt/vet clean, 12/12 packages, race+postgres suites, full E2E incl. BD-fallback carousel (10/10, $0.0015) and YouTube transcript end-to-end.

## Cross-cutting findings during integration
- E: AutoMigrate silently no-ops on EXISTING prod tables (driver bug) — every new column needs explicit ADD COLUMN IF NOT EXISTS DDL; A warned; verify at A integration.
- C: G14 path-embedded googlevideo secrets in raw metadata — FIXED (ed08670), test enabled.
- Manager: Vimeo DRM per-video — scoped --check-formats (80f2f26); probes use non-DRM 22439234.
- Reddit WAF throttles this egress IP under burst; retest with cooldown; UA experiment only if defaults fail (prod parity first).

## AUTHORIZATION UPDATE (2026-08-12 ~23:20 ET, Zach, after risk disclosure)
12-hour shipping window through ~2026-08-13 10:50 ET:
- Manager (only) MAY: push reviewed+tested commits to main (no force-push), allow
  normal Coolify deploy, test/verify production, minimal production-safe live
  capture probes.
- Bright Data: up to $10.00 USD TOTAL new test spend, auditable per-call ledger
  (see BD-SPEND.md). Stop before exceeding.
- Still forbidden: force pushes, destructive prod/db changes, credential rotation
  / auth disabling / secret exposure. Subagents still never push/deploy/spend.
- Process per integration: fetch origin/main → independent review → rerun affected
  tests → push → observe deploy → verify exact revision in prod.
- End state: shipped to prod + production platform matrix verified + cost
  accounting verified. Not just an RC.
- Window expiry ~10:50 ET: stop shipping/spending; reporting still allowed.

## DEADLINE (Zach, 2026-08-13 ~00:05 ET)
Contract must be FULFILLED AND LIVE in prod, owner-testable, working on all common
platforms, by ~08:00 ET (owner wakes). Internal target: shipped + prod-verified by
~07:00 ET. Agent branches time-boxed to 02:00 ET with must-have/cuttable priorities.
BD testing re-confirmed OK <= $10 total, used judiciously.
Known honest limits to surface in the morning report: X/Twitter needs cookies prod
does not have (explicit authentication_required, never false-green); Facebook photo
posts are not a claimed shape; IG under throttle degrades to BD fallback.

## Bright Data spend ledger
Running total: $0.0015 of $10.00 cap (local verification phase). Per-call entries in docs/social-contract/BD-SPEND.md
(every paid call: timestamp, purpose, product, request/snapshot id, est. cost).
