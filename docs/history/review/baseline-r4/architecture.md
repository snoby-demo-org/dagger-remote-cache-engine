> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Architecture: shared object-storage cache for Dagger on spot/ephemeral nodes

Status: design proposal, not implemented or tested. Date: 2026-11-05.
Sources: `recon-protocol.md` (cited as **RP §n**, with its pinned `file:line` at dagger commit `7e9eaac`, which has no remote-cache diff from `v1.0.0-beta.25`), `recon-prior-art.md` (cited as **PA §n**), and saved issue bodies `i_<n>.json` in this directory (cited as **#n**). Anything the recon could not establish is marked **UNKNOWN**. No protocol endpoint here goes beyond RP §2.

---

## 1. Decision

Build a **hybrid (d)**, with one tier that carries the Dagger operation cache and one that carries tool-level caches:

- **Tier 1 (Dagger op cache): the Dagger 1.0-beta remote-cache protocol (a), against a self-hosted Go service called `dcache`.** The service imports `dagql`+`core` pinned to the exact engine commit. It runs as **one fenced metadata writer per trust pool** on non-spot nodes. Layer bytes go **directly between engines and the object store** through presigned URLs — in-cluster MinIO in the design cluster, **external S3 mounted outside the cluster in production** (F3). Durable state lives in three places: immutable generation-keyed blobs in the **object store** (in-cluster MinIO in the design cluster, external S3 in production), a Postgres ledger (`blob` + `blob_object_gc` + `pool_leader` + `part_cost`, `ledger.md`), and create-only SQLite graph checkpoints in the same object store.
- **Tier 2: tool-level remote caches.** Zot as a pull-through mirror for base images, plus a Go module proxy, sccache/Turbo/Nx-style caches on the object store (MinIO in the design cluster; the production Tier-2 backend is named per deployment), injected into `withExec`. Tier 1 structurally **cannot** share cache volumes across engines (RP §13.2 #10) and misses every per-session-keyed call (§7 below). Tier 2 covers those gaps and also works on today's v0.21.11.

The engines move to a **pinned `v1.0.0-beta.25`** on spot nodes with throwaway local disk. **v0.21.x cannot take part in Tier 1**: the protocol exists only in 1.0-beta, and the v0.21 S3 export was removed (PA §1–2, #13405).

Nothing on a spot node is ever treated as durable. A part becomes visible to other engines only after a service on a persistent node has verified every blob it references: authorization + existence + size for all, plus a hash check of a 1 % sample (§5.1), with the engine's own import digest check as the final gate.

**Gate:** Phase 0 (§9) measures the addressable hit rate from telemetry *before* `dcache` is built. If Gate A finds no addressable critical path, ship only Tier 2 plus the "week version" (`decision.md`).

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
 │                                       │                     │  └──────┬────────┘ standby: cold restore  │
 │                                       │                     └─────────┼───────────────▲────────────────┘
 │                                       │  presigned PUT/GET (Range)    │ verify(HEAD+   │ checkpoints
 │                                       │═══════════════════════════╗   │ HEAD + 1% hash)│ (create-only)
 └───────────────────────────────────────┘                           ║   ▼                │
                                                              ┌───────╨────────────────────┴──┐  ┌───────────────┐
  Tier 2 (also from engines / withExec):                      │ MinIO (persistent disks)       │  │ Postgres      │
   Zot pull-through mirror, GOPROXY, sccache/Turbo/Nx ───────►│  pools/<pool>/blobs/sha256/<h>/g<gen> │  │ (Patroni)     │
                                                              │  pools/<pool>/ckpt/<epoch>/<n> │  │ tokens, pools │
                                                              │  tier2/<tool>/...              │  │ blob ledger   │
                                                              └────────────────────────────────┘  │ leader epoch  │
                                                                                                  └───────────────┘
```

## 3. Invariants

- **I1 (publication).** A part is registered with `SetStoredPart` only after every blob in its ordered chain is `verified` **at its current generation**, re-read under the in-process publish mutex (`ledger.md` §5); only then is it eligible for `StoredBundle`/`PlaceOffer`/renew GET URLs. `verified` has exactly one definition (§5.1): authorization + existence + size for every blob, plus a hash check of a 1 % sample. `SetStoredPart` checks nothing itself (RP §8.2, `dagql/cache_stored_parts.go:30-89`), so this gate lives in `dcache`.
- **I2 (engine claims are hints).** `uploaded.done` and a 2xx PUT are never proof of durability (RP §5.3, `engine/remotecache/session.go:307-321`). Bytes that exist only on a node do not exist.
- **I3 (no torn reads; content correctness is weaker than R1 claimed).** S3 single-object PUT atomicity means a key holds *one complete* object, never a partial one (PA §6.1). Content correctness is not proven for every object: authorization + existence + size for all, a 1 % hash sample at the service (§5.1), and the engine's own digest check on import (RP §7.2, `engine/snapshots/pull.go:433-473`) as the final gate. A wrong same-size object on the unsampled path fails the receiving import rather than corrupting the graph.
- **I4 (no URL for unverified bytes).** GET URLs are minted only for ledger-`verified` digests in the caller's pool. Once a digest is `verified`, no PUT URL is issued for it again until it is deleted.
- **I5 (GC fence).** A blob is deleted only when it was **absent from `LiveBlobs()`** at the sweep's graph snapshot **and** `last_url_mint_at < now()-G`; both tests happen under the same in-process mutex `M` as publish, so no publish can interleave. The executable fence is `ledger.md` §3-§13, and it is the only normative copy: liveness **recomputed, never counted** (no `graph_refs`, no head mirror), generation-scoped object keys, an epoch predicate on every write (a stale writer changes nothing), publish = state re-read + `SetStoredPart` under `M` with no DB transaction, mint-as-stamp before the URL leaves the process, owner-fenced verify, `G = 2 × GET TTL`, an idempotent delete-recovery scanner, and restore by digest. R1's single-SQL publish, R2's `graph_refs` and R3's head-lock-across-`SetStoredPart` were each wrong for a different reason (round-2 findings 2/6, round-4 findings 1–8); do not reintroduce any of them.
- **I6 (single writer).** Each pool has exactly one metadata writer, holding a Postgres-fenced epoch. Checkpoint objects are create-only under `ckpt/<epoch>/`, and the "current checkpoint" pointer is advanced by compare-and-set on `(pool, epoch)`. A deposed leader cannot publish (RP §10.5, §12.4).
- **I7 (fail-open).** Every `dcache`/store/Postgres failure maps to one of: no merge, no offer, `renewed{unavailable:true}`, or no export. The engine then computes the result itself (RP §4.2 "engine operation does not wait for the service"; RP §7.2). Known exception: #12883 (§7). An engine **OOMKill is not fail-open** (F1): the engine and the build die together, so the memory limit is a correctness budget, not just capacity.
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
  resources:
    limits:   { memory: 12Gi }     # #14884: fresh remote-cache engines peak 6.0-6.1 GB on a 48 MiB merge
    requests: { memory: 6Gi }
  tolerations: [ { key: node.kubernetes.io/spot, operator: Exists, effect: NoSchedule } ]
magicache: { enabled: false }
```

- **You cannot block `api.dagger.cloud` with a NetworkPolicy** (F59): policies match addresses, never FQDNs, and the CDN address is shared. Use a CoreDNS rewrite (NXDOMAIN for `api.dagger.cloud` inside the cluster) or Cilium `toFQDNs` if Cilium is the CNI. The `DAGGER_CLOUD_URL` redirect still matters because a per-session Cloud URL can take precedence (RP §11.2 caveat).
- Fork/untrusted PR jobs get a **separate engine DaemonSet** on a separate node label, with a read-only pool token (see §4.4).
- Whether the engine's graceful-stop budget can be configured: **UNKNOWN**. #14956 names `defaultGracefulStopTimeout` in `cmd/engine/stop_budget.go`, and the recon does not say whether it is configurable.

### 4.2 `dcache` service (Go, persistent nodes)

| Subcomponent | Responsibility | Grounding |
|---|---|---|
| WS endpoint `/v1/engine` | Basic auth before upgrade; per-socket state machine; separate peer message-ID spaces; lossless uint64 JSON; ≤64 MiB frames; answer engine pings; bounded outbound command queue | RP §2, §4 |
| OTLP receiver `/v1/traces`, `/v1/logs` (`/v1/metrics` accepted and dropped) | Decode only spans with `dagger.io/cache.contract=="1"`; feed `AttachRemoteHolding`/`UpdateRemoteHolding`; handle `engine.start/prune/share/stop` log bodies (JSON inside an OTLP string) | RP §2.1, `engine/telemetryattrs/attrs.go:595-942` |
| Pool graph | `dagql.NewCache(sqlitePath, WithBlobStore())` per pool; `MergeValues`, `SetStoredPart`, `StoredBundle`, `PlaceOffer`, `EquivalentEntries`, `CollectRemoteHoldings`, `DropStoredParts`, `LiveBlobs` | RP §8 |
| Export scheduler | Choose roots from executed/complete facts; dedupe on the target part's **usable stored availability** (verified blob at head) or a live pending reservation — never on value existence, so a metadata-only value cannot suppress a later donor's export (round-2 finding 14); ≤4 exports in flight per engine; ≤32 roots per export (companion policy in #14761) | RP §5.2, §13.1 |
| Upload coordinator | Ledger reservations; presign PUTs; post-upload verify; publish | §5 below |
| Renew signer | Verify HMAC renewal key → presign GETs for that chain only. Budget: p99 < 200 ms including **one committed Postgres stamp** (`ledger.md` §7), hard engine deadline 2 s; the budget is an SLO target, not a measurement. An in-memory chain→digests index removes the **graph walk**, not the durable stamp; index miss ⇒ `bad_key` | RP §5.6, `dagql/cache_part_renewal.go:17-26` |
| Checkpointer | On change with a **≥10 min floor**, and on graceful leader stop: `Checkpoint` → upload `ckpt/<epoch>/<seq>.db` with `If-None-Match: *` → CAS pointer in Postgres. Alarm on `dcache_ckpt_bytes` and checkpoint seconds — a 60 s full-DB dump at GB scale is TBs/day of writes (F62). Retain the last 5 via the pointer, not bucket lifecycle. | RP §9.2, PA §6.1 |
| Sweeper | GC per §6 | RP §8.4, §12.4 |
| Reaper | `ReleaseRemoteCache(cacheID)` once a cacheID has been disconnected > 15 min **and absent from `LiveEntries()` for the live-engine snapshot** — holdings are per live engine, so a stale cacheID must never retire another engine's holdings (F9). A spot engine's cacheID never comes back because its disk is gone | RP §8.1, `dagql/cache_remote_entries.go:1062-1101` |

**Why not stateless front-end replicas.** The brief asks for stateless front ends with the object store as the only durable state. That does not hold for metadata. The dagql graph is an in-memory e-graph with a SQLite checkpoint, the wire has no fencing or lock primitive, and the recon's recommendation is one authoritative writer per pool (RP §10.5, §12.4). The **byte path is stateless**: the service never proxies blob bytes. To scale out, add pools and spread them across leaders; do not add replicas per pool. A standby restores the latest checkpoint at failover and loses only metadata since that checkpoint. That loss is cache-only and fails open.

### 4.3 Indexes and which store holds them

| Index | Store | Why |
|---|---|---|
| recipe/key → value → parts → chain (the "manifest") | dagql pool graph (SQLite) with checkpoints in the object store | Only dagql's codecs can derive recipe identity (RP §6.4: "storing arbitrary JSON plus hashes does not implement this protocol") |
| digest → object / state | Postgres `blob` + `blob_object_gc` (`ledger.md` §1) | Needs epoch-fenced state transitions and one durable row per digest. S3 conditional writes could do it with ETags, but Postgres already exists and is simpler |
| token → pool, mode | Postgres `tokens` (sha256 of token) | RP §3.2: map an opaque token to a trust domain server-side |
| pool → leader epoch, current checkpoint | Postgres `pool_leader` | Fencing (I6) |

```sql
-- blob ledger: see `ledger.md` §1 (blob + blob_object_gc, generation-scoped keys, publish under M,
-- NOT NULL mint stamps, idempotent delete recovery). Not restated here: two copies of the
-- schema is how the R1 contradiction happened.
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
- **URL TTLs:** PUT **5 min** (≤ the 10-minute upload lease minus the verify budget; 15 min > 10 min lease was a contradiction, F16/F17). GET 1 h. GC grace G = 2 × GET TTL = 7200 s. An expired S3 presign returns 403, which triggers renewal (RP §7.2: renewal happens on 401/403/404/410).

### 4.5 Object layout (application-defined, not protocol; RP §12.1)

```
pools/<pool>/blobs/sha256/<hex>/g<gen>   immutable layer blobs (generation in the key: a delayed DELETE cannot hit a newer generation)
pools/<pool>/ckpt/<epoch>/<seq>.db       SQLite checkpoint, create-only
tier2/<tool>/<namespace>/...             sccache/turbo/etc., owned by those tools
```

Lifecycle rules: `AbortIncompleteMultipartUpload` after 1 day. These uploads come from service-side checkpoint/copy only; the engine never does multipart (RP §5.3). Checkpoint objects are retained **by pointer (last 5 per pool)**; there is no age-based expiry, so an unchanged graph cannot lose its current checkpoint (round-2 finding 19). **Blobs have no age expiry**, because the bucket TTL is not the value TTL (RP §12.4).

## 5. Chunking, durability on a 30-second kill, concurrency

**Chunk granularity is chosen by the engine and cannot be changed.** The unit is one whole immutable snapshot-layer blob per OCI descriptor. There is no chunking, multipart, resumable upload or retry in the engine's PUT path. Each upload is one raw PUT with a 1-minute idle bound, at most 8 concurrent per session (RP §5.3, §6.3, `engine/remotecache/remotecache.go:242-279`). So "streaming write-through" means: export each completed part as soon as telemetry shows it, not at session end (PA §7.2). The service controls *when* uploads happen, not their size.

### 5.1 Verification policy (stated once)

Every blob gets **authorization + existence + size** checks (the PUT capability was issued for that exact key, HEAD confirms the object and its length). A **1 % sample** additionally streams a sha256 through the S3 endpoint — never from a local copy — and compares it to the descriptor. The other 99 % are *not* hash-checked at the service; the engine re-hashes every byte it imports (RP §7.2, `engine/snapshots/pull.go:433-473`), so a wrong object makes the engine fall back to compute rather than poisoning it (RP §7.2). `verify_mismatch` must be 0; a mismatch invalidates the blob, drops the part and forces re-export. This is a weaker guarantee than "every blob hash-checked" and is stated as such everywhere.

- **Multipart abort semantics:** these do not apply on the engine path. A killed single PUT leaves no object (PA §6.1). There are no orphaned parts to bill.
- **Size cap:** AWS S3 and other S3 APIs cap a single PUT at 5 GiB. That cap is general S3 knowledge, not from the recon, so verify it for MinIO and B2/Wasabi. Descriptors over the backend cap are **left out of the URL map**, and their parts are never published. An upload may name a subset (RP §5.3).
- **Idempotent retries:** the engine cannot retry within an exportID (RP §5.3). The service retries by **re-exporting** after a backoff. Writing the same digest twice to the same generation's key is idempotent by construction (I3); a later generation has a different key.
- **Why PUTs go straight to the final key, with no staging plus copy:** the ledger, not object presence, decides visibility (I1, I4). Staging would double PUT and storage costs and gain nothing, because verification happens anyway.

**Decision order (F6/F43).** Three questions are settled *before* any dedupe rule applies: (1) is the offered capability still valid — bad HMAC or expired export ⇒ `invalid`; (2) is the chain **verified *and* visible to this pool** — a `verified` blob whose part is unpublished is **not** `alreadyComplete`; claiming it is a lie the engine will believe; (3) has execution started ⇒ `executionStarted`, never `accepted`.

**Concurrent duplicates without locks on S3.** There are no S3 locks and no `If-None-Match` on blob PUTs. The engine adds no headers, so a conditional presign cannot be honored (RP §2 PUT row), and identical bytes make it unnecessary. Dedupe happens at three levels:
1. **Recipe level:** the scheduler skips exporting a root only when the target **part is usable** (a verified blob at head for every digest) or a live pending reservation exists. A stored *value* with no bytes is not a reason to skip — that is the metadata-only trap (round-3 finding 14); `EquivalentEntries` may only short-circuit when the value's parts are all verified at head.
2. **Blob level:** `INSERT … ON CONFLICT DO NOTHING` puts a `pending` row at a new generation with a 10-minute lease. Only the inserting export gets a PUT URL. Other exports leave that digest out of their URL map, and their parts **wait** on the pending row.
3. **Lost race:** two exports may both get PUT URLs, for example after a lease expiry. Both write identical bytes to the same generation key and the store keeps one complete object. The ledger moves to `verified` once. `MergeValues` handles duplicate metadata: an unexpired local value is kept (RP §5.4).

**Hot keys (100 engines, same content, same moment).** Execution is not deduplicated across engines: all 100 run the operation, because the protocol has no on-miss lookup (RP §13.2 #3). Uploads *are* deduplicated, down to 1 export and 1 PUT per new blob. The read-side hot key is the real bottleneck: 100 fresh engines × a 2 GB layer is 200 GB of burst reads from the store (see §8). Per-key "sharding" doesn't help with a single object; that needs more MinIO bandwidth or a range-correct read-through cache (see Red team R5).

## 6. GC and TTL

Two layers, and neither uses bucket lifecycle on blobs:

1. **Graph-level TTL (values/roots):** the service sets retention `expiresAtUnix` on stored roots (RP §7.1). The default is 7 days since last **install** for pool retention. A merge extends the *receiving engine's* local retention, never the pool's; an `accepted` offer is not a serve. `idle_prune_after` (30 d) compares `last_install_at` for every root, and roots never installed get `last_install_at = created_at`, so pre-merge alone never keeps a root alive (round-3 finding 15). Expired roots are pruned through dagql, so their closure becomes unreferenced.
2. **Blob mark-and-sweep (hourly):** live = the last `LiveBlobs()` snapshot (all stored-part blobs; RP §8.4); candidates are `verified` rows not live whose `last_url_mint_at` is older than G. Mark runs under the in-process mutex `M` (`ledger.md` §7): epoch-fenced transition to `deleting` + a queue row, then `DELETE` of the **generation key** outside `M`, then the row returns to `absent` with its `gen` retained (never charged to quota once `absent`). If renew later hits a missing object, run `DropStoredParts(digest)` — the drop is digest-granular and may take unrelated parts, which is accepted because dropping a part only costs recompute (`ledger.md` §8) — and reply `unavailable` (RP §8.4).
   - **Out-of-order arrival:** a blob uploaded before its part is published is protected by its `pending` lease. If publication never happens, the lease expires, the row becomes `failed`, and the next sweep deletes it. A blob whose part was published after the sweep's snapshot is protected by the `last_ref_at` fence.
   - **Scan cost:** the sweep reads the ledger from Postgres and `LiveBlobs()` from memory, with no S3 LIST on the hot path. A weekly reconcile LISTs `pools/<pool>/blobs/`, about 1,000 LIST requests per 1 M objects, which is $0.005 at AWS rates (PA §6), to find objects missing from the ledger.
3. **Per-pool quota:** ledger `SUM(size)` per pool. Eviction selects **reclaim groups** (a set of parts whose collective removal frees distinct blobs) scored by `benefit / distinct bytes freed`, recomputed after each removal (`metrics-and-eviction.md` §3.2) — per-part marginal bytes alone let two cold sharers outscore a hot singleton (round-2 finding 11). Watermarks 0.85 ⇒ evict to 0.70; 0.95 ⇒ stop admitting bytes (`urls:{}`); never evict below `min_retention` (24 h); a marked blob cannot be published, so no projection cancellation is needed.

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

**How to measure it before building (Phase 0).** Use `dagger.io/cache.pairing_digest` (self digest *excluding* implicit inputs; `engine/telemetryattrs/attrs.go:710-718`). Across sessions, count spans with `outcome=executed` where some earlier session had the same `self_digest`+`structural_inputs`. Those are the hits a perfect shared cache would get. Separately count spans with the same `pairing_digest` but a different `self_digest`; those are lost to per-session keying. Weight both by the union-attributed recompute seconds (§1.3 of `metrics-and-eviction.md`), and treat the result as an opportunity estimate, not a gate. This gives "seconds of compute a shared cache could save" per job.

## 8. Cost model

The workload numbers are **assumptions** for the human to replace: 100 nodes; 2,000 jobs/day; 20,000 new unique blobs/day after dedupe, averaging 30 MB (600 GB/day written); per job 150 blob GETs including range reopens and 2 GB downloaded (300 k GET/day, 4 TB/day read). Rates are AWS S3 Standard us-east-1 from PA §6: PUT $0.005/1k, GET $0.0004/1k, $0.023/GB-month.

| Item | Calculation | AWS S3 | MinIO in-cluster |
|---|---|---:|---:|
| PUT (engine uploads) | 20k × $0.005/1k | $0.10/day | $0 (IOPS only) |
| HEAD + sampled verify GET | 20k HEAD + 200 sampled GETs × $0.0004/1k | $0.008/day | $0; 6 GB/day re-read (1 % of 600 GB) ≈ 0.07 MB/s average |
| GET (engine reads) | 300k × $0.0004/1k | $0.12/day | $0 |
| Checkpoints (5 pools, 10-min floor ⇒ 144/day each) | 720 × $0.005/1k | $0.004/day | $0 |
| **Requests total** | $0.10 + $0.008 + $0.12 + $0.004 = $0.232/day | **≈ $7/month** | — |
| Storage (7-day working set ≈ 3 TB after GC) | 3,000 × $0.023 | $69/month | 3 TB × EC overhead (~1.5× **assumed**) of disk |
| Egress to engines | 4 TB/day × 30 × egress rate (in-region private endpoint cheaper; internet higher) | **UNKNOWN in recon**; at typical internet egress pricing this is thousands of $/month | $0 |

Conclusions:
- Layer blobs are large, so **request costs are negligible** (≈ $7/month), unlike per-file caches (compare the PA §6 example at $1,110/month).
- **Bytes moved dominate.** In production the object store **is** off-cluster (owner constraint, §1). Egress and the endpoint's latency are therefore first-class budget lines, monitored store-side (§2.3 of `metrics-and-eviction.md`) with an install-failure breaker; the design cluster's in-cluster MinIO has neither cost nor representative latency.
- **Store throughput is the real budget.** A cold-start wave of 100 engines × 2 GB is 200 GB. At 10 Gb/s aggregate (≈1.25 GB/s) that is ≈160 s of saturation. At 25 Gb/s it is ≈64 s.
- `dcache` compute: one leader per pool. Memory is **UNKNOWN**. #15013 shows 7.5 GB SQLite / 314 k entries for one engine shard; plan 16 GiB per leader to start and measure. OTLP ingest CPU is **UNKNOWN** and must be measured in Phase 1.

## 9. Request flows

**Cold miss (nothing stored).**
1. The engine starts: WS `hello{cacheID,generation=1,…}` → `welcome{}` (RP §5.1). The service sends a warm `merge` (see Hit). Here it has nothing to send.
2. The job runs and the engine executes `X`. Its span (`outcome=executed`, `cache.parts`, `result.id`) reaches `/v1/traces`. `AttachRemoteHolding` runs.
3. Once `X`'s parts are complete (a lazy-eval span), the scheduler checks the part ladder (verified-at-head / live pending / absent) and sends `export{roots:[n], outputs:[…]}` (RP §5.2). `engine.share` carries imported rows, not an inventory of freshly executed roots, so it is **not** a discovery source on its own.
4. `exported{exportID,bundle}`. The service validates the bundle and **enumerates the chain digests outside any graph lock**.
5. For each digest: verified → omit; pending by another export → omit (publication is retried by the partner's verify event, `ledger.md` §5.2); else reserve `pending` (`ledger.md` §3) and presign a PUT. It sends `upload{exportID,urls}` **within 5 s**, always, empty map if nothing is needed (RP §5.2: 4 slots). **The continuation is sent before `CommitValueMerge`** — the graph commit takes the dagql write lock and can exceed 5 s; it must never gate the continuation (round-2 finding 15). Verification proceeds on arrival; publication waits for the commit. If the commit fails or exceeds 30 s, parts are not published and the export is released.
6. `uploaded{done,failed}`. For each `done`: HEAD size, then the §5.1 verification policy (hash-check 1 % of blobs; existence/size/capability for the rest) → `verified` through the owner-fenced transition. When **all distinct digests of the chain** are verified at head: publish under the in-process mutex (`ledger.md` §5): re-read every chain digest's state, require `verified`, then `SetStoredPart`. Blobs that fail or are no longer owned → `failed`, part unpublished, re-export with backoff.
7. The next checkpoint makes the published part survive leader failover.

**Hit.**
- **(a) Pre-merge.** A fresh engine connects. The service picks the pool's hottest live roots, limited to ≤ 48 MiB of merge payload and ~N roots (N **UNKNOWN**, tune it). It runs `StoredBundle`, attaches a GET URL and HMAC `renewalKey` to each chain, and sends `merge` (RP §8.3). `merged` → `ApplyMergedReply`. The job's call hits engine-local metadata (`cache.hit.remote="true"`). On demand the engine does a range GET to MinIO, verifies the digest and installs. The span carries `cache.offer.bytes` (RP §2.1).
- **(b) Live offer.** The engine creates a lazy entry that has a stored equivalent. The service runs `PlaceOffer` and sends `offer`. `accepted` → wait for completion evidence. `executionStarted` → too late, so log it as "offer lost race" (RP §5.5).
- **(c) URL expired.** The GET gets 403, the engine sends `renew`, and the service replies in under 2 s with fresh GETs, or `unavailable` if the blob is gone. In the second case it also runs `DropStoredParts` (RP §5.6, §8.4).

**Concurrent duplicate (100 engines, same new layer).** All 100 execute. The scheduler sees 100 equivalent executed facts and exports from the **first** reporting engine only; the other 99 are recorded as candidate donors. If engine #1's upload fails, the scheduler picks the next donor whose holding is still live. If two exports race past the pending check, the blob ledger allows a single `pending` owner, the other upload omits that digest, and its chain publishes when the partner's verify completes. Result: 1 PUT per new blob, at most a few duplicate metadata merges (harmless, RP §5.4), and 0 S3 conditional writes needed.

**Node killed mid-upload (30 s notice or none).**
- SIGTERM: the engine shuts down within 25 s, or SIGKILL with no notice at all. Both cases give the same outcome.
- In-flight PUTs are cancelled and S3 creates no object. The WS drops; the engine-side export providers and exportID die with the session (RP §4.2).
- The service marks the engine's sessions as ended (`EndRemoteProcess`). `pending` rows for its uploads expire after the 10-minute lease and become `failed`. Parts waiting on those rows are unpublished. Metadata already merged at step 4 stays in the graph as a value **without stored parts**, which is harmless: engines get metadata with no offered bytes and compute it, or a later donor fills it in.
- Already-`verified` parts from that engine are unaffected. Fifteen minutes later the Reaper calls `ReleaseRemoteCache(cacheID)`.
- **Loss bound:** only work whose publication had not finished. The exposure window (span end → part verified) is a primary SLI (§10).

## 10. Failure modes

| Failure | Engine behavior (per recon) | dcache behavior | Measure |
|---|---|---|---|
| dcache down | Reconnect 1→30 s backoff ±20%; builds proceed uncached (RP §4.2). OTLP export failures: blocking behavior **UNKNOWN**, believed bounded and dropping (RP §13.2 #4) | Cold restore: lease ≤30 s, then checkpoint restore, which is linear in graph size (#15013) — failover is an accepted outage of minutes, not seconds. F62: a warm standby tailing every checkpoint is a TB/day write amplifier and is **not** built. | `engine_sessions`, restore duration |
| Leader split-brain | — | Fenced epoch; create-only checkpoints; CAS pointer (I6) | Epoch changes; CAS conflicts |
| Store refuses (5xx/conn refused) | GET non-401/403/404/410 → source exhausted → compute (RP §7.2). PUT failure → `failed` | Store error rate > 10 % / 10 min → stop merges, offers and exports for the pool; the probe is liveness only, never a latency signal | store error rate; `renew_unavailable` |
| **Object store hangs / slow** | **Each demanded part stalls up to 30 s before fallback** (RP §7.2), so builds get *slower* than cold | Trip on **store-side errors** and **install p95** (`metrics-and-eviction.md` §2.3): >2 % 5xx over 10 min warns, >10 % stops merges/offers, `install_p95 > 5 s` warns. The service's own probe is liveness only and must not be the breaker input | store error rate; install p95; demand p95 |
| S3 throttling (503 SlowDown) | Same as refuse, for GET. PUT `failed` | Back off the export rate per pool | 503 counts |
| Postgres down | — | No verify/publish → stop exports. Keep serving merges/renews for already-published parts **only while stamps can still be committed**: a mint is a conditional `UPDATE … RETURNING` committed before the URL leaves the process (F16/F17), so if Postgres is down the service **stops minting** and GC stays paused for G after recovery. No in-memory stamp buffering. | Ledger write errors |
| Token revoked / expired | Close 4001 → endless reconnect (RP §3.1) | Fast 401 at handshake | Handshake 401 rate |
| Engine version mismatch | Connected, idle | Quarantine (I9) | `quarantined_sessions` |
| OTLP overload / dropped spans | Engine drops spans at its bounded queue (RP §13.2 #4) | Fewer exports (miss only); ingest sheds load per pool | Dropped-span counters, ingest lag |
| Engine never gets `upload` | 4 export slots held, chains pinned on a 30 GB disk (RP §5.2) | 5 s upload watchdog per exportID | `export_slot_age` |
| Wrong bytes at a key | Digest check fails → source exhausted → compute (RP §7.2) | HEAD/capability catches wrong size or unauthorized key; the 1 % sample catches content; the engine's import hash catches the rest and makes that read fall back to compute, not the cache. Renew-time 404 → `DropStoredParts` | `verify_mismatch` (alert: should be 0) |

**SLIs (`metrics-and-eviction.md` §2, normative):** `saved_gross` = Σ `donor_cost_seconds` over installs; `saved_net` = Σ max(donor − install, 0); `lost_net` = Σ max(install − donor, 0); `recompute_remaining` = recompute on entries with no install (its own series); entry hit ratio = installs ÷ (installs + computed); ceiling = gross ÷ (gross + remaining). `cache.hit.remote` is an origin marker, not a hit counter. Also tracked: publication p50/p95 (span end → `SetStoredPart`), exports lost to disconnect, deferred publish chains, `renew_unavailable`, job duration against the warm node-local baseline. Duration-weighted or self-time weighting, and "distinct installs ÷ (installs + executed)", are R1 formulas and are retired.

## 11. Migration and rollout

0. **Phase 0, now on v0.21.11 (1–2 weeks).** Ship Tier 2: Zot pull-through, GOPROXY, sccache/Turbo on the object store (MinIO in the design cluster). Enforce `umask 022` in runner images (#15034). Inventory modules for git `clients` (#15014), filtered checks (#15044), URI secrets (#12883) and `PRIVATE` mounts (#13060). Do the pairing-digest analysis of §7 from current telemetry, assuming v0.21.11 emits the `cache.*` contract; **UNKNOWN**, as the contract is cited from the beta source, and if v0.21 lacks it, do the analysis on a beta.25 shadow engine. **Gate A (pre-build):** a discovery capture plus a critical-path opportunity estimate from existing telemetry (per-pipeline `would_hit`/`lost_keying` upper bounds and the fraction of wall time in cacheable ops). **Gate B (post-prototype):** a paired run against today's warm node-local baseline, comparing p50/p95 wall time and failure rate. No percentage threshold is asserted as measured; the 30 % figure was a proposal and is withdrawn as a gate.
1. **Phase 1: compatibility.** Port modules to the 1.0-beta engine; breaking changes from v0.21 → 1.0 are **UNKNOWN** in the recon. Confirm that #14913 and #13898 are fixed in beta.25, that #14884 (merged 2026-10-27) is in beta.25 (**UNKNOWN**: the tag's date relative to that merge is not in the recon), and the no-exec Container path (#14657; RP §13.1 says a fix appears present at the pinned revision).
2. **Phase 2: build `dcache`** with the RP §12.5 acceptance suite plus chaos tests: kill an engine mid-PUT, kill the leader mid-checkpoint, GC racing a publish, a hanging MinIO.
3. **Phase 3: parallel run.** Deploy a new engine DaemonSet (`dagger-engine-ephemeral`, beta.25) on a spot node pool labeled `dagger-tier=ephemeral`, plus a new ARC runner scale set with the same runner→node pinning as today. The current v0.21 DaemonSet and its runners stay untouched on persistent nodes. Shift repos one at a time and compare job duration and failure rate. Kill switch: remove `remoteCache.url`, or quarantine the pool server-side, which makes engines idle-connected without a rollout.
4. **Phase 4: retire** the hostPath DaemonSet once ephemeral p50/p95 job time is within the agreed bound of baseline for 2 weeks.

**Helm chart shape (`dcache`):** StatefulSet (2 replicas, leader via Postgres lease), PVC (SQLite working copy; rebuildable from checkpoint), Services `dcache:8443` (WSS + OTLP on the same listener, path-routed), Secrets (object-store access key scoped to `pools/*` and `ckpt/*`, pool HMAC keys, Postgres DSN), ConfigMap (pools, quotas, TTLs, export policy), PDB `minAvailable: 1`, nodeAffinity to non-spot nodes, ServiceMonitor.

**Engine version choice:** v0.21.11 cannot use Tier 1. **beta.25, pinned by digest**, with `dcache`'s `go.mod` pinned to the same commit, upgraded in lockstep and only after the contract test suite passes. Do not wait for 1.0 GA as a plan: its date is **UNKNOWN**. Tier 2 has no version dependency.

## 12. What I would NOT build

- **No chunking or multipart layer.** The engine does single raw PUTs of whole layers (RP §5.3), so there is nothing to plug one into.
- **No multi-writer metadata, consensus, or Postgres reimplementation of the graph.** The dagql codecs are the recipe identity (RP §6.4, §10.5).
- **No non-Go reimplementation** (RP §6.4).
- **No cross-pool dedupe** (I8; it would create an existence oracle).
- **No shutdown-flush logic or local-cache persistence to a PVC or network volume.** Unclean stops wipe everything (#14956), and even clean ones do (#13952, #15082). A 30 s kill is always unclean.
- **No distributed execution singleflight** across engines. The protocol has no hook for it.
- **No BuildKit S3 export or OCI-registry-as-Dagger-CAS.** Neither path exists in the engine (§14).
- **No in-cluster MinIO in production.** Production has no MinIO: the store is external S3 by owner constraint (§8).

**If I only had a week:** do not build `dcache`.
- **(A, recommended):** keep 3–5 engines on **on-demand, non-spot** nodes with their NVMe caches. Move ARC runners, which are thin clients, to spot. Set `terminationGracePeriodSeconds` above the engine's stop budget (#13951). Add Zot pull-through and a GOPROXY.
- **(B, if engines *must* be on spot):** ephemeral engines with empty caches, plus Tier 2 (Zot mirror, GOPROXY, sccache/Turbo/Nx on the object store, with cache tool env injected into `withExec`), plus the Gate A measurement. Accept full loss of the Dagger op cache, and use the measurement to decide whether Tier 1 is worth a quarter of engineering.

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
→ *Mitigation:* Gate A: the Phase 0 pairing-digest opportunity measurement, then Gate B's paired wall-time run (`decision.md`). Tier 2 ships first. Upstream bug reports or patches for #15014/#15044 are prerequisites, not hopes.

**R2. Beta protocol churn breaks `dcache` on every engine bump.** (High, medium.) Bundle v3 and envelope v5 have no negotiation; hello only reports a version (RP §13.2 #2). Mismatch fails open, but silently, to zero hits.
→ *Mitigation:* I9 quarantine with an alert on `quarantined_sessions > 0`. Lockstep pins. A CI contract test spins up two real engines against `dcache` on every upgrade PR (RP §12.5). Renovate is never allowed to bump the engine alone.

**R3. A cross-engine hit produces a wrong result or a failed build, breaking fail-open.** (Medium, very high.) #14913 (env var missing from the key: a wrong hit). #12883 (URI secret not found after a cross-session hit: a failed build). #13060 (service identity split). Remote merge shares *more* aggressively than a single engine does, so latent keying bugs surface.
→ *Mitigation:* treat a confirmed fix in the pinned build as a release blocker. Lint modules for in-module URI secrets. Shadow-verify a 1% job sample: run with the pool quarantined and compare artifact digests and test results. A per-pool kill switch. Count `secret not found` errors on jobs with `cache.hit.remote`.

**R4. The service-side dagql graph does not scale.** (Medium-high, high.) It is the same library as the engine, so it inherits e-graph eq-class walks under a global lock (#15049) and restore time linear in DB size (#15013, 2m36s for 7.5 GB), and the pool graph accumulates the recipes of *every* engine. Failover then takes minutes, and lock stalls delay merges past the point where offers are useful (`executionStarted`).
→ *Mitigation:* small pools, aggressive 7-day TTL, an entry cap per pool, load tests with synthetic 1 M-entry graphs in Phase 2. Track #15050. Cold restore is accepted (F62) — a checkpoint-tailing warm standby is **not** built.

**R5. The store becomes the bottleneck or the outage.** (Medium, high.) A cold-start wave is 200 GB of burst reads (§8). A *hanging* store makes every demanded part wait 30 s before fallback (RP §7.2), which is worse than having no cache. In production the store is external S3 and its availability is not ours.
→ *Mitigation:* in the **design cluster** pin MinIO to non-spot nodes; in production the store is external S3, so the equivalent is an in-region private endpoint with a stated egress budget. An error-rate breaker that stops merges and offers. Stagger merge size by engine age. Optionally put GET URLs behind a range-correct read-through cache (URLs are opaque to the engine; RP §1), validated against the exact `Content-Range` rules (RP §2). Capacity-plan the **production read burst** (100 × 2 GB) and the store's throughput tier; the design cluster's disk numbers do not size production (round-3 finding 16).

**R6. The exposure window is longer than spot lifetimes.** (Medium, medium-high.) Before bytes are durable, a part must get through OTLP batch delay, scheduler latency, 4 export slots, 8 PUTs and a 1-minute idle bound. Short-lived nodes, or a big layer at the end of a job, never publish, so the cache fills only with cheap early-job layers.
→ *Mitigation:* the publication-latency SLI. Prioritize exports by measured span duration ÷ bytes. Export at span end, not session end. There is **no SIGTERM signal to the service** (F41): the WS protocol carries none, and AKS preemption does not drain a pod without a termination handler. Install a Scheduled-Event handler plus a `preStop` hook that tells the service to stop scheduling exports for that cacheID — or accept that exports race the kill. Accept the loss and measure it.

**R7. The telemetry dependency fails or leaks.** (Medium, medium.) Discovery relies on OTLP (RP §1.3), so dropped spans mean no exports. Ingest volume can be huge (#14884 was a 15.9 GB engine OOM from telemetry structures). If some route ignores `DAGGER_CLOUD_URL`, telemetry carrying our `dck_` token, plus call metadata, goes to `api.dagger.cloud`. Or a runner's real Dagger Cloud token ends up on the engine and is sent to `dcache`.
→ *Mitigation:* a CoreDNS rewrite (NXDOMAIN) for `api.dagger.cloud` inside the cluster, or Cilium `toFQDNs` — a NetworkPolicy cannot match an FQDN (F59, below). Reject `dag_`-prefixed tokens at the handshake and alert. Ingest sheds load per pool. Fallback discovery is **not** solved by lifecycle events: `engine.share` carries imported rows and `engine.stop` carries no entry numbers, so neither can name freshly executed roots (round-3 findings 1/18). If call spans do not reach the self-hosted URL, Tier 1 has no discovery path and the Gate A capture must fail loudly rather than degrade silently.

**R8. GC deletes a blob that is in use, or publishes after a delete.** (Low-medium, medium.) There is no gap between `LiveBlobs()` and `SetStoredPart` any more: publish and GC mark share the in-process mutex M (§0 of `ledger.md`). The remaining gap is between URL minting and deletion. The result is persistent 404s → renew → unavailable → misses. A bug in the fence would make this systematic.
→ *Mitigation:* the epoch-fenced conditional transitions of `ledger.md` (a stale writer changes nothing). A grace G ≥ GET TTL + stall + skew. `DropStoredParts` on any renew-time miss. A chaos test that runs the sweeper at 1-second intervals during the acceptance suite. Start with sweeper delete disabled (mark-only) for the first 2 weeks.

**R9. Leader split-brain corrupts the pool graph.** (Low, high.) A paused (GC/stop-the-world) leader resumes after the standby took over and writes a checkpoint.
→ *Mitigation:* the epoch fence is checked inside the same Postgres transaction as the pointer CAS. Checkpoint keys are epoch-namespaced and create-only. The leader self-terminates when its lease is not renewed within half the TTL.

**R10. Cache poisoning across trust boundaries.** (Low-medium, high.) A runner from a less-trusted repo shares a node engine with a rw token and makes equivalence claims that trusted builds later consume (RP §3.2). Hashes authenticate bytes, not claims (PA §7.7).
→ *Mitigation:* a separate engine DaemonSet per trust pool on separate node labels, with runner scale sets pinned to them. A read-only token for fork PRs. Never share an engine across pools.

**R11. Stuck export slots fill the 30 GB local disk.** (Low, medium.) A `dcache` bug leaves exports without `upload`, so chains stay pinned (RP §5.2) and local GC can't reclaim them. That causes engine disk pressure, and the *build* may fail, which is not fail-open.
→ *Mitigation:* the 5 s upload watchdog (always send `upload`, even `{}`). Alert on `export_slot_age > 60 s`. Engine local GC tuned below the `emptyDir` limit.

**R12. Layers over the backend's single-PUT cap never cache.** (Low, low-medium.) Big monolithic layers (toolchains, datasets) are exactly the expensive ones.
→ *Mitigation:* exclude them from the URL map and count `skipped_oversize_bytes`. Push those workloads to split their layers or to Tier 2. Verify MinIO's actual cap (open question 3).
