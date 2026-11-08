# dcache — external Dagger / S3 cache coordinator

A standalone Go service for **one trusted pool of independent Dagger engines**. The engines are unchanged upstream images, not custom builds or dynamically loaded engine plugins. Only this service is compiled.

The coordinator imports Dagger's pinned graph/transfer libraries, keeps graph state in a persistent local SQLite database, and gives engines signed URLs for direct S3-compatible uploads and downloads. It streams blobs back for integrity verification before advertising completed parts.

**Development prototype.** See `VERIFICATION.md` for execution evidence and remaining limitations; a passing helper test is not cross-engine cache proof.

## Compatibility and prerequisites

- **Linux** build/runtime: imported upstream engine libraries use Linux system calls. Native macOS compilation is not supported by this implementation.
- Go **1.26.6+**, Git, and network access for dependency/bootstrap downloads. The verified Linux build used **Go 1.27.3**.
- Pinned Dagger source/image: **v1.0.0-beta.25**, commit `a45241b20a5e07fbfc5954479ef0b61da843499c`.
- An S3-compatible service and credentials permitting bucket initialization and the needed HEAD/GET/PUT/LIST/DELETE operations.
- Python 3 and Docker for functional tests; test engines require privileged containers.

No claim of compatibility with other engine releases is made. `scripts/bootstrap.sh` refuses an upstream checkout whose commit differs from the pin.

## Build and unit tests

Run on Linux:

```sh
make build
make test
make race
python3 -m unittest discover -s tests/functional -p 'test_*.py' -v
```

`make build` bootstraps the exact upstream source into `.upstream/dagger` and produces `bin/dcache`. The Python tests validate harness behavior; they do not substitute for running real engines.

## Run the coordinator

Configure a **persistent** database path and keep the HMAC secret stable across coordinator restarts. Replace the S3 endpoint/credentials below with your own:

```sh
export DCACHE_LISTEN=:8080
export DCACHE_DB="$HOME/.local/share/dcache/cache.db"
export DCACHE_POOL=shared
export DCACHE_TOKEN="dck-$(openssl rand -hex 32)"
export DCACHE_HMAC_SECRET="$(openssl rand -hex 32)"
export S3_ENDPOINT=http://object-store:9000
export S3_PUBLIC_ENDPOINT=http://object-store:9000
export S3_BUCKET=dagger-cache
export S3_REGION=us-east-1
export S3_ACCESS_KEY=REPLACE_WITH_YOUR_ACCESS_KEY
export S3_SECRET_KEY=REPLACE_WITH_YOUR_SECRET_KEY
./bin/dcache
```

Those generated secrets must be saved in your secret-management mechanism, not regenerated on every launch. The database is **not** stored in S3. Put the SQLite database and its directory on persistent storage; one coordinator owns it.

`S3_ENDPOINT` is used by the coordinator. `S3_PUBLIC_ENDPOINT` is embedded into signed URLs and must be reachable from every engine with its host and scheme unchanged. If omitted, it defaults to the internal endpoint. Engines receive signed URLs, not the S3 account credentials.

### Runtime controls

| Environment variable | Default |
|---|---|
| `DCACHE_ENGINE_VERSION` | `v1.0.0-beta.25` |
| `DCACHE_URL_TTL` | `15m` |
| `DCACHE_CHECKPOINT_INTERVAL` | `10m` |
| `DCACHE_MAX_EXPORTS` | `8` |
| `DCACHE_MAX_VERIFIERS` | `16` |
| `DCACHE_WARM_ROOTS` | `64` |
| `DCACHE_REQUEST_TIMEOUT` | `30s` |
| `DCACHE_UPLOAD_TIMEOUT` | `15m` |
| `DCACHE_RENEW_TIMEOUT` | `1500ms` |

Verification/publication gets a fresh five-minute context after upload. Renewal is served independently of export and verifier worker permits. The bounded worker pool may drop observations for unrelated entries when full; updates for an already active export are coalesced.

## Connect engines and CLI clients

For the pinned engine, configure the following using the same pool token. The address must be reachable from engines and CLI clients; use TLS outside an isolated trusted network.

```sh
_EXPERIMENTAL_DAGGER_REMOTE_CACHE_URL=ws://dcache:8080/v1/engine
DAGGER_CLOUD_URL=http://dcache:8080
DAGGER_CLOUD_TOKEN=<same value as DCACHE_TOKEN>
OTEL_EXPORTER_OTLP_ENDPOINT=http://dcache:8080
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer <same pool token>
OTEL_EXPORTER_OTLP_TRACES_LIVE=1
```

This is environment configuration, not a shell script to source literally: spaces in the header value must be quoted when exporting it. Apply the exporter settings where engine/session telemetry originates, and configure the CLI's Cloud URL/token as well. The remote-cache URL alone does not redirect telemetry.

The service implements `/v1/engine`, OTLP HTTP/protobuf `/v1/traces`, `/v1/logs`, and `/v1/metrics` (metrics payloads are accepted and dropped). `/healthz` is public liveness; `/metrics` and `/debug/state` require `Authorization: Bearer <pool token>`.

## Real functional tests

On an isolated Linux Docker host:

```sh
make functional
# Equivalent, with explicit options:
python3 tests/functional/run.py \
  --binary "$(pwd)/bin/dcache" \
  --output "$(pwd)/artifacts/functional" \
  --engine-image registry.dagger.io/engine:v1.0.0-beta.25 \
  --scenario all
```

The harness launches its own uniquely named coordinator, digest-pinned RustFS S3 fixture, and fresh upstream engines. It uses an external execution counter, removes producer A before querying B, checks local warm reuse, restarts the coordinator with the same SQLite state, and deletes fixture blobs to exercise fallback. Consumers request a previously unseen file-name transformation to force snapshot-byte demand rather than merely reusing a cached scalar result. Failed stages produce a nonzero exit code.

`--keep` preserves this run's containers/state for diagnosis. Normal cleanup targets only the run's owned resource prefix; there is no global Docker prune. Evidence JSON and logs go under the specified output directory. Do not point this destructive fixture test at production data.

A separate integration-tagged storage test accepts `DCACHE_TEST_S3` and fixed **test-only** credentials (`dcachetest` / `dcachetest-secret`). It creates a `dcache-live-test` bucket and tests signed uploads, ranged reads, full-stream digest/size verification, same-size corruption rejection, and deletion:

```sh
DCACHE_TEST_S3=http://YOUR_ISOLATED_FIXTURE:9000 \
  go test -tags integration ./internal/storage -run TestRealS3RoundTrip -v
```

## Concurrent-client test and fixture caveats

```sh
mkdir -p artifacts/mixed
python3 tests/functional/mixed_load.py \
  --binary "$(pwd)/bin/dcache" --output "$(pwd)/artifacts/mixed" \
  --engine-image registry.dagger.io/engine:v1.0.0-beta.25 \
  --clients 4 --engine-memory-mb 256
```

Use `--clients 96` only on a sufficiently provisioned isolated host. A memory guard runs before creating resources and records **NOT_RUN**, not PASS, when capacity is insufficient. Query overlap is calculated from measured Docker/CLI query intervals, not a barrier-thread count. The driver checks both correct outputs and execution counters for shared and unique work.

Both functional drivers use an immutable Alpine image digest. Tag-only `container.from` is deliberately scoped per session in this engine. Disposable engines also receive `--oci-worker-gc-keepstorage=100,0,1000` (reserved/free/maximum MB): GC remains enabled, but the fixture does not inherit a host-wide free-space target that immediately prunes warm entries. Arguments and the reason are recorded in evidence. **No production engine settings are changed.**

## Scope and limits

- One coordinator / graph writer per trusted pool. Sharing a bucket does not make concurrent coordinators safe.
- No distributed build scheduling or in-flight execution deduplication. Reuse is of completed results that were actually exported, verified, and offered/merged in time.
- This does **not** transport arbitrary mounted cache-volume contents such as cppcheck's incremental database.
- S3 cleanup is best effort, not a transactional publish/delete guarantee. Cache loss may require recomputation; orphaned objects may remain.
- Pool members can access each other's cached results. There is no tenant isolation; do not expose the raw service publicly.
- No HA or 96-engine throughput claim. Socket counts, unit fixtures, and storage-only tests are not proof of 96 active mixed Dagger workloads.
- Missing and corrupt blobs must never be treated as verified data. A persistently corrupt existing object can require removal before a later export repairs it; see verification results for cases actually exercised.
