# Documentation index

## Current implementation and operation

| Document | What it covers |
|---|---|
| [Current design](design.md) | Implemented architecture, graph semantics, discovery/export/hit/renewal, concurrency, persistence and accepted limits |
| [S3 integration](s3.md) | Bucket operations/permissions, static credentials, exact key layout, two endpoint scopes, signing, TLS, connectivity and provider limitations |
| [Engine integration](engines.md) | Pinned compatibility, correct service base URL, engine versus CLI/session settings, Docker example, Kubernetes environment fragment and diagnostics |
| [Tests](testing.md) | All checked-in test files, runnable commands, disposable fixtures, acceptance evidence and what remains unverified |
| [Verification report](../VERIFICATION.md) | Exact original executed results; 96-client capacity remains NOT_RUN |
| [Module contracts](../CONTRACTS.md) | Original implementation boundaries and module interfaces; use current source for exact API signatures |
| [Implementation plan](implementation-plan.md) | Implementation sequence and acceptance intentions, not a substitute for execution evidence |
| [Functional review resolution](functional-review-resolution.md) | Acceptance-harness review findings and their disposition |
| [Cleanup review resolution](cleanup-review-resolution.md) | Scoped resource deletion/read-back and result-status corrections |
| [Coordinator environment template](../examples/coordinator.env.example) | Server-only S3/authentication/database settings; required secret fields deliberately blank |
| [Engine environment template](../examples/engine.env.example) | Engine-only base URL/token/telemetry settings; no S3 or HMAC credentials |

**Authority:** current code plus the executed verification evidence determine what exists. These guides do not assert that all earlier design proposals were built. The original verification run had 49 Python tests; three added offline documentation checks are recorded separately. Runtime source and protocol pin are unchanged by this documentation-completeness submission.

## Complete design history

The [historical archive](history/README.md) contains all original Markdown architecture, ledger, eviction, cost/latency/provider research, decision revisions, architecture briefs, peer reviews and review baselines, plus original diagnostic probes/configuration. Its [manifest](history/manifest.json) records complete source coverage and provenance hashes.

History is retained for reasoning and rejected alternatives, **not as current runbook instructions**. Private development infrastructure and workstation paths are replaced with placeholders. Earlier multi-writer/HA designs, provider/version claims, timings and “unknown/not implemented” statements must be read with their historical status. The current system is one coordinator per trusted pool, not the earlier proposed distributed ledger.
