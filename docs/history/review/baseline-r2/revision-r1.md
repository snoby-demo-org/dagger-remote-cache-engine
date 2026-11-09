> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Revision R1 — change ledger

Date 2026-11-07. Response to `review/claude-review.md` (63 findings, 9 P0, verdict STOP). Baseline copies are in `review/baseline/`. This file is the map: what changed, where, and what is still open.

## 1. Verdict on the review

Of 63 findings, **61 hold**. Two were wrong and are documented below rather than silently dropped. The P0 set was correct: the design as written could publish a part whose blob was being deleted, could permanently disable the cache in "mark-only" mode, and had no computable hit/miss metric.

## 2. Changes by file

### `architecture.md`

| Finding | Old claim | Now |
|---|---|---|
| F12 (P0) | Publication "aborts if zero rows change" | Single transaction, **rowcount must equal chain length**, else rollback and no `SetStoredPart`. A two-blob chain with one blob `deleting` can no longer publish. |
| F14 | Restore trusted the checkpoint | After restore, every digest not `verified` in the ledger is dropped before serving merges. |
| F16/F17 (P0) | Mint not stamped; stamps buffered in memory | Mint = conditional `UPDATE … RETURNING` **committed before the URL leaves the process**; missing rows ⇒ omit + `DropStoredParts`; Postgres down ⇒ stop minting. |
| F18 | G = GET TTL + 30 s | **G = 2 × GET TTL = 7200 s** (30 s was not a transfer bound). |
| F13 | Lease/URL TTL contradiction (PUT 15 min > 10 min lease) | **PUT 5 min**, lease 10 min, lease purge after `lease_until + PUT TTL`. |
| F30 | (metrics doc) `evicting` state, mark-only disabled blobs | One enum `pending|verified|deleting`; mark-only writes `gc_candidate_since` and changes nothing. |
| F1 (P0) | (live manifest) 4 GiB memory | `limits: 12Gi`, `requests: 6Gi`; #14884 peaks 6.0–6.1 GB; **OOMKill is not fail-open** — the build dies with the engine. |
| F2 | Helm `hostPath` disabled plus emptyDir | Already correct in the doc; live release still needs the values change (§5). |
| F3 | "dcache is the only stateful component" | `dcache` **and Postgres** are stateful; production S3 is external; checkpoints every ≥10 min on change. |
| F5 (P0) | Discovery assumed | Marked **UNPROVEN**; Phase 0 spike required before service code (in `decision.md`). |
| F6/F43 | `alreadyComplete` decided before capability/visibility | Explicit **decision order**: capability → chain verified *and* visible to pool → execution started. |
| F8 | Verify "streamed from MinIO" (unspecified path) | Verify streams **through the S3 endpoint, never a local copy**; sampled 1%, HEAD + capability default. |
| F9 | Reaper released any cacheID | Release only when the cacheID is absent from `LiveEntries()` for the live-engine snapshot; per-pool PK and sequences (see §4 rule R7). |
| F10 | Renew hot path implied graph/DB reads | In-memory chain→digests index per pool; hot path needs no SQLite walk or Postgres round trip. |
| F34 | Retention refreshed on "merge or offer" | **Install and merge only**; `accepted` offer is not a serve. |
| F35 | 7 d refreshed-on-serve vs 30 d idle prune contradicted | Retention edge 7 d (install/merge); `idle_prune_after` 30 d applies to never-installed roots. |
| F31 | Quota eviction by LRU to 90% | Score-based, **marginal bytes**, watermarks 0.85/0.70/0.95, `pending_free_bytes` subtracted. |
| F41 | "On SIGTERM the service stops scheduling exports" | No SIGTERM reaches the service over the protocol, and AKS does not drain without a handler ⇒ Scheduled-Event handler + `preStop`, or accept the race. |
| F59 | NetworkPolicy denies `api.dagger.cloud` | Impossible by FQDN ⇒ CoreDNS NXDOMAIN or Cilium `toFQDNs`. |
| F40 | "Capacity-plan NICs" | Capacity-plan **disk throughput** and the VM uncached-disk cap; Dsv5 already advertises ~12.5 Gbps. |
| F37/F46 | p99 latency rule of thumb | Per-part cost model + admission factor `k ≈ 3`; n=200 makes "p99" the second-largest sample. |
| F62 | Checkpoint every 60 s + warm standby tailing | ≥10 min floor, alarm on `ckpt_bytes`/`ckpt_seconds`, keep last 5 by pointer; **cold restore accepted**, warm standby rejected. |

### `metrics-and-eviction.md` — rewritten

- **F20/F21/F23/F24/F25 (P0 metric):** hit is an **install** (`cache.offer.part`), not a `hit` span and not `hit.remote`; saved = lazy-inclusive recompute − install seconds; spent = self-time + lazy spans; `would_hit` and `lost_keying` are **subsets** of spent, not additive.
- **F22:** `cache.offer.duration_ns` is uint64 ns → seconds conversion specified.
- **F26:** `repo` label removed (no source of truth); `pool` only.
- **F29:** `leader_epoch` is a gauge, not a counter.
- **F32:** tombstone table; regret computed per reason, orphans excluded.
- **F33:** admission keyed on `(self_digest, structural_inputs)`, window 7 d, relative cost gate — the old `2 sessions/1 h` + `60 s` gate rejected exactly the workloads worth caching.
- **F34:** served = install only.
- **F35:** two clocks reconciled.
- **F12/F13/F14/F16/F17/F18/F30:** the safe-eject sequence is now executable SQL, not prose.
- **F62:** checkpoint cadence and size alarm.
- **F3:** Postgres named as stateful.

### `latency-and-azure.md` — rewritten

- **F36:** PUT was mis-timed; body 27 ms (~156 MB/s) + 19 ms response wait, not "84 ms TTFB". Publication of a 2 GB layer is **≈26–55 s of transfer**, so "publishes inside 30 s" is false.
- **F39:** Azure Blob **does** have capability semantics — SAS GET supports `Range`/`206`/`403`. The real blocker is the missing `x-ms-blob-type` header on PUT, which makes a hybrid (GET via SAS + staging PUT + server-side copy) viable.
- **F40:** size on disk throughput, not NIC.
- **F49:** credentials from a Secret via `envFrom`, never `--env` on the command line.
- **F41:** AKS preemption is not a pod drain.

### `decision.md`

- Topology, statefulness, publication gate, GC fence, R4, R7, Phase 0 (Tier 2 credentials as Secrets, F63), and an explicit **Phase-0 gate is unmeasured / discovery unproven** note.

## 3. Normative rules added (apply to any implementation)

1. **Publication is one transaction with `rowcount == |chain|`.**
2. **Every URL mint is committed before the URL leaves the process.** No in-memory stamps, ever.
3. **Delete state is `deleting` and nothing else**; mark-only means zero state changes.
4. **G = 2 × GET TTL.** A presigned URL outlives any in-process intent to delete.
5. **A hit is an install.** Nothing else counts in the numerator.
6. **Spent/saved are lazy-inclusive** — otherwise both read ≈0 and the go/no-go is meaningless.
7. **Per-pool keys and sequences** (`PRIMARY KEY (pool,digest)`, monotonic local sequence), because a global sequence is a cross-pool side channel and #15049 makes the eq-class walk the scheduler.
8. **Verify over the wire** (S3 endpoint), sampled, with `verify_mismatch` alerting at >0.
9. **Import fills local disk**: a remote install must not evict the local cache the engine is using (`importRemotePart` invalidation, F15) — measure disk after a warm merge (spike c).
10. **Merge insert happens outside the dagql graph lock**, or a 48 MiB merge blocks lookups (#15049).
11. **Offer decision per cacheID** with a per-entry mutex; two engines must not both get `accepted` for one entry (F44).
12. **No private-API port of dagql.** If a needed method is unexported, upstream the patch, don't fork the codec.

## 4. Findings rejected (with evidence)

| Finding | Claim | Evidence |
|---|---|---|
| F27 | `cache.miss.saw_expired`, `incompatible_candidates`, `producer_released`, `requirements_gained` do not exist | They are defined at `engine/telemetryattrs/attrs.go:662-689`; the reviewer read 608–793 selectively and skipped 626–690. |
| F28 | Engine Prometheus metric names unverified | `cmd/engine/metrics.go` defines all six; `cmd/engine/main.go:655` gates the listener behind `_EXPERIMENTAL_DAGGER_METRICS_ADDR`. |

## 5. Still open — do not write service code until these close

| Item | Owner action | Blocking |
|---|---|---|
| **F5 (P0)** does a call span reach a self-hosted `DAGGER_CLOUD_URL`? | Query existing Tempo traces (`helmrelease.yaml:82-100`) — about an hour, no shadow engine needed | Discovery. No spans ⇒ no entry numbers ⇒ no offers. |
| **F21 (P0) verify-on-real-engine** | Run one beta.25 engine with a contract dump; confirm `cache.offer.part` fires per install and lazy spans carry `result.id` | The whole metric model. |
| **F51** memory ceiling | Spike (c): fresh engine RSS after a 48 MiB merge; then set limits | OOMKill takes builds down. |
| **F36** publication budget | Spike (b): 30 MB / 500 MB / 2 GB layer, span end → verified | Determines whether spot caching is viable at all. |
| **F39c** MinIO supply chain | Confirm whether upstream MinIO still ships community binaries/images | Primary store you cannot patch is disqualifying; fallback Ceph RGW. |
| **F62** checkpoint cost | Measure `Checkpoint()` + upload at 1 GB and 7 GB graph | Failover budget and write amplification. |
| Ledger state machine as one spec | Write `ledger.md` (reserve/takeover upsert, publish fence, mint-as-stamp, delete fence, restore revalidation, tombstone) | Implementation correctness; the SQL above is a sketch, not the spec. |

## 6. What this revision does *not* claim

- No code exists. Nothing here has been run against a live beta.25 engine.
- The Azure backing store is still unproven for PUT; the hybrid option is a hypothesis until the header behaviour is tested.
- The Phase-0 gate number has not been computed, so the build decision is still open.
