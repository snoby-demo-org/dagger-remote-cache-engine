> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

VERDICT: agree-with-changes

**Recommended design.** Run one Go process per trust pool, which I'll call `dcache`. Engines open their WebSocket to it and send their OTLP telemetry to it. It holds the upstream `dagql` graph, built with `WithBlobStore` and imported at the same commit as the engines, and saves that graph with dagql's own SQLite checkpoint on a persistent volume. All blob bytes go **directly between engines and an S3-compatible bucket** through presigned PUT and GET URLs, so `dcache` never sits in the byte path. Bytes are published under one in-process mutex after a HEAD check (does the object exist, is the size right) and a read-back digest check. Garbage collection is a periodic sweep based on `LiveBlobs()` with a grace period, under the same mutex. There is no Postgres, no blob ledger, no leader election, no Rust tier and no NativeLink. Capacity grows by adding pools (processes), not replicas. **Fallback:** if the discovery spike shows that call spans do not reach a self-hosted `DAGGER_CLOUD_URL`, or one pool graph cannot handle the load, there is no workable automatic Dagger operation cache. In that case use tool-level caches on S3 (sccache, GOPROXY, Zot) and build no op cache.

*Evidence note:* the Dagger checkout at `scratch/dagger-src` **no longer exists**. Dagger line citations below come second-hand from `recon-protocol.md`. I re-checked the beta.25 `protocol.go` message set live from GitHub: the engine only ever sends `hello` and `renew` as requests, and there is no lookup or miss message. I found no evidence of a final 1.0 release. Research stays pinned to beta.25, which is not necessarily the latest release.

---

## Review findings

**F1 — HIGH, verified omission.** The report's main idea is "reuse storage/tiering/eviction/I/O" (`prior-art-followup.md:7,99-103`). But this protocol has **no service-side byte server to reuse**. The service hands out URLs, and engines PUT and GET straight to them (`recon-protocol.md:17,29-30`; `decision.md:26`). With presigned S3, the "storage tier" is just S3 plus a URL signer. NativeLink stores, bazel-remote, Buildbarn, Foyer, Cachey and Percas all solve a problem this design does not have.
*Correction:* the only reuse that matters is dagql (graph, codecs, merge rules, SQLite checkpoint) and an S3 SDK presigner. Treat the rest as inspiration, or as a possible later read proxy.

**F2 — HIGH, verified omission.** The "Dagger-specific boundary" section (`prior-art-followup.md:85-95`) describes the adapter as protocol and codec work. It leaves out the hardest unavoidable piece: **discovery**. The service only learns engine entry numbers and completed parts from OTLP traces and logs, including call-protobuf decoding. That telemetry is configured separately through `DAGGER_CLOUD_URL` (`recon-protocol.md:18,38-72,461,552`). Discovery to a self-hosted endpoint is still unproven (`decision.md:61`).
*Correction:* make the discovery spike the first gate. If discovery fails, the design fails.

**F3 — MEDIUM, design judgement.** The "Go adapter plus Rust cache service underneath" option gives the Rust process nothing to own. The graph has to be Go, because core-type codecs and equivalence logic live in dagql and core. Metadata persistence is already dagql's SQLite checkpoint (`recon-protocol.md:405-407`). Bytes live in S3. Rust is not a simpler alternative either: it would need a port of bundle v3, envelope v5, core codecs, e-graph equivalence and merge/offer rules (`recon-protocol.md:551`).
*Correction:* use a single Go process. Rust is a possible later read-through GET proxy, and only if S3 latency or egress costs are measured to matter.

**F4 — MEDIUM, partly verified.** The completeness-checking analogy is applied in the wrong place (`prior-art-followup.md:77-79`). REAPI checks completeness when a client looks something up. Dagger has no lookup, so the equivalent checkpoints are (a) publication, (b) the renew handler and (c) `DropStoredParts` (`recon-protocol.md:393-395`). The behaviour the report says "must be verified first" is already established from source:
- A blob that is missing or returns 401/403/404/410 triggers one renewal.
- If renewal returns unavailable, or the digest or copy fails, the engine falls back to ordinary acquisition or the original operation (`recon-protocol.md:233,358-360`).
- A hanging GET stalls for 30 s before that happens (`:356`).

NativeLink does correctly separate `NotFound` from real backend errors (`raw-2.txt:225,310-319`).
*Correction:* missing content is a **miss, not an engine failure, according to source**. That still needs to be confirmed end to end (see T5).

**F5 — MEDIUM, verified omission.** The report never states the actual byte-plane contract, which is what rules most of the candidates in or out. Uploads are a raw single PUT with no auth, checksum or content-type headers added by the engine. Downloads are a GET that always sends `Range`, and expects either a 206 with an exact `Content-Range` or a 200 for the whole object. Authorisation is the URL alone (`recon-protocol.md:29-30,81`).
- S3 and MinIO presigned URLs meet this as-is.
- bazel-remote's and Buildbarn's REAPI/HTTP auth models do not.
- Cachey's custom `/fetch` API does not (`prior-art-followup.md:63` is correct there).
- Requiring S3 checksum headers on the PUT is impossible, because the engine sends none.

**F6 — LOW, verified.**
- Every third-party source URL points at `main` or `master` (`prior-art-followup.md:18-22,44-47,66-73`), so the claims will drift. Pin commits.
- The licence summaries check out: NativeLink is FSL with internal use permitted and competing commercial use barred (`raw-3.txt:20-31,56`). Percas acknowledges writes once they are admitted to memory, and an unclean exit leaves it empty (`raw-7.txt:106-120`). Issue #14657 shows a self-hosted service built on `MergeValues`/`PlaceOffer`/`EquivalentEntries` (`raw-12.txt`).
- These are all fine. Licences become irrelevant once nothing from those projects is adopted.

---

## Recommended architecture

```
 engine A..N (any count)                       one pool = one process
 ┌────────┐  WS /v1/engine (Basic token)      ┌──────────────────────────────┐
 │ engine │ ─────────────────────────────────►│ dcache (Go)                  │
 │        │  OTLP /v1/traces,/v1/logs         │  WS sessions  OTLP ingest    │
 │        │ ─────────────────────────────────►│  scheduler (export/merge/    │
 │        │                                   │   offer/renew)               │
 │        │                                   │  dagql.Cache(WithBlobStore)  │
 │        │                                   │  mutex M: publish | GC       │
 │        │                                   │  SQLite checkpoint (PVC)     │
 │        │                                   └──────────┬───────────────────┘
 │        │  presigned PUT / Range GET                   │ presign, HEAD,
 │        │ ◄──────────────────────────────►  S3 bucket ◄┘ verify-read, DELETE
 └────────┘        pools/<pool>/blobs/sha256/<hex>
```

**Components**
1. **WS front end:** one state machine per session, with separate message-ID counters for each side, ping, and limits on how many commands are in flight.
2. **OTLP receiver:** reads only contract `"1"` facts, plus the `engine.start`, `engine.share`, `engine.prune` and `engine.stop` events.
3. **dagql pool:** the graph plus a periodic `Checkpoint`.
4. **Scheduler:** decides what to export, merge and offer.
5. **Presigner and verifier:** signs URLs and checks uploaded objects.
6. **GC sweeper.**
7. **Config:** maps each token to a pool, and holds the HMAC key for renewal keys.

**Write flow**
1. Telemetry shows engine A finished a part (share event or span parts).
2. The scheduler sends `export{roots}`, in bounded batches.
3. It runs `PrepareValueMerge` and then `CommitValueMerge`.
4. For each chain blob it HEADs the bucket. Present objects are skipped. Missing ones are added to an in-memory in-flight set under M and get a presigned PUT.
5. It sends `upload` (an empty map if nothing is missing).
6. On `uploaded`, it HEADs each blob for existence and size, then reads it back and checks the sha256.
7. If every blob in the chain passes, it takes M, rechecks, calls `SetStoredPart` with relocated owners, and clears the in-flight entries.
8. Anything else is left unpublished, with no retry state kept. A later re-export covers it.

**Hit flow**
1. Engine B says `hello`.
2. The scheduler builds `StoredBundle` for a bounded set of stored roots it picks by policy (for example, recent ones in the pool), attaches presigned GETs and HMAC renewal keys bound to the donor, and sends `merge`.
3. It applies the `merged` reply using the state the engine actually kept.
4. B's lookups now hit locally. Bytes are fetched only when a part is actually needed.
5. When telemetry shows B has an entry whose part is missing, the scheduler uses `EquivalentEntries`/`AvailablePart`/`PlaceOffer` to send an `offer`. `accepted` is recorded as "offered", never as "complete".
6. On `renew`: verify the HMAC, the pool and the exact layer list, HEAD the needed blob, and reply with fresh URLs. If the blob is missing, reply `unavailable` and call `DropStoredParts`. The target is well under the 2 s deadline.

**Unavoidable Dagger-specific code:** the WS state machine; OTLP decoding, including call protobuf, lossless integers, and generation/replacement ordering; the scheduler; renewal-key handling; and the publish gate. **A service-side dagql graph is required.** Equivalence, relocation of numbers between engines, merge rules and offer-owner mapping are all graph operations, so a key-value store cannot replace them. Discovery happens **only** through telemetry. There is no on-miss API, so a hit requires a merge before B makes the call, or an offer before B evaluates the part.

**Where state lives**
- **Blob bytes:** S3, keyed by content digest. Durability comes from S3.
- **Graph state:** SQLite checkpoint on a persistent volume. This is **reproducible cache state**: losing it means a cold cache, and bytes left in S3 are later swept as orphans. It does not replicate itself. Optionally copy it to S3 so the process can move between nodes.
- **Authoritative state:** only config (token-to-pool map, HMAC key).
- **Ephemeral state:** the in-flight set and session holds live in memory and are rebuilt from fresh observations after a restart.

I'd choose no Postgres, RocksDB or SlateDB, because once bytes are in S3 and the graph is checkpointed there is no metadata left for them to hold.

---

## Concurrency and S3 semantics

- **Same content from several producers:** this is safe. Keys are content digests and the bytes are identical, so whichever PUT lands last is still correct. Duplicate uploads waste bandwidth but are acceptable. The in-flight set and HEAD-before-upload reduce them. No generation-scoped keys are needed.
- **Required store behaviour:** strong read-after-write and read-after-delete consistency, so that HEAD after PUT can be trusted. AWS S3 and MinIO provide this. **Any other "S3-style" service must be checked.** Single-PUT objects never appear half-written.
- **Minimum publication rule:** never call `SetStoredPart` on the strength of `uploaded.done` alone. Every distinct blob in the chain must pass the HEAD and digest check under M.
- **Minimum retention rule:** the sweeper deletes an object only if, under M, all of these hold:
  1. it is not in `LiveBlobs()`;
  2. it is not in the in-flight set;
  3. its `LastModified` is older than G;
  4. it has been non-live across two marks at least the maximum GET URL lifetime apart.

  The DELETE is issued while M is held, so a later reservation and PUT always lands after it. G must exceed the PUT URL lifetime plus the verify time. That also covers in-flight reservations lost in a restart.
- **Disconnect and restart:** the export ID and its providers die with the session. Unpublished work is lost and gets re-exported later. Published parts are unaffected. When the `dcache` process restarts, it restores the last checkpoint, and engines reconnect using their 1–30 s backoff.
- **Engine behaviour I could not verify:**
  - Whether a GET that is already streaming survives an S3 DELETE: **UNKNOWN**. If it doesn't, source says the engine falls back.
  - Whether the engine reports digest failures to the service: **UNKNOWN**.

**How engine count can grow**
- *Concurrent clients:* many sockets per process, with nothing hardcoded.
- *Metadata writers:* exactly one per pool.
- *Socket routing:* each pool gets its own URL in engine config. No router is needed.
- *Graph ownership:* per pool. **A shared bucket does not make front ends stateless or safe for multiple writers.**
- *Byte bandwidth:* scales with S3. The one exception is verify read-back, which passes upload volume through `dcache`.
- *Bottlenecks:* the graph's global lock, including equivalence-class walks (#15049); graph memory; restore time, which grows linearly with graph size; OTLP ingest volume; renew latency.
- *Scaling path:* more, smaller pools, with no sharing across pools.

---

## What to reuse / what to write

**Reuse with a real integration boundary**
- dagql and core, imported as Go libraries at the engine's exact commit (Apache-2.0).
- An S3 SDK for presign, HEAD, GET and DELETE.
- An OTLP protobuf decoder.
- A WebSocket library.

**Write**
- Everything listed under "Unavoidable Dagger-specific code" above, plus the sweeper.

**Inspiration only**
- NativeLink: separating `NotFound` from real errors, and coalescing in-flight work. FSL licence, and there is no point where it could plug into this design.
- bazel-remote and Buildbarn: proof that bounded caches can work without reference-counted GC.
- BuildBuddy: not needed.

**Possible later additions, each needing measured justification**
- A read-through GET proxy, Rust or not, that GET URLs point at. It must return byte-exact 206 responses.
- sccache via OpenDAL for incremental compiles, as tier 2.

---

## What to remove from the old design

**Delete now**
- The whole Postgres ledger: the `blob`, `blob_object_gc`, `op_inflight`, `pending_publish`, `part_chain`, `part_cost` and `eviction_cohort` tables (`ledger.md:47-124`).
- Epoch leases, drain-on-promotion and the `pg_terminate_backend` fence (`ledger.md:24-45`).
- Generation-scoped object keys (`ledger.md:126`).
- Quota charging and regret cohorts.
- The 1% hash sampling (replaced by a full check before publish).
- Durable publish-retry state (re-export instead).
- Leader election (`decision.md:37`). Replace it with a single replica on a persistent volume and accept that there is no high availability.

**Needs a measured experiment before relying on it**
- Discovery reaching a self-hosted `DAGGER_CLOUD_URL` (Gate A).
- Graph lock and memory at real pool size.
- Bounds on merge selection, within the 64 MiB message cap.
- G against real URL lifetimes.
- Whether full read-back verification costs too much bandwidth. Relax it only with evidence.
- Whether a single-replica StatefulSet can briefly run two writers during a node partition.

**v1 limitations, stated plainly**
- No high availability. Failover is a cold restore.
- No sharing across pools.
- The merge policy is a heuristic.
- Cache volumes (`withMountedCache`) are not covered.

---

## Small acceptance plan

These are proposed tests. **None has been run.**

| # | Scenario | Pass | Fail |
|---|---|---|---|
| T1 | A builds X; B, isolated with an empty cache, builds X | B's trace shows `hit.remote="true"` for X, with no exec span; S3 logs show GETs only for parts B actually needed | B executes X |
| T2 | Kill A after publication; B builds X | Same result as T1 | B fails or executes X |
| T3 | A and B produce the same X at the same time | One stored part per chain; both sessions finish; object digest is correct | Duplicate or corrupt part, or a session error |
| T4 | Restart `dcache` mid-session, and separately restart an engine | Engines reconnect, stored roots come back from the checkpoint, a new engine C gets a hit; unpublished work is simply re-exported | Graph lost while the checkpoint exists, or a hang |
| T5 | Delete a published blob, or expire its URL | Renew returns fresh URLs, or `unavailable` plus `DropStoredParts`; the engine falls back and the build succeeds | Engine build failure, or repeated renewal |
| T6 | Inject 30 s or more of store latency | Build finishes through fallback after about 30 s; renew still answers in under 2 s | Build fails, or renewals time out |
| T7 | Upload wrong bytes under a digest | Publish refuses the part; a consuming engine rejects the import and falls back | Part published, or bytes installed |

---

## Red team

1. **Discovery doesn't work self-hosted.** The service never learns roots, so it never exports. *Mitigation:* Gate A spike first; otherwise the fallback.
2. **Graph doesn't scale** (global lock, linear restore). *Mitigation:* small pools, entry caps, measured limits. The real ceiling is **UNKNOWN**.
3. **Wrong cross-engine hits** (#14913, #12883). *Mitigation:* confirm these are fixed in the pinned build; one trust domain per pool.
4. **Two writers during a partition.** *Mitigation:* single replica with a ReadWriteOnce volume. Whether that is enough is **UNKNOWN**; if not, add an S3 conditional-write lock object.
5. **GC deletes an object while a GET is mid-stream.** *Mitigation:* the two-mark grace period. Engine behaviour is **UNKNOWN**; source says it falls back.
6. **Beta wire changes silently zero out hits.** *Mitigation:* pin engine and dagql together; run a contract test on every version bump.
7. **Merge is too small to hit, or too large** (64 MiB, memory). *Mitigation:* bounded batches and tuning. The right policy is **UNKNOWN**.
8. **Corrupt or missing bytes cause repeated stalls** (30 s each). *Mitigation:* verify before publish; `DropStoredParts` on renew misses; an error-rate breaker that stops offers.

Sources:
- [Dagger beta.25 protocol.go](https://raw.githubusercontent.com/dagger/dagger/v1.0.0-beta.25/engine/remotecache/protocol/protocol.go)
- [Dagger release notes (Releasebot)](https://releasebot.io/updates/dagger)
- [Dagger CHANGELOG](https://www.Github.com/dagger/dagger/blob/main/CHANGELOG.md)