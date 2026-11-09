> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# S3-style shared Dagger cache for 30-second-ephemeral nodes — decision brief

Date 2026-11-05. Sources in this directory: `architecture.md` (Claude Opus, architect + red team), `recon-protocol.md` (dagger source at `7e9eaac` / v1.0.0-beta.25), `recon-prior-art.md` (BuildKit/REAPI/S3/teardown prior art). Every API Claude named was verified present in the cloned tree.

## The blunt answer

There is **no drop-in S3 cache for Dagger**. Three facts decide the design:

1. The v0.21 `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` S3 export was removed in v0.21.x (#13405). Stable v0.21.11 has **no** cross-node engine cache at all.
2. The 1.0-beta replacement is a **push protocol**, not an object-store importer: the engine opens one WebSocket to `/v1/engine`; the *service* decides what to export (`export{roots:[engineEntryNumbers]}`), hands out **presigned PUT URLs**, gets back `uploaded{done,failed}`, and later pushes `merge`/`offer` to other engines. Blobs are **whole-object single PUTs** (no multipart, no retry in the engine); reads are Range GETs requiring an exact `206 Content-Range`.
3. There is **no public deployable server**. Dagger Cloud's service repo is private (404). The protocol is open, the service is not. So "S3-style cache" = **write a service**.

Consequence: the byte plane is easy (presigned S3/MinIO URLs; a killed node leaves no half-object because single PUT is atomic). The **metadata plane is the project**: WS state machine + OTLP/engine-event ingestion (there is no inventory message and no on-miss lookup — the service must learn entry numbers from telemetry) + a dagql metadata pool + renewal keys (2 s deadline) + GC.

## Topology

Two clusters, one binary:

- **Design cluster (this homelab k3s):** persistent nodes. MinIO (PVC, `192.0.2.10:9000`) + `dcache` + Postgres all run here. This is where the service is built and chaos-tested (kill engine pod, inject S3 latency, pause the leader).
- **Production:** **external S3 outside the cluster**, compute nodes **ephemeral (~30 s)**. Same byte plane (presigned PUT/GET to the external endpoint). `dcache` is the only stateful component and must never run on ephemeral capacity — pin it to a stable control-plane pool or run it outside the cluster, with a warm standby tailing checkpoints.

## Decision

**(d) hybrid, two tiers.**

- **Tier 1 — Dagger op cache.** Engine `v1.0.0-beta.25` pinned by digest on spot nodes, `emptyDir`/NVMe local disk treated as pure scratch. A Go service (`dcache`, StatefulSet) on non-spot nodes: one fenced metadata writer per trust pool, importing the engine's own `dagql`+`core` at the *same commit*. Bytes go **engine → MinIO directly** via presigned URLs; `dcache` never proxies bytes. Durable state = immutable digest blobs + a Postgres blob ledger + create-only SQLite checkpoints in MinIO.
- **Tier 2 — tool-level caches.** Zot pull-through for base images, GOPROXY, sccache/Turbo/Nx on MinIO, injected into `withExec`. Covers what Tier 1 structurally cannot: **cache volumes (`withMountedCache`) are engine-local and are not part of the protocol**, and per-session-keyed calls.

Tier 1 requires the beta line. **v0.21.x cannot participate.** Tier 2 works today on v0.21.11.

## The five invariants that make 30-second teardown safe

1. **Publication gate.** `SetStoredPart` only after the service has HEAD-checked size and streamed a sha256 of every blob in the chain from MinIO. `uploaded.done` and a 2xx PUT are hints, not durability (the engine itself says so).
2. **Metadata claims are hints.** Bytes that exist only on a dying node do not exist.
3. **No torn reads.** S3 single-PUT atomicity + the publication gate + the engine's own import digest check.
4. **GC fence.** Delete only via an atomic conditional `UPDATE … SET state='deleting'` guarded by `LiveBlobs()` plus URL-mint grace ≥ GET TTL + 30 s stall.
5. **Single writer.** Postgres-fenced leader epoch; epoch-namespaced create-only checkpoints. The wire carries **no locks, leases or fencing**, and dagql/SQLite are single-process — so stateless multi-replica metadata is not available. Scale out by adding pools, not replicas.

## What happens when a node dies at second 30

- In-flight PUTs are cancelled; S3 creates no object. The WS drops; the export providers and exportID die with the session.
- `pending` ledger rows leak until their 10-minute lease expires, then `failed`; parts waiting on them stay unpublished. Metadata merged earlier survives as a value with **no stored parts** — harmless: other engines compute it, or a later donor fills the bytes.
- Already-`verified` parts are unaffected. The reaper releases the dead `cacheID` after 15 min.
- **Loss bound = work whose publication had not finished.** So the service must export at span completion, not at session end, and the primary SLI is *span end → part verified*.

## Cost reality

- Request cost is negligible for layer-sized blobs (~$8/month at AWS rates for 20k PUTs + 300k GETs/day).
- **Bytes dominate.** In production the object store is external, so **egress and WAN latency are a real budget line**, not zero: every S3 op gets a p99 SLO and a breaker (a hanging GET stalls builds 30 s before fallback). The design cluster's in-cluster MinIO has no egress cost and is therefore *not* representative of production latency — measure both.
- `dcache` memory unknown; plan 16 GiB/leader and measure. Pool graphs inherit the engine's pathology: restore time linear in DB size (#15013) and e-graph eq-class walks under a global lock (#15049, fix #15050 still open).

## The honest ceiling (why this is a go/no-go, not a sure thing)

Profitable hits = ops that are not per-session keyed and whose outputs are snapshot parts.

- Image pulls / package-install layers: near 100% addressable — but Zot covers them.
- Source-dependent builds/tests: addressable only with uniform umask (#15034) and identical source — and their **incremental** benefit is lost because the compiler cache lives in a cache volume. A one-line change re-runs the full compile, which can be *slower* than today's warm node-local engine.
- Modules with git `clients` (#15014), filtered `dagger check` selections (#15044), `PRIVATE` mounts (#13060): 0% until upstream fixes land.
- Correctness hazards on cross-engine hits: #14913 (env var dropped from key — wrong hit), #12883 (URI secrets — failed build). Both must be confirmed fixed in the pinned build before Tier 1 goes live.

**Gate:** measure with `dagger.io/cache.pairing_digest` (self-digest excluding implicit inputs) across sessions. Count spans that would have hit, and spans lost to per-session keying, weighted by duration. Build `dcache` only if addressable compute ≥ ~30% of job wall time on the top pipelines after Tier 2.

## Phases

0. **Now, on v0.21.11 (1–2 wk):** Tier 2 (Zot pull-through, GOPROXY, sccache/Turbo on MinIO), enforce `umask 022`, inventory git-client modules / filtered checks / URI secrets / PRIVATE mounts, run the pairing-digest measurement (beta.25 shadow engine if v0.21 lacks the contract).
1. **Compat:** port modules to beta.25; verify #14913, #13898, #14884, #14657 in the pinned build.
2. **Build `dcache`:** RP §12.5 acceptance suite + chaos (kill engine mid-PUT, kill leader mid-checkpoint, GC racing publish, hanging MinIO).
3. **Parallel run:** separate `dagger-engine-ephemeral` DaemonSet on a spot pool; existing v0.21 DaemonSet untouched. Kill switch = remove `remoteCache.url` or quarantine the pool.
4. **Retire** the hostPath DaemonSet only after 2 weeks within the agreed p95 bound.

## If you only have a week — do not build `dcache`

**(A, recommended)** Keep 3–5 engines on **on-demand, non-spot** nodes with their NVMe caches; move ARC runners (thin clients) to spot. Set `terminationGracePeriodSeconds` well above the engine stop budget (#13951). Add Zot pull-through + GOPROXY.

**(B, if engines must be spot)** Ephemeral engines with empty caches + Tier 2 + the Phase 0 measurement. Accept full loss of the op cache; let the measurement decide whether Tier 1 is worth a quarter of engineering.

## Top red-team risks

| # | Risk | Mitigation |
|---|---|---|
| R1 | Hit rate too low to justify the build | Phase 0 gate; Tier 2 first |
| R2 | Beta wire churn silently zeroes hits | quarantine + alert; lockstep pins; contract test on every bump |
| R3 | Wrong hit / failed build across engines (#14913, #12883) | release blockers; 1% shadow verify with pool quarantined; per-pool kill switch |
| R4 | Service graph doesn't scale (restore/eq-class stalls) | small pools, 7-day TTL, entry caps, hot standby tailing checkpoints |
| R5 | MinIO bottleneck or on spot nodes | pin MinIO off spot; p99-latency breaker (a hanging GET stalls builds 30 s before fallback) |
| R6 | Exposure window > node lifetime | export at span end; publication-latency SLI |
| R7 | Telemetry dependency / token leak | NetworkPolicy deny `api.dagger.cloud`; reject `dag_` tokens at handshake |
