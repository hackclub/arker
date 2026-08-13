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
| agent/fulfillment-completeness | (pending) | G1 G2 G7 G11 + viewer badge | spawning |
| agent/platform-routing | (pending) | G3a-d + recognition + matrix updates | spawning |
| agent/contract-tests | (pending) | fixtures corpus + fake-binary harness + contract tests | spawning |
| agent/canaries | (pending) | G9 canary system (not activated) | spawning |
| agent/canonical-identity | (pending) | G5 + find-or-create races | spawning |

## Integrated into fulfill-social-contract
(none yet)

## AUTHORIZATION UPDATE (2026-08-12 ~23:20 ET, Zach, after risk disclosure)
12-hour shipping window through ~2026-08-13 10:50 ET:
- Manager (only) MAY: push reviewed+tested commits to main (no force-push), allow
  normal Coolify deploy, test/verify production, minimal production-safe live
  capture probes, activate canaries.
- Bright Data: up to $10.00 USD TOTAL new test spend, auditable per-call ledger
  (see BD-SPEND.md). Stop before exceeding.
- Still forbidden: force pushes, destructive prod/db changes, credential rotation
  / auth disabling / secret exposure. Subagents still never push/deploy/spend.
- Process per integration: fetch origin/main → independent review → rerun affected
  tests → push → observe deploy → verify exact revision in prod.
- End state: shipped to prod + production platform matrix verified + cost
  accounting verified + canaries ACTIVE. Not just an RC.
- Window expiry ~10:50 ET: stop shipping/spending; reporting still allowed.

## Bright Data spend ledger
Running total: $0.00 of $10.00 cap. Per-call entries in docs/social-contract/BD-SPEND.md
(every paid call: timestamp, purpose, product, request/snapshot id, est. cost).
