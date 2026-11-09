> **Historical design/review record, not current implementation status.** See [current design](../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Shared-cache prior art: reuse before invention

## Scope and verdict

Research of public GitHub source and maintainer documentation, not an interoperability test or benchmark. NativeLink, bazel-remote, Buildbarn, BuildBuddy, sccache and Rust cache libraries provide relevant precedents. None of the inspected REAPI servers is a demonstrated drop-in implementation of Dagger's beta.25 WebSocket protocol.

Recommendation: separate Dagger-specific graph/protocol adaptation from generic cache storage. Evaluate existing cache/storage implementations before adding bespoke lifecycle machinery. Rust is viable for generic storage; a Go adapter can reuse Dagger's own graph implementation without porting it. Do not presume the adapter is trivial or that replacing Postgres with RocksDB replaces the graph.

## Projects and what to reuse

### NativeLink — TraceMachina/nativelink

Rust remote build cache and execution server. Its store modules include filesystem, memory, S3, GCS, Azure, Redis, verification, compression, fast/slow tiers, sharding and completeness checking. The fast/slow implementation also coalesces fills and tracks in-flight writes. This is the best matched Rust architectural reference, not proof of Dagger compatibility.

Important: current LICENSE is FSL-1.1-Apache-2.0, not simply Apache-2.0. Its text explicitly includes internal use and restricts competing commercial products/services; each version converts to Apache after two years. Inspect the chosen revision and intended distribution before adoption.

Sources:
- https://github.com/TraceMachina/nativelink
- https://raw.githubusercontent.com/TraceMachina/nativelink/main/nativelink-store/src/lib.rs
- https://raw.githubusercontent.com/TraceMachina/nativelink/main/nativelink-store/src/fast_slow_store.rs
- https://raw.githubusercontent.com/TraceMachina/nativelink/main/nativelink-store/src/completeness_checking_store.rs
- https://raw.githubusercontent.com/TraceMachina/nativelink/main/LICENSE

### bazel-remote — buchgr/bazel-remote

Go HTTP/gRPC REAPI cache. Bounded disk directory with least-recently-used eviction; optional S3/GCS and experimental Azure proxy backends. Separates action cache and content-addressed bytes. Its small standalone shape is a useful complexity baseline. Apache-2.0. Its action-cache endpoint validates REAPI ActionResult messages, so arbitrary Dagger metadata cannot simply be posted there unchanged.

Sources:
- https://raw.githubusercontent.com/buchgr/bazel-remote/master/README.md
- https://raw.githubusercontent.com/buchgr/bazel-remote/master/LICENSE

### Buildbarn — buildbarn/bb-storage

Go REAPI storage daemon, independently deployable without remote execution. Its local backend uses large block files and a hash-table index, described by the project as self-cleaning without a separate garbage collector. Supports composable backends and completeness checking. Apache-2.0. Useful evidence that neither RocksDB nor graph-wide reference-count GC is universally required for build caches; it does not eliminate Dagger's own lifecycle constraints.

Sources:
- https://github.com/buildbarn/bb-storage
- https://github.com/buildbarn/bb-storage/blob/main/pkg/proto/configuration/blobstore/blobstore.proto

### BuildBuddy — buildbuddy-io/buildbuddy

Official enterprise deployment guidance recommends Pebble on persistent SSD: metadata and small artifacts in the KV database, larger artifacts in files. Distributed caching routes to replicas with their own volumes. Source includes approximate-LRU machinery, batched access-time updates and repair/orphan scanning. This supports the Rust+embedded-KV idea in principle, but not putting all large blobs inside the KV store. Pebble cache source is under enterprise/ and its license is not the root open-source license; do not copy it as Apache code.

Sources:
- https://www.buildbuddy.io/docs/enterprise-cache-setup/
- https://raw.githubusercontent.com/buildbuddy-io/buildbuddy/master/enterprise/server/backends/pebble_cache/pebble_cache.go
- https://raw.githubusercontent.com/buildbuddy-io/buildbuddy/master/enterprise/LICENSE

### sccache — mozilla/sccache

Rust compiler-result caching with multiple remote storage backends. Its S3 implementation uses Apache OpenDAL. This is directly relevant to incremental compiler work inside a Dagger exec, but is not a whole-Dagger-operation cache.

Sources:
- https://github.com/mozilla/sccache
- https://raw.githubusercontent.com/mozilla/sccache/main/src/cache/s3.rs
- https://github.com/apache/opendal

## Rust components worth evaluating

- **rust-rocksdb/rust-rocksdb**: Rust bindings to RocksDB. Suitable candidate for a persistent metadata/index layer. RocksDB is embedded, not a replicated service; its FAQ explicitly says it has no built-in replication and no multi-process writes. Durability depends on WAL/sync choices. Large result bytes should be evaluated separately from metadata.
- **foyer-rs/foyer**: Apache-2.0 hybrid memory/disk cache library. Its README lists RisingWave, Chroma, SlateDB, Percas and Cachey as users. Candidate for a reusable local hot tier, not a replacement for the authoritative Dagger graph or durable object store.
- **scopedb/percas**: Rust distributed persistent NVMe cache service with HTTP PUT/GET/DELETE, decentralized routing and distribution. Current README selects `cache2` by default and offers Foyer as an alternative; older descriptions of Foyer as its default are stale. Crucially, current cache2 writes acknowledge memory admission, not durable storage; unclean exits start cold, and callers must retain an authoritative source. It is an acceleration tier, not the sole durable result store. No Dagger compatibility demonstrated.
- **s2-streamstore/cachey**: Foyer-based read-through cache for immutable object-store blobs, range paging, concurrent-request coalescing and hedged requests. Not transparent S3: its /fetch API requires a precise Range and has custom semantics. Dagger integration requires an adapter and exact range-response testing.
- **slatedb/slatedb**: Rust embedded LSM engine with object storage as its durable substrate. Possible metadata alternative if avoiding local-SSD dependence is important. Object-store latency and explicit durability waits remain tradeoffs. Does not implement Dagger's graph.

Sources:
- https://github.com/rust-rocksdb/rust-rocksdb
- https://github.com/meta/rocksdb/wiki/RocksDB-FAQ
- https://raw.githubusercontent.com/foyer-rs/foyer/main/README.md
- https://raw.githubusercontent.com/foyer-rs/foyer/main/LICENSE
- https://raw.githubusercontent.com/scopedb/percas/main/README.md
- https://raw.githubusercontent.com/s2-streamstore/cachey/main/README.md
- https://raw.githubusercontent.com/slatedb/slatedb/main/README.md

## Proven simplification to investigate

REAPI separates an action-to-result mapping from digest-to-bytes storage. Its GetActionResult contract says referenced bytes should exist when a result is returned and remain available for some period. NativeLink's completeness checker explicitly suppresses records with missing referents, while distinguishing actual backend errors from absence.

This supports evaluating independent bounded blob eviction plus completeness checking, instead of assuming every reference needs permanent protection. It does NOT justify deleting bytes still promised by outstanding Dagger URLs or assuming post-offer missing bytes are harmless: verify Dagger fallback, renewal and in-flight read behavior first. No database makes cross-store publication atomic automatically.

Sources:
- https://raw.githubusercontent.com/bazelbuild/remote-apis/main/build/bazel/remote/execution/v2/remote_execution.proto
- https://raw.githubusercontent.com/TraceMachina/nativelink/main/nativelink-store/src/completeness_checking_store.rs

## Dagger-specific boundary

The inspected beta.25 protocol uses WebSocket JSON envelopes and dagql's transfer types for export, upload, merge, offer and renewal. Standard REAPI services speak a different interface. An existing storage backend may be reused behind a Dagger adapter, but signed raw HTTP transfers, range reads, compression and authentication must match the engine; a configuration URL alone is not an adapter.

Public issue #14657 documents another operator's self-hosted remote-cache implementation using MergeValues, PlaceOffer and EquivalentEntries. Thus an external implementation is not purely hypothetical. These searches did not locate a published deployable version of that operator's service.

Sources:
- https://raw.githubusercontent.com/dagger/dagger/v1.0.0-beta.25/engine/remotecache/protocol/protocol.go
- https://raw.githubusercontent.com/dagger/dagger/v1.0.0-beta.25/dagql/cache_stored_parts.go
- https://raw.githubusercontent.com/dagger/dagger/v1.0.0-beta.25/engine/remotecache/remotecache.go
- https://github.com/dagger/dagger/issues/14657

## Evaluation order, not an implementation commitment

1. Compare NativeLink's store architecture, bazel-remote's simpler bounded-cache model and Buildbarn's self-cleaning storage before choosing a backend.
2. Isolate the Dagger adapter contract and retain its graph semantics using upstream code where practical.
3. Evaluate Rust+RocksDB only for metadata needs that remain; consider Foyer for local caching and an established storage client for the byte tier.
4. Prove export from engine A and reuse by independent engine B, including after A disappears.
5. Test missing objects, corrupted objects, concurrent uploads, slow reads and expiry. Establish the minimum retention promise before adopting a simpler eviction scheme.
6. Add distributed metadata leadership or sophisticated cost-based eviction only if measured requirements justify them.

No source code or deployment was changed by this research. This note is not a benchmark, licensing opinion or claim that a backend has been integrated successfully.
