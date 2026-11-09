> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

STOP

# Round-5 adversarial review of Revision R4

**23 findings: 1 P0, 15 P1, 7 P2.**

The `M`/`LiveBlobs()` change is a substantial improvement. I could not break ordinary publish versus a **definitively completed** mark in one authoritative process. Removing counters, the head mirror, and database locks across `SetStoredPart` genuinely closes the original rollback/count-drift/cross-resource-lock defects. It does not establish the claimed epoch fence during promotion, settle an ambiguous database mark before releasing `M`, make the failed-reserve branch executable, or provide a per-part eviction operation. Several metric and delayed-free findings from round 4 remain open despite the disposition claims.

Stop approval of the Tier-1 specification as written. Tier 2 and the explicitly bounded discovery, contract, memory, publication, and store-capability experiments may proceed. An unrun experiment is not itself a new finding. No implementation is requested or presumed to exist.

## Scope and evidence

Paths below are relative to `<DESIGN_WORKSPACE>`. I read the brief and the requested documents in order, including the entire 1,258-line cumulative `review/r4.diff`, both complete round-4 reviews, and the round-4 baseline. The eight baseline files were read and compared programmatically with their corresponding current files: all eight were byte-identical. The cumulative diff is against baseline-r2, not a clean R3-to-R4 patch; it is not evidence that every displayed change was introduced this round. The claim that both previous reviewers proposed this exact simplification is overstated: Claude proposed M (`review/claude-r4.md:149-160`), whereas GPT recommended deferring online deletion or a derived graph (`review/gpt-r4.md:415-421`). That provenance difference does not decide whether M itself works.

`src:` denotes `<HISTORICAL_SCRATCH>/dagger-src`, whose HEAD was read as `7e9eaacc376e5c3bed3efdca9e1ba750bc8d9233`. Targeted tag comparisons found no differences for the inspected stored-parts, stored-queries, telemetry-attribute, cache, checkpoint-worker, and remote-cache files. A broader dagql comparison has unrelated differences; I do not claim the entire checkout equals beta.25.

I inspected the actual `LiveBlobs`, `DropStoredParts`, `SetStoredPart`, lazy-span emitter, part-address types, and checkpoint snapshot code. Executed checks used in-memory SQLite for the literal reserve/verify/quarantine predicates and Python for explicit interleavings, interval unions, eviction/quota examples, and arithmetic. These are predicate and abstract-model witnesses, **not executions of the proposed service or PostgreSQL concurrency tests**. The PostgreSQL scheduling arguments are reasoned from its primary documentation. No cluster probe, real-engine interoperability run, production burst, or live PostgreSQL mutation was performed. The supplied fresh MinIO measurements are accepted facts.

Only this review file was created by this reviewer. No design, probe, manifest, baseline, source, or skill was changed by this reviewer. Final validation detected concurrent edits to the root `ledger.md` after the initial byte-identical comparison. This report therefore freezes its subject at `review/baseline-r4/`, not those subsequent edits; design-document file-and-line references below name that frozen snapshot explicitly. I did not review or overwrite another actor's in-progress revision.

## Sequential findings

### Finding 1 [P0] — The epoch `EXISTS` predicate is a snapshot test, not a promotion fence

**References:** `review/baseline-r4/ledger.md:81-90,189-216,243-245,271`; `review/baseline-r4/architecture.md:53-54,341-342`; PostgreSQL primary references [PG-I], [PG-L], [PG-T].

The mark statement reads `pool_leader` without locking it or establishing a dependency that prevents promotion while that statement remains executable. Its statement snapshot can retain the old epoch. `now()` also does not advance during a transaction. Repeating this predicate in every write is not equivalent to fencing the lifetime of those writes.

Concrete allowed history:

1. A owns epoch 1. Its graph has pruned P; an older checkpoint still contains P→D. D is verified with an old mint stamp.
2. Another A-side transaction holds D's row lock. A takes `M_A`, snapshots D as non-live, and submits its mark. The statement starts while epoch 1 and its lease are valid, then waits for D.
3. A pauses; the database statement remains active. B takes epoch 2 after expiry and restores the checkpoint. D is still verified, so P survives restore in B's authoritative graph.
4. The row-lock holder rolls back without changing D. A's statement resumes against its original snapshot, marks D deleting and queues g1. Its ordinary `EXISTS` does not acquire B's new authority.
5. A or B drains that queue and destroys D/g1, although B's graph names it.

The two processes have different mutexes. A paused process cannot execute its half-TTL self-termination code, and mark has no stated server-side lifetime bound. This is a concrete failure of the single-authoritative-graph premise at handoff, not a failure of digest generation keys.

**Required change:** specify a database-operation/promotion serialization boundary and bounded outstanding-operation drainage. A new epoch must not start authoritative restore/service while an old-epoch mutation can still commit. Keep database locks out of graph calls; an epoch-namespaced checkpoint key alone does not fence marks.

### Finding 2 [P1] — An ambiguous mark can outlive `M` even without failover

**References:** `review/baseline-r4/ledger.md:150-164,189-208`; `review/baseline-r4/architecture.md:253`; primary references [PG-I], [PG-C].

R4 removes the old failed-publish-COMMIT problem, but does not define the terminal-outcome rule for the database mutation performed while holding `M`.

History: mark obtains `M`, submits its CTE, and encounters a client connection/cancellation failure before knowing whether the server transaction is terminal. If the error path unlocks `M`, a publication on another healthy connection can read the old committed verified state and insert P. The still-active mark can then commit deleting+queue. The eventual DELETE removes P's blob. A nonlocking state read need not wait for an uncommitted update. A dispatched cancellation is not proof of cancellation.

This is a permitted failure path in the specification, not an assertion about an unprovided Go driver's implementation. There is no requirement to retain serialization, close the pool, or prove the old database operation dead before resuming publication. “Postgres down ⇒ no publish” does not specify how to settle a single uncertain connection or safely reopen the pool.

**Required change:** keep publication closed until the mark is definitively committed or aborted, or enter an explicit pool-failed state and resolve outstanding mutations before reopening. An ambiguous response cannot be treated as rollback or as an ordinary retryable scan error.

### Finding 3 [P1] — Successful failed-object destruction still has no executable allocation/cleanup transition

**References:** `review/baseline-r4/ledger.md:69,75-77,100-108,123-134,205-210,250-256`; `review/baseline-r4/revision-r4.md:21-23,39`.

`failed` takes the row lock, destroys its current object, and says “continue.” The only allocation UPDATE requires `state='absent'`. No intervening failed→absent statement is specified there, and no failed→pending UPDATE implements the transition table. Re-reading three times just sees failed again. Executing that predicate after a successful destruction returned **zero rows**, leaving `(g1,failed)`.

The asynchronous route is also incomplete: the queue worker's only displayed cleanup transition requires `state='deleting'`. A failed row therefore cannot reach absent through that statement, although the transition table and scanner promise failed→absent cleanup. Draining/removing its queue without changing failed either strands it or leaves inconsistent charging evidence.

This is not a request for production SQL to exist. The SQL that is given does not implement its own state table.

**Required change:** specify the guarded successful-destruction transition for failed in both paths, then allocate exactly once with a returned generation. Preserve generation-qualified late-delete intent when advancing the head, and define no-allocation/error outcomes before commit.

### Finding 4 [P1] — Verify is epoch-fenced, not owner- or lease-fenced, and cannot adopt inherited pending uploads

**References:** `review/baseline-r4/ledger.md:70-72,110-118,136-146,250-254`; `review/baseline-r4/latency-and-azure.md:46-47`.

The verify predicate has neither `owner_export=$export` nor an unexpired lease condition. Same-epoch takeover changes the export but not the epoch or generation. A late E1 verify can therefore succeed after E2 took over; a verify after lease expiry also succeeds until another transition happens. The executed written predicate returned one row for an expired lease owned by E2 when invoked as the old export in the same epoch. This contradicts line 146's categorical refusal claim. Identical bytes make duplicate PUTs harmless at the byte layer; they do not enforce the advertised owner transition protocol or protect a new owner's pending work from stale failure events.

Failover exposes the inverse problem. B sees an inherited pending row with `actor_epoch=A`, HEADs a completed object on the dead-owner path, and calls verify “as the new owner.” Verify requires `actor_epoch=B`; the only displayed ownership-adoption UPDATE requires an **expired** lease. An unexpired inherited upload cannot be salvaged immediately as promised. The hourly pending scanner has the same mismatch.

**Required change:** define atomic owner/export adoption for both expired and known-dead owners, and fence verify/failure by that ownership identity and an explicit expiry policy. Do not rely on the new process's epoch magically changing a pending row.

### Finding 5 [P1] — New synchronous reserve I/O conflicts with the five-second single continuation

**References:** `review/baseline-r4/ledger.md:105-112`; `review/baseline-r4/architecture.md:146,225-227,257,348`; `review/baseline-r4/latency-and-azure.md:35-38,47`.

Reserve now performs synchronous DELETE for failed and HEAD plus verification for a dead pending owner while holding the row lock, before the URL map is ready. A sampled 2 GB salvage involves roughly a full reread; even the supplied small-hot-read extrapolation is about 21 seconds. A slow DELETE alone can exceed five seconds. The cold flow nevertheless requires one upload continuation within five seconds, always, and explicitly postponed `CommitValueMerge` to protect that deadline.

The watchdog is named, but there is no branch saying that these newly blocking reserve paths yield/decline before the watchdog, no total reservation budget, and no rule preventing a late-returning reservation from issuing a second continuation or capabilities after an empty continuation has consumed the provider.

**Required change:** make pre-continuation reservation bounded and define the terminal handoff to the watchdog. Perform long salvage/cleanup asynchronously, or decline the digest for this export. Specify what happens to allocations that finish after the one continuation was sent. A measurement of a fast HEAD does not bound a hash reread or a hung delete.

### Finding 6 [P1] — Event-driven publication has a lost-wakeup window

**References:** `review/baseline-r4/ledger.md:110-112,167-173`; `review/baseline-r4/architecture.md:226-227`.

The new `waiting_chains` registration and verification notifications are not serialized or given a register-then-recheck rule.

History: C's upload completion checks partner D and sees pending; D becomes verified and its event handler enumerates no waiters; C is then registered in `waiting_chains`; no more verify occurs for D. C is all-verified but remains unpublished indefinitely. An executed abstract schedule produced exactly that state: verified D, zero notified chains, C still waiting. This is not fixed by removing the old four-second waiter.

There is a second readiness boundary: the continuation can finish and verify events can run before `CommitValueMerge` completes. Architecture says publication waits for that commit, but §5.2 lists no merge-completion trigger to retry a chain that became all-verified earlier.

**Required change:** specify atomic waiter registration plus a final state/readiness recheck, notify after durable verification, and include successful graph-merge completion as a readiness transition. No blocking waiter or second upload message is needed.

### Finding 7 [P1] — Two deletes twenty minutes apart do not close the late-PUT finding

**References:** `review/baseline-r4/ledger.md:43-48,205-210,250-256`; `review/baseline-r4/architecture.md:132,146,173`; `review/baseline-r4/revision-r4.md:23,59`; primary reference [S3-E].

There is no maximum duration for a progressing PUT. A lease is metadata authority, URL expiry is request admission, and the one-minute engine bound is idle time. None establishes a twenty-minute transfer tail.

Example: an upload starts at minute 0 under its five-minute capability, advances continuously at about 0.694 MB/s, and completes a decimal 2 GB object at minute 48. At minute 10 the lease fails; the first DELETE succeeds or returns 404. At minute 30 the second DELETE also succeeds/404 and the queue is discarded. Completion at minute 48 recreates g1 after every specified watcher stopped. A third finite attempt only moves the counterexample. The measured 258-second contention scenario is not an upper bound on production uploads.

A **new** request at minute 40 using that expired capability should be rejected; the counterexample is an already-admitted request completing after minute 40.

The weekly LIST only “finds objects missing from the ledger.” A retained digest row for current g2/absent does not by itself identify retired g1, and no deletion/retry action is defined.

**Required change:** retain cleanup intent until a real, enforced maximum transfer lifetime has drained, or implement recurring generation-aware orphan reconciliation. See the listing assessment below. Do not declare the original finding closed by choosing another unproved fixed delay.

### Finding 8 [P1] — One head row cannot charge physically recreated retired generations

**References:** `review/baseline-r4/ledger.md:27-48,205-210,258-262`; `review/baseline-r4/metrics-and-eviction.md:108-110,185-191`.

Even if a late PUT finishes **before** the second delete, quota undercharges it. First cleanup sets g1 absent and clears its size. Reserve advances the same row to g2 and charges g2. A late g1 PUT then recreates a second physical object. The queue stores no size, and the sole blob row still describes only g2. Until the next delete, two objects occupy storage while only one is charged. After finding 7's tail, the undercharge can be indefinite.

The Python witness used two equal-sized objects: physical bytes 4 units, ledger charge 2. No state-based SUM over the one current row can recover the missing retired-generation bytes. Thus “never under-charges” and “one row holds one generation's worth” do not prove the advertised physical storage budget.

**Required change:** distinguish current-head reservations from generation-qualified cleanup liabilities, including their charged sizes and confirmation/drainage policy. It is acceptable to expose an explicitly estimated liability; it is not acceptable to call physically recreated old objects uncharged absent rows while claiming conservative accounting.

### Finding 9 [P1] — Per-part eviction requires the API that the ledger explicitly says does not exist

**References:** `review/baseline-r4/metrics-and-eviction.md:175-183`; `review/baseline-r4/ledger.md:218-227,276`; `src:dagql/cache_stored_parts.go:162-191`.

The corrected selection algorithm drops one chosen part at a time and recomputes bytes. The only chosen removal primitive drops **all parts naming a digest**. These are not equivalent operations.

Example: valuable H has chain `[A,B]`, cold C has `[A]`. C sorts first. There is no digest in C that can be passed to `DropStoredParts` without also removing H. Dropping A evicts both, including B's otherwise valuable bytes; recomputing freed bytes **after** the drop cannot undo that collateral loss or enforce H's retention eligibility. The executed incidence example removed both hot and cold parts. If a cold part has a uniquely identifying blob X, dropping X can work; the design cannot assume every selected part has one.

Accepting coarse drops for restore or corrupt/missing bytes does not make them an implementation of a value-aware per-part policy. R4's removal of whole-component closure is useful, but its new actuator is still undefined.

**Required change:** select and score actual digest-drop blast radii, accepting the policy consequences, or explicitly require a compatible graph API change. Do not reintroduce reference counters merely to approximate a removal operation.

### Finding 10 [P1] — Pre-mark retirement projections are still missing, so grace-time over-eviction survives unchanged

**References:** `review/baseline-r4/metrics-and-eviction.md:183,185-191`; `review/baseline-r4/ledger.md:194-197,226-227,260`; `review/gpt-r4.md:214-224`.

R4 removed the duplicate projection table but did not answer the original finding's **pre-mark** half. After a graph drop, recently minted bytes remain verified during G, plus sweep scheduling. `reclaim_pending` includes only deleting rows, so it is zero during that interval.

With charged=90 and low=70, the first pass drops parts representing 20 units of eventual frees. A URL was recently minted, so those blobs cannot yet become deleting. The next controller pass still observes 90−0 and drops another 20. Eventual use is 50, not 70. “Do not evict again while a delete is in flight” cannot cover a period before a delete is eligible. Recomputing marginal freed bytes within one pass does not persist intent across ticks or restarts.

**Required change:** represent/reconstruct graph-retirement liabilities before mark and reconcile them if parts are republished, or define an explicit controller wait/recalculation policy that survives restart. Removing post-mark cancellation is correct; it does not eliminate the distinct pre-mark state where a reference really can reappear.

### Finding 11 [P1] — The entry boundary still leaves enclosing intervals inclusive, and the catalogue still sums them

**References:** `review/baseline-r4/metrics-and-eviction.md:41-52,59-64,73,180`; `src:dagql/otelprof_lazy.go:109-117,145-199,249-285`; `src:engine/telemetryattrs/attrs.go:429-467,731-788`.

Stopping traversal at a different result ID prevents adding B's span directly to A. It does not subtract B's interval from A's own enclosing evaluation span. For A=0–100 and B=10–90, union(A's own span)=100 and union(B)=80 even after that boundary. The executed witness still totals **180**. Line 50 says never to sum these inclusive quantities across entries; lines 59–64 immediately define pool counters that do so, and donor/benefit sums inherit the same issue.

Likewise, an `executed` resolver or a `wcprof.op.kind=lazy` span may contain waiting on dependencies. Tagging the enclosing span as evaluation does not remove its internal waits. The source does provide the real lazy discriminator and wait-link interval attributes; the design's unnamed “lazy-evaluation attribute” need not be invented, but its proposed filter alone does not implement the claimed exclusion.

**Required change:** choose disjoint attributable work and specify nested/wait interval subtraction, or retain inclusive latency only as a diagnostic and stop feeding its sum into claimed independent savings/cost totals. The uncertainty exclusion is a real improvement and should remain.

### Finding 12 [P1] — `part_cost` still lacks a defined key/join and allocation from multi-part entry work

**References:** `review/baseline-r4/ledger.md:54-59`; `review/baseline-r4/metrics-and-eviction.md:17-23,73-79,180,199-205`; `src:dagql/cache_transfer_types.go:25-28,52-56`; `src:engine/telemetryattrs/attrs.go:758-788`.

Moving cost off blob generations and writing at finalization are correct. However, `part_digest` has no derivation or receiver-install join in the design. The actual installation attribute names a **part address**, with an output path and part key, not a globally unique part-content digest. Common addresses repeat across unrelated entries. The design does not specify how entry/recipe identity, replacement revision, relocation, and chain identity produce the cost key seen by a recipient.

The assigned value is still the union cost of “the entry that produced it.” One entry can expose multiple snapshot parts. Copying its whole entry cost into both part rows credits both when installed; a mixed entry can also assign Q's actual evaluation to P unless production ownership is explicitly partitioned. Saying “never sum layers” fixes a different dimension, not the multi-part counterfactual.

A scalar seconds+sample-count row also does not by itself support an exact streaming median; retained samples or an explicitly approximate sketch would need a home.

**Required change:** define the canonical part-cost identity and donor/receiver mapping, allocate or mark unknown the work attributable to each produced part, and specify estimator storage. Do not assume an unprovided `part_digest` telemetry field.

### Finding 13 [P1] — Removing opportunity work from `recompute_remaining` inflates value and makes the new ceiling a different quantity

**References:** `review/baseline-r4/metrics-and-eviction.md:59-67,79,85-88`; `review/baseline-r4/architecture.md:197,260,264`.

The emitted remaining series first means all unavoided recompute, including mixed residuals. Line 87 then says it **excludes** would-hit/lost-keying entries, and the same remaining series is used by the value ratio. Those entries were actually computed; an earlier usable match did not avoid their current cost.

Executed example: net saved=10 seconds, actual recompute=90, all 90 classified would-hit. The exclusion yields remaining=0 and value **100%**, instead of 10/(10+90)=**10%**. Reporting opportunity separately does not justify removing real work from the denominator. R3's double addition should have been corrected without deleting the subset itself from R.

The new saved-gross/(saved-gross+remaining) is also not an attainable opportunity ceiling: before any service exists, saved-gross is zero even if every operation is an otherwise valid would-hit. The label cannot supply Gate A's opportunity estimate.

**Required change:** retain all actual unavoided work in R, report disjoint opportunity sets separately, and distinguish realized gross coverage from an opportunity/attainability estimate. Preserve mixed-entry residuals in every copy of R's definition.

### Finding 14 [P1] — Regret is still a ratio of different event-time populations, not a closed cohort

**References:** `review/baseline-r4/metrics-and-eviction.md:123,161,193-195`; `review/baseline-r4/ledger.md:27-58`; `review/gpt-r4.md:202-212`.

The claimed fix changes `rate` to `increase` but retains independent rolling event windows. Ten items evicted at hour 0 and refetched at hour 23 remain in the numerator at hour 25, while those evictions are outside its last-24-hour denominator. Add one new eviction and the ratio is **10/1=1000%**. Unique membership and a generation-qualified join do not align the windows.

The durable cohort is also not represented: the head row has no eviction timestamp/identity, and the queue may disappear twenty minutes after cleanup, long before the 24-hour refetch attribution horizon. No other cohort store is specified.

**Required change:** expose/query outcomes for an eviction-time cohort with a maturation horizon and eligible-member count, persisting the old generation association, or explicitly label the existing expression an unbounded event-volume ratio. This is the same unresolved GPT R4 finding 16, not a new property introduced by `increase`.

### Finding 15 [P1] — A per-row 500 ms lock timeout does not bound the chain renewal to two seconds

**References:** `review/baseline-r4/ledger.md:175-185`; `review/baseline-r4/architecture.md:99,233`; `review/baseline-r4/metrics-and-eviction.md:97-98`.

Single-row autocommit removes the former multi-row deadlock and graph-coupled lock starvation. But line 185 treats it as a guarantee of the complete chain's deadline. Twenty successful row waits of 400 ms take **eight seconds**, although none reaches the 500 ms timeout. Connection acquisition, network/statement execution, signing, and response dispatch are outside a lock timeout as well.

The quoted 8–20 ms is an unmeasured fast-path estimate, not a worst-case policy. No total request deadline, remaining-budget check, early-unavailable branch, or bounded database statement duration is specified. A single-row lock fix does not replace the protocol's two-second end-to-end requirement.

**Required change:** apply a total renewal budget with time reserved for the reply, terminate/decline the remaining chain before it expires, and distinguish lock, statement, connection, and whole-request limits. A partial URL stamp may conservatively remain committed; it must not cause a late or second reply.

### Finding 16 [P1] — Quarantine ignores the sampled generation and grants delete intent independently of transition success

**References:** `review/baseline-r4/ledger.md:74-76,81-90,205-208,229-239`; `review/baseline-r4/architecture.md:150`.

A mismatch observation belongs to the object generation that was read. The quarantine UPDATE filters only pool, digest, and verified state, not `gen=$g`. A delayed mismatch from g1 can therefore quarantine a newly verified correct g2. Executing the written predicate with current g2 and an old g1 observation changed g2 to quarantined.

The following queue INSERT is unconditional, unlike GC's `marked` CTE. If the fenced UPDATE changes zero rows, the INSERT can still create delete work. The queue drainer does not require the current state to authorize the external DELETE; it only guards the later absent transition. The SQLite witness produced zero stale-epoch updates followed by one queued target. A queue row is not an epoch predicate.

The shown quarantine path also never calls the architecture's promised drop, enqueues while still quarantined although the transition table reserves quarantined→deleting for operator repair, and leaves that row outside the worker's deleting-only completion transition. Immediate unavailability of known bad bytes is reasonable; these different operations need one coherent state path.

**Required change:** bind observations and state transitions to the sampled generation, derive queue rows from successfully authorized transitions, and specify drop/repair/delete completion ordering. Do not let a stale detector or zero-row UPDATE enqueue authority to destroy a current key.

### Finding 17 [P2] — The ten-percent overshoot cap still conflicts with indivisible objects and unspecified candidate progress

**References:** `review/baseline-r4/metrics-and-eviction.md:179-183,190,223`.

Per-part dropping cannot split a blob's bytes. With charged=90, low=70, and the only removable object/group worth 30 units, reaching low requires ending at 60. Overshoot is 10, while ten percent of the 20-unit target is 2. The algorithm must either stop above low, violate its cap, or fail to make progress. R4 removed the old split-largest-part instruction but did not resolve this discrete case.

“Lowest score” also alternates between groups and individual parts, and the 2,000-candidate cap has no coverage/cursor/fairness rule. It is a bounded search budget, not a guarantee of eventual reclamation.

**Required change:** state a stop-above-low or indivisible-overshoot policy, define which score orders actual actions, and specify what happens when the bounded search finds no eligible progress. This is separate from finding 9's missing actuator.

### Finding 18 [P2] — The 24-hour minimum retention has no enforcing predicate

**References:** `review/baseline-r4/ledger.md:194-197,260-262`; `review/baseline-r4/metrics-and-eviction.md:169-183,211-228`; `review/baseline-r4/architecture.md:174`.

The ledger says `min_retention` 24 h is respected by `last_url_mint_at`, but its only deletion-age comparison is G=2 h. The eviction eligibility rules have no creation/install-age guard and the revised config no longer includes `min_retention`. A merely old low-score part can be dropped without demonstrating 24 hours of protection; its never-served bytes can then become deletable at two hours.

A mint timestamp is also not the same fact as creation time or genuine last install. Which event starts the 24-hour promise must be chosen explicitly, including collateral members of a coarse drop.

**Required change:** define and apply one retention eligibility clock, or remove the promise. Do not infer a 24-hour guard from a two-hour URL grace predicate.

### Finding 19 [P2] — The schema cannot persist the recovery ages and successful-attempt spacing that the scanner requires

**References:** `review/baseline-r4/ledger.md:27-48,207-210,243-254`; `review/baseline-r4/metrics-and-eviction.md:225-227`.

The blob table has no deleting-transition timestamp for “deleting older than one hour.” The queue has `queued_at` and an attempt count, but no first-success/last-success timestamp for “second successful delete at least twenty minutes after the first.” If the first delete is delayed for an hour, twenty minutes after enqueue is not twenty minutes after success. Pending age can be inferred from a fixed lease only until takeovers alter it; the intended age semantics are not stated.

The documentation also names 5-minute queue recovery, a one-hour rebuild criterion, and a 900-second `delete_recovery_after` setting without assigning distinct roles. These are not enough durable facts to recover exact spacing after restart.

**Required change:** specify persistent transition/attempt times and one restart-safe scheduling rule. Retry counters alone are not elapsed-time evidence.

### Finding 20 [P2] — The two-phase slow-scan fallback repeats the blocking scan rather than bounding it

**References:** `review/baseline-r4/ledger.md:214,279`; `review/baseline-r4/revision-r4.md:92`; `src:dagql/cache_stored_queries.go:159-174`; primary reference [GO-RW].

As literally written, snapshot under `M`, then mark under `M` **after re-snapshot**, is safe against the ordinary intervening-publish race if the second snapshot is the one actually used. I could not break that literal variant. It still performs the slow `LiveBlobs()` scan while holding `M`, however, and now may do it twice. It is not a mitigation for a multi-second scan. Dropping the second scan without another validation mechanism would reintroduce stale-candidate deletion.

`LiveBlobs()` can also wait behind a queued egraph writer; Go's RWMutex does not admit arbitrary new readers ahead of a waiting writer. Its cost is scan plus graph-lock wait, sorting, and database mark duration, not only the advertised hourly in-memory traversal.

**Required change:** retain the scan-size gate, but either accept bounded-pool blocking honestly or specify a versioned/incremental candidate-validation scheme. Do not present a second lock-held full scan as the latency fallback.

### Finding 21 [P2] — The consistency sweep missed operative R3 clauses and new metric contradictions

**References:** `review/baseline-r4/architecture.md:99,161,172,197,215,240,251,260`; `review/baseline-r4/decision.md:42-44`; `review/baseline-r4/ledger.md:62,167-173`; `review/baseline-r4/metrics-and-eviction.md:60,79,85`; `review/baseline-r4/latency-and-azure.md:47`; `review/baseline-r4/revision-r4.md:53-58,66-72`.

Surviving discrepancies, not extra individually counted findings:

- `review/baseline-r4/architecture.md:172` still invokes `last_ref_at`, a removed field/fence. It also treats unpublished verified bytes as though their pending lease still controls cleanup.
- `review/baseline-r4/architecture.md:240` and `review/baseline-r4/decision.md:42` still wait ten minutes for known-dead owners, contrary to the new immediate HEAD/verify/fail rule.
- `review/baseline-r4/architecture.md:251` still lists `demand p95`; `:215` still calls the breaker an install-failure breaker after the normative document disclaims that population.
- `review/baseline-r4/architecture.md:260` and `review/baseline-r4/metrics-and-eviction.md:85` describe remaining recompute as entries with no install, omitting the mixed residual explicitly restored at metrics `:60,79`.
- `review/baseline-r4/architecture.md:99` budgets “one” committed stamp and points to ledger §7, while §6 now does one stamp **per digest**.
- `review/baseline-r4/architecture.md:161` still describes INSERT itself creating pending/new gen, although the new INSERT creates absent and the locked UPDATE allocates.
- `review/baseline-r4/architecture.md:197` still labels earlier exact matches as perfect-cache hits without its normative earlier-verified-part requirement, and gives pairing matches no explicit exact-match precedence/structural-equality rule.
- `review/baseline-r4/decision.md:44` and `review/baseline-r4/architecture.md:242` still end the exposure SLI at verified, rather than usable successful publication.

The old publication pins, `graph_refs`/head-mirror requirements, pre-generation object layout, 30% gate, ordinary FQDN NetworkPolicy, all-service-hashed guarantee, fixed waiter, and legal `deleted` state are genuinely removed or occur only in explicit historical negations. Admission “pinned,” engine export pins, and version pinning are different concepts and are not false-positive surviving publish pins. No new production-MinIO recommendation was found; the homelab diagram does not override the explicit external-S3 production constraint.

**Required change:** align the operative copies with the normative state/metric model, not merely the previously reported text locations.

### Finding 22 [P2] — Publication needs explicit missing-row and `SetStoredPart` refusal outcomes

**References:** `review/baseline-r4/ledger.md:152-165,169-171`; `review/baseline-r4/architecture.md:227-228`; `src:dagql/cache_stored_parts.go:45-89`.

“Require every returned row verified” must include equality between the returned distinct-digest set and the complete requested set. A SELECT that returns only verified A for requested `[A,B]` cannot establish B. A partial/denied reservation or an unreserved chain must not pass a vacuous/all-returned-rows check. The surrounding all-digests prose expresses the right intention, so I treat the missing executable equality as a specification precision issue rather than assuming the author intends subset publication.

`SetStoredPart` also returns `(bool,error)` and can return `false,nil` without installing a part. R4's commit-point/SLI wording treats merely returning from the call as publication. The old counter leak is gone, but refusal must remain unpublished, with its object eligible for later non-live reclamation and its publication metric not counted as success.

**Required change:** state exact set equality and successful `stored==true` as publication success, with error/refusal/retry behavior. These are API outcomes, not a request for implementation code.

### Finding 23 [P2] — The displayed PUT duration and acknowledgement arithmetic are internally inconsistent

**References:** `review/baseline-r4/latency-and-azure.md:15-21,31-39`; `review/baseline-r4/s3_latency_probe.py:88-98,112-116`.

The accepted measurements are body=36.0 ms, response=19.3 ms, end-to-end rate=72.4 MiB/s=75.9 MB/s. The table's **57.7 ms** does not match them: body+response is **55.3 ms**. A literal 57.7 ms for 4 MiB implies about **69.32 MiB/s / 72.69 MB/s**, not 72.4 MiB/s. This is a table-duration defect, not a reason to replace the supplied measured end-to-end rate with an invented rerun.

The publication table then adds an acknowledgement to a PUT duration already extrapolated from an end-to-end rate. It is small at this sample size but is the wrong decomposition. An individual blob selected into the 1% hash sample incurs a full reread; the no-reread “sampled fast path” label should mean the 99% unsampled path, not partial hashing of every blob.

**Required change:** preserve the supplied measured rate and its uncertainty, correct the duration label, and avoid adding response time twice. The approximately 26.4-second single-stream, 258-second fair-share eight-stream, and 47-second full-reread scenarios remain the accepted conditional extrapolations, not measured 2 GB bounds.

## Direct answers to the brief

### Structural symptoms and the ordinary `M` proof

| Attack / symptom | Result |
|---|---|
| Old publish transaction rolled back or COMMIT unknown after graph insertion | **Original defect closed:** `review/baseline-r4/ledger.md:150-165` has no database transaction around the graph write and no reference stamp to roll back. A confirmed state read plus graph insertion is serialized against a confirmed mark. The new mark-outcome defect is finding 2, not the old publish COMMIT. |
| Cross-resource `egraphMu`/Postgres lock deadlock | **Original cycle closed under the stated lock contract:** `review/baseline-r4/ledger.md:162` forbids a DB lock across `SetStoredPart` and says graph-lock holders never acquire Postgres locks. I could not construct that former cycle while respecting those rules. New reserve callbacks must still be dispatched only after releasing their DB row lock; otherwise a callback trying M while GC holds M waiting on that row creates a separate inversion. The design should make this global order explicit. |
| Former publish-induced mint starvation | **Closed for that cause:** publication no longer holds the blob rows while waiting for the graph. Sequential autocommit mint also removes the old multi-row mint cycle. The whole-chain deadline guarantee remains open, finding 15. |
| `graph_refs` liveness/decrement and restore quarantine | **Closed:** the counter and mirror are removed. `LiveBlobs()` recomputes actual stored-part incidence; native prune, replacement, coarse drop, duplicate/no-op Set calls no longer require external decrement accounting. Restore does not quarantine for checkpoint/ledger accounting drift. |
| Publish first, then mark | Under M, the inserted part appears in the subsequent `LiveBlobs()` snapshot, so mark retains its digests. Executed abstract model preserved this invariant. |
| Mark first, then publish while DELETE runs outside M | A definitively committed mark leaves deleting. Publication reads deleting and refuses; generation-qualified DELETE remains safe for later generations. Executed abstract model preserved this invariant. |
| Writer waits for egraphMu while GC wants `LiveBlobs()` | Publication holding M prevents GC from acquiring M first. An unrelated graph writer can delay GC's RLock, but nothing in the stated graph-lock contract needs M/DB in reverse. I could not build a wedge from the mere queued writer. This is latency, not a newly invented deadlock. |
| Slow-scan two-phase variant | Safe **if** re-snapshot+mark remain one M hold using the fresh snapshot; not an effective blocking-time mitigation, finding 20. Without revalidation, publish between phases can invalidate the candidate set. |
| Failover / deposed writer | Not proved by process-local M plus snapshot EXISTS; finding 1 gives the interleaving. The existing rule forbidding stale checkpoints is useful but is not a mutation/drainage fence. |

### Every reserve branch

| State under the row lock | Intended outcome and adversarial result |
|---|---|
| absent, including INSERT-created absent | Epoch-authorized UPDATE increments gen once and records declared size/lease/owner. A second locked reserver then sees pending. If allocation returns zero, no capability may be issued. INSERT itself still lacks an explicit guarded INSERT-SELECT form; do not interpret the prose epoch convention as literal valid SQL for all INSERTs. |
| failed | Synchronous destruction is a genuine new guard against allocation over known current bytes. The state still remains failed for the absent-only allocation predicate, and the queue cleanup has the same missing branch: finding 3. Late recreation remains findings 7–8. |
| pending, live owner, unexpired | Omit the digest and return no URL/no blocking waiter. This is the correct single-continuation shape. Completed partner verification must reliably wake its chain: finding 6. |
| pending, dead owner, unexpired | Immediate HEAD/verify-or-fail is now expressly intended. It cannot adopt an old epoch with the written verify predicate, and expensive verification conflicts with pre-continuation timing: findings 4–5. “No event for that export” also needs a defined timeout, not silence alone proving death. |
| pending, expired | Same-gen takeover UPDATE is present. It does not allocate g+1, so duplicate identical writes remain atomically harmless at the object layer. It needs the owner/lease completion rules in finding 4; sweeping expiry into failed and takeover must be explicit mutually exclusive outcomes under the row lock. |
| verified | Commit, omit PUT, then conditionally stamp/serve. If GC wins before that stamp, no URL is legal; return unavailable/defer rather than trusting the earlier reserve read. Reuse is not a new generation. |
| deleting | Decline, finish cleanup, reserve only on a later demand after absent. Publish cannot rescue a marked generation. An old delete of g1 cannot affect g2. |
| quarantined | Decline until operator repair. This is now an explicit branch; the old missing-quarantine reserve finding is closed. Quarantine's observation and delete-authority SQL remain inconsistent, finding 16. |
| Denied/partial reserved set | No invented second continuation, no URL for an unallocated generation, and no publication unless **all** chain digests are verified. Budget exhaustion and late reservation results require finding 5's handoff; exact missing-row/refusal behavior requires finding 22. |

### Restore and coarse drops

`review/baseline-r4/ledger.md:243-245` deliberately rebinds digest-only graph descriptions to the current verified content. That is coherent for content-addressed bytes; no graph-held generation is needed, and a checkpoint containing an old generation's digest may safely use a new verified generation of the same digest. The old missing `(digest,gen)` restore mapping and count-reconciliation quarantine findings are closed.

Dropping one bad digest may discard otherwise valid parts sharing it; this is an accepted cache-miss cost for restore/missing/corrupt content. It is not a claim that an arbitrary selected part can be removed independently. That distinction is finding 9.

No merge may be served before restored parts have been checked and dropped as necessary. All graph-dependent offers/exports must likewise remain gated while the graph is being restored under M; WS welcome/pings and safe ledger-only unavailability can be served, and already-issued object URLs do not depend on a warm service. A standalone renewal can be safe only with a validated key/index and successful verified-state stamp; missing reconstruction is unavailable/bad-key, not permission to walk an incompletely restored graph. The document's merge gate is good; it must not be bypassed by another graph-serving path.

### What a sound recurring listing sweep would require

A recurrent reconciler can eventually reclaim the unbounded-tail example without pretending two fixed deletes prove physical drainage:

- Parse the **entire** `(pool,digest,gen)` key. Compare it against the ledger's retained monotonic head and generation-qualified cleanup state, not only whether a digest row exists.
- Never delete a current pending/verified generation based on an old LIST or head snapshot. Revalidate current-key eligibility through the authoritative writer. A generation strictly older than a non-reused current head cannot become current again; that is the useful ABA property.
- Delete retired/current-absent or genuinely orphaned keys through explicit cleanup intent, retaining/retrying that intent or repeating the sweep. An in-progress PUT may appear only after a sweep, so no finite listing pass proves drainage without a real maximum duration.
- Charge observed retired bytes/cleanup liability and count retries. Keep late completions discoverable after the first and second delete.

Under the design's existing 1,000-keys/page and illustrative AWS rate assumptions, listing one million objects takes about 1,000 LIST requests, roughly $0.005 **for listing alone** (`review/baseline-r4/architecture.md:173`). Metadata joins, pagination, repeated generations, object DELETE requests, retry work, and elapsed scan time are additional. This is a sound eventual-reclamation direction, not an implemented reconciler or proof of a strict physical quota under an unbounded inflight tail.

### Metrics computability and limits

- **Grounded:** cache/session/result identity, engine replacement counter, completion carrier `dagger.io/cache.session.spans` with its cache-result population, part-address/bytes/nanoseconds on successful installs, causal parent override, and timestamped spans. R4 correctly treats missing completion as unknown and excludes uncertain donor credit.
- **Grounded lazy discriminator:** `wcprof.op.kind=lazy` is emitted by the actual source; the source also provides wait-link intervals. The design should name those rather than imply a new cache-contract field. Neither enclosing-span label alone proves wait-free attributable work, finding 11.
- **Not a grounded global identity:** `part_digest` is a service-defined field with no specified derivation/join; installed part addresses are not that identity, finding 12.
- **Per part, never per layer:** moving cost to `part_cost` closes the old blob-layer multiplication. Multi-part allocation and entry-inclusive double credit remain distinct open defects.
- **Mixed residual:** expressly reaches the remaining series at metrics `:60,79`; conflicting copies and opportunity exclusions can still remove it or other real work, findings 13 and 21.
- **Provider breaker:** named delayed provider sources, success-only installation latency, and client-hang blindness are now honest limitations. `computed_with_url` is merely a correlation proxy; neither it nor deferred publication measures a failed GET's stall. I do not require an imaginary failure attribute.

## Closure audit — all 23 GPT round-4 findings

“Closed” means the particular design defect is closed, not that an unrun deployment experiment passed. “Partial/open” points to the residual without recounting it as an additional finding.

| R4 G# | Disposition | R4 evidence / residual |
|---|---|---|
| 1 publish rollback/unknown COMMIT | **Closed for the original publication transaction** | No DB transaction/counter across Set, `review/baseline-r4/ledger.md:150-165`. Mark uncertainty is the different failure boundary in finding 2. |
| 2 reference-counter lifecycle/idempotency | **Closed** | Counter removed; graph liveness recomputed at `review/baseline-r4/ledger.md:18-20,189-212`. Duplicate/refused Set no longer increments a phantom count. |
| 3 graph-generation/restore binding | **Closed by an explicit semantic choice** | Keys retain gen, graph remains digest-only, restore validates current digest state, `review/baseline-r4/ledger.md:62,243-245`. |
| 4 failed-source reserve / zero-row flow | **Partial/open** | Locked branches and quarantine outcome exist, but failed cannot reach the absent-only allocation/cleanup UPDATE, finding 3. |
| 5 unconditional head mirror | **Closed** | Mirror deleted from schema and transitions. Quarantine's independent queue INSERT is a new analogous source-success defect, finding 16, not a surviving mirror. |
| 6 leadership fence | **Open** | Epoch replaces holder but a snapshot EXISTS is not promotion serialization, finding 1; unfenced/source-independent queue writes remain finding 16. |
| 7 lock order / renew starvation | **Partial** | No DB locks across graph Set; sequential autocommit mint closes former cycles, `review/baseline-r4/ledger.md:162,177-185`. Whole-request deadline still finding 15. |
| 8 late PUT after cleanup | **Open** | Two twenty-minute attempts do not impose a progressing-transfer bound; generation-aware recurring reclamation is still unspecified, finding 7. |
| 9 completed PUT/dead donor trigger | **Partial** | Immediate HEAD trigger now at `review/baseline-r4/latency-and-azure.md:47`, `review/baseline-r4/ledger.md:112`. Owner/epoch adoption and timing are findings 4–5; old flow copy persists, finding 21. |
| 10 corruption feedback / fallback wording | **Closed for the reported claims** | Fallback-to-compute aligned at `review/baseline-r4/architecture.md:150,258`; no protocol mismatch report is promised as a real wire message, `review/baseline-r4/ledger.md:231,276-277`. The limitation is accepted. New quarantine-transition defects are finding 16. |
| 11 completeness attribute / revision namespace | **Closed** | Correct carrier population, engine replacement counter, missing-carrier unknown policy, `review/baseline-r4/metrics-and-eviction.md:9-12`. |
| 12 inclusive attribution / non-evaluation work | **Open** | Boundary/filter/uncertainty clauses improve attribution but do not trim parent intervals, and pool sums still exist, finding 11. |
| 13 donor granularity / mixed residual | **Partial** | Finalized per-part table and explicit residual, `review/baseline-r4/ledger.md:54-59`, metrics `:73-79`; canonical key/production-work allocation still finding 12, contradictory remaining copies finding 21. |
| 14 ceiling double-counting | **Partial/open** | Old duplicate addition removed, but actual opportunity work is excluded from shared R and the new ratio is not the old opportunity ceiling, finding 13. |
| 15 invented fallback wait / miss flags | **Closed for that metric** | `computed_with_url` is labelled proxy, not duration; limitations explicit at metrics `:137-140`. A stale architecture panel name remains finding 21. |
| 16 event-window regret not cohort | **Open, same witness** | `increase[24h]` still divides event-time windows, metrics `:161,195`; finding 14. |
| 17 grace-time projection / occupied failures | **Partial/open** | Failed/quarantined current heads now charged, but pre-mark retirement projection is still absent and retired-gen bytes cannot be charged, findings 8 and 10. |
| 18 component eviction / indivisible overshoot | **Partial/open** | Arbitrary candidate sets replace closure, metrics `:179`. Actual per-part actuator missing, discrete cap/progress unresolved, findings 9 and 17. |
| 19 architecture metric / Phase-0 residue | **Partial** | R1 formula replaced and Gate A/B named, `review/baseline-r4/architecture.md:260,264`. Query eligibility, mixed R, breaker names, and SLI endpoint still inconsistent, finding 21. |
| 20 removed pins / pre-gen keys | **Closed for those instructions** | `review/baseline-r4/architecture.md:40,49,137,154,227` now uses gen and M rather than publish pins. The stale INSERT description is a different residual, finding 21. |
| 21 cost total | **Closed** | `review/baseline-r4/architecture.md:205-214`, `review/baseline-r4/decision.md:48` now approximately $0.232/day and $7/month; Python re-derived $0.23168/day and $6.9504 per 30 days from unrounded request rows. |
| 22 FQDN NetworkPolicy residue | **Closed** | `review/baseline-r4/architecture.md:86,336` and `review/baseline-r4/decision.md:87` now agree on CoreDNS/Cilium alternatives. This audits text consistency, not deployment enforcement. |
| 23 known-dead owner ten-minute suppression | **Partial** | Immediate HEAD policy exists, but inherited-owner adoption is missing, and architecture/decision retain the old wait, findings 4 and 21. |

## Closure audit — all 31 Claude round-4 findings

| R4 C# | Disposition | R4 evidence / residual |
|---|---|---|
| 1 graph_refs never decrements | **Closed** | Removed, `review/baseline-r4/ledger.md:18-20,189-212`; actual graph incidence supplies liveness. |
| 2 Set outside Postgres COMMIT | **Closed for that protocol** | No DB publication transaction, `review/baseline-r4/ledger.md:150-165`; distinct mark uncertainty remains finding 2. |
| 3 graph/DB deadlock, renewal starvation, lease cascade | **Closed for the former lock chain** | No DB rows held across Set and no explicit GC lock on leader row, `review/baseline-r4/ledger.md:162,189-202`. This does not prove epoch handoff, finding 1. |
| 4 mint plan-order deadlock | **Closed for the multi-row cycle** | One autocommit row at a time, metrics-independent sorted sequence, `review/baseline-r4/ledger.md:177-185`; total deadline finding 15. |
| 5 source/mirror rowcount divergence | **Closed** | No mirror remains. Queue authorization must still follow source transition success, finding 16. |
| 6 failed/deleted reserve guards, non-head cleanup | **Partial/open** | Synchronous destruction added, deleted state removed, but failed branch cannot allocate/finish, finding 3; old-gen tail remains finding 7. |
| 7 restore quarantines newest bytes / missing gen | **Closed** | `review/baseline-r4/ledger.md:243-245` restores by digest state and never quarantines for accounting drift. |
| 8 scanner deleted while parts remain | **Closed for the old path** | No deleted state/counter reconciliation; verified GC still requires non-liveness under M. Promotion/uncertain mark exceptions are findings 1–2. |
| 9 union-find component closure | **Partial** | Arbitrary sets allowed at metrics `:179`; chosen per-part actions still impossible in general, finding 9. |
| 10 donor stored per blob / too early | **Partial** | Per-part finalization table closes those dimensions, metrics `:73-79`; part key/join and multi-part cost allocation remain finding 12. |
| 11 inclusive nested work / missing remaining series | **Partial/open** | Remaining series now emitted at metrics `:60`; boundary does not make intervals disjoint and catalogue still sums, finding 11. |
| 12 PUT buffer-fill rate | **Closed** | Probe prints body+response end-to-end rate, `review/baseline-r4/s3_latency_probe.py:88-98,114-116`; revised measured rates accepted. Table duration typo/double ack is finding 23. |
| 13 aggregate as per-stream rate | **Closed as an illustrative scenario** | `review/baseline-r4/latency-and-azure.md:26,39,45` divides fair-share aggregate by eight and labels extrapolation. It is not a production tail bound. |
| 14 dead-donor publication | **Partial** | Explicit immediate HEAD trigger exists, latency `:47`, ledger `:112`; owner adoption/timing still findings 4–5. |
| 15 production breaker blindness/source | **Closed as a disclosed limitation** | Provider source/delay and unobservable client hang explicit at metrics `:134-142`, latency `:64-82`. No fictional engine-attempt metric is required. |
| 16 removed expiring pin in architecture | **Closed** | `review/baseline-r4/architecture.md:49,227` now references M/state read. |
| 17 R1 SLI formulas verbatim | **Closed for those old formulas; residual copy open** | Formula replaced at `review/baseline-r4/architecture.md:260`, but mixed residual definition still stale, finding 21. |
| 18 miss_reasons / demand_p95 mislabel | **Closed in normative catalogue** | Proxy renamed and no wait claimed, metrics `:137-140`; old `demand p95` architecture cell remains finding 21. |
| 19 quarantine branch / undefined g_new / unlocked reread | **Closed for old branch mechanics; new failed edge open** | Locked state branching and explicit decline at `review/baseline-r4/ledger.md:100-134`; no mirror/g_new assignment. Finding 3 covers the distinct failed allocation omission. |
| 20 holder-only / unfenced scanner | **Partial/open** | Epoch is used rather than holder, but snapshot fencing and unconditional queue mutation are findings 1 and 16. |
| 21 pre-gen storage text | **Closed for keys/idempotence** | Generation layout at `review/baseline-r4/architecture.md:40,137,154,161-162`; physical multi-gen accounting is finding 8, stale INSERT description finding 21. |
| 22 withdrawn-threshold residue | **Mostly closed** | Threshold references removed at architecture `:19,264,318`; actionable opportunity query still lacks normative eligibility/precedence, finding 21. |
| 23 MinIO production / probe breaker residue | **Closed at topology/signal level** | External S3 and named provider sources control production; architecture `:12,250,270,283,330`, latency `:55-57`. Homelab-specific examples are not deployment requirements. |
| 24 NetworkPolicy FQDN residue | **Closed** | `review/baseline-r4/architecture.md:336` now agrees with `:86`. |
| 25 wrong bytes / fictitious report | **Closed for the asserted fallback/report claims** | Architecture `:150,258` agrees on fallback; ledger `:231,276-277` admits missing unsampled detection. New quarantine SQL problems finding 16. |
| 26 cost total | **Closed** | Approximately $0.232/day / $7/month, architecture `:209,214`, decision `:48`. |
| 27 completeness name / missing series / cohort | **Partial** | Correct carrier and named GC/store series at metrics `:11,123,134-135`; event-window regret remains finding 14. Architecture-only undeclared panel names remain finding 21. |
| 28 ceiling duplication | **Partial/open** | Gross numerator now consistent, but dropping opportunity work from remaining creates new omission bias, finding 13. |
| 29 quota residue | **Partial** | Duplicate table/cancellation removed and current failed/quarantined charged, metrics `:187-191`; pre-mark retirement and retired generations still findings 8 and 10. |
| 30 fixed four-second waiter loss | **Closed for the timeout policy** | Event-driven retry replaces blocking wait, `review/baseline-r4/ledger.md:167-173`; lost notification/readiness race is new finding 6. |
| 31 chain length vs distinct digests | **Closed semantically** | Distinct sets explicit at `review/baseline-r4/ledger.md:154-165`, `review/baseline-r4/decision.md:33`; executable equality/refusal precision remains finding 22. |

## What actually improved, what is newly introduced, and the release boundary

**Genuine closures:** recomputed graph liveness; no mirrored head; no publication transaction spanning graph mutation; digest-only restore/rebinding; no accounting-driven quarantine; sequential mint locking; proper completion carrier and engine revision; finalized per-part rather than per-blob storage; explicit mixed residual intent; named delayed provider metrics and honest hang blindness; no fixed publication waiter; corrected PUT end-to-end rate and fair-share contention arithmetic; aligned generation keys, publication pins, topology, request totals, and FQDN mitigation.

**Newly executable against changed R4 text:** snapshot epoch fencing (1), uncertain mark escaping M (2), failed-only/absent-only state-flow mismatch (3), missing inherited-owner adoption (4), synchronous reserve I/O on the continuation budget (5), lost verify/merge readiness events (6), old-generation charge disappearance under the one-row model (8), the impossible per-part actuator (9), opportunity-exclusion bias (13), cumulative per-row renewal budget (15), and generation/source-independent quarantine intent (16). Findings 7, 10–12, 14, and 17 include residuals whose predecessors were already reported; they are not relabelled new just because wording changed.

The review does **not** establish that M must be abandoned. The restricted one-process, confirmed-operation proof holds. It establishes that M needs a real authority handoff and terminal-operation contract, and the surrounding reserve, cleanup, metrics, and eviction policies must be executable with the selected API. Do not restore `graph_refs` or DB locks across `SetStoredPart` to repair these findings.

### Measurements that remain prerequisites, not invented results

1. Gate A discovery capture and exact install/lazy/completion attribute contract on beta.25; validate the actual OTLP route and loss/completeness accounting.
2. Real-pool `LiveBlobs()` scan **and graph-lock wait**, M hold through mark, publish/merge latency, pool-size cap, and restore duration.
3. Span-end to **successful usable publication**, including compression/scheduling, for 30 MB, 500 MB, and 2 GB; donor disconnect before `uploaded`; asynchronous merge/partner completion orders.
4. Promotion/paused-leader tests, old DB statements blocked across expiry, lost mark responses/cancellation, and current-epoch queue authorization. These verify the corrected failure protocol, not a substitute for defining it.
5. Slow-progressing PUT completion past both deletes, recurring generation-aware reconciliation, failed/retired byte charging, and controller ticks during URL grace.
6. Fresh-engine merge/install memory spike before setting limits; production 100×2 GB read burst and eight large PUTs against the actual external S3 endpoint.
7. Exact Range 206/Content-Range, header-less PUT compatibility, single-PUT cap, missing-key DELETE, create-only checkpoint behavior, and backend-specific admission/transfer time limits.
8. Gate B paired wall-time/failure comparison against the warm node-local baseline. Inclusive donor-seconds ratios cannot replace it.

### Primary documentation consulted

These establish platform premises, not execution of the proposed service. The configured web extractor failed because its native backend is search-only; fallback searches returned partial results. Direct HTTP reads of all six official pages below then returned HTTP 200 and the relevant passages.

- **[PG-L]** PostgreSQL 18 manual, §13.3, row-lock lifetime and independence from ordinary data reads: `https://www.postgresql.org/docs/18/explicit-locking.html`.
- **[PG-T]** PostgreSQL 17 manual, §9.9.5, `now()` is transaction-start time, while `clock_timestamp()` advances: `https://www.postgresql.org/docs/17/functions-datetime.html`.
- **[PG-I]** PostgreSQL 18 manual, §13.2, Read Committed statement snapshots and updating-command behavior: `https://www.postgresql.org/docs/18/transaction-iso.html`. Its distinction between target-row rechecks and the command snapshot is material to finding 1.
- **[PG-C]** PostgreSQL 17 libpq manual, §32.7, cancellation dispatch does not guarantee that the command was canceled; its terminal result must be observed: `https://www.postgresql.org/docs/17/libpq-cancel.html`.
- **[S3-E]** Amazon S3 User Guide, presigned URL expiration checked at request time, with an example of a download continuing past expiration: `https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html`. The slow-PUT history is a design counterexample under the brief's already-acknowledged admitted-transfer tail, not a newly measured AWS PUT duration.
- **[GO-RW]** Go `sync.RWMutex` documentation, a queued writer blocks new reader acquisitions: `https://pkg.go.dev/sync`.
