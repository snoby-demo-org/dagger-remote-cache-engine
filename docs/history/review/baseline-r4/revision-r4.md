> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Revision R4 — change ledger

Date 2026-11-07. Round 4 reviewed R3 with both models, in parallel, independently:
- Claude Opus: `review/claude-r4.md` — STOP, 31 findings (1 P0 / 16 P1 / 14 P2).
- ChatGPT model: `review/gpt-r4.md` — STOP, 23 findings (1 P0 / 18 P1 / 4 P2).

Both P0s are the same defect seen from two sides: **R3's publish held Postgres row locks across `SetStoredPart`**, so the graph write was outside the database transaction. That is one structural defect with four symptoms, not four patchable bugs. Both reviewers independently recommended cutting it. I verified the recommendation against the dagql API and adopted it.

## Adopted: the structural change (Claude Q8, GPT "structural simplification")

**Cut Postgres as the publish/GC serializer. Cut `graph_refs`, the head-lock-across-`SetStoredPart`, and the `blob_head` mirror. Take liveness from `LiveBlobs()`.**

Grounds, checked in source:
- `dagql.Cache.LiveBlobs()` — `dagql/cache_stored_queries.go:162` — returns every blob a stored part names, under `egraphMu.RLock`. It exists and is cheap enough to run hourly.
- `dagql.Cache.DropStoredParts(blob)` — `dagql/cache_stored_parts.go:167` — drops **every** part whose chain names that digest. It is digest-granular by construction, so a per-part generation identity cannot be enforced from the service side; `graph_refs` was unsatisfiable (Claude F1, GPT F2/F3).
- The service already hosts the only graph (I6: one fenced writer per pool), so the graph is in-process and a mutex is a real serializer.

New model (`ledger.md` §0–§13):
- Publish = `lock M; re-read all distinct digests' states; require verified; SetStoredPart; unlock`. **No database transaction**, no DB lock held across the graph write, nothing to roll back, no orphan if the process dies (a part that was never checkpointed does not exist on restart, so the object is simply reclaimed after G).
- GC mark = `lock M; live := LiveBlobs(); epoch-fenced UPDATE … state='deleting' + queue row; unlock; DELETE g<gen> outside M`. Liveness is **recomputed, never counted**.
- Reserve branches on state under one row lock; `absent`/`failed` allocate gen+1, `verified` reuses, `pending` takes over only if the lease expired or the owner's export is dead, `deleting`/`quarantined` decline.
- Every ledger write carries an epoch predicate; `delete_epoch`-stamp-then-compare is gone.
- `failed → pending` requires the old object destroyed first (synchronously), and every destroyed generation is queued with **two delete attempts 20 minutes apart** to reclaim a late PUT that lands after the first delete.
- Publication is **event-driven**: a chain whose partner digest is `pending` waits for that digest's verify event, with no blocking waiter anywhere in the merge path (removes the whole 4 s-waiter class).
- Restore = drop parts whose digest is not `verified` at head. No accounting reconciliation, so failover can no longer quarantine the newest, hottest blobs.
- Quota = charged by state (including `failed` and `quarantined`), projection read straight from `state='deleting'`, no `pending_free` table.

## Round-4 dispositions

### Structural (both reviewers)

| # | Sev | Finding | Fix |
|---|---|---|---|
| C1 / G2,3 | P0/P1 | `graph_refs` incremented on publish, never decremented on any real removal path; GC could never delete a once-published blob | `graph_refs` **deleted**. Liveness from `LiveBlobs()` at mark time (`ledger.md` §7). |
| C2 / G1 | P1/P0 | `SetStoredPart` outside the DB transaction: failed/unknown COMMIT leaves an unreferenced part, no compensation | No DB transaction around publish at all (`ledger.md` §5.1). |
| C3 | P1 | DB row locks held while waiting on the dagql lock: undetectable deadlock, renew starvation, leader-lease cascade | `M` is in-process only; nothing holding `egraphMu` ever takes a Postgres lock (`ledger.md` §0/§5.1). |
| C4 / G7 | P1 | Mint locks rows in plan order, deadlocks with digest-ordered publish | Mint = one autocommit single-row statement per digest, sorted in Go, `lock_timeout` 500 ms (`ledger.md` §6). |
| C5 / G5 | P1 | Head-mirror update not conditioned on the primary's rowcount; verify/lease race creates two generations | `blob_head` mirror **deleted**. |
| C6 / G4 | P1 | Reserve accepts `failed`/`deleted` without their guards; non-head `failed` row can never finish cleanup | Explicit synchronous destroy before `failed→pending`; `absent`/`failed` are the only allocatable states (§3). |
| C7 / G3 | P1 | Restore quarantines every post-checkpoint publish and deletes it immediately, bypassing G | Restore drops parts by **digest state**, no accounting reconciliation (§10). |
| C8 | P1 | Recovery scanner stamps `deleted` while parts remain, opening a second generation | Scanner always drains before the transition; states have no `deleted` (§11). |
| C9 / G18 | P1 | Union-find closure makes reclaim groups whole connected components; cold sharers unevictable; split rule contradicts closure | **Any set of parts is a candidate**; selection is per part, lowest score first, freed bytes recomputed after each drop (`metrics` §3.2). |
| C31 / G6 | P2/P1 | `decision.md` says `rowcount == |chain|` where the ledger needs distinct digests; leadership unfenced | Distinct digests; epoch predicate on every write. |

### Metrics

| # | Sev | Finding | Fix |
|---|---|---|---|
| C10 / G13 | P1 | Donor cost stored per blob generation at reserve: wrong granularity, wrong moment, missing on reuse | `part_cost(pool, part_digest)`, written at session finalisation, median over producers, **never summed across a chain's layers**, labelled an estimate; mixed entries keep their residual in `recompute_remaining`. |
| C11 / G12 | P1 | Interval union includes nested entries' work and is summed across entries | Ancestor walk stops at the nearest span with a different `result.id`; evaluation-only filter; uncertain attribution never credited; reported per entry, never summed as independent work. |
| C12 | P1 | PUT "body rate" measures socket-buffer fill, not transfer; real rate ≈72.9 MB/s | Probe prints end-to-end rate; re-measured: **75.9 MB/s**, 2 GB ≈ **26.4 s**; "18 s" withdrawn. |
| C13 | P1 | Aggregate scenario divides 2 GB by the whole 8-writer aggregate | Per-stream under contention ≈ 7.76 MB/s ⇒ ≈ **258 s**; stated as the real production risk. |
| C14 / G9 | P1 | Dead-donor publication still not executable; lease expiry deletes the object | On WS close / `EndRemoteProcess` / upload timeout: HEAD every `pending` key immediately, verify what exists, fail the rest. |
| C15 / G10 | P1 | Breaker cannot see a hang: install p95 samples only successes, provider logs lag, no MinIO exporter in production | Provider source named (CloudWatch / Azure `ServerBusy`), delay stated; service-side proxies (`renew_unavailable`, `computed_with_url`, `deferred_publish_chains`); client-side hang declared **undetectable**. |
| C18 / G15 | P2/P1 | `miss_reasons` ungrounded; `demand_p95` mislabelled | `miss_reasons` removed; `dcache_computed_with_url_total` replaces `demand_p95` and claims no wait time. |
| C27 / G11 | P2/P1 | Nonexistent `cache.cache_span_count`; revisions in the wrong namespace; undefined series; regret not a cohort | `dagger.io/cache.session.spans` on the carrier span, compared with cache-result spans only; `dagger.io/cache.replacements` for revisions; missing completion carrier ⇒ `unknown`, not complete; series defined; regret = closed cohort counters. |
| C28 / G14 | P2/P1 | Ceiling double-counts `would_hit`/`lost_keying` and mixes net with gross | `ceiling = saved_gross / (saved_gross + remaining)`, opportunity sets reported separately. |
| C29 / G17 | P2/P1 | Quota residue: `pending_free` duplicate, impossible cancellation, uncharged `failed`/`quarantined` bytes | Charged by state incl. `failed`/`quarantined`; projection = `state='deleting'`; cancellation rule deleted. |
| G8 | P1 | Late PUT recreates a cleaned-up generation after the scanner stops watching | Two delete attempts 20 min apart per generation (`ledger.md` §7). |
| G23 | P2 | Known-dead owner suppresses a digest for the full lease | Dead-export detection triggers immediate HEAD/verify/fail (§3.3). |

### Surviving clauses (grep sweep)

| # | Finding | Fix |
|---|---|---|
| C16 / G20 | `architecture.md` still says "pinned (`ledger.md` §4)" and "publish under a durable pin" | Both rewritten to the `M`/state-read model. |
| C17 | R1 SLI formulas verbatim at `architecture.md:260` | Rewritten to the normative metric set; R1's duration/self-time weighting explicitly retired. |
| C19 / G20 | Pre-generation keys in architecture (`:40`, `:137`, `:154`, `:161`, `:162`, `:174`) | `/g<gen>` added; idempotency restated per generation; "cancel projected frees" removed. |
| C21–C24 | Deadlock/threshold residue, MinIO-as-production, probe-as-breaker, FQDN NetworkPolicy | All rewritten; R7 mitigation now CoreDNS/Cilium, not NetworkPolicy. |
| C25 | Wrong-bytes behaviour: "fails the build" vs "falls back to compute" | Both sites say fall back to compute (RP §7.2); unsampled corruption declared undetectable. |
| C26 / G21 | Cost total never recomputed | $0.232/day ⇒ **≈$7/month**. |
| G19 | Architecture metric/Phase-0 procedures incompatible after the rewrite | §7, §9/§10 and §13(0) aligned to Gate A/B and the normative SLIs. |

## Evidence produced this round

- **Fresh probe run** (`s3probe4`, all assertions passed): GET 4 MiB TTFB p50 4.48 / p95 5.91 / p99 12.51 ms, total 43.4 ms (92.2 MiB/s); range p50 3.50 / p99 9.21; HEAD p50 2.10 / p99 7.56; PUT body 36.0 ms (111.2 MiB/s), response 19.3 ms, **end-to-end 72.4 MiB/s = 75.9 MB/s**; 8× concurrent PUT 59.2 MiB/s aggregate; expired capability 403.
- The R3 headline (109.5 MB/s, 18 s) was **buffer-fill, not transfer**. Both reviewers caught it independently; the correction is applied to the doc and the probe now prints both numbers.

## What genuinely closed

Both reviewers agree these hold and could not be broken: generation-scoped keys defeating delayed-DELETE ABA; the NULL-mint immortality fix; delete-recovery idempotency; the disjoint `would_hit`/`lost_keying` opportunity sets; single-continuation upload; correlated mint join; retention-on-install-only; AND-admission; net/gross arithmetic bounds.

## Still open — unchanged or new

1. F5 discovery capture / Gate A.
2. Install-attribute contract check on beta.25.
3. Publication spike (30 MB / 500 MB / 2 GB, span end → published, compression included).
4. Fresh-engine memory spike, then memory limits.
5. Production read burst (100 × 2 GB) — the missing `read_throughput`.
6. Store capabilities: `If-None-Match: *`, exact 206, DELETE of missing key, single-PUT cap; Azure PUT shape.
7. MinIO supply chain.
8. **NEW:** `LiveBlobs()` scan time on a real pool size, and whether GC's two-phase mark is needed; the mutex means publishes wait for the scan.

## Loop status

Round 5 runs both reviewers over R4. Loop stops when **both** return no P0/P1, or a round produces no new P1+ against changed text. Four-round cap was already reached at round 4; this is round 5 because round 4 changed structure rather than text — I am extending by one round and will say so plainly if it does not converge rather than call it clean.
