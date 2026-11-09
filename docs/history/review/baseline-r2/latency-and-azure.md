> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Latency reality check + Azure deployment variant — revised (R1)

Updated 2026-11-07 after the independent review. Changes are keyed to review findings (F36–F42, F49, F45) and marked ⚠ where the earlier text was wrong.

## 1. Measured, not guessed: this design cluster

Probe v2: a Python pod in the cluster against `minio.minio.svc.cluster.local:9000`, **presigned query-string URLs** (what the engine actually receives), keep-alive, 4 MiB objects. Script `s3_latency_probe.py`; n=200 GET, 100 range GET, 50 HEAD, 8 concurrent PUTs. Credentials come from a k8s Secret via `envFrom`, not `--env` on the command line (F49).

| Operation | p50 | p95 | p99 | max |
|---|---:|---:|---:|---:|
| GET 4 MiB (presigned) TTFB | **3.96 ms** | 6.13 | 8.03 | 10.19 |
| GET 4 MiB total | 43.1 ms | — | — | — |
| GET open-ended range `bytes=off-(size-1)` TTFB | **4.10 ms** | 4.95 | 7.02 | 10.69 |
| HEAD (verifier path) | **1.92 ms** | 2.62 | 3.06 | 3.06 |
| PUT 4 MiB body transfer | 27.0 ms (**155.6 MB/s**) | — | — | — |
| PUT 4 MiB response wait | 18.8 ms | — | — | — |
| 8× 4 MiB concurrent PUT | wall 457 ms (**70.1 MB/s aggregate**) | — | — | — |
| Expired capability | **403** (renewal trigger confirmed) | | | |

⚠ **Correction (F36):** the earlier "PUT is 20× slower than GET TTFB (84 ms) because MinIO fsyncs" was **wrong**. v1 timed body upload inside the "TTFB" window. A 4 MiB PUT moves at ~155 MB/s and the server takes ~19 ms to acknowledge; the honest comparison is PUT total (~46 ms) vs GET total (~43 ms). MinIO is not the bottleneck — **the network/disk is**, and the aggregate falls to ~70 MB/s at 8 concurrent writers.

What that means:

- **GET round trips are ~4 ms**, so per-object latency for a 150-blob job is ~0.6 s. Against a public-region S3 at 50–200 ms it would be 7–30 s. That still justifies an in-region store.
- **Publication is throughput-bound, not latency-bound.** Budget per blob: engine diff+compress → PUT (size ÷ ~70–155 MB/s) → HEAD (~2 ms) → verify read (size ÷ ~93 MB/s). For a 2 GB layer that is **≈26–55 s of transfer alone**, before compression. ⚠ A single large layer produced in the last 30 s of a node's life **cannot publish**, and the earlier "22 s" figure understated it. Export small, expensive parts first; split monolith layers where possible.
- **HEAD is cheap (1.9 ms)** — the size check costs nothing, so keep it even if full streaming verification is sampled (see `decision` §"what we cut").
- **Percentiles:** at n=200, "p99" is the second-largest sample; treat p99.9 and the 30 s stall bound (RP §7.2) as unobserved by this probe. Do not derive production thresholds from a 100-sample run (F37/F46).

## 2. Reference numbers elsewhere (cited, not measured here)

| Target | Report | Source |
|---|---|---|
| AWS S3 Standard, same region, GetObject | 50–200 ms typical; 100+ ms p99 in independent benchmarks | cubeapm FAQ; nixiesearch benchmark |
| AWS S3 Express One Zone (single AZ) | 2–8 ms | cubeapm FAQ |
| Azure Premium Block Blob | "single-digit millisecond" server latency | Microsoft Learn |
| Azure Hot/Cool Blob | "milliseconds" (no per-op SLA published) | Microsoft Learn |
| Azure Files / managed disk | sub-ms to low-ms, but **not S3 API** | — |
| Cross-region / internet S3 | 20–100+ ms, plus egress $ | varies |

## 3. Azure: what you actually put behind the URIs

⚠ **Correction (F39):** the earlier claim that Blob has "no presigned semantics" was wrong. A **SAS URL is a capability**: Blob `Get Blob` with SAS supports `Range` and returns `206` with a standard `Content-Range`, and an expired SAS returns `403` — which is exactly the renewal trigger the engine expects (RP §7.2). **The GET side works natively on Azure Blob.**

The real blocker is **PUT**: `Put Blob` requires the `x-ms-blob-type: BlockBlob` header, and the engine adds no headers to its raw single PUT (RP §2). That produces an option the earlier draft never considered:

| Option | GET | PUT | Latency | Verdict |
|---|---|---|---|---|
| **MinIO on AKS (dedicated non-spot pool)** or 2–4 VMs + Premium SSD v2 | S3 presigned | S3 presigned | ~2–6 ms | **Recommended** — one code path, same probe, same as homelab. |
| **Hybrid: GET from Blob SAS + PUT to a staging endpoint, then `Put Blob From URL` server-side copy** | native, managed, zone-redundant | dcache front end accepts the header-less PUT, verifies, copies | GET low-ms; PUT +1 hop | Viable if Blob is mandated. Copy is server-side (no egress), but doubles PUT-side work and needs its own verification gate. |
| Azure Blob via S3 gateway (s3proxy / Zenko) | via gateway | gateway-dependent, **unverified** | +1–3 ms | Only if a gateway is already operated. Prove it with the real engine first. |
| Ceph RGW on AKS/VMs | S3 presigned | S3 presigned | ~3–10 ms | Works; heavier ops burden. |
| Wasabi / B2 / R2 over internet | S3 presigned | S3 presigned | 20–100 ms + Azure egress ($0.087/GB) | Rejected as primary read tier; DR copy only. |

**Counter-arguments to MinIO-on-AKS that must be answered before committing** (F39):
- MinIO's guidance favours locally attached drives; Premium SSD v2 is network block storage with provisioned MB/s caps, and the VM's **uncached-disk throughput** — not the NIC — bounds a 200 GB cold burst.
- AKS node-image upgrades drain nodes; zonal managed disks pin pods to a zone. Verify erasure-set write quorum survives a node-pool upgrade with the PDB/`maxUnavailable` you intend to run.
- ⚠ **UNVERIFIED but load-bearing:** upstream MinIO reportedly stopped publishing community binaries/images in late 2025 and moved the community repo to maintenance mode. A primary store you cannot patch is disqualifying. Check this before selecting MinIO for production; if true, Ceph RGW or a maintained fork is the answer.
- Because the cache fails open (I7), durability buys little: a disposable single-replica MinIO on local NVMe (Lsv3 class) is a legitimate, simpler shape than erasure-coded HA.

**Sizing (F40):** ⚠ size on **disk throughput**, not NIC. Small Dsv5 sizes already advertise up to ~12.5 Gbps networking, so upsizing the VM for bandwidth targets the wrong limit. Provision Premium SSD v2 MB/s and check the VM's uncached-disk cap against the measured 70 MB/s-per-writer aggregate; measure the 200 GB burst itself.

**Where `dcache` runs in Azure.** It is the only stateful service (Postgres and the SQLite PVC are also stateful — see `metrics-and-eviction.md` §7 correction). AKS spot nodes are disqualified: run it on a tainted, non-spot system pool or outside AKS, with Postgres (Azure DB for PostgreSQL) for the ledger/fencing and checkpoints in the same object store. The autoscaler must never scale that pool to zero.

⚠ **Correction (F41):** AKS preemption does **not** turn a Scheduled Event into a pod drain unless a termination handler is installed, and the WS protocol carries **no SIGTERM signal to `dcache`** — so "on SIGTERM the service stops scheduling exports" (old R6) cannot happen as written. Either install a preemption handler + `preStop` hook that tells `dcache` to stop scheduling for that cacheID, or drop the claim and accept that exports race the kill.

Other knobs: MinIO behind a private endpoint; storage-account egress becomes a first-class budget line (`dcache_s3_egress_bytes_total`); `terminationGracePeriodSeconds: 25` with no local persistence.

## 4. Breaker thresholds, from measured numbers

| Deployment | `s3_get_p99` warn | breaker | Rationale |
|---|---|---|---|
| Homelab in-cluster MinIO (measured 8.0 ms) | 25 ms | 200 ms | 4 ms is normal; 200 ms means something is broken |
| Azure MinIO same-VNet (expect 2–8 ms) | 25 ms | 200 ms | same |
| Blob SAS GET (expect 5–15 ms) | 50 ms | 500 ms | managed service + network path |
| Blob via gateway (expect 5–20 ms) | 50 ms | 500 ms | gateway hop |
| Public S3 over WAN (50–200 ms) | 250 ms | 2 s | 2 s stays the pure *hang* detector |

⚠ **Correction (F37):** replace the old "p99 GET > 5% of median recompute" rule — it compares a per-request tail with a per-part median, and with ~150 GETs per job the chance of seeing at least one p99 event is 1−0.99¹⁵⁰ ≈ 78%, so the job tail is set by p99.9+ and by the 30 s stall bound. Use a **per-part cost model** instead, and feed it into admission:

```
serve_cost(part) = Σ_layers [ TTFB + size/throughput + apply ]  +  P(renew)·2 s  +  P(stall)·30 s
admit iff   recompute_lazy_inclusive ≥ k · serve_cost(part),   k ≈ 3
```

Both terms come from measured data (TTFB ~4 ms, throughput ~70–155 MB/s) and from the engine's own lazy-span durations (see metric redefinition in `metrics-and-eviction.md`).

## 5. How to run the probe

```bash
# 1. credentials from a Secret, never --env on the command line
kubectl create secret generic s3probe-creds \
  --from-literal=S3_AK=... --from-literal=S3_SK=...

kubectl run s3probe --rm -i --restart=Never --image=python:3.12-alpine \
  --overrides='{"spec":{"containers":[{"name":"s3probe","image":"python:3.12-alpine",
     "stdin":true,"stdinOnce":true,
     "envFrom":[{"secretRef":{"name":"s3probe-creds"}}],
     "env":[{"name":"S3_HOST","value":"minio.minio.svc.cluster.local:9000"}]}]}}' \
  -- python - < s3_latency_probe.py

# Azure / HTTPS: same script with
#   S3_HOST=minio.dcache.svc:9000  S3_SCHEME=https  S3_REGION=<region>
# Then run the burst test (100 parallel 2 GB GETs) — the burst, not the
# single-object latency, sizes the tier.
```

The script asserts exact `Content-Range` on open-ended ranges, expects 403 on an expired capability, reads every HEAD response, and reports PUT as MB/s plus response wait separately.
