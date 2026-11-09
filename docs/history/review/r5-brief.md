> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Round-5 review brief — Revision R4 (structural change)

Round 5. Round 4 returned STOP from both reviewers with the *same* root defect, and both recommended the same structural fix. R4 adopts it. This round exists to decide whether the simplified model holds, or whether the design needs one more structural change rather than another patch pass.

## What changed in R4, and why

R3 published by holding **Postgres head row locks** across `dagql.SetStoredPart`. That put the graph write outside the database transaction (Claude F2 / GPT F1), deadlocked against the dagql write lock invisibly (Claude F3), starved mint (Claude F4), and required `graph_refs`, which had **no observable decrement path**: `DropStoredParts(digest)` (`dagql/cache_stored_parts.go:167`) drops every part naming a digest, is digest-granular by construction, and retention pruning is invisible to the service. `graph_refs` could therefore never reach zero for a published blob (Claude F1 / GPT F2/F3), and restore "repaired" the drift by quarantining the newest blobs.

R4 adopts the recommendation both reviewers made:

**Cut Postgres as the publish/GC serializer. Cut `graph_refs`, the head-lock-across-`SetStoredPart`, and the `blob_head` mirror. Serialize publish, GC mark and restore in-process under one per-pool mutex `M` in the single fenced writer, and take liveness from `LiveBlobs()`.**

Verified in source before adopting: `LiveBlobs()` exists (`dagql/cache_stored_queries.go:162`, `egraphMu.RLock`); `DropStoredParts(digest)` is coarse by construction; the service hosts the only graph (I6).

## The R4 model in one paragraph

Publish = `lock M; re-read all distinct chain digests; require verified; SetStoredPart; unlock` — no DB transaction, no DB lock held across the graph write. GC mark = `lock M; live := LiveBlobs(); epoch-fenced UPDATE to 'deleting' + queue row; unlock; DELETE g<gen> outside M`. Liveness is recomputed, never counted. Every ledger write carries an epoch predicate; a stale writer changes nothing. Reserve branches on state under one row lock (`absent`/`failed` allocate gen+1, `failed` only after its object is destroyed; `verified` reuses; `pending` takes over only on lease expiry or a dead owner's export; `deleting`/`quarantined` decline). Publication is event-driven on partner verify; no blocking waiter exists. Restore drops parts whose digest is not verified at head. Deletes get two attempts 20 minutes apart to catch a late PUT at the old generation key.

## Files

`revision-r4.md` (finding→change ledger), `ledger.md` (R4, normative), `architecture.md`, `metrics-and-eviction.md`, `latency-and-azure.md`, `decision.md`, `s3_latency_probe.py`, `review/r4.diff` (cumulative vs `review/baseline-r2/`), `review/claude-r4.md`, `review/gpt-r4.md`, `review/baseline-r4/`.

## Questions

1. **Does the structural change actually close the four symptoms?** For each of: rolled-back/unknown COMMIT, cross-resource deadlock, mint starvation, `graph_refs` liveness — cite the clause or state it is open. Give a concrete interleaving if you can break one.
2. **Attack `M`.** Is the publish/mark mutex sufficient? Can a publish observe a digest as verified while GC is between snapshot and delete? Can `LiveBlobs()` (RLock) run while a writer waits for `egraphMu`, and does that wedge anything? What happens if `LiveBlobs()` takes seconds on a large pool — is the two-phase fallback sufficient, or does it reopen a race?
3. **Attack reserve.** Walk every branch of §3 (`absent`, `failed`, `pending` live/dead, `verified`, `deleting`, `quarantined`) and state the outcome for a denied reserved set, an expired lease, a late PUT and a takeover. Does the SQL implement the transition table exactly?
4. **Attack the delete path.** Two attempts 20 minutes apart: is that enough for a 2 GB upload on a slow link? What if the runner PUTs after 40 minutes? Is there a listing-based sweep that would be sound, and what does it cost?
5. **Attack restore.** Drop-by-digest is coarse (drops unrelated parts sharing a digest). Is that acceptable? Is any servicing possible before restore completes?
6. **Metrics.** Are the filters (evaluation-only, stop at a different `result.id`, uncertainty never credited) computable from the cited attributes? Is `part_cost` per part, never per layer, and does the mixed-entry residual now reach the denominator? Any attribute still assumed?
7. **Eviction.** Per-part, lowest-score-first selection with freed bytes recomputed per drop: can it starve, overshoot, or fail to free a cold sharer of a hot base?
8. **Consistency.** Grep the normative docs for any clause that contradicts the R4 model (`pin`, `graph_refs`, `blob_head`, "head row lock", "every blob hashed", MinIO as production, 30 % gate, waiter, `pending_free`, `deleted` tombstone, `demand_p95`, `miss_reasons`, `cache.cache_span_count`). Report file and line.
9. **New errors** introduced by R4.
10. **If nothing P0/P1 remains, say PROCEED** and list what must be measured before implementation.

## Output contract

- Write findings to `review/claude-r5.md` or `review/gpt-r5.md`.
- First line: `PROCEED`, `PROCEED-WITH-CHANGES` or `STOP`.
- Numbered findings with P0/P1/P2 and file:line. No large quotations.
- Read-only. Do not modify design docs, probe, manifests or baselines.
- Chat reply ≤ 200 words: verdict, counts, three worst findings, path.
