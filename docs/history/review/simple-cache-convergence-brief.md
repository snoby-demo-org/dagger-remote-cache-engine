> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Converge Claude's simple cache design; do not reopen architecture

Resume the independent review/design session. User has accepted YOUR architecture as the baseline. This is a focused design resolution and implementation brief, NOT an implementation assignment.

## User's latest direction

"Let's start with Claude's design for point A i don't really care, for point B. we may / will have 96 different pods hitting this storage interface, some with the same jobs so some cache can hit. Many not sharing anything at all. For point C you two can debate it."

Interpretation:
- Point A was parent's concern about GC/late uploads. User accepts the cache tradeoff. Do not turn GC edge cases into another approval gate, introduce generations/Postgres, or repeat architecture red-team rounds. Document best-effort reclamation and accepted cache loss/orphan bytes. This is NOT permission to serve bytes failing integrity validation.
- Point B was parent's concern about overlapping metadata writers, NOT the number of client pods. Actual workload requirement is 96 concurrent client pods (design for up to 96 independent engines as conservative assumption). Some repeated jobs, many unrelated jobs. Prefer one shared trust pool by default so overlap can hit, with trust splitting only if actually required. Do not silently make 96 pools or 96 metadata writers. No HA requirement supplied. One coordinator per pool is accepted; capacity still must be tested.
- Point C is for us to settle from source: discovery and export/upload ordering. Deliver a concrete build brief, not more open architecture alternatives.

## Accepted baseline

One Go coordinator per pool; reuse pinned upstream dagql + core; SQLite graph checkpoint on persistent storage; S3-compatible byte store; signed engine-to-S3 PUT/GET; no Postgres, RocksDB, Rust tier, NativeLink, leader-election project or distributed execution. Single active coordinator, no automatic failover requirement. Engine data is cacheable work, not authoritative user data. No production rollout, engine upgrade or service restart authorized.

## Parent's concrete challenges for resolution (give agree/reject + source evidence)

C1. Your original report write-flow step 1 says completed result discovered through "share event or span parts". recon-protocol.md:59 says lazy/share updates never create a holding; ordinary call facts establish identity. architecture.md:223-224 says engine.share is imported rows, not inventory of freshly executed roots. Need explicit ordinary call identity ingestion -> existing holding -> lazy completed-part update -> export. If the prior recon is wrong, inspect pinned producer code and explain exactly why.

C2. Original write-flow step 3 does CommitValueMerge before sending upload. The engine retains limited export providers/slots until upload continuation. recon-protocol.md:380 says commit takes graph write lock. Prior design architecture.md:225-227 sends upload continuation before potentially slow commit. We do not need a fabricated mandatory protocol 5-second deadline: choose a bounded service watchdog policy, always consume provider via exactly one upload request (empty map when declining), and avoid graph lock or full verify reads gating this continuation. Can prepare/validate -> reserve/sign -> send upload -> graph commit -> verify -> publish work with the real APIs? Address failures, release and no double continuation without durable retry machinery.

C3. User has 96 concurrent clients with mixed unrelated work. Specify the minimal bounded per-client queues/export limits and global concurrency limits, fairness, duplicate-upload tolerance, and how merge/offer selection avoids broadcasting every unrelated graph record to every engine. Initial limits are configuration proposals, NOT measured capacity. Distinguish metadata critical sections from normal I/O and full read-back verification; don't hold a global mutex during all S3 transfer time. If GC holds M across DELETE in your design, identify its global stall risk and keep best-effort GC out of the initial performance-critical path.

C4. T1 in original report treats hit.remote=true as sufficient. recon says origin marker persists; require observable avoidance of intended expensive execution plus S3 fetch/install evidence and identical output, not origin alone. T7 mixed corrupt-before-publication with corrupt-after-publication; split expected outcomes.

## Evidence available

Read original <DESIGN_WORKSPACE>/review/claude-simple-cache.md and primary snapshot <HISTORICAL_SCRATCH>/cache-prior-art/dagger-protocol.txt as needed. Focused recon: <DESIGN_WORKSPACE>/recon-protocol.md lines 38-72, 343-407; <DESIGN_WORKSPACE>/architecture.md lines 219-242.

Pinned primary source can be fetched publicly at https://raw.githubusercontent.com/dagger/dagger/v1.0.0-beta.25/engine/telemetryattrs/attrs.go and corresponding dagql/cache_remote_entries.go, engine/server/engine_events.go, engine/server/remote_cache_protocol.go, engine/remotecache/session.go, dagql/cache_value_merge.go. No local full dagger checkout is assumed to exist. Use targeted reads/fetches, not whole-file floods. Source matters more than prior narrative. UNKNOWN for anything unverified.

## Boundaries and final output

Read/search/public-web tools only. No writes, source changes, deployment, shell commands, credentials, network probes, installations or implementation. Parent saves your final response.

Return COMPLETE Markdown, max 1,500 words. First: `CONVERGED: yes|no` and any truly blocking incompatibility (not generic production-hardening warnings). Then:
1. C1-C4 resolution: agree/reject, evidence, exact decision. No code quotes.
2. Final simple architecture and corrected discovery/write/hit flow.
3. 96-client operating model: one pool, what is concurrent, what is serialized, candidate bounded controls and stress-test acceptance. No invented benchmark result.
4. Small numbered implementation sequence ending in real two-engine reuse and a 96-client mixed-workload test; distinguish connection count test from active-workload capacity proof.
5. Accepted tradeoffs: best-effort cache cleanup, no HA, cross-engine key compatibility, volumes not shared. List only remaining actual prerequisites.

Do not add an eight-risk red-team section or restart a broad review. User has chosen the architecture; settle the details and make it buildable.
