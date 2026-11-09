> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Metrics + eviction policy — addendum to the dcache design

Date 2026-11-05. Applies to `decision.md` / `architecture.md` with the topology correction below.

## 0. Topology correction (from the owner)

- **Design cluster** = this homelab k3s. Nodes are **persistent**. In-cluster MinIO (PVC, `192.0.2.10:9000`) is the S3 backend here. `dcache`, MinIO and Postgres can all live on those nodes. This is where the service is built and chaos-tested.
- **Production cluster** = **external S3**, nodes **ephemeral** (~30 s). The byte plane (presigned PUT/GET) points at the external endpoint; engines never hold durable state.
- What this changes:
  1. The "MinIO on spot nodes" risk (R5) is a *design-cluster* non-issue and a *production* non-issue, because production does not run MinIO at all.
  2. Production's real new budget is **egress + WAN latency to the external S3**, not local NIC bandwidth. Every S3 op gets a latency SLO and a breaker (`§2.4`).
  3. `dcache` is the **only stateful component** and must never land on ephemeral capacity: production pins it to a stable control-plane pool or runs it outside the cluster. Leader failover = restore from checkpoint, and restore is linear in graph size (#15013), so "warm standby that tails checkpoints" is mandatory in production, not optional.
  4. Chaos tests that kill nodes are valid in production only; in the design cluster you simulate them (kill engine pod, inject S3 latency/5xx, pause the leader) — the failure semantics are identical.

## 1. What is measurable, and where the truth lives

Three sources, and only one of them can answer "hit or miss" correctly:

| Source | What it gives | Limits |
|---|---|---|
| **Engine Prometheus** (`/metrics`, enabled by `_EXPERIMENTAL_DAGGER_METRICS_ADDR`, cache gauges refresh every `_EXPERIMENTAL_DAGGER_METRICS_CACHE_UPDATE_INTERVAL`, default 5 min) | `dagger_connected_clients`, `dagger_dagql_cache_entries`, `dagger_dagql_cache_metadata_estimated_bytes`, `dagger_local_cache_total_disk_size_bytes`, `dagger_local_cache_entries`, `dagger_local_cache_corrupt_db_reset` | Gauges only. **No hit/miss, no counters, nothing remote-cache specific.** Verified in `cmd/engine/metrics.go`, `cmd/engine/main.go:654`. |
| **OTLP spans/logs ingested by `dcache`** | The authoritative per-call facts: `dagger.io/cache.outcome` ∈ {hit, executed, joined, uncached}, `cache.hit.remote`, `cache.hit.route` ∈ {recipe, digest, structural}, `cache.miss.*` reasons, `cache.pairing_digest`, `cache.self_digest`, `cache.offer.bytes`, `cache.offer.duration_ns`, retention/expiry attrs (`engine/telemetryattrs/attrs.go:608-793`) | Discovery is push-only; spans can be dropped (bounded queue). Treat as near-lossless, reconcile with WS events. |
| **`dcache` internals** (own Prometheus registry) | Offers/merges/renewals/uploads/verifications/GC/ledger/S3 client/leader state | Only place that knows *why* an offer failed and what it cost. |

**Rule:** the hit/miss counter is derived in `dcache` from `cache.outcome`, never inferred from a cache size gauge. Report both an unweighted ratio and a **duration-weighted** ratio — a 0.5 s hit and a 40 min hit are not the same event.

## 2. Metric catalogue

### 2.1 Hit / miss (the SLO set)

```
dcache_lookups_total{pool,repo,outcome}           # outcome: hit|executed|joined|uncached
dcache_hits_remote_total{pool,repo,route}          # cache.hit.remote=true; route=recipe|digest|structural
dcache_miss_total{pool,repo,reason}                # incompatible_candidates|saw_expired|unknown_input|producer_released|requirements_grew
dcache_saved_seconds_total{pool,repo}              # Σ span duration where outcome=hit AND hit.remote=true
dcache_spent_seconds_total{pool,repo}              # Σ span duration where outcome=executed
dcache_lost_session_keying_seconds_total{pool,repo} # pairing_digest matched a prior self_digest, self_digest differed (#15014/#15044/#15034)
dcache_uncached_total{pool,repo}                   # DoNotCache: excluded from hit-ratio denominator
```

Definitions that must not drift:

- **hit ratio** = `hits_remote / (hits_remote + executed)` — `joined` is in-process dedup, not a cache hit; `uncached` is by design and excluded.
- **value ratio** = `saved_seconds / (saved_seconds + spent_seconds)` — the number that justifies the project.
- **addressable ceiling** = `(saved + lost_session_keying) / (saved + spent + lost_session_keying)` — this is the Phase-0 go/no-go, and it is also a *continuous* production metric: when `lost_session_keying` stays high while `saved` is flat, the upstream per-session-keying bugs own the outcome, not the cache.

### 2.2 Transfer plane

```
dcache_offers_total{pool,outcome}                  # accepted|alreadyComplete|executionStarted|unavailable|invalid
dcache_offer_bytes_total{pool}                     # cache.offer.bytes
dcache_offer_install_seconds{pool}                 # histogram; cache.offer.duration_ns
dcache_publish_seconds{pool}                       # histogram: span end → part verified  (exposure window)
dcache_renew_total{pool,result}                    # renewed|unavailable|bad_key|too_slow
dcache_renew_seconds{pool}                         # histogram, SLO reply < 2 s (engine mailbox deadline)
dcache_verify_total{pool,result}                   # ok|mismatch  — mismatch must be 0
dcache_verify_seconds{pool}
dcache_exports_total{pool,skip_reason}             # gone|busy|refused|oversize|equivalent|pending
dcache_export_slot_age_seconds{pool}               # engine holds 4 slots; >60 s means the service is starving it
dcache_merge_payload_bytes{pool}                   # cap ~48 MiB per warm merge
```

### 2.3 Bytes, storage and ledger

```
dcache_blob_bytes{pool,state}                      # pending|verified|deleting
dcache_blob_count{pool,state}
dcache_storage_bytes_used{pool}                    # ledger SUM(size)
dcache_storage_quota_bytes{pool}
dcache_s3_requests_total{op,code}                  # op=put|get|head|delete|list
dcache_s3_seconds{op}                              # histogram; production: external S3, so this is the breaker input
dcache_s3_egress_bytes_total                      
dcache_ingest_spans_total{pool,contract}           # contract="1" only; others dropped
dcache_ingest_dropped_total{pool,reason}
dcache_sessions{pool,state}                        # connected|quarantined|ended
dcache_leader_is_leader{pool}                      # 0/1
dcache_leader_epoch{pool}                          # counter; increments on failover
```

### 2.4 Alerts (starting set, all actionable)

| Alert | Expression sketch | Why |
|---|---|---|
| Verification mismatch | `increase(dcache_verify_total{result="mismatch"}[10m]) > 0` | Correctness; page. |
| Publication too slow | `histogram_quantile(.95, rate(dcache_publish_seconds_bucket[15m])) > 60` | Exposure window exceeds short-lived nodes → cache fills with cheap early layers only. |
| Renewal failing | `rate(dcache_renew_total{result="unavailable"}[5m]) / rate(dcache_renew_total[5m]) > 0.01` | Bytes vanished (GC too aggressive) or S3 unreachable. |
| Version quarantine | `dcache_sessions{state="quarantined"} > 0` | Engine/service drift → silent zero hits. |
| Leader flapping | `changes(dcache_leader_is_leader[15m]) > 2` | Restore cost / split-brain risk. |
| Export starvation | `dcache_export_slot_age_seconds > 60` | Service bug pins engine chains to local disk; can fail builds. |
| Expensive storage, no value | `dcache_storage_bytes_used / dcache_storage_quota_bytes > 0.7` **and** `rate(dcache_hits_remote_total[1h]) == 0` | Cache is bloated with unreachable entries (per-session keying) — stop growing it. |
| Eviction regret | `rate(dcache_evicted_refetch_total[24h]) / rate(dcache_gc_evicted_total{reason="value"}[24h]) > 0.05` | Policy is evicting things that come straight back. |
| External S3 slow | `histogram_quantile(.99, rate(dcache_s3_seconds_bucket{op="get"}[10m])) > 2` | Breaker input; a hanging GET stalls builds 30 s before fallback. |

## 3. Eviction: the "smarts"

The engine already tells us the two things a policy needs: **whether a miss was caused by expiry** (`cache.miss.saw_expired`) and **what the entry cost to compute** (the producing span's duration). Use them.

### 3.1 Value model

Per stored root/part, keep: `bytes`, `recompute_seconds` (span duration at publication), `served_count`, `last_served_at`, `created_at`, `expires_at`.

```
score = (Σ served_events × w(Δt) × recompute_seconds) / max(bytes, 1 MiB)
w(Δt) = 0.5^(Δt / H)          H = half-life, default 7 d per pool, tunable
```

Evict the lowest score first. This is GDSF (GreedyDual Size-Frequency) with a cost term: it keeps small, expensive-to-recompute artifacts and drops large, cheap-to-recompute ones — which is exactly the shape of a CI workload (a 2 GB toolchain layer that is recomputed in 20 s is worth less than a 30 MB compile layer that takes 4 min).

### 3.2 Admission control (the part that stops pollution)

Do **not** admit on first sight. A blob must clear one of:

- **Frequency gate:** ≥2 distinct sessions requested the same `pairing_digest` within the admission window (TinyLFU-style counting, 1 h), or
- **Cost gate:** `recompute_seconds ≥ admission_min_cost` (default 60 s), or
- **Pinned:** explicitly warmed (release-branch prewarm job).

Rejected entries are still *meta-stored* (recipe → digests) so a later hit can be recognized, but their bytes never upload. This is what keeps a bucket from filling with one-off branch builds while costing almost nothing in hit rate (two-hit-wonder workloads dominate CI).

### 3.3 Timeouts, and the two different clocks

- **Value clock (retention):** `expiresAtUnix` on the stored root, default 7 d, refreshed on every serve/merge/offer. `no serve within 30 d` → prune root.
- **Byte clock (blob):** a blob has no independent TTL. It dies only when *no part references it*, after the GC fence (§3.4). Bucket lifecycle TTL is never applied to blobs (a shared blob can outlive one value's expiry).
- `cache.miss.saw_expired` is the ground truth that the value clock fired and someone wanted it back. Feed it into the same regret counter as §3.4.

### 3.4 Safe eject sequence (no torn reads, no deleting bytes under a live URL)

Never a bare `DELETE`. Per candidate:

1. **Eligibility (single conditional SQL):**
   `UPDATE blob_ledger SET state='evicting' WHERE pool=$1 AND digest=$2 AND state='verified' AND last_ref_at < now()-G AND last_url_mint_at < now()-G`
   — zero rows changed ⇒ skip (`dcache_gc_skipped_total{reason="live"|"recent_url"}`).
   `G ≥ max GET-URL TTL (1 h) + engine 30 s stall + clock skew`.
2. **Not in `LiveBlobs()`** at the sweep's graph snapshot, and no `pending` upload lease.
3. **Re-check at delete time** (fence again; the graph may have referenced it in between). On conflict → revert row to `verified`.
4. **Delete the object** (`dcache_gc_evicted_total{reason="value"|"quota"|"orphan"}`), then delete the ledger row.
5. **Repair on miss:** if a renewal or GET finds the object gone, `DropStoredParts(digest)` and reply `renewed{unavailable:true}` — the engine falls back and recomputes. Never serve a half-truth.
6. **Regret accounting:** if the same digest is re-uploaded within 24 h, increment `dcache_evicted_refetch_total`. That counter, plus `saw_expired` misses, is the feedback loop: if regret > 5 %, raise `H`, raise the retention floor, or lower the quota-pressure threshold — do not just widen the quota.

### 3.5 Storage-pressure controller

Watermark with hysteresis, not a hard cap:

- `used/quota > 0.85` ⇒ evict by score until `≤ 0.70`.
- `used/quota > 0.95` ⇒ **stop admitting** new bytes (exports return `urls:{}`; parts stay metadata-only) until `≤ 0.80`. Builds keep running; they just stop growing the cache.
- Per-pool quota, plus a per-repo soft quota so one monorepo cannot starve the others.
- Never evict below `min_retention` (default 24 h) — a job that publishes and immediately needs its own output must not lose it.

### 3.6 Config sketch

```yaml
dcache:
  cache:
    half_life: 168h
    min_retention: 24h
    value_ttl: 168h
    idle_prune_after: 720h          # "never used"
    admission: { min_frequency: 2, min_cost_seconds: 60 }
  storage:
    quota_per_pool: 3TiB
    high_watermark: 0.85
    low_watermark: 0.70
    hard_watermark: 0.95
  gc:
    sweep_interval: 1h
    url_grace: 3900s                # GET TTL 1h + 30s stall + skew
    start_mode: mark_only           # first 2 weeks: mark, never delete
  slo:
    publish_p95_seconds: 60
    renew_p99_seconds: 2
    s3_get_p99_seconds: 2
```

## 4. Dashboards

1. **Value:** value ratio (saved vs spent seconds) by repo; saved-seconds/day; addressable ceiling.
2. **Hit/miss anatomy:** stack of miss reasons over time — this is the tuning surface. `saw_expired` rising with storage flat = TTL too short. `incompatible_candidates` = key hygiene. `unknown_input` = engine bug.
3. **Transfer health:** publication p50/p95, offer outcomes, renew p99 vs the 2 s line, verify mismatches (should be an empty panel that stays empty).
4. **Storage:** used/quota, blob count by state, evictions/bytes reclaimed, **eviction regret**, admission rejections.
5. **Dependency:** external S3 p50/p99 per op (production), egress bytes/day, breaker state; leader epoch.
6. **Engine-side (scraped):** per-pod `dagger_dagql_cache_entries` / `metadata_estimated_bytes` / `connected_clients` — local cache pressure and exposure, scraped via the Helm `containerPorts: metrics` + `_EXPERIMENTAL_DAGGER_METRICS_ADDR`.

## 5. What we deliberately will *not* measure

- No per-digest Prometheus labels (cardinality bomb). Per-digest facts live in the ledger, not in metrics.
- No "cache hit rate" from bytes alone — a byte hit ratio without the duration weighting is exactly the number that lies in CI.
- No alert on absolute hit ratio: low hit rate on a fork-PR pool is expected and correct.
