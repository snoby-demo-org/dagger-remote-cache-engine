# Current implementation design

This document describes the shipped source, not the earlier proposals. dcache is a **standalone network service**: one Go coordinator, one trusted pool, one graph writer, local SQLite metadata, and S3-compatible snapshot storage. It is not an engine plugin, engine fork, distributed executor, or mounted-cache-volume replication service.

The compatibility baseline is Dagger `v1.0.0-beta.25`, commit `a45241b20a5e07fbfc5954479ef0b61da843499c`. [Bootstrap](../scripts/bootstrap.sh) checks that exact revision; [go.mod](../go.mod) resolves the imported Dagger modules to the bootstrap checkout. Changing the accepted engine-version setting does not upgrade the compiled protocol/library implementation. See [verification](../VERIFICATION.md) for executed evidence and limitations, [engine integration](engines.md), [S3 integration](s3.md), and [testing](testing.md).

## Topology and public surface

- CLI/session exporters and engines send OTLP telemetry to the coordinator. Engines also maintain a bidirectional WebSocket control connection.
- The coordinator requests exports, supplies transfer capabilities, merges metadata, and offers stored parts.
- Engines PUT and GET snapshot objects **directly against S3**, using presigned URLs. They do not route these transfers through the coordinator.
- The coordinator separately performs HEAD checks and full-content verification GETs against its internal S3 endpoint. It therefore consumes blob-read bandwidth, even though it is not the engine byte proxy.

The actual [HTTP handler](../internal/server/server.go) registers `/v1/engine`, `/v1/traces`, `/v1/logs`, `/v1/metrics`, `/healthz`, `/metrics`, and `/debug/state`. Renewal is a **WebSocket `renew` request**, not an HTTP renewal route. OTLP metrics are decoded and accepted but discarded; `/metrics` exposes coordinator counters. Health and coordinator metrics are unauthenticated; debug state requires authentication. OTLP HEAD probes also require no authentication.

Engine authentication uses Basic credentials with the shared token as username and an empty password. OTLP additionally accepts the same token as Bearer. There is no token-to-multiple-pools routing or per-client authorization. The CLI serves plain HTTP; secure exposure requires an appropriate network/TLS boundary.

## Dagger owns graph semantics

[The graph wrapper](../internal/graph/graph.go) opens the real `dagql.Cache` in `WithBlobStore` mode. It imports `core` for persisted object families/codecs and uses Dagger's transfer, snapshot, equivalence, dependency, replacement, expiry, retention, and holding APIs. `WithBlobStore` selects graph behavior; [storage](../internal/storage/store.go) implements the separate byte store.

Remote entry numbers belong to their cache identity; transfer ordinals are not coordinator entry numbers. After merging, output ownership dependencies and service-result references are relocated through the committed ordinal-to-number mapping. Relocation rejects malformed or missing references, permits multiple ordinals to collapse onto one result, and deduplicates owner dependencies. Publication uses the exported copy's expiry, not an assumed target expiry. Value freshness, retention, URL lifetime, and renewal-capability lifetime remain distinct.

## Discovery and observation ordering

[Telemetry ingestion](../internal/telemetry/ingest.go) learns holdings from outcome-bearing call spans carrying cache contract `1`. Recipe identity comes from `dagger.io/dag.output`; request identity comes from `dagger.io/dag.digest`. Structural terms and content identities feed the existing equivalence machinery. Call-payload protobuf logs provide deferred field identity; decoding them is not the sole source of the recipe.

Call spans attach holdings, establish session holds, replay deferred facts, observe retention, collect eligible remote holdings, and notify the export pipeline. Lazy spans and `engine.share` only update known holdings; they cannot create identities. Unknown-holding updates are buffered until attachment. Higher replacement counts supersede older state, while older observed content identities remain learnable. Pending buffers hold at most 4,096 keys and 4,096 call frames, with 64 content digests per pending key; overflow evicts an arbitrary tracked map entry, not a FIFO record.

Session-completion carriers release session holds. Engine stop ends observed sessions; prune observations remove retention edges using generation/time ordering; wiped-cache start events release the old cache identity. A disconnected socket alone is not a synthetic session-completion event. Discovery is push-driven: there is no inventory poll or on-miss lookup endpoint.

## Export, commit, verify, publish

[The pipeline](../internal/server/pipeline.go) reacts to observations rather than running a ranked export scheduler:

1. Admit a worker or drop/count the observation when the global export gate is full. Coalesce subsequent observations for the same holder key and revisit them before releasing its worker slot.
2. Acquire a connection-local export slot, read current holding state, and try live offers before export. Export one root, selecting complete non-metadata parts and refreshing selection on busy retries.
3. Receive the bundle and prepare its value merge. HEAD each declared layer; present objects need no PUT, while missing objects receive presigned PUT URLs and in-memory in-flight claims.
4. Start the upload continuation and commit metadata concurrently. Claims are reference counts, **not duplicate-upload exclusion**; two exports can upload the same digest.
5. After successful upload reply, verify reported completed blobs and previously present blobs. Each verification streams the full expected content, bounded by declared size plus one, and checks exact size and SHA256. The verification phase has a five-minute context.
6. Relocate completed outputs and call `SetStoredPart` only when every layer in a part verified. Pending, absent, and metadata outputs are not published as stored snapshot parts.

An empty upload releases unused captures. Busy roots receive up to ten export attempts with bounded delays. Typed engine errors leave sibling exports connected; ambiguous export failures close the peer to release possible stranded captures. Upload/verification/publication failures can leave metadata without stored parts or orphan objects. There is no durable export retry queue or atomic SQLite/S3 transaction.

## Reuse and renewal

On connection, `warmPeer` takes the trailing bounded roots from `StoredState`, builds their full dependency closure, signs output chains, and sends one merge. **This is not consumer-frequency ranking**: upstream returns roots sorted by entry number, so “recent” means higher-numbered roots, not measured recent demand. Single-donor roots are included. The signed JSON bundle is capped at 16 MiB by dropping roots and rebuilding, never by cutting closure values. `ApplyMergedReply` records what the engine actually retained, completed, or merely accepted as offers.

Live offers target an observed holding and stored equivalent entries. `AvailablePart` and `PlaceOffer` supply relocated parts; completed/already-offered addresses are excluded. Accepted and execution-started responses remain offered, not installed. There is no explicit offer-outcome backoff scheduler.

[HMAC renewal capabilities](../internal/renewal/token.go) bind pool, positive donor, ordered digests, and expiry. The handler verifies these and membership of the needed blob, then checks **every layer** and signs fresh GET URLs under one default 1.5-second context. A definite missing object drops all stored parts naming that digest; backend errors return unavailable without that drop. Renewal does not rehash bytes or revalidate donor liveness. Its capability lasts seven days, independently of default 15-minute S3 URLs. Handler work avoids export/verifier gates, but response dispatch/write is outside the renewal handler's deadline; this is not an end-to-end latency guarantee.

## Persistence, lifecycle, and cleanup

Startup validates configuration, ensures the S3 bucket, opens/restores SQLite, starts periodic checkpointing, and listens. Checkpoints save the stored-root closure and stored parts through Dagger's persistence machinery; they are local, not uploaded to S3. The default interval is ten minutes, unconditionally, and periodic checkpoint errors are ignored by the loop. Peer maps, deferred observations, in-flight claims, and worker state are volatile. Losing metadata means a cold cache; bucket listing does not reconstruct the graph.

Shutdown cancels workers, closes peers, waits for tracked work, then checkpoints and closes the graph under a fresh request-timeout context. The CLI subsequently drains HTTP. This is bounded best effort, not crash-at-every-step durability.

**No `Sweep` implementation exists in the current coordinator**, callable or CLI-scheduled. Storage exposes list/delete primitives and Dagger exposes liveness queries, but there is no wired sweeper, grace-period policy, or periodic reclamation. In-flight bookkeeping does not itself reclaim orphan objects.

## Limits, trust, and source/test map

[CLI configuration](../cmd/dcache/main.go) defaults to eight global export workers, four export captures per peer, sixteen concurrent verification calls, and 64 warm roots. Layer verification is sequential within each worker. Peers allow 128 pending outbound requests and 32 inbound handlers; decompressed OTLP requests are capped at 64 MiB. Request/upload defaults are 30 seconds/15 minutes. These are work-count bounds, not total-memory, graph-size, byte-budget, fairness, or client-capacity guarantees.

All authenticated writers are trusted equally. Hash verification establishes content integrity, not honest recipes or tenant isolation. Missing/corrupt/unavailable data must remain cache misses and recomputation, not incorrect results; corruption after publication relies on engine import validation. Existing corrupt objects may require manual deletion before re-export repairs them. There is no scrubber or distributed execution deduplication.

Tests sit beside graph relocation/persistence, peer transport/compression/errors, renewal capabilities, telemetry ordering/lifecycle, server publication/coalescing/slots/timeouts, storage signing/verification, and CLI validation. [Functional harnesses](../tests/functional/) cover real producer/consumer reuse, restart, missing/corrupt objects, mixed jobs, and fixture cleanup. Recorded evidence proves four actual overlapping queries; **96-client capacity is NOT_RUN**.

Unlike the [historical multiwriter/ledger designs](history/README.md), this implementation has no Postgres ledger, leader election, fencing epochs, HA handoff, transactional reclamation, ranked/fair scheduler, prepared-merge byte budget, or scheduled sweeper. Historical proposals are context, not implemented guarantees.
