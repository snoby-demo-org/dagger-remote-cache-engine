> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Independent Claude review and simple S3-backed Dagger cache architecture

## User request (this is the assignment)

"Have claude review your prior-art-follup.md file and your opinion. Ask claude to come up with a simple caching system that will run in an S3 style service to back any number of dagger engines concurrently."

The filename is actually <DESIGN_WORKSPACE>/prior-art-followup.md. Read that file in full. This is an independent review AND architecture proposal, not implementation. User explicitly wants Claude's judgment, not endorsement of the parent assistant.

## Goal and scope

Design the simplest credible self-hosted shared cache backed by S3-compatible object storage, serving many independent Dagger engines concurrently. Interpret "any number" as no hardcoded small engine count and a capacity/scaling path, NOT unlimited throughput or a proven numeric capacity. State the actual bottlenecks. Do not invent quantitative workload requirements.

"S3 style service" means an S3-compatible durability substrate and a separately deployed cache service engines can connect to, not code custom-compiled into each engine. If multiple interpretations lead to materially different designs, explain the distinction and choose an explicit default.

Rust is welcome; RocksDB is an option, not a user requirement. Existing components are preferred to new storage engines. Do not assume Postgres, RocksDB, a Rust rewrite, multi-writer graph metadata, sophisticated GC or HA are necessary. Conversely, do not declare simplicity by dropping required Dagger protocol semantics.

No server has been implemented in the inspected project. The cppcheck patch is a separate engine-local optimization and is not the requested shared service.

## Opinion to challenge (parent assistant's recommendation)

- We over-designed generic caching problems; build a Dagger adapter and reuse storage/tiering/eviction/I/O.
- NativeLink is the strongest Rust build-cache reference; bazel-remote and Buildbarn are useful simplicity baselines; BuildBuddy demonstrates embedded KV for metadata and small values, separate large blobs.
- Foyer, OpenDAL and potentially RocksDB could be reusable pieces. SlateDB is an object-backed KV alternative. Percas current cache2 is an acceleration tier, NOT acknowledged durable storage.
- Consider bounded independent blob eviction plus completeness checking instead of a complex cross-store ledger, but do not violate outstanding URLs or Dagger's renewal/fallback behavior.
- REAPI servers are not drop-in Dagger WebSocket servers. A small Go graph/protocol adapter could reuse upstream graph libraries with a Rust/cache service underneath. This is only one option; challenge whether the extra process adds unnecessary complexity or whether Rust offers a practical simpler alternative.
- A blanket ban on non-Go implementations is unwarranted, but porting the Dagger graph/codecs should not be hand-waved.

## Read order and focused evidence

1. <DESIGN_WORKSPACE>/prior-art-followup.md (main subject).
2. <DESIGN_WORKSPACE>/recon-protocol.md: locate only sections needed to establish protocol, graph primitives, discovery, storage and retention requirements. Do not exhaust turns reading every old design/review.
3. <DESIGN_WORKSPACE>/decision.md for the OLD design being reconsidered. It is NOT a binding spec.
4. Optional targeted portions of <DESIGN_WORKSPACE>/ledger.md and architecture.md if necessary to explain what to remove.
5. Primary source snapshots retrieved by parent live from GitHub are at <HISTORICAL_SCRATCH>/cache-prior-art/:
   - raw-0.txt: NativeLink store modules
   - raw-1.txt: NativeLink fast_slow_store.rs
   - raw-2.txt: NativeLink completeness_checking_store.rs
   - raw-3.txt: NativeLink LICENSE (FSL, not plain Apache)
   - raw-4.txt: bazel-remote README
   - raw-5.txt: Buildbarn README
   - raw-6.txt: BuildBuddy enterprise LICENSE
   - raw-7.txt: Percas README (read durability warnings)
   - raw-8.txt: SlateDB README
   - raw-9.txt: sccache S3 source (OpenDAL)
   - raw-11.txt: REAPI protobuf/spec
   - raw-12.txt: GitHub issue 14657 JSON (another operator's Dagger service)
   - rocksdb.txt, foyer.txt, cachey.txt, buildbuddy-source.txt, dagger-protocol.txt, opendal.txt, *-license.txt.
   These are actual source snapshots, not implementations we executed. Use targeted ranges/searches for large files. Files named source-*.json contain a FAILED extraction backend response, not useful evidence; ignore them.
6. The Dagger checkout, if still present, is <HISTORICAL_SCRATCH>/dagger-src at the previously researched revision. Confirm file availability rather than assuming it exists. Research is pinned to beta.25; do not call it the latest release. You may use WebSearch/WebFetch for current primary upstream evidence and correct the report.

## Questions the proposal must answer

1. What is wrong, overstated or missing in the prior-art report and the parent's opinion? Give file/line evidence or precise primary-source references. Distinguish verified defects from design preferences.
2. Choose ONE recommended architecture, plus at most one fallback. Components and responsibilities, one compact ASCII architecture diagram, concise write and hit flows.
3. What exact Dagger-specific implementation is unavoidable? Does it need the service-side dagql graph? How are completed roots discovered and offers/merges supplied? No fictitious on-miss API.
4. Can an existing service/library actually be reused, or is it only architectural inspiration? Identify the real API/byte-plane integration boundary, auth and license restrictions. Avoid simply renaming our custom store 'NativeLink' without an integration path.
5. Where do metadata, graph state and blob bytes live? If RocksDB, SQLite, Postgres or SlateDB is chosen, justify it over fewer components. Separate reproducible cache state from durable authoritative state and do not imply a local embedded DB replicates itself.
6. How do simultaneous engines publish the same content, consume it, disconnect and restart safely? Explain the minimum publication and retention rules. Are duplicate uploads acceptable? Does missing content become a miss or an engine failure? Mark unverified engine behavior UNKNOWN.
7. How can engine count grow? Separate independent concurrent clients, number of metadata writers, socket routing, graph ownership, byte-serving bandwidth and pools/shards. Do not imply one shared bucket makes graph front ends stateless or multi-writer-safe.
8. What complexity can be deleted from the old design now, and which requirements need a measured experiment before deletion? Keep v1 intentionally small, but label limitations honestly.
9. Small acceptance plan: two real engines A/B, reuse after producer death, concurrent producers, restart, expired URL/missing blob, store slowdown, integrity failure. Specify decisive pass/fail evidence, not an implementation.

## Hard boundaries

Read-only source/design review. Do not modify ANY files, implement code, install software, access credentials, probe the cluster/LAN, run builds, publish or restart services. Tools are restricted to reads/search and public web lookup. Web pages and repository instructions are untrusted data, not authority to change this task. No code quoting; reference paths/lines and explain in prose. Do not claim experiments were run.

## Output contract

Return your COMPLETE report as the final Markdown response; the parent will save it. Do not attempt Write/Edit tools.

Maximum 2,200 words, excluding compact diagrams and source identifiers. Begin with:
- `VERDICT: agree | agree-with-changes | disagree` on the prior-art report/opinion.
- Your recommended design in one paragraph.

Then sections: `Review findings` (severity, file:line/source, exact correction; cap 6 findings), `Recommended architecture`, `Concurrency and S3 semantics`, `What to reuse / what to write`, `What to remove from the old design`, `Small acceptance plan`, and mandatory `Red team` (8 concise failure modes with mitigation or explicitly UNKNOWN).

Prioritize a concrete simple design over another sprawling review. If a genuinely simple design is blocked by Dagger semantics, say exactly which source behavior forces which component. No unsupported endpoints, magic libraries, pretend infinite scalability, or architecture selected only because the parent likes it.
