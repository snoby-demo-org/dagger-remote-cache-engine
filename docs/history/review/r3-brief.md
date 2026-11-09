> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Round-3 review brief — Revision R2 of the Dagger shared-cache design

You are the adversarial reviewer for **round 3**. The artifact is a design document set, deliberately unimplemented. Do not ask for code; judge whether the design is consistent, provable from cited evidence, and executable.

## Situation

A shared, S3-style cache for Dagger CI:
- **Design cluster**: homelab k3s, persistent nodes, in-cluster MinIO (`minio.minio.svc.cluster.local:9000`), Postgres, `dcache`.
- **Production**: Azure, **S3 outside the cluster** (endpoint/region/provider per deployment), compute nodes ephemeral with **~30 s notice**. No MinIO in production.
- Requirements: Prometheus metrics with real hit/miss rates; safe eviction of unused/expired entries.
- Engine: Dagger `v1.0.0-beta.25` (`7e9eaac`). Remote cache is a **push** protocol: WS `/v1/engine`; the service decides exports and issues presigned PUT URLs; whole-object single PUT, no multipart, no engine retry; reads are Range GETs requiring exact `206 Content-Range`; renewal reply deadline 2 s; **no inventory message, no on-miss lookup** → discovery needs OTLP ingestion.

## Round history

- **Round 1** (Claude Opus): 63 findings, verdict STOP. `review/claude-review.md`.
- **Round 2** (a different model): 19 findings (4 P0 / 13 P1 / 2 P2), verdict STOP. `review/gpt-review.md`. Its required-closure list was: reused/mixed-owner chains, NULL mint stamps, graph-publication pins, failed/deleting recovery, delayed external deletes, the metric event schema, topology/verification-policy/measurement-point consistency, and the two spikes.
- **R2** (what you are reviewing) is the response. `revision-r2.md` maps every round-2 finding to a change; read it first, then verify it did not overclaim.

## Files

- `revision-r2.md` — finding→change map, open items, loop policy.
- `ledger.md` — **the normative blob state machine**: tables, transition table, reserve, owner-fenced verify, all-distinct-verified publish under a durable pin, waiter path, mint-before-send, generation-scoped keys, GC mark/delete, recovery scanner, restore, invariants.
- `architecture.md` — invariants I1–I9 (§3), components (§4.2/§4.3), chunking + verification policy (§5/§5.1), GC/TTL (§6), cost (§8), flows (§9), failure modes (§10), red team (from ~line 313).
- `metrics-and-eviction.md` — event model, decision classes, cost estimate, metric definitions, engine-path measurement, reclaim groups, admission, config, alerts.
- `latency-and-azure.md` — measurement table, corrected publication arithmetic, topology conformity, measurement points, breaker, serve-cost model, probe usage.
- `decision.md` — brief.
- `s3_latency_probe.py` — v3 (assertions). `s3_latency_probe_v2.py` — previous.
- `review/r2.diff` — unified diff vs `review/baseline-r2/`.
- `review/baseline-r2/` — pre-R2 copies.

## Measured facts you may rely on (design cluster, presigned URLs)

GET 4 MiB TTFB p50 3.96 ms / p95 6.13 / p99 8.03 (n=200); GET total p50 43.1 ms; open-ended range TTFB p50 4.10 (n=100); HEAD p50 1.92 (n=50); PUT 4 MiB body 27.0 ms (155.6 MB/s) + 18.8 ms ack (**n=1**); 8 concurrent PUTs 70.0 MiB/s (73.4 MB/s) aggregate; expired capability 403.

## Questions — answer each explicitly

1. **Closure audit.** For all 19 round-2 findings, cite the clause that closes it, or state it is still open. Do not accept `revision-r2.md`'s word.
2. **Attack `ledger.md`.** Find a concrete interleaving that breaks:
   (a) two concurrent uploads of one digest;
   (b) a publish racing a GC mark/delete, including a publisher paused longer than the 2 min pin;
   (c) a delayed S3 `DELETE` from a deposed leader arriving after a new generation is published;
   (d) crash between mark and delete, and between delete and the `deleted` stamp;
   (e) reserve while a row is `deleting`;
   (f) a waiter whose chain partner is owned by another export that dies.
   For each: does the design hold, and if not, give the interleaving and the minimal fix. Say explicitly when you cannot break it.
3. **Metric computability.** Are the seven decision classes, `cost_estimate`, exclusive-time attribution and the disjoint opportunity sets actually derivable from the cited attributes? Name anything assumed but ungrounded. Check that no ratio can exceed 100 % or divide by zero.
4. **Topology consistency.** Does every normative section now describe external S3 in production with no MinIO, `dcache` + Postgres stateful, cold restore? Quote any surviving contradictory clause.
5. **Publication math.** Re-derive the 12.85 / 27.3 / 13–28 / 34–50 s figures from the measured numbers; state whether the replaced categorical claim is now correct and whether the residual tail risk is described honestly.
6. **Verification policy.** Is the single §5.1 policy consistent across I1, the overview, the cold flow, the failure table and `decision.md`? Is calling it "authorized+existence+size, 1 % hash" a defensible tradeoff for a fail-open cache?
7. **Eviction.** Does reclaim-group scoring actually fix the cold-sharer pathology, and what new pathology does it introduce? Is the admission rule coherent and applied before the single upload continuation?
8. **Probe.** Do the assertions in `s3_latency_probe.py` v3 match the claims in `latency-and-azure.md` §0 and §7? Any units or order-statistic error left?
9. **New errors.** Anything the R2 fixes broke. File and line.

## Output contract

- Write findings to the file named in your task (`review/claude-r3.md` or `review/gpt-r3.md`).
- First line: `PROCEED`, `PROCEED-WITH-CHANGES` or `STOP`.
- Numbered findings, each marked P0/P1/P2, with file and line references.
- Do not quote large blocks; reference locations.
- Do not modify any design document, probe or manifest. Read-only.
- Chat reply ≤ 200 words: verdict, counts, your three most serious findings, path written. Do not restate findings inline.
