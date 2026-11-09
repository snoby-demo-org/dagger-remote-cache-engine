# Verification — dcache prototype

Verified on November 8, 2026. This is an implemented and exercised **external network service**, not an engine fork, linked-in engine extension, or cppcheck cache-volume patch. Only the coordinator imports pinned Dagger libraries. Nothing was published, tagged, deployed to production, or changed in VerusCoin's CI pins.

## Exact baseline

- Source checkout: `github.com/dagger/dagger` commit `a45241b20a5e07fbfc5954479ef0b61da843499c` (`v1.0.0-beta.25`).
- Unmodified test engine image: `registry.dagger.io/engine:v1.0.0-beta.25`; image IDs retained in execution JSON.
- Linux/amd64 execution on ethpow, **Go 1.27.3**. The module declares Go 1.26.6 as its minimum; that older toolchain was not independently exercised.
- Isolated, digest-pinned RustFS S3 fixture. No production S3 bucket or production engine was used.
- Immutable Alpine workload image; fresh, separate engine containers and named state volumes.

## Executed checks

| Check | Actual result | Evidence |
|---|---|---|
| `make build` | PASS; runnable Linux binary | `verification/make-build-test-race.log` |
| `make test` | PASS, seven packages | same |
| `make race` and final readonly-module race run | PASS; **125 top-level Go tests**, 202 passing events including subtests | `verification/go-tests.json`, `go-race.jsonl` |
| `go vet ./...` | PASS | `verification/vet-repeat-storage.log` |
| Telemetry tests repeated 20 times | PASS | same |
| Python harness/helper regressions | **49 tests, PASS** | `verification/python-tests.log` |
| Real authenticated S3 round-trip | PASS: >2 MiB upload, exact ranged GET, streamed SHA256/size validation, same-size corruption rejection, deletion | `verification/vet-repeat-storage.log` |
| Independent Claude Opus reviews | No remaining blockers in reviewed scope after fixes; static review, not independent execution | `verification/reviews.json` |

The documented `make` commands were exercised without `-mod=mod`; missing SDK checksum metadata was found and fixed before delivery.

## Real engine acceptance

`verification/functional.json` records exit code **0** and all seven stages passing:

| Stage | Result | External execution counter | Wall time |
|---|---|---:|---:|
| A: fresh producer | Correct result | 1 | 3.37 s |
| B: fresh independent consumer, producer removed | Correct cached result | **1** | **0.13 s** |
| C: same consumer, local warm | Correct cached result | 1 | 0.09 s |
| D: coordinator restarted, another fresh consumer | Correct cached result | **1** | 0.11 s |
| S3 deletion | All five enumerated fixture objects removed | — | — |
| E: missing-object fallback | Correct recomputed result | **2** | 1.55 s |
| F: corrupt-object fallback | Correct recomputed result | **3** | 1.54 s |

Consumers use distinct downstream file-name transformations to demand snapshot bytes rather than only reusing a previously cached scalar. Producer removal is checked by Docker inspect before B runs. These are tiny synthetic fixtures, **not VerusCoin/cppcheck benchmarks or general speedup claims**.

`verification/mixed4.json` records **four completed independent consumers, measured query overlap of four, and exit code 0**. One shared job remained at execution count 1; all three distinct jobs executed exactly once and returned their own expected output. Overlap comes from recorded query intervals, not the thread barrier. Actual coordinator debug state is recorded separately from host thread activity.

## Fixture GC and cleanup

Default engine free-space GC initially pruned the prewarmed results before the mixed test started. Full engine logs showed `dagql pruned result` and a roughly 52 GB reclaim target. Test engines therefore use explicit bounded GC settings: `--oci-worker-gc-keepstorage=100,0,1000` MB. GC remains enabled. Arguments and the reason are in evidence; **production engine settings were not changed**.

Both final harness runs checked Docker removal statuses and exact-name absence for containers, named volumes, and networks, with empty cleanup error lists. Older diagnostic fixtures and the bootstrap test stack were subsequently removed; `verification/diagnostic-cleanup.json` records the removal and read-back of 46 containers, 72 volumes, and 12 networks with none remaining. Root-owned forensic state directories may remain under `/data/dcache-dev/artifacts`; they are not live services and are explicitly reported as retained.

## Not verified / limitations

- **96-engine capacity: NOT_RUN**, not PASS. Even after diagnostic cleanup, the resource guard found 18,816,016,384 available bytes versus 28,185,722,880 required for 96 consumers plus producer/headroom. No 96-client performance or production-sizing claim is made. See `verification/capacity96.json`.
- Real expired-URL renewal, prolonged transfer failures, crash-at-every-step recovery, and sustained large build workloads were not exercised end-to-end. Renewal capabilities, binding, validation and deadline paths have unit coverage.
- One trusted pool, one graph writer; no HA, multi-tenant isolation, or transactional S3 reclamation. The service is not a distributed executor.
- Arbitrary mounted cache-volume contents are not shared by this implementation.
- Observation/export capacity is bounded and can drop new observations; cache misses and recomputation remain possible. The mixed run is a correctness/interoperability check, not a throughput guarantee.
- Existing corrupt objects may require manual removal before repair by a later export; empty-object handling and several non-blocking cleanup/retention bounds remain prototype hardening items in the review records.
- Native macOS builds are unsupported by imported Linux engine libraries. S3 compatibility was exercised against RustFS, not every S3 provider.

Build/run instructions and all required configuration are in `README.md`. The tested Linux binary is at `ethpow:/data/dcache-dev/service/bin/dcache`; source is retained in this project and the downloadable source archive.

## Documentation-completeness follow-up

The [documentation index](docs/README.md) now links the implemented design, dedicated S3 and engine integration guides, environment templates, complete test inventory, and all original historical design/research/review records. All 48 original Markdown documents and seven diagnostic files are covered by the historical manifest; private development paths/addresses are replaced with documentation placeholders. Historical proposals are not claimed as implemented features.

All 30 original service test files remain included. One new test file adds three offline documentation regressions; the current Python suite is **52 tests, PASS**, recorded separately in [documentation-tests.log](verification/documentation-tests.log). The original 49-test evidence above is historical and has not been relabeled. No runtime Go code or original tests changed in this follow-up.

The setup audit corrected the README's remote-cache URL to the service **base URL**, since the pinned engine appends `/v1/engine`. It also corrected the access-control description: coordinator `/metrics` is unauthenticated, while `/debug/state` requires the pool token. The S3 guide explicitly documents unconditional bucket initialization and provider limitations, including the AWS `us-east-1` existing-bucket ACL-reset caution. No real AWS deployment, new real-engine run, or 96-client capacity pass is implied by these offline documentation checks.
