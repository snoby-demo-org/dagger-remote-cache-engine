> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Latency and publication budget — R4

Supersedes R3. Round-4 findings 12, 13, 14 applied; measured on a fresh run (`s3probe4`, 2026-11-07) with **end-to-end** PUT timing.

## 0. Measurement basis

Probe v3 (`s3_latency_probe.py`), pod in the design cluster, `minio.minio.svc.cluster.local:9000`, presigned query-string URLs (what the engine receives), keep-alive, credentials from a Secret via `envFrom`. All assertions passed.

| Operation | n | p50 | p95 | p99 |
|---|---:|---:|---:|---:|
| GET 4 MiB TTFB | 200 | **4.48 ms** | 5.91 | 12.51 |
| GET 4 MiB total | 200 | 43.4 ms (92.2 MiB/s) | — | — |
| GET open-ended range TTFB | 100 | **3.50 ms** | 4.84 | 9.21 |
| HEAD | 50 | **2.10 ms** | 2.27 | 7.56 |
| PUT 4 MiB body only | **1** | 36.0 ms (111.2 MiB/s) | — | — |
| PUT 4 MiB response wait | 1 | 19.3 ms | — | — |
| PUT 4 MiB **end-to-end** | **1** | **57.7 ms → 72.4 MiB/s = 75.9 MB/s** | — | — |
| 8× 4 MiB concurrent PUT | 8 | wall 540 ms (**59.2 MiB/s = 62.1 MB/s** aggregate, all asserted) | — | — |
| Expired capability | 1 | **403** (asserted) | | |

**Why end-to-end is the only PUT rate.** `put_object` stops the body timer when `http.client` returns from `request()`, i.e. when the 4 MiB has been handed to the kernel socket buffer. That is the same measurement error R3 fixed for GET TTFB (round-4 finding 12). Bytes ÷ (body + response) is the transfer rate; the body-only figure is a *buffer-fill* rate and must never be used for sizing.

Honest limits:
- **PUT is n=1.** GETs have n=200. `p99` at n=200 is the third-largest sample.
- Sequential GETs all hit the same 4 MiB key: this is not a cold-read or a concurrent-read test, and 92.2 MiB/s is not a burst throughput.
- The 8× figure is an aggregate under load, not a per-stream rate.
- Compression, scheduling and telemetry delay are outside every interval below.

## 1. Publication budget — re-derived (round-4 findings 12, 13)

2 GB decimal = 2000 MB. Rates: e2e 75.9 MB/s; reread 96.7 MB/s; aggregate 62.1 MB/s.

| Path | Component | Time |
|---|---|---|
| PUT, single stream | 2000 / 75.9 | **26.4 s** |
| Reread for a full rehash | 2000 / 96.7 | **20.7 s** |
| Sampled fast path (1 % hash) | PUT + 19.3 ms ack + 2.1 ms HEAD | **≈26.4 s** |
| Full rehash path | PUT + reread + ack + HEAD | **≈47.1 s** |
| 2 GB among 8 concurrent PUTs | 62.1/8 = 7.76 MB/s ⇒ 2000/7.76 | **≈258 s** (worst case) |

What can and cannot be claimed:

- **Withdrawn: "a 2 GB layer cannot publish inside 30 s"** — R3 already withdrew this; R4 confirms it stayed withdrawn only by accident, because the number it rested on was mismeasured. Correct statement: **≈26 s best case, design cluster, transfer only, no compression, one stream, n=1.**
- **Also withdrawn: the stronger "exactly 18 s"** from R3, which used the buffer-fill rate.
- **Per-stream under contention is the real production risk**, not the single-stream number. The engine holds 8 PUT slots per session (RP :158); a 2 GB layer sharing them takes minutes, not tens of seconds. Dividing 2 GB by the aggregate rate is exactly the substitution §0 forbids (round-4 finding 13).
- **The donor must survive its whole export, not just its own PUT.** `uploaded{done}` follows *all* of the export's PUTs (RP :150-158), so a donor that finishes one PUT and dies before the export completes never triggers verification — R3's "donor death doesn't matter because verification is service-side" was correct in principle and wrong in execution (round-4 finding 14).
- **Fix applied:** on WS close, `EndRemoteProcess` for that export, or an `uploaded` timeout, the service HEADs every `pending` key of the export immediately; keys that exist with the declared size are verified, the rest fail at once. No 10-minute wait, and no dependence on a donor surviving to send `uploaded`.

## 2. Reference numbers elsewhere (cited)

AWS S3 same-region 50–200 ms typical; S3 Express One Zone 2–8 ms; Azure Premium Block Blob "single-digit ms"; cross-region/WAN 20–100+ ms plus egress. The design-cluster numbers here are an **in-cluster MinIO floor**, not a production estimate.

## 3. Production store — the constraint wins

- **Production is external S3 outside the cluster.** Endpoint, region and provider are stated per deployment; "outside the cluster" does not imply WAN.
- **No MinIO in production.** MinIO is the design-cluster backend only.
- **Stateful**: `dcache` and Postgres (and any SQLite working copy) never on ephemeral capacity; cold restore accepted.
- If the mandated backend cannot accept the protocol's header-less single PUT, the escape hatch is a service-side staging front end (verify + server-side copy), measured before adoption.

## 4. Where latency is measured

Engines hit presigned URLs directly; the service cannot observe their GETs.

| Signal | Source | Use |
|---|---|---|
| `dcache_store_errors_total` / `_requests_total` | provider metrics (S3 → CloudWatch 5xx/SlowDown; Azure → ServerBusy/ClientTimeout), **minutes late** | breaker input, provider-side only |
| `dcache_install_p95_seconds` | `cache.offer.duration_ns` | successful imports only |
| `dcache_computed_with_url_total` | `computed` entries holding a minted URL in the last hour | proxy for attempted-and-fell-back; **not** a wait time |
| `dcache_renew_total{result="unavailable"}` | mint refusals | service-side early warning |
| `dcache_deferred_publish_chains` | chains waiting on a partner verify | service-side early warning |
| `dcache_probe_s3_seconds{op}` | active probe, 30 s | liveness only |

A client-side hang (connect, then no response) is **not observable** by the service at all: no per-attempt engine failure attribute exists in beta.25, and provider metrics lag. That is stated as a limitation, not papered over with a fabricated signal.

## 5. Breaker thresholds (policy, not measurement)

| Deployment | store errors warn | trip | install p95 warn |
|---|---|---|---|
| Design cluster | 2 % / 10 min | 10 % | 5 s |
| Production external S3 | 2 % / 10 min | 10 % | 5 s |

A hanging store makes each demanded part stall up to 30 s before fallback (RP §7.2) — worse than no cache. The breaker stops **merges and offers**, never running builds.

## 6. Per-part serve-cost model (assumptions labelled)

```
serve_cost(part) = Σ_layers [ TTFB + bytes/read_throughput + apply ]
                 + P(renew)·2 s + P(stall)·30 s
```

- TTFB measured (§0); production from the provider exporter.
- `read_throughput`: **not measured** — PUT rates are not a read basis, and the 4 MiB single-key GET is not a burst test. Production must supply it with 100 parallel 2 GB GETs; until then the term is `UNKNOWN`, admission falls back to the frequency gate only, and the dashboard flags it.
- `apply`: unmeasured; the engine interleaves download and application (`attrs.go:782-787`), so summing is an upper bound.
- A slow-but-progressing read is not capped at 30 s; 30 s is the per-idle-stall bound.
- Admission uses `donor_cost ≥ k · serve_cost`, `k = 3` — a policy choice, not an optimum.

## 7. Reproducing

```bash
kubectl run s3probe --rm -i --restart=Never --image=python:3.12-alpine \
  --overrides='{"spec":{"containers":[{"name":"s3probe","image":"python:3.12-alpine",
     "stdin":true,"stdinOnce":true,
     "envFrom":[{"secretRef":{"name":"s3probe-creds"}}],
     "env":[{"name":"S3_HOST","value":"minio.minio.svc.cluster.local:9000"}]}]}}' \
  -- python - < s3_latency_probe.py
```

V3 asserts bucket reachability, every GET/PUT/HEAD status, HEAD size, exact `Content-Range` and byte count on open-ended ranges, 403 on an expired capability, and that all 8 concurrent PUTs succeed; exits non-zero on any failure; prints MiB/s and MB/s; prints the **end-to-end** PUT rate next to the buffer-fill rate.

**The measurement that sizes production:** 100 parallel 2 GB GETs against the real endpoint from a same-region node, plus 8 parallel 2 GB PUTs to establish the per-stream contention number.
