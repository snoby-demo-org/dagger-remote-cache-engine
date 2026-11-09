# Historical design, research and review archive

This directory preserves **all 48 original Markdown design/research/brief/review documents** from the design workspace, including earlier review baselines, plus seven diagnostic source/configuration files. The [manifest](manifest.json) lists every original relative path, original hash, and published hash. It records coverage rather than claiming these proposals are implemented.

## Read current documentation first

- [Current implemented architecture](../design.md)
- [S3 setup and limitations](../s3.md)
- [Compatible engine setup](../engines.md)
- [Current tests and inventory](../testing.md)
- [Executed verification](../../VERIFICATION.md)

Earlier designs explore multi-writer ledgers, stronger eviction/fencing, speculative provider support, large-client budgets, and acceptance goals that were later simplified or not implemented. Historical “latest version,” “not implemented,” “unknown,” timing and resource claims describe the time of those notes, not the present implementation. The converged small-service design is important context, not an exact runtime feature list.

Private hostnames, LAN addresses and workstation paths have been replaced with reserved documentation addresses/placeholders. Headers are added to historical Markdown; original source hashes allow provenance comparison without publishing private values. Review prompts/briefs are archived as data, not operating instructions. Code excerpts and upstream file-line references retain their historical meaning.

## Key records

- [Source-grounded protocol reconnaissance](recon-protocol.md)
- [Prior-art reconnaissance](recon-prior-art.md) and [follow-up comparison](prior-art-followup.md)
- [Original architecture](architecture.md), [ledger](ledger.md), [decision](decision.md), [metrics/eviction](metrics-and-eviction.md), [latency/provider discussion](latency-and-azure.md)
- [Simplified design review](review/claude-simple-cache.md) and [converged baseline](review/claude-simple-cache-converged.md)

## Diagnostic safety and validity

Historical Gate A/Tempo probes, the OTLP collector/analyzer and all three S3 latency-probe versions are preserved for completeness. They are **not** the supported service tests and have not been rerun for this documentation commit. Some use old infrastructure paths, fixed diagnostic resource names, obsolete assumptions, or write objects; do not run them against production data. The old OTLP analyzer expects `durationNanos` and may report zero durations for ordinary OTLP spans that use start/end timestamps. Its timing output must not be treated as acceptance evidence. S3 probe versions are historical comparisons, not storage-backend certifications.

The separate cppcheck release patch and its application script are intentionally excluded: they are not this coordinator or its tests. Production credentials, private configuration, upstream source checkouts, binaries and raw agent runtime transcripts are not part of this archive.

## Complete archived-file inventory

- [`architect-brief.md`](architect-brief.md) — historical-design
- [`architecture.md`](architecture.md) — historical-design
- [`decision.md`](decision.md) — historical-design
- [`gate_a.md`](gate_a.md) — historical-design
- [`harness/README.md`](harness/README.md) — historical-design
- [`latency-and-azure.md`](latency-and-azure.md) — historical-design
- [`ledger.md`](ledger.md) — historical-design
- [`metrics-and-eviction.md`](metrics-and-eviction.md) — historical-design
- [`prior-art-followup.md`](prior-art-followup.md) — historical-design
- [`recon-prior-art.md`](recon-prior-art.md) — historical-design
- [`recon-protocol.md`](recon-protocol.md) — historical-design
- [`review/baseline/architecture.md`](review/baseline/architecture.md) — historical-design
- [`review/baseline/decision.md`](review/baseline/decision.md) — historical-design
- [`review/baseline/latency-and-azure.md`](review/baseline/latency-and-azure.md) — historical-design
- [`review/baseline/metrics-and-eviction.md`](review/baseline/metrics-and-eviction.md) — historical-design
- [`review/baseline-r2/architecture.md`](review/baseline-r2/architecture.md) — historical-design
- [`review/baseline-r2/decision.md`](review/baseline-r2/decision.md) — historical-design
- [`review/baseline-r2/latency-and-azure.md`](review/baseline-r2/latency-and-azure.md) — historical-design
- [`review/baseline-r2/metrics-and-eviction.md`](review/baseline-r2/metrics-and-eviction.md) — historical-design
- [`review/baseline-r2/revision-r1.md`](review/baseline-r2/revision-r1.md) — historical-design
- [`review/baseline-r4/architecture.md`](review/baseline-r4/architecture.md) — historical-design
- [`review/baseline-r4/decision.md`](review/baseline-r4/decision.md) — historical-design
- [`review/baseline-r4/latency-and-azure.md`](review/baseline-r4/latency-and-azure.md) — historical-design
- [`review/baseline-r4/ledger.md`](review/baseline-r4/ledger.md) — historical-design
- [`review/baseline-r4/metrics-and-eviction.md`](review/baseline-r4/metrics-and-eviction.md) — historical-design
- [`review/baseline-r4/revision-r3.md`](review/baseline-r4/revision-r3.md) — historical-design
- [`review/baseline-r4/revision-r4.md`](review/baseline-r4/revision-r4.md) — historical-design
- [`review/claude-r3.md`](review/claude-r3.md) — historical-design
- [`review/claude-r4.md`](review/claude-r4.md) — historical-design
- [`review/claude-r5.md`](review/claude-r5.md) — historical-design
- [`review/claude-review.md`](review/claude-review.md) — historical-design
- [`review/claude-simple-cache-converged.md`](review/claude-simple-cache-converged.md) — historical-design
- [`review/claude-simple-cache.md`](review/claude-simple-cache.md) — historical-design
- [`review/gpt-r3.md`](review/gpt-r3.md) — historical-design
- [`review/gpt-r4.md`](review/gpt-r4.md) — historical-design
- [`review/gpt-r5.md`](review/gpt-r5.md) — historical-design
- [`review/gpt-review-brief.md`](review/gpt-review-brief.md) — historical-design
- [`review/gpt-review.md`](review/gpt-review.md) — historical-design
- [`review/r3-brief.md`](review/r3-brief.md) — historical-design
- [`review/r4-brief.md`](review/r4-brief.md) — historical-design
- [`review/r5-brief.md`](review/r5-brief.md) — historical-design
- [`review/review-brief.md`](review/review-brief.md) — historical-design
- [`review/simple-cache-architect-brief.md`](review/simple-cache-architect-brief.md) — historical-design
- [`review/simple-cache-convergence-brief.md`](review/simple-cache-convergence-brief.md) — historical-design
- [`revision-r1.md`](revision-r1.md) — historical-design
- [`revision-r2.md`](revision-r2.md) — historical-design
- [`revision-r3.md`](revision-r3.md) — historical-design
- [`revision-r4.md`](revision-r4.md) — historical-design
- [`gate_a.py`](gate_a.py) — historical-diagnostic
- [`gate_a2.py`](gate_a2.py) — historical-diagnostic
- [`s3_latency_probe.py`](s3_latency_probe.py) — historical-diagnostic
- [`s3_latency_probe_v2.py`](s3_latency_probe_v2.py) — historical-diagnostic
- [`s3_latency_probe_v3.py`](s3_latency_probe_v3.py) — historical-diagnostic
- [`harness/analyze.py`](harness/analyze.py) — historical-diagnostic
- [`harness/otel-collector.yaml`](harness/otel-collector.yaml) — historical-diagnostic
