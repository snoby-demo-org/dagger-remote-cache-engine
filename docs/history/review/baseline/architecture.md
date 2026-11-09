> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Architecture: shared object-storage cache for Dagger on spot/ephemeral nodes

Status: design proposal, not implemented or tested. Date: 2026-11-05.
Sources: `recon-protocol.md` (cited as **RP §n**, with its pinned `file:line` at dagger commit `7e9eaac`, which has no remote-cache diff from `v1.0.0-beta.25`), `recon-prior-art.md` (cited as **PA §n**), and saved issue bodies `i_<n>.json` in this directory (cited as **#n**). Anything the recon could not establish is marked **UNKNOWN**. No protocol endpoint here goes beyond RP §2.

---

## 1. Decision

Build a **hybrid (d)**, with one tier that carries the Dagger operation cache and one that carries tool-level caches:

- **Tier 1 (Dagger op cache): the Dagger 1.0-beta remote-cache protocol (a), against a self-hosted Go service called `dcache`.** The service imports `dagql`+`core` pinned to the exact engine commit. It runs as **one fenced metadata writer per trust pool** on non-spot nodes. Layer bytes go **directly between engines and in-cluster MinIO** through presigned URLs. Durable state lives in three places: immutable digest-keyed blobs in MinIO, a Postgres blob ledger (digest → state), and create-only SQLite graph checkpoints in MinIO.
- **Tier 2: tool-level remote caches.** Zot as a pull-through mirror for base images, plus a Go module proxy, sccache/Turbo/Nx-style caches on MinIO, injected into `withExec`. Tier 1 structurally **cannot** share cache volumes across engines (RP §13.2 #10) and misses every per-session-keyed call (§7 below). Tier 2 covers those gaps and also works on today's v0.21.11.

The engines move to a **pinned `v1.0.0-beta.25`** on spot nodes with throwaway local disk. **v0.21.x cannot take part in Tier 1**: the protocol exists only in 1.0-beta, and the v0.21 S3 export was removed (PA §1–2, #13405).

Nothing on a spot node is ever treated as durable. A part becomes visible to other engines only after a service on a persistent node has re-hashed every blob it references.

**Gate:** Phase 0 (§9) measures the addressable hit rate from telemetry *before* `dcache` is built. If the measured ceiling is below the threshold in §9, ship only Tier 2 plus the "week version".

## 2. Diagram

```
 SPOT / EPHEMERAL NODES (10-100+, killed with ~30s notice)            PERSISTENT (non-spot) NODES
 ┌───────────────────────────────────────┐                     ┌──────────────────────────────────────────┐
 │ ARC runner pod ──DAGGER_RUNNER_HOST──►│                     │  dcache (StatefulSet, 2 replicas)         │
 │ dagger engine v1.0.0-beta.25 (DS)     │  WSS /v1/engine     │  ┌───────────────┐ leader (fenced epoch)  │
 │  - local NVMe/emptyDir = L1 scratch   │◄═══════════════════►│  │ WS session mgr│  per pool:             │
 │  - remoteCache.url = dcache           │  Basic(token:"")    │  │ export sched. │  dagql.NewCache(       │
 │  - DAGGER_CLOUD_URL = dcache OTLP     │                     │  │ offer/merge   │   WithBlobStore) +     │
 │                                       │  OTLP/HTTP          │  │ renew signer  │  SQLite WAL on PVC     │
 │                                       │──/v1/traces,/v1/logs►│  │ OTLP ingest   │                        │
 │                                       │                     │  └──────┬────────┘ standby: warm restore  │
 │                                       │                     └─────────┼───────────────▲────────────────┘
 │                                       │  presigned PUT/GET (Range)    │ verify(HEAD+   │ checkpoints
 │                                       │═══════════════════════════╗   │ sha256 stream) │ (create-only)
 └───────────────────────────────────────┘                           ║   ▼                │
                                                              ┌───────╨────────────────────┴──┐  ┌───────────────┐
  Tier 2 (also from engines / withExec):                      │ MinIO (persistent disks)       │  │ Postgres      │
   Zot pull-through mirror, GOPROXY, sccache/Turbo/Nx ───────►│  pools/<pool>/blobs/sha256/<h> │  │ (Patroni)     │
                                                              │  pools/<pool>/ckpt/<epoch>/<n> │  │ tokens, pools │
                                                              │  tier2/<tool>/...              │  │ blob ledger   │
                                                              └────────────────────────────────┘  │ leader epoch  │
                                                                                                  └───────────────┘
```

## 3. Invariants

- **I1 (publication).** A part is registered with `SetStoredPart` only after every blob in its ordered chain has ledger state `verified`, so only then does it become eligible for `StoredBundle`/`PlaceOffer`/renew GET URLs. `verified` means the service itself did a `HEAD` size check and streamed a sha256 of the object in MinIO, and both match the OCI descriptor. `SetStoredPart` checks nothing itself (RP §8.2, `dagql/cache_stored_parts.go:30-89`), so this gate lives in `dcache`.
- **I2 (engine claims are hints).** `uploaded.done` and a 2xx PUT are never proof of durability (RP §5.3, `engine/remotecache/session.go:307-321`). Bytes that exist only on a node do not exist.
- **I3 (no torn reads).** A blob key only ever holds bytes that hash to its name. This rests on three layers: S3 single-object PUT atomicity (an aborted PUT leaves no object; PA §6.1), I1, and the engine's own digest and size check on import (RP §7.2, `engine/snapshots/pull.go:433-473`).
- **I4 (no URL for unverified bytes).** GET URLs are minted only for ledger-`verified` digests in the caller's pool. Once a digest is `verified`, no PUT URL is issued for it again until it is deleted.
- **I5 (GC fence).** A blob is deleted only after one atomic conditional `UPDATE … SET state='deleting' WHERE state='verified' AND last_ref_at < now()-G AND last_url_mint_at < now()-G`, and only if it was absent from `LiveBlobs()` at the sweep's graph snapshot. Publication does `UPDATE … SET last_ref_at=now() WHERE state='verified'` and aborts if zero rows change. Here G ≥ max GET-URL TTL + 30 s stall + skew.
- **I6 (single writer).** Each pool has exactly one metadata writer, holding a Postgres-fenced epoch. Checkpoint objects are create-only under `ckpt/<epoch>/`, and the "current checkpoint" pointer is advanced by compare-and-set on `(pool, epoch)`. A deposed leader cannot publish (RP §10.5, §12.4).
- **I7 (fail-open).** Every `dcache`/MinIO/Postgres failure maps to one of: no merge, no offer, `renewed{unavailable:true}`, or no export. The engine then computes the result itself (RP §4.2 "engine operation does not wait for the service"; RP §7.2). Known exception: #12883 (§7).
- **I8 (trust).** Everyone who can write metadata to a pool is mutually trusted (RP §3.2, `dagql/cache_value_codec.go:12-23`). Untrusted code never runs on an engine that holds a read-write token.
- **I9 (version lock).** The `dcache` build's dagql commit equals the engine commit. Any other `hello.engineVersion` gets `welcome` and then **quarantine**: no commands. Rejecting the socket would only cause an endless reconnect loop (RP §3.1).

## 4. Components

### 4.1 Engine (spot DaemonSet), Helm values

These use the generic chart fields (RP §11.2). Magicache is irrelevant here (RP §11.2, PA §2).

```yaml
engine:
  kind: DaemonSet
  image: { ref: registry.dagger.io/engine:v1.0.0-beta.25@sha256:<pin> }   # digest UNKNOWN until pulled
  configJson: |
    {"remoteCache":{"url":"https://dcache.dcache.svc:8443"}}
  env:
    - name: DAGGER_CLOUD_TOKEN            # Basic username, empty password (RP §3.1)
      valueFrom: { secretKeyRef: { name: dcache-engine-token-trusted, key: token } }
    - name: DAGGER_CLOUD_URL              # engine events/OTLP go here, NOT to api.dagger.cloud (RP §11.1)
      value: https://dcache.dcache.svc:8443
  hostPath: { dataVolume: { enabled: false } }   # local state is scratch; use emptyDir on NVMe
  volumes: [ { name: data, emptyDir: { sizeLimit: 30Gi } } ]
  terminationGracePeriodSeconds: 25      # nothing local worth saving; #13951/#14956/#15082
  tolerations: [ { key: node.kubernetes.io/spot, operator: Exists, effect: NoSchedule } ]
magicache: { enabled: false }
```

- A NetworkPolicy blocks engine egress to `api.dagger.cloud`. This guards against telemetry paths that `DAGGER_CLOUD_URL` does not redirect (RP §11.2 caveat: a per-session Cloud URL can take precedence).
- Fork/untrusted PR jobs get a **separate engine DaemonSet** on a separate node label, with a read-only pool token (see §4.4).
- Whether the engine's graceful-stop budget can be configured: **UNKNOWN**. #14956 names `defaultGracefulStopTimeout` in `cmd/engine/stop_budget.go`, and the recon does not say whether it is configurable.

### 4.2 `dcache` service (Go, persistent nodes)

| Subcomponent | Responsibility | Grounding |
|---|---|---|
| WS endpoint `/v1/engine` | Basic auth before upgrade; per-socket state machine; separate peer message-ID spaces; lossless uint64 JSON; ≤64 MiB frames; answer engine pings; bounded outbound command queue | RP §2, §4 |
| OTLP receiver `/v1/traces`, `/v1/logs` (`/v1/metrics` accepted and dropped) | Decode only spans with `dagger.io/cache.contract=="1"`; feed `AttachRemoteHolding`/`UpdateRemoteHolding`; handle `engine.start/prune/share/stop` log bodies (JSON inside an OTLP string) | RP §2.1, `engine/telemetryattrs/attrs.go:595-942` |
| Pool graph | `dagql.NewCache(sqlitePath, WithBlobStore())` per pool; `MergeValues`, `SetStoredPart`, `StoredBundle`, `PlaceOffer`, `EquivalentEntries`, `CollectRemoteHoldings`, `DropStoredParts`, `LiveBlobs` | RP §8 |
| Export scheduler | Choose roots from executed/complete facts; dedupe by recipe equivalence and pending-export set; ≤4 exports in flight per engine (engine limit); ≤32 roots per export (companion-service policy in #14761, not a protocol constant) | RP §5.2, §13.1 |
| Upload coordinator | Ledger reservations; presign PUTs; post-upload verify; publish | §5 below |
| Renew signer | Verify HMAC renewal key → presign GETs for that chain only; target reply < 200 ms (engine deadline is 2 s) | RP §5.6, `dagql/cache_part_renewal.go:17-26` |
| Checkpointer | Every 60 s, and on graceful leader stop: `Checkpoint` → upload `ckpt/<epoch>/<seq>.db` with `If-None-Match: *` → CAS pointer in Postgres | RP §9.2, PA §6.1 |
| Sweeper | GC per §6 | RP §8.4, §12.4 |
| Reaper | `ReleaseRemoteCache(cacheID)` once a cacheID has been disconnected > 15 min. A spot engine's cacheID never comes back because its disk is gone | RP §8.1, `dagql/cache_remote_entries.go:1062-1101` |

**Why not stateless front-end replicas.** The brief asks for stateless front ends with the object store as the only durable state. That does not hold for metadata. The dagql graph is an in-memory e-graph with a SQLite checkpoint, the wire has no fencing or lock primitive, and the recon's recommendation is one authoritative writer per pool (RP §10.5, §12.4). The **byte path is stateless**: the service never proxies blob bytes. To scale out, add pools and spread them across leaders; do not add replicas per pool. A standby restores the latest checkpoint at failover and loses only metadata since that checkpoint. That loss is cache-only and fails open.

### 4.3 Indexes and which store holds them

| Index | Store | Why |
|---|---|---|
| recipe/key → value → parts → chain (the "manifest") | dagql pool graph (SQLite) with checkpoints in MinIO | Only dagql's codecs can derive recipe identity (RP §6.4: "storing arbitrary JSON plus hashes does not implement this protocol") |
| digest → object / state | Postgres `blob_ledger` | Needs transactional state transitions (I1, I5). S3 conditional writes could do it with ETags, but Postgres already exists and is simpler |
| token → pool, mode | Postgres `tokens` (sha256 of token) | RP §3.2: map an opaque token to a trust domain server-side |
| pool → leader epoch, current checkpoint | Postgres `pool_leader` | Fencing (I6) |

```sql
CREATE TABLE blob_ledger (
  pool text, digest text, size bigint,
  state text CHECK (state IN ('pending','verified','deleting','failed')),
  lease_until timestamptz, uploader_cache_id text, export_id text,
  verified_at timestamptz, last_ref_at timestamptz, last_url_mint_at timestamptz,
  PRIMARY KEY (pool, digest));
CREATE TABLE tokens (token_sha256 bytea PRIMARY KEY, pool text, mode text CHECK (mode IN ('rw','ro')),
  revoked_at timestamptz);
CREATE TABLE pool_leader (pool text PRIMARY KEY, epoch bigint, holder text, lease_until timestamptz,
  ckpt_key text);
```

### 4.4 Auth and token model

- Tokens look like `dck_<random 32B>`. Postgres stores their hash. The `dck_` prefix exists so a real Dagger Cloud token (`dag_…`) can never be mistaken for one. The engine itself validates no format (RP §3.1).
- **Pool = trust domain.** Example: `org-internal` (rw) for main-branch and trusted-branch engines, and `org-internal` (ro) for the fork-PR engine pool. The service gates read-only by *never issuing `export`/`upload`* to that socket. Merge, offer and renew are still served. `hello.cacheID` is never used to pick a tenant (RP §3.1).
- **Cross-repo sharing** happens inside a pool. **No cross-pool dedupe.** Keys are prefixed by pool, so the "is digest X already stored?" check cannot become an existence oracle across tenants.
- **Renewal key** = `base64(pool | donorNumber | sha256(ordered layer digests) | exp) + HMAC-SHA256(poolKey, …)`. The format is application-defined (RP §5.6). On `renew` the service checks the HMAC, the pool, that the layers match the hashed list, and that every digest is `verified`, then presigns GETs for those layers only.
- **Revocation:** close with 4001 and refuse future handshakes. The engine keeps reconnecting with 1–30 s backoff (RP §3.1, §4.2). At 100 engines that is ≤ ~3–100 handshakes/s, which is cheap to reject.
- **URL TTLs:** PUT 15 min. GET 1 h. An expired S3 presign returns 403, which triggers renewal (RP §7.2: renewal happens on 401/403/404/410).

### 4.5 Object layout (application-defined, not protocol; RP §12.1)

```
pools/<pool>/blobs/sha256/<hex>          immutable layer blobs (compressed bytes as exported)
pools/<pool>/ckpt/<epoch>/<seq>.db       SQLite checkpoint, create-only
tier2/<tool>/<namespace>/...             sccache/turbo/etc., owned by those tools
```

Lifecycle rules: `AbortIncompleteMultipartUpload` after 1 day. These uploads come from service-side checkpoint/copy only; the engine never does multipart (RP §5.3). Old `ckpt/` objects expire after 7 days. **Blobs have no age expiry**, because the bucket TTL is not the value TTL (RP §12.4).

## 5. Chunking, durability on a 30-second kill, concurrency

**Chunk granularity is chosen by the engine and cannot be changed.** The unit is one whole immutable snapshot-layer blob per OCI descriptor. There is no chunking, multipart, resumable upload or retry in the engine's PUT path. Each upload is one raw PUT with a 1-minute idle bound, at most 8 concurrent per session (RP §5.3, §6.3, `engine/remotecache/remotecache.go:242-279`). So "streaming write-through" means: export each completed part as soon as telemetry shows it, not at session end (PA §7.2). The service controls *when* uploads happen, not their size.

- **Multipart abort semantics:** these do not apply on the engine path. A killed single PUT leaves no object (PA §6.1). There are no orphaned parts to bill.
- **Size cap:** AWS S3 and other S3 APIs cap a single PUT at 5 GiB. That cap is general S3 knowledge, not from the recon, so verify it for MinIO and B2/Wasabi. Descriptors over the backend cap are **left out of the URL map**, and their parts are never published. An upload may name a subset (RP §5.3).
- **Idempotent retries:** the engine cannot retry within an exportID (RP §5.3). The service retries by **re-exporting** after a backoff. Writing the same digest twice to the same key is idempotent by construction (I3).
- **Why PUTs go straight to the final key, with no staging plus copy:** the ledger, not object presence, decides visibility (I1, I4). Staging would double PUT and storage costs and gain nothing, because verification happens anyway.

**Concurrent duplicates without locks on S3.** There are no S3 locks and no `If-None-Match` on blob PUTs. The engine adds no headers, so a conditional presign cannot be honored (RP §2 PUT row), and identical bytes make it unnecessary. Dedupe happens at three levels:
1. **Recipe level:** the scheduler skips exporting a root when `EquivalentEntries` shows a stored value, or when an equivalent export is pending.
2. **Blob level:** `INSERT … ON CONFLICT DO NOTHING` puts a `pending` row with a 10-minute lease. Only the inserting export gets a PUT URL. Other exports leave that digest out of their URL map, and their parts **wait** on the pending row.
3. **Lost race:** two exports may both get PUT URLs, for example after a lease expiry. Both write identical bytes and S3 keeps one atomically. The ledger moves to `verified` once. `MergeValues` handles duplicate metadata: an unexpired local value is kept (RP §5.4).

**Hot keys (100 engines, same content, same moment).** Execution is not deduplicated across engines: all 100 run the operation, because the protocol has no on-miss lookup (RP §13.2 #3). Uploads *are* deduplicated, down to 1 export and 1 PUT per new blob. The read-side hot key is the real bottleneck: 100 fresh engines × a 2 GB layer is 200 GB of burst reads from MinIO (see §8). Per-key "sharding" doesn't help with a single object; that needs more MinIO bandwidth or a range-correct read-through cache (see Red team R5).

## 6. GC and TTL

Two layers, and neither uses bucket lifecycle on blobs:

1. **Graph-level TTL (values/roots):** the service sets retention `expiresAtUnix` on stored roots (RP §7.1). The default is 7 days since last served, refreshed whenever a merge or offer uses the root. Expired roots are pruned through dagql, so their closure becomes unreferenced.
2. **Blob mark-and-sweep (hourly):** live = `LiveBlobs()` (all stored-part blobs; RP §8.4) ∪ ledger `pending` with an unexpired lease ∪ `last_url_mint_at` within G. Candidates are `verified` rows not in live. Each candidate goes through I5's conditional UPDATE to `deleting`, then S3 `DELETE`, then the row is deleted. If renew later hits a missing object, run `DropStoredParts(digest)` and reply `unavailable` (RP §8.4).
   - **Out-of-order arrival:** a blob uploaded before its part is published is protected by its `pending` lease. If publication never happens, the lease expires, the row becomes `failed`, and the next sweep deletes it. A blob whose part was published after the sweep's snapshot is protected by the `last_ref_at` fence.
   - **Scan cost:** the sweep reads the ledger from Postgres and `LiveBlobs()` from memory, with no S3 LIST on the hot path. A weekly reconcile LISTs `pools/<pool>/blobs/`, about 1,000 LIST requests per 1 M objects, which is $0.005 at AWS rates (PA §6), to find objects missing from the ledger.
3. **Per-pool quota:** ledger `SUM(size)` per pool. Above the quota, evict roots by last-served time (LRU) until projected unreferenced bytes bring usage under 90%, then sweep. Shared blobs are not freed until every referencing part is gone, so quota enforcement is approximate.

## 7. Hit-rate ceiling: what is addressable at all

Every spot engine is a **new cache identity, and every CI job is a new session**. All Tier-1 value therefore comes from **cross-session, cross-engine** hits. Known breakage, from the saved issues:

| Issue (state in saved JSON) | Effect on cross-session hits | Scope / quantification from evidence | Controllable? |
|---|---|---|---|
| #15014 (open): module whose SDK scope declares git `clients` → `PerSessionInput` | **0%** for every call into that module | 275 s vs 105 s on the reporter's monorepo | Yes: use local/vendored clients, which are keyed by content |
| #15044 (open): filtered `dagger check` selection re-keys per session | **0%** for the filtered checks | All `__check_*` under the filter re-run | Yes: select via module setting, not filter |
| #15034 (open): host-dir permission bits in keys | **0%** between checkouts made under different umasks | 271/272 checks re-ran | Yes: force `umask 022` in every runner image and cache warmer |
| #13060 (open): `PRIVATE` cache mounts add a random nonce | **0%** for those ops | Every evaluation is unique | Partly: avoid `PRIVATE` sharing |
| RP §13.2 #10: cache volumes are engine-local, not part of the protocol | **0% sharing of `withMountedCache` contents** (Go build cache, npm cache, etc.) | On a fresh engine, every cache volume starts empty | Only via Tier 2 |
| #13898 (closed): `withRegistryAuth` changed identity | Was 0% for authenticated pulls and everything downstream | Fix version **UNKNOWN**; verify on beta.25 | Verify |
| #12883 (open): URI secrets created inside a module | A cross-session hit **fails the build** (`secret not found`), which is worse than a miss | Named `SetSecret` is unaffected. Behavior across engines via remote merge is **UNKNOWN** | Yes: ban `cmd://`/`env://` secrets created inside modules |
| #14913 (closed): `.env` value missing from the cache key (v1 regression) | **Wrong cache hit** (a correctness bug) | Fixed version **UNKNOWN**; must be confirmed fixed in the pinned build | Verify (blocker) |
| `cache.outcome=uncached` (DoNotCache) | 0% by design | RP §2.1 | No |

**Ceiling statement.** With today's evidence no single percentage can be honest; workload data is needed. The structural ceiling can still be stated: **addressable = ops that are not per-session keyed and whose outputs are snapshot parts.** For a typical pipeline:
- **Image pulls and package-install layers:** near 100% addressable. Tier 2 (Zot mirror) covers these too.
- **Source-dependent `withExec` builds/tests:** addressable only with uniform umask and identical source. Their **incremental** benefit is lost, because the build cache lives in cache volumes. A one-line change re-runs the whole compile with an empty build cache, which can make it *slower* than today's warm node-local engine.
- **Modules with git clients, filtered checks, private mounts:** **0%** until the upstream fixes land.

**How to measure it before building (Phase 0).** Use `dagger.io/cache.pairing_digest` (self digest *excluding* implicit inputs; `engine/telemetryattrs/attrs.go:710-718`). Across sessions, count spans with `outcome=executed` where some earlier session had the same `self_digest`+`structural_inputs`. Those are the hits a perfect shared cache would get. Separately count spans with the same `pairing_digest` but a different `self_digest`; those are lost to per-session keying. Weight both by span duration. This gives "seconds of compute a shared cache could save" per job.

## 8. Cost model

The workload numbers are **assumptions** for the human to replace: 100 nodes; 2,000 jobs/day; 20,000 new unique blobs/day after dedupe, averaging 30 MB (600 GB/day written); per job 150 blob GETs including range reopens and 2 GB downloaded (300 k GET/day, 4 TB/day read). Rates are AWS S3 Standard us-east-1 from PA §6: PUT $0.005/1k, GET $0.0004/1k, $0.023/GB-month.

| Item | Calculation | AWS S3 | MinIO in-cluster |
|---|---|---:|---:|
| PUT (engine uploads) | 20k × $0.005/1k | $0.10/day | $0 (IOPS only) |
| HEAD + verify GET | 40k × $0.0004/1k | $0.016/day | $0; 600 GB/day re-read ≈ 7 MB/s average |
| GET (engine reads) | 300k × $0.0004/1k | $0.12/day | $0 |
| Checkpoints (pools × 1/min) | 5 × 1,440 × $0.005/1k | $0.036/day | $0 |
| **Requests total** | ≈ $0.27/day | **≈ $8/month** | — |
| Storage (7-day working set ≈ 3 TB after GC) | 3,000 × $0.023 | $69/month | 3 TB × EC overhead (~1.5× **assumed**) of disk |
| Egress to on-prem engines | 4 TB/day × 30 × egress rate | **UNKNOWN in recon**; at typical internet egress pricing this is thousands of $/month | $0 |

Conclusions:
- Layer blobs are large, so **request costs are negligible** (≈ $8/month), unlike per-file caches (compare the PA §6 example at $1,110/month).
- **Bytes moved dominate.** An off-cluster S3 as the *primary* read tier is ruled out by egress and latency. Off-cluster storage is acceptable only as an async DR copy of `ckpt/` plus hot blobs, if at all.
- **MinIO bandwidth is the real budget.** A cold-start wave of 100 engines × 2 GB is 200 GB. At 10 Gb/s aggregate (≈1.25 GB/s) that is ≈160 s of saturation. At 25 Gb/s it is ≈64 s.
- `dcache` compute: one leader per pool. Memory is **UNKNOWN**. #15013 shows 7.5 GB SQLite / 314 k entries for one engine shard; plan 16 GiB per leader to start and measure. OTLP ingest CPU is **UNKNOWN** and must be measured in Phase 1.

## 9. Request flows

**Cold miss (nothing stored).**
1. The engine starts: WS `hello{cacheID,generation=1,…}` → `welcome{}` (RP §5.1). The service sends a warm `merge` (see Hit). Here it has nothing to send.
2. The job runs and the engine executes `X`. Its span (`outcome=executed`, `cache.parts`, `result.id`) reaches `/v1/traces`. `AttachRemoteHolding` runs.
3. Once `X`'s parts are complete (a lazy-eval span, or `engine.share`), the scheduler checks for no equivalent stored or pending export, then sends `export{roots:[n], outputs:[…]}` (RP §5.2).
4. `exported{exportID,bundle}`. The service prepares outside the lock, `CommitValueMerge`s (RP §8.2), and lists the chain digests.
5. For each digest: verified → omit; pending by another export → omit and wait; else insert `pending` and presign a PUT. It sends `upload{exportID,urls}` **within 5 s**, always, sending an empty map if nothing is needed, so engine export slots are never left held (RP §5.2: 4 slots).
6. `uploaded{done,failed}`. For each `done`: HEAD size, then stream sha256, then `verified`. When all blobs of a part are verified: conditional `last_ref_at` update (I5), then `SetStoredPart`. Failed or mismatched blobs → `failed`, part unpublished, re-export with backoff.
7. The next checkpoint makes the published part survive leader failover.

**Hit.**
- **(a) Pre-merge.** A fresh engine connects. The service picks the pool's hottest live roots, limited to ≤ 48 MiB of merge payload and ~N roots (N **UNKNOWN**, tune it). It runs `StoredBundle`, attaches a GET URL and HMAC `renewalKey` to each chain, and sends `merge` (RP §8.3). `merged` → `ApplyMergedReply`. The job's call hits engine-local metadata (`cache.hit.remote="true"`). On demand the engine does a range GET to MinIO, verifies the digest and installs. The span carries `cache.offer.bytes` (RP §2.1).
- **(b) Live offer.** The engine creates a lazy entry that has a stored equivalent. The service runs `PlaceOffer` and sends `offer`. `accepted` → wait for completion evidence. `executionStarted` → too late, so log it as "offer lost race" (RP §5.5).
- **(c) URL expired.** The GET gets 403, the engine sends `renew`, and the service replies in under 2 s with fresh GETs, or `unavailable` if the blob is gone. In the second case it also runs `DropStoredParts` (RP §5.6, §8.4).

**Concurrent duplicate (100 engines, same new layer).** All 100 execute. The scheduler sees 100 equivalent executed facts and exports from the **first** reporting engine only; the other 99 are recorded as candidate donors. If engine #1's upload fails, the scheduler picks the next donor whose holding is still live. If two exports race past the pending check, the blob ledger allows a single `pending` owner, the other upload omits that digest, and its part waits on the same row. Result: 1 PUT per new blob, at most a few duplicate metadata merges (harmless, RP §5.4), and 0 S3 conditional writes needed.

**Node killed mid-upload (30 s notice or none).**
- SIGTERM: the engine shuts down within 25 s, or SIGKILL with no notice at all. Both cases give the same outcome.
- In-flight PUTs are cancelled and S3 creates no object. The WS drops; the engine-side export providers and exportID die with the session (RP §4.2).
- The service marks the engine's sessions as ended (`EndRemoteProcess`). `pending` rows for its uploads expire after the 10-minute lease and become `failed`. Parts waiting on those rows are unpublished. Metadata already merged at step 4 stays in the graph as a value **without stored parts**, which is harmless: engines get metadata with no offered bytes and compute it, or a later donor fills it in.
- Already-`verified` parts from that engine are unaffected. Fifteen minutes later the Reaper calls `ReleaseRemoteCache(cacheID)`.
- **Loss bound:** only work whose publication had not finished. The exposure window (span end → part verified) is a primary SLI (§10).

## 10. Failure modes

| Failure | Engine behavior (per recon) | dcache behavior | Measure |
|---|---|---|---|
| dcache down | Reconnect 1→30 s backoff ±20%; builds proceed uncached (RP §4.2). OTLP export failures: blocking behavior **UNKNOWN**, believed bounded and dropping (RP §13.2 #4) | Standby takes the lease (≤30 s) and restores the checkpoint. #15013 suggests restore time grows with DB size; measure | `engine_sessions`, failover duration |
| Leader split-brain | — | Fenced epoch; create-only checkpoints; CAS pointer (I6) | Epoch changes; CAS conflicts |
| MinIO refuses (5xx/conn refused) | GET non-401/403/404/410 → source exhausted → compute (RP §7.2). PUT failure → `failed` | Health probe fails → stop merges, offers and exports for the pool (no point offering unreadable bytes) | MinIO error rate; `renew_unavailable` |
| **MinIO hangs / slow** | **Each demanded part stalls up to 30 s before fallback** (RP §7.2), so builds get *slower* than cold | Health probe uses p99 latency, not just liveness; trip the breaker on p99 > 2 s | p99 GET latency; offer-install duration |
| S3 throttling (503 SlowDown) | Same as refuse, for GET. PUT `failed` | Back off the export rate per pool | 503 counts |
| Postgres down | — | No verify/publish → stop exports. Keep serving merges/renews for already-published parts (no ledger write needed except `last_url_mint_at`, which buffers in memory; GC paused) | Ledger write errors |
| Token revoked / expired | Close 4001 → endless reconnect (RP §3.1) | Fast 401 at handshake | Handshake 401 rate |
| Engine version mismatch | Connected, idle | Quarantine (I9) | `quarantined_sessions` |
| OTLP overload / dropped spans | Engine drops spans at its bounded queue (RP §13.2 #4) | Fewer exports (miss only); ingest sheds load per pool | Dropped-span counters, ingest lag |
| Engine never gets `upload` | 4 export slots held, chains pinned on a 30 GB disk (RP §5.2) | 5 s upload watchdog per exportID | `export_slot_age` |
| Wrong bytes at a key | Digest check fails → source exhausted → compute (RP §7.2) | Verify catches it before publish; renew-time 404 → `DropStoredParts` | `verify_mismatch` (alert: should be 0) |

**SLIs:** remote-hit seconds saved per job (Σ duration of spans with `cache.hit.remote="true"` against the executed-time baseline); publication latency p50/p95 (span end → part verified); share of exports lost to disconnect; fail-open events (renew unavailable, offer install failures); job duration against the node-local baseline.

## 11. Migration and rollout

0. **Phase 0, now on v0.21.11 (1–2 weeks).** Ship Tier 2: Zot pull-through, GOPROXY, sccache/Turbo on MinIO. Enforce `umask 022` in runner images (#15034). Inventory modules for git `clients` (#15014), filtered checks (#15044), URI secrets (#12883) and `PRIVATE` mounts (#13060). Do the pairing-digest analysis of §7 from current telemetry, assuming v0.21.11 emits the `cache.*` contract; **UNKNOWN**, as the contract is cited from the beta source, and if v0.21 lacks it, do the analysis on a beta.25 shadow engine. **Go/no-go:** build `dcache` only if addressable compute is ≥ 30% of job wall time on the top pipelines, given Tier 2 (the threshold is a proposal).
1. **Phase 1: compatibility.** Port modules to the 1.0-beta engine; breaking changes from v0.21 → 1.0 are **UNKNOWN** in the recon. Confirm that #14913 and #13898 are fixed in beta.25, that #14884 (merged 2026-10-27) is in beta.25 (**UNKNOWN**: the tag's date relative to that merge is not in the recon), and the no-exec Container path (#14657; RP §13.1 says a fix appears present at the pinned revision).
2. **Phase 2: build `dcache`** with the RP §12.5 acceptance suite plus chaos tests: kill an engine mid-PUT, kill the leader mid-checkpoint, GC racing a publish, a hanging MinIO.
3. **Phase 3: parallel run.** Deploy a new engine DaemonSet (`dagger-engine-ephemeral`, beta.25) on a spot node pool labeled `dagger-tier=ephemeral`, plus a new ARC runner scale set with the same runner→node pinning as today. The current v0.21 DaemonSet and its runners stay untouched on persistent nodes. Shift repos one at a time and compare job duration and failure rate. Kill switch: remove `remoteCache.url`, or quarantine the pool server-side, which makes engines idle-connected without a rollout.
4. **Phase 4: retire** the hostPath DaemonSet once ephemeral p50/p95 job time is within the agreed bound of baseline for 2 weeks.

**Helm chart shape (`dcache`):** StatefulSet (2 replicas, leader via Postgres lease), PVC (SQLite working copy; rebuildable from checkpoint), Services `dcache:8443` (WSS + OTLP on the same listener, path-routed), Secrets (MinIO access key scoped to `pools/*` and `ckpt/*`, pool HMAC keys, Postgres DSN), ConfigMap (pools, quotas, TTLs, export policy), PDB `minAvailable: 1`, nodeAffinity to non-spot nodes, ServiceMonitor.

**Engine version choice:** v0.21.11 cannot use Tier 1. **beta.25, pinned by digest**, with `dcache`'s `go.mod` pinned to the same commit, upgraded in lockstep and only after the contract test suite passes. Do not wait for 1.0 GA as a plan: its date is **UNKNOWN**. Tier 2 has no version dependency.

## 12. What I would NOT build

- **No chunking or multipart layer.** The engine does single raw PUTs of whole layers (RP §5.3), so there is nothing to plug one into.
- **No multi-writer metadata, consensus, or Postgres reimplementation of the graph.** The dagql codecs are the recipe identity (RP §6.4, §10.5).
- **No non-Go reimplementation** (RP §6.4).
- **No cross-pool dedupe** (I8; it would create an existence oracle).
- **No shutdown-flush logic or local-cache persistence to a PVC or network volume.** Unclean stops wipe everything (#14956), and even clean ones do (#13952, #15082). A 30 s kill is always unclean.
- **No distributed execution singleflight** across engines. The protocol has no hook for it.
- **No BuildKit S3 export or OCI-registry-as-Dagger-CAS.** Neither path exists in the engine (§14).
- **No off-cluster S3 as the primary read tier** (egress, §8).

**If I only had a week:** do not build `dcache`.
- **(A, recommended):** keep 3–5 engines on **on-demand, non-spot** nodes with their NVMe caches. Move ARC runners, which are thin clients, to spot. Set `terminationGracePeriodSeconds` above the engine's stop budget (#13951). Add Zot pull-through and a GOPROXY.
- **(B, if engines *must* be on spot):** ephemeral engines with empty caches, plus Tier 2 (Zot mirror, GOPROXY, sccache/Turbo/Nx on MinIO, with cache tool env injected into `withExec`), plus the Phase 0 measurement. Accept full loss of the Dagger op cache, and use the measurement to decide whether Tier 1 is worth a quarter of engineering.

## 13. Open questions

1. **UNKNOWN:** real workload numbers: jobs/day, spot node lifetime distribution, layer size distribution, the share of time spent in `withMountedCache`-heavy steps.
2. Does MinIO run on non-spot nodes with persistent disks? What are its aggregate NIC bandwidth and erasure-coding layout?
3. **UNKNOWN:** MinIO behavior on `If-None-Match: *` for PutObject (checkpoints), exact 206 `Content-Range`, the single-PUT size cap, and presign with unsigned content-type (PA §6.5: "compatible does not mean identical").
4. **UNKNOWN:** whether v0.21.11 emits the `dagger.io/cache.*` contract (needed for Phase 0).
5. **UNKNOWN:** whether OTLP export to an unavailable `DAGGER_CLOUD_URL` can delay engine work.
6. **UNKNOWN:** whether all telemetry routes honor `DAGGER_CLOUD_URL`, or whether a per-session Cloud URL overrides it (RP §11.2).
7. **UNKNOWN:** whether the engine stop budget is configurable (#14956).
8. **UNKNOWN:** how big a warm merge should be, and its memory cost on a fresh engine (#14884 measured engine peaks of ~6 GB after the fix on a large module).
9. **UNKNOWN:** whether #15050 (the fix referenced in #15049) is merged, and whether the e-graph lookup pathology also affects the service-side pool graph.
10. Which repos share a trust pool? Do fork-PR builds exist?
11. **UNKNOWN:** how secrets and session resource handles (`sessionResourceHandle` in the envelope; RP §6.4) behave across engines via merge (#12883 interaction).

## 14. Rejected alternatives

- **(b) BuildKit-style export to S3 or a registry.** Dropped in v0.21 by a backend replacement, confirmed by a maintainer (#13405; PA §2). The BuildKit S3 exporter also overwrites one mutable manifest without CAS, so concurrent writers lose records (PA §4). It is an end-of-session bulk export, which fits a 30 s kill badly (#11328; PA §2).
- **(c) OCI registry (Zot) as the Dagger CAS.** The engine has no path to import or export its dagql cache through a registry in v0.21 or 1.0-beta. The only public tool (cbochs `remote-cache`, v0.21.0) explicitly exports cache volumes or directories at a final step, and its concurrency and 30 s-kill behavior are untested (PA §3). Zot stays in Tier 2 as a base-image mirror. An optional future use: seeding cache volumes from immutable per-commit tags published only by a single designated main-branch job (PA §5 "OCI registry as cache"). Untested, not in scope.
- **Network-attached PVC that follows the engine to a new node.** The dagql local cache is all-or-nothing and wiped on unclean stop (#14956), and a 30 s kill is unclean. Even a clean stop can lose it (#13952, #15082). Startup is linear in DB size (#15013).
- **Stateless multi-replica `dcache` sharing one bucket.** There is no wire-level fencing, and dagql/SQLite are single-process (RP §10.5).
- **Engines on persistent nodes as the end state.** This is the recommended *week version* and fallback, but it does not meet the brief's spot-compute requirement.

---

## Red team

Ordered by probability × damage. Each item gives the failure and then the mitigation.

**R1. The hit rate is too low to pay for `dcache`.** (Probability high, damage high: a quarter of engineering for little gain.) Cache volumes don't transfer (RP §13.2 #10). Per-session keying hits whole modules and checks (#15014, #15044). So the remote cache mostly restores image layers, which Zot already covers, and source builds lose their incremental build cache. Spot jobs could end up *slower* than today's warm node-local engines.
→ *Mitigation:* the Phase 0 pairing-digest measurement with a go/no-go threshold. Tier 2 ships first. Upstream bug reports or patches for #15014/#15044 are prerequisites, not hopes.

**R2. Beta protocol churn breaks `dcache` on every engine bump.** (High, medium.) Bundle v3 and envelope v5 have no negotiation; hello only reports a version (RP §13.2 #2). Mismatch fails open, but silently, to zero hits.
→ *Mitigation:* I9 quarantine with an alert on `quarantined_sessions > 0`. Lockstep pins. A CI contract test spins up two real engines against `dcache` on every upgrade PR (RP §12.5). Renovate is never allowed to bump the engine alone.

**R3. A cross-engine hit produces a wrong result or a failed build, breaking fail-open.** (Medium, very high.) #14913 (env var missing from the key: a wrong hit). #12883 (URI secret not found after a cross-session hit: a failed build). #13060 (service identity split). Remote merge shares *more* aggressively than a single engine does, so latent keying bugs surface.
→ *Mitigation:* treat a confirmed fix in the pinned build as a release blocker. Lint modules for in-module URI secrets. Shadow-verify a 1% job sample: run with the pool quarantined and compare artifact digests and test results. A per-pool kill switch. Count `secret not found` errors on jobs with `cache.hit.remote`.

**R4. The service-side dagql graph does not scale.** (Medium-high, high.) It is the same library as the engine, so it inherits e-graph eq-class walks under a global lock (#15049) and restore time linear in DB size (#15013, 2m36s for 7.5 GB), and the pool graph accumulates the recipes of *every* engine. Failover then takes minutes, and lock stalls delay merges past the point where offers are useful (`executionStarted`).
→ *Mitigation:* small pools, aggressive 7-day TTL, an entry cap per pool, a hot standby that tails checkpoints so restore happens before it is needed, and load tests with synthetic 1 M-entry graphs in Phase 2. Track #15050.

**R5. MinIO becomes the bottleneck or the outage.** (Medium, high.) A cold-start wave is 200 GB of burst reads (§8). A *hanging* MinIO makes every demanded part wait 30 s before fallback (RP §7.2), which is worse than having no cache. If MinIO actually sits on spot-eligible nodes, the "durable" tier dies with them.
→ *Mitigation:* pin MinIO to non-spot nodes. A latency-based circuit breaker that stops merges and offers. Stagger merge size by engine age. Optionally put GET URLs behind a range-correct read-through cache (URLs are opaque to the engine; RP §1), validated against the exact `Content-Range` rules (RP §2). Capacity-plan NICs from measured cold-start waves.

**R6. The exposure window is longer than spot lifetimes.** (Medium, medium-high.) Before bytes are durable, a part must get through OTLP batch delay, scheduler latency, 4 export slots, 8 PUTs and a 1-minute idle bound. Short-lived nodes, or a big layer at the end of a job, never publish, so the cache fills only with cheap early-job layers.
→ *Mitigation:* the publication-latency SLI. Prioritize exports by measured span duration ÷ bytes. Export at span end, not session end. On SIGTERM, the service stops scheduling new exports for that engine and lets in-flight ones finish, without touching the engine. Accept the loss and measure it.

**R7. The telemetry dependency fails or leaks.** (Medium, medium.) Discovery relies on OTLP (RP §1.3), so dropped spans mean no exports. Ingest volume can be huge (#14884 was a 15.9 GB engine OOM from telemetry structures). If some route ignores `DAGGER_CLOUD_URL`, telemetry carrying our `dck_` token, plus call metadata, goes to `api.dagger.cloud`. Or a runner's real Dagger Cloud token ends up on the engine and is sent to `dcache`.
→ *Mitigation:* a NetworkPolicy denying engine egress to `api.dagger.cloud`. Reject `dag_`-prefixed tokens at the handshake and alert. Ingest sheds load per pool. Fallback discovery: on `engine.share`/`engine.stop` and periodically, export recently executed roots even without full span facts.

**R8. GC deletes a blob that is in use, or publishes after a delete.** (Low-medium, medium.) The gaps are between `LiveBlobs()` and `SetStoredPart`, and between URL minting and deletion. The result is persistent 404s → renew → unavailable → misses. A bug in the fence would make this systematic.
→ *Mitigation:* I5 conditional updates on both sides. A grace G ≥ GET TTL + stall + skew. `DropStoredParts` on any renew-time miss. A chaos test that runs the sweeper at 1-second intervals during the acceptance suite. Start with sweeper delete disabled (mark-only) for the first 2 weeks.

**R9. Leader split-brain corrupts the pool graph.** (Low, high.) A paused (GC/stop-the-world) leader resumes after the standby took over and writes a checkpoint.
→ *Mitigation:* the epoch fence is checked inside the same Postgres transaction as the pointer CAS. Checkpoint keys are epoch-namespaced and create-only. The leader self-terminates when its lease is not renewed within half the TTL.

**R10. Cache poisoning across trust boundaries.** (Low-medium, high.) A runner from a less-trusted repo shares a node engine with a rw token and makes equivalence claims that trusted builds later consume (RP §3.2). Hashes authenticate bytes, not claims (PA §7.7).
→ *Mitigation:* a separate engine DaemonSet per trust pool on separate node labels, with runner scale sets pinned to them. A read-only token for fork PRs. Never share an engine across pools.

**R11. Stuck export slots fill the 30 GB local disk.** (Low, medium.) A `dcache` bug leaves exports without `upload`, so chains stay pinned (RP §5.2) and local GC can't reclaim them. That causes engine disk pressure, and the *build* may fail, which is not fail-open.
→ *Mitigation:* the 5 s upload watchdog (always send `upload`, even `{}`). Alert on `export_slot_age > 60 s`. Engine local GC tuned below the `emptyDir` limit.

**R12. Layers over the backend's single-PUT cap never cache.** (Low, low-medium.) Big monolithic layers (toolchains, datasets) are exactly the expensive ones.
→ *Mitigation:* exclude them from the URL map and count `skipped_oversize_bytes`. Push those workloads to split their layers or to Tier 2. Verify MinIO's actual cap (open question 3).
