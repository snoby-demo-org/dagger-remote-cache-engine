> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

CONVERGED: yes

Nothing blocks the design. One prerequisite is still unverified: call spans and engine-event logs have to reach a self-hosted `DAGGER_CLOUD_URL` and must not be redirected per session (`recon-protocol.md:491`; `decision.md:61`). If they don't, the service can't discover anything and the op cache can't work. Step 1 below checks this.

I checked these points against pinned beta.25 source fetched live: `cache_remote_entries.go`, `attrs.go`, `engine_events.go`, `cache_value_merge.go` and `engine/remotecache/session.go`.

## 1. C1–C4 resolution

**C1: Agree. My "share event or span parts" step was wrong.**
- In `cache_remote_entries.go`, `AttachRemoteHolding` is the only observation path that creates a holding. It needs a recipe, which comes from call identity. `UpdateRemoteHolding` returns false and changes nothing when the holding doesn't exist.
- In `attrs.go`, `cache.outcome` appears only on call spans. Lazy-evaluation spans carry `result.id`, `deps`, `parts` (complete parts, including absent and metadata parts) and `content_digest`, but no outcome.
- In `engine_events.go`, `engine.share` reports one snapshot-sharing pass and nothing else. It is not a list of freshly executed roots, which matches `architecture.md:224`.

*Decision:* discovery works like this:
1. A call span with contract `"1"` arrives. The service decodes the `dag.call` protobuf to get the recipe, then calls `AttachRemoteHolding` and `AddRemoteHold` for the session.
2. Lazy-evaluation spans and share events call `UpdateRemoteHolding`, which only ever updates.
3. Once a holding has a complete non-metadata part and nothing equivalent is stored, it becomes an export candidate.
4. Session release and `engine.stop` call `ReleaseRemoteSession` or `EndRemoteProcess`. `engine.prune` calls `ObserveRemoteRetention`.
5. `CollectRemoteHoldings` runs after each message, batch or reply.

**C2: Agree. The real APIs support "upload first, commit concurrently."**
- `session.go` holds an export slot until an upload arrives or the session ends. The engine has no timer for this. The first upload removes the export ID, a second one gets an error, and an empty map releases the export and returns `done:[]`.
- `PrepareValueMerge` holds the graph lock only briefly, to reserve numbers. It works on a clone, and the input must stay unchanged until exactly one terminal action, commit or `Release`.
- On the coordinator side, preparation drops parts and outputs, and parts are admitted only later through `SetStoredPart`. Blob digests can therefore be read from the unchanged exported bundle before the commit.
- The commit reply supplies the ordinal-to-number mappings that the `SetStoredPart` relocation needs.

*Decision:* each export runs this sequence:
1. Receive `exported` and run `PrepareValueMerge`.
2. List the chain digests from the bundle's output descriptors.
3. Take a short, local lock on the in-flight map. Skip digests that are already present (HEAD hit, or held by another export) and claim the rest.
4. Presign a PUT for each claimed digest.
5. Send exactly one `upload`.
6. Run `CommitValueMerge` while the PUTs are in flight.
7. Once both `uploaded` and the commit result are in, verify the blobs and then publish.

**Rules for exactly one continuation:** each export has an in-memory record with a single compare-and-set "continued" flag.
- **Watchdog (a policy, not a protocol rule, 10 s proposed):** if preparation fails, is cancelled or misses the watchdog, send `upload{urls:{}}` and `Release` the prepared merge. The graph lock and verify reads never gate this step.
- **Commit fails:** publish nothing. Any uploaded objects become accepted orphans.
- **A digest fails or verification fails:** don't publish the parts that need it. The rest of the metadata stays.
- **Session closes:** drop the record. `Release` the merge if it isn't committed yet, and clear the export's in-flight claims.
- There is no durable retry. A later re-export of an equivalent donor covers anything lost.

**C3: Agree.** Section 3 lists the limits. `M` is gone. Publication relies only on dagql's own lock inside `SetStoredPart`, plus a recheck of chain presence just before it. GC never holds a lock across a DELETE. Holding one across DELETE calls would stall every publication for the whole sweep.

**C4: Agree.** `hit.remote` survives restarts, so on its own it doesn't prove reuse. T7 is now two tests: corruption before publication and corruption after it.

## 2. Final architecture and flows

**Components**
- **One coordinator (Go), one pool, one replica.** Its parts:
  - WS front end
  - OTLP receiver (`/v1/traces`, `/v1/logs`)
  - Pinned dagql+core, using `WithBlobStore`
  - SQLite checkpoint on a persistent volume
  - Scheduler
  - Presigner
  - Verifier pool
  - Best-effort sweeper
- **One S3 bucket**, with keys of the form `pools/<pool>/blobs/sha256/<hex>`. Engines PUT and Range-GET it directly.
- **Auth:** one token-to-pool map. All 96 pods share the default pool, so all writers are trusted equally (`recon-protocol.md:88`). Split pools only when there is a real trust boundary.

**Discovery:** as in C1.

**Write**
1. The scheduler chooses a candidate and sends `export{roots≤32, outputs}`.
2. It continues as in C2.
3. Verification: HEAD the object to check size, then read it back and compute the full sha256. Both happen outside any lock.
4. If every distinct digest in the chain passed, relocate the owner and service references through the commit mappings, recheck that the chain is present, and call `SetStoredPart`.
5. Checkpoint every 10 minutes when something changed (a proposal).

**Hit**
- **(a) Merge at `hello`.** Send a capped warm set (section 3). Then `ApplyMergedReply` using the state the engine actually kept.
- **(b) Live offer.** When B attaches a holding that is equivalent to a stored, unexpired part and B has no complete part of its own, run `AvailablePart` and `PlaceOffer`, then send `offer`. `accepted` is recorded as offered, never as complete. `executionStarted` and `unavailable` go to backoff.
- **(c) Renew.** Check the HMAC key, which binds pool, donor and layers. HEAD the needed blob and reply with fresh URLs. If it's missing, reply `unavailable` and call `DropStoredParts`. The target is under 500 ms, against the engine's 2 s deadline.

**GC (best effort)**
- Runs on a schedule, with a rate limit, never during scheduling.
- Takes a `LiveBlobs()` snapshot, then lists the bucket.
- Deletes objects that are not live, not claimed in-flight, and whose `LastModified` is more than 24 h old. The 24 h grace period is a proposal.
- Races are accepted: one can delete a fresh re-upload. They cost cache misses, never wrong bytes, because of renewal 404 → `DropStoredParts` → engine fallback, and the engine's own digest check.

## 3. 96-client operating model

**Concurrent**
- WS sessions
- OTLP decoding
- `PrepareValueMerge`
- PUTs and GETs, which go straight to S3
- Verify reads
- Renewals
- `StoredBundle` reads

**Serialized by dagql's graph write lock**
- `CommitValueMerge`
- `SetStoredPart`
- Holding updates
- `DropStoredParts`
- Checkpoint

Nothing else is held across network I/O.

**Proposed bounded controls (configuration, not measured capacity)**

| Control | Initial value |
|---|---|
| Exports outstanding per engine | ≤2 (the engine allows 4) |
| Roots per export | ≤32 |
| Prepared merges, global | ≤8, with byte cap 512 MiB |
| Inbound `merge` per engine | 1 at a time |
| Offer requests in flight per engine | ≤4 |
| Renewal | separate lane, never queued behind scheduling |
| Verifier workers | 16 |
| Verify bytes in flight | ≤2 GiB |
| OTLP ingest queue | bounded; when full, drop and count. Telemetry already isn't guaranteed (`recon-protocol.md:553`) |

**Fairness**
- The export scheduler round-robins across `cacheID`s, so a chatty engine can't starve the others.
- For each equivalence class there is one donor export in flight. Other engines with complete equivalents are fallback donors.

**Duplicate uploads**
- They are tolerated: the bytes are identical and the last write wins.
- They are limited by the HEAD check and in-flight claims. A claim belonging to a dead export is released when its session closes.

**Merge and offer selection (no broadcast)**
- Offers are naturally targeted, since each one is for one of B's own holdings.
- The warm merge at `hello` is capped at ≤64 roots and ≤16 MiB of payload, both proposals. It is ranked by how many distinct consumers in the pool used the root recently, and roots with a single consumer are excluded.
- Unrelated jobs therefore get almost no merge traffic.
- Whether this warm set actually produces hits is **UNKNOWN**. It has to be measured with per-root hit attribution.
- No engine ever receives the whole graph.

## 4. Implementation sequence

1. **Discovery spike (needs no coordinator).** Point one beta.25 engine's `DAGGER_CLOUD_URL` at an OTLP capture sink. Confirm that call spans with `dag.call` and contract `"1"`, lazy spans, and `engine.*` logs all arrive.
2. **Front ends.** WS state machine (`hello`/`welcome`, ping, separate message-ID counters, error replies), plus the OTLP decoder feeding C1 discovery against a dagql pool.
3. **Write path.** The C2 pipeline, the export record and watchdog, verifier, `SetStoredPart`, and checkpoint/restore.
4. **Hit path.** Warm merge, live offers, and renew with an HMAC key.
5. **Two-engine reuse test.** Pass criteria, all required:
   - B's trace for the expensive target op shows no executing child span (no process run).
   - The lazy span carries `cache.offer.part` and `cache.offer.bytes > 0`, and the S3 access log shows the matching GETs.
   - B's output content digest matches A's.

   Then:
   - Repeat after killing A.
   - Repeat after restarting the coordinator from its checkpoint.
   - Run A and B concurrently on the same job: one stored part per chain, both succeed.
   - Delete the blob, or let its URL expire: renew returns fresh URLs, or `unavailable` plus a drop, and the build succeeds through fallback.
   - **Corrupted before publication** (wrong bytes uploaded): verification rejects it, there is no `SetStoredPart`, and no offer ever names that digest.
   - **Corrupted after publication** (object tampered with): B's import digest check fails, B falls back, and the build succeeds with correct output. Whether the coordinator ever finds out is **UNKNOWN**, so an optional scrubber is listed under prerequisites.
6. **96-connection test.** Run 96 idle or trivial engines. Check that all sessions are welcomed and stable through ping, reconnect after a coordinator restart works, renew latency stays under 500 ms, and memory is recorded. *This only shows the coordinator can hold 96 connections. It does not show how much work it can handle.*
7. **96-client mixed-workload test.** Mix repeated and unrelated jobs, using real pipelines. Pass if all of these hold:
   - No build fails because of the cache.
   - Repeated jobs show the step-5 reuse evidence.
   - Unrelated jobs show no merge traffic beyond the cap.
   - Renew stays under 2 s.
   - Every bounded queue stays below its limit or drops with a counter.
   - Publication latency (span end to `SetStoredPart`) and graph-lock hold time are recorded.

   Capacity is whatever this run measures. Nothing is claimed in advance.

## 5. Accepted tradeoffs and remaining prerequisites

**Accepted**
- Cleanup is best effort. Cache loss and orphan bytes are expected and acceptable. Bytes that fail integrity checks are never served.
- No HA and no automatic failover. Recovery is a cold restore from the checkpoint, and losing the checkpoint means a cold cache.
- One shared trust pool by default.
- Cache volumes (`withMountedCache`) are engine-local and not shared.
- Cross-engine hits only happen when cache keys are identical (same pinned engine, umask, inputs).

**Remaining prerequisites**
1. Step 1 discovery succeeds.
2. Engines and the coordinator are pinned to the same beta.25 dagql+core commit.
3. The S3 endpoint has strong read-after-write consistency.
4. #14913 and #12883 (cross-engine correctness bugs) are confirmed fixed in the pinned build.
5. A persistent volume for the checkpoint, with the coordinator on capacity that isn't ephemeral.
6. *Optional:* a post-publication scrubber, if the step-5 tampering test shows engines don't report import failures.