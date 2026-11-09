> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

PROCEED-WITH-CHANGES

# Round-5 review of R4 (Claude)

**Verdict:** the structural change holds. `M` together with `LiveBlobs()` and a state read before `SetStoredPart` closes all four R3 symptoms, and I could not break publish against mark. **No P0 remains.** Seven P1s remain. Each is a local specification defect: a missing predicate, a missing gate, a wrong unit or an unsound timing bound. None needs another structural change. The design can go to implementation once the P1 text changes are made and the items in §12 are measured.

Counts: **0 P0 / 7 P1 / 22 P2.**

---

## 1. The four R3 symptoms (Q1)

| Symptom | Status | Clause |
|---|---|---|
| Rolled-back or unknown COMMIT around publish | **Closed.** Publish has no DB write (`ledger.md:152-161`). Residuals are F15 (mark commit) and F14 (verify commit). | §5.1 |
| Cross-resource deadlock (DB row lock ↔ `egraphMu`) | **Closed.** No Postgres lock is held across the graph write (`ledger.md:162`). New lock-order residuals are F12 and F13. | §0, §5.1 |
| Mint starvation | **Closed.** Mint is one autocommit statement per digest with `lock_timeout` (`ledger.md:177-185`). Nothing holds a lock on a `verified` row for long. New hazard: a timeout or a fence failure is read as "blob gone" (F1). | §6 |
| `graph_refs` cannot reach zero | **Closed.** Liveness is recomputed (`ledger.md:189-203`, `:268`). | §7 |

Interleavings I tried that fail to break the model:
- **Publish vs mark.** Both take `M`. Mark moves the row to `deleting` before it unlocks, so a later publish reads `deleting` and refuses.
- **Mint vs mark.** Postgres re-evaluates the row: a mark that lands first makes mint's `state='verified'` fail; a mint that lands first makes mark's stamp test fail.
- **Reuse (§3.1) vs mark.** The stamp is taken under `FOR UPDATE`, so the digest cannot be marked for G.
- **ABA across generations.** The graph never stores `gen`, so a republished digest at gen+1 is served correctly.

---

## Findings

### P1

**F1 — P1 — A zero rowcount from mint conflates "blob gone" with "not leader" or a timeout, and either one triggers `DropStoredParts`. A leader-lease blip can wipe the graph.**
`ledger.md:181-185`, `:86-88`, `:218-226`; `architecture.md:233`, `:171`.
- Every mint carries the epoch predicate `l.lease_until > now()`. If the leader's lease renewal is late by one second (Postgres failover, GC pause), every mint in that window returns 0 rows, which becomes `renew_unavailable`.
- §8 and `architecture.md:233` then call `DropStoredParts(digest)` on renew-miss. That drops every part naming that digest.
- A shared base layer is named by most parts, so one blip drops most of the pool's graph.
- `lock_timeout` 500 ms produces an error rather than a zero rowcount. What happens on that error is unspecified; mapping it to the same miss path has the same effect.
- **Fix:** drop only after a positive read shows `state <> 'verified'` while the leader lease is valid. Fence failure, timeout or error must reply `unavailable` **without** a drop.

**F2 — P1 — Eviction overshoots without bound, because the projection lags by G.**
`metrics-and-eviction.md:183`, `:188-190`; `ledger.md:196`, `:226`, `:262`.
- Eviction drops parts. Their bytes only reach `deleting` at the first hourly mark after `last_url_mint_at < now()-G`, which is 2–3 h later.
- The controller's stop rule is `charged − reclaim_pending`, where `reclaim_pending` counts only `state='deleting'`. For those 2–3 h the dropped bytes appear in neither term.
- "Does not evict again while a delete is in flight" never fires, because nothing is in flight during G.
- Each controller cycle therefore sees the pool still above 0.85 and drops more, potentially down to only hot parts.
- The "10 % overshoot cap" is computed on frees that have not happened yet.
- **Fix:** projected frees = `deleting` + Σ size of (`verified` ∧ not in the last `LiveBlobs()` snapshot). Do not run another eviction pass until the next mark has absorbed the previous one.

**F3 — P1 — "Per-part" eviction cannot be implemented with a per-digest drop, and the score ignores collateral.**
`metrics-and-eviction.md:179`, `:183`; `ledger.md:220-227`; `architecture.md:174`.
- The only drop primitive is `DropStoredParts(digest)` (RP:393). There is no way to drop "one part".
- Dropping a cold part P = [B, X] means dropping X, which also drops every part whose chain contains X. That includes hot children such as [B, X, Y].
- §3.2 scores P alone and then executes a drop of `parts(X)`.
- **Fix:** make the candidate unit a digest d. Lost benefit = Σ benefit over `parts(d)`. Freed bytes = blobs referenced only by `parts(d)`.
- This needs a part→chain enumeration. Only `StoredState` (part addresses) and `LiveBlobs` (digests) are grounded, so name the API or mark it **UNKNOWN**.
- Recomputing freed bytes "after each drop" via `LiveBlobs()` costs O(graph) per drop × 2,000 candidates, all under `M` (see F11).
- `architecture.md:174` still describes reclaim groups, which contradicts `metrics:183`.

**F4 — P1 — `part_cost(pool, part_digest)` is keyed on an attribute that does not exist.**
`metrics-and-eviction.md:23`, `:73-79`; `ledger.md:54-59`.
- `PartAddress` is `{outputPath?, part:"fs"|"snapshot"|...}` (RP §6.2, `:269-291`). It is a structural path inside an entry, not a content identity.
- `cache.offer.part` and `cache.parts` carry that address, so `{"part":"fs"}` is the same string for millions of entries. There is no "part digest" on any span.
- **Fix:** key the table on `sha256(ordered chain layer digests)`, which the service knows at export.
- Then specify both joins:
  - **Producer side:** the export bundle's ordinal → engine result number → lazy-evaluation span `result.id`.
  - **Install side:** `(cache_id, result.id)` of the receiving entry → the merge/offer mapping the service sent to that engine → the chain.
- Without those joins, `saved_*`, admission's `donor_cost` and eviction `benefit` cannot be computed.
- The same missing join affects `last_install_at` per root (`metrics:171`) and `computed_with_url_total` (`metrics:137`): "held a minted URL for one of their digests" needs a map from entry to digest per engine.

**F5 — P1 — The delete path's timing bound is unsound, and no normative sweep catches the orphans it leaves.**
`ledger.md:206-210`; `architecture.md:132`, `:146`, `:173`.
- A late PUT finishes at most at (PUT URL expiry, 5 min) + transfer time.
- The engine's PUT has only a **1-minute idle** bound (`architecture.md:146`). A progressing upload has no total bound.
- A 2 GB layer at under ~1.9 MB/s (slow link, or 8-way contention on a weak NIC) takes more than 20 min, so it lands after the second delete. At the 7.76 MB/s per-stream contention figure it takes 258 s, which is inside the window.
- A PUT that finishes at 40 min leaves a permanent orphan: uncharged, invisible to quota, never reclaimed.
- `architecture.md:173` mentions a weekly LIST reconcile, but the ledger does not define it and gives it no action rule.
- **Fix — the sound and cheap sweep.** Generations are monotonic and only the takeover path reuses a key, so for every listed key `…/<hex>/g<n>`:
  - `n < row.gen` → garbage, delete now;
  - `n = row.gen` with state in `absent` or `failed` and `LastModified` older than PUT TTL + 1 h → delete;
  - no row → delete.
- Cost: about 1 LIST per 1,000 keys. At 1 M objects that is about $0.005 per run; daily is fine.
- With this sweep, the 2-attempts rule becomes an optimisation, not the correctness mechanism.
- Related: §3.2's synchronous `DestroyBlob` (`ledger.md:106-108`) never enqueues the second attempt. A lease-expired owner still uploading to the `failed` generation is therefore not covered even by the 20-min rule.

**F6 — P1 — The reserve, verify and expiry SQL does not implement the transition table.**
`ledger.md:66-77`, `:110-118`, `:139-146`, `:207`, `:253`.
- **Verify is not owner-fenced.** The guard is `actor_epoch=$me`, which is the *leader* epoch, and both the old and new owner run under the same leader. `owner_export` is never compared.
- `:146` says "a verify whose lease expired is refused", but the SQL has no `lease_until` predicate.
- **`pending→failed` on lease expiry has no SQL.** The table guard is `gen=$g` only, so a sweeper can fail a row that a takeover (`:113-116`) just re-leased while the new owner is uploading. That destroys the object under the new owner and causes upload churn.
- The table also lists lease expiry as both `→failed` and `→takeover` with no precedence.
- **`failed→absent` and `quarantined→deleting` have no SQL.** §7 step 2 only matches `state='deleting'`, so the queue drain leaves `failed` and `quarantined` rows in place permanently.
- **§11 "pending older than 1 h"** names no column (there is no `reserved_at`). It also only handles rows "whose object exists", so a `pending` row with no object and no further demand is never failed.
- **Fix:** add `owner_export=$export AND lease_until >= now()` to verify. Write `pending→failed` as `… AND lease_until < now() AND owner_export = $seen_owner`. Write explicit SQL for every row of the table. Add `reserved_at`.

**F7 — P1 — Restore and GC have no ordering gate, so a mark against an empty or partially loaded graph marks the whole cold pool.**
`ledger.md:241-246`, `:250`; `metrics:225-228`.
- Liveness comes from the in-process graph. If the hourly or startup mark runs before §10 step 3, it can see an empty or partially loaded graph, a failed checkpoint download, or a fall-back to an older checkpoint.
- In that case `LiveBlobs()` returns ∅ or a subset, and every `verified` blob not minted for 2 h is marked and deleted.
- §11 runs "at startup", and nothing in the spec says the mark waits for restore.
- **Fix:** disable mark until restore step 3 has completed under `M` in this epoch. Refuse to mark if `|live|` is below a fraction of the checkpoint's recorded `live_blobs` count (alert instead).
- Answer to Q5: reserve and verify are safe before restore completes. Merge, renew, publish and mark must wait.

### P2

**F8 — P2 — The two-phase GC fallback is incoherent as written, and its natural reading reopens a race.**
`ledger.md:214`, `:279`.
- "Snapshot under `M`, mark under `M` after re-snapshot" holds `M` for two scans, not fewer.
- Taking the snapshot outside `M` races with any publish whose digest stamp is older than G. That is reachable through `waiting_chains` (§5.2), which have no time bound.
- **Fix:** snapshot S outside `M`. Under `M`, publish adds its digests to `published_since(S)`. Mark under `M` removes those digests from its candidates. That costs O(publishes), not O(graph).

**F9 — P2 — The bounded hold time of `M` does not follow from `LiveBlobs()` scan time.**
`ledger.md:214`; `architecture.md:226`, `:100`; RP:380.
- `CommitValueMerge` takes the egraph write lock outside `M` and "can exceed 5 s". Checkpointing may also take it.
- Go's `RWMutex` is writer-preferring, so `LiveBlobs()`'s `RLock` under `M` waits behind any pending writer.
- The time `M` is held is therefore at least the longest write-lock hold anywhere, not the scan time. The "1 s gate" measures the wrong quantity.
- No wedge exists unless something under `egraphMu` takes `M` (F12).
- Measure `M` hold time directly.

**F10 — P2 — Drops on the renew path run under `M`, which breaks the 2 s renew deadline.**
`ledger.md:225`; `architecture.md:233`.
- `DropStoredParts` runs under `M`, so it queues behind a GC scan (F9).
- **Fix:** send the reply first, then do the drop asynchronously under `M`.

**F11 — P2 — Eviction's per-drop `LiveBlobs()` multiplies the time `M` is held.**
`metrics:183`.
- 2,000 candidates × an O(graph) scan.
- Compute freed bytes incrementally from a digest→parts index built once per pass.

**F12 — P2 — The lock order `M` → `egraphMu` is never stated.**
`ledger.md:162`.
- §0 forbids Postgres locks under `egraphMu`, but not taking `M`.
- Any dagql callback (prune or release hooks) that calls into service code taking `M` deadlocks against a GC holding `M` and waiting for `RLock`.
- State: "nothing called with `egraphMu` held may take `M`".

**F13 — P2 — Store I/O runs inside a transaction holding a row lock.**
`ledger.md:105-112`.
- §3.2 `DestroyBlob` and §3.3 HEAD run under `FOR UPDATE`.
- A hanging store, which F15 in metrics calls undetectable, holds the row lock and blocks every concurrent reserver for that digest.
- This is the same class of problem R4 removed from publish.
- Do the I/O outside the transaction, then re-lock and re-check `gen` and `state`.

**F14 — P2 — Publish can fire before the verify commit is visible, and ambiguous verify commits are not handled.**
`ledger.md:169-171`, `:146`.
- §3.3's dead-owner branch verifies inside the reserve transaction. If it triggers publish synchronously, publish reads `pending` from another connection and drops the chain.
- An unknown verify commit leaves `waiting_chains` unwoken.
- The publish trigger must run after COMMIT. On an ambiguous commit, re-read state.

**F15 — P2 — An ambiguous commit of the mark transaction can still interleave with publish.**
`ledger.md:189-203`.
- If the client times out while the server is still committing and `M` is released, a publish can read `verified`, call `SetStoredPart`, and then the mark commits on a now-live digest.
- Hold `M` until a re-read confirms the outcome.

**F16 — P2 — `waiting_chains` has no bound or TTL, and nothing removes entries whose partner reached `deleting` or `quarantined`.**
`ledger.md:170-173`.
- Expire an entry when every partner row is in a terminal state, or after G.

**F17 — P2 — Takeover neither stamps nor HEADs.**
`ledger.md:113-118`.
- Takeover issues a PUT URL without bumping `last_url_mint_at`, so G does not cover that URL (see F5).
- Takeover re-uploads up to 2 GB even when the dead owner's object is already complete. HEAD first, as the dead-owner branch does.

**F18 — P2 — A deposed leader can still publish and serve.**
`ledger.md:152-158`; `architecture.md:54`.
- Publish is a SELECT followed by an in-memory write with no epoch, so I6's "a deposed leader cannot publish" is false.
- It is harmless, because the checkpoint pointer is fenced and mint is fenced, but the text claims otherwise.
- Check the local lease in publish, or correct I6.

**F19 — P2 — Quota charges `quarantined` bytes permanently after the object is destroyed.**
`ledger.md:236`, `:260`.
- §9 queues the object immediately, but §12 charges `quarantined` regardless of whether the object still exists.
- Charge `quarantined` only while its queue row exists.

**F20 — P2 — The metrics definitions of `recompute_remaining` disagree.**
- `metrics:60` includes the residual of mixed entries.
- `metrics:85` and `architecture.md:260` say "entries with no install".
- `metrics:87` excludes `would_hit` and `lost_keying`, which `:60` does not.
- That is one series name with three definitions. Q6's "residual reaches the denominator" is true only under `:60`.

**F21 — P2 — The regret join needs columns the schema lacks.**
`metrics:195`.
- The regret cohort joins on `evicted_gen` and an eviction time.
- `blob` has neither; the row is reused at gen+1.
- Add an `evicted_at` and `evicted_gen` log, or columns.

**F22 — P2 — Unknown-cost parts starve admission.**
`metrics:182`, `:262`.
- Unknown-cost parts are evicted only at 0.95, the same watermark that stops admission.
- If unknown-cost bytes dominate (incomplete sessions give unknown cost), the pool parks at 0.95 with admission off.
- Evict unknown-cost parts by age above 0.85.

**F23 — P2 — Stale clauses (Q8).**
- `architecture.md:172` — the `last_ref_at` fence (R2 residue). Also "lease expires → failed → next sweep deletes", which contradicts `ledger.md:112` and `latency-and-azure.md:47` (HEAD immediately on WS close).
- `architecture.md:240`, `decision.md:42` — the same 10-minute-lease story.
- `architecture.md:251` — "demand p95" (retired term).
- `architecture.md:99` — "one committed Postgres stamp (`ledger.md` §7)". The mint is §6, and it is one statement *per digest*.
- `architecture.md:161` — "INSERT … ON CONFLICT DO NOTHING puts a `pending` row at a new generation". The INSERT creates `absent`; allocation is the §3.5 UPDATE.
- `architecture.md:97`, `:160` — "verified blob at head". The head mirror is gone.
- `architecture.md:174` — reclaim groups (see F3).
- `architecture.md:164`, `:231` — "MinIO bandwidth" and "range GET to MinIO" in the generic or production flow.
- `architecture.md:41` vs `:138` vs `ledger.md:244` — three different checkpoint key formats.
- `architecture.md:173` — weekly LIST reconcile, not in the ledger (see F5).
- `architecture.md:226` — "publication waits for the commit" (`CommitValueMerge`). This precondition is missing from `ledger.md` §5.
- `decision.md:36` — "Mark-only startup writes candidates without changing state". This is undefined in the ledger: there is no table for it, and `start_mode: dry_run` at `metrics:228` is the only hint.
- `ledger.md:231` — "§5.1 sample" points to ledger §5.1 (publish); the sample is defined at `architecture.md` §5.1.
- Clean: `graph_refs`, `blob_head`, `pending_free`, `deleted` and `pin` appear only as negations. "Every blob hashed" appears only as a disclaimer, and the 30 % gate as withdrawn. `miss_reasons` and `cache_span_count` are absent.

**F24 — P2 — The probe arithmetic is inconsistent.**
`latency-and-azure.md:17`.
- The probe computes e2e as body + response = 36.0 + 19.3 = **55.3 ms**, which gives 72.3 MiB/s.
- The table prints **57.7 ms**, which would give 69.3 MiB/s.
- The rate is right and the duration is wrong.

**F25 — P2 — The lease length is not tied to measured upload time.**
- With a 10-minute lease and no progress-based renewal, a contended 2 GB export near or above 10 min hits takeover or failure mid-upload, which causes double PUTs and late PUTs.
- Size the lease from the measured publication spike.

**F26 — P2 — Restore drop by digest is acceptable (Q5).**
- At restore, every part naming a non-`verified` digest must go, so the coarseness costs nothing there.
- The coarseness only costs at eviction (F3) and at renew-miss (F1).

**F27 — P2 — The `$live` array parameter will be large.**
- At 50k+ digests per hourly mark, `ANY($live)` becomes a large array.
- Use a temp table plus an anti-join and measure. This is not a correctness issue.

**F28 — P2 — §11 "`failed` rows with a queued object → destroy" is redundant with the queue drain, and the drain cannot transition `failed` rows (F6).**

**F29 — P2 — The breaker trips on a 10-minute window of provider metrics that arrive minutes late.**
- The trip is effectively 15–20 min after onset.
- This is stated honestly, but the breaker table should carry the effective delay.

---

## Q-by-Q pointers

- **Q2:** Publish against mark is sound. The time `M` is held is not bounded by scan time (F9), drops on the renew path wait on `M` (F10), the fallback is wrong (F8), and the lock order is unstated (F12).
- **Q3:**
  - `absent` → allocate gen+1. Correct.
  - `failed` → synchronous destroy, then allocate. Sound for bytes, but there is no second attempt (F5), and the store I/O runs under the row lock (F13).
  - `pending` with a live owner → decline, publish on the verify event. Correct.
  - `pending` with a dead owner → HEAD, then verify or fail. Correct, apart from F13 and F14.
  - `pending` with an expired lease → takeover. No stamp or HEAD (F17), and the verify and expiry guards are missing (F6).
  - `verified` → reuse and stamp. Correct.
  - `deleting` or `quarantined` → decline. Correct; the quota residue is F19.
  - The SQL ≠ the table (F6).
- **Q4:** Not enough. A 40-minute PUT leaves a permanent orphan. A listing sweep keyed on monotonic generations is sound and costs about $0.005 per 1 M objects (F5).
- **Q5:** The coarseness is free at restore. Reserve and verify are safe before restore completes; merge, renew, publish and mark must wait (F7).
- **Q6:**
  - The evaluation filter is computable: a lazy-evaluation span carries `result.id` and no `outcome` (RP `attrs.go:738-742`).
  - Stopping the ancestor walk at a different `result.id` is computable.
  - `part_cost` has no key (F4). The residual has three definitions (F20).
- **Q7:** Overshoot (F2), collateral (F3), starvation (F22). A cold sharer of a hot base frees its unique digests correctly once F3's unit is used.

## 12. Measure before implementation

1. `M` hold time per GC, including the wait for `RLock` behind `CommitValueMerge` and the checkpoint write lock, on a real-size pool. Also `LiveBlobs()` scan time alone.
2. The longest egraph write-lock hold (`CommitValueMerge` p99, checkpoint).
3. Publication spike: 30 MB, 500 MB and 2 GB under 8-way contention, measured from span end to `SetStoredPart`. Use it to size the lease (F25) and to check late-PUT tails (F5).
4. Upload-time distribution on the slowest production runner link. This decides whether a listing sweep is required (my answer: yes, regardless).
5. Leader-lease renewal jitter under Postgres failover, i.e. how often the F1 window opens.
6. Join coverage for F4: the fraction of install spans that map to a chain via the merge or offer mapping.
7. Everything still open in `revision-r4.md:85-92` (Gate A discovery, install-attribute contract on beta.25, memory spike, 100 × 2 GB read burst, store capabilities, MinIO supply chain).
