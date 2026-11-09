> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Metrics + eviction policy — revised (R1)

Updated 2026-11-07 after the independent review. Every change is keyed to a finding; the disposition table is at the end.

## 0. Topology (settled)

- **Design cluster** = this homelab k3s, **persistent nodes**. In-cluster MinIO (PVC) + Postgres + `dcache` live here. Chaos tests run here (kill engine pod, inject S3 latency/5xx, pause the leader).
- **Production** = **external S3 outside the cluster**, compute nodes **ephemeral** (~30 s *notice*; lifetime distribution still unmeasured — see D1). Production runs no MinIO.
- `dcache` **and Postgres are both stateful** (F3) and must never land on ephemeral capacity: tainted non-spot pool or outside the cluster. Restore is linear in graph size (#15013), so failover is a **cold restore with accepted outage**, not a tailing warm standby (F62).

## 1. Where the truth lives

| Source | What it gives | Limits |
|---|---|---|
| **Engine Prometheus** (`/metrics`, enabled by `_EXPERIMENTAL_DAGGER_METRICS_ADDR`) | `dagger_connected_clients`, `dagger_dagql_cache_entries`, `dagger_dagql_cache_metadata_estimated_bytes`, `dagger_local_cache_total_disk_size_bytes`, `dagger_local_cache_entries`, `dagger_local_cache_corrupt_db_reset` | Gauges only, no hit/miss. Names verified in `cmd/engine/metrics.go` + `cmd/engine/main.go:655` (review F28 rejected — see disposition). |
| **OTLP spans/logs** | `cache.outcome` ∈ {hit, executed, joined, uncached}; `cache.hit.remote` (**origin marker, not a hit counter**); `cache.hit.route`; `cache.miss.*`; `cache.pairing_digest`; `cache.self_digest`; `cache.structural_inputs`; `cache.result.id`; `cache.offer.part` / `.bytes` / `.duration_ns` (`engine/telemetryattrs/attrs.go:608-793`) | Push-only discovery; spans can drop. Lazy-evaluation spans carry `result.id` and **no outcome** (`attrs.go:738-742`) — this is why §2.1 changes. |
| **`dcache` internals** | offers/merges/renews/uploads/verify/GC/ledger/S3/leader | The only place that knows why an offer failed and what it cost. |

**Rule:** hit/miss is derived from *installs*, not from hit spans and not from a size gauge.

## 2. Metric catalogue (redefined)

### 2.1 Hit / miss — the corrections that matter

⚠ **F20/F21/F23/F24/F25**: the old definitions were not computable and would have produced a flattering number.

- A **hit span's duration is not the compute saved**; it measures the lookup.
- A metadata-only remote value records `hit` + `hit.remote` and then **recomputes** in a lazy span with no outcome — old `saved` counted it as a win and old `spent` never saw the work.
- Dagger evaluates lazily, so the expensive work sits in lazy spans; `outcome=executed` call spans are milliseconds. Both `spent` and `recompute_seconds` read ≈0 for exactly the expensive ops.
- `hit.remote` marks a **remote-origin entry**; every later local hit in the same session repeats it, so one module call hit 50 times looked like 50 remote hits.
- Nested spans double-count if you sum durations naively.

Redefined series:

```
dcache_engine_cache_decisions_total{pool,outcome}   # hit|executed|joined|uncached  (renamed from "lookups")
dcache_installs_total{pool,route}                   # distinct (cacheID,result.id) with a cache.offer.part install span
dcache_saved_seconds_total{pool}                    # Σ over installs (recompute_lazy_inclusive − install_seconds)
dcache_spent_seconds_total{pool}                    # Σ self-time of executed spans + lazy spans, per §2.1 join rule
dcache_would_hit_seconds_total{pool}                # executed spans whose (self_digest, structural_inputs) matched an earlier span in the lookback window
dcache_lost_keying_seconds_total{pool}              # equal pairing_digest, different self_digest (per-session keying)
dcache_uncached_total{pool}                         # DoNotCache, excluded from ratios
```

Definitions (normative):

- **install** = a `cache.offer.part` span that completed for a part (RP §2.1 / `attrs.go:772-788`). That is the only event counted as a remote hit. `offered{accepted}` is not an install; `executionStarted` is a miss.
- **recompute_lazy_inclusive**(entry) = Σ duration of lazy-evaluation spans joined on `(cacheID, result.id)`, plus the executed call span's **self time**. Never a naive sum over nested spans.
- **install_seconds** = `cache.offer.duration_ns` parsed as uint64 and divided by 1e9.
- **hit ratio** = `installs / (installs + executed)`, **distinct** entries per session, unweighted and duration-weighted reported separately.
- **value ratio** = `saved_seconds / (saved_seconds + spent_seconds)`.
- **addressable ceiling** = `(saved_net + would_hit + lost_keying) / (saved_net + spent)` where `would_hit` and `lost_keying` are **subsets of spent** (F23: the old formula counted them twice), matched on `(self_digest, structural_inputs)` for `would_hit` and on equal `pairing_digest`/different `self_digest` for `lost_keying`, over a bounded lookback (default 7 d).
- **`repo` label dropped** (F26): no protocol attribute, `hello` field or session identity supplies a repository. Attribution, if needed, comes from a trusted runner-scale-set → token → pool mapping, in the ledger, not in Prometheus.

### 2.2 Transfer plane

```
dcache_offers_total{pool,outcome}                  # accepted|alreadyComplete|executionStarted|unavailable|invalid
dcache_offer_bytes_total{pool}
dcache_offer_install_seconds{pool}                 # ns→s conversion required (F22)
dcache_publish_seconds{pool}                       # span end → part verified (exposure window)
dcache_renew_total{pool,result}                    # renewed|unavailable|bad_key|too_slow
dcache_renew_seconds{pool}                         # reply SLO < 2 s; engine deadline
dcache_verify_total{pool,result}                   # ok|mismatch|sampled — mismatch must be 0
dcache_verify_seconds{pool}
dcache_exports_total{pool,skip_reason}             # gone|busy|refused|oversize|not_stored|pending
dcache_export_slot_age_seconds{pool}
dcache_merge_payload_bytes{pool}                   # cap enforced on the final frame (<64 MiB)
```

### 2.3 Bytes, storage, ledger, checkpoints

```
dcache_blob_bytes{pool,state}                      # pending|verified|deleting   (one enum, F30)
dcache_blob_count{pool,state}
dcache_storage_bytes_used{pool}                    # ledger SUM(size) — charged per digest once
dcache_storage_marginal_bytes{pool}                # bytes referenced by exactly one part (drives eviction, F31)
dcache_storage_quota_bytes{pool}
dcache_s3_requests_total{op,code}
dcache_s3_seconds{op}
dcache_s3_egress_bytes_total
dcache_ckpt_seconds{pool}                          # checkpoint write duration
dcache_ckpt_bytes{pool}                            # full-DB size — the F62 alarm
dcache_ckpt_seq{pool}                              # gauge, advances on each durable checkpoint
dcache_ingest_spans_total{pool,contract}
dcache_ingest_dropped_total{pool,reason}
dcache_sessions{pool,state}                        # connected|quarantined|ended
dcache_leader_is_leader{pool}                      # gauge 0/1
dcache_leader_epoch{pool}                          # **gauge** holding the current epoch (F29)
```

### 2.4 Alerts

| Alert | Expression sketch | Why |
|---|---|---|
| Verify mismatch | `increase(dcache_verify_total{result="mismatch"}[10m]) > 0` | Correctness; page. |
| Publication too slow | `histogram_quantile(.95, rate(dcache_publish_seconds_bucket[15m])) > 90` | 2 GB layers need ≈26–55 s of transfer alone (F36); >90 s means it will never beat a 30 s kill. |
| Renewal failing | `rate(dcache_renew_total{result="unavailable"}[5m]) / rate(dcache_renew_total[5m]) > 0.01` | Bytes vanished or S3 unreachable. |
| Version quarantine | `dcache_sessions{state="quarantined"} > 0` | Drift → silent zero hits. |
| Leader flapping | `changes(dcache_leader_is_leader[15m]) > 2` | Restore cost / split-brain. |
| Export starvation | `dcache_export_slot_age_seconds > 60` | Service bug pins engine chains to local disk. |
| Checkpoint too big | `dcache_ckpt_bytes > 2e9` **or** `rate(dcache_ckpt_seconds_sum[1h]) > 30` | F62: a 60 s full-DB checkpoint at GB scale is TBs/day. |
| Storage with no installs | `dcache_storage_bytes_used / dcache_storage_quota_bytes > 0.7` **and** `rate(dcache_installs_total[1h]) == 0` | Bloated with unreachable entries (per-session keying) — stop growing. |
| Eviction regret | `rate(dcache_evicted_refetch_total{reason!="orphan"}[24h]) / rate(dcache_gc_evicted_total{reason!="orphan"}[24h]) > 0.05` | Evicting things that come straight back. |
| External S3 slow | `histogram_quantile(.99, rate(dcache_s3_seconds_bucket{op="get"}[10m])) > 0.25` | Same-VNet breaker; 2 s stays the separate hang detector. |

## 3. Eviction policy (rewritten)

### 3.1 Cost and value

- **cost** = `recompute_lazy_inclusive` (§2.1), never the call-span duration (F21).
- **served** = an **install** event only. A pre-merge or an `accepted` offer is not a serve (F34) — otherwise hot roots reinforce themselves forever.
- **bytes** = **marginal unique bytes** of the part (blobs referenced only by it). Shared blobs are charged to nobody, so evicting a cheap part no longer frees nothing and triggers a cascade (F31).

```
score = (Σ install_events × 0.5^(Δt/H) × recompute_lazy_inclusive) / max(marginal_bytes, 1 MiB)
H = 7 d default
```

### 3.2 Admission (fixed to stop starving real workloads)

⚠ **F33**: `≥2 sessions within 1 h` + `≥60 s recompute` rejects a 45 s monorepo build that runs daily — exactly what the warm node-local engine serves today — and, with F21, even 40-minute lazy builds measured ≈0 s.

- **Key** the frequency sketch on `(self_digest, structural_inputs)` — not `pairing_digest`, which by definition matches calls that can never hit.
- **Window** ≥ the maximum job period (7 d), TinyLFU-style counting sketch.
- **Relative cost gate**: admit iff `recompute_lazy_inclusive ≥ k · serve_cost(part)` with `k ≈ 3`, where `serve_cost` is the per-part model from `latency-and-azure.md` §4 (TTFB × layers + size/throughput + P(renew)·2 s + P(stall)·30 s).
- **Pinned** entries (release-branch prewarm) bypass admission.
- Rejected entries keep **metadata only**, so a later hit is still recognized, but no bytes upload. A first session with nothing admitted seeds nothing — say so in the docs, don't pretend otherwise (F5/F33).

### 3.3 Two clocks, reconciled

⚠ **F35**: "value TTL 7 d refreshed on serve" and "no serve within 30 d → prune" contradicted each other.

- **Retention edge** = `roots[].expiresAtUnix`, refreshed on **install and merge**, default 7 d. This is the only knob that expiring roots use (RP §7.1).
- **Idle prune** applies to roots with **no install ever** after `idle_prune_after` (30 d): they are pruned even if merged — pre-merge alone does not keep a root alive.
- **Blob clock** is independent: a blob dies only when no part references it, after the fence. Bucket lifecycle TTL is never applied to blobs.

### 3.4 Safe eject (the fence, corrected)

⚠ **F12**: the old "abort if zero rows change" allowed publishing a part whose blob was already `deleting`. ⚠ **F30**: `evicting` was not a legal state, and "mark-only" would have permanently disabled every marked blob while freeing nothing. ⚠ **F16/F17**: minting and renewal were not stamped; ⚠ **F13**: reserve/lease transitions were undefined.

Normative sequence:

1. **Reserve** (per digest, atomic upsert):
   ```sql
   INSERT INTO blob_ledger (pool,digest,size,state,lease_until,export_id,uploader_cache_id)
   VALUES ($1,$2,$3,'pending',now()+interval '10 min',$4,$5)
   ON CONFLICT (pool,digest) DO UPDATE
     SET state='pending', export_id=EXCLUDED.export_id,
         lease_until=EXCLUDED.lease_until, uploader_cache_id=EXCLUDED.uploader_cache_id
   WHERE blob_ledger.state='failed'
      OR (blob_ledger.state='pending' AND blob_ledger.lease_until < now())
      OR (blob_ledger.state='deleting' AND blob_ledger.delete_committed_at < now());
   ```
   `PUT URL TTL ≤ lease − max_verify_time` (15 min URL vs 10 min lease is **not** allowed; use 5 min URL / 10 min lease).
2. **Verify → publish**, one transaction:
   ```sql
   UPDATE blob_ledger SET state='verified', verified_at=now(), last_ref_at=now()
    WHERE pool=$1 AND digest=ANY($2) AND state='pending' AND export_id=$3;
   -- require rowcount == cardinality(distinct $2), else ROLLBACK and do not publish
   ```
   then `SetStoredPart`. A chain of two blobs with one in `deleting` now fails the rowcount check and does not publish.
3. **Mint (merge or renew)** is a **conditional stamp committed before the URL leaves the process**:
   ```sql
   UPDATE blob_ledger SET last_url_mint_at=now()
    WHERE pool=$1 AND digest=ANY($2) AND state='verified' RETURNING digest;
   ```
   Missing rows ⇒ omit that chain and `DropStoredParts`. No in-memory buffering of stamps. If Postgres is down, **stop minting** (fail open) and keep GC paused for G after recovery.
4. **GC mark** uses the **same state as delete** — one enum: `pending|verified|deleting`. There is no `evicting`:
   ```sql
   UPDATE blob_ledger SET state='deleting', delete_started_at=now()
    WHERE pool=$1 AND digest=$2 AND state='verified'
      AND last_ref_at < now()-G AND last_url_mint_at < now()-G
   ```
   `G = 2 × GET TTL` (7200 s) — "+30 s" was not a transfer bound (F18).
5. **Delete** (re-fence in the same transaction), `DELETE` the object, then delete the row. `failed` leases are purged only after `lease_until + PUT TTL`.
6. **Mark-only is a dry run**: write `gc_candidate_since` and a counter, **never** change state. Two weeks of the old design would have quietly disabled the cache.
7. **Restore revalidation (F14):** after a leader restore, walk `StoredState` and call `DropStoredParts` for every digest not `verified` in the ledger before serving merges.
8. **Repair on miss:** renewal/GET 404 ⇒ `DropStoredParts` + `renewed{unavailable:true}`.

### 3.5 Regret and the pressure controller

⚠ **F32**: deleting the ledger row erased the evidence, and the ratio could exceed 1 by mixing reasons.

- **Tombstone table** `(pool,digest,evicted_at,reason,referencing_parts)`. Regret = re-upload within 24 h, computed **per reason**, orphans excluded. Better: regret driven by `cache.miss.saw_expired` (attribute exists — `attrs.go:667`; review F27 rejected) plus executed spans whose recipe had a pruned root.
- **Controller** tracks `pending_free_bytes` and stops when `used − pending_free ≤ low_watermark` — otherwise it keeps evicting on every tick for ~2 h while sweeps lag (F31). One watermark set, shared with `architecture.md`: `0.85 / 0.70 / 0.95`.
- Per-pool quota. A per-repo soft quota only if a trusted repo source exists (§2.1).

### 3.6 Config sketch (revised)

```yaml
dcache:
  cache:
    half_life: 168h
    min_retention: 24h
    retention_edge: 168h
    idle_prune_after: 720h
    admission: { key: self_digest+structural_inputs, window: 168h, cost_factor: 3 }
  storage:
    quota_per_pool: 3TiB
    high_watermark: 0.85
    low_watermark: 0.70
    hard_watermark: 0.95
  gc:
    sweep_interval: 1h
    url_grace: 7200s              # 2 × GET TTL
    start_mode: dry_run           # gc_candidate_since only
  checkpoint:
    on_change_min_interval: 10m   # not 60 s (F62)
    keep: 5                       # pointer-driven, not bucket lifecycle
  slo:
    publish_p95_seconds: 90
    renew_p99_seconds: 2
    s3_get_p99_seconds: 0.25
```

## 4. Dashboards

1. **Value:** saved vs spent seconds, installs/day, addressable ceiling, `lost_keying`.
2. **Miss anatomy:** stack by reason — `saw_expired` up with storage flat ⇒ retention too short; `incompatible_candidates` ⇒ key hygiene; `unknown_input` ⇒ engine bug.
3. **Transfer:** publication p50/p95, offer outcomes, install seconds, renew p99 vs 2 s.
4. **Storage:** used/quota, marginal bytes, evictions/bytes reclaimed, **regret by reason**, admission rejects, dry-run candidates.
5. **Durability:** checkpoint bytes/seconds/seq, leader epoch, restore duration.
6. **Engine-side:** per-pod dagql entries / estimated bytes / clients.

## 5. Review disposition

| Finding | Disposition |
|---|---|
| F12, F13, F14, F16, F17, F18, F20–F26, F30–F35, F62 | **Applied** in this revision |
| F27 (miss attrs don't exist) | **Rejected** — `attrs.go:662-689` defines `incompatible_candidates`, `saw_expired`, `unknown_input`, `producer_released`, `requirements_grew`. Reviewer skipped lines 626–690. |
| F28 (engine metric names unverified) | **Rejected** — verified directly in `cmd/engine/metrics.go` and `main.go:655`. |
| F29 (leader_epoch is a counter) | Applied — now a gauge. |
| F31 watermark disagreement | Applied — one set, 0.85/0.70/0.95, shared with architecture. |
| F63 (Tier 2 creds in env) | Applied in `decision.md`: secrets, not env values that are part of the op key. |
