> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

STOP

# Round-4 review of Revision R3 (Claude Opus)

**Totals: 31 findings: 1 P0, 16 P1, 14 P2.**

**Scope.** I read `revision-r3.md`, `ledger.md`, `architecture.md`, `metrics-and-eviction.md`, `latency-and-azure.md`, `decision.md`, `s3_latency_probe.py` (byte-identical to `_v3`), both round-3 reviews and the relevant `recon-protocol.md` sections (telemetry contract at :40-75, upload at :150-160, stored parts at :378-395, attrs at :1533-1592). I ran no PostgreSQL, S3 or engine experiment. I argue interleavings from the SQL text under PostgreSQL READ COMMITTED semantics. I modified only this file.

**Verdict rationale.** R3 fixes most of what round 3 asked for: state-branched reserve, correlated mint, a single continuation, lowest-score eviction, AND admission, net/gross accounting, two gates, and the probe TTFB. The R3 publish design ("hold Postgres head row locks across an in-process `SetStoredPart`") does close the paused-publisher hole. But it closes it by creating a two-store commit problem. That problem has four symptoms: `graph_refs` has no decrement path, rollback-after-insert leaves orphan parts, DB locks are held while waiting on the dagql write lock, and restore reconciliation turns into mass quarantine. These are one structural defect, not four patchable ones (see Q8). Another patch round will produce another set of these. I recommend the structural change in Q8 before R4.

---

## Q1. Closure audit

### Claude round-3 findings (F1–F26)

| # | Status | Closing clause / what remains |
|---|---|---|
| F1 P0 reserve/head mirror | **PARTIAL** | Branching `ledger.md:93-105` and the mirror rule `:75-80` close the original defect. New defects: the mirror update is not conditioned on the primary's rowcount (#5), the `failed` source is unguarded (#6), and there is no `quarantined` branch (#19). |
| F2 waiter/watchdog | **PARTIAL** | Wire error closed at `ledger.md:166`. The 4 s budget still abandons parts behind slow-but-live owners (#30). |
| F3 gen not in graph | **PARTIAL** | Mint resolves gen from head (`ledger.md:180-188`), which closes the main defect. Restore still tests "its `(digest,gen)`" (`ledger.md:230`), and the graph does not have that field (#7). |
| F4 deposed-leader GC | **MOSTLY CLOSED** | `ledger.md:196` adds a leader check, and `graph_refs` replaces the stale `LiveBlobs()` snapshot. Residue: the check compares holder, not epoch, and the stamp is unfenced (#20). |
| F5 cost starvation | **PARTIAL** | Donor cost is now recorded (`metrics:67`), but at the wrong granularity and too early (#10). |
| F6 classes | **CLOSED** | `metrics:28-39` uses lazy-span evidence, and the install→entry join is grounded by `result.id` on the install span (RP attrs :772-777). Zero denominators are handled at `metrics:75`. |
| F7 inverted greedy | **PARTIAL** | Direction fixed at `metrics:164`. Group construction is now wrong (#9), and per-part×per-entry double counting persists (#10). |
| F8 MinIO-as-prod | **PARTIAL** | Fixed at :12, :330 and `decision.md:85`. Survives at `architecture.md:164,216,250,270` (#23). |
| F9 surviving R2 clauses | **PARTIAL** | Fixed at :99, :160, :170 and :264. **`architecture.md:260` still has R1's SLIs verbatim** (#17), and :197 survives (#22). |
| F10 dead donor | **OPEN** | `latency:45` repeats the claim, and nothing triggers verification without `uploaded` (#14). |
| F11 aggregate as per-stream | **OPEN** | `latency:34` (#13). |
| F12 full-path arithmetic | **CLOSED** | `latency:33-37` re-derives correctly from its inputs. One input is itself mismeasured (#12). |
| F13 cost table | **PARTIAL** | HEAD row fixed (`architecture.md:206`). The total is still $0.27/day ≈ $8/month (:209, :214; `decision.md:48`) (#26). |
| F14 wrong bytes | **PARTIAL** | I3 fixed (:51), diagram fixed (:35-36). ":150/:258 fails the receiving build" still contradicts the engine column and I7. The "engine mismatch report" (`ledger.md:69,225`) is not a protocol message (#25). |
| F15 breaker | **PARTIAL** | The p99 breaker is gone (:251, `decision.md:49`). The production signal source and hang detection are ungrounded (#15). |
| F16 probe TTFB | **CLOSED** for GET/HEAD (`s3_latency_probe.py:68-70`). The same class of bug survives in the PUT timer (#12). |
| F17 pin | **CLOSED** (removed). |
| F18 failed objects | **PARTIAL** | `ledger.md:224` deletes the object. A non-head failed row cannot complete its transition (#6). The tombstone purge horizon is unspecified. |
| F19 scanner drop | **REGRESSED** | `ledger.md:223` now stamps `deleted` while leaving parts. That breaks the one-generation gate (#8). |
| F20 decision inv. 1 | **MOSTLY CLOSED** | `decision.md:33` now describes the head-lock transaction, but says `|chain|` where the ledger says distinct (#31). |
| F21 pre-gen storage text | **OPEN** | `architecture.md:40,137,154,161-162,174` (#21). |
| F22 metric gaps | **PARTIAL** | Net saved and the `would_hit` verified test are closed. Undefined series remain (#27). |
| F23 exclusive time | **CLOSED** by interval union (`metrics:43-45`). Its aggregation double-counts (#11). |
| F24 admission timing | **PARTIAL** | The interim rule is stated (`metrics:186`). Admission is still at reservation, not at export scheduling (`metrics:187`). P2, not re-raised. |
| F25 R7 residue | **PARTIAL** | Fallback text fixed. "A NetworkPolicy denying engine egress" survives at `architecture.md:335` (#24). |
| F26 session-end fail | **OPEN** | Folded into #14. |

### GPT round-3 findings (1–22)

| # | Status | Clause / residue |
|---|---|---|
| 1 P0 reserve | **PARTIAL** | Same as F1: #5, #6, #19. |
| 2 P0 pin authority | **CLOSED for the paused publisher** (`ledger.md:156-162`). It reappears as a two-store atomicity defect (#2, #3). |
| 3 gen binding | **PARTIAL** | `graph_refs` gate at `ledger.md:15-20`. It is not enforced in the reserve SQL (#6), is violated by the scanner (#8), never decrements (#1), and restore cannot evaluate it (#7). |
| 4 mint join | **CLOSED** | `ledger.md:183-185`. It now has a lock-order defect (#4). |
| 5 failed evidence | **PARTIAL** | `ledger.md:224`. The non-head case is blocked by the mirror rowcount rule (#6). The late-PUT reconciliation trigger is unspecified (#14). |
| 6 waiter second upload | **CLOSED** (`ledger.md:166`). |
| 7 classes | **CLOSED**, except an attribute-name error (#27). |
| 8 attribution | **CLOSED** for the 10 s/100 s case. New double count (#11). |
| 9 net saved | **CLOSED** in formula (`metrics:71-73`). The C input is unsound (#10). |
| 10 breaker population | **PARTIAL** (#15). |
| 11 reclaim direction | **PARTIAL** | Direction closed. Construction broken (#9). |
| 12 quota | **MOSTLY CLOSED** | `metrics:170-172`. Residue: `architecture.md:174` "ledger SUM(size)" and the cancellation semantics (#29). |
| 13 renew round trip | **CLOSED** (`architecture.md:99`, `ledger.md:190`). |
| 14 value-existence | **CLOSED** (`architecture.md:160`). |
| 15 merge retention | **CLOSED** (`architecture.md:170`, `metrics:155`). |
| 16 MinIO | **PARTIAL** (#23). |
| 17 I3/corruption | **PARTIAL** | I3 and a `quarantined` path were added. The trigger is ungrounded, and the build-fail text survives (#25). |
| 18 circular gate | **CLOSED** in `metrics:131-132`, `decision.md:61` and `architecture.md:264`. Residue at `architecture.md:19,197,318` (#22). |
| 19 probe TTFB | **CLOSED** for GET/HEAD. Open for PUT (#12). |
| 20 table precision | **CLOSED** arithmetically. |
| 21 admission Boolean | **CLOSED** (`metrics:181-186`). |
| 22 ratios/PromQL | **PARTIAL** | Renew alert fixed. The regret PromQL is not the cohort it claims, and its denominators are undefined (#27). |

---

## Q2. Attacking the publish path

**(a) Deadlock.** I **cannot** build a pure-Postgres deadlock between publish and GC mark. Mark locks `pool_leader`, then one digest's `(b,h)` (`ledger.md:196-199`). Publish locks `(b,h)` in digest order and never takes `pool_leader`. A single-digest holder cannot close a cycle. **Three other cycles exist:**
1. *Publish ↔ mint* (#4). Mint's `UPDATE … FROM … ANY($chain)` locks blob rows in plan order, which is not digest order. Publish P holds `b(A),h(A)` and waits on `b(B)`. Mint M has updated `b(B)` and waits on `b(A)`. PostgreSQL resolves this after `deadlock_timeout` (1 s), which eats half of the 2 s renew deadline.
2. *Publish ↔ dagql write lock* (#3). This cycle is invisible to Postgres. P holds rows and blocks in `SetStoredPart` on the graph write lock. Suppose any goroutine holds the graph lock (for example, `CommitValueMerge`, a quarantine "drop parts under head lock", or a sweeper keeping its `LiveBlobs` snapshot consistent) and issues a statement on one of P's rows. The process then hangs, and no detector fires.
3. *Lease cascade* (#3). P waits on the graph lock (`CommitValueMerge` "can exceed 5 s", `architecture.md:226`). GC holds `pool_leader FOR UPDATE` and waits on P's rows. The leader's own lease-renew `UPDATE pool_leader` waits on GC. The lease then lapses, and the leader self-terminates at half-TTL (`architecture.md:342`). Result: a cold-restore outage of several minutes, triggered by a slow merge.

**(b) Killed between `SetStoredPart` and `COMMIT`.** The spec does not say whether `SetStoredPart` is durable. The pool graph is `dagql.NewCache(sqlitePath)` on a PVC (`architecture.md:96,270`).
- *If the mutation is in memory only*, process death loses both sides. That case is consistent.
- *If it writes through to SQLite and restart reuses the PVC working copy*, the part survives with `graph_refs` un-incremented. `last_ref_at` is also rolled back to `verified_at`. After G the GC marks it, deletes it and the part 404s. Restore reconciliation (`ledger.md:230`) runs only "after a leader restore", and whether a same-pod restart counts is undefined.
- *The process-alive variant is worse*: see (c).

**(c) A rolled-back publisher can leave graph parts.** Yes (#2). Concrete cases:
- `COMMIT` returns an error after a Patroni switchover.
- The connection resets during `COMMIT`. The outcome is unknown, and PostgreSQL may have committed.
- `idle_in_transaction_session_timeout` or `statement_timeout` kills the session while `SetStoredPart` waits on the graph lock.

In each case the graph holds the part and the ledger shows `graph_refs` and `last_ref_at` un-advanced. The process keeps running, so no restore reconciliation ever runs. There is no compensating action, and no precise one exists: `DropStoredParts(digest)` also drops every unrelated part that shares any digest (RP :393).

**(d) The `graph_refs=0` gate does not hold.** Three concrete breaks:
1. *Scanner* (#8). The stuck-`deleting` path stamps `deleted` "otherwise it leaves the parts" (`ledger.md:223`). The reserve SQL accepts `deleted` without reading `graph_refs` (`ledger.md:98`; `graph_refs` lives on `blob`, and the `INSERT … SELECT` reads only `blob_head`). gen2 is allocated while gen1 still has parts.
2. *Under-count* from (b) or (c): gen1 has a part and `graph_refs=0`. GC deletes it, and reserve allocates gen2. Two generations then have parts.
3. *Restore* (#7). The checkpoint lags the ledger by ≥10 min. Every digest published since the checkpoint has `graph_refs` greater than its actual part count. Reconciliation "quarantines the digest", and quarantine deletes immediately with no G (`ledger.md:70,225`). Engines that were merged in that window hold valid 1 h GET URLs and now get 404s. Each failover becomes a mass-delete of the newest and hottest blobs.

Generations hold identical bytes (same digest), so a "wrong-gen" part is mostly an **accounting** error. That is part of the argument in Q8 that `graph_refs` should not exist.

## Q3. Reserve

The branch is correct for `absent`, `verified`, `pending` and `deleting` (`ledger.md:93-105`). The partial unique index cannot fire while the head mirror is accurate. **It does not implement the transition table exactly:**
- Row `failed→pending` requires "object of that gen physically deleted" (`ledger.md:62`), but the SQL accepts `failed` unconditionally (`:98`). This contradicts invariant 5 (`:238`) and the bullet at `:107` (#6).
- Row `deleted→pending` requires `graph_refs=0` (`:63`). The SQL never reads it (#6).
- A `quarantined` head matches neither the allocation set nor any zero-row action (`:105`) (#19).
- `UPDATE blob_head SET gen=$g_new …` (`:101`) is unconditional. On zero rows it writes NULL into a NOT NULL column and aborts. The caller then "reads the head state", unlocked, after the abort (#19). The locked `SELECT … FOR UPDATE` already returned the state, so use that.
- The index stops being "only a backstop" once the mirror diverges (#5): then gen2 is legal even though gen1 is verified.

## Q4. Metrics computability

- **Interval union.** Computable from `WcprofParentAttr`/parent and `result.id` (RP attrs :772-777). Per entry it includes descendant entries' work, and §2 sums it across entries, so nested work is counted at every ancestor (#11).
- **Assumed attributes.** `cache.cache_span_count` does not exist; the real attribute is `dagger.io/cache.session.spans` (RP :790-793) (#27). `miss_reasons` and the set {incompatible, expired, released} have no source in the contract table (RP :42-57) (#18).
- **Gross/net saved.** The formulas are bounded and nonnegative. C (`donor_recompute_seconds`) is stored per blob generation at reserve time (`ledger.md:37,95`). That is before session finalization (`metrics:11`), and it is not per entry (#10).
- **Hit ratio.** Bounded, NaN at 0/0. Fine.
- **Ceiling.** ≤ 1, but `recompute_remaining` already contains the `would_hit` and `lost_keying` entries, so the denominator double-counts them, and the numerator mixes net and gross (#28).
- **Regret.** "Bounded by construction" is true of the cohort definition (`metrics:176`), not of the PromQL (`metrics:144`). The PromQL is a ratio of two independent 24 h rates, and its denominator series is undefined (#27).
- **Value ratio.** Needs `recompute_remaining`, which no emitted series provides: `dcache_recompute_seconds_total` includes mixed entries (`metrics:54`) (#11).

## Q5. Eviction

Lowest-score selection fixes the R2 *direction* bug. But the candidate rule (`metrics:162`) keeps only groups that leave "every referenced blob with zero surviving references". Under union-find, that means whole connected components. A base layer shared by most parts puts the pool into one giant component, so the cold-sharer case becomes **unevictable without also evicting the hot part** (#9). The overshoot cap then forces a "split by dropping its largest part" (`metrics:166`), and the split breaks the closure rule just stated. The result is either starvation (nothing evictable) or overshoot (the component goes).

Quota is consistent with tombstones (`metrics:170`). `pending_free` restates `state='deleting'`. "New references cancel the projection" cannot happen after a mark, because publish requires `verified` (#29). Quota is also unreachable in practice: GC never frees a blob that was ever published (#1).

## Q6. Consistency grep (file:line)

| Theme | Surviving clause |
|---|---|
| Expiring pin | `architecture.md:49` "and pinned (`ledger.md` §4)"; `architecture.md:227` "publish under a durable pin (`ledger.md` §4), then `SetStoredPart`" (#16) |
| R1 SLIs / self-time | `architecture.md:260` (#17) |
| Threshold gate | `architecture.md:19` ("threshold in §9"), `:197` ("Weight both by span duration"), `:318` ("go/no-go threshold") (#22) |
| MinIO as production | `architecture.md:164,216,250,270`; `:211` "on-prem engines" (#23) |
| Probe as breaker | `architecture.md:250` "Health probe fails → stop merges…" vs `:251` "probe is liveness only" (#23) |
| Pre-generation keys/dedupe | `architecture.md:40,137` (no `/g<gen>`), `:154` "same key is idempotent", `:161` "`INSERT … ON CONFLICT DO NOTHING`", `:162` "S3 keeps one atomically", `:174` "ledger `SUM(size)`" (#21) |
| Wrong bytes fail the build | `architecture.md:150,258` (#25) |
| NetworkPolicy for FQDN | `architecture.md:335` (#24) |
| Cost total | `architecture.md:209,214`; `decision.md:48` (#26) |
| Retention on merge, value-existence dedupe, p99 breaker, "every blob hashed", renewal without DB write, 30 % gate | **none found** in normative text. `architecture.md:150` mentions "every blob hash-checked" only as the rejected stronger guarantee, which is correct. |

## Q7. Latency re-derivation

- The arithmetic checks against the displayed rates: 2000/109.5 = 18.26, 2000/62.7 = 31.90, 2000/97.0 = 20.62, sums 38.9 and 52.5.
- **But 109.5 MB/s is not a transfer rate** (#12). `put_object` stops `body_s` when `c.request()` returns (`s3_latency_probe.py:89-90`), which means the 4 MiB has been copied into the kernel send buffer. That is the same bug class R3 fixed for GET TTFB. The end-to-end rate is 4,194,304 B / (38.3 + 19.2) ms ≈ **72.9 MB/s**, giving **≈27.4 s** for 2 GB on the fast path and **≈48 s** with a full reread. That rate also agrees with the 8× aggregate (62.7 MB/s), which removes the oddity of aggregate < single-stream.
- **Per-stream under contention** (#13). The engine shares 8 PUT slots per session (RP :158). 62.7/8 ≈ 7.8 MB/s, so a 2 GB layer among 8 takes ≈ **255 s**, not 31.9 s. Line 34 divides by the whole aggregate, which line 24 forbids.
- **Unsupported categorical claim**: `latency:43`, "'cannot publish inside 30 s' is false". That rests on one 4 MiB in-cluster PUT with a mismeasured timer, before compression or telemetry delay, on a non-production store. Supported version: "≈27 s best case, design cluster, transfer only; unknown for production."
- **Donor-death distinction** (#14). Correct in principle: verification is service-side. But it is not executable. Verification is triggered by `uploaded.done` (`architecture.md:227`). A donor that finishes its PUT and dies before `uploaded` (which follows *all* of the export's PUTs, RP :150-158) goes lease→`failed`→object deleted (`ledger.md:120-121,224`). The donor must survive the whole export, not just its own PUT.

## Q8. What I would cut

**Cut Postgres as the publish/GC serializer. Cut `graph_refs`, the head-lock-across-`SetStoredPart`, and the `blob_head.state` mirror. Serialize publish, GC mark and restore in-process under one per-pool mutex in the single fenced writer, and take liveness from `LiveBlobs()`.**

I6 already guarantees one writer per pool, and the graph lives in that process. So:
- `publish = lock M; read ledger verified-at-head; SetStoredPart; unlock`
- `mark = lock M; LiveBlobs(); conditional UPDATE … SET state='deleting' WHERE state='verified' AND last_url_mint_at < now()-G AND <epoch fence>; unlock; DELETE g<gen>`
- Mint stays a single conditional `UPDATE … RETURNING`. It races mark only on one row's predicate.

A deposed leader's writes fail the epoch predicate. Its in-memory graph is not authoritative. Generation keys stay, and stay ledger-only, to defeat delayed DELETE ABA. Restore drops parts whose digest is not verified-at-head, which needs no gen and no reconciliation.

This removes, as a class: #1, #2, #3, #4, #5, #7, #8; the reserve `graph_refs` guard in #6; and open item 8 in `revision-r3.md:58`. The cost is that `LiveBlobs()` runs under M, an in-memory scan once per hour. That cost is measurable, and it is the same lock `SetStoredPart` already contends.

## Q9. New errors introduced by R3

These are #1–#12, #19, #27 (the attribute name), #28 and #29 below. The rest are carried over or surviving clauses.

---

## Findings

### 1 [P0] `graph_refs` is incremented on publish but never decremented on the dominant removal paths, so GC can never delete a once-published blob
`ledger.md:17-20,149,203,225`; `architecture.md:170-171,233`; `metrics-and-eviction.md:162-166`. §0 says "every publish and drop" adjusts `graph_refs` in §5 and §8, but §8 contains no drop. The parts actually disappear in three ways:
- dagql retention pruning of expired roots (`architecture.md:170`), which the service does not observe per part;
- eviction group removal (`metrics:164`), whose mechanism is unspecified;
- renew-miss `DropStoredParts(digest)` (`architecture.md:171,233`).

None of these decrements. Worse, `DropStoredParts(digest)` removes every part that *mentions* the digest (RP :393), and those parts also reference other digests, whose counters nobody touches. GC's `graph_refs=0` predicate (`ledger.md:203`) is therefore unreachable for any blob that was ever published. Storage grows to the 0.95 watermark, admission stops (`architecture.md:174`), and the cache degrades monotonically. Restore reconciliation (#7) then "repairs" the over-count by quarantining. **Fix:** Q8. Otherwise, define `graph_refs` as derived (recount from `LiveBlobs()` under the graph lock before each mark), not maintained.

### 2 [P1] `SetStoredPart` is not part of the Postgres transaction; a failed or unknown COMMIT leaves an unreferenced graph part with no compensation
`ledger.md:156-160,245`. Cases: COMMIT error on Patroni switchover, connection loss during COMMIT, or a session timeout while blocked on the graph lock. The graph has the part, and `graph_refs` and `last_ref_at` are not advanced. "Authority is a live transaction" holds only for the DB side. No undo exists, because `DropStoredParts(digest)` is too coarse. The result is GC of a referenced blob after G, then 404, then drop of every part sharing the digest. **Fix:** Q8. Otherwise, specify reconciliation on every COMMIT error and on every startup, not only "after a leader restore".

### 3 [P1] DB row locks are held while waiting on the dagql write lock: an undetectable cross-resource deadlock, renew starvation and a leader-lease cascade
`ledger.md:141-157,196`; `architecture.md:99,226,342`; RP :434 (merge commits under the graph write lock); #15049 (eq-class walks under a global lock). Lock-hold time equals graph-lock wait time, not "milliseconds" (`ledger.md:245`). Mint (renew path, 2 s deadline) blocks on publish's `b` rows. GC holds `pool_leader FOR UPDATE` while it waits, which blocks lease renewal, which leads to self-termination (Q2a.3). Any graph-lock holder that touches a locked row gives a deadlock PostgreSQL cannot see. **Fix:** Q8. At minimum, state a global order (graph lock before DB rows, never the reverse), take `pool_leader` with `FOR SHARE` or check it via the epoch predicate instead of locking it, and set `lock_timeout` < 1 s on mint.

### 4 [P1] Mint locks blob rows in plan order and deadlocks with digest-ordered publish
`ledger.md:180-185` vs `:141-147`. Interleaving in Q2a.1. Overlapping mints over different chain orders deadlock with each other too. Every victim costs ≥ `deadlock_timeout` on a 2 s budget. **Fix:** `SELECT … ORDER BY digest FOR UPDATE` before the UPDATE, or Q8.

### 5 [P1] Head-mirror updates are not conditioned on the primary statement's rowcount; a verify/lease-expiry race creates two generations and leaks gen1
`ledger.md:75-80,116-124,128-134`. Timeline:
1. Verify starts at t=599.9, so `now()` is the transaction start and `lease_until > now()` holds. It commits at 600.6.
2. The sweeper's candidate SELECT at 600.5 sees `pending`. Its `UPDATE blob … state='pending' AND lease_until<now()` waits on verify's row lock, then matches 0 rows on recheck.
3. Its "head mirror for both" statement still sets `head.state='failed'`, rowcount 1, so the transition counts as satisfied.
4. Reserve now allocates gen2 over verified, published gen1.
5. Mint (head-joined) can no longer serve gen1, and GC (head-joined, `:197-199`) can never mark it. That is a permanent leak and two generations.

The §2 rule checks the mirror's rowcount, not the primary's. **Fix:** a single statement (`WITH t AS (UPDATE blob … RETURNING …) UPDATE blob_head … FROM t`), or require primary rowcount = 1 before the mirror runs. Or drop the mirror (Q8).

### 6 [P1] Reserve accepts `failed` and `deleted` without their guards; a non-head failed row can then never finish cleanup
`ledger.md:62-63,98,107,224,238`.
- `failed` is allocatable at once, contrary to `:62`, `:107` and invariant 5.
- After allocation, the old failed row's `failed→deleted` mirror update uses `WHERE gen=$old` and matches 0 rows. Per §2 that is "ROLLBACK", so the transition is void forever, and a late-PUT object at `g<old>` is never reclaimed.
- `deleted` is allocatable without reading `blob.graph_refs`.

**Fix:** restrict the allocation set to `absent` and `deleted`, join `blob` for `graph_refs=0`, and exempt non-head transitions from the mirror rule.

### 7 [P1] Restore reconciliation quarantines every digest published since the last checkpoint and deletes it immediately, bypassing G
`ledger.md:70,225,230`; `architecture.md:100,104`. The checkpoint floor is 10 min, so post-checkpoint publishes are lost from the graph but still counted in `graph_refs`. A mismatch leads to quarantine, which goes to `deleting` "immediately, no age gate". URLs minted in the last hour 404. This is the newest and hottest data. Also, "its `(digest,gen)`" is not representable in the graph (round-3 F3). **Fix:** on restore, drop parts whose *digest* is not verified at head. Never quarantine for accounting drift. Recount instead.

### 8 [P1] The recovery scanner stamps `deleted` while parts remain, which lets reserve open a second generation
`ledger.md:223`, together with `:98`. This is the concrete break of Q2(d). **Fix:** the scanner always calls `DropStoredParts(digest)` before the stamp, as round 3 F19 asked.

### 9 [P1] The union-find closure rule makes reclaim groups whole connected components; cold sharers of a hot base layer are unevictable, and the split rule contradicts the closure
`metrics-and-eviction.md:162,166`. Hot H=[A,B], cold C1=[A,X], C2=[A,Y]: the only legal group is {H,C1,C2}. Freeing X requires evicting H. The 1,000-candidate cap is "ordered by the blobs they free", so the lowest-score group may not be in the candidate set at all. **Fix:** any set of parts is a candidate. Freed bytes are only the blobs whose referencing parts are all in the set. Generate candidates greedily from the lowest-benefit parts outward.

### 10 [P1] `donor_recompute_seconds` is stored per blob generation at reserve time: wrong granularity, wrong moment, and missing on reuse
`ledger.md:37,93-95`; `metrics:23,56-58,67,163,182`.
- A blob is shared by many parts and entries. The first reserver's span cost is attached to a shared base layer.
- Summing over a chain counts it per layer.
- The reuse path (verified ⇒ no reserve) records nothing for the new entry.
- The value is written before the session-finalized interval union exists (`metrics:11`).
- Metrics says the value lives "on the published part", but the ledger stores it on the blob.

Saved gross/net, eviction benefit (which also multiplies per-part installs by per-entry cost, round 3 F7(iv)) and admission all depend on it. **Fix:** store it keyed by `(pool, recipe/part)` at session finalization, in service metadata beside the graph.

### 11 [P1] Per-entry interval union includes nested entries' work, and §2 sums it across entries; `recompute_remaining` is not an emitted series
`metrics:43-47,54,73,75`. If E calls F, F's lazy spans have E's call as a causal ancestor, so F's seconds are counted in F and again in E. Recompute totals, the value-ratio denominator and the ceiling are all inflated by nesting depth. **Fix:** stop the ancestor walk at the nearest span carrying a different `result.id`, and emit `dcache_recompute_remaining_seconds_total` separately from mixed-entry recompute.

### 12 [P1] The PUT "body rate" measures socket-buffer write, not transfer; the real single-stream rate is ≈72.9 MB/s and the 2 GB fast path is ≈27.4 s
`s3_latency_probe.py:84-96`; `latency-and-azure.md:15-16,33-37,43`. Derivation in Q7. The "18 s" headline and "'cannot publish inside 30 s' is false" do not survive. Best case, transfer only, design cluster, the result is ≈27 s against a 30 s notice, with compression and telemetry excluded. **Fix:** report (body+response) as the PUT rate, and restate §1 as marginal and design-cluster-only.

### 13 [P1] The aggregate scenario divides 2 GB by the whole 8-writer aggregate (carried from round 3 F11)
`latency:17,24,34,44`. The per-stream rate is ≈7.8 MB/s, giving ≈255 s. Line 24 forbids the very substitution line 34 makes.

### 14 [P1] Dead-donor publication is still not executable (round 3 F10/F26, GPT 5)
`latency:45`; `architecture.md:227,240`; `ledger.md:111,120-121,224`. `uploaded` follows all of the export's PUTs, and lease expiry goes straight to `failed`, then delete. **Fix:** on WS close, `EndRemoteProcess` or `uploaded` timeout, HEAD each `pending` key of that export and verify the ones that exist. Fail the rest at once instead of waiting 10 min.

### 15 [P1] The production breaker cannot see the failure it exists for (a hanging store)
`metrics:118-125`; `latency:65,74-79`; `architecture.md:251`. Three gaps:
- `install_p95` only covers successful installs, so a hang that ends in fallback drops out of the sample.
- Server access logs (S3 or Azure) arrive minutes late and do not record client-side timeouts.
- Production has no "MinIO metrics exporter".

The trip input is therefore empty in exactly the hang case. **Fix:** name the production source and its delay. Add an engine-path signal the service *can* observe: renew rate and `renew_unavailable`, plus `computed` entries that had a minted URL in the last hour. State what remains undetectable.

### 16 [P1] `architecture.md` still specifies the removed expiring pin
`architecture.md:49` ("and pinned (`ledger.md` §4)") and `:227` ("publish under a durable pin (`ledger.md` §4), then `SetStoredPart`"). This directly contradicts the R3 core change, and §4 is now verify.

### 17 [P1] The R1 SLI formulas survive verbatim for a third round
`architecture.md:260`. "lazy-inclusive recompute − install", "self-time of executed spans" and "distinct installs ÷ (installs + executed)" contradict `metrics:56-75`.

### 18 [P2] `miss_reasons` and its value set are ungrounded; `demand_p95` is mislabelled
`metrics:21,120`; `latency:67`. RP :42-57 has no miss-reason attribute. Recompute duration is not "wait before fallback". The stall time is not observed at all.

### 19 [P2] Reserve edge cases: no `quarantined` branch; unconditional `$g_new` update; unlocked re-read
`ledger.md:90-105`. See Q3.

### 20 [P2] The leader fence checks `holder`, not epoch; the delete stamp and scanner are unfenced; `delete_epoch` is never compared
`ledger.md:196,209,213-216,223`. StatefulSet pod names repeat across epochs. Use `epoch=$mine` in every ledger write.

### 21 [P2] Pre-generation storage semantics survive in architecture
`architecture.md:40,137` (keys without `/g<gen>`), `:154`, `:161-162`, `:174`.

### 22 [P2] Withdrawn-threshold residue
`architecture.md:19` ("threshold in §9", and §9 is request flows), `:197`, `:318`.

### 23 [P2] MinIO-as-production and probe-as-breaker residue
`architecture.md:164,211,216,250,270`. Line 250 contradicts 251.

### 24 [P2] The R7 NetworkPolicy text contradicts F59
`architecture.md:335` vs `:86`.

### 25 [P2] Wrong-bytes behaviour is inconsistent, and the quarantine trigger is ungrounded
`architecture.md:150,258` say the build fails, but the engine column and I7 say fall back to compute. `ledger.md:69,225` "engine mismatch report": no such message exists in the protocol, so unsampled corruption still has no detection path.

### 26 [P2] The cost total was not recomputed, despite `revision-r3.md:36`
0.10 + 0.008 + 0.12 + 0.004 = $0.232/day ≈ $7/month. The docs say $0.27 and $8 (`architecture.md:209,214`; `decision.md:48`).

### 27 [P2] Wrong attribute name and undefined series; the regret PromQL is not a cohort
- `metrics:11`: `cache.cache_span_count` should be `dagger.io/cache.session.spans`, read from the session-complete carrier span.
- `dcache_gc_evicted_total`, `dcache_s3_requests_total` and `dcache_sessions` are not defined in §2 (`metrics:141,144-145`).
- The regret ratio of independent `rate[24h]` series (`:144`) can exceed 1 after an eviction burst. That contradicts `:176`.

### 28 [P2] The ceiling denominator double-counts opportunity sets
`metrics:75`. `would_hit` and `lost_keying` entries are already in `recompute_remaining`, and the numerator mixes net (`saved_net`) with gross. The ratio stays ≤ 1, but it is biased low. Also, the value ratio reads 100 % on a 1 s net gain (`:73`). That is disclosed, but a poor headline KPI.

### 29 [P2] Quota residue
- `pending_free` duplicates `state='deleting'`.
- "New references cancel the projection" is impossible after a mark (`metrics:171`; `architecture.md:174`).
- `quarantined` rows and `failed` rows whose late-PUT object still exists are physically stored but uncharged (`metrics:97,170`).

### 30 [P2] The 4 s waiter budget abandons publishable parts behind slow-but-live owners
`ledger.md:170`. A 2 GB partner takes ≥27 s (#12). Expire the wait with the partner's lease, as round 3 F2 suggested.

### 31 [P2] `decision.md:33` says `rowcount == |chain|`; the ledger requires distinct digests
`ledger.md:154`.

---

## Minimum for a convergent R4

1. Adopt Q8. Delete `graph_refs`, the head-lock-across-`SetStoredPart` and the mirror. Add an epoch predicate to every ledger write. Restore by digest. This disposes of #1–#8.
2. Re-run the probe with an end-to-end PUT timer. Restate §1 with per-stream rates and production caveats (#12, #13). Add the disconnect-triggered HEAD (#14).
3. Fix candidate generation, and move donor cost to finalized per-part metadata (#9, #10, #11).
4. Do one mechanical grep pass over `architecture.md` and `decision.md` using the Q6 table (#16, #17, #21–#26).
