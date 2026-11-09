> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

STOP

# Round-3 adversarial review of Revision R2 (Claude Opus)

**Totals: 26 findings — 1 P0, 13 P1, 12 P2.**

**Scope.** I read `revision-r2.md`, `ledger.md`, `architecture.md`, `metrics-and-eviction.md`, `latency-and-azure.md`, `decision.md`, `s3_latency_probe.py` (byte-identical to `s3_latency_probe_v3.py`), `s3_latency_probe_v2.py`, the round-2 review and the baseline-r2 latency doc. I accepted the brief's measured facts. I ran no PostgreSQL or S3 experiment. Interleavings below are argued from the SQL text and PostgreSQL READ COMMITTED semantics. I modified only this file.

**Verdict rationale.** R2 does fix what round 2 mainly asked for: the publish predicate, NULL mint stamps, generation keys and a single verification policy. But the new normative ledger cannot be executed as written. `blob_head.state` is never maintained, and reserve has no guard. Five round-2 clauses that R2 reports as fixed survive word for word in `architecture.md`. Most fixes are mechanical. A consistency pass plus small ledger edits should be enough for R3.

---

## Q1. Closure audit (all 19 round-2 findings)

| # | Status | Closing clause / what remains |
|---|---|---|
| 1 P0 discovery | **OPEN (acknowledged)** | `decision.md:61`, `revision-r2.md:39`. The lifecycle fallback that round 2 said cannot seed fresh work is still offered as mitigation at `architecture.md:336`, and as a completion source at `architecture.md:224` ("or `engine.share`"). See F25. |
| 2 P0 publish SQL | **CLOSED (predicate)** | `ledger.md:94-126`: owner-fenced verify plus an all-distinct-verified publish. The mixed-owner and reused-blob cases now pass. The waiter path built on top of it is wrong (F2). |
| 3 P0 metric model | **PARTIAL** | `metrics-and-eviction.md:27-87`. The R1 formula survives at `architecture.md:260` (F9). Classification and the install join are not computable as stated (F6). The cost estimate starves when the cache works well (F5). |
| 4 P0 gap + ABA | **CLOSED for ABA, conditionally** | `ledger.md:9-15,173`. This holds only if the generation is resolved from `blob_head` at mint time (F3) and the head row is maintained (F1). The gap is actually protected by `last_ref_at` and the mint recheck, not by the 2-minute pin (F17). Deposed-leader GC is unfenced (F4). |
| 5 P1 NULL mint | **CLOSED** | `ledger.md:31,52,78`. |
| 6 P1 reserve/failed/delete | **PARTIAL** | The transition table exists (`ledger.md:56-64`). There is no SQL for `pending→failed`. The head-state mirror is never written, and the reserve guard is absent (F1). The failed purge leaves the object behind (F18). `architecture.md:171-172` still says "then the row is deleted" (F21). |
| 7 P1 renew round trip | **PARTIAL** | `ledger.md:152` closes it, but the exact cited clause survives at `architecture.md:99` ("must not need … a Postgres round trip … treat a cache miss there as `bad_key`") (F9). |
| 8 P1 sampling | **PARTIAL** | `architecture.md:49,150,227,258` and `decision.md:33` are aligned. I3 (`architecture.md:51`) still claims a key "only ever holds bytes that hash to its name". `architecture.md:35-36` still shows "sha256 stream" on every verify. The cost table still assumes a full reread (F14, F13). |
| 9 P1 30 s claim | **MOSTLY CLOSED** | `latency-and-azure.md:27-44`. The categorical claim is gone. The arithmetic is slightly off (F12), the tail uses an aggregate rate as if it were a per-stream rate (F11), and the "donor may be dead" path is not specified (F10). |
| 10 P1 admission | **CLOSED as a labelled scaffold** | `metrics-and-eviction.md:170-177`, `latency-and-azure.md:86-97`. There is no default while `read_throughput` is unmeasured, and the decision point is late (F24). |
| 11 P1 shared-group scoring | **PARTIAL / regressed** | `metrics-and-eviction.md:157-168`. Literally, the greedy rule evicts the **most** valuable group (F7). Unknown-cost groups score zero (F5). |
| 12 P1 merge-refreshed retention | **PARTIAL** | `metrics-and-eviction.md:151-155` closes it. `architecture.md:170` still says "refreshed on install and merge" and "merged but never installed" (F9). |
| 13 P1 ceiling > 100 % | **PARTIAL** | The bound now holds (`metrics-and-eviction.md:86`). The Phase-0 query is still duration-weighted call spans (`architecture.md:197`), and the gate is still "≥ 30 % of job wall time" (`architecture.md:264`), against the wall-time gate at `metrics-and-eviction.md:131-133` (F9). `would_hit` lacks the reachability test that `lost_keying` has (F22). |
| 14 P1 metadata-only suppression | **PARTIAL** | Fixed at `architecture.md:97`. The contradicting rule survives at `architecture.md:160` ("skips exporting a root when `EquivalentEntries` shows a stored value") (F9). |
| 15 P1 continuation behind commit | **CLOSED** | `architecture.md:225-226`. The waiter design reintroduces a budget conflict (F2). |
| 16 P1 topology | **PARTIAL** | Many MinIO-as-production clauses survive (F8). |
| 17 P1 breaker visibility | **PARTIAL** | `metrics-and-eviction.md:119-129` and `latency-and-azure.md:66-84` close the measurement-point question. But the breaker input `dcache_install_failure_ratio` has no grounded source, and `architecture.md:251` and `decision.md:49,85` still trip on p99 (F7b → F15). |
| 18 P2 probe | **PARTIAL / regressed** | The assertions were added. v3 broke TTFB timing, and §0 credits v2's numbers to v3 (F12b → F16). |
| 19 P2 checkpoints | **CLOSED** | `architecture.md:100,142,208`; diagram line 34; `metrics-and-eviction.md:110,143`. The request total was not recomputed (F13). |

---

## Q2. Attacks on `ledger.md`

| Attack | Result |
|---|---|
| (a) two concurrent uploads of one digest | **Broken as specified.** See F1. The head-row `FOR UPDATE` lock does serialize the two transactions. But the loser test ("no row ⇒ another export owns it", `ledger.md:80`) is wrong: the loser computes `gen+1` and therefore does not conflict on the PK arbiter. It either hits `blob_one_pending` (an error, not `DO NOTHING`) or, if the head gen is `verified`, it **succeeds**. |
| (b) publish vs GC, including a pause > 2 min | **I cannot break safety.** 2b sets `last_ref_at=now()` (`ledger.md:115`), so GC cannot mark for G = 2 h whatever the pin says. With a pause longer than G, GC deletes, the late `SetStoredPart` publishes a dead part, and the next mint (`ledger.md:145-152`) omits the digest and calls `DropStoredParts`. That is fail-open. The pin itself adds nothing and is cleared non-reentrantly (F17). |
| (c) delayed DELETE after a new generation | **Holds**, provided the URL key is built from `blob_head.gen` at mint (F3) and re-reserve after `deleted` is reachable (F1). A different deposed-leader effect does break availability: GC driven by a stale `LiveBlobs()` snapshot (F4). |
| (d) crash mark→delete, delete→stamp | **Holds.** The recovery scanner (`ledger.md:180`) re-deletes the same gen and stamps it. Its `DropStoredParts` condition is vacuous (F19). |
| (e) reserve while `deleting` | **Depends on the F1 fix.** Under the SQL as written there is no guard: the reserve inserts `gen+1` over a `deleting` row and breaks "never re-reserved while deleting" (`ledger.md:66`). If the guard reads `blob_head.state`, it sees the stale `'pending'` and refuses. That is correct here, but only by accident. |
| (f) waiter whose partner owner dies | **Fails open, but the mechanism is incoherent.** See F2. |

---

## Findings

### F1 [P0] Reserve is not executable: `blob_head.state` is never maintained and the reserve SQL has no state guard

**Refs:** `ledger.md:39-45,58-59,73-84,97-101,161-177`; no SQL for `pending→failed` exists at `ledger.md:61`.

`blob_head.state` "mirrors blob.state" (`ledger.md:43`). The only statement that writes it is reserve (`:82`). Verify, mark, the delete stamp and fail all update `blob` only. The reserve SQL then inserts `gen+1` without checking the head state it just locked. Its only loser test is `ON CONFLICT (pool,digest,gen) DO NOTHING RETURNING gen`. That test cannot fire, because the loser computes a fresh `gen+1`.

Either reading breaks the state machine.

- **Guard on `head.state`, as the transition table implies (`:59`).** E1 reserves g1 and head becomes `pending`. E1's spot node dies, and the sweeper marks g1 `failed`, but the head stays `pending`. From then on, every reserve sees `pending` and treats it as "another export owns it". The digest is **permanently** unexportable, and every waiter on it times out (F2). The same happens after any GC delete. On spot capacity, failed uploads are routine, so the cache decays monotonically.
- **SQL as written, no guard.**
  - Interleaving 1: E1 reserves g1 and commits. E2 takes the lock, sees `(1,pending)`, and inserts g2 `pending`. That violates the partial unique index `blob_one_pending`, which is not the `ON CONFLICT` arbiter. The result is a unique-violation error, not "no row".
  - Interleaving 2: E1 verifies g1 and publishes. E2's pre-check (`architecture.md:226`) ran earlier, outside the lock, and saw `absent`. E2 locks, sees `(1, …)`, inserts g2 `pending` successfully, and moves the head to g2. This issues a PUT URL for a verified digest, which violates I4 (`architecture.md:52`). Every mint for the published g1 parts now fails the `blob.gen=h.gen` join, so the parts are dropped. g1 can never be collected, because mark only considers the head gen. That is a permanent leak.

**Minimal fix.** After `SELECT … FOR UPDATE`, branch explicitly on `blob.state` at the head gen: proceed only if it is `absent`, `failed` or `deleted`; otherwise ROLLBACK and return no URL. Either drop `blob_head.state`, or update it in the same statement as every transition (verify, fail, mark, stamp). Add the `pending→failed` SQL, guarded by `lease_until < now()`, and an early-fail when `EndRemoteProcess` reports the owner's session dead.

### F2 [P1] The waiter path is tied to a watchdog it does not belong to, and names a message that does not exist

**Refs:** `ledger.md:130-140`; `architecture.md:226-227`.

The 4 s retry is said to sit "inside the engine's 5 s upload-continuation watchdog", and on timeout E2 "sends `upload{done:[]}`" (`ledger.md:136-138`). But the publish fence (2a) can only succeed **after** E2's own blob B is verified, which happens after `uploaded{done}`. That is far past the 5 s window for sending `upload{exportID,urls}` (`architecture.md:226`).

`upload` carries `urls`; `done` belongs to the engine's `uploaded` reply. So one of two things is true:

- the wait happens before the continuation, in which case B cannot be verified yet, and the wait eats 4 of the 5 s budget that F15 just protected; or
- the wait happens after `uploaded`, in which case no watchdog applies and there is nothing to send.

Because of this tie-in, a slow but live E1 (a 2 GB A taking about 20 s) makes E2 abandon a publishable part after 4 s. Re-publishing then needs a live donor to re-export, which spot capacity makes unlikely.

`ledger.md:140` ("No export ever waits on an object it does not own") contradicts the section it ends.

**Fix.** Publication needs no engine participation: the bundle is already held service-side. Persist a durable *publish intent* `(part, chain)` when the commit lands. Re-evaluate it whenever any digest in the chain is verified. Expire it with the owners' leases rather than with a 4 s timer.

### F3 [P1] Generations cannot be stored in dagql parts; mint and publish are digest-keyed, so "a part references (digest, gen)" is undefined

**Refs:** `ledger.md:15,105-126,145-149,180,186`; `architecture.md:171` (`DropStoredParts(digest)`).

`SetStoredPart` stores OCI descriptor chains, and the protocol identifies layers by digest. There is nowhere in the graph to put `gen`. Mint (`:145-149`) and publish (`:109-118`) take `$chain` as **digests** and join the head generation. They never compare it against a part-held generation. The recovery scanner calls `DropStoredParts(digest,gen)` and restore checks "its (digest,gen)". Neither has a gen to check.

The design is safe only under one rule: the GET/PUT key is computed at mint time from `blob_head.gen`, and the graph never holds `gen`. If an implementer instead caches `gen` with the part, as §0 says, then a part published against g1 after g2 exists mints a g1 URL. That URL returns 404 forever, while the ledger says "verified", so the part is never dropped.

**Fix.** State the rule as normative: generation is ledger-only and resolved at each mint. Rewrite `ledger.md:15,180,186` accordingly.

### F4 [P1] A deposed leader's GC is not epoch-fenced, so a stale `LiveBlobs()` snapshot can delete live published blobs

**Refs:** `ledger.md:158-180`; `architecture.md:54,171,342`.

1. Leader A takes a `LiveBlobs()` snapshot at time S, then stalls (GC pause or VM freeze).
2. B takes over and publishes part P referencing blob X at time T. That stamps `last_ref_at=T`.
3. P is not merged or renewed for 2 h.
4. A resumes at T+2h+ε with its snapshot from S. X is not in that snapshot. The SQL age, mint and pin checks all pass, so A marks X `deleting` and deletes it.
5. B's live part P is now dead at its next mint.

None of the ledger statements checks the epoch. R9's "self-terminates within half the TTL" (`architecture.md:342`) cannot help: a frozen process checks nothing before its first post-resume write.

The outcome is fail-open, but it destroys exactly the idle-but-valuable entries that retention is supposed to keep.

**Fix.** Add `AND EXISTS (SELECT 1 FROM pool_leader WHERE pool=$1 AND epoch=$my_epoch AND lease_until>now())` to mark, to the delete stamp and to reserve. Also refuse to mark when the `LiveBlobs()` snapshot is older than one sweep interval.

### F5 [P1] `cost_estimate` starves when the cache works, which zeroes "saved" and makes hot entries evict first

**Refs:** `metrics-and-eviction.md:65-66,75-78,162,166`.

The estimate draws only from **executed/deferred** decisions in a 30-day window. An entry that the cache serves reliably is installed, not executed. After 30 days of hits it has n = 0, so its cost is unknown. It then drops out of `saved` and the value ratio, and its reclaim-group benefit becomes 0.

§3.2 evicts zero-benefit groups "oldest-`last_install` first". The policy therefore evicts the entries the cache is best at serving. The measured savings fall to zero exactly when the cache is working.

**Fix.** Record the donor's measured `recompute_secs` in service metadata at export or publish time; the service has the executed span then. Keep it as `basis="donor"` for the life of the part. Never let an installed entry's benefit go to 0 just because its estimate is unknown.

### F6 [P1] Decision classes are not derivable as specified: `hit_local` is empty and the install→entry join is ungrounded

**Refs:** `metrics-and-eviction.md:19-21,40-48,52-55`.

1. `recompute_secs` includes **call-span self time** (`:53`). Every `outcome=hit` call span has a positive self time (lookup). So class 4 (`deferred_recompute`, `recompute_secs > 0`) absorbs every local hit, and class 6 `hit_local` is empty by construction. Spent time and the hit-ratio denominator are inflated by every local hit. **Fix:** test only lazy-span time for class 4.
2. The decision row is keyed `(…, result_id)`, but the install span's grounded attributes (`:21`) are part, bytes and duration only. No result.id is listed and no parent-span rule is given. Attributing installs to an entry is assumed. `revision-r2.md:40` admits this as open item F21, yet the class table depends on it without saying so.
3. An entry that installs one part and recomputes another is classed `installed`, and its recompute time vanishes from `spent` (`:64` sums executed and deferred only). The value and hit ratios are biased upward.

On the ratio checks: all three ratios are ≤ 100 % (`:84-86`), but each divides by zero on an idle pool. The document should state NaN/absent semantics.

### F7 [P1] Reclaim-group greedy is inverted: "pick the highest score" evicts the most valuable group

**Refs:** `metrics-and-eviction.md:164-166`.

`score = benefit / bytes` is a value density. Eviction must remove the **lowest** score first. Read literally, the worked counterexample evicts R (6000 s per GiB) before {P, Q} (120 s per GiB), which is the pathology round 2 described, in a new form.

**New pathologies, assuming the direction is fixed:**

- (i) Groups are never enumerated. Minimal reclaim groups over a shared-blob graph are combinatorial, and "recompute groups after each removal" has no stated algorithm or cost.
- (ii) Whole-group removal with no size consideration can overshoot the low watermark. For example, one cold 1 TB shared toolchain group might be evicted to free 50 GB.
- (iii) It is unspecified whether a group with one member under `min_retention` is skipped as a whole.
- (iv) Benefit sums per-part installs × per-entry `cost_estimate`, so multi-part entries are double-counted (round-2 F3's granularity point, re-introduced).

### F8 [P1] Topology: MinIO-as-production clauses survive in normative text

**Refs (surviving clauses):**

- `architecture.md:12`: blobs and checkpoints "in MinIO"; "external S3 mounted outside the cluster".
- `architecture.md:38-43`: diagram shows MinIO as the only store.
- `architecture.md:110`: checkpoints in MinIO.
- `architecture.md:164`: "200 GB of burst reads from MinIO … more MinIO bandwidth".
- `architecture.md:203-211`: cost column "MinIO in-cluster", egress `$0`.
- `architecture.md:216`: "MinIO bandwidth is the real budget".
- `architecture.md:250-251`: failure rows for "MinIO refuses" and "MinIO hangs".
- `architecture.md:270`: Helm Secret "MinIO access key".
- `architecture.md:292`: open question about MinIO placement.
- `architecture.md:329-330`: R5 "pin MinIO to non-spot nodes", plus the Azure Premium SSD v2 / uncached-disk sizing that round 2 F16 named explicitly.
- `decision.md:85`: "pin MinIO off spot".
- Tier 2 "on MinIO" at `architecture.md:13`, `decision.md:27,65`, with no production store named.

Postgres placement in production is stated only in `decision.md:20` and `metrics-and-eviction.md:9`. `architecture.md:270` pins only `dcache`.

Cold restore is consistent (`architecture.md:34,248`; `decision.md:20`).

### F9 [P1] `revision-r2.md` overclaims: five round-2 clauses survive in `architecture.md`

| Round-2 finding | Surviving clause |
|---|---|
| F3 | `architecture.md:260` restates R1's SLIs verbatim: "(lazy-inclusive recompute − install seconds)" and "distinct installs ÷ (installs + executed)". These are the formulas `metrics-and-eviction.md:13` says were wrong. |
| F7 | `architecture.md:99`: renew "must **not** need … a Postgres round trip" and "treat a cache miss there as `bad_key`". This is the exact clause round 2 cited. |
| F12 | `architecture.md:170`: "refreshed on **install and merge**"; idle prune is still "merged but never installed". |
| F13 | `architecture.md:197`: "Weight both by span duration". `architecture.md:264`: "≥ 30 % of job wall time" as the gate. Both conflict with `metrics-and-eviction.md:131-133`. |
| F14 | `architecture.md:160`: the recipe-level rule still skips on "a stored value". |

`revision-r2.md:34` says `architecture.md` was "patched in place at 14 sites". These sites were missed.

### F10 [P1] "The donor may already be dead" has no specified path; the cold and killed-node flows fail those rows instead

**Refs:** `latency-and-azure.md:41`; `architecture.md:227,240`; `ledger.md:61`.

Verification is triggered by `uploaded{done}` (`architecture.md:227`). A donor killed after its PUT returns 2xx but before it sends `uploaded` never triggers verification. Per `architecture.md:240`, its `pending` row expires after 10 minutes and becomes `failed`. The latency doc's headline distinction, that a dead donor's completed PUT can still publish, is therefore not executable.

**Fix.** On WS disconnect, or on `uploaded` timeout, HEAD every `pending` key owned by that export and verify any that exist. This is legal because I2 already treats `uploaded` as a hint.

### F11 [P1] The publication tail is understated: an 8-writer aggregate is used as if it were a single-stream worst case

**Refs:** `latency-and-azure.md:17,34-35,44`; engine "8 concurrent per session" at `architecture.md:146`.

73.4 MB/s is the **aggregate** of 8 writers. A 2 GB layer sharing the session's 8 PUT slots gets about 9.2 MB/s, which is about 218 s, not 27.3 s. The "≈13–28 s" range therefore presents the contended case as the pessimistic bound when it is roughly 8× too optimistic.

Every rate in §1 is also a design-cluster MinIO observation (PUT n = 1). §1 never says that none of it describes the production external endpoint, where the 30 s notice actually applies.

"Not impossible, not guaranteed" is now correct. The residual-risk sentence (`:44`) is honest about compression and telemetry, but not about contention or production.

### F12 [P2] The full-path range does not follow from its own inputs

**Refs:** `latency-and-azure.md:33-37`.

- 2000 / 155.6 = 12.85 ✓; 2000 / 73.4 = 27.25 ✓.
- Fast path: 12.85 + 0.0207 = 12.87 ✓; 27.27 ✓ (≈ 13 / 28).
- Reread: 2000 / 97.3 = 20.6 ✓ (97.3 MB/s = 4.194 MB / 43.1 ms).
- Full path: 12.85 + 20.62 + 0.02 = **33.5 s**, and 27.25 + 20.62 + 0.02 = **47.9 s**. The table says ≈ 34–50 s.

The 34–50 figure is round 2's result for 93 MB/s and 70.1 MB/s, carried over unchanged. Separately, the 97 MB/s reread rate is a hot, same-key 4 MiB GET. Using it contradicts `latency-and-azure.md:25,94` ("read throughput … not measured").

### F13 [P2] The cost table was not recomputed after R2 changes

**Refs:** `architecture.md:206-209`.

- With 144 checkpoints/day the request total is 0.10 + 0.016 + 0.12 + 0.004 = **$0.24/day** (≈ $7.2/month), not $0.27. The old figure is the 1440/day total.
- "HEAD + verify GET 40k" and "600 GB/day re-read" assume a full reread for every blob. Under §5.1 it is about 20.2k requests and about 6 GB/day.

Round 2 F8 asked for the sampling policy to be carried into the egress budgets.

### F14 [P1] Wrong-bytes handling: I3 overclaims, the failure table contradicts itself, and there is no detection path for the unsampled 99 %

**Refs:** `architecture.md:51,150,258`.

1. I3 still asserts that a key "only ever holds bytes that hash to its name". Under 1 % sampling this is false: the key can hold wrong bytes, and readers detect them.
2. §5.1 and the dcache column at `:258` say a wrong object "fails the receiving build". The engine column of the same row (RP §7.2) says "source exhausted → compute", which is fail-open. These cannot both be true, and "fails the build" would violate I7.
3. No grounded telemetry carries an engine import digest mismatch back to the service. A corrupt unsampled blob therefore stays published. Every receiver downloads it in full, rejects it and recomputes, which is worse than no cache, until retention expires. Live offers keep advertising it.

**Is the tradeoff defensible?** For a mutually trusted pool (I8) where the threat is bugs rather than malice, yes — but only with a feedback path. Treat "offered/merged URL served, then class `deferred_recompute` with bytes transferred" as a suspect signal, and force a hash-check of that blob.

"Authorization" adds no information about content. The service issued the capability itself, and "size" compares the engine's own descriptor to the engine's own PUT. The honest label is **existence + size-consistency + 1 % hash**.

The diagram (`architecture.md:35-36`) still shows "sha256 stream" on every verify.

### F15 [P1] The breaker input is ungrounded, and the p99 breaker survives in three places

**Refs:** `metrics-and-eviction.md:126,146`; `latency-and-azure.md:73,78-82`; `architecture.md:251`; `decision.md:49,85`.

`dcache_install_failure_ratio` comes "from offer/install telemetry". But the grounded install attributes (`metrics-and-eviction.md:21`) describe **successful** installs only. No failure event or attribute is cited, so the numerator does not exist.

Against R2's install-failure breaker, `architecture.md:251` still says "trip the breaker on p99 > 2 s", and `decision.md:49` says "every S3 op gets a p99 SLO and a breaker". The WAN row's "2 s per-request hang detector" (`latency-and-azure.md:82`) has no measurement point on the engine path. `minio_s3_requests_ttfb_seconds` exists only on the design cluster, and the production "provider access-log exporter" is unnamed.

**Fix.** Define the failure numerator from grounded events, for example renew results plus `deferred_recompute` after a URL was minted. Delete the p99 breaker text.

### F16 [P1] Probe v3 broke TTFB timing, and §0 credits v2's measurements to v3

**Refs:** `s3_latency_probe.py:62-70`; `s3_latency_probe_v2.py:66-69`; `latency-and-azure.md:7,18`.

v3's `do()` stops the TTFB clock right after `c.request()` returns, **before** `getresponse()`. For a GET, `request()` only writes the request, so v3's "TTFB" is socket-write time (microseconds). v2 correctly timed through `getresponse()`.

The 3.96 ms, 4.10 ms and 1.92 ms figures therefore could not have come from v3. Yet §0 says "Probe v3" and marks the 403 as "asserted", which v2 only printed.

Run against production (§7), v3 would report near-zero TTFB for a hanging store's GETs. In fact it would block in `getresponse()`, but the number it records stays tiny.

**What is correct:**

- p99 is the third-largest of 200 (`round(.99·199) = 197`) ✓.
- The MiB/s → MB/s factor 1.048576 ✓.
- 32 MiB / 0.457 s = 70.0 MiB/s = 73.4 MB/s ✓.

The docstring's "every … PUT … size is asserted" overstates: the concurrent PUTs are never HEAD-checked. `s3_latency_probe_v3.py` duplicates `s3_latency_probe.py` byte for byte.

### F17 [P2] The 2-minute publish pin is redundant and its clear is not re-entrant

**Refs:** `ledger.md:115,124-128`.

2b already sets `last_ref_at=now()`, which blocks marking for G = 2 h. A 120 s pin therefore protects nothing that `last_ref_at` does not already protect for 60× longer. The sentence "this is what makes the graph-publication gap safe" credits the wrong mechanism: the real backstop beyond G is the mint recheck.

Two publishers sharing digest A will see the first one's `SET publish_pin_until=NULL` clear the second one's pin. Either drop the pin, or use `GREATEST` and never clear.

### F18 [P2] Failed generations and late PUTs leak objects; `deleted` rows grow without bound

**Refs:** `ledger.md:88,182`; `architecture.md:173`.

"Purge after `lease_until` + PUT TTL" deletes the row but never the g<n> object. A PUT that starts just before URL expiry can complete after the purge, because S3 checks expiry at request start. That object is never referenced again (which is safe) and never deleted (a leak). The weekly reconcile only looks for objects "missing from the ledger" and does not say whether it deletes them.

`deleted` rows are kept forever, one per lifetime. Specify that the reconcile deletes any key whose (digest, gen) is not `pending` or `verified`, and that `deleted` and `failed` rows older than the reconcile horizon are purged.

### F19 [P2] The recovery scanner's `DropStoredParts` condition is vacuous

**Refs:** `ledger.md:180`.

While a row is `deleting`, re-reserve is forbidden, so the head always points at it. "If the head no longer points at them" therefore never fires. Make the drop unconditional for the digest. It is cheap, and it covers parts published after the sweep's snapshot.

### F20 [P2] `decision.md` invariant 1 contradicts the ledger

**Refs:** `decision.md:33`; `ledger.md:105-122`.

`decision.md` says "`SetStoredPart` only in a transaction whose rowcount equals the chain length". The ledger puts `SetStoredPart` **outside** the transaction, under a pin, and counts **distinct** digests.

### F21 [P2] `architecture.md` describes pre-generation storage semantics

**Refs:** `architecture.md:137,40` (keys without `/g<gen>`); `:161-162` (insert-on-conflict, and two exports writing "identical bytes" to one key that "S3 keeps … atomically" — with generation keys they write different keys); `:171-172` ("then the row is deleted", "next sweep deletes it"); `:154` ("same key is idempotent").

### F22 [P2] Metric definition gaps

**Refs:** `metrics-and-eviction.md:65-68,84-87,144-146`.

- `saved` is gross (Σ `cost_estimate`) rather than net of `install_seconds`, so a slow install counts both as full savings and in `lost`.
- `would_hit` lacks the "earlier entry had a verified part" eligibility that `lost_keying` requires, so unreachable work inflates the ceiling.
- The alerts reference metrics that are never defined: `dcache_storage_quota_bytes`, `dcache_sessions{state}`, and the ratio gauge from F15.
- The regret denominator counts hourly re-candidacy, not distinct evictions.

### F23 [P2] Exclusive-time rule is under-specified

**Refs:** `metrics-and-eviction.md:50-57`.

"Self time" is never defined for overlapping parallel children (interval union vs. sum). It is also not said whether other entries' lazy spans nested inside are subtracted. Dropped child spans silently inflate a parent's self time. Round 2 asked for an explicit interval rule.

### F24 [P2] Admission is decided too late, and the cost gate has no default

**Refs:** `metrics-and-eviction.md:170-177`; `architecture.md:224-226`.

Frequency and cost are known when the scheduler sees the span, at step 3. Deciding at the upload continuation, step 5, wastes an export slot, a bundle transfer and a `CommitValueMerge`, and it grows the pool graph with metadata-only roots.

`serve_cost` needs a production `read_throughput` that is declared unmeasured, and no default is given, so the normative cost gate cannot be evaluated today. Move admission to export scheduling, and state the interim rule (for example, frequency only).

### F25 [P2] Residual R7 text contradicts F59 and round-2 F1

**Refs:** `architecture.md:336`.

R7 still says "a NetworkPolicy denying engine egress to `api.dagger.cloud`". That contradicts `architecture.md:86` and `decision.md:87`. R7 also still offers the `engine.share`/`engine.stop` fallback discovery that round 2 F1 showed cannot seed fresh work.

### F26 [P2] Dead owners hold reservations for the full lease

**Refs:** `ledger.md:61`; `architecture.md:240`.

The service learns that a session died (`EndRemoteProcess`), but its `pending` rows still block the digest for 10 minutes. Every export that needs that digest in the meantime waits and fails. Fail those owners' `pending` rows when the session ends; generation keys make that safe even if a late PUT lands.

---

## Q3–Q9 summary answers

- **Q3 Metric computability.**
  - The seven classes are not derivable as written (F6: `hit_local` is empty, and the install→entry join is assumed).
  - `cost_estimate` is derivable but self-starving (F5).
  - The exclusive-time rule is under-specified (F23).
  - The opportunity sets are disjoint and bounded at 100 %. Every ratio divides by zero on an idle pool, and no NaN/absent handling is stated.
  - Ungrounded: install-span → result.id attribution; the install-failure numerator; `self_digest` equality between receiver and donor under equivalence-class hits (this affects whether `cost_estimate` ever finds the donor's samples).
- **Q4 Topology.** Not consistent (F8, F25).
- **Q5 Publication math.** 12.85 ✓, 27.3 ✓, 13–28 ✓ arithmetically, 34–50 ✗ (should be 33.5–47.9) (F12). The replacement claim is correct. The tail is understated (F11), and the dead-donor path is unspecified (F10).
- **Q6 Verification policy.** Consistent across I1, the overview, the cold flow and `decision.md`. I3, the failure-table engine/dcache columns, the diagram and the cost table disagree (F14, F13). The tradeoff is defensible for a trusted, fail-open pool only with a mismatch feedback path and an honest label (F14).
- **Q7 Eviction.** Group scoring would fix the cold-sharer case if the direction were correct (F7). The new pathologies are unspecified group enumeration, overshoot, and zero-benefit eviction of hot unknown-cost entries (F5). Admission is coherent and does precede the continuation, but it belongs at export scheduling (F24).
- **Q8 Probe.** The assertions match §7, except the concurrent PUT sizes. The TTFB timing regressed, and the §0 attribution is wrong (F16). The order statistic and units are correct.
- **Q9 New errors.** F1 (head mirror), F2 (waiter/protocol), F3 (gen not representable in graph), F7 (inverted greedy), F12 (34–50), F13 (stale total), F16 (TTFB regression), F17 (pin), F19.

## Minimum for R3 to reach no-P0/P1

1. Ledger: reserve guard plus head maintenance and the `pending→failed` SQL (F1); durable publish intent (F2); generation resolved at mint (F3); epoch predicate on GC, stamp and reserve (F4); a disconnect-triggered HEAD of pending keys (F10).
2. Metrics: donor cost recorded at publish (F5); fix class 4's test and specify the install join, or mark it blocked on F21 (F6); flip the greedy direction (F7); a grounded failure numerator (F15).
3. One mechanical pass over `architecture.md` and `decision.md` for F8, F9, F14 and F21.
4. Revert the probe's TTFB timing to v2's, and label the §0 table as v2 data (F16).
5. Restate the tail using per-stream rates and production caveats (F11).
