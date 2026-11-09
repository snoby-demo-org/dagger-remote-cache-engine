> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Round-4 review brief — Revision R3 of the Dagger shared-cache design

Round 4. Both reviewers previously returned STOP. This round exists to decide whether the specification is now internally consistent and executable, or whether the design needs a structural change rather than another patch pass.

## Situation (unchanged)

Shared S3-style cache for Dagger CI. Design cluster: homelab k3s, persistent nodes, in-cluster MinIO, Postgres. Production: Azure, **S3 outside the cluster**, compute nodes ephemeral with ~30 s notice, no MinIO. Engine: Dagger v1.0.0-beta.25 (7e9eaac); push protocol over WS `/v1/engine`, service-issued presigned PUTs, whole-object single PUT, Range GETs requiring exact 206, renewal reply 2 s, no inventory and no on-miss lookup (discovery needs OTLP).

## History

- Round 1 (Claude): 63 findings, STOP → R1.
- Round 2 (ChatGPT): 19 findings, STOP → R2.
- Round 3 (both, parallel): Claude 26 (1 P0/13 P1/12 P2), ChatGPT 22 (2 P0/18 P1/2 P2) → R3.

## What R3 changed (read `revision-r3.md` first, then verify it)

- `ledger.md` rewritten: state-branched reserve under a head lock, head-state mirror maintained on every transition, **publication by holding head row locks across `SetStoredPart`** (the expiring pin is gone), generation-scoped keys plus `graph_refs` so only one generation can hold graph parts, leader-fenced delete with `delete_epoch`, failed-object cleanup, `quarantined` corruption path, recovery scanner.
- `metrics-and-eviction.md` rewritten: revision-keyed entries, interval-union attribution, install and recompute as independent measures, donor cost recorded at publish, gross/net saved and lost, breaker grounded on store errors + install p95 + demand p95, **lowest-score** reclaim groups, quota charged by state with durable `pending_free`, cohort-based regret, one admission rule (AND), corrected PromQL.
- `architecture.md` patched at 20 more sites (renewal budget, scheduler part-availability, retention on install only, I3 weakened honestly, store breaker, Gate A/B, MinIO rescoped to the design cluster, lifecycle fallback declared incapable).
- `latency-and-azure.md` rewritten from a **fresh probe run**: GET p50 TTFB 4.27 ms / p99 8.90, HEAD 2.18, PUT 109.5 MB/s + 19.2 ms ack, 8× aggregate 62.7 MB/s, 403 asserted, all assertions passed.
- `s3_latency_probe.py`: TTFB now taken after response headers.

## Files

`revision-r3.md`, `ledger.md`, `architecture.md`, `metrics-and-eviction.md`, `latency-and-azure.md`, `decision.md`, `s3_latency_probe.py`, `review/r3.diff` (cumulative R1→R3 diff), `review/baseline-r2/`, plus your own and the other reviewer's previous rounds.

## Questions

1. **Closure audit** of all findings from round 3 (yours and the other reviewer's): for each, cite the clause that closes it or state it is open.
2. **Attack the new publish path.** Publication holds head row locks across `SetStoredPart`:
   (a) can you construct a deadlock between publish and GC mark with multi-digest chains?
   (b) what happens if the process is killed between `SetStoredPart` and `COMMIT`?
   (c) can a publisher whose transaction is rolled back leave graph parts behind?
   (d) does the `graph_refs=0` gate actually prevent two generations from having graph parts, given restore and quarantine?
   Give a concrete interleaving or state you cannot break it.
3. **Reserve.** Does the state-branched `INSERT … SELECT … WHERE state IN (...)` under `FOR UPDATE` implement the transition table exactly, including the partial unique index behaviour and the reuse/wait/skip outcomes?
4. **Metrics.** Is the interval-union attribution computable from the cited attributes? Are gross/net saved, hit ratio, ceiling and regret each bounded and computable, with defined behaviour at zero denominators? Any attribute still assumed?
5. **Eviction.** Does lowest-score group selection now fix the cold-sharer pathology without introducing overshoot or starvation? Is quota accounting consistent with tombstones and projected frees?
6. **Consistency.** Grep the normative docs for any surviving clause that contradicts R3 (retention on merge, value-existence dedupe, p99 breaker, MinIO as production, "every blob hashed", renewal without a DB write, 30 % gate). Report file and line.
7. **Latency.** Re-derive the publication intervals from the measured rates; is the donor-death distinction correct and is any categorical claim left unsupported?
8. **What would you cut?** If one structural simplification would remove a whole class of findings, name it.
9. **New errors** introduced by R3.

## Output contract

- Write findings to `review/claude-r4.md` or `review/gpt-r4.md`.
- First line: `PROCEED`, `PROCEED-WITH-CHANGES` or `STOP`.
- Numbered findings with P0/P1/P2 and file:line. No large quotations; reference locations.
- Read-only. Do not modify any design document, probe, manifest or baseline.
- Chat reply ≤ 200 words: verdict, counts, three worst findings, path.
