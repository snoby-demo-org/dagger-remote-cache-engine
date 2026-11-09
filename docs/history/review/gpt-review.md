> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

STOP

# Adversarial round-2 review of Revision R1

## Summary and scope

**19 findings: 4 P0, 13 P1, 2 P2.** Stop Tier-1 service implementation, not Phase-0 investigation or Tier 2. The revision improves several specific defects, but its publication SQL cannot publish ordinary deduplicated chains, its metrics still lack a coherent event population and donor-cost join, and its claimed fencing does not cover delayed external effects. Discovery remains explicitly unproved.

This is a review of a deliberately unimplemented design, not a demand for service code or compilation. I read the brief, R1 ledger, four current design documents, round-1 review, full R1 diff, baseline documents and probe. I also inspected the existing pinned Dagger checkout identified by `recon-protocol.md:7-10`: HEAD is `7e9eaacc376e5c3bed3efdca9e1ba750bc8d9233`. A targeted diff against beta.25's commit showed no differences in `engine/telemetryattrs/attrs.go` or `dagql/cache_part_content.go`. References to these source paths below mean that checkout. The measured facts supplied in the brief are accepted; this review does not claim a new cluster measurement.

Verification performed: Python arithmetic, in-memory SQLite checks of the relevant SQL predicates, and deterministic state-transition witnesses. These are not PostgreSQL concurrency tests, a live S3 experiment, or a real-engine contract test. No PostgreSQL server was available at the local default socket. Only this review file was written; no design, probe, manifest or source was changed.

## 1. Required round-1 P0 closure assessment

| Round-1 item | Actual text checked | Round-2 disposition |
|---|---|---|
| **F5: discovery** | `decision.md:61` explicitly says discovery is unproven and requires a spike before service code. `architecture.md:95,223-224` still relies on call spans. | **OPEN, acknowledged rather than closed.** Naming the prerequisite is good scope control, not evidence that the discovery path works. Finding 1. |
| **F12: publish racing delete** | `architecture.md:53` now requires a transaction stamping **all distinct verified digests**, rollback on any missing digest, and no `SetStoredPart` on failure. | **The original partial-rowcount bug is closed in I5. The end-to-end publication algorithm is not closed.** `metrics-and-eviction.md:156-162` substitutes a different, pending-and-owned-only predicate that fails on shared/reused blobs; SQL commit and graph publication remain separate. Findings 2 and 4. |
| **F20/F21: metrics** | `metrics-and-eviction.md:47-50` switches to installation evidence, converts nanoseconds, and proposes a lazy-span join. | **PARTIALLY corrected, still OPEN as a computable metric model.** The joined receiving ID does not supply the donor's counterfactual computation, part and entry populations are mixed, and metadata-only recomputation is absent from the stated hit-ratio denominator. Findings 3 and 13. |
| **F30: eviction enum / mark-only** | `metrics-and-eviction.md:169-177` uses `deleting`; line 177 explicitly restricts dry-run writes to the candidate timestamp and a counter. `architecture.md:53,121` supplies the timestamp. | **CLOSED for the exact original enum/mark-only defect.** It no longer changes availability during dry run. The broader lifecycle is still incomplete: schema includes `failed`, timestamps are nullable, and crash recovery of `deleting` is unspecified. Findings 5 and 6 are distinct remaining defects, not claims that mark-only still disables blobs. |
| **F51: engine memory** | `architecture.md:79-81` proposes a **12Gi limit / 6Gi request**, and `architecture.md:55` correctly states that engine OOM kills builds. `architecture.md:298` still leaves merge memory unknown. | **The proposed 4Gi-versus-reported-6GB sizing error is corrected; measured closure remains OPEN.** A 12Gi proposal is not a demonstrated ceiling for concurrent merges and actual workloads. The preserved live manifest is still 4Gi (`review/live-helmrelease.yaml:77-81`), but it describes the existing engine, not a completed beta rollout. Do not mistake that unchanged snapshot for evidence that R1 deployed anything. Require the already-planned fresh-engine peak-RSS/merge-concurrency measurement before rollout. |
| **F36: publication budget** | `latency-and-azure.md:15-25` separates PUT body time from response wait and accounts for verification, but asserts 26–55s and an unconditional inability to publish in 30s. | **PUT timing interpretation corrected; publication feasibility still OPEN, and the new categorical conclusion is wrong.** Sampling, units, end-to-end unmeasured costs, and the persistent verifier's ability to finish after donor death all matter. Finding 9. Round 1 labelled F36 P1; I assess it here because the brief explicitly requests it. |

Credit also due: durable mint-before-send, restore-time dropping of nonverified digests, the consistent quota watermarks, dropping `repo`, and accepting cold restore are material improvements. None requires pretending the outstanding spikes have already run.

## 2. Sequential findings

### Finding 1 [P0] — Discovery is still a blocking hypothesis; the retained fallback cannot discover fresh work

**References:** `decision.md:13,61`; `architecture.md:95,223-224,335-336`; `recon-protocol.md:18,63-72,450,491`; `revision-r1.md:90`.

R1 expressly admits that no test has shown call spans reaching the self-hosted URL. There is no inventory message and no on-miss request to replace that channel. The retained R7 fallback says to export recently executed roots using `engine.share`/`engine.stop`, but stop carries no entry numbers, and share is an imported-row snapshot-sharing path, not an inventory of newly executed roots. A fresh engine emitting only lifecycle events still cannot seed this cache by the stated algorithm.

**Required closure:** a beta.25 telemetry capture with the actual engine and client configuration, recording cache/session identity, result numbers, structural attributes, completion facts and delivery delay. Inspecting existing Tempo traces can establish attribute availability, but does not by itself prove the differently configured self-hosted Cloud route. Until then, do not call F5 closed or rely on the fallback.

### Finding 2 [P0] — The rewritten publication SQL rejects reused blobs and waiting duplicate exports

**References:** `metrics-and-eviction.md:144-162`; `architecture.md:53,160-162,225-227,235`.

I5 and the metrics document implement different operations. I5 stamps every **already verified** digest. The rewritten SQL instead transitions rows only where `state='pending' AND export_id=$3`, then requires that update's rowcount to equal the entire distinct chain.

Concrete ordinary case: chain `[A,B]`, where A is already verified from an earlier part and B is newly pending for export E. The engine correctly omits A from the upload. The SQL updates B only: **1 row, required 2**, so it rolls back. Retrying E cannot help: the reservation upsert will not turn a verified A into E's pending row. This is the normal layered/deduplicated case, not an exotic failure.

Concrete duplicate case: E1 reserves A; E2 loses the conflict, omits A and waits as prescribed. E1 uploads and verifies A. E2's pending-and-owned predicate now matches **zero** rows forever. A chain split across two reservation owners also cannot satisfy the all-owned predicate. Even retrying publication after an already-committed verification finds no pending rows.

The in-memory predicate checks returned 1/2 for the mixed chain and 0/1 for the waiter. They do not test PostgreSQL locking, but the predicate contradiction is independent of locking.

**Required closure:** separate owner-fenced per-blob verification from an all-distinct-digests, verified-state publication fence. Specify how waiters resume and how idempotent publication works. Do not replace the correct I5 predicate with the pending-owner transition.

### Finding 3 [P0] — The redefined hit/saved model still cannot be computed as specified

**References:** `metrics-and-eviction.md:16,27-52,111-116`; `architecture.md:260`; `engine/telemetryattrs/attrs.go:627-647,731-788`; `dagql/cache_part_content.go:586-608,710-714`.

The attributes exist, and installation is a better success signal than `hit.remote`. The remaining problems are semantic joins and event populations:

- **No counterfactual cost on the receiving ID.** `cache.offer.part` names an installed part on the **receiving** entry. The lazy span on that `(cacheID,result.id)` measures import, not the work skipped. The donor's execution has a different cache/result namespace. No rule maps donor cost, version/replacement, part, and receiving installation, selects a historical estimate, or handles missing donor telemetry. Applying line 48 literally to the receiver uses installation work as its own recomputation baseline. No `cache.offer.recompute_seconds` or donor-ID attribute is grounded by the cited producer contract.
- **Part and entry granularity disagree.** The producer installs one part per installation span, but the counter deduplicates only `(cacheID,result.id)`. A Container can install multiple parts; the specification neither counts them separately nor says which one represents an entry hit. Applying the full entry recomputation cost to each part overcounts; collapsing all parts loses events and install cost. Replacement and session boundaries are also absent from the counter's identity despite line 50 requiring per-session distinctness.
- **The denominator still misses the original failure case.** A metadata-only remote shell emits `outcome=hit`, then computes in a lazy span with **no outcome**. With no installation and no `outcome=executed` call for that work, it contributes neither term of `installs/(installs+executed)`. A fallback from failed installation has the same classification gap. Conversely an entry can both install one part and compute another, so entry-level hit and executed sets are not inherently disjoint.
- **Lazy sums are not exclusive time.** Line 48 sums lazy-span durations and makes only the call span exclusive, despite saying never to naively sum nested spans. Nested/overlapping lazy work needs a specified interval/causal attribution rule, not that admonition. Import spans also need explicit treatment: they are lazy spans, but not original computation.
- **Route is not attached to install evidence.** `cache.hit.route` is a call lookup attribute, not an installation attribute. The catalogue assumes `dcache_installs_total{route}` is available without specifying a causal call join, handling multiple ancestors, or defining merge-vs-live-offer route instead.

**Required closure:** one explicit event schema and join algorithm, including per-part versus per-entry units, donor/recipient mapping, session and replacement identity, metadata-hit-local-evaluation classification, exclusive-time attribution and an unknown-cost bucket. Validate a seed, pre-merge hit, multiple-part hit, metadata-only recomputation, failed import fallback, and repeated local hit. This is a design requirement, not a request for an implementation to exist today.

### Finding 4 [P0] — The fence does not cover the graph-publication gap or old S3 DELETE effects across generations

**References:** `architecture.md:53-54,125-126,171,227,341-342`; `metrics-and-eviction.md:156-178`.

The rowcount correction serializes competing SQL updates; it does not make PostgreSQL, the in-memory graph and S3 one transaction. Two remaining witnesses matter:

**Publication gap:** publish commits the verified-row reference stamp at time t, then its worker blocks before `SetStoredPart`. Heartbeats can continue in other workers. A sweep sees no graph reference. After G has elapsed, the SQL age checks permit marking and deleting the blob. The original worker then resumes and calls `SetStoredPart` without a fresh ledger check, publishing a deleted chain. There is no documented upper bound or cancellable publication pin across this gap. The normal short-gap race is fixed; the unconditional invariant is not proved.

**External DELETE ABA:** leader A marks a digest deleting and dispatches an S3 DELETE which is delayed. A dies or loses its connection/lease. Leader B recovers. If B retries the outstanding delete to make progress, its own DELETE can complete, after which it removes the row. B can then reserve the same digest/key again, upload, verify, publish and mint a fresh URL. A's previously dispatched DELETE finally reaches the store and removes the **new** object. The ledger remains verified and the fresh URL returns 404. A need not resume service code or pass another epoch check: its HTTP request was already outside PostgreSQL. A SQL transaction cannot roll back that request. If recovery never retries, the design instead has Finding 6's permanently deleting row.

Checking a leader epoch before issuing DELETE, even inside a SQL transaction, is insufficient to fence that in-flight object-store effect. The SQL shown does not include the epoch check at all; the only concrete epoch transaction described is checkpoint-pointer CAS.

**Required closure:** specify the graph publication reservation lifetime and the object-generation/delete recovery protocol. Distinguish an object version or generation from its digest; identical-content ETags alone do not identify successive lifetimes. If the backend cannot fence delayed deletes, narrow the availability guarantee and define a safe alternative. Do not claim the current SQL is a complete linearizable ledger specification.

### Finding 5 [P1] — NULL mint timestamps make never-served verified objects unevictable

**References:** `architecture.md:116-122,171-174`; `metrics-and-eviction.md:146-159,165-174`.

Reservation omits `last_url_mint_at`, verification never initializes it, and the schema provides neither a default nor NOT NULL. The first verified state of a newly uploaded, never-served blob therefore has a NULL mint timestamp. The GC condition `last_url_mint_at < now()-G` does not pass for NULL. A root can be pruned and become absent from `LiveBlobs()` forever without becoming deletable.

The in-memory SQL predicate check changed **zero** rows for exactly this state. This is particularly damaging for one-off branch builds: the entries with no installations are precisely those that never receive the missing initialization. The pressure controller can count such bytes as pending-free and still never reclaim them.

**Required closure:** explicit timestamp initialization/NULL semantics and a never-minted-object GC case in the ledger specification.

### Finding 6 [P1] — Reserve, failed cleanup and deletion recovery still are not an executable state machine

**References:** `architecture.md:118-121,161-162,171-172,240`; `metrics-and-eviction.md:148-155,169-176`; `revision-r1.md:96`.

The architecture retains insert-on-conflict-do-nothing while the rewritten metrics document specifies takeover upsert. The schema and reserve predicate use `failed`; the purported single enum lists only pending/verified/deleting. More importantly, reserve can take over a deleting row only when `delete_committed_at < now()`, but no shown statement sets that timestamp, and the prescribed successful delete removes the row entirely.

A crash after marking `deleting` but before object deletion leaves a row that cannot be reserved and is not selected by the shown verified-only GC mark. A crash after S3 deletion but before row deletion has the same problem. There is no specified recovery scanner, owner/generation for delete work, successful-delete stamp, or resolution of an unknown S3 outcome. The undefined recovery is not supplied by restore revalidation: that only drops graph parts.

The five-minute capability also does not establish an upload-completion deadline; the engine's one-minute bounds are idle/header-wait bounds, not a maximum transfer duration (`recon-protocol.md:159`). The design allows lease takeover but does not fully define how already-running old uploads are accounted for. URL expiry is not a general transfer drain guarantee; AWS documents that a download already started can outlive capability expiry. [S3]

**Required closure:** one legal transition table with ownership, deadlines, retry/recovery and terminal outcomes, consistent across documents. R1 itself calls the SQL a sketch; its disposition table should not present that as complete execution semantics.

### Finding 7 [P1] — Renewal simultaneously forbids and requires a PostgreSQL round trip

**References:** `architecture.md:53,99,134,253`; `metrics-and-eviction.md:163-168`; `revision-r1.md:28,66-67`.

The new signer responsibility says the hot path must not need a PostgreSQL round trip. The new mandatory mint rule requires a conditional committed database write **for every merge or renewal URL**. An in-memory chain index can remove graph traversal, but cannot replace the mandatory stamp or authoritative verified-state decision. A digest-keyed ledger also does not by itself reconstruct ordered part-chain membership.

These requirements cannot both be implemented literally. Bypassing PostgreSQL for speed reinstates the race R1 was intended to close; performing the update violates the no-round-trip clause. The two-second deadline requires a database-inclusive latency budget, not an impossible guarantee.

**Required closure:** explicitly allow the bounded durable stamp on the renew path and define which operations the index actually avoids. Missing cached metadata is not necessarily a cryptographically bad key.

### Finding 8 [P1] — Sampling contradicts the publication invariant and the claimed immutable-key proof

**References:** `architecture.md:17,49-52,155,227,258`; `decision.md:33-35`; `latency-and-azure.md:25-26`.

I1 now says verified means the service streamed and checked the digest, then says that verification is sampled at 1%. The overview and cold flow still require rehashing **every** blob. The wrong-bytes failure row still claims verification catches bad content before publication. HEAD plus a correctly associated PUT capability establishes existence/size/authorized target, not the content hash of the remaining 99%.

Sampling can be a defensible trusted-cache tradeoff because the engine verifies imports. It is not the same invariant. Nor does single-PUT atomicity prove that the bytes hash to the key: it prevents partial replacement, not incorrect complete content. Outstanding PUT capabilities also are not revoked merely because the ledger becomes verified.

**Required closure:** choose and state one guarantee. If sampling is intended, distinguish authorized/existence-checked from hash-checked content, describe corrupted-object invalidation/re-export, and carry the same policy into latency and egress budgets. Do not present sampling as proof of the stronger all-bytes-verified statement.

### Finding 9 [P1] — The new 30-second impossibility claim is false; the transfer arithmetic and donor deadline are conflated

**References:** `latency-and-azure.md:15-25`; `metrics-and-eviction.md:97,212`; `revision-r1.md:54,93`; `architecture.md:98,136,227,242`.

Using the supplied rates and decimal **2GB = 2000MB**, arithmetic gives:

- PUT body at 155.6MB/s: **12.85s**; at the supplied 70.1 aggregate: **28.53s**. Treat the latter only as a rough scenario, not a measured single-layer lower bound.
- A full verify reread at the document's approximate 93MB/s: **21.51s**.
- Sequential PUT + full reread + 18.8ms response wait + 1.92ms HEAD: **34.38–50.06s**, not the stated 26–55s from the displayed rates. The supplied GET total instead implies about 97.3 decimal MB/s for this small-object test, illustrating the document's MiB/MB mixing.
- With the stated 1% hashing policy, the unsampled path has **no full reread**. The fast-path PUT-plus-ack-plus-HEAD estimate is about **12.87s**, before unmeasured compression/scheduling. Even its expectation including 1% full rereads is only about **13.09s** at that upload rate; an expectation is not a tail guarantee.

Thus the measurements do **not** prove every 2GB layer can publish within 30s, but they also do **not** prove it cannot. A precompressed/export-ready layer on the unsampled path is a direct counterexample to the categorical argument.

There is a second distinction even under full hashing: after the PUT completes, the persistent verifier can finish reading from external storage and publishing **after the donor dies**. The donor's remaining lifetime must cover obtaining/exporting/uploading bytes, not necessarily the service's entire span-end-to-verified latency. A 34s publication may survive a donor death at 30s if its PUT was complete in time. Conversely short uploads may still fail because telemetry and export/compression consume the notice window.

**Required closure:** separate donor-dependent exposure from service verification/publication latency, sampled versus full-hash paths, compressed descriptor size versus uncompressed layer size, and notice versus lifetime. Keep the end-to-end spike open. A 90s SLI threshold is not justified as an impossibility theorem about 30s notice.

### Finding 10 [P1] — The per-part admission model is a useful scaffold, not a calibrated decision rule

**References:** `latency-and-azure.md:68-85`; `metrics-and-eviction.md:120-128`; `s3_latency_probe.py:93-116,135-150`; `decision.md:49`.

Summing layer startup, transfer and application is dimensionally better than comparing one request's p99 to an operation median. `k≈3` is defensible as a conservative **initial policy choice**, not as a measured optimum or guarantee. The text overclaims that its terms are measured:

- 70–155MB/s are **PUT** observations; serving needs read throughput under demand concurrency. Neither an eight-small-PUT burst nor a hot 4MiB GET establishes cold, large-object production reads.
- `apply`, renewal/stall probabilities, overlap of applying layers with transfer, and actual numbers of downloaded rather than locally reused layers are unmeasured. The source explicitly says download and application interleave (`engine/telemetryattrs/attrs.go:782-787`).
- A slow continuous read is not limited to 30s: the engine bound is per idle stall, not total transfer (`recon-protocol.md:356`). A failed remote attempt can incur transfer time, fallback and original recomputation, not just a universal 30s penalty. Renewal's 2s is a deadline, not the usual duration of every successful renewal.
- The frequency sketch has a key and window but no admission threshold or comparison rule. The only normative admit-if condition is cost-based. It therefore does not reject expensive one-use branch artifacts merely because they never repeat, and the causal ordering of learning descriptor sizes versus sending the one permitted upload continuation is unspecified.

**Required closure:** label assumptions, estimate read/apply/failure terms in the actual production topology, define frequency's role, and measure net job time against the warm local baseline. Avoid adding a complex frequency controller before the metric joins work.

### Finding 11 [P1] — Marginal-byte scoring can protect cold shared groups while evicting a much hotter singleton

**References:** `metrics-and-eviction.md:111-118,185-186`; `architecture.md:174`.

Marginal bytes are the right input for **immediate reclaim accounting**. They are not, alone, a complete replacement-cost denominator, and the statement that the change prevents a cascade is too strong.

Concrete equal-age example, after the retention floor: P and Q each reference the same 1GiB blob, have no unique bytes, and have each seen one 60s-benefit install. R references a separate 1GiB blob and has seen 100 60s-benefit installs. Under the given formula, each of P/Q scores `60/1MiB`; R scores `6000/1GiB`. Each cold sharer scores **10.24 times higher** than the much hotter R, so the policy evicts R first. Removing P and Q together would release the same bytes at a combined historical benefit of only 120s instead of R's 6000s. No rule considers that shared eviction group.

If zero-marginal candidates are skipped, a pool dominated by shared groups can have no reclaimable candidate and remain admission-stopped. If they are not skipped, a never-installed shared part has score zero despite the small denominator, so it can still be pruned while freeing nothing; recomputing marginal bytes can abruptly transfer the entire charge to its last sibling. The controller must recalculate distinct projected frees after each closure change and cancel projected frees if new references appear. Neither operation is specified.

**Conclusion:** install-only evidence removes false popularity from mere offers/merges; it does not prevent extinction of hot entries under this scoring rule. Shared-group myopia, zero-install ties, feedback starvation and pressure/accounting rules remain. Define a simple group-aware fallback or explicitly accept this policy limitation before calling F31 solved in full.

### Finding 12 [P1] — Install-only popularity is undermined by merge-refreshed retention

**References:** `architecture.md:170,231`; `metrics-and-eviction.md:112,134-135`; `revision-r1.md:29-30,46`.

The scoring definition says only installation is serving. The retention rule still refreshes on **install and merge**. The 30-day override applies only to roots with **no install ever**.

Counterexample: a root is installed once, becomes popular enough to be selected for every fresh engine's warm merge, but is never demanded again. Every pre-merge extends its seven-day retention; the no-install-ever override no longer applies. It can survive indefinitely without a new install, contrary to the stated goal of removing merge-driven reinforcement. A never-installed root does have a finite 30-day override, but that does not close this case.

**Required closure:** distinguish recipient ownership edges from authoritative pool retention/popularity. If merges must extend recipient retention, they need not refresh pool demand history. Define the 30-day idle condition using last genuine demand rather than a once-ever Boolean, or document the intended exception.

### Finding 13 [P1] — The addressable ceiling can exceed 100% and attributes legitimate changes to broken keying

**References:** `metrics-and-eviction.md:40-52`; `architecture.md:192-197,264`; `decision.md:54,61`; `engine/telemetryattrs/attrs.go:691-718`.

Making `would_hit` and `lost_keying` subsets of spent fixes denominator duplication but not overlap. A current executed entry can match prior A on exact self+inputs, and prior B on pairing digest with a different self digest. The rules count the same work in both subsets. With 100s spent and no saved work, classifying all 100s in both produces **200%**. The predicates must be mutually exclusive or combined as a union before duration aggregation.

Equal pairing digests are expressly **candidate correspondence**, not proof of a hit or of a faulty per-session key. Different legitimate implicit inputs and different structural inputs can make reuse invalid; pairing factors those distinctions away. The formula does not require identical ordered inputs for `lost_keying`, inspect changed implicit inputs, check value expiry/session requirements, or require a completed byte-bearing reusable part. Nor does its default wording require a different earlier session, unlike the architecture's Phase-0 paragraph.

Finally, the Phase-0 prose still says to weight call spans by duration, whereas the revised model says those spans miss expensive lazy work. Aggregated exclusive work-seconds across parallel branches are not the same as **job wall-time** savings. A threshold expressed as 30% of wall time needs a causal/critical-path or observed job-level comparison, not just a work-seconds ratio.

**Required closure:** separate reachable cache opportunity from diagnostic keying candidates, use disjoint sets and explicit earlier-session/part eligibility, and align the Phase-0 query and denominator with the rollout gate.

### Finding 14 [P1] — Metadata-only values still suppress the very re-export intended to repair them

**References:** `architecture.md:160,224-227,235,240`; `metrics-and-eviction.md:128`; `revision-r1.md:25,74`.

The new capability/visibility decision paragraph does not replace the scheduler's retained recipe rule: skip export when `EquivalentEntries` shows a stored **value**. The cold flow commits that value before any upload. If the first donor dies, or admission rejects its bytes, metadata remains and the same predicate suppresses later donor export. The promise that another donor fills the bytes is still not executable under that rule.

The ledger's R1 change labelled F15 concerns import disk growth, not the round-1 review's F15 scheduler defect; the original stored-value-versus-stored-part distinction remains. This is not fixed by defining `alreadyComplete` more carefully because that is a different decision point.

**Required closure:** deduplicate export by the particular requested part's usable stored availability or a bounded live reservation, not merely value existence. Show the first-donor-fails and admission-reconsidered paths in the design.

### Finding 15 [P1] — The five-second upload continuation still waits behind the graph commit it is meant to survive

**References:** `architecture.md:225-226,257,348`; `revision-r1.md:75`; `recon-protocol.md:148,157,380,434`.

The cold flow still calls `CommitValueMerge` before preparing/sending upload. That commit necessarily takes the graph write lock. R1's rule that merge insertion happens outside the dagql graph lock cannot literally remove a lock required by the imported library; expensive preparation outside a service work lock is a different thing.

If commit waits beyond five seconds, either the service misses its own continuation deadline, or the watchdog consumes the export with an empty URL map and releases its providers before the commit finishes. On a dying donor that loses the only available export. R1 has not applied the round-1 recommendation to enumerate descriptors/reserve/send the upload continuation independently of the slow commit.

**Required closure:** an explicit dependency graph for bundle validation/preparation, descriptor enumeration, upload continuation and metadata commit. Preserve graph locking needed by dagql rather than specifying its impossible removal.

### Finding 16 [P1] — Production topology is still accepted, rejected and replaced in different normative sections

**References:** `decision.md:20,26,49`; `metrics-and-eviction.md:8`; `architecture.md:12,215,283`; `latency-and-azure.md:48-60,62,72-76`.

The supplied topology is unambiguous: external S3 in production, homelab MinIO for design tests. R1 adds that fact to the architecture's opening but retains two explicit prohibitions of off-cluster S3 as primary. The Azure options still recommend MinIO on AKS, while the metrics document says production has no MinIO. This is not an invitation to change the user's constraint: the rest of the design must conform to it.

Consequently the production cost/latency/admission and breaker budgets still describe incompatible paths. Outside a Kubernetes cluster does not itself imply cross-region or public-WAN transit; location, endpoint and provider must be specified rather than assuming either zero egress or generic internet S3. Azure disk-sizing advice for an in-cluster MinIO deployment does not size the declared external primary.

`latency-and-azure.md:62` also calls dcache the only stateful service in the same sentence that admits stateful Postgres, and points to a nonexistent metrics section 7. These are retained contradictions, not evidence the statefulness issue was fully reconciled.

### Finding 17 [P1] — The stated direct byte path cannot supply the proposed production S3 latency/egress breaker by itself

**References:** `architecture.md:12,104,250-251`; `decision.md:26,49`; `metrics-and-eviction.md:17,79-81,105`; `latency-and-azure.md:66,68-85`.

Engines perform PUT/GET directly against presigned object URLs. The service's own S3 client can observe HEAD, sampled verification GET, checkpoint and delete requests, but not every engine GET's response time, bytes or failure. `cache.offer.bytes/duration_ns` gives completed part-level import data, not all attempted request TTFBs, range reopens, stalls or failed demand episodes. It is also delayed telemetry.

No object-store access-log/metrics ingestion, engine HTTP instrumentation, representative active probe or attribution rule is specified to bridge that gap. Using the service's warm verifier reads as `s3_get_p99` can report healthy while engine reads over a different production path stall. A per-request latency breaker also must state whether it measures TTFB or total time; large legitimate reads and HEAD-sized probes are not comparable.

**Required closure:** name the measurement point and exporter for engine-path request latency and egress, define an active health signal and recovery hysteresis, and distinguish it from install-level SLIs. Do not silently solve this by proxying bytes, which would change the declared byte plane.

### Finding 18 [P2] — The rewritten probe's assertions, percentile explanation and units still disagree with its code

**References:** `s3_latency_probe.py:8-10,52-55,89-97,109-125,128-150`; `latency-and-azure.md:7,15-18,27,60,107`.

The new protocol-shaped presigning, range-header validation and HEAD body closure are real improvements. Remaining evidence-quality problems:

- Expiry is **printed**, not asserted; HEAD status and size are unchecked. Range status/header are checked but its returned byte count is ignored. Initial reachability and the first PUT status are printed, not asserted.
- Worker PUT statuses are ignored; thread exceptions do not cause the main thread to fail or require eight successful results. The script can print aggregate throughput without eight successful writes.
- There is one serial PUT observation, not a PUT p50 from n=200. The n=200 run is GET; the documentation/table should not imply all operations have that sampling basis.
- `8*4/wall` reports **MiB/s**, labelled MB/s. The supplied 70.1 figure remains an accepted observation for this review, but the script's formula at 457ms is about 70.0MiB/s or 73.4 decimal MB/s. It cannot be labelled both without conversion. `latency-and-azure.md:60` additionally calls an explicitly aggregate figure per-writer.
- At n=200 this exact `round(.99*(n-1))` implementation chooses zero-based index **197**, the **third-largest** observation, not second-largest as the revised text states. The caution about weak tail evidence is right; its order statistic is still wrong.
- All sequential GETs still hit the same 4MiB key; the distinct objects are in the PUT burst, not a cold-read/concurrent-read experiment.

These defects do not license discarding the brief's supplied measurements. They limit what the artifact asserts and what future runs can prove.

### Finding 19 [P2] — Checkpoint retention and cost still use the superseded schedule/lifecycle policy

**References:** `architecture.md:33,100,146,206-209,248`; `metrics-and-eviction.md:102,208-210`; `decision.md:20,26`.

The new policy is change-triggered checkpoints with a minimum ten-minute interval, last five retained by pointer and cold restore. The object-layout lifecycle still expires checkpoint objects after seven days, the cost table still assumes one checkpoint per minute, and the diagram still says warm restore.

Most importantly, seven-day lifecycle expiry can delete **the pointer's current checkpoint** if the graph is unchanged or uploads are failing for that long. Last-K pointer retention does not protect against an independent age-based bucket delete. This is cache-only state loss, not build-data corruption, but contradicts the accepted restore policy. The checkpoint alert's `rate(...seconds_sum)>30` also tests roughly thirty seconds of checkpoint time per wall-clock second, not a thirty-second checkpoint; it is not the advertised duration alarm.

## 3. Metric attribute audit — what is real and what is assumed

This audit does **not** repeat round 1's incorrect F27/F28 claims.

| Input / proposed output | Evidence and derivability |
|---|---|
| `dagger.io/cache.offer.part`, `.bytes`, `.duration_ns` | **Grounded.** `engine/telemetryattrs/attrs.go:772-788`; emitted by `dagql/cache_part_content.go:592-608,710-714`. Identifies a receiver's installed part, actual downloaded bytes, and nanoseconds through installation. It does not report original recompute time, donor ID, job speedup, or every underlying request. |
| Lazy `cache.result.id` with no outcome | **Grounded.** `attrs.go:731-742`. Join to the cache identity resource, not globally by number. It identifies the evaluated receiver entry, not a corresponding donor in another cache. |
| `cache.structural_inputs` | **Grounded, not invented.** `attrs.go:698-708`, emission at `core/telemetry.go:326-338`. Preserve the **ordered native string array**, including empty-present versus absent. Exact array equality is a computable conservative identity match; it is not the engine's full evolving equivalence-class lookup. |
| `cache.self_digest` / `cache.pairing_digest` | **Grounded.** `attrs.go:691-718`. Pairing equality is candidate correspondence, not permission to reuse nor proof a difference is a per-session-keying bug. |
| cache and session identity | **Grounded resource fields.** `recon-protocol.md:44-46,129`. They are not an extra `cacheID` attribute assumed on each span. Session identity must actually enter the proposed per-session aggregation key. |
| route | **Grounded only on lookup hits.** `attrs.go:627-647`; `core/telemetry.go:293-300`. Installation route requires a separately specified causal join or a different internally derived route definition. |
| successful install evidence | Computable as observed install events, subject to delivery gaps and choosing an identity including part/session/replacement as appropriate. OTLP replay/dropped-span handling and completeness must be defined before labelling it an exact fleet total. `attrs.go:790-793` even exposes a session cache-span count that could help detect incompleteness; R1 does not use it. |
| donor part compute cost / avoided computation | **Assumed, not supplied by these install attributes.** No documented `cache.offer.donor`, `cache.offer.recompute_seconds`, or per-install donor-cost field. Could be derived through a designed metadata/telemetry association, but R1 does not define one. |
| saved counter / duration-weighted hit ratio | Not established by available install evidence alone. Negative net savings are possible when a remote path is slower than recompute; the document must choose a signed accumulation or separate saved/lost accounting rather than assume a nonnegative `_total` and a bounded value ratio. |
| `saw_expired`, incompatible/session-resource miss flags | **Grounded.** `attrs.go:656-689`. Expired-candidate existence does not identify a particular service eviction, its reason or a tombstone. Regret still needs that join and matching numerator/denominator cohort. |

Missing joins are not missing attributes that should simply be declared real. The right resolution is an explicit derivation, an estimate labelled as such, or an unknown observation — not another metric name.

## 4. Requested concurrency attack results

| Attack | Result |
|---|---|
| **(a) Two simultaneous reservations of one digest** | I cannot break initial exclusive ownership if the upsert's affected-row result gates URL issuance. One wins and the other gets no PUT URL. PostgreSQL's conflicting-update recheck supports that ordinary row-level serialization. However, the SQL omits an explicit returned ownership result, and the follow-on waiter cannot publish under the pending-owner predicate: Finding 2's exact E1/E2 witness. Different-owned digests in one chain likewise deadlock publication logically. [S1] |
| **(b) Publish racing delete, original two-blob case** | I cannot reproduce the original partial-rowcount publication using I5's corrected all-distinct-verified transaction. Delete first means publication aborts; publication first stamps references and makes the age check fail. But the rewritten SQL is not that transaction, and a publication worker paused longer than G before graph insertion breaks the unconditional invariant: Finding 4. |
| **(c) Failover after a committed mint, before deletion of that same generation** | I cannot make immediate failover erase the stamp: it is durable Postgres state, independent of the older graph checkpoint. If the new leader obeys that state and uses short transactions, delete is prevented for G. This improvement genuinely closes the old buffered-mint race. The remaining failover attack is an **already-dispatched old DELETE** outliving its owner and a new object's mint; Finding 4 gives the ABA sequence. Recovery must resolve both safety and the stuck-state alternative in Finding 6. |

Two-hour grace is an operational margin, not a mathematical upper bound on all transfers: the engine's idle bound does not bound a slow but progressing download (`recon-protocol.md:356`). Stamp time also must track actual capability issue time. PostgreSQL `now()` is transaction-start time; a transaction left open for a long time can commit an already-old stamp. R1 specifies no transaction-duration or stamp-to-send deadline. This does not negate the ordinary race fix, but prevents claiming a general outstanding-read guarantee from G alone. [S2]

## 5. Contradiction map introduced or exposed by the patches

The full consequences are in the numbered findings; these are the exact incompatible clauses.

| Patched clause | Incompatible clause | Finding |
|---|---|---|
| `architecture.md:53`: publish requires verified rows | `metrics-and-eviction.md:158-160`: publish requires pending rows owned by this export | 2 |
| `architecture.md:99`: renew needs no PostgreSQL round trip | `architecture.md:53,253`; metrics `:163-168`: every renewal mint must commit a database stamp | 7 |
| `architecture.md:49`, decision `:33`: default 1% hashing | architecture `:17,49,227,258`: every published blob rehashed / bad bytes caught before publish | 8 |
| metrics `:112`: only install is served | metrics `:134`; architecture `:170`: merge refreshes retention, with no-install-ever override only | 12 |
| metrics `:169`: three-state single enum | architecture `:118`; metrics `:151,176`: `failed` is a real fourth state used by the algorithm | 6 |
| New last-five pointer retention, architecture `:100` | old checkpoint age expiry, architecture `:146` | 19 |
| New external-S3 production opening, architecture `:12` | retained primary-tier prohibitions `:215,283`; revised Azure recommendation `latency-and-azure.md:48` | 16 |
| F59 correction, architecture `:86`; decision `:87` | retained FQDN NetworkPolicy mitigation, architecture `:336` | 1; retained contradictory mitigation, not an additional counted finding |
| New no-hashing default for 99%, architecture `:49` | revised full-reread publication impossibility budget, latency `:25` | 8–9 |
| New ten-minute checkpoint floor / cold restore, architecture `:100,248` | old one-minute cost table `:208` / warm diagram `:33` | 19 |

R1 is a ledger of changes, not an authoritative override mechanism that resolves contradictory normative text. Its claim that 61 findings hold does not mean all 61 recommended changes were applied. In particular, discovery, memory and end-to-end publication are openly deferred, while part-availability scheduling and upload-before-slow-commit remain unchanged.

## 6. Top three cuts, ranked for a fail-open cache

1. **Cut the composite value/frequency/regret controller from v1.** References: `metrics-and-eviction.md:109-128,181-198`. It requires counterfactual per-part cost, exact installation attribution, historical sketches, shared-group optimization, distinct pending-free accounting and reason/cohort joins before basic savings are measured. Start with install/demand recency, a quota, correct distinct-byte accounting and simple retention. This does **not** mean cutting the safe publication/delete fence.
2. **Defer live-offer scheduling and retry sophistication.** References: `architecture.md:97,157,232`; round-1 `review/claude-review.md:73`. OTLP arrives after span completion and may lose the race to lazy evaluation; the design provides no measured incremental win over bounded pre-merge. Prove pre-merge benefit, measure offer acceptance/install outcomes, and add live offers only if their value is material. Do not add cross-engine acceptance mutexes as if the push protocol could deduplicate execution globally.
3. **Defer the second metadata replica / HA orchestration in the homelab.** References: `architecture.md:26,100,270,341-342`; `metrics-and-eviction.md:9`. One bounded leader, managed/existing Postgres, infrequent checkpoints and an accepted cold restore can prove the workload case. Full replica failover and object-delete recovery add distributed correctness obligations before a demonstrated hit-rate benefit. If production HA is retained, its epoch/delete-generation correctness remains mandatory, not optional. Warm checkpoint tailing was already cut in R1; do not claim cutting it again as new work.

## 7. Exit criteria

Tier 1 should proceed only after: (1) the discovery capture and metric-population/cost-join spec produce a reproducible Phase-0 result; (2) the ledger specification handles reused/mixed-owner chains, NULL timestamps, graph-publication pins, failed/deleting recovery and delayed external delete effects; (3) production topology, verification policy and measurement points are made consistent; and (4) the already-planned publication and fresh-engine memory spikes establish deployment budgets. These are design/evidence gates, not a request to build the entire service to earn permission to review it.

## Primary external references

The review relies primarily on the local, line-cited design set and pinned engine source. These three additional primary documentation sources were checked through retrieved search results; the direct web extractor was unavailable. No current MinIO supply-chain assertion or Azure product recommendation was treated as verified by this review.

- **[S1] PostgreSQL manual, §13.2.1, Read Committed Isolation Level.** Concurrent UPDATE/DELETE search-condition rechecking and INSERT ON CONFLICT behavior. `https://www.postgresql.org/docs/current/transaction-iso.html`
- **[S2] PostgreSQL manual, §9.9.5, Current Date/Time.** `now()` is equivalent to transaction-start timestamp, unlike clock time. `https://www.postgresql.org/docs/current/functions-datetime.html`
- **[S3] Amazon S3 User Guide, Download and upload objects with presigned URLs, Expiration time for presigned URLs.** Request-time expiry checks and an already-started download continuing after expiry. `https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html`
