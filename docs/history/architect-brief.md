> **Historical design/review record, not current implementation status.** See [current design](../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Architecture brief — shared object-storage cache for Dagger on ephemeral nodes

You are the architect. Design, then attack your own design. Read the recon files listed at the end before concluding anything about the protocol.

## Situation

Dagger CI runs on k3s. Today: one Dagger engine pod per node as a DaemonSet, each with a node-local hostPath NVMe cache (~30 GB/node) on `registry.dagger.io/engine:v0.21.9`. Runners are ARC pods that set `DAGGER_RUNNER_HOST` to the engine Service and are pinned to the node whose engine serves them. That works only because the nodes are persistent.

The requirement changes: the compute nodes become **on-demand/spot instances that can be torn away with ~30 seconds notice** (or drained at any moment). Consequences:

- Node-local cache is worthless — it dies with the node. A cache tier must live outside every node.
- Many nodes (10-100+) must use the same cache **concurrently**, with no per-node partitioning and no single-writer assumption.
- A node can vanish mid-write. Anything that requires a graceful shutdown to be durable is disqualified (see dagger issues #13951, #13952, #14956, #15082: the engine's local dagql cache is all-or-nothing on restart and loses TBs even on clean shutdown).
- Cache must be **best-effort and fail-open**: a cache miss costs compute, never a wrong result or a failed build.

## Available infrastructure

- k3s on-prem, MinIO already deployed in-cluster (S3 API), plus the option of real S3-compatible object storage off-cluster (Backblaze/Wasabi/AWS).
- Zot OCI registry in-cluster (from the `zot-registry` skill work).
- Postgres (Patroni) available for metadata if needed.
- Dagger stable line v0.21.11; 1.0.0-beta.25 exists. The 1.0-beta line introduced a **remote cache protocol** (self-hosted, token-based, `_EXPERIMENTAL_DAGGER_REMOTE_CACHE_URL`), whose server-side logic is open source; the older `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` S3 export path was dropped in v0.21.x (#13405).

## What the design must answer

1. **Which tier carries the cache**: (a) Dagger 1.0-beta remote cache protocol against a self-hosted service, (b) BuildKit-style cache export to S3/registry (magicache-era, if still functional anywhere), (c) an OCI registry as the CAS, or (d) a hybrid. Justify against the 30-second-teardown constraint and against the known upstream bugs (#14657 no-exec Container offer, #14884 remote-cache OOM, #15013/#15049 local-cache pathology).
2. **Server design** if (a): stateless front-end replicas + object store as the only durable state; what indexes are needed (key→manifest, chunk→object) and which store can provide them with the consistency required; auth/token model; per-session isolation vs cross-repo sharing.
3. **Chunking and durability on a 30s kill**: streaming write-through, chunk granularity, multipart abort semantics, idempotent retries, and how a half-written entry is prevented from ever being read. State the invariant explicitly (e.g. "a manifest object is only visible after every chunk it references is durable").
4. **Concurrency**: 100 writers, same content, simultaneously. Dedupe without locks; compare-and-swap on S3 (`If-None-Match: *`); what happens on lost races; per-key sharding; hot-key behaviour; request-cost math (PUT/GET/$ per 1k) at expected volume.
5. **GC and TTL**: object lifecycle rules vs application-level mark-and-sweep; how to know a chunk is unreferenced when manifests arrive out of order; per-repo quotas; cost of scanning.
6. **Hit-rate ceiling**: what fraction of a Dagger run is even addressable in a shared cache given the issues found (umask bits #15034, per-session keying #15014/#15044, secrets #12883, registry auth #13898). If cross-session hits are structurally broken for some workloads, say so and quantify.
7. **Failure and degradation**: cache service down, S3 throttling, token expiry, partial outage; what the nodes do (fail-open), and how to measure it.
8. **Migration and rollout**: how to run this alongside the current node-local DaemonSet, then remove it; Helm chart shape; what to do about engine version choice (v0.21.11 vs beta.25 vs waiting).
9. **What you would NOT build**, and why — including an explicit "if I only had a week" version.

## Output contract

- Write the full design to `<HISTORICAL_SCRATCH>/architecture.md`.
- Structure: Decision (one paragraph, unambiguous), Diagram (ASCII), Invariants, Components with concrete config/API sketches, Request flows (cold miss / hit / concurrent duplicate / node killed mid-upload), Failure modes table, Cost model with numbers, Open questions, Rejected alternatives.
- Cite the recon files and any upstream file:line or issue you rely on. Do not invent protocol endpoints — if the recon says unknown, mark it UNKNOWN and design around it.
- Then a second section, `## Red team`, where you attack your own design as the reviewer: list at least 8 ways it fails, ordered by probability × damage, each with the mitigation you would add.
- Chat reply: max 250 words — the decision, the three riskiest assumptions, and anything you need from the human.

## Recon files (read these first)

- `<HISTORICAL_SCRATCH>/recon-protocol.md` — Dagger remote cache protocol from source
- `<HISTORICAL_SCRATCH>/recon-prior-art.md` — prior art, S3 semantics, upstream status
