# dcache v1 implementation plan

> **For Hermes:** Use subagent-driven-development to execute task-by-task with local builders, then spec review and independent Claude quality review.

**Goal:** A runnable self-hosted Dagger remote-cache coordinator, unit-tested and exercised with real isolated beta.25 engines and S3-compatible storage.

**Architecture:** One trusted pool and one Go coordinator with upstream dagql/core at immutable a45241b20a5e07fbfc5954479ef0b61da843499c. SQLite checkpoints; direct presigned S3 PUT/GET. Best-effort GC; no HA, Postgres, Rust or engine modifications.

**Tech stack:** Go 1.26.6+; upstream Dagger beta.25; coder/websocket; OTLP protobuf; MinIO Go client for S3. Development sources on Mac, Linux compilation/tests on ethpow in /data/dcache-dev, isolated named containers only. Existing infrastructure untouched.

## Acceptance and evidence

- Unit tests for configuration, signatures, byte integrity, observations, WebSocket routing/continuation lifecycle, publication, renewal and checkpointing. Run actual RED before implementation and GREEN after.
- Functional test: engine A computes, coordinator exports/verifies/publishes; independent B obtains same correct output without executing target. Repeat with A stopped and coordinator checkpoint restored.
- Corruption before publication refused; post-publication corruption/missing bytes exercise actual fallback; expiry triggers renew or explicit unavailable. No mocked-engine result counts as real interoperability.
- 96 active mixed clients are distinct from 96 idle sockets. If hardware cannot run 96 real isolated engines, report that limit; protocol-load results are a separate label, not substitute evidence.
- Local-model implementation and test authors; parent independently runs tests; Claude Opus quality/security review after spec verification, fixes followed by re-review.

## Task sequence and exact ownership

1. Bootstrap: go.mod/go.sum, scripts/test-remote.sh, upstream pin and isolated test capacity. Parent owns scaffolding only.
2. S3 storage: internal/storage/store.go and store_test.go. Test wrong-size/hash, raw signed PUT and exact Range GET against httptest S3 or real isolated S3. No cloud resources.
3. Renewal capability: internal/renewal/token.go and token_test.go. HMAC pool/donor/ordered-layer binding, expiry, malformed input, timing-safe signature verification.
4. Graph adapter: internal/graph/graph.go and graph_test.go. Real upstream cache creation/core codec registration; attach/update/release observations; merge, owner relocation, publication, stored bundle, availability/placement and checkpoint/restore.
5. Telemetry: internal/telemetry/ingest.go and ingest_test.go. Protobuf/JSON OTLP decode, cache contract, encoded call lane, exact holding identity/session/generation, out-of-order completion bounded buffering. Identity before updates; no share-as-inventory shortcut.
6. WS peer: internal/peer/peer.go and peer_test.go. Basic auth, hello/welcome, separate request IDs, concurrent request/reply routing, bounded writes, pong/deadline, disconnect cancellation and exactly-one upload continuation.
7. Coordinator: internal/server/server.go and server_test.go. Compose peer, graph, telemetry, storage, signing; bounded fair export scheduler; send upload before slow commit/verify; publish only complete verified chains; targeted merge/offer; priority whole-request renewal deadline; statistics for tests.
8. CLI/config: cmd/dcache/main.go and internal/server/config.go (tests in subsequent scoped task). Validated env/flags, no secret logging; checkpoint/shutdown; /healthz and /metrics.
9. Functional harness: tests/functional/run.py and tests/functional/compose.yaml. Isolated containers and network; resource limits; real pinned engine and S3. Preserve actual observations/results under artifacts/. Unit false positives cannot satisfy functional acceptance.
10. Independent spec review then Claude quality review; local fixes and reruns. README/build instructions and reproducible checks. No publishing or production deployment.

## Per-task workflow

For each implementation task: write smallest failing behavioral test; run scripts/test-remote.sh ./internal/<package> (RED); implement minimum; run same scoped test (GREEN); save exact log. Agent may write only its assigned <=2 files. Other files are concurrent work; report, never fix. Parent runs cross-package integration only after contracts settle. Artifacts and diagnostics are real execution, never invented.

## Common module contracts

Module is github.com/snoby/dcache. Upstream checkout .upstream/dagger is pinned and replaced by go.mod, including sdk/go and distconsts module replacements. All Linux tests run with GOMAXPROCS=4 and go test -p 2 to avoid disturbing existing work. Agent test tool synchronizes project to dedicated /data/dcache-dev/service first.

Storage and renewal contracts are in CONTRACTS.md. Graph and server contracts will be anchored to actual upstream declarations before dispatch; never invent method signatures from the architecture prose.

## Constraints carried from user

96 concurrent client pods, some repeated jobs and many unrelated jobs, one shared trusted pool. Accepted best-effort cleanup and cache loss/orphans; never accept incorrect output. No automatic failover. Do not regress to generic backend research or another broad architecture review.
