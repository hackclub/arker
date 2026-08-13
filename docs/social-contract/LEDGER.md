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

## Isolation: steps requiring later shipping approval
- Push of fulfill-social-contract / merge to main / PR
- Deploy (Coolify auto-deploys on main merge — merging IS deploying)
- Canary activation (schedule + any paid-path canaries)
- Any live Bright Data verification
- Prod DB migration execution (happens automatically on deploy startup; verify additive-only before approving)
