> **Historical design/review record, not current implementation status.** See [current design](../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Revision R3 — change ledger

Date 2026-11-07. Round 3 was reviewed by both models in parallel against R2:
- Claude Opus: `review/claude-r3.md` — STOP, 26 findings (1 P0 / 13 P1 / 12 P2).
- ChatGPT model: `review/gpt-r3.md` — STOP, 22 findings (2 P0 / 18 P1 / 2 P2).

I verified each P0 against my own text before acting. Every finding holds; none rejected.

## Round-3 dispositions (union; # = GPT numbering where distinct)

| # | Sev | Finding | Fix |
|---|---|---|---|
| 1 | P0 | Reserve does not branch on head state; head mirror never advances | `ledger.md` §3: head created + `FOR UPDATE`, allocation `INSERT … SELECT … WHERE h.state IN ('absent','failed','deleted')`; verified ⇒ reuse, pending ⇒ wait, deleting ⇒ skip. §2: every transition updates `blob_head.state` in the same transaction, rowcount 1 or rollback. |
| 2 | P0 | Expiring pin does not revoke a paused publisher | Pin **deleted**, not patched: §5 publishes by holding the head row locks across `SetStoredPart`, committing immediately after. GC takes the same locks in the same (digest-sorted) order. A dead publisher's transaction rolls back — no reference, no part. |
| 3 | P1 | No durable part→generation binding; generation-blind `DropStoredParts` | `ledger.md` §0: `blob.graph_refs`, and a new generation is legal only when the head is `deleted` **and** `graph_refs=0`. Only one generation can ever hold graph parts, so digest-granular drop is unambiguous. `dcache_graph_refs` metric added. |
| 4 | P1 | Mint join not correlated to pool/digest; returns digest only | §7 joins `h.pool`, `h.digest`, `h.gen` and returns `(digest,gen)`; URL built from the returned generation. |
| 5 | P1 | Failed uploads lose evidence; object never reclaimed | §3.2 explicit `pending→failed` for both upload-failure and lease-expiry; §9 deletes `g<gen>` before the row becomes a `deleted` tombstone; late PUT completion reconciled by the verifier or the recovery scanner. |
| 6 | P1 | Waiter path sends an impossible second upload, wrong deadline | §6: exactly one continuation ever; internal retry ≤4 s (service budget, not the engine's pre-continuation watchdog); on timeout abandon, leave unpublished, schedule a later donor; owner death handled by lease expiry. |
| 7 | P1 | Seven classes are labels, not populations; mixed entries lose work | `metrics-and-eviction.md` §1.1–1.2: revision keying, session finalization, `incomplete` handling; install and recompute kept as independent measures on every entry; `local_hit` requires **absence of lazy spans**, not zero call overhead. |
| 8 | P1 | "Exclusive" self-time attribution returns 10 s for a 100 s interval | §1.3: **interval union** of every span whose causal parent reaches the entry (`WcprofParentAttr` fallback), minus install intervals; overlap/nesting handled by construction; `attribution_uncertain` flagged. |
| 9 | P1 | Saved/value omits ordinary install cost | §2: gross avoided, `saved_net = Σmax(C−I,0)`, `lost_net = Σmax(I−C,0)`, remaining recompute; value ratio = net/(net+lost+remaining). C comes from `donor_recompute_seconds` recorded at publish — no telemetry-time join, no starvation. |
| 10 | P1 | Breaker input has no computable failure population | §2.3: v1 breaker = `dcache_store_errors_total` (provider 5xx/timeouts) + `install_p95` + `demand_p95`; the absence of a per-download failure attribute is stated as a v1 limitation instead of invented. |
| 11 | P1 | Reclaim groups evicted in reverse value order | §3.2: **lowest** score evicted first; bounded candidate generation (≤1000, union-find over shared blobs); unknown cost excluded from score ordering; overshoot capped; split-large-part rule. |
| 12 | P1 | Quota charges deleted generations; no durable projected-free accounting | §3.3: charged = `pending|verified|deleting`; deleted/failed tombstones excluded; `pending_free` table survives leader restart; controller stops at `charged − projected ≤ low`; nothing claims immediate release. |
| 13 | P1 | Renew still forbids and requires a Postgres round trip | `architecture.md` §4.2 signer row rewritten: p99 < 200 ms **including one committed stamp**, SLO target not measurement; index removes the graph walk only. |
| 14 | P1 | Value-existence scheduler rule survives | `architecture.md` §5.1 dedupe level 1 rewritten: skip only on usable part (verified at head) or live pending reservation. |
| 15 | P1 | Retention still refreshes on merge | `architecture.md` §6 rewritten: pool retention refreshes on install only; merge extends recipient retention; idle prune uses `last_install_at` for every root. |
| 16 | P1 | Unscoped MinIO dependencies and production-MinIO sizing | `architecture.md` §1/§4.3/§8/§12/red team rescoped: design-cluster MinIO vs production external S3; R5 mitigation no longer sizes MinIO for production; checkpoint location is "the object store". |
| 17 | P1 | I3 contradicts sampling; no legal corruption transition | `architecture.md` I3 rewritten as "atomic complete object, content checked by 1 % sample + engine import hash"; `ledger.md` adds `verified→quarantined→deleting` as an immediate fenced path; cost table re-read reduced to the 1 % expectation. |
| 18 | P1 | Phase-0 gate is circular (needs the service it gates) | `decision.md` + `metrics-and-eviction.md` §2.4: **Gate A** pre-build (discovery capture + telemetry critical-path opportunity), **Gate B** post-prototype (paired wall-time vs warm local baseline). 30 % threshold withdrawn. |
| 19 | P1 | Probe v3 records request-send time as TTFB | `s3_latency_probe.py`: TTFB taken after `getresponse()`. **Re-run in the cluster** and the latency doc rewritten from that run (GET p50 4.27 ms, HEAD 2.18 ms, PUT 109.5 MB/s, 8× aggregate 62.7 MB/s, 403 asserted, all assertions passed). |
| 20 | P2 | Publication table mixes precision, overstates interval | `latency-and-azure.md` §1 re-derived from the new run: 18.3 s / 31.9 s PUT, +20.6 s reread ⇒ 38.9–52.5 s full path, ±0.1 s; unmeasured overhead outside the interval; aggregate caveat explicit. |
| 21 | P2 | Admission Boolean rule unspecified | §3.5: `admit = pinned OR (freq≥2 AND cost_known AND C ≥ 3·serve_cost)` — **AND** between frequency and cost; unknown cost ⇒ not admitted unless pinned; unknown read throughput ⇒ frequency-only with a dashboard warning. |
| 22 | P1 | Ratio bounds conditional; PromQL label matching wrong | §2.5: every ratio `sum by (pool)` both sides with `clamp_min`; regret defined as a closed cohort of unique evicted items refetched within 24 h; ratios NaN/no-data at zero denominator. |
| 23 | P1 | Discovery fallback cannot name fresh roots | `architecture.md` §9/red team: `engine.share` = imported rows, `engine.stop` = no entry numbers; the fallback is explicitly incapable, and Gate A must fail loudly rather than degrade silently. |
| 24 | P1 | Checkpoint cost/lifecycle vs new schedule | R2 item re-checked: pointer retention (last 5), no age expiry, cost row 144/day/pool; request total re-derived at ≈$0.24/day ≈ $7/month. |

Also applied from Claude's P2 list: cost-table re-read corrected, the diagram's verify label, the manifest index wording, and the `blob_ledger` name.

## Evidence produced this round

- **Fresh probe run** (v3, corrected TTFB): all assertions passed — GET 4 MiB TTFB p50 4.27 / p95 5.17 / p99 8.90 ms; range p50 4.44 / p99 6.43; HEAD p50 2.18 / p99 8.89; PUT body 38.3 ms (109.5 MB/s) + 19.2 ms ack; 8× concurrent PUT 62.7 MB/s aggregate; expired capability 403.
- The previous v2 numbers (3.96/4.10/1.92 ms) were measured with the old timing definition and are superseded, not "credited to v3".

## What the reviewers conceded

Both round-3 reviews independently confirmed as **holding**: the all-distinct-verified publish predicate, NULL-mint immortality fix, generation-scoped keys defeating delayed-DELETE ABA, delete-recovery idempotency, and the disjoint opportunity sets. GPT could not break attacks (c) and (d) at the object layer; Claude could not break (a) at the index layer. Those are real closures.

## Still open — unchanged

1. F5 discovery capture / Gate A.
2. F21 install-attribute contract check on beta.25.
3. Publication spike (30 MB / 500 MB / 2 GB, span end → published).
4. Fresh-engine memory spike, then memory limits (12 GiB proposal).
5. Production read burst (100 × 2 GB) — the missing `read_throughput` term.
6. Store capabilities: `If-None-Match: *`, exact 206, DELETE of missing key, single-PUT cap; Azure PUT shape if Blob is mandated.
7. MinIO supply chain.
8. Row-lock hold time during `SetStoredPart`, and deadlock-freedom of digest-sorted head locking — new, introduced by the R3 publish design.

## Loop policy

Round 4 runs both reviewers again over the R3 artifact. The loop stops when **both** return no P0/P1, or when a round produces no new P1+ against changed text. P2s are fixed in place. Four-round cap; if it does not converge I will say so rather than call it clean.
