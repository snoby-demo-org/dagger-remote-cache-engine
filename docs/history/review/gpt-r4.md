> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

STOP

# Round-4 adversarial review of Revision R3

**23 findings: 1 P0, 18 P1, 4 P2.** The ordinary live-transaction publish/GC race is improved. The specification still mistakes a PostgreSQL transaction for a transaction over an independently mutable dagql graph. Its new reference counter is neither idempotent nor maintained through ordinary graph removal, reserve still bypasses cleanup, and zero-row transitions can corrupt the head mirror. These are design defects, not requests for an implementation to exist.

Stop approval of the Tier-1 specification. Tier 2, discovery/contract captures, and bounded publication/memory/store-capability experiments can proceed. An honestly unrun experiment is not counted as a new finding.

## Scope, evidence, and notation

I read the brief and every requested artifact in the requested order, including the entire 2,250-line cumulative diff, all 22 GPT round-3 findings, all 26 Claude round-3 findings, and the round-2 lineage. Repeated whole-file additions in the diff were not treated as independent changes. `revision-r3.md` is a claim ledger, not proof of closure.

Paths below are relative to `<DESIGN_WORKSPACE>`. `src:` denotes the existing checkout `<HISTORICAL_SCRATCH>/dagger-src`, whose HEAD was read back as `7e9eaacc376e5c3bed3efdca9e1ba750bc8d9233`. A targeted `git diff --stat v1.0.0-beta.25 --` over the inspected graph/persistence/telemetry/content files returned no differences.

Verification used actual source reads, in-memory SQLite predicate witnesses, Python arithmetic, and an actual local delayed-header HTTP server exercising the probe's extracted `do()` function. SQLite witnesses test the written predicates, not PostgreSQL scheduling. A small Python dictionary illustrates the separate-resource rollback problem; it is explicitly not an execution of dagql. No live PostgreSQL concurrency test, cluster probe, real-engine acceptance run, or production burst was performed. The brief's fresh design-cluster measurements are accepted facts, not replaced with local-test results. Only this review file was written; no design, probe, manifest, baseline, source, or skill was changed.

## Sequential findings

### Finding 1 [P0] — Database rollback does not roll back `SetStoredPart`, and graph readers/checkpoints can observe an uncommitted publication

**References:** `ledger.md:138-162`; `revision-r3.md:14`; `architecture.md:100,104`; `decision.md:33`; `src:dagql/cache_stored_parts.go:45-89`; `src:dagql/cache_persistence_worker.go:32-56,64-85`.

The new claim at ledger line 160, that rollback means both no reference and no part, is false for the API actually selected. `SetStoredPart` mutates `res.storedParts` under `egraphMu` and releases that lock when it returns. It neither participates in the caller's PostgreSQL transaction nor supplies undo on a later database failure. PostgreSQL row locks end with their database transaction; they do not bind an unrelated Go map to its outcome. [PG-L]

Concrete surviving-process interleaving:

1. Sweeper S snapshots `LiveBlobs()` before P's insertion. Old verified D/g1 has zero references and old timestamps.
2. P locks D's rows, increments `graph_refs`, stamps its reference, and calls `SetStoredPart(P,D)` successfully.
3. The database transaction is rolled back or its backend connection is terminated before commit; the service process and graph remain alive. No compensation or mandatory pool shutdown is specified.
4. S acquires the released locks. The committed ledger still has `graph_refs=0` and the old reference timestamp. Its stale graph snapshot and the SQL guards now allow deleting D/g1, although P remains in the actual graph.

No G-long pause is needed: the fresh reference was rolled back. A rejected/ambiguous commit also cannot simply be retried, because the graph side effect may already exist. A backend killed before the call can similarly release authority while a waiting graph operation remains capable of proceeding; the API checks context before waiting for its graph lock, not a PostgreSQL publication token at insertion.

**Process-kill qualification:** killing the entire service between insertion and commit loses its in-memory part if no checkpoint captured it. I could not break that narrower case. But after `SetStoredPart` returns, a concurrent checkpointer can capture and persist/upload the part before PostgreSQL commits, because it takes the graph read lock, not the publication transaction's locks. A subsequent process kill rolls back PostgreSQL but can leave that stored part in a checkpoint. Restore may detect the count mismatch and quarantine it; that is recovery/data loss, not the claimed absence of any part. The specification must define this window rather than declare it impossible.

**Required change:** specify committed publication identity and visibility across graph mutation, serving, and checkpoint capture; handle failed/ambiguous commits and lost database authority. Either make dagql a derived, revalidated index of committed publication records or provide a serialized mutation/undo protocol whose failure closes the pool before readers, GC, or checkpoints proceed. Merely writing the call between `BEGIN` and `COMMIT` is insufficient. My round-3 row-lock suggestion also required insertion to be forbidden after losing authority; this revision omitted that load-bearing condition and rollback integration.

### Finding 2 [P1] — `graph_refs` is not an executable reference-accounting protocol, including on successful operations

**References:** `ledger.md:15-20,35,149-157,203,223-230`; `architecture.md:170-174`; `metrics-and-eviction.md:100,162-172`; `src:dagql/cache_stored_parts.go:75-89,162-191`; `src:dagql/cache_stored_prune.go:50-79,87-108`; `src:dagql/cache_current_entry.go:126`.

The only explicit increment adds one per distinct chain digest on every publication attempt. Native `SetStoredPart` replaces a map entry keyed by part address. Publishing the same part twice can therefore leave one actual part but `graph_refs=2`. It can also return `false,nil` without storing a part. The specification does not make reference increments conditional on that return value, distinguish replacement from addition, or subtract the previous chain when replacing a part. A transaction that commits after a no-op leaks a permanent positive count even without finding 1.

The normal removal path is missing too. Expiry/prune and value replacement can remove stored parts inside dagql; the design supplies no accounting hook or atomic reconciliation for those removals. `graph_refs=1` can remain after the last actual part is pruned, permanently defeating the GC `graph_refs=0` guard. Recovery only reconciles on leader restore.

Multi-digest example: P has chain `[A,B]`, so both counters are one. Quarantining A calls native `DropStoredParts(A)`, which removes the entire P, not just an A attachment. The specified A counter becomes zero, but no operation identifies and decrements B under the complete sorted lock set. B is now unreferenced but pinned by a stale counter. The API returns entry/address identities, not the removed chains, so this requires a preimage/association strategy, not just decrementing the requested digest. Rollback of a graph drop gives the inverse divergence.

`sum(graph_refs)` is also an incidence count, not the number of live parts: one part with two distinct blobs contributes two. The equality promised by `metrics-and-eviction.md:100` is dimensionally wrong.

**Required change:** define part/revision identity, unique part-to-generation edges, idempotent insert/update/remove deltas, success/refusal handling, and accounting for all prune/replacement/cascade paths. Define how complete removed chains are captured and how all their heads are locked. A counter declaration is not proof of the one-generation invariant.

### Finding 3 [P1] — The `graph_refs=0` gate does not provide the generation identity required by restore or prove graph-generation exclusivity

**References:** `ledger.md:15-20,105-107,223-230`; `architecture.md:130`; `src:dagql/cache_transfer_types.go:40-56`; `src:dagql/cache_stored_parts.go:129-146`; finding 2.

The graph's persisted part has a digest chain, optional addresses, and an opaque renewal key. This design gives no durable part/revision-to-generation mapping, no rule encoding a generation in those optional fields, and no rule restoring such a mapping. The renewal-key construction still hashes ordered digests only. Yet restore says to inspect every part's `(digest,gen)` and compare it to the verified head. The graph counter is aggregate state and cannot supply that part-held `gen`.

Concrete restore history: checkpoint C contains P referencing D from g1. P is later dropped, g1 deleted, and g2 uploaded and verified; failover restores older C. The counter gate might have been respected during normal operation, but the restored graph can reintroduce the older publication. The first restore instruction needs P's old generation to reject it; the specified representation does not identify it. Merely assigning every restored part to the current head changes the stated semantics and can mis-reconcile counts from a different checkpoint era.

Quarantine's zeroing does not repair this: finding 2 shows it is not a complete graph-edge removal transaction, and finding 1 shows rollback can leave part/count disagreement. A concurrent stale leader is another independent route around aggregate counts (finding 6).

**Required change:** choose explicitly between generation-bound publications with durable side-state and a digest-only graph that is deliberately rebound to current verified content on restore/mint. The latter may be a useful simplification for content-addressed bytes, but it is not the current `(digest,gen)` restore algorithm or proof that only one generation owns parts. Generation-scoped keys remain legal; this is an integration defect, not a wire-protocol objection.

### Finding 4 [P1] — Reserve still allocates directly from `failed` before physical cleanup, and its zero-row branch is not the written transaction

**References:** `ledger.md:18,55,61-66,87-107,117-123,224,235-238`.

R3 genuinely fixes the previous unconditional generation advancement for pending, verified, and deleting heads. The partial unique index is now a backstop rather than the ordinary waiter mechanism. But the allocation predicate still permits `h.state='failed'` with no physical-delete or `graph_refs=0` predicate. Failed state is set before object cleanup; saying the sweeper only sets **deleted** after cleanup does not constrain the distinct **failed** branch.

Interleaving: a completed object loses its donor reply; the lease sweeper changes pending g1 to failed and commits. Before failed-object cleanup runs, E2 locks the failed head and the exact `INSERT ... SELECT` allocates pending g2. This contradicts both the transition table's cleanup guard and the stronger deleted-only statement at line 18. My in-memory predicate execution returned g2 for a failed g1 without any deletion evidence. It also returned g2 for a deleted row with `graph_refs=1`, because that guard is prose only.

There is a second control-flow omission: when `RETURNING gen` yields no row, the next SQL statement still unconditionally assigns `$g_new` and sets head pending. `$g_new` is undefined on the promised reuse/wait/skip paths. Line 105 describes a branch, but does not place it before this update/commit, and supplies no `quarantined` outcome. The text also says decisions read `blob.state` under the head lock, while reserve actually branches on the mirror.

**Required change:** allow allocation only from an explicitly cleanup-complete source with zero references, or remove failed-to-pending allocation and require failed-to-deleted cleanup first. State the early no-allocation return while the lock is held, including quarantine behavior, and update the head generation only on successful insertion. Do not infer a missing SQL guard from an invariant the SQL is supposed to establish.

### Finding 5 [P1] — A zero-row source transition can still change the head mirror, bypassing GC protections and confusing ownership

**References:** `ledger.md:75-82,115-136,194-216`; `architecture.md:53`.

The new rowcount check tests the **head update**, not whether the guarded **blob update** happened. An update that matches zero rows is not a SQL error; the following head update can still affect one row and satisfy the stated check. [PG-U]

Concrete GC race: S takes a candidate snapshot; a concurrent mint refreshes its timestamp; S then gets the head lock. The guarded blob transition correctly changes zero rows because its age guard fails. The next unconditional head update nevertheless sets `deleting`. The text then proceeds to the external DELETE without a specified successful-mark check. Thus the very conditional predicate intended to protect the fresh URL does not gate the rest of the displayed operation. An equally direct fixture with `graph_refs=1` produced zero blob updates, one head update, and committed states `(verified,deleting)`.

A stale upload-failure report can likewise change a verified head to failed after its pending-only blob update changed nothing. A late verify after lease failure can change a failed head to verified after its source update changed nothing. The latter fixture produced `(failed,verified)` in the in-memory SQL witness. Verify's zero-row instruction prohibits publication, but does not explicitly roll back/skip the mirror update already shown. GC and failure statements do not even have that instruction.

**Required change:** couple the mirror to the source transition's returned rows, require the intended source rowcount, and perform no mirror transition or external DELETE when the source guard fails. Define no-op/stale-event handling for every transition. A head row existing at the right generation is not evidence that its blob changed state.

### Finding 6 [P1] — Publication and reserve are still unfenced by leadership; recording `delete_epoch` is not enforcing it

**References:** `ledger.md:87-102,140-157,194-219,223,239`; `architecture.md:54,249,341-342`; `decision.md:37`.

Publish has no current-leader predicate or locked `pool_leader` row. Reserve and verification do not have one either. GC mark reads a live holder, with an application comment checking identity, but neither checks `$epoch` nor gates the subsequent delete/stamp with it. `delete_epoch` is recorded and never compared in the shown delete-confirmation UPDATE. The claim that a deposed leader cannot delete because it no longer holds the lease row is particularly misleading after the mark transaction commits: its row lock has already been released.

Concrete split-brain: A pauses after learning an export; B acquires a new epoch, restores its graph, and starts serving. A resumes, takes the ordinary blob/head locks and completes the written publish transaction into A's separate graph. No statement rejects A. B's graph and A's graph now have different stored parts, while both operate on the shared counters. Digest-sorted locking serializes database rows, not the two graphs, and a checkpoint-pointer CAS does not fence those graph writes.

There is a further long-transaction issue: PostgreSQL `now()` is fixed at transaction start, not re-read wall time. A lease check made after a long row-lock wait can therefore accept an already-expired lease relative to real time. A bounded transaction/check-at-use policy is necessary if that predicate is claimed to be a live authority test. [PG-T]

**Required change:** specify current holder **and epoch** validation for every authoritative mutation, including restore/reconciliation, with a consistent leader/head/graph lock order and transaction-age budget. State the narrower truth about already-dispatched DELETEs: generation-qualified keys protect newer generations, but do not prove that only the current leader can execute the old request. Enforce or withdraw the stronger epoch-fenced-delete invariant.

### Finding 7 [P1] — Sorted publication does not establish global deadlock freedom or protect the renewal deadline

**References:** `ledger.md:140-162,179-190,194-205,245-246`; `architecture.md:99`; `src:dagql/cache_stored_parts.go:60-67`.

**Restricted answer:** assuming the stated same per-row order, I could not construct a deadlock using only one sorted publish transaction and the single-digest GC-mark transaction as written. Mark holds no second digest while waiting for a first. Two sorted multi-digest publishers also have no digest-order cycle under that assumption. That limited result should be credited, not replaced with an invented publish/mark deadlock.

The rest of the protocol does not obey that order. Mint is a multi-row UPDATE without a sorted locking prepass. An allowed execution is: mint M locks blob B first; publisher P locks A then waits on B; M next tries A and waits on P. M and P form a database deadlock even though P's own query is sorted. No PostgreSQL query-plan guarantee requires the mint UPDATE to visit A before B. PostgreSQL explicitly recommends a consistent acquisition order across all transactions, not just a selected pair. [PG-L]

Independently, P can hold blob/head locks while `SetStoredPart` waits on the global graph lock. A renew for one of those digests blocks in its mandatory stamp UPDATE. A slow graph operation can thereby consume the engine's entire 2-second renewal budget even though renewal does no graph walk. There is no lock-wait/statement timeout, cancellation-to-`unavailable` procedure, or separate deadline budget for this case. It is not merely an unmeasured millisecond constant.

**Required change:** define the order and bounded-wait handling for mint, all transitions, multi-chain drops, leader operations, and graph acquisition; handle database deadlock victims and publication rollback using finding 1's protocol. The restricted sorted-head result is not a global deadlock-freedom proof.

### Finding 8 [P1] — Late PUT completion can recreate a cleaned-up generation after the scanner has stopped watching it

**References:** `ledger.md:107,111,126-136,221-226`; `architecture.md:173`; `revision-r3.md:17`.

The specification now correctly acknowledges that capability expiry is not transfer drainage. Its recovery algorithm still does not implement that acknowledgement.

Interleaving: a PUT starts before the 5-minute capability expires and continues making progress. At lease expiry g1 becomes failed; cleanup DELETEs a missing/old g1, stamps deleted, and purges its accounting row. The already-started PUT then completes, materializing g1 again. Verification cannot revive it: its pending/owner/unexpired predicate fails. The recovery scanner enumerates failed and deleting rows, not a completed deleted generation whose row has been purged. A weekly LIST is only described as finding objects missing from the ledger, with no deletion/retry rule.

Generation keys prevent this late PUT from overwriting g2. They do not reclaim g1 or establish the claim that its object remains physically gone. An extra fixed wait based on PUT URL TTL would repeat the same mistake the document now warns against.

**Required change:** retain cleanup intent or define a repeated reconciliation/deletion policy for late objects in retired generations, including deleted/non-head rows and missing-ledger keys. Define the retirement/purge horizon and how new late completions remain discoverable. Do not mark the late-completion half of the previous finding closed by adding an explanatory sentence alone.

### Finding 9 [P1] — The completed-PUT/dead-donor publication path still has no trigger

**References:** `latency-and-azure.md:45`; `ledger.md:111,126-136,173,224`; `architecture.md:227,239-242`; `decision.md:41-44`; `revision-r3.md:17`.

The donor/service time distinction is physically sound after bytes reach the store. The stated workflow still verifies only digests in `uploaded.done`. If the donor completes PUT and dies before reporting `uploaded`, no described event starts verification. Its reservation expires, becomes failed, and is deleted. Ledger line 111 refers to a late completion being reconciled by §4 or the scanner, but §4 is only the owner-fenced UPDATE and the scanner deletes failed objects; neither HEADs an unreported completed pending upload and invokes verification/publication.

This exact case was Claude round-3 F10 and was qualified in my round-3 latency answer. It remains open. A donor that reported completion before dying can of course leave service verification running; that narrower case is not disputed.

**Required change:** specify a bounded HEAD/verification trigger on disconnect, upload-report timeout, or pending-object reconciliation, preserving the descriptor/chain and publish intent; or narrow the claim to replies already delivered. Distinguish the end of donor dependence from span-end-to-published exposure. The categorical killed-node assertion that every in-flight PUT creates no object also needs its acknowledged-completion/race qualification.

### Finding 10 [P1] — Corruption quarantine now has a legal name, but unsampled corruption still lacks service feedback and the fallback description remains wrong

**References:** `ledger.md:69-70,225`; `architecture.md:51,55,150,258`; `metrics-and-eviction.md:112-123`; `src:engine/snapshots/chain_error.go:73-86`; `src:dagql/cache_part_content.go:674-684`; `src:engine/remotecache/protocol/protocol.go:46-62`.

The content invariant is honestly weakened and a verified-to-quarantined transition now exists. That closes two specific text defects. It does not create the newly assumed **engine mismatch report**. No protocol command or grounded telemetry-to-digest rule is specified to tell the service which unsampled object failed content verification. Success-only install attributes cannot supply it. A same-size wrong complete object can therefore remain advertised to fresh receivers, which repeatedly transfer, reject and recompute it while the store reports successful GETs.

The ordinary checksum-mismatch path in the pinned source produces a `ChainContentError`; the importer exhausts that source and returns a refused-part result. Architecture line 258's engine column correctly says source-exhausted-to-compute, but its service column and line 150 still say the wrong object fails the receiving **build**. These describe different outcomes. Arbitrary local-storage/import errors may propagate, but that is not a reason to relabel ordinary bad-remote-content fallback.

**Required change:** distinguish those error classes and specify a real suspect-object/hash-scrub/repair feedback path, or explicitly accept repeated unsampled-corruption misses as a v1 limitation. Do not invent a wire report. Apply quarantine with finding 2's complete-chain accounting, not just zeroing one digest counter.

### Finding 11 [P1] — Session completeness uses a nonexistent attribute and revisions are assigned in the wrong namespace

**References:** `metrics-and-eviction.md:9-12,37,67`; `architecture.md:95`; `src:engine/telemetryattrs/attrs.go:731-742,766-793`.

The completeness guard is written as `cache.cache_span_count`. The pinned contract actually emits `dagger.io/cache.session.spans`, on the session-complete carrier, counting spans carrying `CacheResultIDAttr`. It is not a count of every ingested span. Comparing that number to all spans received can also miss losses: unrelated spans can make the raw count larger even when cache-result spans are absent. A killed session may never deliver the completion carrier at all; a 10-minute idle timeout does not prove completeness.

Likewise, the entry revision is said to increment when **the graph** replaces the value under the id. Receiver IDs belong to each engine cache. Replacements in the service's merged graph do not define those receiver revisions. The grounded `dagger.io/cache.replacements` supplies the engine's replacement counter, absent at zero, but the document does not use it or define out-of-order assignment against it.

**Required change:** use the exact producer attributes and resource namespaces, compare like populations, define unknown/missing completion behavior, and bind installs/lazy observations to the co-observed engine replacement revision. Do not classify idle-but-unverifiable sessions as complete or claim that only pre-field historical parts have unknown donor evidence.

### Finding 12 [P1] — Interval union fixes within-entry nesting but double-attributes descendant entries and includes non-evaluation work

**References:** `metrics-and-eviction.md:17-26,41-47,54,67,163`; `src:engine/telemetryattrs/attrs.go:731-742,772-777`.

The new union correctly yields the full outer interval rather than R2's ten-second remainder for same-entry nested work. Causal-parent fallback is also a real correction. But it now collects **every span** whose parent chain reaches each entry, without an ownership boundary at other entries or an evaluation-only filter.

Counterexample: entry A's evaluation runs from 0 to 100, with entry B's evaluation nested from 10 to 90. A receives 100 seconds and B receives 80. Summing per-entry recompute yields 180 seconds for the same 100-second execution interval. This is ancestor/descendant duplication, not legitimate parallel work accounting. If both are stored, their full inclusive donor costs can both be credited on installs, even though installing A's snapshot need not evaluate B separately.

A call that only waits for another entry's lazy task, or performs lookup/serialization, is also included by the literal every-span rule. Subtracting install-span intervals does not distinguish that waiting/overhead from the entry's own original evaluation. Missing-parent fallback can silently replace the expensive lazy interval with the cheap call interval; it is flagged, but not excluded from donor-cost/admission decisions.

**Required change:** specify evaluation-span ownership and entry boundaries, distinguish inclusive diagnostic latency from disjoint attributable work, and define uncertainty treatment. Preserve the useful union for spans owned by the same chosen accounting unit. Do not sum causal-inclusive costs as if they were independent saved computation.

### Finding 13 [P1] — Donor cost is stored per blob but used per part/entry, and mixed-entry recomputation still disappears from value

**References:** `ledger.md:25-40,93-95`; `metrics-and-eviction.md:17-26,54-75,163`; `architecture.md:260`; `src:dagql/cache_transfer_types.go:52-56`; `src:engine/telemetryattrs/attrs.go:772-788`.

Recording donor evidence at publication is better than allowing a historical estimator to starve after thirty days of hits. The proposed schema, however, stores `donor_recompute_seconds` on `(pool,digest,gen)` and fills it at reserve. Metrics say it is on the **published part** and then add it over **installed entries**. There is no persisted part/revision-cost association or receiver-install-to-publication selection rule.

Two different recipes can produce the same blob with very different recomputation costs. A blob-level scalar cannot represent both recipes' counterfactual costs. A chain can have several blobs and an entry several parts; the design does not say whether to sum costs, select a donor, allocate whole-entry cost across parts, or prevent credit for uninstalled sibling work. Native success telemetry supplies the receiver entry and installed part, not the chosen donor publication's cost identity.

Mixed work is still omitted from the ratio: line 54 retains recompute for installed-mixed entries, but line 73 defines `recompute_remaining` only on **entries that were not installed**. An entry installing P and computing Q for 100 seconds contributes none of Q's 100 seconds to that denominator. Applying the full entry cost to P also credits Q as avoided. Separating gross from `max(C-I,0)` and `max(I-C,0)` fixes arithmetic for a correctly identified pure install; it does not fix these input units.

**Required change:** define the durable cost key and install mapping, choose a per-part or per-entry counterfactual with unknown handling, and include residual recompute on mixed entries. Label donor duration as an estimate of avoided compute under a specified topology, not an intrinsically exact per-blob property.

### Finding 14 [P1] — The new ceiling denominator double-counts the opportunity subsets and is not the claimed attainable fraction

**References:** `metrics-and-eviction.md:60-61,73-76,131`; `architecture.md:197,264`.

The displayed ceiling is `(S+W+K)/(S+W+K+R)`, while W and K are explicitly subsets of remaining recompute R. Adding those subsets again to the denominator recreates denominator duplication.

Concrete pre-build case: no installs, S=0; remaining recompute R=100 seconds; all 100 seconds are valid would-hits W; K=0. The formula reports **50%**, although every second of the measured remaining workload is in the opportunity set. Python reproduced 100/(100+100)=0.5. The corrected R2 formula was bounded by requiring disjoint subsets, not by adding those subsets to both sides of a second denominator.

R3 also asserts mutual exclusion without carrying forward the old explicit tie rule: a current entry can have both an earlier exact match and a different earlier pairing match. That does not make this particular fraction exceed one, because its denominator contains its numerator, but it still double-counts the opportunity population. An expression bounded by construction is not necessarily a meaningful ceiling. Gross/net, install losses, mixed work and unknown costs must also use consistent units/populations.

**Required change:** define one residual-work population and the union/tie precedence of W and K, then derive the fraction from nonduplicated quantities. Keep zero-denominator no-data behavior, which is now stated correctly. Do not use the present formula as Gate A's quantitative ceiling.

### Finding 15 [P1] — `demand_p95` is recomputation duration, not observed fallback wait, and miss flags do not identify remote attempts

**References:** `metrics-and-eviction.md:118-125`; `latency-and-azure.md:65-79`; `architecture.md:251`; `src:engine/telemetryattrs/attrs.go:656-689`.

The new `dcache_demand_p95_seconds` is the recompute union for entries with incompatible/expired/released miss flags, described as how long engines waited before falling back. Those flags are lookup/session-resource facts. They do not assert a remote HTTP attempt or remote candidate provenance, and they do not timestamp its beginning or fallback boundary.

An entry that immediately rejects an expired local candidate and computes for 120 seconds would contribute 120 seconds of alleged remote wait. A remote content attempt can instead stall and then recompute without any of those lookup miss flags: a metadata lookup hit can precede the failed lazy import. That demand can be omitted entirely. Recompute duration is neither an upper nor a lower bound on just the failed-download waiting interval in these cases.

Provider errors plus successful-install p95 are now clearly separated from the service's liveness probe. That closes the invented failure-ratio defect. The remaining new panel must not claim attempt-level wait observability, and the only latency threshold currently warns rather than trips; the automatic breaker should not be described as detecting all engine-path hangs or slow continuous reads. The document does correctly admit that such reads are not capped at thirty seconds (`latency-and-azure.md:91`).

**Required change:** rename the panel to the population actually observed, or add a grounded attempt/fallback timing source. State the provider-only breaker limitation for client-network and success-censored failures, including the no-sample case; do not infer a missing engine signal from lookup flags.

### Finding 16 [P1] — Regret's event-rate alert is not a closed cohort and can exceed 100%

**References:** `metrics-and-eviction.md:107,144,174-176`; `revision-r3.md:34`.

Unique refetch deduplication is useful, and per-pool aggregation fixes the previous label mismatch. The alert still divides refetch **event time** over the last 24 hours by eviction **event time** over the last 24 hours, rather than evaluating outcomes for the same eviction cohort.

Witness: ten items are evicted, then refetched 23 hours later. At hour 25 their refetch events remain in the rolling numerator but their evictions have left the denominator. If one new eviction is in that denominator, the ratio is 10/1 = **1000%**; with none, the clamp produces another large misleading result. Counting each item once does not fix different time populations. Calling both windows the same window is not a cohort join.

The identity is also underspecified across generation changes: the re-upload belongs to a new generation and must be associated with one old eviction event, not joined on equality of the uploaded and evicted generations.

**Required change:** maintain and query an eviction-time cohort with unique members and a defined maturation horizon, or stop labelling this event-rate alert as bounded regret. Expose eligible/matured/no-data counts and define assignment of a re-upload to its eviction. Preserve per-pool label aggregation.

### Finding 17 [P1] — Delayed-free accounting starts after the grace interval it is meant to cover, and failed/quarantined bytes disappear from charge before deletion

**References:** `metrics-and-eviction.md:96-100,168-172`; `ledger.md:117-123,200-216,224-225`; `architecture.md:171-174`.

The durable `pending_free` table only represents generations whose delete is **committed but unconfirmed**. After eviction removes graph references, a blob may remain verified for G and another hourly sweep before it can be marked deleting. During that interval it is charged, but absent from projected frees. The controller can repeatedly remove additional graph groups to reach the same target, exactly the cross-tick over-eviction the prior review identified.

Example in arbitrary capacity units: charged=90 and low=70. Dropping a group projected to free 20 now does not create a deleting row because a URL was just minted. Next tick still sees charged=90, committed-delete projections=0, and drops another 20. The eventual result is 50 instead of 70. Recomputing groups within a pass does not record this earlier graph-retirement intent.

Conversely, `failed` and `quarantined` generations are excluded from charge even when they still hold physical objects. Changing verified to quarantined releases its charge before the immediate repair DELETE is confirmed; a failed upload can also have completed bytes awaiting cleanup. This conflicts with the stated physical-confirmation release rule and can admit beyond the storage budget during failures.

**Required change:** distinguish allocated/physically-occupied bytes, graph-retirement projections, marked-delete work, and confirmed frees; persist generation-qualified projections from selection through grace and deletion, with cancellation/reconciliation. Specify which budget, if any, intentionally excludes orphan/corrupt bytes, rather than silently calling them tombstones before their objects are gone.

### Finding 18 [P1] — Connected-component eviction and the anti-overshoot rule can make reclamation impossible or unbounded

**References:** `metrics-and-eviction.md:158-172,204`; `architecture.md:174`.

Lowest-score selection fixes the original two-cold-sharers versus hot-singleton ordering. It does not fix the new candidate and stopping rules.

Union-find over all parts sharing blobs creates connected components. A common base blob can connect every application snapshot into one huge component. Requiring removal to leave **every referenced blob** with zero surviving references excludes a cold application's profitable unique-blob reclamation merely because its base remains shared. Whole-component removal instead includes hot unrelated applications. The intended minimal reclaim set and the connected-component approximation are not the same thing.

The overshoot/stopping inequalities conflict for indivisible blobs: a selection must leave `projected_after >= low`, while the loop stops only at `<= low`. Unless the selected distinct bytes land exactly on low, the next necessary selection may be forbidden. For usage 90, target 70, and one 30-unit shared group, removing it leaves 60 and is forbidden. Splitting by dropping its largest part frees zero if a sibling still owns that blob; dropping the sibling later still frees all 30. Parts do not split object bytes. The algorithm either stalls above target or violates its own overshoot cap.

A 1,000-candidate bound also needs a coverage/fairness rule and retention eligibility for all group members; these are not specified. Dropping the largest part is not a value-aware or retention-safe decomposition. Unknown-cost groups being excluded until the hard watermark is now explicit policy, but it does not solve these known-cost cases.

**Required change:** choose a tractable candidate approximation that measures distinct bytes actually freed, permit a documented bounded overshoot or a defined stop-above-low result, and enforce group-member retention/fairness. Do not claim that splitting a graph part can split an indivisible shared blob.

### Finding 19 [P1] — Architecture still defines incompatible metric and Phase-0 procedures after the metric rewrite

**References:** `architecture.md:19,197,215,260,264,318`; `metrics-and-eviction.md:41-75,112-132`; `decision.md:61`.

The old SLI paragraph survives at architecture line 260: receiving lazy-inclusive recompute minus installation, self-time plus lazy spans, and installs divided by installs plus executed. R3's metrics use donor cost, interval union, computed/no-install populations, mixed-entry measures, and separate gross/net counters. Following the architecture still reintroduces the old donor/receiver and metadata-hit denominator mistakes. This was explicitly part of Claude round-3 F9 and my F9's closure request.

Architecture line 197 also retains the call-duration pairing query, labels exact prior matches as perfect-cache hits without the new verified-part eligibility, and does not carry forward the disjoint/tie handling. Line 264 still directs Phase 0 to that query before describing the corrected critical-path gate. The new two-gate decision is a genuine closure of the circular live-service prerequisite, but the actionable measurement procedure is not aligned with it. Line 19 points to a threshold in §9 that is no longer supplied, and R1's mitigation at line 318 still refers to a go/no-go threshold.

Line 215 additionally calls the breaker an install-failure breaker after the metrics document explicitly disclaims a grounded install-failure population.

**Required change:** make the architecture's SLI/query/breaker descriptions reference the selected normative metric semantics, and specify the pre-build opportunity calculation separately from the post-prototype wall-time comparison. Do not leave old operational formulas alongside the new names.

### Finding 20 [P2] — Removed pins and pre-generation object keys survive in the architecture

**References:** `architecture.md:40,49,137,154,161-162,227`; `ledger.md:9-20,53,138-162`; `decision.md:36`.

I1 and the cold-flow publication step still require a pin, and line 227 specifically directs publication under a **durable pin** to ledger §4, which now describes verification. Ledger §5 and decision invariant 4 expressly remove pins. The diagram and object-layout section still omit `/g<gen>`, while the lost-race prose says old/new exporters write the same key. These are not the generation-key workflow that now carries byte-delete safety.

Ledger's normative override helps establish intended precedence, so I do not count this stale copy alone as another P0. It still gives an implementer the old key derivation and wrong publication instructions in the main architecture.

**Required change:** remove the obsolete pin/key flow and describe generation-scoped capabilities consistently. Same-digest retry idempotence must not be used to erase the distinction between different generation keys.

### Finding 21 [P2] — Request-cost totals were still not recomputed, despite the R3 closure claim

**References:** `architecture.md:205-209,214`; `decision.md:48`; `revision-r3.md:36`.

The current rows imply $0.10 engine PUT + $0.00808 HEAD/sample GET + $0.12 engine GET + $0.0036 checkpoints = **$0.23168/day**, or **$6.9504 per 30 days**. They still display about $0.27/day and $8/month. R3 says it re-derived approximately $0.24/day and $7/month, but did not update the actual table/summary.

The sampled reread expectation and checkpoint schedule rows themselves are now corrected. This is a small stale-summary issue, not a material change to the conclusion that requests are inexpensive relative to bytes. The budget also remains a scheduled-on-change estimate, not a strict maximum including graceful-stop checkpoints.

**Required change:** update the total/summary from the selected displayed rows and name the assumed month length.

### Finding 22 [P2] — The impossible FQDN NetworkPolicy mitigation still survives in R7

**References:** `architecture.md:86,336`; `decision.md:87`.

R7 still prescribes a NetworkPolicy denying egress to `api.dagger.cloud`, while the architecture's own correction and decision brief state that ordinary NetworkPolicy cannot match that FQDN and name CoreDNS/Cilium alternatives. The discovery-fallback half of R7 was corrected; the network-mitigation half of Claude round-3 F25 was not.

**Required change:** align R7 with its existing corrected mitigation. This finding is the contradiction between the supplied clauses, not a newly investigated network implementation.

### Finding 23 [P2] — Known-dead owners still suppress a digest for the full ten-minute lease

**References:** `ledger.md:67-68,115-123,168-175`; `architecture.md:97,160,240`; Claude round-3 F26.

Session death is known through `EndRemoteProcess`, yet the specified recovery waits for lease expiry. Later candidate donors are suppressed while that pending reservation is considered live. Each waiter has only four seconds, so the cache can repeatedly discard otherwise reusable chains during the ten-minute owner-death blackout. The wire/path incoherence from R2 is fixed; this remaining availability policy was not addressed.

**Required change:** either explicitly accept and measure the blackout or trigger owner-fenced early failure/reconciliation when session loss is authoritative. Generation keys avoid overwriting a replacement generation, but early failure must still respect findings 8 and 9's late-object cleanup and completed-PUT salvage cases.

## Closure audit: all 22 GPT round-3 findings

“Closed” means the specific specification defect is closed, not that a deployment gate has been run. “Partial/open” identifies the residual rather than recounting it as a second finding.

| R3 # | Disposition | Current evidence and residual |
|---|---|---|
| 1 reserve/head mirror | **Partial/open** | `ledger.md:87-106` now blocks pending/verified/deleting allocations; `:75-80` adds mirror writes. Cleanup predicates, zero-row branch and source/mirror coupling remain wrong: findings 4–5. |
| 2 expiring publication pin | **Partial/open; replacement has a new P0** | Pin removed at `ledger.md:53`; same-head live-transaction exclusion at `:140-162` closes the original continuously-live lock race. It does not implement rollback/visibility/cancellation: finding 1. |
| 3 generation/graph binding | **Open** | `ledger.md:15-20,223-230` substitutes an aggregate counter but supplies no per-part generation identity, drop deltas or restore association: findings 2–3. |
| 4 uncorrelated mint join | **Closed for that predicate/return defect** | `ledger.md:180-185` correlates pool, digest and generation and returns `(digest,gen)`. Broader identity/locking questions are findings 3 and 7, not recurrence of the missing join. |
| 5 failed-object evidence/cleanup | **Partial/open** | `ledger.md:117-123,224` adds explicit failures and physical DELETE before purge. Failed-source reserve and post-cleanup late PUT remain: findings 4 and 8; reply-loss salvage is finding 9. |
| 6 impossible waiter message/deadline | **Closed for the reported wire/deadline defect** | `ledger.md:164-175` sends no second continuation and makes four seconds an internal policy. Its intentionally lossy short wait and dead-owner blackout remain policy limitations; finding 23. |
| 7 entry populations/mixed work | **Partial/open** | `metrics-and-eviction.md:26-39` fixes the local-hit zero-overhead test and stores mixed measures. Exact session/revision handling and mixed-ratio/cost accounting remain wrong: findings 11 and 13. |
| 8 exclusive-time attribution | **Partial/open** | `metrics-and-eviction.md:43-45` fixes same-entry nested undercount and causal ancestry. Cross-entry ownership/all-span union remains wrong: finding 12. |
| 9 gross/net/install cost | **Partial/open** | `metrics-and-eviction.md:56-58,71-73` fixes `max(C-I,0)`/`max(I-C,0)` for a known pure install. Cost units/join, mixed residual and architecture formula remain: findings 13 and 19. |
| 10 ungrounded breaker population | **Partial/open** | Success-only limitation and provider-error basis at `metrics-and-eviction.md:112-125` remove the invented failure ratio. Newly invented fallback-wait meaning and stale architecture description remain: findings 15 and 19. |
| 11 reversed reclaim-group scoring | **Partial/open** | Lowest-first at `metrics-and-eviction.md:164` fixes the exact reversal and original isolated cold-sharer witness. Component collateral, discrete overshoot and eligibility remain: finding 18. |
| 12 historical-generation quota/delayed frees | **Partial/open** | State-based charge and durable table at `metrics-and-eviction.md:170-172` stop charging deleted rows in that document. Pre-mark delayed frees and occupied failed/quarantined bytes are missing: finding 17; architecture `:174` still says unfiltered sum. |
| 13 renew no-DB contradiction | **Closed** | `architecture.md:99` and `ledger.md:188-190` explicitly include one committed stamp and label 200 ms a target. Blocking behavior introduced by head-lock publication is finding 7. |
| 14 value-existence dedupe | **Closed** | `architecture.md:97,160,224` now requires usable parts/live pending reservations. No surviving stored-value-only skip rule was found. |
| 15 merge-refreshed pool retention | **Closed** | `architecture.md:170` and `metrics-and-eviction.md:154-156` agree on install-only pool demand and timestamp-based idle pruning for all roots. Recipient merge retention is correctly separate. |
| 16 production topology | **Closed at design-topology level** | `architecture.md:12-13,283,330`, `latency-and-azure.md:54-56`, `decision.md:20,26-27,85` consistently distinguish design-cluster MinIO and production external S3/stable metadata. Old diagram/credential names are now scoped by latency `:55`; no production-MinIO recommendation remains. |
| 17 sampling/content/repair | **Partial/open** | Weakened I3 `architecture.md:51`, 1% budget `:206`, quarantine `ledger.md:69-70,225` are real fixes. Ordinary fallback/build-failure conflict and unsampled feedback remain: finding 10. |
| 18 circular pre-build gate/discovery | **Partial/open** | Two gates at `decision.md:61`, `metrics-and-eviction.md:131-132`, `architecture.md:264` remove circularity; `architecture.md:224,336` correctly disclaims lifecycle discovery. Discovery remains honestly unproved, not a new P0. The old query/SLIs still survive: finding 19. |
| 19 probe TTFB regression | **Closed** | `s3_latency_probe.py:68-74` timestamps after headers. Actual delayed-header test returned 200, TTFB 0.130503 s and total 0.130697 s with a 0.120 s server delay. Fresh cluster figures are accepted as supplied; no false v2 attribution remains. |
| 20 publication arithmetic | **Closed for the displayed-rate arithmetic** | `latency-and-azure.md:29-39` gives approximately 18.3/31.9 PUT and 38.9–52.5 full path; independently re-derived below. These are scenarios, not measured 2 GB bounds. |
| 21 admission Boolean/unknowns | **Closed as a labelled provisional policy** | `metrics-and-eviction.md:181-188` states AND, pin override, unknown donor rejection, and an explicit frequency-only unmeasured-read fallback. `latency-and-azure.md:89-92` agrees. Inputs still inherit findings 11–13; the model is not calibrated. |
| 22 ratios/PromQL/cohort | **Partial/open** | No-data denominators at `metrics-and-eviction.md:75`, per-pool renewal aggregation `:140`, and unique refetch identity `:176` improve it. Regret still uses mismatched time cohorts and the rewritten ceiling is wrong: findings 14 and 16. |

## Closure audit: all 26 Claude round-3 findings

| R3 F# | Disposition | Current evidence and residual |
|---|---|---|
| F1 reserve/head/failure SQL | **Partial/open** | State branch and explicit failure statements now exist, `ledger.md:87-123`; cleanup and source/mirror coupling remain findings 4–5. |
| F2 waiter/deadline/durable intent | **Partial/open** | Wrong message and watchdog dependency removed at `ledger.md:164-175`. Durable waiter intent was not adopted; four-second abandonment is explicit fail-open policy, not a wire bug. Completed-PUT salvage still lacks a trigger: finding 9; dead-owner blackout: 23. |
| F3 generation representation | **Open** | Correlated mint at `ledger.md:180-185` is fixed, but `(digest,gen)` restore and exclusivity remain undefined, findings 2–3. A ledger-only current-head interpretation must be stated rather than assumed. |
| F4 deposed leader GC | **Partial/open** | `ledger.md:196,200` adds holder locking and an epoch record. Reserve/publish, epoch-at-use and delete stamp remain unfenced: finding 6. Correct counters would stop the old stale-snapshot witness, but those counters are not maintained: finding 2. |
| F5 donor-cost starvation | **Partial/open** | Persistent donor-cost intent at `ledger.md:37`, `metrics-and-eviction.md:67,165` removes expiring-history-as-zero policy. Actual blob/part/entry association remains absent: finding 13. |
| F6 class/join/mixed/zero denominators | **Partial/open** | Local-hit predicate and zero-denominator policy are fixed at `metrics-and-eviction.md:34,75`; receiver entry/part attributes are grounded by `src:engine/telemetryattrs/attrs.go:772-788`. Namespace/completeness and mixed-cost/ratio issues remain, findings 11 and 13. |
| F7 greedy/groups/retention/units | **Partial/open** | Lowest-first and bounded candidate intent `metrics-and-eviction.md:162-166` fix direction. Graph-component blast radius, indivisible overshoot, retention/fairness and cost granularity remain: findings 13 and 18. |
| F8 MinIO/production topology | **Closed at topology level** | `architecture.md:12-13,283,330`, `latency-and-azure.md:54-56`, `decision.md:20,27,85` agree. Source/probe/capacity experiments remain rollout gates, not contradictory deployments. |
| F9 five surviving architecture clauses | **Partial/open** | Renewal `architecture.md:99`, retention `:170`, availability dedupe `:160`, gate `:264` corrected. SLI `:260` and naive opportunity query `:197` remain: finding 19. |
| F10 dead-donor verification trigger | **Open** | `architecture.md:227,240` still verifies delivered done lists and expires unreported uploads; `ledger.md:111,126-136,224` supplies no salvage trigger. Finding 9. |
| F11 aggregate rate as per-stream tail | **Closed as a bound claim** | `latency-and-azure.md:20-25,39,54-55` explicitly limits small-object scenarios and production applicability. No 2 GB tail measurement exists; see arithmetic assessment below. A fair-share eight-large-object illustration is about 255 s, not a measured tail. |
| F12 full-path arithmetic | **Closed** | `latency-and-azure.md:33-39` agrees with independently calculated 38.906–52.539 s using the displayed fresh rates. Hot-read extrapolation is explicitly a scenario, not production sizing. |
| F13 request costs/reread policy | **Partial/open** | Sampled reread and checkpoint rows fixed at `architecture.md:206,208`; total/summary still wrong at `:209,214` and `decision.md:48`. Finding 21. |
| F14 wrong bytes/I3/feedback/diagram | **Partial/open** | I3 weakened `architecture.md:51`; diagram sample `:35-36` corrected; quarantine legal `ledger.md:69-70,225`. Feedback and ordinary fallback contradiction remain: finding 10. |
| F15 breaker provenance/p99 | **Partial/open** | Invented failure ratio and p99 breaker removed in `metrics-and-eviction.md:112-125`, `architecture.md:251`, `decision.md:49,85`. Mislabelled demand duration and stale install-failure wording remain: findings 15 and 19. |
| F16 TTFB/probe attribution | **Closed for timing and stated measurement basis** | Timestamp after headers `s3_latency_probe.py:68-74`; fresh numbers/sample limits at `latency-and-azure.md:7-25`; actual local timing test passed. Concurrent PUTs assert status, not readback integrity; no stronger integrity result is claimed here. |
| F17 redundant/non-reentrant pin | **Closed in ledger; stale architecture open** | Pin removed at `ledger.md:53`; old pin instructions persist at `architecture.md:49,227`, finding 20. Replacement protocol's rollback defect is finding 1. |
| F18 failed/late objects/tombstone growth | **Partial/open** | Failed DELETE before purge at `ledger.md:224` fixes the first orphan path. Late recreated objects remain finding 8. Deleted rows are retained (`architecture.md:171`) without a declared compaction horizon; that is a disclosed capacity-policy omission, not evidence of immediate data corruption. |
| F19 scanner Drop condition | **Partial/open** | `ledger.md:223` replaces the old head-not-current condition with head-current and zero refs. Normal GC already requires zero refs; it is not a substitute for the missing drop/count protocol or a repair of count-positive mismatches. Findings 2–3. |
| F20 decision transaction wording | **Closed syntactically, not as atomicity proof** | `decision.md:33` now matches call placement in `ledger.md:140-157`. Neither makes dagql transactional: finding 1. Distinct-count precondition remains correctly explicit in ledger `:148-154`. |
| F21 pre-generation architecture | **Partial/open** | Tombstone wording and generation DELETE at `architecture.md:171` corrected. Layout `:40,137`, same-key race `:161-162`, and pin flow `:227` remain stale: finding 20; unfiltered quota wording `:174` conflicts with finding 17's selected semantics. |
| F22 metric gaps | **Partial/open** | Pure-install net arithmetic `metrics-and-eviction.md:56-58`, prior verified-part requirement `:60,76`, quota gauge `:99` fixed. `dcache_sessions` is still referenced at `:141` without a catalogue definition; more importantly cost granularity, ceiling and regret remain findings 13–16. |
| F23 exclusive overlap/dependencies | **Partial/open** | Explicit union/causal fallback at `metrics-and-eviction.md:43-45` fixes within-entry overlap, not cross-entry attribution and evaluation filtering: finding 12. |
| F24 admission timing/default | **Mostly closed as policy; scheduling optimization not adopted** | `metrics-and-eviction.md:186-188` defines fallback and pre-reservation/pre-continuation decision. It still need not run before the export/bundle when descriptor sizes are unknown. This wastes control-plane work but is not a new protocol safety defect. |
| F25 NetworkPolicy/lifecycle fallback | **Partial/open** | Lifecycle fallback explicitly disclaimed `architecture.md:224,336`. Same R7 line still prescribes impossible FQDN NetworkPolicy, finding 22; corrected alternatives already exist at `:86` and `decision.md:87`. |
| F26 dead-owner full lease | **Open, accepted only if made explicit policy** | `ledger.md:173`, `architecture.md:240` retain ten-minute expiry despite known session death. Finding 23. |

## Direct answers to the publish attacks and reserve question

| Attack | Result |
|---|---|
| Sorted multi-digest publish versus GC mark | **I could not break the restricted pair** while both database transactions stay live, use the stated common per-row order, and respect success checks. Mark as written handles one digest. Publish/mint has a concrete additional cycle; global graph/leader/drop ordering is not specified. Finding 7. |
| Process killed after `SetStoredPart`, before commit | **No checkpoint captured:** graph memory dies and PostgreSQL rolls back; no orphan part survives. **Checkpoint captured:** graph part can survive in the saved graph without a committed publication reference. Restore must reconcile before serving, and does not have the specified generation mapping. Findings 1 and 3. |
| Transaction rolled back while process survives | **Broken:** native graph mutation remains and a pre-insertion GC snapshot plus rolled-back reference stamp permits deletion. Finding 1 gives the complete interleaving. |
| `graph_refs=0` across quarantine/restore | **Not established:** duplicate/no-op/replacement/prune operations diverge counters; a quarantine drop removes whole multi-digest parts; restore lacks part-held gen and has different checkpoint age. Findings 2–3. |
| Two reserves while pending | **Original race fixed at the allocation predicate:** first winner holds the head lock; second sees pending and inserts zero rows, so it does not hit the partial unique index. The written zero-row control flow still needs finding 4's branch. |
| Reserve while verified/deleting | **Original illegal allocation fixed:** predicate inserts no row. Reuse should capture/mint the verified head and deleting should skip. No new PUT is legal on either branch. Finding 5 can still corrupt the mirror that reserve trusts. |
| Reserve from failed/deleted/quarantined | **Not exact:** failed allocates before cleanup; zero-ref guard is missing from allocation SQL; quarantine is unhandled; zero-row `$g_new` update is not a defined return path. Finding 4. |
| Delayed old-generation DELETE | **Byte ABA remains fixed:** a DELETE for D/g1 cannot remove D/g2 if generation keys are actually used and never reused. Recording an epoch does not cancel that request or prove only the current leader performs it. Finding 6 narrows the leadership claim without reopening the key-level ABA closure. |
| Mark/delete crash recovery | Reissuing DELETE for the same generation remains idempotent under the declared missing-key-success capability assumption. Source-update success, mirror/epoch fencing and graph integration still need findings 2, 5 and 6. |

## Metric computability, boundedness, and zero denominators

- **Grounded:** receiver result ID and replacement count; success-only install part/bytes/nanoseconds; structural/pairing digests and ordered inputs; causal ancestry; lookup miss flags. The actual completeness carrier is `dagger.io/cache.session.spans` (`src:engine/telemetryattrs/attrs.go:731-793`). Missing receiver IDs or ancestry are not fixed by simply declaring an entry class.
- **Interval union:** computable from complete timestamps and resolvable causal links, and correct for chosen same-entry intervals. The current every-descendant ownership rule is not disjoint entry attribution; parent loss is uncertainty, not measured original cost. Finding 12.
- **Gross/net:** the positive/negative decomposition is valid for a correctly identified cost C and install I; current cost storage/mapping does not define C for a receiver part or mixed entry. “Only old parts unknown” is not justified when donor evidence can be missing. Finding 13.
- **Entry hit ratio:** with a unique complete entry/revision population and its stated no-install `computed` denominator, the expression is bounded for positive denominators. Mixed entries are intentionally counted as entry hits, so this is not a part/byte/work hit fraction. Those units must stay visible. Incomplete-session exclusions require finding 11's fix.
- **Value ratio:** algebraically bounded for nonnegative inputs, but currently excludes mixed residual work and has an unresolved counterfactual mapping. A bounded number is not yet a defensible value estimate. Finding 13.
- **Ceiling:** algebraically bounded because its numerator is a subset of its written denominator, but incorrectly normalized and lacking explicit tie precedence. The 100%-opportunity workload returning 50% is a direct failure. Finding 14.
- **Regret:** unique identities alone do not align event-time rate windows. The shipped alert is not bounded; use a matured eviction cohort. Finding 16.
- **Zero denominators:** `metrics-and-eviction.md:75` now explicitly supplies NaN/no-data for the main ratios. This is a real closure. Alert clamping is a policy to avoid undefined arithmetic, not the same no-data semantics and not a repair for an absent/mismatched cohort. Renewal per-pool aggregation is corrected at `:140`. The blanket claim at `:148` is not literal for the unaggregated storage ratio at `:143`; exporter label/replica identity still needs a contract, although I do not inflate that into another P1.

## Consistency search results

I searched the current normative documents, not revision logs or baselines, and inspected matches in context.

| Requested category | Result |
|---|---|
| Pool retention refreshed on merge | **No surviving contradiction found.** `architecture.md:170`, metrics `:155` correctly separate recipient merge retention from pool install demand. |
| Value-existence export dedupe | **No surviving stored-value-only skip rule found.** Corrected at `architecture.md:97,160,224`. |
| Renewal without a database write | **No surviving prohibition found.** `architecture.md:99`, ledger `:188-190` agree on the stamp. |
| p99-based breaker | **No surviving p99 trip rule found.** Renewal p99 is an SLO, not the object-store breaker. Stale install-failure wording remains at `architecture.md:215`, finding 19. |
| MinIO as production | **No surviving production-MinIO recommendation found.** Explicit current topology controls homelab-only diagram/failure examples; `latency-and-azure.md:55` scopes them. Deployment-specific endpoint/exporter/capability tests remain open. |
| Every blob service-hashed | **No surviving positive all-service-hashed requirement found.** I3, overview, verification, diagram and sampled reread budget are aligned. Engine import hashing is a different, valid assertion. Remaining error behavior/feedback is finding 10. |
| Thirty-percent gate | **Withdrawn at `architecture.md:264`.** It occurs there only as a withdrawn proposal. Dangling threshold/query references remain at `:19,197,318`, finding 19. |
| Removed publish pin | **Still contradictory:** `architecture.md:49,227`, finding 20. |
| Generation object layout | **Still contradictory:** `architecture.md:40,137,161-162`, finding 20. |
| Rewritten SLI population | **Still contradictory:** `architecture.md:260`, finding 19. |
| FQDN NetworkPolicy | **Still contradictory:** `architecture.md:336` versus `:86`/decision `:87`, finding 22. |

## Publication arithmetic and probe assessment

All arithmetic below was executed in Python. These are extrapolated scenarios using displayed design-cluster rates, not new cluster measurements or demonstrated 2 GB performance.

| Quantity | Result |
|---|---:|
| Decimal 2 GB PUT at 109.5 MB/s | 18.264840 s |
| Decimal 2 GB divided by 62.7 MB/s aggregate | 31.897927 s |
| Add 19.2 ms ack and 2.18 ms HEAD, no full reread | 18.286220–31.919307 s |
| 92.5 MiB/s expressed in decimal MB/s, used for full reread | conversion applied in calculation |
| Full 2 GB reread at that small-hot-read rate | 20.619985 s |
| Sequential PUT + reread + ack + HEAD | 38.906205–52.539292 s |
| Illustrative eight equal 2 GB writers sharing 62.7 MB/s aggregate | 255.183413 s each/wave completion under that fair-share assumption |

The revised table's 18.3/31.9 and 38.9–52.5 figures therefore follow from its displayed rates and rounding. The eight-large-object line illustrates why the aggregate scenario is **not a per-stream tail bound**; it is not a measured result or a prediction that the small-object aggregate persists at that size. The fresh GET/HEAD timing and samples remain exactly those supplied in the brief.

`do()` now measures through `getresponse()`. The actual delayed-header local test passed with status 200, a 0.120-second deliberate server delay, 0.130503-second header latency, and 0.130697-second total. This confirms the timing placement, not MinIO latency. Status/size/range/expiry/concurrent-status assertions described in `latency-and-azure.md:106` match the current code. The probe does not retain the source payload digest or HEAD/read back the concurrent objects; it must not be read as a stronger all-content integrity test. Its finite-to-end Range shape is consistent with the supplied engine behavior despite the colloquial “open-ended” label.

**Categorical claims:** neither universal publication inside thirty seconds nor universal impossibility follows from one small PUT and one hot-object read series. R3's explicit scenario limits and unmeasured end-to-end spike are appropriate; an export-ready blob at a stated sustained rate has the computed transfer time, conditionally. The unsampled path is the no-full-reread path; selecting an individual blob into the 1% sample adds its entire reread, not 1% of that blob's transfer to every publication.

The donor-dependent window covers discovery/export/compression/PUT and the evidence needed to trigger service continuation. Service verification can outlive the donor **once triggered**, but completion without the report is not yet handled, finding 9. Architecture/decision still call span-end-to-verified the loss bound even though graph publication has a separate commit/failure window. It should be measured to usable committed publication, as the revised metric intends (`metrics-and-eviction.md:84`). Thirty seconds is notice, not a measured node lifetime or a maximum duration of a progressing transfer.

Production remains Azure compute with external S3, not an extrapolation of homelab MinIO. The 100-parallel-2-GB read burst, endpoint capabilities, memory spike, discovery capture, and installation contract check remain explicit gates, not results fabricated by this review.

## Structural simplification I would choose

**Cut online per-blob deletion and generation reuse from v1.** Use immutable append-only generation keys, a hard admission/storage budget, and explicitly accepted fail-open misses when the budget stops new admission. Keep metadata pruning for memory. Perform byte reclamation only during an explicitly quiesced/offline pool-maintenance procedure with no surviving publishers or PUT authority, or defer it entirely during the bounded prototype.

This sacrifices storage efficiency and automatic reclamation; it does not pretend to provide a production retention solution. It removes the live publish-versus-delete race, graph-reference counter as a deletion authority, multi-chain drop/GC locking, replacement-generation cleanup ambiguity, and much of the cross-store atomicity burden while Gate B establishes whether Tier 1 is worth further engineering. Current quota expectations must be replaced honestly if this cut is adopted. Do not merely disable the DELETE RPC while keeping an eviction controller that claims frees already occurred.

If online GC is mandatory for the first release, the structural alternative is committed publication/edge records in one authoritative store with dagql treated as a derived index, rather than pretending its in-memory mutations share PostgreSQL rollback. That is a substantive protocol redesign, not another `WHERE`-clause patch.

## What genuinely closed, what is new, and release boundary

Genuine R3 closures include the pending/verified/deleting allocation branch in its normal case, mint correlation and generation return, removal of the impossible second upload, explicit internal waiter budget, local-hit lazy-evidence test, same-entry interval union/causal-parent rule, pure-install gross/net decomposition, install-only pool retention, availability-based scheduling, topology separation, circular-gate removal, honest sampled-content invariant, lowest-first ordering, probe header timing, and displayed-rate publication arithmetic. They should not be reopened merely because the design deliberately has no service implementation.

New or newly executable R3 failures include graph/database rollback and checkpoint visibility (1), reference-counter lifecycle/idempotency (2), inadequate restore binding under the new gate (3), failed-state reserve bypass (4), unconditional mirror transitions after a zero-row source update (5), incomplete epoch enforcement (6), mint/publish lock ordering and renewal coupling (7), nonexistent completeness attribute/receiver revision logic (11), descendant-inclusive attribution (12), blob-level donor-cost granularity (13), reintroduced ceiling denominator duplication (14), fallback-wait meaning invented from miss flags (15), event-time regret presented as a closed cohort (16), pre-mark projection omission (17), and unachievable exact-target/discrete-group eviction rules (18). Findings 8–10 and 19–23 include material previously reported gaps or surviving clauses rather than being labelled new just because the heading changed.

The highest-priority design corrections are findings **1, 5, and 2**. A design is not ready for implementation merely because an ordinary transaction wins a short race; rollback, no-op return values, restore age, graph pruning, and every state-transition outcome must preserve the same authoritative facts. The current specification does not do that.

### Additional primary documentation checked

- **[PG-L] PostgreSQL official manual, §13.3, Explicit Locking**, especially row-level locks and deadlocks: transaction lifetime and consistent object-lock acquisition order.
- **[PG-U] PostgreSQL official UPDATE reference, Outputs and RETURNING:** a zero affected-row count is not an error; returned rows identify actual updates.
- **[PG-T] PostgreSQL official manual, §9.9.5, Current Date/Time:** `now()` is transaction-start time, unlike `clock_timestamp()`.

Official documentation search results supplied the relevant passages. The generic page-extraction tool failed because the configured native backend is search-only; fallback search providers returned the primary PostgreSQL pages. No unavailable extraction or PostgreSQL stress test is represented as successful. These references support the database premises of the interleavings, not execution of the proposed service.
