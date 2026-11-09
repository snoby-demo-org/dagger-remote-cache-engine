> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

STOP

# Round-3 adversarial review of Revision R2

**22 findings: 2 P0, 18 P1, 2 P2.** Stop Tier-1 implementation against this specification, not discovery/publication/memory spikes or Tier 2. The generation-key idea is valid and defeats the original delayed-DELETE ABA. The normative reserve operation, expiring publication pin, and generation-to-graph integration do not yet establish the claimed state machine. Several fixes also remain contradicted by unmodified normative clauses.

This reviews a deliberately unimplemented design. No service implementation is requested. Only `review/gpt-r3.md` was written. References are relative to `<DESIGN_WORKSPACE>`, except `src:`, which means `<HISTORICAL_SCRATCH>/dagger-src` at `7e9eaacc376e5c3bed3efdca9e1ba750bc8d9233`. I verified HEAD and a zero targeted diff against beta.25 for `engine/telemetryattrs/attrs.go` and `dagql/cache_part_content.go`.

Evidence: all files in the brief, including the full 2,039-line supplied diff and both earlier reviews; targeted pinned-source reads; Python arithmetic; in-memory SQLite predicate/uniqueness witnesses; and a local delayed-header HTTP test executing the probe's actual `do()` function. These are **not** PostgreSQL concurrency tests, a live object-store run, or an engine interoperability test. `psql` exists, but its default local socket has no server. The supplied historical design-cluster measurements remain accepted facts; I do not replace them with synthetic measurements.

## Q1. Closure audit of all 19 round-2 findings

“Closed” below means the specific text defect is closed, not that an acknowledged deployment spike has run. `revision-r2.md:3` saying all 19 hold is neither closure evidence nor an override for conflicting clauses.

| R2 # | Actual clause checked | Round-3 assessment |
|---|---|---|
| 1 — discovery | `decision.md:61`; `revision-r2.md:39-40` retain the self-hosted capture and install-contract spikes. `architecture.md:336` still promises the same lifecycle fallback. | **OPEN, explicitly gated, not a new P0 merely because the design is unimplemented.** The fallback still cannot name fresh executed roots. The new pre-build live-pool gate is circular: finding 18. |
| 2 — reused/mixed-owner chains | `ledger.md:94-120` separates owner-fenced verification from an all-distinct-current-verified pin/update. | **Exact pending-and-owned publication predicate defect CLOSED.** With A verified and B newly verified, count/update is 2/2; no all-owned condition remains. End-to-end closure is still blocked by reserve, pin and generation integration: findings 1–4, 6. |
| 3 — metric model | `metrics-and-eviction.md:27-55,75-80` adds entry keys, seven classes, explicit historical cost estimates and unknowns. | **PARTIAL, OPEN.** Donor cost is now explicitly estimated rather than invented, and ns conversion is explicit. Entry/outcome conflicts, local-hit classification, mixed-part work, exclusive-time attribution and net value remain wrong: findings 7–9. |
| 4 — publication gap / DELETE ABA | `ledger.md:9-15,114-128,173-180`. | **Delayed DELETE across different immutable keys CLOSED in principle. Publication gap OPEN.** An expiring pin does not revoke a paused publisher; actual grace-aware counterexample in finding 2. Generation-aware graph cleanup is unspecified: finding 3. |
| 5 — NULL mint stamp | `ledger.md:31,52,77-78`; GC predicate `:166`. | **CLOSED for NULL immortality.** Reserve initializes a non-NULL timestamp. This is not proof every object is eventually collectable: failed/non-head generations have separate defects. |
| 6 — executable lifecycle | Transition table `ledger.md:56-66`, reserve `:70-83`, recovery `:173-182`. | **PARTIAL, OPEN.** A stuck deleting row has an explicit retry and deleted stamp, but reserve ignores state, head-state mirrors are not maintained, and failed physical objects are not reclaimed: findings 1, 5. |
| 7 — renewal database contradiction | `ledger.md:152-156` explicitly permits a committed write; `architecture.md:99` still forbids a Postgres round trip. | **OPEN as consistency requirement.** Finding 13. |
| 8 — one verification policy | `architecture.md:17,49,150,227,258`; `decision.md:33`. | **PARTIAL.** Overview/I1/cold flow now agree on existence/size plus 1% hashing. I3 still claims every key has correct content; the failure row contradicts itself and ordinary fallback; corrupted verified generations have no immediate invalidation transition. Finding 17. |
| 9 — publication arithmetic | `latency-and-azure.md:29-44`. | **Categorical impossibility CLOSED; residual arithmetic inaccurate (P2).** Donor/service distinction and tail caveats are real improvements. Finding 20 and Q5 below. |
| 10 — admission basis | `metrics-and-eviction.md:172-177`; `latency-and-azure.md:93-97`. | **Measured-PUT-as-read-basis overclaim CLOSED.** Read/apply terms and k are honestly assumptions. **Policy closure partial:** OR versus AND gates and unknown costs remain undefined, finding 21. |
| 11 — cold sharers | `metrics-and-eviction.md:159-168`. | **OPEN, now reversed outright.** Group awareness is useful, but the algorithm removes the highest benefit/byte first. Finding 11. |
| 12 — merge-driven retention | `metrics-and-eviction.md:153-155` makes pool demand install-only and last-install timestamp based. `architecture.md:170` retains merge-refreshed stored-root retention. | **OPEN across normative documents**, despite the good metrics clause. Finding 15. |
| 13 — overlapping opportunity sets | `metrics-and-eviction.md:68-69,84-87`. | **Original >100% overlap and “pairing proves broken keying” defects CLOSED in this document.** Would-hit wins ties, both sets are spent subsets, and keying is diagnostic. Zero denominators and other alert ratios remain broken (finding 22); architecture's old query at `:197,264` is not the new gate (finding 18). |
| 14 — metadata suppresses repair | `architecture.md:97` correctly switches to usable part availability. `:160` still says skip on a stored value. | **OPEN: two incompatible scheduler rules.** Finding 14. |
| 15 — continuation behind commit | `architecture.md:225-227` explicitly sends the one upload continuation before `CommitValueMerge` and gates publication on commit. | **CLOSED for the original dependency-order defect.** Ledger waiter now introduces a different wire/timing error (finding 6). The five seconds is a service watchdog, not a native engine deadline. |
| 16 — production topology | `architecture.md:215,283`; `latency-and-azure.md:57-62`; metrics `:8-9`; decision `:20,26`. | **Primary-tier prohibitions, only-stateful-service claim and warm standby CLOSED.** **Not globally closed:** unqualified durable/Tier-2 MinIO and Azure-MinIO sizing text survive (finding 16). |
| 17 — measurement point | `metrics-and-eviction.md:121-129`; `latency-and-azure.md:68-84`. | **PARTIAL, OPEN.** Store metrics, probes and installs are now distinguished, but the new engine failure ratio has no attempt/failure event derivation, and server TTFB is not client-observed latency. Finding 10. |
| 18 — probe assertions/units/order statistic | `s3_latency_probe.py:98-118,131-157,174-177`; latency `:20-25,112`. | **Original listed assertion, unit and order-statistic defects CLOSED.** Every listed status/length/expiry/concurrent result is checked. **New regression:** `do()` stops its TTFB timer before receiving response headers, finding 19. |
| 19 — checkpoint schedule/lifecycle | `architecture.md:33,100,142,208`; metrics `:110,143,199-201`. | **CLOSED.** Diagram is cold restore, age expiry removed, last-five pointer policy and ten-minute floor agree, cost uses 720 scheduled uploads/day across five pools, duration alert is a gauge comparison. This is a scheduled-on-change budget, not a strict maximum including graceful-stop checkpoints. |

### Corrections to my round-2 assessment

- The discovery, memory and publication spikes are evidence gates, not an obligation to have implemented the service before reviewing its design. I do **not** count an honestly unrun spike alone as a fresh P0.
- R2's requested historical cost estimate is now a legitimate labelled estimator; no per-install donor-cost attribute is needed if that is the chosen meaning. Remaining complaints concern the estimator's input attribution and the dashboard's meaning, not a nonexistent attribute.
- R2 used the supplied “70.1” aggregate as decimal MB/s in part of its numerical example. The underlying formula was MiB/s. With the corrected 73.4 decimal MB/s and approximately 97.3 MB/s read estimate, full transfer-plus-reread is approximately 33.43–47.82 s, not my old 34.38–50.06 s scenario.
- A publisher paused just over two minutes does **not** by itself defeat the GC fence: `last_ref_at` also protects it for G=7,200 s. Finding 2 uses G+1, not 121 s.
- The five-second upload watchdog is the service's policy. `recon-protocol.md:147-159` does not establish an engine-enforced five-second continuation deadline. I should not have let that description become an engine requirement.

## Sequential findings

### Finding 1 [P0] — Reserve does not implement its legal transitions, and head-state mirrors never advance

**References:** `ledger.md:39-48,58-66,70-86,97-100,161-178`.

The normative reserve SQL reads `(gen,state)` but never branches on state. E1 reserves D/g1. E2 then serializes on the head, reads g1, and attempts **g2**, not E1's g1. If g1 is pending, the insert violates `blob_one_pending`; its `ON CONFLICT(pool,digest,gen)` only targets the primary key, so it does not produce the promised zero-row waiter result for this other unique index. If E1 verifies between E2's earlier eligibility check and E2 acquiring the head lock, E2 inserts pending g2 and moves head away from usable g1. A caller-side stale precheck does not supply the missing locked state branch. If g1 is deleting, it likewise creates g2 before delete confirmation, explicitly forbidden by line 66. These results were reproduced with the same two unique constraints in in-memory SQLite; no database concurrency claim is required.

Separately, verify, GC mark and deleted-stamp statements update only `blob`; none updates `blob_head.state`. After E1 verifies, head still says pending. Adding just the prose's state guard would therefore wedge reuse/recovery instead of fixing it.

**Minimal fix:** specify all head-locked branches: verified→reuse captured generation; pending→wait without insert; deleting→retry/skip without insert; absent/failed/deleted→allocate only after the appropriate cleanup fence. Atomically maintain the head mirror on every state transition, or remove it and read the current blob state under the same head lock. Update head only after successful allocation. Never treat every reserve call as generation advancement.

### Finding 2 [P0] — Expiry does not revoke publication authority; a paused publisher still publishes deleted bytes

**References:** `ledger.md:114-128,161-168,192-195`; `architecture.md:49,53`.

At t=0, publisher P pins verified D/g1 until t=120 and stamps `last_ref_at=0`; no stored part is in the graph yet. Take `last_url_mint_at<=0`. P pauses before `SetStoredPart`, while other workers remain live. At t=7,201, a sweep snapshots no graph reference; pin expired and both timestamps are older than `now()-7200`. Mark→DELETE→deleted succeeds. P resumes and unconditionally calls `SetStoredPart` for g1. Neither the SQL nor the graph insertion checks an unrevoked publication token at the actual insertion point. This is the old long-gap counterexample, not fixed by a shorter expiring pin.

The two-minute pin is durable, but **durable expiry is not cancellation**. Overlapping publishers can also overwrite the single deadline or clear another publisher's pin at `:124-125`; there is no publisher identity or reference count.

**Minimal fix:** serialize actual graph insertion with GC eligibility using an explicit publication token/epoch and a common lock order. One concrete option is retaining the generation's database row lock through graph insertion and committing afterward, with insertion forbidden after losing that transaction/epoch. If expiring intents are retained, expiration must revoke a token that is checked within the protected insertion operation, not merely checked before another arbitrarily pausable gap. Per-publisher pins must not clear one another.

### Finding 3 [P1] — Generation-scoped keys are valid, but graph persistence and generation-selective cleanup are not specified

**References:** `ledger.md:15,122-125,180,186`; `architecture.md:130,137`; `src:dagql/cache_transfer_types.go:40-56`; `src:dagql/cache_stored_parts.go:156-182`; `src:dagql/cache_stored_bundle.go:310-316`.

The design now asserts a part references `(digest,gen)`, but provides no durable part-to-generation representation. The imported chain has digest descriptors, URL addresses and an opaque renewal key, not a generation field. `StoredBundle` removes addresses/renewal keys from emitted outputs. The architecture renewal key still hashes only ordered digests. Most concretely, native `DropStoredParts` accepts **one digest**, not the `(digest,gen)` overload newly used by the recovery scanner.

Witness: scanner captures deleting g1; its delete completes; g2 is subsequently reserved, verified and published. Scanner's delayed graph cleanup now sees head no longer points to g1—the exact condition at line 180. Translating its call to native `DropStoredParts(D)` removes **g2's** parts as well. Skipping cleanup instead leaves old-generation graph references unclassified on restore.

**Minimal fix:** define a durable generation binding per stored part/value revision and a generation-aware wrapper with revalidation against graph mutation. State how that binding survives checkpoints and joins renewal; use generation-qualified keys throughout the object layout. No wire prohibition prevents this, and no upstream schema change is inherently required, but the missing side-state cannot be assumed to exist.

### Finding 4 [P1] — Mint's head join does not join the head to the requested blob

**References:** `ledger.md:144-152`, contrasted with its correctly correlated publication join at `:110,116-118`.

Mint joins `blob_head h` only on `blob.gen=h.gen`: neither `h.pool=blob.pool` nor `h.digest=blob.digest` is present. Consequently **any head anywhere with the same generation number** makes a verified row eligible. The returned identity is only digest, not generation.

Concrete reachable state from finding 1: pool p/D has g1 verified and g2 pending at head; unrelated pool q/X has head g1. Minting p/D updates and returns the non-head g1 instead of refusing. An in-memory execution of the shown UPDATE returned `(D,1)` for exactly this fixture. With a strict generation-at-head policy, it must return nothing. Cross-pool row selection is a predicate defect, not evidence of cross-pool byte disclosure: the target blob pool is still constrained.

**Minimal fix:** correlate pool, digest and generation, bind the requested part's generation, and return `(digest,gen)` for exact URL construction. Omitting this join must not be justified by invariants that the normative reserve already violates.

### Finding 5 [P1] — Failed uploads lose their ledger evidence without a physical-object cleanup protocol

**References:** `ledger.md:61,66,171-182,195-196`; `architecture.md:171-173,240`.

E1's PUT to g1 succeeds, then E1 dies before `uploaded` reaches the verifier. At ten minutes g1 becomes failed. At fifteen minutes `:182` purges its row. The only specified S3 deletion path targets **current verified** generations. Thus the fully uploaded g1 object was neither verified nor deleted before its evidence disappeared. If g2 is allocated from failed g1 first, the older failed generation is not head and cannot enter that GC path either.

Weekly LIST is described as finding missing-ledger objects, not as an executable retry/retirement protocol. A valid PUT begun before URL expiry can also remain in progress after the lease/TTL arithmetic; no maximum completion time or late-completion reconciliation is specified.

**Minimal fix:** retain failed-generation cleanup intent and generation key until an idempotent physical-delete/reconciliation path has run. Define how late PUT completion is detected/reaped; capability expiry alone is not transfer drainage. Only then purge accounting evidence. Also specify explicit upload-failure versus lease-expiry transitions—the table's failure trigger currently still requires lease expiry.

### Finding 6 [P1] — The waiter path sends an impossible second upload and uses the wrong deadline

**References:** `ledger.md:134-140,202`; `architecture.md:225-227`; `recon-protocol.md:147-159`; `src:engine/remotecache/session.go:265-321`.

E1 owns A and dies. E2's chain is `[A,B]`; E2 omits A and receives its one `upload{exportID,urls:{B:...}}`. B completes and E2's publication fence sees only one of two verified blobs. After four seconds, ledger §5 says E2 **sends `upload{done:[]}`**. `done` belongs to the engine's `uploaded` reply, not the service's upload request. That exportID was consumed at first upload dispatch; a second upload cannot release or retry it. The graph publication decision needs no second engine command at all.

The four-second publication wait is also placed “inside the engine's five-second upload-continuation watchdog,” although verification necessarily follows the continuation and potentially a long PUT. The five-second policy is service-side, not an engine publication deadline.

**Minimal fix:** separate the pre-continuation watchdog from post-upload internal publication retries. Send exactly one correctly shaped upload; after its reply, abandon or continue waiting internally with a stated bound. On owner death, leave the part unpublished, expire/reap A, and reschedule a live donor. This is fail-open loss, not an engine deadlock, once the wire error is removed.

### Finding 7 [P1] — The seven classes are disjoint labels, not a valid entry-decision population

**References:** `metrics-and-eviction.md:27-48,53-55,64-65,84`; `src:engine/telemetryattrs/attrs.go:612-625,731-742,766-788`.

A pure local hit has positive call-span self time—say 0.001 s. §1.3 includes that time in `recompute_secs`, so the hit satisfies the **deferred_recompute** predicate despite doing no lazy recomputation. The promised hit_local class effectively requires zero lookup overhead.

A mixed-part entry that installs P and computes Q for 100 s is classified installed by priority. Spent only sums executed/deferred decisions, so its 100 s disappears. Applying the full historical entry cost as saved simultaneously overstates its benefit. Failed-install lazy spans also lack the success-only offer attributes and are not inherently identifiable as recomputation from the listed fields.

Finally, several lookup calls can return the same `(session,cache,result_id)` with executed, joined and hit outcomes; replacements can change the value under that ID. There is no outcome-resolution/revision rule for that one row. Priority joined can discard actual work associated with the same entry.

**Minimal fix:** distinguish lookup decision identity from entry/part/replacement identity; classify observed local evaluation using actual evaluation evidence, not positive call overhead. Keep import and recompute work as independent measures even on installed entries. Define session finalization, replay dedupe, missing/out-of-order spans and unknown observations; use the available replacement/session-span-completeness evidence rather than presenting seven labels as seven verified populations.

### Finding 8 [P1] — “Exclusive” lazy attribution discards nested work and omits the producer's causal parent

**References:** `metrics-and-eviction.md:19-21,50-57,75`; `src:engine/telemetryattrs/attrs.go:772-777`.

§1.3 combines **self time** with an “outermost lazy span only” filter. For one entry with an outer lazy interval of 100 s containing a same-entry nested lazy span of 90 s, exclusive self times are 10 and 90. The filter keeps only the outer self time: **10 s instead of 100 s**. Outermost-inclusive aggregation and summing exclusive fragments are different algorithms; they cannot be mixed.

No interval-union rule handles overlapping children, and the proposed tree uses ordinary parent IDs even though the producer names `WcprofParentAttr`, falling back to parent, as the installation's causal ancestry. This can attach work/route to the wrong demanding call even when all fields arrive.

**Minimal fix:** specify one attribution algorithm over causal parent links: either disjoint exclusive interval fragments from all relevant spans, or a union of outermost inclusive intervals with separately subtracted import/dependency work. Define concurrency and missing-parent handling. Do not call self duration a raw attribute; it is a derived measurement requiring that algorithm.

### Finding 9 [P1] — Saved/value metrics still omit the ordinary installation cost

**References:** `metrics-and-eviction.md:64-67,75-85`; conflicting older net formula at `architecture.md:260`.

The new saved counter adds the full estimated recomputation cost C. Lost adds only `max(0,I-C)` for install time I. For C=60 s, I=59 s and no other work: saved=60, lost=0, value ratio=**100%**, while net savings is **1 s**. For C=1, I=100, saved=1/lost=99 reports a positive 1% “value” even though the attempt lost 99 s. All the successful-path import cost below C is omitted from the denominator/cost accounting.

Calling C “avoided compute” is defensible, but not calling this a net savings/benefit model used to justify the project. R2's separate unknown bucket does not resolve the accounting semantics. The architecture still defines a different net metric using the obsolete receiving-lazy baseline.

**Minimal fix:** publish gross avoided compute, actual import/failed-attempt work and net estimated saved/lost separately. For nonnegative counters, accumulate `max(C-I,0)` as saved and `max(I-C,0)` as loss, with remaining recomputation accounted even on mixed entries; label any derived ratio precisely. Align architecture's SLI paragraph with the selected estimator.

### Finding 10 [P1] — The new engine-path breaker input has no computable failure population

**References:** `metrics-and-eviction.md:19-23,121-129,146`; `latency-and-azure.md:70-84`; `architecture.md:251`; `src:dagql/cache_part_content.go:592-608,674-684,710-714`.

`cache.offer.part/bytes/duration_ns` are emitted on **successful part installation**, not on every attempt or source failure. Offer acceptance is not a download attempt: lazy results may never be demanded. A call can fall back and succeed locally after a remote error. Thus “failure ratio from offer/install telemetry” has neither a stated numerator nor a denominator. There is no grounded failure attribute, attempt identity, timeout-to-failure rule or recording-rule definition.

Witness: 100 demanded imports fail and recompute successfully, yielding zero install-success attributes. The cited inputs cannot distinguish that from 100 accepted but never demanded offers plus independent local work. Successful slow-but-progressing downloads can instead have zero failures and still damage job time.

Store-side TTFB excludes client-network/request-path delays; access logs emitted after completion cannot implement the claimed live two-second per-request hang detector. Architecture still says probe p99 drives the breaker, while the rewritten tables call probes liveness-only. Metrics alerts use 5% but the topology table trips at 10% or 20%, with no separate policy definition.

**Minimal fix:** ground a real attempt/failure signal or explicitly limit v1 to provider errors plus representative reachability and install-duration signals. Specify window/minimum samples, thresholds, delayed/missing telemetry handling, recovery hysteresis and the hang detector's measurement point. Treat server and client latency as distinct.

### Finding 11 [P1] — Reclaim groups are evicted in reverse value order; arbitrary group selection adds blast-radius and complexity

**References:** `metrics-and-eviction.md:159-168`; `architecture.md:174`.

Same-age P and Q share one 1 GiB blob; each has one 60 s-benefit install. Their collective score is 120/1024=**0.1171875 s/MiB**. Hot singleton R has a separate 1 GiB and 100 such installs: 6000/1024=**5.859375 s/MiB**. The new command “pick the highest score, remove the whole group” evicts R first, sacrificing **50 times** the benefit for identical reclaimed bytes. Even the P+Q+R union has lower density than R, so allowing arbitrary sets does not rescue this witness.

Group accounting would fix the cold-sharer denominator pathology **if lowest benefit per reclaimable byte were removed**, and groups were actually constructed. Here their construction is unspecified: arbitrary subsets can be exponential, overlap, and include hot bridge parts to reclaim cold shared bytes. “Greedy ... cheap” describes selection after candidates exist, not generation of all candidates. Removing an entire group can also greatly overshoot the remaining quota target.

**Minimal fix:** reverse the eviction order, define a bounded candidate-generation rule and retention eligibility, recompute distinct frees after each selection, and state/limit group overshoot and hot-member collateral. Unknown cost must not silently equal zero benefit. A simpler install-recency fallback is preferable to an undefined subset optimizer.

### Finding 12 [P1] — Quota accounting includes deleted generations, and R2 drops the explicit delayed-free controller

**References:** `architecture.md:171-174`; `ledger.md:20-36,59,175-182`; `metrics-and-eviction.md:107-109,163-168`; `review/r2.diff:931-935`.

The architecture still defines used bytes as ledger `SUM(size)`. R2 now keeps deleted generations as rows. After three upload/delete lifetimes of one 1 GiB digest, two deleted plus one verified row yield **3 GiB ledger sum versus 1 GiB physical live bytes**. A 3 GiB quota stops admitting at 95% despite only one third being occupied. Per-state metrics do not supply the missing definition of physical/quota-charged bytes.

Conversely, removing graph parts does not free S3 bytes immediately: GC still honors mint/reference grace and runs hourly. The new group text says zero-benefit groups free bytes “immediately.” R2 removed the previous explicit `used - pending_free <= low` stop rule. Without carrying distinct generation-qualified projected frees across ticks, used=90, low=70 can evict 20 twice before the first delete executes, eventually reducing usage to 50. Recomputing groups inside one pass does not define cross-tick accounting.

**Minimal fix:** define charged occupied/reserved bytes by state and generation, exclude confirmed-deleted rows, retain failed objects until cleanup, and maintain durable/in-memory-reconstructible distinct pending-free intent until physical reclamation. New references must cancel projected frees. Do not claim immediate byte release.

### Finding 13 [P1] — Renewal still simultaneously forbids and requires a Postgres round trip

**References:** `architecture.md:99,253`; `ledger.md:145-156`; `revision-r2.md:15`.

The change map says this contradiction is fixed, but architecture's signer responsibility still forbids a Postgres round trip. Ledger now explicitly requires and allows one committed durable statement for each renewal. The index can remove a graph walk, not the authoritative conditional write.

Ledger additionally says local commits **are** sub-millisecond and whole-path p99 **stays** under 200 ms without any such measurement in the supplied evidence; the metrics document correctly treats 200 ms as an SLO.

**Minimal fix:** replace the retained prohibition with a database-inclusive deadline/cancellation budget, describe index reconstruction for ordered chains, and label commit/path timing as targets pending measurement. Preserve stop-minting behavior when durable stamps cannot commit.

### Finding 14 [P1] — The old value-existence scheduler rule still overrides the intended repair path

**References:** `architecture.md:97,160,224-227,235,240`; `ledger.md:138`.

§4.2 says never deduplicate on value existence, but §5.1 still says skip a root when `EquivalentEntries` shows a stored **value**. First donor's metadata commits; its PUT fails; a later donor reports the same recipe. Following line 160 skips re-export, leaving metadata-only state until expiry. Following line 97 schedules repair. Both instructions remain normative, with no stated scheduler override.

**Minimal fix:** remove/replace the retained recipe-level predicate and use particular-part usable availability plus bounded pending reservations in every scheduling flow, including admission reconsideration and failed-owner takeover. The corrected component table is substantive progress, not global closure.

### Finding 15 [P1] — Stored-root retention still refreshes on merge in the architecture

**References:** `architecture.md:170`; `metrics-and-eviction.md:151-155`; `revision-r2.md:20`.

The new metrics policy separates recipient retention from pool last-install demand and fixes the once-ever boolean. Architecture §6 still describes **stored roots** with seven days since last served, refreshed on install **and merge**. It also retains “merged but never installed” wording rather than the new last-install clock for all roots.

Witness: root installed once at day 0 is pre-merged every day without demand. Under the retained stored-root refresh rule it stays live indefinitely; under the new authoritative demand rule it ages out. Reading the change map does not resolve which policy the controller implements.

**Minimal fix:** carry the recipient/pool distinction into §6 and define initial uninstalled-root timestamps. Refresh authoritative pool retention only on genuine demand, and apply last-install idle pruning independently of whether it has ever been installed.

### Finding 16 [P1] — Production's primary topology is fixed, but unscoped MinIO dependencies and production-MinIO sizing survive

**References:** `architecture.md:12-13,110,216,270,283,287,329-330`; `decision.md:20,26-27`; `latency-and-azure.md:57-62`; `metrics-and-eviction.md:7-9`.

The explicit external-S3 primary and cold restore now agree. But architecture's opening still declares durable blobs and checkpoints **“in MinIO”** after introducing production; the manifest index says checkpoints in MinIO; Tier 2 is still **“on MinIO”** in both primary decisions without a design-cluster qualifier or production alternative. The Azure-specific R5 mitigation still sizes MinIO against Premium SSD v2 and VM uncached-disk caps, although production has no MinIO. Decision's cost paragraph also implies external necessarily means WAN, unlike latency §3's correct private/in-region distinction.

**Minimal fix:** scope the homelab diagram, credentials and MinIO bandwidth discussion explicitly to the design cluster; identify the production Tier-2 backend and generic checkpoint location; remove production-MinIO disk-sizing advice. Keep the stateful non-spot dcache/Postgres and accepted cold-restore policy unchanged. This is a remaining deployment ambiguity, not a claim the old external-primary prohibition survived.

### Finding 17 [P1] — Sampling is defensible, but I3, corruption behavior and invalidation transitions still contradict it

**References:** `architecture.md:49-55,150,258`; `decision.md:33-36`; `ledger.md:56-66`; `src:engine/snapshots/chain_error.go:73-86`; `src:dagql/cache_part_content.go:674-684`.

Authorization/existence/size plus a 1% hash sample is a defensible mutually trusted, fail-open cache tradeoff **when explicitly weaker than full server content verification**. Overview, I1 and cold flow now say this correctly. I3 still says every key only holds bytes hashing to its name, which a same-size wrong complete PUT on the 99% path disproves. Single-PUT atomicity proves complete replacement, not correct content.

§5.1 and the wrong-bytes row now say the engine's hash fails the receiving build, while that row's engine column says source-exhausted→compute and I7 promises fail-open. Ordinary checksum mismatch is wrapped as ChainContentError and refused as exhausted source in the pinned code; it is not a blanket build-failure policy.

Finally, invalidating a **verified** corrupted generation and forcing re-export has no legal immediate ledger transition. Only age-gated GC moves verified out of service; pending→failed does not repair an already published object. There is also no stated engine failure-to-digest feedback path for the unsampled corruption.

**Minimal fix:** weaken the stored-content claim, consistently describe ordinary digest-mismatch fallback, and specify a fenced verified→quarantined/deleting corruption path, generation-specific part removal and repair. If arbitrary import failures can fail builds, distinguish receiver/storage failures from bad remote content. Align verification cost: `architecture.md:206` still budgets 600 GB/day full rereads rather than the sampled expectation.

### Finding 18 [P1] — The new “before build” Phase-0 gate requires the service already built

**References:** `decision.md:61,65-67`; `metrics-and-eviction.md:131-133`; `architecture.md:19,197,264,336`; `revision-r2.md:39-43`.

The new gate says run one pipeline with the pool live and another quarantined, then **build the service only if** that measured wall-time delta justifies it. There is deliberately no service yet; a live shared pool is the missing deliverable. This cannot be the pre-build Phase-0 procedure. The quarantine run is also not automatically today's warm node-local baseline: that depends on keeping the same warmed engine/topology, not merely disabling remote commands.

Architecture retains the older call-duration/opportunity-to-job-wall-time procedure and 30% threshold. Its R7 lifecycle fallback still cannot supply fresh entry numbers without call spans, so it cannot bootstrap this live gate either.

**Minimal fix:** two gates: pre-build discovery capture plus telemetry/critical-path opportunity bounds and baseline workload characterization; post-prototype paired live/quarantined wall-time/failure comparison with explicit warm-local control. Keep discovery, memory and end-to-end publication spikes honestly open. Do not require a working Tier-1 service as evidence for permission to write its first code.

### Finding 19 [P1] — Probe v3 records request-send time as GET/HEAD TTFB, regressing the measurement basis

**References:** `s3_latency_probe.py:62-70,115-123,131-150`; `latency-and-azure.md:7-14,93,112`; `review/r2.diff:1843-1859`.

`do()` records `ttfb` immediately after `c.request()`, **before `c.getresponse()`**. For GET/HEAD this measures request serialization/send/connect overhead, not response first-byte/header latency. The preceding probe version waited for the response first; this is an R2 regression, not an old unresolved assertion issue.

I executed the actual function against a local HTTP server that waited 150 ms before sending response headers. It returned HTTP 200, probe “TTFB” **0.000293 s**, total **0.285030 s**. The environment added other latency, but the missing response wait is unambiguous. No actual MinIO data was changed by this test.

**Minimal fix:** take the latency timestamp after response headers arrive (and label that approximation accurately), preserve body-total timing, and re-run the future v3 measurement before attributing latency numbers to it. The brief's historical 3.96/4.10/1.92 ms facts remain accepted; they are not invalidated by the rewritten script's inability to reproduce that measurement.

### Finding 20 [P2] — Corrected publication table still mixes rate precision and overstates its own full-path interval

**References:** `latency-and-azure.md:33-42`.

With displayed rates, `2000/73.4 = 27.247956 s`, which rounds to 27.25 s (27.2 at one decimal), not 27.3. The measured GET total implies 97.315638 decimal MB/s; rereading 2 GB is 20.551682 s. Adding PUT, 18.8 ms ack and 1.92 ms HEAD gives **33.425872–47.820358 s**. Using its rounded 97 MB/s instead gives approximately 33.49–47.89 s. Neither derives an upper endpoint of 50 s or, under ordinary nearest rounding, a lower endpoint of 34 s.

**Minimal fix:** distinguish conservative rounded scenarios from literal re-derivation; display approximately 33.4–47.8 s for these inputs, with unmeasured overhead outside the interval. An eight-small-object aggregate is only an illustrative throughput scenario, not a proven single-large-object rate. The fast-path “approximately 13–28 s” remains a defensible coarse envelope, not an end-to-end measurement.

### Finding 21 [P2] — Admission says “admit if” twice but never specifies the Boolean rule or unknown handling

**References:** `metrics-and-eviction.md:172-177,187`; `latency-and-azure.md:97`; `metrics-and-eviction.md:75-78,162`.

Frequency says admit if two sessions requested the key, while cost says admit if C≥3·serve_cost. One-shot artifacts are rejected **unless** cost passes; pinned entries bypass **both**. This suggests OR but does not state it, and latency's wording can be read as a mandatory cost condition. A twice-requested cheap 1 s result costing 20 s to serve passes OR and fails AND. An unknown cost/throughput has no admission or eviction fallback; a two-sample prior is called “the single sample” without a selection rule.

**Minimal fix:** one explicit formula with precedence and unknown branches, e.g. pinned OR an explicitly chosen frequency/cost combination. Explain whether frequency alone can override negative serve value. Define bootstrap for current unfinished lazy work and missing production read estimates. Keep evaluation before reservation/PUT URL issuance and the single upload continuation; the latter ordering is now stated correctly.

### Finding 22 [P1] — Ratio semantics are bounded only conditionally; shipped alert expressions have wrong label matching

**References:** `metrics-and-eviction.md:84-87,137-147`.

Entry hit ratio, value ratio and disjoint ceiling are mathematically in [0,1] **when their denominators are positive and inputs obey the intended cohorts**. Empty pools/all-unknown-cost installed pools yield 0/0; no no-data rule exists. The opportunity overlap fix is nevertheless real.

The renewal alert divides the `result="unavailable"` series by an unaggregated vector still labelled by result. Default PromQL matching pairs it with the **unavailable denominator series**, not the total across result values. One unavailable and 99 renewed events therefore produces **1/1 = 100%**, not 1/100. The regret numerator has `reason` while its denominator has `mode`; without aggregation/matching they do not pair. Regret also has no defined unique eviction cohort and can count multiple refetches per candidate, so it is not inherently a bounded percentage. These matching rules are documented in the official Prometheus operators manual's “One-to-one vector matches” section.

**Minimal fix:** aggregate compatible numerator/denominator label sets per pool and define zero/no-data behavior and minimum traffic. Define regret as unique evicted items refetched within a closed window divided by the same eviction cohort; use a reason-aware history/join, not unmatched candidate and refetch event rates. Clearly distinguish mathematical ratios from sums over raw events.

## Q2. Ledger attacks (a)–(f): explicit outcomes and interleavings

These are state-transition witnesses, not simulated claims of a running service. Where I separate the intended transition table from the shown SQL, both are part of the design and the discrepancy itself is a defect.

| Attack | Interleaving and result | Minimal correction / what holds |
|---|---|---|
| **(a) two concurrent uploads of D** | E1 locks/creates head0, inserts pending g1, moves head1, commits. E2 next locks head1 and attempts pending g2. While E1 remains pending, the partial index rejects E2 with an unhandled constraint violation rather than the documented loser/waiter result. If E1 verifies before E2's insert, E2 instead succeeds and moves head to pending g2 despite reusable g1. | **I could not make the partial index permit two simultaneous pending rows.** That safety property holds; the specified winner/loser protocol and verified reuse do not. Finding 1's state branches repair it. |
| **(b) publish versus mark/delete, including >2-minute pause** | Ordinary race: GC marks first→all-distinct pin update is short and must roll back; publisher pins first→its refreshed reference blocks GC. A pause of 121 s still has G protection. Break: pin/ref at t=0, graph insert paused; no graph reference at t=7201→mark/delete/stamp; P resumes→SetStoredPart(g1). | **Short race holds; long-pause safety breaks.** Finding 2. A resumed worker must not retain authority after its protection is retired. |
| **(c) delayed DELETE from deposed leader** | A dispatches DELETE key D/g1, then dies. B retries DELETE D/g1, confirms and stamps deleted; after legitimate reserve g2, upload/verify/publish/mint for D/g2, A's old DELETE finally reaches the store. Its target remains D/g1. | **I could not break byte safety with this delayed-DELETE interleaving if keys are immutable and never reused.** It cannot delete g2. This is a genuine R2 closure. The delayed generation-blind graph cleanup in finding 3 is a separate availability attack, not a claim DELETE itself still has ABA. |
| **(d) crash mark→delete and delete→stamp** | First window: mark g1 deleting; crash before DELETE. Scanner after 15 minutes reissues DELETE(g1), then stamps deleted. Second: DELETE(g1) succeeds; crash before stamp; scanner DELETEs missing g1 idempotently and stamps deleted. Unknown outcome repeats DELETE, not resurrection. | **I could not break these object-side recovery steps under the stated missing-key/delete-success assumption.** However both leave head.state unchanged in the shown SQL; applying the legal reserve guard then cannot progress, or applying its literal SQL illegally bypasses the state. Finding 1. Old graph cleanup also needs finding 3. Do not treat this as an uncaught S3 recovery gap: the scanner is a real improvement. |
| **(e) reserve while deleting** | D/g1 deleting, head.gen1; E2 executes shown reserve: reads head, inserts pending g2, advances head, sends PUT before g1 deletion is confirmed. The witness succeeds with the stated unique constraints because g1 is not pending. | **Broken as SQL, forbidden as prose.** Generation keys prevent g1's DELETE from destroying g2, but the legal transition and no-reserve-while-deleting contract are violated. Finding 1. With explicit state branching I could not break the intended “wait for confirmed deleted” rule. |
| **(f) waiter whose chain partner dies** | E1 owns A; E2 owns B and omits A from its sole continuation. E1 dies. E2 verifies B, sees 1/2 and retries 4 s. Its prescribed next `upload{done:[]}` is wrong-direction/wrong-shape and second-use of an already consumed export. A remains pending until lease expiry; live pending suppression can defer a later donor until then. | **Publication safety holds** if E2 honors the all-digests fence: it cannot publish only B. **The documented abandonment protocol breaks**, finding 6; restart after failed A also requires findings 1 and 5. Correct behavior is internal abandon/defer plus later donor scheduling, not a second upload. |

Additional ledger witness: mint's uncorrelated head join can authorize a non-head generation (finding 4). Failed-generation orphan cleanup and the missing immediate corruption transition also prevent the universal “every generation terminal/eventually collectable” claims from following from this specification (findings 5, 17). A live forever-referenced verified blob need not become terminal at all; those invariants require liveness/unused assumptions, not literal unconditional quantification.

## Q3. Metric computability and ratio audit

- **Grounded facts:** outcomes; exact ordered structural inputs; self/pairing digests; receiver result identity; replacement count; success-only install part/bytes/ns; cache/session resources; miss flags. The source does not supply raw “self duration,” donor cost, per-request TTFB, install-attempt counts or a failed-demand ratio. The new cost median is an explicit estimator and needs no invented donor attribute.
- **Seven classes:** syntactically single-valued but not semantically derived correctly, due to local lookup overhead, multiple outcomes per entry, replacement and partial-install/local-work cases (finding 7). Unknown cannot account for an entire missing decision without a completeness rule.
- **Exclusive work:** not specified correctly; self-time/outermost combination loses work, and causal-parent handling is missing (finding 8). Historical estimates and eviction/admission inherit that bias. A self_digest factors out reference inputs, so estimates also mix input sizes/hardware/cache warmth; that is a calibration limitation to label, not proof the attribute is missing.
- **Net cost:** install cost is not normally subtracted; positive gross avoided work is not positive net value (finding 9). Failed remote attempts are additional unobserved spent work (finding 10).
- **Opportunity sets:** under the explicit spent-subset and tie precedence rules, W+K≤spent. For saved=25, spent=100, W=60, K=40, ceiling=(25+60+40)/(25+100)=1. It cannot exceed 100% for positive denominator; that exact R2 defect is fixed. Pairing candidates remain a diagnostic, not a justified workload hit ceiling or proof of faulty keying. Previous exact occurrence alone does not prove earlier bytes/session compatibility were reusable at lookup; retain the “upper bound” label.
- **Zero denominators / alerts:** all three top-level ratios need no-data semantics. Renewal and regret alert label matching is additionally wrong; regret lacks a bounded matching cohort (finding 22). Unknown-cost population must be displayed rather than hidden as zero benefit.

## Q4. Topology consistency

**No, not every normative section is aligned.** The production primary is now external S3; no surviving explicit prohibition says otherwise. Stateful dcache + Postgres, stable placement and cold restore are consistent in `decision.md:20,26`, `metrics-and-eviction.md:8-9`, `architecture.md:33,104,248` and latency §3.

Surviving contradictory/unscoped clauses are short enough to quote precisely: architecture `:12` says durable blobs/checkpoints are “in MinIO”; decision `:27` puts Tier 2 “on MinIO”; architecture `:330` still capacity-plans the MinIO mitigation using “Premium SSD v2” and the “VM's uncached-disk cap.” The first two need explicit homelab scope or a production replacement; the last is Azure-MinIO advice for a production topology that excludes MinIO. Finding 16. Homelab MinIO discussion itself is valid and should not be deleted merely because production differs.

## Q5. Publication arithmetic and residual risk

All following arithmetic was executed in Python, using decimal 2 GB = 2000 MB and 4 MiB = 4,194,304 bytes:

| Quantity | Calculation | Result |
|---|---|---:|
| Best observed PUT-rate scenario | 2000 / 155.6 | 12.853470 s |
| Eight-writer aggregate-rate scenario | 2000 / 73.4 | 27.247956 s |
| Unsampled path | preceding + 0.0188 ack + 0.00192 HEAD | 12.874190–27.268676 s |
| Read-rate estimate from small hot GET | 4,194,304 / 0.0431 / 1e6 | 97.315638 MB/s |
| Full 2 GB reread at that estimate | 2000 / 97.315638 | 20.551682 s |
| Sequential PUT + full reread + ack + HEAD | sum above | 33.425872–47.820358 s |
| Concurrent units check | 32 MiB / 0.457 | 70.021882 MiB/s = 73.423265 MB/s |

Thus **12.85 is reproduced; 27.3 is imprecise; 13–28 is a coarse fast-path envelope; 34–50 does not follow from the replacement inputs**. Finding 20 is P2 because the principal feasibility correction is sound. For one unsampled export-ready compressed blob, the measured-rate scenario can fit inside 30 s. Compression/export, telemetry, scheduler/slot delay, graph commit and real large-object behavior remain unmeasured, so neither a guarantee nor an impossibility follows. One PUT observation and eight small concurrent PUTs are not a production tail.

The donor/service split is correct **provided the persistent verifier has the reservation/descriptor and a trigger to inspect a completed object after losing the donor reply**. The normal flow still verifies “for each done” (`architecture.md:227`), so merely finishing S3 PUT before death does not guarantee a late publication; dying before delivering `uploaded` is finding 5's existing-object/failed-row case. The text honestly keeps the end-to-end spike open, but should state this evidence/trigger qualification. A two-minute pin and a 90 s p95 target do not bound arbitrary paused workers. Thirty seconds is notice, not a measured node lifetime or total GET-transfer deadline.

## Q6. Verification-policy answer

**The intended sampling policy is defensible, but the invariant/failure/repair claims are not fully consistent.** The correct aligned clauses are architecture `:17,49,150,227` and decision `:33`: all blobs authorized/existence/size checked; 1% content-hashed; engine import checks content. That is a useful trusted-cache optimization and avoids mandatory full service rereads.

It does not prove I3's every-key content property. Decision `:33` also retains “SetStoredPart only in a transaction,” unlike ledger's actual graph insertion outside its SQL transaction; it should reference the normative pin protocol instead of claiming atomicity the library cannot provide. The wrong-bytes row has incompatible compute-fallback/build-failure descriptions, and invalidation/re-export lacks a legal verified-generation transition. Finding 17 (and pin finding 2). With fail-open digest fallback and a specified corruption quarantine/repair path, sampling is a consciously weaker availability/integrity boundary; it is not authorization to accept a wrong application result.

## Q7. Eviction and admission answer

Group-aware **distinct reclaim accounting** addresses the old two-cold-sharers/one-hot-singleton example in concept. The present implementation prescription reverses selection and evicts the hot singleton, quantitatively by a 50× benefit ratio (finding 11). New pathologies are group blast radius/overshoot, hot bridge parts, unknown-cost-as-cold assumptions, and unspecified potentially exponential candidate construction. These must be explicit policy limits, not hidden behind “greedy.”

Quota `SUM(size)` now counts historical generations, and graph removal does not immediately free physical bytes; the delayed-free controller cannot be elided (finding 12). The current new-gen tombstones must be reflected in physical-byte accounting.

Admission now correctly uses exact keys, distinct sessions over seven days, labels k=3 and unmeasured production reads honestly, and states evaluation **before the single continuation**. This closes the old post-continuation ordering problem at the design level. It still needs an explicit OR/AND/unknown rule (finding 21) and corrected cost evidence (findings 7–10). Do not reserve/upload rejected bytes; pinning is an intentional override, not a measured cost justification.

## Q8. Probe answer

**Assertion claims now match** the named conditions in latency §7: bucket create/list status; seed PUT; GET status/size; HEAD status/length; exact finite-to-end range header and size; expiry 403; all eight worker PUT statuses. A failure causes a nonzero result, including a caught worker exception through its zero status. The probes are sized/status checks, not byte-for-byte content-hash validation; no source-payload digest is retained. No PUT-response-body size check or concurrent PUT readback is supplied, so the module header's “every ... size” should not be read as that stronger guarantee.

**Units/order statistics are corrected:** conversion factor 1.048576, n=200 p99 index197/third-largest, n=100 index98/second-largest, n=50 index49/maximum. These choices are valid documented order statistics, not a population tail guarantee. Sequential reads are hot single-key tests, now acknowledged. `bytes=off-(SIZE-1)` matches the engine's request shape despite the colloquial “open-ended” wording.

**New measurement defect:** GET/range/HEAD “TTFB” is measured before response arrival (finding 19), so v3 is not a valid reproduction of latency §0. New PUT connections also include DNS/connect/TLS in the nominal body window; that is a labelled future measurement caveat, not permission to reinterpret the supplied historical PUT observation. No live cluster probe was run for this review.

## Q9. New errors and what can proceed

New R2 defects include unchecked generation advancement/head mirrors (1), expiring/anonymous pins (2), generation-blind native-API cleanup integration (3), mint's uncorrelated head join (4), impossible waiter continuation (6), positive-lookup-time miss classification (7), self-time/outermost undercounting (8), gross savings relabelled as value (9), ungrounded failure-ratio breaker (10), highest-benefit-first eviction (11), historical-generation quota sums and loss of explicit pending-free control (12), circular Phase-0 gate (18), and the probe TTFB regression (19). Findings 13–17 are important surviving contradictions; findings 20–22 refine remaining arithmetic/policy/ratio problems. The supplied diff itself repeats several whole files after their deltas; I read those repetitions but did not treat them as independent changes or evidence of new implementations.

**Proceed now:** discovery/contract captures, end-to-end publication and memory spikes, production read-burst/capability tests, and a simpler Tier-2/baseline characterization. **Do not sign off the Tier-1 design** until legal transitions, publication authority, generation/graph identity, metric populations and eviction direction are corrected and the relevant evidence gates have explicit executable sequencing. This is not a request to fabricate test data or build the service to satisfy a paper review.

### Verification limitations and primary external checks

The local SQL witnesses exercise predicate/uniqueness semantics, not PostgreSQL scheduling, triggers or failover. No implied trigger was assumed where the normative SQL supplies none. The local HTTP witness is a real delayed response test of the actual probe function, not a fabricated MinIO response. Official Python HTTP client documentation distinguishes sending `request()` from obtaining `getresponse()`; the PostgreSQL INSERT manual documents targeted ON CONFLICT handling; the official Prometheus repository's operators documentation specifies default exact-label vector matching. The generic extraction backend failed, a Prometheus site fetch returned 403, and an initial documentation-repository path returned 404; direct official documentation/source retrieval and the executed witnesses provided the needed checks. None of these retrieval failures was substituted with made-up measurement output.
