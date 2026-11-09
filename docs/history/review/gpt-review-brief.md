> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Adversarial review brief — Revision R1 of the Dagger shared-cache design

You are reviewing a *design document set*, not code. There is no implementation yet; that is deliberate. Your job is to find what is still wrong, contradictory, or unprovable before anyone writes service code.

## What this is

A shared, S3-style cache for Dagger CI on Azure spot nodes that can be destroyed with ~30 s notice. Constraints:
- Design cluster = homelab k3s, persistent nodes, in-cluster MinIO at 192.0.2.10:9000.
- Production = Azure, S3 mounted **outside** the cluster, compute nodes ephemeral.
- Must expose Prometheus metrics with real hit/miss rates and must safely evict unused/expired entries.
- Dagger v1.0.0-beta.25 (commit 7e9eaac) remote-cache protocol: the engine opens a WebSocket to /v1/engine, the *service* decides exports and hands out presigned PUT URLs; whole-object single PUTs, no multipart, no engine retry; reads are Range GETs requiring exact 206 Content-Range; renewal must reply within 2 s; no inventory message and no on-miss lookup (discovery requires OTLP) .

## Round 1

An independent Opus review of the first draft returned STOP with 63 findings (9 P0). It is in `review/claude-review.md` — read it. Two of its findings were wrong (F27, F28) and are rejected with evidence in `revision-r1.md`.

## Round 2 — what you are reviewing

- `revision-r1.md` — the change ledger, with finding→change mapping, normative rules, rejected findings, and open items.
- `architecture.md` — patched in place (invariants, component table, ledger SQL, GC/TTL, failure modes, red team).
- `metrics-and-eviction.md` — rewritten (metric redefinitions, executable SQL for the eviction fence, admission, regret loop).
- `latency-and-azure.md` — rewritten from a *fresh, corrected measurement* of in-cluster MinIO with presigned URLs.
- `decision.md` — patched (topology, publication gate, GC fence, phase 0).
- `review/r1.diff` — unified diff of everything against `review/baseline/`.
- `review/baseline/` — the pre-fix originals for comparison.
- `s3_latency_probe.py` — the probe that produced the numbers (stdlib SigV4, presigned).

## Measured facts you may rely on (all from this cluster, presigned URLs, n=200)

GET 4 MiB TTFB p50 3.96 ms / p95 6.13 / p99 8.03; GET total p50 43.1 ms; open-ended range TTFB p50 4.10 ms; HEAD p50 1.92 ms; PUT 4 MiB body 27.0 ms (155.6 MB/s) + 18.8 ms response wait; 8 concurrent PUTs aggregate 70.1 MB/s; an expired capability returns 403.

## Exactly what to answer

1. **Did the P0s actually close?** For each of F5, F12, F20/F21 (metric), F30, F51, F36: cite the exact clause that closes it, or state it is still open. Do not accept the ledger's word for it — check the document text.
2. **New contradictions.** Anything the patches introduced (e.g. one doc still asserts what another now denies). Give file and line.
3. **Metric computability.** Are the redefined metrics actually derivable from the telemetry attributes as described (`cache.offer.part`, `cache.offer.duration_ns`, lazy spans with `cache.result.id`, `cache.structural_inputs`)? Name any attribute that is assumed but not grounded.
4. **Eviction SQL.** Is the reserve/publish/mint/delete sequence race-free against (a) two concurrent uploads of the same digest, (b) a publish racing a delete, (c) a leader failover between mint and delete? Name the specific interleaving that breaks it if one exists.
5. **Latency logic.** Given the measured numbers, is the per-part cost model and the admission factor defensible? Is the claim "2 GB layer cannot publish inside 30 s" right?
6. **Eviction policy soundness.** Does marginal-bytes scoring plus install-only serving actually prevent extinction of hot-but-shared entries, or does it introduce a new pathology?
7. **What would you cut first?** Rank the top three things that are over-engineered for a cache that fails open.

## Output rules

- Write the full findings to `<DESIGN_WORKSPACE>/review/gpt-review.md`.
- Verdict must be one of PROCEED / PROCEED-WITH-CHANGES / STOP, first line of the file.
- Number findings sequentially, mark P0/P1/P2, and give file+line references.
- Do not quote large blocks of source or docs; reference locations.
- Keep any chat reply under 200 words and do not repeat the findings inline.
