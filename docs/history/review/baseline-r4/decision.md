> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# S3-style shared Dagger cache for 30-second-ephemeral nodes — decision brief

Date 2026-11-05. Sources in this directory: `architecture.md` (Claude Opus, architect + red team), `ledger.md` (normative blob state machine), `metrics-and-eviction.md`, `latency-and-azure.md`, `recon-protocol.md` (dagger source at `7e9eaac` / v1.0.0-beta.25), `recon-prior-art.md` (BuildKit/REAPI/S3/teardown prior art). Every API Claude named was verified present in the cloned tree.

## The blunt answer

There is **no drop-in S3 cache for Dagger**. Three facts decide the design:

1. The v0.21 `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` S3 export was removed in v0.21.x (#13405). Stable v0.21.11 has **no** cross-node engine cache at all.
2. The 1.0-beta replacement is a **push protocol**, not an object-store importer: the engine opens one WebSocket to `/v1/engine`; the *service* decides what to export (`export{roots:[engineEntryNumbers]}`), hands out **presigned PUT URLs**, gets back `uploaded{done,failed}`, and later pushes `merge`/`offer` to other engines. Blobs are **whole-object single PUTs** (no multipart, no retry in the engine); reads are Range GETs requiring an exact `206 Content-Range`.
3. There is **no public deployable server**. Dagger Cloud's service repo is private (404). The protocol is open, the service is not. So "S3-style cache" = **write a service**.

Consequence: the byte plane is easy (presigned S3/MinIO URLs; a killed node leaves no half-object because single PUT is atomic). The **metadata plane is the project**: WS state machine + OTLP/engine-event ingestion (there is no inventory message and no on-miss lookup — the service must learn entry numbers from telemetry) + a dagql metadata pool + renewal keys (2 s deadline) + GC.

## Topology

Two clusters, one binary:

- **Design cluster (this homelab k3s):** persistent nodes. MinIO (PVC, `192.0.2.10:9000`) + `dcache` + Postgres all run here. This is where the service is built and chaos-tested (kill engine pod, inject S3 latency, pause the leader).
- **Production:** **external S3 outside the cluster**, compute nodes **ephemeral (~30 s)**. Same byte plane (presigned PUT/GET to the external endpoint). `dcache` **and its Postgres** are the stateful components (F3) and must never run on ephemeral capacity — pin them to a stable control-plane pool or run them outside the cluster. Failover is a **cold restore** (minutes, linear in graph size), not a checkpoint-tailing warm standby (F62: tailing every checkpoint is a TB/day write amplifier).

## Decision

**(d) hybrid, two tiers.**

- **Tier 1 — Dagger op cache.** Engine `v1.0.0-beta.25` pinned by digest on spot nodes, `emptyDir`/NVMe local disk treated as pure scratch. A Go service (`dcache`, StatefulSet) on non-spot nodes: one fenced metadata writer per trust pool, importing the engine's own `dagql`+`core` at the *same commit*. Bytes go **engine → object store directly** via presigned URLs (in-cluster MinIO here, external S3 in production); `dcache` never proxies bytes. Durable state = immutable digest blobs (in-cluster MinIO here, **external S3 in production**) + a Postgres blob ledger + create-only SQLite checkpoints written every ≥10 min on change (not every 60 s).
- **Tier 2 — tool-level caches.** Zot pull-through for base images, GOPROXY, sccache/Turbo/Nx on the object store (MinIO in the design cluster; production backend named per deployment), injected into `withExec`. Covers what Tier 1 structurally cannot: **cache volumes (`withMountedCache`) are engine-local and are not part of the protocol**, and per-session-keyed calls.

Tier 1 requires the beta line. **v0.21.x cannot participate.** Tier 2 works today on v0.21.11.

## The five invariants that make 30-second teardown safe

1. **Publication gate.** Graph insertion happens under the in-process mutex `M` after re-reading every **distinct** digest of the chain (chains repeat layers) and requiring each to be `verified` (F12, round-4 F31); GC mark takes the same `M`, so it cannot interleave. There is no database transaction around `SetStoredPart` and none is needed — the graph write is the commit, and a killed process leaves no part and no orphan (round-4 F2). Verification is authorization + existence + size for every blob plus a sha256 of a **1 % sample** through the S3 endpoint (§5.1 of `architecture.md`); the engine's import hash is the final content gate. `uploaded.done` and a 2xx PUT are hints, not durability.
2. **Metadata claims are hints.** Bytes that exist only on a dying node do not exist.
3. **No torn reads.** S3 single-PUT atomicity + the publication gate + the engine's own import digest check.
4. **GC fence.** `ledger.md` is the only normative copy: liveness **recomputed from `LiveBlobs()`** at mark time under the same in-process mutex as publish (never a maintained counter), generation-scoped object keys (a delayed delete cannot remove a newer generation), an epoch predicate on every write, publish = state re-read + `SetStoredPart` with **no DB transaction** and no lock held across it, `G = 2 × GET TTL`, owner-fenced verify and an idempotent recovery scanner. Mark-only startup writes candidates without changing state. There is no expiring pin: a durable deadline is not cancellation.
5. **Single writer.** Postgres-fenced leader epoch; epoch-namespaced create-only checkpoints. The wire carries **no locks, leases or fencing**, and dagql/SQLite are single-process — so stateless multi-replica metadata is not available. Scale out by adding pools, not replicas.

## What happens when a node dies at second 30

- In-flight PUTs are cancelled; S3 creates no object. The WS drops; the export providers and exportID die with the session.
- `pending` ledger rows leak until their 10-minute lease expires, then `failed`; parts waiting on them stay unpublished. Metadata merged earlier survives as a value with **no stored parts** — harmless: other engines compute it, or a later donor fills the bytes.
- Already-`verified` parts are unaffected. The reaper releases the dead `cacheID` after 15 min.
- **Loss bound = work whose publication had not finished.** So the service must export at span completion, not at session end, and the primary SLI is *span end → part verified*.

## Cost reality

- Request cost is negligible for layer-sized blobs (~$7/month at AWS rates for 20k PUTs + 300k GETs/day).
- **Bytes dominate.** In production the object store is external, so **egress and endpoint latency are a real budget line**, not zero. "External" does not imply WAN: an in-region private endpoint is a different cost and latency class from internet S3, and the deployment must state which it is. The breaker keys on **store-side errors and install p95**, not the service's own probe (a hanging GET stalls builds 30 s before fallback). The design cluster's in-cluster MinIO has neither egress cost nor representative latency — measure production with a 100 × 2 GB burst.
- `dcache` memory unknown; plan 16 GiB/leader and measure. Pool graphs inherit the engine's pathology: restore time linear in DB size (#15013) and e-graph eq-class walks under a global lock (#15049, fix #15050 still open).

## The honest ceiling (why this is a go/no-go, not a sure thing)

Profitable hits = ops that are not per-session keyed and whose outputs are snapshot parts.

- Image pulls / package-install layers: near 100% addressable — but Zot covers them.
- Source-dependent builds/tests: addressable only with uniform umask (#15034) and identical source — and their **incremental** benefit is lost because the compiler cache lives in a cache volume. A one-line change re-runs the full compile, which can be *slower* than today's warm node-local engine.
- Modules with git `clients` (#15014), filtered `dagger check` selections (#15044), `PRIVATE` mounts (#13060): 0% until upstream fixes land.
- Correctness hazards on cross-engine hits: #14913 (env var dropped from key — wrong hit), #12883 (URI secrets — failed build). Both must be confirmed fixed in the pinned build before Tier 1 goes live.

**Two gates (round-3 finding 18):** **Gate A (pre-build, no service):** the discovery capture (do call spans reach a self-hosted `DAGGER_CLOUD_URL`?) plus a critical-path opportunity estimate from existing telemetry — per-pipeline `would_hit`/`lost_keying` upper bounds and the share of wall time in cacheable ops. **Gate B (post-prototype):** paired run against today's warm node-local baseline with the same topology, comparing p50/p95 wall time and failure rate. A live shared pool cannot be evidence for permission to write its first code. **This gate is still unmeasured, and discovery is unproven** (F5): no test has shown that call spans reach a self-hosted `DAGGER_CLOUD_URL`. Run that spike before writing service code.

## Phases

0. **Now, on v0.21.11 (1–2 wk):** Tier 2 (Zot pull-through, GOPROXY, sccache/Turbo on the object store — credentials from **Secrets, never env values that become part of the tool's cache key**, F63), enforce `umask 022`, inventory git-client modules / filtered checks / URI secrets / PRIVATE mounts, run the pairing-digest measurement (beta.25 shadow engine if v0.21 lacks the contract).
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
| R4 | Service graph doesn't scale (restore/eq-class stalls) | small pools, 7-day TTL, entry caps, accept cold restore (F62) |
| R5 | Store too slow or on ephemeral capacity | design cluster: MinIO off spot. Production: external S3 with a stated egress budget and an error-rate breaker (store errors > 2 % warn, > 10 % stop merges/offers); a hanging GET stalls builds 30 s before fallback |
| R6 | Exposure window > node lifetime | export at span end; publication-latency SLI |
| R7 | Telemetry dependency / token leak | CoreDNS NXDOMAIN or Cilium `toFQDNs` for `api.dagger.cloud` (a NetworkPolicy **cannot** match an FQDN, F59); reject `dag_` tokens at handshake |
