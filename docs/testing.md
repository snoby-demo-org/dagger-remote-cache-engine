# Tests, acceptance commands, and complete inventory

All coordinator tests are checked in, not just execution logs. The inventory below is generated from the service source tree. The legacy diagnostic probes are additionally preserved under [history](history/README.md), but are not mixed into the supported suite.

## Prerequisites and safety

- Run Go builds/tests on **Linux**. The imported engine code is not a native macOS build target. The Python helper/documentation tests can run without an engine.
- Bootstrap the exact upstream dependency using `make bootstrap`; do not edit the upstream pin to make a different engine version appear compatible.
- Functional tests need local Docker, privileged disposable engine containers, enough RAM/disk, and network access to pull their pinned images/base layers. Use an isolated development host, never production engine names/data.
- Functional drivers provision their own uniquely prefixed RustFS bucket/storage, coordinator, counter server, and separate engine volumes. They deliberately delete/corrupt fixture objects to verify fallback.
- Fixed fixture credentials in test code are public dummy values for disposable stores, not deployment credentials. Never reuse them in production.

## Unit, race, vet, and documentation checks

```sh
make build
make test
make race
GOMAXPROCS=4 go vet -p 2 ./...
python3 -m unittest discover -s tests/functional -p 'test_*.py' -v
```

The Go suite covers graph identity/merge/expiry, renewal capabilities, authenticated signed storage operations and streaming verification, peer framing/lifecycle/errors, OTLP reconciliation/cleanup, bounded export/offer coordination, and CLI validation/timeouts. Python tests validate harness behavior, query byte-demand/immutable-image construction, readiness, corruption scenarios, measured concurrency and cleanup. The documentation regressions also check engine base-URL configuration, separation of server-only credentials, inventory coverage, and current relative links.

The original acceptance run recorded **125 top-level Go tests** (202 pass events including subtests) and **49 Python tests**; its unaltered result meaning is retained in [VERIFICATION.md](../VERIFICATION.md) and [verification logs](../verification/python-tests.log). This submission adds three offline documentation checks; the updated Python run is recorded separately in [documentation-tests.log](../verification/documentation-tests.log). These additional checks do not imply a new real-engine or 96-client run.

## Real authenticated S3 regression

Provision an isolated S3 fixture with the fixed test-only access/secret values used in [live_test.go](../internal/storage/live_test.go) and [README](../README.md). The test creates the `dcache-live-test` bucket and writes/overwrites/deletes an object. The endpoint variable is a test interface, not the coordinator's deployment configuration:

```sh
DCACHE_TEST_S3=http://YOUR_DISPOSABLE_FIXTURE:9000 \
  GOMAXPROCS=4 go test -p 2 -tags integration ./internal/storage \
  -run '^TestRealS3RoundTrip$' -count=1 -v
```

An unset `DCACHE_TEST_S3` skips this tagged test. A skipped test is **not a successful S3 interoperability run**. The recorded real run verifies a payload larger than 2 MiB, exact ranged response and byte count, full SHA256/size verification, same-size corruption rejection and deletion. It does not test all object-store providers, AWS IAM deployment, STS, or KMS.

## Isolated-engine acceptance: producer disappears

```sh
make functional
# Equivalent after make build:
python3 tests/functional/run.py \
  --binary "$(pwd)/bin/dcache" \
  --output "$(pwd)/artifacts/functional" \
  --engine-image registry.dagger.io/engine:v1.0.0-beta.25 \
  --scenario all
```

Acceptance requires correct output **and external execution-counter evidence**:

1. A executes the immutable workload once and exports completed results.
2. A is stopped/removed, with absence recorded; B has its own empty engine state.
3. B requests bytes through a distinct downstream read transformation and receives the correct output without incrementing the producer execution counter.
4. C checks B's local-warm behavior.
5. D restarts the coordinator with its persistent checkpoint and uses another fresh consumer; the execution counter still stays at one.
6. Delete all enumerated fixture blobs; E recomputes safely and increments the counter to two.
7. Corrupt fixture blobs; F recomputes safely and increments the counter to three.

These stage letters describe the recorded harness, not a claim that every production failure is covered. Results include container image identity, queries/timings, counters, coordinator state and teardown evidence. A fast scalar read, idle socket or merge count alone does not prove transferred-byte reuse.

## Mixed shared/unique consumers and capacity guard

```sh
mkdir -p artifacts/mixed
python3 tests/functional/mixed_load.py \
  --binary "$(pwd)/bin/dcache" --output "$(pwd)/artifacts/mixed" \
  --engine-image registry.dagger.io/engine:v1.0.0-beta.25 \
  --clients 4 --engine-memory-mb 256
```

The measured successful case has four overlapping independent consumer queries plus a separately prepared producer. One job reuses the producer's shared output while three unique jobs execute once each. `peak_inflight` is derived from actual query intervals, not barrier-thread count; authenticated coordinator state is stored separately.

Change to `--clients 96` only on a sufficiently provisioned isolated host. The preflight memory guard exits **2 / NOT_RUN** before creating resources when capacity is insufficient. The checked-in [96-client record](../verification/capacity96.json) is that guard result, **not a capacity pass**. There is no verified 96-engine throughput or latency figure.

## GC, cleanup, and remaining gaps

Both real drivers use a digest-addressed Alpine image; a mutable tag can make recipe identity session-specific in the pinned engine. Disposable engines use explicit bounded `--oci-worker-gc-keepstorage=100,0,1000` settings, with GC still enabled, to avoid host free-space pressure immediately pruning their prewarmed results. The exact arguments/reason are in evidence. No production engine policy is changed.

Normal teardown checks Docker removal return codes and reads back absence for owned containers, named volumes and networks. Cleanup failures set a nonzero result before final JSON is written. `--keep` intentionally retains that fixture for diagnosis; clean up its exact owned resources afterward rather than issuing global prune commands. Root-owned forensic state files can remain and are recorded, not claimed deleted.

Not covered end-to-end: sustained large real build workloads, 96 clients, real expired-URL renewal over a long-running session, every crash boundary, every S3 provider, HA, or multi-tenant isolation. Unit renewal tests are not the same as an expired-URL end-to-end test. See [current design](design.md) and [VERIFICATION](../VERIFICATION.md).

## Complete checked-in test inventory

**21 Go test files and 10 Python test files (31 total).** The integration-tagged S3 regression is included in the Go files. Harness entrypoints are listed separately below.

### Go tests

- [`cmd/dcache/main_test.go`](../cmd/dcache/main_test.go)
- [`cmd/dcache/timeouts_test.go`](../cmd/dcache/timeouts_test.go)
- [`internal/graph/graph_test.go`](../internal/graph/graph_test.go)
- [`internal/graph/validation_test.go`](../internal/graph/validation_test.go)
- [`internal/peer/compression_test.go`](../internal/peer/compression_test.go)
- [`internal/peer/peer_test.go`](../internal/peer/peer_test.go)
- [`internal/peer/remote_error_test.go`](../internal/peer/remote_error_test.go)
- [`internal/renewal/token_test.go`](../internal/renewal/token_test.go)
- [`internal/server/coalescing_test.go`](../internal/server/coalescing_test.go)
- [`internal/server/export_slots_test.go`](../internal/server/export_slots_test.go)
- [`internal/server/pipeline_test.go`](../internal/server/pipeline_test.go)
- [`internal/server/pipeline_validation_test.go`](../internal/server/pipeline_validation_test.go)
- [`internal/server/server_test.go`](../internal/server/server_test.go)
- [`internal/server/timeouts_test.go`](../internal/server/timeouts_test.go)
- [`internal/storage/live_test.go`](../internal/storage/live_test.go)
- [`internal/storage/store_test.go`](../internal/storage/store_test.go)
- [`internal/telemetry/cleanup_regression_test.go`](../internal/telemetry/cleanup_regression_test.go)
- [`internal/telemetry/deferred_identity_test.go`](../internal/telemetry/deferred_identity_test.go)
- [`internal/telemetry/ingest_test.go`](../internal/telemetry/ingest_test.go)
- [`internal/telemetry/lazy_carrier_test.go`](../internal/telemetry/lazy_carrier_test.go)
- [`internal/telemetry/lifecycle_validation_test.go`](../internal/telemetry/lifecycle_validation_test.go)

### Python tests

- [`tests/functional/test_byteplane.py`](../tests/functional/test_byteplane.py)
- [`tests/functional/test_cleanup.py`](../tests/functional/test_cleanup.py)
- [`tests/functional/test_corrupt_scenario.py`](../tests/functional/test_corrupt_scenario.py)
- [`tests/functional/test_documentation.py`](../tests/functional/test_documentation.py)
- [`tests/functional/test_fixture_gc.py`](../tests/functional/test_fixture_gc.py)
- [`tests/functional/test_image_pin.py`](../tests/functional/test_image_pin.py)
- [`tests/functional/test_mixed_load.py`](../tests/functional/test_mixed_load.py)
- [`tests/functional/test_readiness.py`](../tests/functional/test_readiness.py)
- [`tests/functional/test_run.py`](../tests/functional/test_run.py)
- [`tests/functional/test_workload_pin.py`](../tests/functional/test_workload_pin.py)

### Harness entrypoints and fixtures

- [`tests/functional/run.py`](../tests/functional/run.py)
- [`tests/functional/mixed_load.py`](../tests/functional/mixed_load.py)
- [`tests/functional/discovery.graphql`](../tests/functional/discovery.graphql)
- [`tests/functional/discovery-collector.yaml`](../tests/functional/discovery-collector.yaml)
- [`Makefile`](../Makefile)
- [`scripts/bootstrap.sh`](../scripts/bootstrap.sh)
