> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Latency reality check + Azure deployment variant

Date 2026-11-05. Companion to `decision.md` and `metrics-and-eviction.md`.

## 1. Measured, not guessed: this design cluster

Probe: a Python pod in the cluster (`default` ns) against `minio.minio.svc.cluster.local:9000`, SigV4 signed, keep-alive connections, 4 MiB objects. Script: `s3_latency_probe.py` (stdlib only, no pip, no mc). n=100 GET / 50 range GET / 30 PUT.

| Operation | TTFB p50 | TTFB p95 | TTFB p99 | total p50 |
|---|---:|---:|---:|---:|
| GET 4 MiB (keep-alive) | **4.29 ms** | 6.40 | 6.90 | 42.9 ms (≈93 MB/s per conn) |
| GET 1 MiB range | **4.01 ms** | 5.94 | 9.85 | 12.8 ms |
| GET 4 MiB (cold connect) | 10.49 ms | 11.6 | 11.6 | 49.9 ms |
| PUT 4 MiB | **84.6 ms** | 142 | 143 | 84.6 ms |

Notes that matter for the design:

- **Round-trip latency is ~4 ms**, so per-object overhead is negligible. A job that reads 150 blobs pays **~0.6 s** total in latency; the same job against a public-region S3 at 50–200 ms TTFB would pay **~7–30 s**. That gap is the whole argument for in-region/in-cluster object storage.
- **PUT is 20× slower than GET TTFB** (84 ms p50) because MinIO commits and fsyncs. That is the floor of the publication window (#14956-style exposure): a part cannot be `verified` faster than its slowest blob PUT.
- **4 MiB at 93 MB/s/conn** means a 2 GB layer is ≈22 s of transfer on one connection. With the engine's 8 concurrent PUTs this is fine in aggregate, but a *single large layer produced in the last 20 s of a 30-s-lifetime node will not publish*. Prioritise exporting small, expensive parts first (R6).
- The HEAD path (verifier) was not measured — probe bug, fix before trusting `dcache_verify_seconds`.

## 2. Reference numbers elsewhere (cited, not measured here)

| Target | Report | Source |
|---|---|---|
| AWS S3 Standard, same region, GetObject | 50–200 ms typical; 100+ ms p99 in independent benchmarks | cubeapm FAQ; nixiesearch benchmark |
| AWS S3 Express One Zone (single AZ) | 2–8 ms | cubeapm FAQ |
| Azure Premium Block Blob | "single-digit millisecond" server latency | Microsoft Learn (storage-blobs-latency / access tiers) |
| Azure Hot/Cool Blob | "milliseconds" (no SLA number published per op) | Microsoft Learn |
| Azure Files / managed disk | sub-ms to low-ms, but **not S3 API** | — |
| Cross-region / internet S3 | 20–100+ ms, plus egress $ | varies |

Conclusion: "same datacenter" is a spectrum. In-cluster MinIO ≈ 4 ms. Azure Blob Premium ≈ low single-digit ms server-side but you must add the network path. Public-region S3 ≈ 50–200 ms and is 10–50× worse for a many-blob workload.

## 3. Azure: what you actually put behind the presigned URLs

**Azure Blob Storage is not S3-compatible.** There is no native S3 API, no AWS SigV4, no presigned-PUT semantics (Blob has SAS tokens and block-based upload). The Dagger engine's PUT path sends a raw single PUT with a presigned URL and no extra headers — so whatever you point it at must speak S3 and honour presigned URLs.

| Option | S3 API | Presigned PUT/GET | Latency | Verdict |
|---|---|---|---|---|
| **MinIO on AKS (dedicated non-spot pool) or on 2–4 VMs, Premium SSD v2** | yes | yes (we just tested SigV4 against it) | ~2–6 ms in-region | **Recommended.** Same binary, same probe, same design as the homelab. |
| Ceph RGW (AKS or VMs) | yes | yes | ~3–10 ms | Works; heavier ops burden than MinIO. |
| Azure Blob + S3 gateway (s3proxy / Zenko CloudServer) | via gateway | gateway-dependent, **unverified** | +1–3 ms hop + Blob's own ms | Only if Blob is mandated. Must be proven with the real engine before relying on it. |
| Wasabi / B2 / R2 over internet | yes | yes | 20–100 ms + Azure egress ($0.087/GB) | Rejected as primary read tier; DR copy only. |

Why MinIO wins in Azure: the byte plane is already S3, the service code is unchanged, and you avoid a translation layer whose presign behaviour nobody has validated against this protocol.

**Sizing the Azure MinIO tier.** A cold-start wave of 100 engines × 2 GB = 200 GB of burst reads. A D4s_v5 VM cannot absorb that (≈2 Gbps → 800 s). Use a size with Accelerated Networking and 10+ Gbps aggregate (e.g. D8s_v5 class, or 2–4 nodes behind a load balancer), or accept a staggered merge policy that warms engines slowly. Measure with the probe from an AKS node before committing.

**Where `dcache` runs in Azure.** It is the only stateful component; AKS spot nodes are disqualified. Run it on the system/dedicated node pool (tainted, non-spot) or outside AKS entirely, with managed-disk PVC for the SQLite working copy, Postgres (Azure Database for PostgreSQL) for the ledger/fencing, and checkpoints in the same MinIO/S3. AKS node autoscaler must never scale that pool to zero.

**Azure-specific knobs worth setting:**
- MinIO behind a private endpoint; engines reach it over the cluster network, never the public internet.
- Storage account / NIC egress metrics become a first-class budget line (`dcache_s3_egress_bytes_total`).
- AKS spot eviction gives ~30 s notice (SIGTERM then SIGKILL) — exactly the teardown model the design assumes, so no change to the engine-side config; just `terminationGracePeriodSeconds: 25` and no local persistence.
- If Azure policy forbids running storage on AKS nodes, MinIO on two standalone VMs in the same VNet is the fallback; that keeps the ~1–2 ms intra-VNet path.

## 4. Breaker thresholds, now that we have numbers

`metrics-and-eviction.md` proposed a 2 s p99 GET breaker. Keep it as the **hang detector**, but add a **selection** threshold:

| Deployment | `s3_get_p99` warn | breaker | Rationale |
|---|---|---|---|
| Homelab in-cluster MinIO (measured 6.9 ms) | 25 ms | 200 ms | 4 ms is normal; 200 ms means something is wrong, not merely slow |
| Azure MinIO same-VNet (expect 2–6 ms) | 25 ms | 200 ms | same |
| Azure Blob via gateway (expect 5–15 ms) | 50 ms | 500 ms | gateway hop + Blob server latency |
| Public S3 over WAN (expect 50–200 ms) | 250 ms | 2 s | here 2 s is the *hang* bound; the design should not run this way as a primary tier |

Rule of thumb: **if p99 GET latency exceeds ~5% of the median recompute time of the parts you are serving, the cache stops paying for itself.** At 4 ms against a 40 s rebuild it is 0.01%; at 200 ms against a 0.5 s step it is 40% — which is why short, cheap operations should not be remote-cached at all (the admission `min_cost_seconds` gate handles this).

## 5. What to run before committing either cluster

```bash
# in-cluster (homelab) — already done, numbers above
kubectl run s3probe --rm -i --restart=Never --image=python:3.12-alpine \
  --env=MINIO_HOST=minio.minio.svc.cluster.local:9000 \
  --env=MINIO_AK=... --env=MINIO_SK=... -- python - < s3_latency_probe.py

# Azure: same script, one pod on an AKS node, endpoint = MinIO service DNS.
# Record: GET TTFB p50/p99, PUT p50/p99, and the 200 GB cold-start burst test
# (100 parallel 2 GB GETs) — the burst, not the single-object latency, sizes the tier.
```
