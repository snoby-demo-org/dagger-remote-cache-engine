> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Independent review: dcache design (beta.25 + self-hosted remote cache)

Reviewer: Claude (adversarial pass). Date 2026-11-07.
Inputs read in full: `decision.md`, `architecture.md`, `metrics-and-eviction.md`, `latency-and-azure.md`, `s3_latency_probe.py`, `live-helmrelease.yaml`, `live-networkpolicy.yaml`, and `recon-protocol.md` §0–§14 plus Appendix A `attrs.go:595-625` and `:691-793`. RP wins every conflict. **No dcache code exists**, so this reviews the paper design's internal consistency and its claims, plus the two artifacts that do exist (the probe and the live manifests).

Severity: **P0** = the design is wrong or can't be built as written, so fix it before any code. **P1** = a correctness, cost or feasibility defect that will bite in Phase 2–3. **P2** = an inaccuracy or hygiene issue.

---

## Verdict

**STOP.** Don't start building `dcache` yet. Two things block it. First, the discovery channel the whole design depends on has not been shown to work under self-hosted config (F5). Second, the documents disagree on node lifetime and on what the production object store is (F1, F2). Phase 0 can go ahead now, together with the "week version" (A) and the three spikes in *Top 5*. The go/no-go metric must be redefined first (F20, F21).

---

## Question → finding index

| Q | Answer (short) | Findings |
|---|---|---|
| 1 | There are no REST, `.zip` or engine-multipart assumptions. One minor 206-only inaccuracy, and one dead multipart lifecycle rule. | F10, F11, F8 |
| 2 | Entry numbers come only from OTLP call spans, and RP does not prove those spans reach a self-hosted URL. A brand-new engine can seed only if they do, and then only for ops that pass admission. Sub-60 s ops never seed in the first session. | F5, F33 |
| 3 | The limits are respected nominally. But the 5 s watchdog destroys exports under lock stalls, and the 48 MiB cap isn't measured on the final frame. | F6, F7, F8 |
| 4 | No flow counts `accepted` as a hit directly. But merges/offers count as "served", and metadata-only remote hits count as hits. | F20, F25, F34 |
| 5 | Yes. A chain with ≥2 digests passes the "zero rows" fence while one digest is `deleting`. Failover also resurrects deleted blobs. | F12, F13, F14 |
| 6 | Engines don't get stuck (they fail open). The *cache* gets stuck: the scheduler's dedupe rule keeps a value metadata-only forever. | F15 |
| 7 | Constructed: a mint TOCTOU with a buffered or post-send stamp, and failover. | F16, F14 |
| 8 | No. The fence doesn't explicitly cover renewal-minted URLs, and "+30 s" is not a transfer bound. | F17, F18 |
| 9 | Not computable as written. A hit span's duration is not the compute saved, lazy work sits in spans with no outcome, and ns→s conversion is unspecified. | F20, F21, F22, F24 |
| 10 | No digest or call-name labels. A `repo` label exists with no defined source. | F26 |
| 11 | The ceiling double-counts `lost_session_keying`. Nested spans are double-counted. Repeated hits on a remote-origin entry inflate the hit count. | F23, F24, F25 |
| 12 | Safe, but inefficient and unstable: score uses non-marginal bytes, and the controller over-evicts while frees lag ~2 h. Regret is mis-attributed. | F31, F32 |
| 13 | Yes. A 45 s daily build is never admitted. | F33 |
| 14 | Not safe. `evicting` isn't a legal state, and if it were, nothing reverts it, and it disables the blob. | F30 |
| 15 | The 5% rule isn't meaningful. "PUT TTFB" is really throughput, and the publication arithmetic is off by ~3×. | F36, F37, F38 |
| 16 | The design rejects native Blob for the wrong reason. MinIO-on-AKS has real counter-arguments. | F39, F40, F41 |
| 17 | HEAD bug explained, plus 7 more. | F43–F50 |
| 18 | 10 required changes. Memory limit, configJson clobber, CA trust and egress policy are the blockers. | F51–F61 |

---

## 0. Topology & internal consistency — **FAIL**

**F1 [P0] `decision.md:1`, `architecture.md:24`, `latency-and-azure.md:20`, `metrics-and-eviction.md:8`.** Claim: the nodes are "30-second-ephemeral". Problem: the documents use "30 s" both as the *notice* window (arch:24 "killed with ~30s notice") and as the *lifetime* (latency:20 "a 30-s-lifetime node", decision title, metrics:8 "~30 s"). Every feasibility argument depends on which one it is. If 30 s is the lifetime, a fresh engine has to pull its image onto a new node, wait for `welcome` (up to 30 s, RP §4.2), apply a warm merge, run the job, and then get spans → export → compress → PUT → verify done before the node dies (RP §5.2: entries released at session end come back as `gone`). Nothing seeds and little gets consumed. Fix: state lifetime (p10/p50/p90) and notice separately. If the p50 lifetime is under a few minutes, Tier 1 is not viable and only the week-version (A) applies.

**F2 [P0] `decision.md:20` and `metrics-and-eviction.md:8-11` vs `architecture.md:209,277` vs `latency-and-azure.md:42`.** Claim: production uses "external S3 outside the cluster". Problem: the architecture explicitly rules that out ("An off-cluster S3 as the *primary* read tier is ruled out by egress and latency", arch:209; "No off-cluster S3 as the primary read tier", arch:277). The Azure doc recommends MinIO in-cluster, while metrics:10 says "production does not run MinIO at all". That makes three incompatible production topologies. The §8 cost table (MinIO column, $0 egress), the R5 mitigations and the breaker table each describe a different one. Fix: choose one production byte plane. Then redo arch §8 cost (include verify-read egress, F62 checkpoint bytes, and GET egress) and the breaker thresholds for it.

**F62 [P1] `architecture.md:97,101,142,202`; `metrics-and-eviction.md:12`.** Claim: a checkpoint every 60 s to `ckpt/<epoch>/<seq>.db`, kept 7 days, costing $0.036/day, with a "warm standby that tails checkpoints" (mandatory per metrics:12). Problem: each checkpoint is a full SQLite DB. #15013 shows 7.5 GB for *one engine's* graph, and a pool graph accumulates every engine's recipes (arch:320). Even at 2 GB, that's ~2.9 TB/day written per pool and ~20 TB retained over 7 days. The cost table counts only PUT requests. Restore is linear in DB size (2m36s for 7.5 GB, #15013), which is longer than the 60 s cadence, so a "tailing" standby can never keep up. Fix: checkpoint on change with a ≥10 min floor, keep the last K checkpoints per pool (pointer-driven GC, not a 7 d lifecycle rule), and measure DB size in Phase 2. Make the standby a cold restore with an accepted outage; that's acceptable because the cache fails open (I7).

**F3 [P2] `decision.md:20`, `metrics-and-eviction.md:12`.** Claim: "`dcache` is the only stateful component". Problem: Postgres (Patroni: ledger, tokens, leader epoch; arch:41-43,108-123) and the SQLite PVC are also stateful. Losing Postgres removes I5/I6. Fix: list Postgres as a production stateful dependency with its own placement and HA plan.

**F4 [P2] `decision.md:65`, `architecture.md:258` vs `live-helmrelease.yaml:11,43-54`.** Claim: Phase 0 runs on v0.21.11 and "ships" a Zot pull-through. Problem: the live system runs 0.21.9, and Zot already exists as the LAN registry (`development.example.test`). Fix: inventory what Tier 2 already provides. Run Phase 0 on 0.21.9, since Tier 2 doesn't depend on the engine version, and query the **existing Tempo traces** (helmrelease:82-100 already ships engine spans) for `dagger.io/cache.*` attributes. That answers arch open Q4 in an hour.

**F63 [P2] `decision.md:27`, `architecture.md:13`.** Claim: Tier 2 caches are "injected into `withExec`". Problem: env vars are part of the op key (and #14913 shows env keying bugs). Injecting endpoints or credentials as env means rotating credentials invalidates every Tier 1 key. Fix: pass credentials as secrets (not keyed) and keep env values constant across runners.

---

## 1. Protocol conformance — **FAIL** (wire shapes correct; discovery channel UNVERIFIED and load-bearing)

**F5 [P0] `architecture.md:92,217-218,330`; `decision.md:13`; RP §1.3, §11.1, §11.2.** Claim: the engine's call spans (`cache.result.id`, `cache.parts`, `cache.outcome`) reach dcache's `/v1/traces` once `DAGGER_CLOUD_URL` points there. Problem: RP establishes only that enabling remote cache turns on **engine events** (RP §11.1 row `_EXPERIMENTAL_DAGGER_ENGINE_EVENTS`). It says explicitly that the sample "does not prove all CLI/client telemetry routes are redirected — explicit per-session Cloud URL can take precedence" (`session_cloud_telemetry.go:66-73`, RP §11.2). If call spans are exported only for sessions whose client carries Cloud config, then a runner with no Cloud token yields just `engine.start/prune/share/stop`:
- `engine.stop` carries no entry numbers (RP §2.1 event shapes).
- `engine.share` covers snapshot-sharing of *imported* rows (RP §10.5 last paragraph), not freshly executed ones.

So a brand-new engine's executed work is invisible, and **it can never seed anything**. R7's fallback ("on `engine.share`/`engine.stop` … export recently executed roots", arch:330) can't name roots from those payloads.

Fix: a Day-1 spike. Run a beta.25 engine with only the engine-level env, a stub OTLP receiver, and a runner CLI without a Cloud token. Record which attributes arrive and the span-end→receipt latency. If call spans don't arrive, either configure per-session Cloud telemetry on the runners pointing at dcache (and revisit "reject `dag_` tokens", R7) or abandon Tier 1. A last-resort fallback is possible but weak: result numbers are monotonic per cache (RP §5.1), so blind `export{roots:[range]}` with `gone` skips can enumerate entries, but it gives no recipe or duration facts to schedule with.

**F6 [P1] `architecture.md:219-220,251`.** Claim: after `exported`, the service prepares, `CommitValueMerge`s, then sends `upload` "within 5 s, always, sending an empty map if nothing is needed". Problem: RP sets no upload deadline; the only cost of waiting is slot occupancy (RP §5.2). `CommitValueMerge` runs under the egraph write lock (RP §8.2, §10.2), and #15049 documents global-lock stalls. When commit takes more than 5 s, the watchdog sends `urls:{}`, which **releases the chains** (RP §5.3). The export is lost and has to be redone from an engine that, on spot capacity, is probably dead. Fix: enumerate the needed digests from the `exported` bundle's output chains (no commit needed, RP §12.2 "Inspect actual descriptor objects"), then reserve ledger rows, presign, and send `upload` immediately. Run `CommitValueMerge` concurrently. Keep a watchdog only as a 60 s backstop, alerting on `export_slot_age`.

**F7 [P1] `architecture.md:225`, `metrics-and-eviction.md:60`.** Claim: a warm merge is capped at "≤ 48 MiB of merge payload". Problem: `StoredBundle` output chains carry **no GET addresses or renewal keys** (RP §8.3); the service adds them afterwards, per chain. A SigV4 presigned URL is ~0.5–1.5 KB, and shared digests repeat in every chain that references them. If the cap is checked before URLs are attached, the final frame can exceed `64<<20`. An oversized received frame **closes the socket** (RP §4.2), the engine reconnects, gets the same merge, and loops at 1–30 s backoff. Fix: enforce the cap on the final serialized envelope, split into multiple `merge` requests, and add acceptance tests at 63.9 MiB and 64.1 MiB.

**F8 [P2] `architecture.md:218-220`.** The design doesn't handle two `exported` replies. One is the `error{"too large"}` substitution (RP §4.2). The other is the case with no `exportID` (no surviving root): there you must *not* send `upload` (RP §5.2). Fix: on `too large`, halve the roots and retry. On no `exportID`, record the skip reasons and send no upload.

**F9 [P1] `architecture.md:226`.** Claim: the live offer (b) is a hit path. Problem: the service only learns about the lazy entry from OTLP *after* the span ends, plus the batch delay and ingest time. Most CI pipelines demand the result immediately (`withExec` → `stdout`/`sync`), so the offer arrives after evaluation has started and the reply is `executionStarted` (RP §5.5, §10.2). The design gives no expected win rate. Fix: in v1, treat pre-merge as the only hit path. Measure `accepted`/`executionStarted` before investing in the PlaceOffer flow.

**F10 [P2] `decision.md:10`.** Claim: "reads are Range GETs requiring an exact `206 Content-Range`". Problem: a `200` whole-object response is also accepted (prefix discarded at a nonzero offset; RP §2 GET row). This matters when evaluating gateways or a read-through cache (R5, F39). Fix: state both behaviours.

**F11 [P2] `architecture.md:220`.** Claim: "It sends `upload{exportID,urls}`". Problem: `upload` has to be a **new request** with a fresh service ID and `re=0`, not a reply to `exported` (RP §5.3). This is an easy implementation trap and the design never mentions it. Fix: write it into the WS state-machine spec and add a contract test. Also, `AbortIncompleteMultipartUpload` (arch:142) applies to nothing, because arch:151 rules out staging and copy. Delete it.

Q1 sub-answers: there is no pull/on-miss lookup (arch:158 correctly cites RP §13.2 #3). There are no REST routes (arch:4 matches RP §2). There's no engine multipart (arch:146 matches RP §5.3) and no `.zip`-style key (arch §4.5 is digest-keyed, application-defined per RP §12.1). **PASS on those points.**

---

## 2. Invariants I1–I9 — **FAIL**

**F12 [P0] `architecture.md:53` (I5) and `:221`.** Claim: "Publication does `UPDATE … SET last_ref_at=now() WHERE state='verified'` and aborts if zero rows change." Problem: take a part whose chain is `[A,B]`. GC moves A from `verified` to `deleting`, then publication runs. The UPDATE touches only B, so **one** row changes, not zero. Nothing aborts, and `SetStoredPart` publishes a part whose blob A is being deleted. That violates I1 under a plain interleaving with one sweeper and one publisher. Fix: do it in one transaction: `UPDATE blob_ledger SET last_ref_at=now() WHERE pool=$1 AND digest=ANY($2) AND state='verified'`. Require `rowcount == cardinality(distinct $2)`, otherwise roll back and don't publish. Call `SetStoredPart` only after the commit. The GC's conditional UPDATE and this one take row locks, so they serialize correctly.

**F13 [P1] `architecture.md:155-156,132`.** Claim: reservation is "`INSERT … ON CONFLICT DO NOTHING` … `pending` row with a 10-minute lease", and yet "two exports may both get PUT URLs, for example after a lease expiry". Problem: those two statements contradict each other. With `DO NOTHING`, an expired `pending` or `failed` row blocks every later reservation until the sweeper removes it. The PUT TTL (15 min) is longer than the lease (10 min), so the original engine can still PUT after its row has become `failed`. The transitions for a late `done` against a `failed` row aren't defined. The sweeper can DELETE the object while a still-valid PUT URL rewrites it (an orphan), or while the verifier is streaming it. Fix: define the state machine explicitly:
- **Reserve:** `INSERT … ON CONFLICT (pool,digest) DO UPDATE SET state='pending', export_id=EXCLUDED.export_id, lease_until=EXCLUDED.lease_until WHERE blob_ledger.state='failed' OR (blob_ledger.state='pending' AND blob_ledger.lease_until<now())`.
- **PUT URL TTL** ≤ lease − maximum verify time.
- **Verify:** transitions `pending→verified WHERE export_id=$me`.
- **GC** deletes `failed` objects only after `lease_until + PUT TTL`.

**F14 [P1] `architecture.md:97,101,165` (I6).** Claim: a standby restores the latest checkpoint, and "loses only metadata since that checkpoint. That loss is cache-only." Problem: the sweeper decides using the *leader's in-memory* `LiveBlobs()`, while a standby restores the last *durable* checkpoint, which can predate a prune. Suppose checkpoints fail or lag for longer than G (plausible given F62). After failover, the restored graph contains stored parts whose blobs the old leader has already deleted. I1 is violated after restore, and nothing in the design re-checks. Fix: before serving after a restore, reconcile `StoredState` (RP §9.2, §12.4) against the ledger and call `DropStoredParts` for any digest that isn't `verified`. Also, or instead, have GC delete a digest only once the durable checkpoint sequence is at or past the prune that unreferenced it.

**F15 [P1] Q6. `architecture.md:154,219,234`; `decision.md:42`.** Claim: metadata merged before bytes is "harmless: other engines compute it, or a later donor fills the bytes". Problem: dedupe rule 1 is "skip exporting a root when `EquivalentEntries` shows a stored value". Step 4 commits the value *before* the bytes exist, so once the first donor dies mid-PUT (exactly the arch:234 case), **every later engine's export of that recipe is skipped**. The "next donor whose holding is still live" (arch:229) doesn't exist on ephemeral nodes. The value stays metadata-only until its TTL. Engines don't wedge, because a missing description leads to local computation (RP §7.2). But the cache misses permanently while the metrics count those runs as hits (F20). A description pointing at a later-deleted blob also doesn't wedge an engine: the GET 404s, the engine sends one `renew`, gets `unavailable`, and falls back (RP §7.2: "A renewed address never renews again in that demand episode"). Fix: dedupe on **part availability** (`AvailablePart == stored`, RP §8.3) or on a pending export with a live deadline, never on whether the value exists.

**F16 [P1] Q7 (constructed interleaving). `architecture.md:53,96,130,247`.**
1. Blob A is `verified`, part P is stored, and both `last_ref_at` and `last_url_mint_at` are older than G.
2. Engine E connects, and `StoredBundle` captures P under the read lock. In parallel, the quota controller prunes P's root (metrics:143), so the next sweep snapshot has A ∉ `LiveBlobs()`.
3. The service attaches a GET URL u(A) with a 1 h TTL. Its `last_url_mint_at` stamp is either written after `merge` is sent, or buffered in memory (arch:247 "buffers in memory").
4. The GC's conditional UPDATE to `deleting` commits, because the stamp is still old, and the object is DELETEd.
5. E holds a correctly signed URL to a deleted object. It gets a 404, then `renew` → `unavailable` → recompute.

The same race happens through `renew`: arch:130 checks the HMAC and `verified`, not that P is still stored. This breaks the design's own "no deleting bytes under a live URL" claim (metrics:125). Fix: make minting a conditional write, `UPDATE … SET last_url_mint_at=now() WHERE pool=$1 AND digest=ANY($2) AND state='verified' RETURNING digest`, committed *before* the URL leaves the process. For missing rows, omit the chain and call `DropStoredParts`. Never buffer stamps in memory. If Postgres is down, stop minting (fail open) and keep GC paused for G after recovery.

**F17 [P1] Q8. `architecture.md:130`, `metrics-and-eviction.md:132`.** Claim: `G ≥ GET TTL + 30 s + skew` protects outstanding URLs. Problem: the `renew` flow (arch:130) never says it updates `last_url_mint_at`; only arch:247 hints at it. A renewal mints a *fresh* 1 h URL. Without a stamp, GC can delete G after the *merge* mint while the renewed URL is still valid. RP §7.2 forbids a second renewal in that demand episode, so the result is a guaranteed miss. Fix: `renew` uses the same conditional stamp as F16 before it replies. That's one indexed UPDATE, which fits the <200 ms target (RP §12.3).

**F18 [P2] `metrics-and-eviction.md:132,165`; `architecture.md:53`.** Claim: the "+30 s stall" term covers in-flight reads. Problem: RP §7.2's 30 s is a **per-stall idle bound, "not a total download timeout"**, and writer backpressure isn't counted. A GET that starts just before expiry can stream for minutes; S3 checks auth only when the request starts. Deleting underneath it causes a short EOF and a fallback. Fix: `G ≥ TTL + max_size/min_throughput + skew`. In practice, just set G = 2×TTL (7200 s instead of 3900 s); the cost is trivial.

**F19 [P2, PLAUSIBLE] `architecture.md:128`.** Claim: a read-only pool is safe because it is "never issuing `export`/`upload`". Problem: fork-PR engines still send OTLP, and that feeds `AttachRemoteHolding` and equivalence facts (content digests, structural inputs) into the *same* pool graph. RP §3.2 says equivalence claims carry trust, so untrusted telemetry could steer donor and offer selection for trusted engines. Fix: ingest RO-engine telemetry into a per-socket shadow that is used only to target offers *to that engine*, never into pool equivalence. Verify what `AttachRemoteHolding` actually admits.

---

## 3. Metrics — **FAIL**

**F20 [P0] `metrics-and-eviction.md:35,43-44`; RP §2.1 outcome row.** Claim: `saved_seconds = Σ span duration where outcome=hit AND hit.remote=true`, and value ratio = saved/(saved+spent). Problem: a hit span's duration measures the lookup, not the compute avoided. And "a hit on a lazy shell is not evidence of byte completion" (RP §2.1). Take a metadata-only remote value: one from F15, or one rejected by admission but "meta-stored" (metrics:117). The engine records `hit`+`hit.remote`, then **recomputes** in a lazy-evaluation span that has no outcome attribute (`attrs.go:738-742`). That work is counted as a hit and never as spent. Both terms of "the number that justifies the project" are wrong. Fix: count saved = Σ over parts *actually installed* (the `cache.offer.part` span, RP §2.1, `attrs.go:772-788`) of (donor recompute cost − install cost). A remote hit whose part was later evaluated locally counts as a miss.

**F21 [P0] `metrics-and-eviction.md:36,100,114`.** Claim: `spent` and `recompute_seconds` are the durations of `outcome=executed` call spans. Problem: Dagger evaluates lazily, so the expensive work happens in lazy-evaluation spans that carry `result.id` but no outcome (`attrs.go:738-742`). The call span of a lazy `withExec` lasts milliseconds. So `spent` undercounts, `recompute_seconds` is ≈0 for exactly the expensive ops, the 60 s cost gate never passes, and every GDSF score collapses. Fix: cost(entry) = Σ duration of lazy-evaluation spans joined on `(cacheID, result.id)`, plus the executed call span's self time. Validate it against job wall-clock in Phase 0.

**F22 [P1] `metrics-and-eviction.md:52`.** Claim: `dcache_offer_install_seconds` histogram from `cache.offer.duration_ns`. Problem: that attribute is a **decimal string of nanoseconds** (`attrs.go:785-788`), and no conversion is specified. Fed raw into second-based buckets, every value lands in `+Inf`. Install cost is also never subtracted from `saved`. Fix: parse as uint64 and divide by 1e9. Define saved_net = recompute − install.

**F23 [P1] `metrics-and-eviction.md:37,45` vs `architecture.md:191`.** Claim: ceiling = (saved + lost)/(saved + spent + lost). Problem: `lost_session_keying` spans are themselves `outcome=executed`, so they are already inside `spent`, and the denominator counts them twice. The definition also compares `pairing_digest` against a prior **`self_digest`**, mixing two different keys. And it isn't a ceiling: it leaves out "executed while an identical recipe was already known" (would-have-hit), which is how arch:191 defines Phase 0. Fix: ceiling = (saved_net + would_hit + lost_keying)/(saved_net + spent), where `would_hit` and `lost_keying` are subsets of `spent`. Match `would_hit` on (`self_digest`, `structural_inputs`). Match `lost_keying` on equal `pairing_digest` with a different `self_digest`. Use a bounded lookback window.

**F24 [P1] `metrics-and-eviction.md:35-36`.** Claim: Σ span durations. Problem: nested spans are counted repeatedly. A module function span contains its child `withExec` spans, and both are `executed`. Fix: use self time (critical-path exclusive), or count only leaf entries that have parts.

**F25 [P1] `metrics-and-eviction.md:33,43`; RP §2.1 `hit.remote` row.** Claim: `hits_remote_total` counts remote hits. Problem: `hit.remote` marks a remote-*origin* entry ("origin survives restart"), so every later local hit on that entry in the same session also carries it. A module that calls the same function 50 times records 50 "remote hits". Fix: count distinct `(cacheID, result.id)` per session.

**F26 [P1] Q10. `metrics-and-eviction.md:32-38,145`.** No metric carries a digest or call-name label (and metrics:184 forbids it, which is good). But **`repo`** appears on seven series, and no RP §2.1 attribute, the `hello` message, or the session identity supplies a repo. The engine is shared across runners. If repo is derived from span names or runner labels, it's either missing or unbounded. The per-repo soft quota (metrics:145) depends on the same missing fact. Fix: drop `repo` from Prometheus. Keep per-repo attribution in the ledger or analytics only if there is a trusted source, such as a runner-scale-set → token → pool mapping.

**F27 [P2] `metrics-and-eviction.md:22,34,96,123,176`.** Claim: the code emits `cache.hit.route` and the miss reasons `saw_expired`, `incompatible_candidates`, `producer_released`, `requirements_grew`. Problem: none of these appear in RP §2.1 or the appendix (`attrs.go:595-625`, `:691-793`). Only `CacheMissUnknownInputAttr` is referenced, in a comment at `attrs.go:703`. The eviction regret loop depends on `saw_expired`. Fix: cite `attrs.go:626-690` or mark these as UNKNOWN, and pin them with a contract test.

**F28 [P2] `metrics-and-eviction.md:21`.** The engine Prometheus series and the `_EXPERIMENTAL_DAGGER_METRICS_*` env names are "verified in `cmd/engine/metrics.go`", but they aren't in RP and can't be checked against ground truth for beta.25. Mark them UNVERIFIED.

**F29 [P2] `metrics-and-eviction.md:38,77`.** `dcache_leader_epoch` is described as a "counter" but is a gauge that holds a value. `dcache_uncached_total` duplicates `lookups_total{outcome="uncached"}`. Also rename `lookups` to `engine_cache_decisions`, so nobody reads it as a dcache-side lookup (there is none, RP §13.2 #3).

---

## 4. Eviction — **FAIL**

**F30 [P0] Q14. `metrics-and-eviction.md:130,134,166` vs `architecture.md:115`.** Claim: the eject sequence moves rows to `state='evicting'`, and "mark-only for 2 weeks" is the safe start. Problem 1: `evicting` isn't in the CHECK constraint (`'pending','verified','deleting','failed'`), so every eject UPDATE fails. Problem 2: even with the constraint fixed, mark-only isn't safe. Rows move `verified→evicting` and **nothing reverts them**: the only revert is at delete time (metrics:134), which mark-only never reaches. Once a blob is marked:
- I4 stops issuing GET URLs for it.
- `renew` sees a non-verified digest, replies `unavailable`, and calls `DropStoredParts` (arch:165), so its parts get unpublished.
- The publish fence (`WHERE state='verified'`) aborts every new part that shares the blob.
- `ON CONFLICT DO NOTHING` (arch:155) never issues a PUT for it again.

Two weeks of mark-only **progressively and permanently disables** the cache for every marked blob while freeing zero bytes. Fix: make mark-only a true dry run: write a `gc_candidate_since` column and a metric, never change state. Use one state name (`deleting`). If you want a reversible mark, define resurrection: publish and mint run `UPDATE SET state='verified' WHERE state='evicting'` until the delete has committed.

**F31 [P0] Q12. `metrics-and-eviction.md:103,143`; `architecture.md:168`.** Claim: the score is `… / max(bytes,1 MiB)`, and the controller evicts by score until usage is ≤0.70. Problem: for a digest shared by 5 parts, each part's `bytes` includes the shared blob. A cheap part whose bytes are all shared with hotter parts scores low, gets evicted, and **frees nothing**. Safety holds, because the blob stays in `LiveBlobs()`. But the controller reads ledger `SUM(size)`, which only drops after an hourly sweep plus G (65 min). For at least ~2 h, used/quota stays above 0.85, so the controller keeps evicting by score on every tick, a **cascading mass eviction**. The watermarks also disagree: arch:168 says 90%, metrics says 0.70/0.85/0.95. Fix: use *marginal unique bytes* (blobs referenced only by this part) in the denominator. Have the controller track pending-free bytes and stop when `used − pending_free ≤ low`. Use one watermark set.

**F32 [P1] `metrics-and-eviction.md:91,135,137`.** Claim: regret = re-upload within 24 h divided by `evicted{reason="value"}`. Problem: step 4 **deletes the ledger row**, so nothing remembers the eviction. The numerator counts re-uploads of quota and orphan evictions (orphans are failed uploads being retried normally), while the denominator counts value evictions only, so the ratio can exceed 1 and blame the wrong policy. For a shared digest, the reason was a per-part decision but it's recorded per digest. Fix: add a tombstone table (`pool, digest, evicted_at, reason, referencing_parts`), compute regret per reason, and exclude orphans. Better still, define regret from `saw_expired` misses and executed spans whose recipe had a pruned stored root (once F27 is verified).

**F33 [P1] Q13. `metrics-and-eviction.md:113-114`.** Claim: admission requires ≥2 sessions within 1 h, or ≥60 s recompute. Problem: a 45 s monorepo build that runs once a day fails both gates (1 run per 24 h, and 45 < 60), so it is **never admitted**. That's exactly the workload the warm node-local engine serves today. With F21, even 40-minute lazy builds measure ≈0 s and fail the cost gate. The frequency gate also keys on `pairing_digest`, which by definition matches per-session-keyed calls that can never hit ("candidates, not proof", `attrs.go:710-718`), so it admits bytes that can't be reached. Fix: key the gate on (`self_digest`, `structural_inputs`), use a window ≥ the maximum job period (7 d; a TinyLFU sketch is cheap), and make the cost gate relative: admit when `recompute > k·(bytes/throughput + n_layers·TTFB)` with k≈3. Also, the CI pattern being filtered is "one-hit wonders", not "two-hit wonders" (metrics:117).

**F34 [P1] `architecture.md:164,225`; `metrics-and-eviction.md:100,121`.** Claim: expiry and `served_count` refresh "whenever a merge or offer uses the root". Problem: pre-merge picks the "hottest live roots", and being merged makes them hotter, so the loop reinforces itself. Roots that are never installed stay hot and immune to eviction. An `accepted` offer counts as a serve even though RP §5.5 says it is "no proof of a download/completion". This is the Q4 failure mode in eviction form. Fix: a served event = a `cache.offer.part` install span, or a remote hit whose part wasn't re-evaluated locally.

**F35 [P2] `metrics-and-eviction.md:121,155-156`.** Claim: a 7 d value TTL refreshed on serve, *and* "no serve within 30 d → prune". Problem: these contradict each other, because an unserved root already expires at 7 d. The design also conflates value-own expiry with retention-edge expiry (RP §7.1 items 1–2). Fix: manage retention with `roots[].expiresAtUnix` (the retention edge) and leave value-own expiry as the engine produced it.

---

## 5. Latency / Azure — **FAIL**

**F36 [P1] `latency-and-azure.md:14,19-20`.** Claim: "PUT is 20× slower than GET TTFB (84 ms p50) because MinIO commits and fsyncs". Problem: the probe's PUT timer covers the entire 4 MiB body upload. `conn.request(body)` runs inside the timed window (`s3_latency_probe.py:33-36`), which is why the table shows PUT TTFB = total = 84.6 ms. That's **throughput (~50 MB/s)**, not latency. The fair comparison is against GET *total* (42.9 ms): about 2×, not 20×, and the fsync explanation is unproven. Consequence: a 2 GB layer upload takes ≈40 s, not 22 s. Add the verify re-read (≈22 s at 93 MB/s, arch:221) and engine diff+compression at export ("compression is the engine's export default", RP §6.3), and publishing a big layer takes more than 60 s. Fix: redo the publication budget as compress + PUT(size/≈50 MB/s) + HEAD (+ verify) + checkpoint, and state plainly that large layers can't publish within 30 s.

**F37 [P1] Q15. `latency-and-azure.md:70`.** Claim: "if p99 GET > 5% of median recompute, the cache stops paying". Problem 1: it compares a *per-request* tail with a *per-part* median. A demanded part is a chain of layers plus reopen GETs, and the transfer term (size/throughput) dominates TTFB by about three orders of magnitude (2 GB: ~22 s vs 7 ms). Problem 2: with ~150 GETs per job, P(at least one request beyond p99) = 1−0.99¹⁵⁰ ≈ 78%. The job-level tail is set by p99.9+ and by the 30 s stall bound (RP §7.2), which a 100-sample probe can't observe. Problem 3: with the probe's `pct()`, "p99" at n=100 is the second-largest sample, and at n≤50 it's the maximum. So the rule isn't statistically meaningful. Fix: build a per-part model, `Σ_layers TTFB + size/throughput + apply + P(renew)·2 s + P(stall)·30 s < recompute`, and feed it into the admission gate (F33). Set thresholds from ≥10k samples under concurrency.

**F38 [P2] `latency-and-azure.md:18`.** "Round-trip latency is negligible" is true for in-cluster GET TTFB. The PUT figure doesn't contradict it, because PUTs are off the build's critical path (RP §4.2: engine work doesn't wait). But the measurement is not production (decision:49 admits this, and F2 applies). Fix: don't take any production threshold from this probe.

**F39 [P1] Q16. `latency-and-azure.md:38,42,47`.** Claim: Azure Blob is unusable because it has "no presigned-PUT semantics". Problem: that's the wrong reason. SAS URLs *are* presigned capabilities. A Blob GET with SAS supports `Range` and returns 206 with a standard `Content-Range`, and an expired SAS returns 403, which triggers renewal. So the **GET side works natively**. The real blocker is PUT: Put Blob requires the `x-ms-blob-type: BlockBlob` header, and the engine adds no headers (RP §2 PUT row). That opens an option the doc never considers: serve GETs directly from Blob SAS (managed, zone-redundant), take PUTs on a small disposable staging endpoint, and after verification have dcache copy server-side (Put Blob From URL).

The strongest counter-argument to MinIO on AKS:
- (a) MinIO's guidance favours locally attached drives. Premium SSD v2 is network block storage with provisioned MB/s caps, and the VM's uncached-disk throughput cap, not the NIC, bounds the 200 GB cold burst.
- (b) AKS node-image and auto-upgrades drain nodes, and zonal managed disks pin pods to a zone. Erasure-set write quorum can be lost during an upgrade unless the PDB and `maxUnavailable` are drilled.
- (c) Supply chain: as far as I know, upstream MinIO stopped publishing community binaries and images in late 2025 and moved the community repo to maintenance mode. **Verify this.** A primary store you can't patch is the real anti-pattern.
- (d) The cache fails open (I7), so durability buys little. If you run MinIO at all, run it as disposable single-replica on local NVMe (e.g. Lsv3) and drop the erasure-coding overhead (arch:204).

What would change my mind: a drill showing MinIO keeps write quorum through an AKS node-pool upgrade during a 200 GB burst; a maintained, patchable MinIO build or licence; or a test showing a Blob S3 gateway accepts a header-less raw PUT and returns an exact 206.

**F40 [P2] `latency-and-azure.md:49`.** Claim: "D4s_v5 ≈2 Gbps → 800 s; use D8s_v5". Problem: Azure lists the small Dsv5 sizes at up to 12.5 Gbps (check the size page), and D8s_v5 has the same NIC ceiling, so the proposed upsize targets the wrong limit. The binding limits are VM uncached-disk MB/s and Premium SSD v2 provisioned throughput. Fix: size the MinIO tier on disk throughput.

**F41 [P2] `latency-and-azure.md:56`; `architecture.md:327`.** Claim: AKS spot gives "~30 s notice (SIGTERM then SIGKILL)", and R6 says "on SIGTERM the service stops scheduling new exports". Problem: AKS doesn't turn a Scheduled Events preempt into a pod drain unless a termination handler is installed (verify). Independently, the WS protocol carries no SIGTERM signal to dcache, so R6's step can't happen. Fix: drop that step, or add a `preStop` hook that calls dcache.

**F42 [P2] `latency-and-azure.md:42`.** Claim: "presigned PUT/GET: yes (we just tested SigV4 against it)". Problem: the probe signs with headers (`s3_latency_probe.py:11-27`). It never generated a presigned query-string URL, never sent UNSIGNED-PAYLOAD, never used the engine's open-ended Range, and never checked `Content-Range`. Fix: see F45.

---

## 6. Code: `s3_latency_probe.py` — **FAIL**

**F43 [P1] `s3_latency_probe.py:83-87,37` — the HEAD crash.** `read_body=False` leaves the `HTTPResponse` unread. For HEAD, `http.client` sets `length=0`, but the response object isn't *closed* until `read()` or `close()`, so `HTTPConnection` keeps it as its current response. On the **second** HEAD, `request()` is allowed (the connection state is idle and the old response isn't closed), but `getresponse()` raises `ResponseNotReady('Request-sent')` because a previous response is still attached. MinIO isn't involved; it's the client's state machine. The uncaught exception ends the script before `report()`, so no HEAD numbers were collected. Fix: always call `r.read()` (it returns `b""` for HEAD) or `r.close()`.

**F44 [P1] `s3_latency_probe.py:33-36,55`.** PUT "TTFB" includes the body upload, because `request()` writes the full body before `getresponse()`. It's reported as latency. Fix: send the headers and body separately and time from end-of-body to response, or report PUT as MB/s.

**F45 [P1] `s3_latency_probe.py:11-27,48,70-71`.** The probe doesn't test what the engine actually does:
- It uses header SigV4 with a hashed payload, not presigned query URLs.
- It sends bounded `bytes=a-b` ranges, while the engine always sends `bytes=<off>-<size-1>` (RP §2).
- It checks only the 206 status, not the exact `Content-Range` string or the body length.
- It uses plain HTTP (`HTTPConnection`), while production is HTTPS, so TLS handshake and crypto costs are missing.

Fix: generate presigned URLs (`X-Amz-Algorithm`, `X-Amz-Expires`, UNSIGNED-PAYLOAD). PUT with only `Content-Length`. GET with `Range: bytes=off-(size-1)` and assert `Content-Range == f"bytes {off}-{size-1}/{size}"` and the length. Test that an expired URL returns 403, and test the 200 fallback. Use HTTPS.

**F46 [P2] `s3_latency_probe.py:41-43`.** Nearest-rank percentiles on tiny samples. At n=30, p99 is the max and p95 is the second-largest value; at n=5 (cold connect), p95 = p99 = max (the table at latency:13 shows 11.6/11.6). Fix: use n≥1000 for p99, or label the value "max".

**F47 [P2] `s3_latency_probe.py:61`.** All 100 GETs read one 4 MiB object, so it's page-cache hot, while production reads distinct, cold blobs. The test is single-connection and sequential with no concurrency, so "93 MB/s per conn" (actually 93.2 MiB/s, or 97.8 MB/s) predicts neither 8 concurrent PUTs nor a 100-engine burst, and the Python read loop may be the limit. Fix: test N distinct cold objects at concurrency 8 and 64, plus at least one object ≥1 GB.

**F48 [P2] `s3_latency_probe.py:49,76-79`.** The bucket-create status is printed but not checked. The cold-connect loop checks neither status nor length, so a 403 caused by clock skew would be recorded as a fast GET. Fix: assert both.

**F49 [P2] `latency-and-azure.md:76-78`.** Passing credentials with `--env` on `kubectl run` exposes them in the pod spec, audit logs and shell history. Fix: use a Secret with `envFrom`.

**F50 [P2] `s3_latency_probe.py:6`.** The region is hard-coded to `us-east-1`, which is fine for MinIO and wrong for external S3 anywhere else.

---

## 7. Live manifests (Q18) — **FAIL** (not ready for beta.25 ephemeral)

**F51 [P0] `live-helmrelease.yaml:80`.** `limits.memory: 4Gi`. #14884 (RP §13.1) reports fresh remote-cache engines peaking at ~6.0–6.1 GB *after* the fix on a large module, and arch §4.1 sets no resources at all. A fresh engine receiving a warm merge is exactly that case, so it gets OOMKilled mid-job, which is a failed build, not fail-open. Fix: measure on beta.25. Start the ephemeral DaemonSet at ≥8Gi, and size merges from engine memory.

**F52 [P1] `live-helmrelease.yaml:49-54` vs `architecture.md:69-70`.** Arch's `configJson` replaces the whole document with `{"remoteCache":…}`. That drops `development.example.test.insecure`, so Zot pulls fail, and Zot is the Tier 2 dependency. Fix: merge the keys into one document: `{"registries":{…},"remoteCache":{"url":…}}`.

**F53 [P1] `live-helmrelease.yaml:43-47` (comment) and `architecture.md:70,75`.** WSS and OTLP to `https://dcache.dcache.svc:8443` require the engine to trust dcache's certificate. The live setup fell back to `insecure` for the internal CA precisely because trust isn't plumbed through, and RP §11.1 lists no insecure option for the remote-cache or Cloud URL. Fix: mount the viporlab-internal CA into the engine (CA bundle volume or `SSL_CERT_FILE`), issue a certificate with SAN `dcache.dcache.svc`, and cover it in Phase 2 tests.

**F54 [P1] `live-helmrelease.yaml:71-76`; `architecture.md:76-77`.** `hostPath.dataVolume` and `runVolume` must be turned off. Arch's `engine.volumes` emptyDir isn't a chart field established by RP §11.2 (which names only `env` and `configJson`). If the chart mounts nothing at the state dir once hostPath is disabled, the engine state ends up on the container rootfs: overlay-on-overlay snapshotting plus ephemeral-storage eviction. Fix: check the beta.25 chart template, or post-render an NVMe-backed emptyDir at the state path with an `ephemeral-storage` request.

**F55 [P1] `live-helmrelease.yaml:3-14,27-39`.** The chart is pinned to 0.21.9 and the release is named `dagger-engine`. Phase 3 needs a **separate** HelmRelease with a beta.25 chart and an image pinned by digest; don't upgrade in place. The post-renderer patches the Service *by this release's name* (`dagger-dagger-engine-dagger-helm`), so a second release's Service stays `Cluster`. And with `Local` on ephemeral nodes, a runner that starts before its node's engine is Ready gets connection refused, because `Local` has no fallback. Fix: add a per-release patch and gate runner readiness on the node-local engine (e.g. an initContainer that waits for it).

**F56 [P1] `live-helmrelease.yaml:63-70`.** The required `node-purpose=worker` affinity pins engines to the 3 persistent nodes. Fix: give the ephemeral DaemonSet the spot pool label and toleration (arch:79), and match the runner scale-set affinity to it.

**F57 [P1] `live-helmrelease.yaml:87-100`.** Add `DAGGER_CLOUD_TOKEN` (Secret in the `dagger` namespace) and `DAGGER_CLOUD_URL`. Two risks to check:
- The Cloud exporter is "built by official OTel exporter" (RP §2), so the existing `OTEL_EXPORTER_OTLP_*` env vars may redirect or duplicate exports between Tempo and dcache.
- `OTEL_RESOURCE_ATTRIBUTES` overrides `service.instance.id` with the node name, and RP §2.1 names that as the process resource for engine events. Spot VMSS node names get reused, so that identity collides.

Fix: spike with both exporters enabled, and key process identity only on `dagger.io/engine.cache`.

**F58 [P1] (absent) `live-helmrelease.yaml`, `live-networkpolicy.yaml:10-23`, `metrics-and-eviction.md:180`.** There is no metrics env, no metrics port and no PodMonitor. The NetworkPolicy allows ingress only on port 1234 from `flux-system`, so a Prometheus scrape would be dropped. Fix: add `_EXPERIMENTAL_DAGGER_METRICS_ADDR` (verify the name on beta.25), a containerPort, a PodMonitor, and an ingress rule from the monitoring namespace.

**F59 [P1] `live-networkpolicy.yaml:10-11`; `decision.md:87`; `architecture.md:83`.** Claim: "NetworkPolicy deny `api.dagger.cloud`". Problem: Kubernetes NetworkPolicy is an IP/CIDR allow-list. It has no FQDN matching and no deny rules. The live policy doesn't have `Egress` in `policyTypes` at all, and the default k3s controller can't do FQDN rules. A default-deny egress policy would break builds, because engines fetch from arbitrary hosts. Fix: enforce at DNS (a CoreDNS override that returns NXDOMAIN for `api.dagger.cloud`), through an egress proxy, or with Cilium FQDN policy. Verify with a canary.

**F60 [P2] `live-networkpolicy.yaml:14-20`.** The comment says "namespace where ARC runner pods run", but the selector is `flux-system`. Either the runners run in `flux-system`, which means Flux controllers can reach an unauthenticated engine, or the policy isn't enforced and the engine is open to the whole cluster. Fix: confirm the runner namespace and correct the selector.

**F61 [P2] `live-helmrelease.yaml` (no `terminationGracePeriodSeconds`).** The default is 30 s. The ephemeral DaemonSet's 25 s (arch:78) is fine because nothing local is worth saving. For week-version (A), set it above the engine stop budget, as decision:73 says; that budget is still UNKNOWN (#14956).

---

## Top 5 changes before any code

1. **Settle the facts the design depends on (F1, F2, F62).** Get the node lifetime distribution, separately from the notice window. Pick one production byte plane. Then redo the cost model, including checkpoint bytes and verify egress.
2. **Run three one-day spikes on beta.25.**
   - (a) Do call spans reach a self-hosted `DAGGER_CLOUD_URL` without client Cloud config, and how fast (F5)?
   - (b) How long does the end-to-end publication pipeline take for 30 MB, 500 MB and 2 GB layers (diff+compress+PUT+HEAD) (F36)?
   - (c) What is fresh-engine peak RSS after a 48 MiB merge (F51)?

   In parallel, run the Phase 0 query against the **existing Tempo traces** (F4).
3. **Rewrite the ledger state machine as one spec (F12, F13, F16, F17, F30).**
   - One state enum.
   - Reserve/takeover upsert, with PUT TTL ≤ lease.
   - Publish fence with `rowcount == |chain|`.
   - Minting and renewal as conditional stamps committed before send.
   - No buffered stamps.
   - Mark-only as a dry run.
   - Restore-time revalidation (F14).
   - G = 2×TTL.
4. **Redefine the go/no-go metric (F20–F25).** saved = installed parts × donor lazy-inclusive recompute − install time. Count distinct installs, not hit spans. Fix the ceiling double count. Use self-time, not nested sums. Drop the `repo` label unless it has a trusted source. And compare against the **warm node-local baseline** the fleet has today, not against a cold engine.
5. **Fix scheduling so failures don't stick (F6, F7, F15, F33).** Send `upload` before `CommitValueMerge`. Dedupe on part availability, not value existence. Enforce the 64 MiB limit on the final frame. Use an admission window ≥ the job period with a relative cost gate.

## What I would cut from the design

- **Streaming sha256 verification.** Keep the HEAD size check plus the capability association (the PUT URL was issued for that key and export), and sample 1%. The engine already verifies digest and size on import (RP §7.2, I3), and the pool is mutually trusted (I8). RP §8.2 asks for "size and digest/authorized upload association". Cutting this saves ~600 GB/day of re-reads (arch:200) and ~20 s per 2 GB of exposure window.
- **Live offers (arch §9 Hit b) in v1.** They rarely win the race against OTLP latency (F9). Ship pre-merge only and measure first.
- **The "warm standby tailing checkpoints" and the 60 s full-DB checkpoint (F62).** Replace them with infrequent checkpoints and a cold restore. Fail-open makes the outage acceptable.
- **GDSF scoring and the frequency/cost admission gates in v1.** Their inputs (`recompute_seconds`, `saw_expired`) are either unmeasured (F21) or unverified (F27). Start with LRU by last *install* plus a quota with marginal-byte accounting.
- **The 5 s upload watchdog (F6), `If-None-Match` on checkpoints** (the Postgres CAS plus epoch-namespaced keys already fence them), **and the multipart lifecycle rule (F11).**
- **The 5% rule (F37), and the Azure Blob-via-S3-gateway row.** Replace the gateway row with the GET-from-SAS / PUT-to-staging split (F39) if Blob is mandated.
- **Two dcache replicas in the design cluster.** Keep one leader until failover has been measured.

## Verified-correct list (checked against RP; credit due)

- No invented REST routes. The control plane is WS message types and blob addresses are opaque (arch:4 matches RP §2, line 36).
- Token goes in Basic username with an empty password; remote cache needs both URL and token; `DAGGER_CLOUD_URL` is separate; `magicache` is irrelevant (arch:72-80 matches RP §3.1, §11.1, §11.2).
- Quarantine instead of rejecting a version mismatch, because rejection causes an endless reconnect and 4001 isn't terminal (I9 matches RP §3.1).
- 4 export slots; always send `upload`, even `{}`; one upload consumes the exportID; retry means re-export (arch:150,220 matches RP §5.2–5.3).
- Single raw PUT, 8 concurrent, 1-minute idle bound, no multipart or retry; a 2xx is not durability (arch:146, I2 match RP §5.3).
- `SetStoredPart` verifies nothing, so the publication gate has to live in dcache (I1 matches RP §8.2).
- No torn reads: S3 single-PUT atomicity plus the engine's import digest check (I3 matches RP §7.2).
- `accepted` means wait for completion evidence; `executionStarted` means the offer wasn't admitted (arch:226 matches RP §5.5).
- Renewal triggers only on 401/403/404/410; 2 s deadline; one renewal per episode; an expired S3 presign returns 403 (arch:96,132 match RP §5.6, §7.2).
- `DropStoredParts` on a missing blob; `LiveBlobs` doesn't see pending uploads, so reservations are needed; no bucket TTL on blobs (arch:142,165-166 match RP §8.4, §12.4).
- One fenced writer per pool; the wire has no fencing; build in Go with pinned dagql and the codecs (I6, arch:101,272 match RP §6.4, §10.5).
- No on-miss lookup and no cross-engine execution singleflight; all 100 engines execute (arch:158,275 match RP §13.2 #3).
- Cache volumes aren't in the protocol, so Tier 2 is genuinely required (arch:13 matches RP §13.2 #10).
- Mutual trust within a pool; `hello.cacheID` is never used for tenancy (I8, arch:128 match RP §3.1–3.2).
- `pairing_digest` semantics are cited correctly (arch:191 matches `attrs.go:710-718`).
- The 32-roots limit is correctly labelled as #14761 service policy, not a protocol constant (arch:94 matches RP §13.1).
- Arithmetic checked: request cost ≈ $0.27/day ≈ $8/month; 200 GB at 1.25 GB/s ≈ 160 s; 4 MiB/42.9 ms ≈ 93 MiB/s.
- The probe's SigV4 header-auth canonical request is correct: the header block is followed by a blank line, and the Host header including the port matches what is sent.
- Week-version (A) is sound and is the right default. The engine-side fail-open mapping (I7) matches RP §4.2 and §7.2.
