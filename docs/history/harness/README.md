> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Gate A harness — beta.25 engine with local telemetry capture

Purpose: answer the two questions the live cluster cannot (`gate_a.md`):
1. does `v1.0.0-beta.25` actually emit the `dagger.io/cache.*` contract?
2. what is the cross-session repeat rate of `cache.pairing_digest` on a real pipeline?

Runs entirely on **development-host** (docker context `development-host`, 124 GB free), modifies nothing in the cluster.

## Pieces

| File | Role |
|---|---|
| `otel-collector.yaml` | collector config: OTLP http/grpc receivers → `file` exporter at `/out/spans.json` |
| `analyze.py` | parses that file and answers the three questions (contract presence, part identity, repeat rate) |
| `../gate_a.md` | findings from the live cluster (v0.21.9) |
| `../gate_a2.py` | live-Tempo trace-name/attribute probe |

## Run

```bash
# 0. images (already pulling)
ssh development-host 'docker pull otel/opentelemetry-collector-contrib:0.165.0; \
            docker pull registry.dagger.io/engine:v1.0.0-beta.25'

# 1. collector
ssh development-host 'mkdir -p /data/gate-a/out && \
  docker rm -f otelcol >/dev/null 2>&1; \
  docker run -d --name otelcol --network host \
    -v /data/gate-a:/out \
    -v /data/gate-a/otel-collector.yaml:/etc/otelcol/config.yaml:ro \
    otel/opentelemetry-collector-contrib:0.165.0 \
    --config=/etc/otelcol/config.yaml'

# 2. engine, exporting to the collector on the host
ssh development-host 'docker rm -f dagger-a >/dev/null 2>&1; \
  docker run -d --name dagger-a --privileged --network host \
    -v /var/lib/dagger \
    -e OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 \
    -e OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf \
    -e OTEL_RESOURCE_ATTRIBUTES=service.name=dagger-engine,service.instance.id=gate-a \
    registry.dagger.io/engine:v1.0.0-beta.25'

# 3. drive a real pipeline N times (see "Driver" below), then
python3 analyze.py <spans.json>
```

## Driver — the one open wiring question

The engine must be the *instrumented* container above, not one the CLI starts implicitly. Two candidate mechanisms, to be tested in this order:

1. `_EXPERIMENTAL_DAGGER_RUNNER_HOST=docker-container://dagger-a` with a pinned CLI
   (`go install dagger.io/dagger/cmd/dagger@v1.0.0-beta.25` on development-host, `go` is present).
2. If that env is gone in beta.25: read `dagger --help` / `dagger version` on the pinned CLI and use the documented engine-address mechanism; fall back to running the engine **inside** the CLI's expected path and injecting the OTEL env via a wrapper image.

Do not guess: the first step of the harness is to print the pinned CLI's help and record what it accepts. That result goes into this file.

## What the run must produce

- A `spans.json` with at least one `executed` and one lazy span carrying the cache contract.
- The attribute inventory from `analyze.py` §1 — that is the empirical answer to review findings F11/F12 (completeness attribute name; whether `cache.offer.part` carries a `PartAddress` or a content identity).
- §3's cross-session repeat share, labelled an **upper bound** (repeats may already be local-cache hits).
- Repeat the pipeline cold and warm, and record wall time both ways: the pair is Gate B's baseline input.

## Cost

~1.1 GB of image pull, a few minutes of CPU, no cluster change, no production impact.
