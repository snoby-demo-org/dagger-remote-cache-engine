> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Metrics + eviction policy — R4

Supersedes R3. Fixes round-4 findings 9, 10, 11, 15, 18, 27, 28, 29. The ledger is `ledger.md` (R4); donor cost now lives in its `part_cost` table, written at session finalisation, not at reserve.

## 1. Event model

### 1.1 Identity and finalization

- **Lookup identity**: `(pool, session_id, cache_id, result_id)`.
- **Entry revision**: use the engine's own counter, `dagger.io/cache.replacements` (absent at zero), never the service graph's notion of a replacement — receiver ids and their revisions belong to each engine cache, and the merged pool graph does not define them (round-4 GPT F11). Two observations of one id with different replacement counters are two revisions of one entry, not two entries.
- **Finalization**: classification happens at session end (or after a 10 min idle), once every span for the session has arrived or been declared missing. Completeness uses only like populations: the carrier span's `dagger.io/cache.session.spans` counts spans carrying `CacheResultIDAttr`, so it is compared with the **cache-result spans** ingested, never with all spans. A session whose completion carrier never arrives — killed engine, dropped batch — is `unknown`, not `complete`: its entries are excluded from ratios and flagged, because a 10-minute idle proves silence, not completeness (round-4 GPT F11).
- Replay/duplicates are deduped on `(session_id, span_id)`.

### 1.2 Per-entry measures (independent, not a single label)

```
install_parts, install_bytes, install_seconds   # import work, from cache.offer.part / .bytes / .duration_ns
recompute_seconds                               # interval union of evaluation work (§1.3)
outcome_raw                                     # hit | executed | joined | uncached
lazy_spans_present                              # boolean: was there evaluation work at all
session_spans_declared                        # dagger.io/cache.session.spans on the carrier span
self_digest, pairing_digest, structural_inputs[]
donor_cost_seconds                              # from part_cost[(pool,part_digest)], written at finalisation
```

Install and recompute are **kept as independent measures even on the same entry** (round-3 finding 7): a mixed entry that installs P and computes Q contributes both `install_seconds` and `recompute_seconds`.

Derived classes, used only for population counts:

| Class | Predicate |
|---|---|
| `installed` | install_parts ≥ 1 |
| `computed` | lazy_spans_present and no install |
| `local_hit` | outcome hit, no install, **no lazy span** (a metadata-only hit that evaluated nothing) |
| `joined` | outcome joined and no install/recompute evidence |
| `uncached` | outcome uncached |
| `incomplete` | session marked incomplete, or attributes missing |

`local_hit` no longer depends on "call overhead ≈ 0" — that made a normal local hit look like `deferred_recompute` (round-3 finding 7). A metadata-only remote shell is `computed` **and** carries the `hit` outcome; that is the R1 denominator gap, now closed by evidence rather than by label priority.

### 1.3 Recompute attribution — interval union

For each entry, walk the causal parent chain (`WcprofParentAttr`, falling back to the ordinary parent id) and **stop at the first span carrying a different `result.id`** — that boundary is a nested entry, and its seconds belong to it, not to the ancestor (round-4 finding 11). Collect the `[start, end]` interval of every span inside that boundary, subtract install intervals, and take the **measure of the union**.

This replaces R2's "self time + outermost lazy span only" rule, which returned 10 s for a 100 s outer interval containing a 90 s nested one (round-3 finding 8). Union handles nesting, overlap and concurrency by construction.

Two filters keep the union honest:
- **Evaluation only.** A span counts only if it carries `CacheOutcomeAttr=executed` or a lazy-evaluation attribute; a span that merely waits on another entry's lazy task, or does lookup/serialisation, is excluded. Subtracting install intervals is not enough to separate waiting from work.
- **Uncertainty is never credited.** A fallback attribution (`attribution_uncertain`) is reported but excluded from donor cost, admission and saved-seconds; it cannot silently substitute a cheap call interval for the expensive lazy one.
- **Disjoint, not inclusive.** The union is an inclusive diagnostic latency for one entry. Summing it across entries double-counts nested work (entry A 0–100, entry B 10–90 must not read as 180 s), so per-entry recompute is only ever reported per entry and never summed into a pool total as if it were independent work (round-4 GPT F12).

The union is evaluation work, not job wall time; it is never summed across parallel branches to argue wall-time savings (§2.4).

## 2. Metrics

```
dcache_decisions_total{pool,class}              # 6 derived classes above
dcache_installs_total{pool,unit}                # unit="part"|"entry"
dcache_recompute_seconds_total{pool}            # Σ recompute over computed + installed-mixed (finalised)
dcache_recompute_remaining_seconds_total{pool}  # Σ recompute not avoided: entries with no install + residual recompute of mixed entries
dcache_install_seconds_total{pool}              # Σ import work (all entries)
dcache_saved_gross_seconds_total{pool}          # Σ donor_cost_seconds over installed entries (avoided compute)
dcache_saved_net_seconds_total{pool}            # Σ max(donor_cost − install_seconds, 0)
dcache_lost_net_seconds_total{pool}             # Σ max(install_seconds − donor_cost, 0)
dcache_saved_unknown_total{pool}                # installed entries with no recorded donor cost
dcache_would_hit_seconds_total{pool}            # computed entries matching an earlier-session entry that had a verified part
dcache_lost_keying_seconds_total{pool}          # disjoint: pairing match, structural equal, self differs, earlier session, no install, earlier part verified
dcache_ingest_spans_total{pool,contract}
dcache_ingest_dropped_total{pool,reason}
dcache_ingest_incomplete_sessions_total{pool}
```

**Donor cost** is `part_cost(pool, part_digest)` — written **once per part, at session finalisation**, from the evaluation-only union-attributed recompute seconds of the entry that produced it, as a median over samples.

- **Key = the part, never the blob.** One part is one entry's snapshot; its chain's layers are that part's bytes, so the cost is **never summed across layers**. A blob shared by fifty parts carries no cost of its own (round-4 finding 10, GPT F13).
- **Two producers, one key.** Different recipes can produce identical bytes with different costs; the stored value is the median over recorded producers and is labelled `estimate_of_avoided_compute` with its sample count, not an intrinsic property.
- **Reuse records nothing**; that is correct, because the cost travels with the part, not with the reserver.
- Installs of a part with no `part_cost` row increment `saved_unknown_total`.
- **Mixed entries keep their residual.** An entry that installs P *and* computes Q contributes `donor(P)` to avoided and Q's seconds to `recompute_remaining`; crediting the whole entry cost to P would claim Q was avoided too (GPT F13).

**Accounting (round-3 finding 9):**

- `saved_gross` = avoided compute — a defensible upper bound, reported separately.
- `saved_net` / `lost_net` = net effect including the import cost, both nonnegative counters.
- **value ratio** = `saved_net / (saved_net + lost_net + recompute_remaining)` where `recompute_remaining` is `dcache_recompute_remaining_seconds_total` — recompute on entries with no install, as its own emitted series, so the denominator never silently mixes entry classes (round-4 finding 11). A C=60 s, I=59 s install yields 1/1 = **100 % of net**, but gross 60 s and net 1 s are both visible; nobody can quote a 60 s win from a 1 s gain.
- **hit ratio (entry)** = `installs{unit="entry"} / (installs{unit="entry"} + computed)`.
- **ceiling** = `saved_gross / clamp_min(saved_gross + recompute_remaining, 0.001)`; `recompute_remaining` **excludes** the `would_hit` and `lost_keying` entries, which are reported separately as opportunity, not as remaining work. R3's form put those sets in the denominator *and* in the numerator while mixing net with gross, biasing the ratio low (round-4 finding 28). The ratio is ≤ 1 by construction. All ratios are `NaN`/no-data when the denominator is 0.
- `lost_keying` remains a **candidate** diagnostic; `would_hit` also requires that the earlier entry had a **verified part at the time**, which R2 omitted.

### 2.1 Transfer

```
dcache_offers_total{pool,outcome}     # accepted|alreadyComplete|executionStarted|unavailable|invalid
dcache_offer_bytes_total{pool}
dcache_install_seconds{pool}          # histogram
dcache_publish_seconds{pool}          # span end → SetStoredPart returns (the commit point; no pin, no DB commit)
dcache_renew_total{pool,result}
dcache_renew_seconds{pool}            # SLO p99 < 200 ms, deadline 2 s
dcache_verify_total{pool,result}      # ok|mismatch|sampled_out
dcache_exports_total{pool,skip_reason}
dcache_export_slot_age_seconds{pool}
dcache_deferred_publish_chains{pool}  # chains waiting on a partner digest's verify event
```

### 2.2 Storage and durability

```
dcache_blob_bytes{pool,state}         # absent|pending|verified|deleting|failed|quarantined
dcache_storage_charged_bytes{pool}    # Σ size WHERE state IN ('pending','verified','deleting','quarantined','failed')
dcache_storage_reclaim_pending_bytes{pool}   # Σ size of rows in 'deleting' (the projection, from durable state)
dcache_storage_quota_bytes{pool}
dcache_live_blob_bytes{pool}          # Σ size of blobs in the last LiveBlobs() snapshot
dcache_live_blobs{pool}               # count of the last LiveBlobs() snapshot
dcache_gc_runs_total{pool,result}     # ok|error
dcache_gc_scan_seconds{pool}          # histogram: LiveBlobs() + mark
dcache_gc_evicted_total{pool}         # unique blobs transitioned verified→deleting
dcache_ckpt_seconds{pool}             # GAUGE: duration of the last checkpoint
dcache_ckpt_bytes{pool}
dcache_ckpt_seq{pool}
dcache_leader_is_leader{pool}
dcache_leader_epoch{pool}
dcache_gc_candidates_total{pool,mode}
dcache_evicted_refetch_total{pool}    # unique evicted blobs re-uploaded within 24 h of eviction (cohort)
```

### 2.3 Engine-path health — what is actually measurable (round-3 finding 10)

`cache.offer.part` fires only on **successful installation**. There is no grounded per-attempt failure attribute, so "install failure ratio" as R2 defined it had no numerator or denominator.

v1 keys the breaker on what *is* grounded:

| Signal | Source | Definition |
|---|---|---|
| `dcache_store_errors_total{op,code}` | **production: the provider's own metrics** (S3 → CloudWatch `5xxErrors`/`SlowDown`; Azure → `ServerBusyError`/`ClientTimeoutError`), which arrive **minutes late** (round-4 F15). Design cluster: MinIO metrics exporter. | 5xx and timeout counts per op, delayed. **Provider-side** liveness only — this cannot see a client-side hang. |
| `dcache_store_requests_total{op,code}` | same source | denominator for the error ratio |
| `dcache_install_p95_seconds` | `cache.offer.duration_ns` histogram | how long successful imports actually took |
| `dcache_computed_with_url_total{pool}` | `computed` entries that held a minted URL for one of their digests in the previous hour | a proxy for "the engine tried remote and fell back" — **not** a wait time; the stall itself is unobservable (round-4 F18) |
| `dcache_probe_s3_seconds{op}` | active probe, 30 s | liveness only, never a latency SLO |

**What the breaker still cannot see (stated, not patched around):** a store that accepts connections and never completes a GET. `install_p95` samples *successful* installs, so a hang that ends in fallback leaves no trace; provider metrics lag by minutes; and the service never sees the engine's GET. The compensating service-side signals are renewable-failure rate (`renew_unavailable`), `dcache_computed_with_url_total` (engines that computed despite holding a URL), and a rise in `dcache_deferred_publish_chains`. None of them measures the stall itself; a real attempt-level signal needs engine HTTP instrumentation that beta.25 does not have.

Thresholds: store errors > 2 % of ops over 10 min ⇒ warn; > 10 % ⇒ trip (stop merges/offers). `install_p95 > 5 s` ⇒ warn. The two thresholds are policy, not measurements.

### 2.4 Two gates, not one (round-3 finding 18)

R2's "run the pipeline live vs quarantined, then build the service" was circular — the live pool is the thing being built.

- **Gate A (pre-build, no service):** a discovery capture (does a call span reach a self-hosted `DAGGER_CLOUD_URL`?), plus a critical-path opportunity estimate from existing telemetry: per-pipeline `would_hit`/`lost_keying` upper bounds and the fraction of wall time in cacheable ops. This is measurable today.
- **Gate B (post-prototype):** a paired run of the prototype against today's **warm node-local baseline** with the same topology; compare p50/p95 wall time and failure rate. Only this delta justifies continued investment.

### 2.5 Alerts — corrected PromQL

| Alert | Expression |
|---|---|
| Verify mismatch | `sum by (pool)(increase(dcache_verify_total{result="mismatch"}[10m])) > 0` |
| Publication slow | `histogram_quantile(.95, sum by (pool,le)(rate(dcache_publish_seconds_bucket[15m]))) > 90` |
| Renewal failing | `sum by (pool)(rate(dcache_renew_total{result="unavailable"}[5m])) / clamp_min(sum by (pool)(rate(dcache_renew_total[5m])), 0.001) > 0.01` |
| Quarantine | `sum by (pool)(dcache_blob_bytes{state="quarantined"}) > 0` |
| Checkpoint too big | `dcache_ckpt_bytes > 2e9` or `dcache_ckpt_seconds > 30` |
| No installs | `dcache_storage_charged_bytes / dcache_storage_quota_bytes > 0.7 and sum by (pool)(rate(dcache_installs_total{unit="entry"}[1h])) == 0` |
| Regret | `sum by (pool)(increase(dcache_evicted_refetch_total[24h])) / clamp_min(sum by (pool)(increase(dcache_gc_evicted_total[24h])), 1) > 0.05` |
| Store errors | `sum by (pool)(rate(dcache_store_errors_total[10m])) / clamp_min(sum by (pool)(rate(dcache_store_requests_total[10m])), 0.001) > 0.02` |
| GC scan slow | `histogram_quantile(.5, sum by (pool,le)(rate(dcache_gc_scan_seconds_bucket[1h]))) > 1` |

Every ratio uses `sum by (pool)` on **both** sides and `clamp_min` on the denominator; the R2 renewal alert compared a labelled vector against itself and read 100 % on a single failure (round-3 finding 22).

## 3. Eviction

### 3.1 Demand and retention

- **Demand = installs only.** `last_install_at` is the pool clock.
- **Retention** (`expiresAtUnix`, 7 d) refreshes on genuine demand only. A merge extends the recipient engine's local retention, never pool retention (round-3 finding 15).
- **Idle prune** = `last_install_at` older than 30 d, applied to every root whether or not it was ever installed; roots never installed get `last_install_at = created_at` at merge time.

### 3.2 Reclaim groups — evict the **lowest** score

R2 said "pick the highest score" while defining score as benefit per byte, which evicted the most valuable group first (round-3 finding 11: it sacrificed 50× the benefit for the same bytes). Corrected:

- **Any set of parts is a candidate.** Freed bytes = the blobs whose referencing parts are **all** inside the set. Generate greedily from the lowest-benefit part outward: take the least-valuable part, add the parts that share its blobs, expand while the group's score stays under the pool median, and stop at 2,000 candidates. R3's closure rule ("removal leaves every referenced blob with zero surviving references") made whole connected components of anything sharing a hot base layer, so a cold sharer of a 2 GB base was unevictable without evicting the hot part that shares it (round-4 finding 9).
- `score = Σ benefit / max(distinct bytes freed, 1 MiB)`, `benefit = Σ installs × 0.5^(Δt/7d) × donor_cost_seconds`.
- **Evict the group with the lowest score first.** Zero-benefit groups (never installed) go first among equals, by oldest `last_install_at`.
- Unknown donor cost is **not** zero benefit: such parts sort last and are only evicted under the hard watermark, by age.
- Selection is **per part, lowest score first**, recomputing freed bytes after each drop: `freed = Σ size of blobs that were live before the drop and are not live after`. Stop at the low watermark; cap overshoot at 10 % of the target; no "split a group" rule is needed because dropping parts one at a time is always legal (§8 of `ledger.md`).

### 3.3 Quota accounting (round-3 finding 12)

- **Charged bytes** = `SUM(size) WHERE state IN ('pending','verified','deleting','quarantined','failed')`, using the declared size where the actual is unknown. A `failed` row whose late PUT object still exists is physically stored and therefore charged until the object is destroyed (round-4 finding 29). Nothing is charged twice: one row per `(pool,digest)` holds one generation's worth of bytes.
- **Projected frees** = `SUM(size) WHERE state='deleting'` — read straight from durable state, no `pending_free` table (it duplicated exactly this), so the controller survives a leader restart by construction.
- **No "new references cancel the projection" rule.** After a mark, the blob is no longer `verified`, so publish refuses it; cancellation is impossible and unnecessary (round-4 finding 29).
- The controller stops at `charged − reclaim_pending ≤ low_watermark` and does not evict again while a delete is in flight.
- Nothing "frees bytes immediately": quota is released when the object delete is confirmed and the row returns to `absent` (§7 of `ledger.md`).

### 3.4 Regret cohort

Both counters are incremented **once per unique item**: `dcache_gc_evicted_total` on the `verified→deleting` transition, `dcache_evicted_refetch_total` when a reserve allocates a new generation for a digest evicted less than 24 h earlier, joined on `(pool, digest, evicted_gen)`. A refetch twice counts once. Because every refetch is a subset of the evicted set, `increase(…_refetch_total[24h]) / increase(…_evicted_total[24h])` is ≤ 1 by construction; R3's form divided two independent `rate` windows and could exceed 1 after an eviction burst (round-4 finding 27).

### 3.5 Admission — one Boolean rule

```
admit = pinned
     OR (frequency(key) ≥ 2 sessions in 7 d AND donor_cost_known AND donor_cost ≥ 3 × serve_cost)
```

- **AND**, not OR, between frequency and cost: a twice-requested 1 s result costing 20 s to serve is rejected (round-3 finding 21).
- Unknown donor cost ⇒ not admitted unless pinned. Unknown `read_throughput` ⇒ `serve_cost` uses the conservative floor from the last burst test and is flagged `estimate`; until a production burst exists, `serve_cost` is `UNKNOWN` and only the frequency gate applies with a **warning** on the dashboard.
- The decision is made **before** the reservation and before the single upload continuation.
- Rejected entries keep metadata only; the first session seeds nothing unless pinned.

## 4. Config sketch

```yaml
dcache:
  cache:
    half_life: 168h
    retention_edge: 168h
    idle_prune_after: 720h
    admission: { window: 168h, min_sessions: 2, cost_factor: 3 }
  storage:
    quota_per_pool: 3TiB
    high_watermark: 0.85
    low_watermark: 0.70
    hard_watermark: 0.95
    max_candidates: 2000
  gc:
    sweep_interval: 1h
    url_grace: 7200s
    delete_recovery_after: 900s
    start_mode: dry_run
  checkpoint:
    on_change_min_interval: 10m
    keep: 5
  slo:
    publish_p95_seconds: 90          # target, not measured
    renew_p99_seconds: 0.2           # SLO target
```

## 5. Dashboards

1. Value: gross vs net saved, net lost, remaining recompute, unknown-cost count.
2. Population: classes, incomplete sessions, `attribution_uncertain`.
3. Transfer: publish p50/p95, install p95, deferred publish chains.
4. Storage: charged bytes, reclaim pending, live blob bytes vs charged, GC scan seconds, regret.
5. Durability: ckpt bytes/seconds/seq, epoch, restore duration.
6. Engine path: store errors, install p95, computed-with-URL, renew unavailable, probe liveness — five separate panels, never merged.

## 6. Not measured

No per-digest labels; no byte-hit ratio; no absolute hit-ratio alert.
