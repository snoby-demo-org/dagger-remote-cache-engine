> **Historical design/review record, not current implementation status.** See [current design](../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Revision R2 — change ledger

Date 2026-11-07. Round 2 was reviewed by a second model (`review/gpt-review.md`, verdict STOP, 19 findings: 4 P0, 13 P1, 2 P2) against the R1 docs. Every finding was checked by me before acting; **all 19 hold, none rejected**. This file maps each to what changed.

## Round-2 dispositions

| # | Sev | Finding (short) | Where fixed |
|---|---|---|---|
| 1 | P0 | Discovery still unproven; lifecycle fallback cannot seed fresh work | `decision.md` Phase 0 gate rewritten as a wall-time measurement; F5 remains an explicit **spike**, not a claim. Not "closed"; scope-controlled. |
| 2 | P0 | Publication SQL rejects reused blobs and mixed-owner chains | `ledger.md` §4: **two fences** — owner-fenced per-blob verify, then all-distinct-digests-verified publish. `metrics-and-eviction.md` no longer carries SQL. |
| 3 | P0 | Hit/saved model not computable (no donor cost, mixed units, missing population) | `metrics-and-eviction.md` §1–§2: decision table with 7 disjoint classes (incl. `deferred_recompute`), exclusive-time attribution, `cost_estimate(self_digest)` with labelled basis and an unknown bucket. |
| 4 | P0 | Publication gap + delayed-delete ABA | `ledger.md`: durable `publish_pin_until` across `SetStoredPart`; **generation-scoped object keys** so a stale DELETE can only hit its own generation. |
| 5 | P1 | NULL mint timestamps ⇒ immortal verified blobs | `last_url_mint_at NOT NULL DEFAULT now()`, initialized at reserve; `ledger.md` §1/§9 invariant 7. |
| 6 | P1 | Reserve/failed/delete recovery not executable | `ledger.md` §2 transition table, §3 reserve, §7 delete + recovery scanner, `failed` purge rule. |
| 7 | P1 | Renew "no PG round trip" vs mandatory durable mint | `ledger.md` §6: the stamp is one statement, allowed on the renew path; the in-memory index removes the **graph walk**, not the stamp; 2 s is a deadline, not zero-I/O. |
| 8 | P1 | 1 % sampling contradicts "re-hashed every blob" | `architecture.md` §5.1 defines **one** policy; I1, overview, cold flow and the wrong-bytes row all restated; `decision.md` invariant 1 aligned. |
| 9 | P1 | "2 GB cannot publish in 30 s" false; arithmetic conflated | `latency-and-azure.md` §1: 12.85–27.3 s PUT, ≈13–28 s sampled fast path, 34–50 s full rehash; donor-dependent vs service-dependent split; spike stays open. |
| 10 | P1 | Admission not calibrated; PUT rates used as read basis | `metrics-and-eviction.md` §3.3: assumptions labelled (`k=3`), read throughput explicitly unmeasured, frequency role and pre-upload ordering defined. |
| 11 | P1 | Marginal-byte scoring protects cold sharers, evicts hot singleton | §3.2 reclaim-group scoring with worked counterexample; `architecture.md` §6.3 updated. |
| 12 | P1 | Merge-refreshed retention defeats install-only popularity | §3.1: pool demand clock = `last_install_at`; merge extends recipient retention only; idle prune is a timestamp, not a boolean. |
| 13 | P1 | Ceiling can exceed 100 %; legitimate changes blamed on keying | §1.2 single-valued classes make `would_hit`/`lost_keying` disjoint; `lost_keying` labelled a candidate with explicit preconditions. |
| 14 | P1 | Metadata-only value suppresses re-export | `architecture.md` §4.2 scheduler keys on **part availability**, not value existence. |
| 15 | P1 | Upload continuation waits behind the graph commit | `architecture.md` §9 cold flow: enumerate → reserve → send continuation → commit; commit never gates the 5 s deadline. |
| 16 | P1 | Production topology accepted/rejected/replaced in different sections | `architecture.md` §8/§12 prohibitions replaced; `latency-and-azure.md` §3 conforms to external S3; "only stateful service" and dangling §7 removed. |
| 17 | P1 | Breaker cannot see engine-path latency | `metrics-and-eviction.md` §2.3: store-side TTFB exporter + install-failure ratio + liveness probe, three separate signals. |
| 18 | P2 | Probe assertions/units/percentile disagree with code | `s3_latency_probe.py` v3: all statuses/sizes/ranges/expiry/8-writers asserted, MiB/s and MB/s both, p99 order statistic documented. |
| 19 | P2 | Checkpoint retention/cost still on the old schedule | `architecture.md`: ckpt age expiry removed (pointer, last 5), cost row 144/day per pool, diagram says cold restore; alert is a gauge, not `rate(sum)`. |

## New normative documents

- **`ledger.md`** — the blob state machine: tables, transitions, reserve, verify/publish, waiter path, mint, GC mark/delete, recovery scanner, restore, invariants, and what is still unproven. It is the **only** place the SQL lives.
- `metrics-and-eviction.md` — rewritten (event model, cost estimate, reclaim groups, disjoint opportunity sets, engine-path measurement).
- `latency-and-azure.md` — rewritten (corrected arithmetic, topology conformity, measurement points, v3 probe usage).
- `architecture.md` — patched in place at 14 sites; its §3 invariants now defer to `ledger.md`.
- `s3_latency_probe.py` — v3 (assertions); v2 kept as `s3_latency_probe_v2.py`.

## Still open — unchanged and honest

1. **F5 discovery spike** — does a call span reach a self-hosted `DAGGER_CLOUD_URL`? (Tempo query, ~1 h.)
2. **F21 contract check** — does `cache.offer.part` fire per install on a real beta.25 engine, and do lazy spans carry `cache.result.id`?
3. **Publication spike** — 30 MB / 500 MB / 2 GB, span end → published.
4. **Memory spike** — fresh engine RSS after a 48 MiB merge; then `res limits` (proposed 12 GiB is a proposal).
5. **Production read burst** — 100 × 2 GB against the real external endpoint; read throughput is the missing term in admission and capacity.
6. **Store capabilities** — MinIO `If-None-Match: *`, exact 206, DELETE of a missing key, single-PUT cap; Azure PUT shape if Blob is ever mandated.
7. **MinIO supply chain** — confirm community images are still published before making it a primary store.

## Loop policy

Round 3 is run by **both** reviewers in parallel (Claude Opus via CLI, ChatGPT model via delegation), each briefed to verify closure of the round-2 findings and to attack `ledger.md` specifically. The loop ends when both return no P0/P1 findings, or when a round produces no new P1+ against a doc that changed. P2 nits are fixed in place. Max four rounds; if it does not converge, we report that instead of claiming success.
