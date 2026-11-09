> **Historical design/review record, not current implementation status.** See [current design](../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Gate A — can we measure addressable cache opportunity from existing telemetry?

Date 2026-11-07. Cluster: homelab k3s, Dagger v0.21.9 DaemonSet (3/3, 27 days up). Backend: Tempo at `192.0.2.11:3200` (the HelmRelease exports OTLP to the in-cluster collector, which forwards there).

## Verdict

**Gate A cannot be answered from current telemetry — but not for the reason the brief feared.** Spans *do* reach our own endpoint; what is missing is the cache contract on this engine version, and any workload big enough to measure.

| Question | Evidence | Answer |
|---|---|---|
| Do engine spans reach a self-hosted endpoint? | `service.name` tag values include `dagger-engine`; 1,000 traces fetched, all resolved | **YES.** The discovery prerequisite of round-1 F5 is satisfied *for the OTLP side*. The engine honours `OTEL_EXPORTER_OTLP_ENDPOINT` pointed at our collector. |
| Are the `dagger.io/cache.*` attributes present? | `/api/search/tag/dagger.io%2Fcache.outcome/values` → `{"tagValues":[]}`; same for `cache.pairing_digest`. A full trace shows attributes `dagger.io/dag.digest`, `dagger.io/dag.call`, `dagger.io/dag.inputs`, `dagger.io/dag.output`, `dagger.io/ui.*`, `rpc.*` and **no cache attribute at all** | **NO.** v0.21.9 does not emit the cache contract. The contract in the recon is from the beta source. |
| Is there a workload to measure? | 7 days, `limit=1000`: **995 traces are `dagger core version`** (p50 547 ms, p95 720 ms); the remaining 5 include one 1,467 s trace | **NO.** The cluster runs version probes, not builds. There is nothing whose repetition would demonstrate a hit rate. |
| Can the opportunity be computed anyway from `dag.digest` + `dag.call`? | 200 recent traces → 1 distinct `(call, digest)` key, all duration ≈ 0 | **NO.** Every sampled trace is the same trivial probe. |

## What this means for the decision

The go/no-go question — "is there addressable compute worth building a shared cache for?" — cannot be answered on the live cluster, because the live cluster is not doing the work. Measuring `dagger core version` 1,000 times tells us nothing about CI wall time.

So Gate A does not fail on a *design* premise (discovery works); it fails on **evidence availability**. The design's own text anticipated this: if v0.21 lacks the contract, the analysis must be done on a beta.25 shadow engine.

## What Gate A now requires (the actual next step, not another review)

A **controlled beta.25 run with telemetry captured**, in one of two places:

1. **Local Docker on the Mac** (fastest, most isolated): `registry.dagger.io/engine:v1.0.0-beta.25`, `OTEL_EXPORTER_OTLP_ENDPOINT` at a local OTLP/HTTP receiver that writes spans to SQLite, then run one real pipeline N times (cold and warm) and compare:
   - which `dagger.io/cache.*` attributes actually appear on `executed` vs lazy spans (`cache.pairing_digest`, `cache.outcome`, `cache.offer.part`, `cache.parts`, `cache.session.spans`, `cache.replacements`);
   - whether `cache.offer.part` carries a usable part identity (the review's F12 says it carries only a `PartAddress`; this run settles it empirically);
   - the repeat rate across sessions on a real pipeline, which is the number Gate A exists to produce.
2. **A shadow engine on the cluster** (closer to production topology, but it changes a 27-day-stable DaemonSet; requires a separate Deployment, not a rollout of the live one).

Recommendation: **option 1 first.** It costs a ~1 GB image pull and a local receiver; it modifies nothing in the cluster, and it answers both open items at once (contract presence + repeat rate).

## What is already settled by this probe

- The telemetry path is real: engine → collector → Tempo, with `service.name=dagger-engine` and per-node `service.instance.id`.
- v0.21.9's span set is insufficient for any hit/miss metric — a concrete confirmation of the design's claim that metrics must come from the beta contract, not from the running version.
- The cluster's Dagger usage is trivial; any gate number must come from a synthetic workload, and must then be re-measured on the real CI pipelines in Azure.

## Reproduce

```bash
cd <DESIGN_WORKSPACE>
python3 gate_a2.py 7          # trace-name distribution + dag-attribute presence
curl -s 'http://192.0.2.11:3200/api/search/tag/service.name/values'
curl -s 'http://192.0.2.11:3200/api/search/tag/dagger.io%2Fcache.outcome/values'
```

Note: Tempo streams multiple JSON documents per response; `gate_a2.py` decodes with `raw_decode` in a loop because `json.loads` on the whole body fails with "Extra data".
