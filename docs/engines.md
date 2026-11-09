# Connecting independent Dagger engines

This service is an **external network cache coordinator**, not a plugin or patched engine. Each engine keeps a separate local state directory and connects to the same coordinator. See [S3 setup](s3.md), [architecture](design.md), and [testing](testing.md).

## Compatibility is deliberately narrow

Use the unmodified image `registry.dagger.io/engine:v1.0.0-beta.25` and its matching CLI. The service imports Dagger commit `a45241b20a5e07fbfc5954479ef0b61da843499c`; it is not a generic BuildKit/REAPI cache server. The handshake accepts the configured version or that version with a `+...` build suffix. Changing `DCACHE_ENGINE_VERSION` alone does not update the imported protocol libraries.

**Any number of independently provisioned compatible engines can be configured; this is not a capacity guarantee.** Four overlapping query clients were verified. The 96-client run was blocked by the fixture memory guard. Other releases, including existing 0.21.x engines, are not supported by this tested implementation.

## Three distinct configuration scopes

| Process | Configuration |
|---|---|
| Coordinator | S3 endpoint/credentials, persistent database, pool name/token and private HMAC secret |
| Each engine | Remote-cache service base URL and matching pool token; Cloud URL/telemetry settings |
| Each CLI/runner session | Matching `DAGGER_CLOUD_URL` and `DAGGER_CLOUD_TOKEN`, plus the address of its intended engine |

Do not give engines or runners the coordinator's S3 keys/HMAC secret. Do not share one engine data volume between engines. No Dagger Cloud account or `dagger login` is needed for this self-hosted path; the `DAGGER_CLOUD_*` names are also used by the pinned client's telemetry/session machinery.

## Engine environment: use the base URL

Start from [engine.env.example](../examples/engine.env.example). Fill its empty token with the exact value of coordinator `DCACHE_TOKEN`:

```sh
export _EXPERIMENTAL_DAGGER_REMOTE_CACHE_URL=http://dcache:8080
export DAGGER_CLOUD_URL=http://dcache:8080
export DAGGER_CLOUD_TOKEN="${DCACHE_TOKEN}"
export OTEL_EXPORTER_OTLP_TRACES_LIVE=1
```

**Do not append `/v1/engine` to `_EXPERIMENTAL_DAGGER_REMOTE_CACHE_URL`.** The pinned engine appends that route itself; supplying it twice dials the wrong path. Use the service HTTP(S) base URL. The engine establishes its WebSocket internally.

`DAGGER_CLOUD_TOKEN` must be nonempty and must not be the literal `oidc`; OIDC mode does not enable this engine-token connection. Using a generated `dck-...` token matches the exercised fixtures. A remote-cache URL without a token leaves the integration disabled. Engine environment/config changes require your normal engine recreation/restart procedure; this guide does not modify existing deployments.

Apply these settings to the **engine process**, not only the runner. For session call identity to be discovered, apply Cloud URL/token settings to the CLI sessions as well. The real functional harness exercises exactly this combination without a separate collector: engine events and session traces reach the coordinator and produce exports.

### Explicit OTLP exporters, if your deployment needs them

The service also exposes the normal OTLP HTTP routes. When configuring an exporter directly, quote the header containing a space:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://dcache:8080
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_EXPORTER_OTLP_HEADERS="Authorization=Bearer ${DAGGER_CLOUD_TOKEN}"
export OTEL_EXPORTER_OTLP_TRACES_LIVE=1
```

Configure the actual process that emits the relevant telemetry. A connected WebSocket alone does not create discovery holdings. This explicit-exporter variant is configuration guidance; the end-to-end fixture uses the tested Cloud URL/token path. Avoid sending a second duplicate stream unintentionally.

## Minimal Linux Docker wiring

This example wires an existing isolated S3-compatible server into a dedicated Docker network; its `objectstore` alias must be reachable on the network created below. It does **not** provision a production object store or install a deployment. Use a new isolated namespace and never reuse production container/volume names.

1. Build on Linux with `make build`. Create the dedicated network, then attach your separately provisioned disposable S3 container (replace the placeholder name):

   ```sh
   docker network create dcache-demo
   docker network connect --alias objectstore dcache-demo YOUR_DISPOSABLE_S3_CONTAINER
   ```

2. Copy the coordinator example to ignored `.env`, fill all blanks, and retain its secrets securely. Its endpoints are `http://objectstore:9000`, database `/state/cache.db`. Start the service using the same Ubuntu runtime shape exercised by the harness:

   ```sh
   cp examples/coordinator.env.example .env
   # Fill .env from your secret manager before running the next command.
   docker run -d --name dcache --network dcache-demo \
     --env-file .env \
     -p 127.0.0.1:8080:8080 \
     -v "$(pwd)/bin/dcache:/dcache:ro" \
     -v dcache-demo-coordinator:/state \
     ubuntu:24.04 /dcache
   ```

   This binds the diagnostic HTTP port only to host loopback. Engines on the bridge use `http://dcache:8080`. The container needs access to the S3 origin and appropriate certificate trust if you change endpoints to HTTPS.

3. Copy the engine example into an ignored file such as `examples/.env`, fill only `DAGGER_CLOUD_TOKEN` with the matching pool token, and launch separate engines:

   ```sh
   cp examples/engine.env.example examples/.env
   # Fill examples/.env with the matching pool token; never copy S3 keys here.
   for engine in a b; do
     docker run -d --name "dcache-demo-engine-${engine}" \
       --network dcache-demo --privileged \
       --env-file examples/.env \
       -v "dcache-demo-engine-${engine}:/var/lib/dagger" \
       registry.dagger.io/engine:v1.0.0-beta.25
   done
   ```

   Separate named volumes are intentional. No engine control port is published. Privileged engines are powerful infrastructure: use only isolated trusted hosts. The example leaves the engine's default GC policy unchanged; the automated fixtures use separately documented bounded GC settings.

4. Give the CLI session Cloud URL/token too. The following assumes the shell already has `DAGGER_CLOUD_URL=http://dcache:8080` and the saved `DAGGER_CLOUD_TOKEN` exported. GraphQL is passed on **stdin**, not as a positional argument:

   ```sh
   docker exec -i \
     -e DAGGER_CLOUD_URL -e DAGGER_CLOUD_TOKEN \
     dcache-demo-engine-a \
     dagger api query --no-load-module \
       --engine unix:///run/dagger/engine.sock --progress plain \
       < tests/functional/discovery.graphql
   ```

   Repeat against engine B by changing only the container name. This discovery query is a connectivity/telemetry probe, **not proof of cross-engine reuse**. For the producer-removal, byte-demand and execution-counter proof, run [the automated acceptance harness](testing.md).

Manage these manually created demo containers/volumes explicitly when finished. There is no global Docker-prune instruction here. The automated harness creates unique fixture names, performs scoped cleanup and verifies absence.

## Existing Kubernetes/ARC engines

Apply the equivalent environment to the existing **engine container**. For example, a fragment in an engine pod specification could use a dedicated Kubernetes Secret:

```yaml
env:
  - name: _EXPERIMENTAL_DAGGER_REMOTE_CACHE_URL
    value: http://dcache.cache.svc.cluster.local:8080
  - name: DAGGER_CLOUD_URL
    value: http://dcache.cache.svc.cluster.local:8080
  - name: DAGGER_CLOUD_TOKEN
    valueFrom:
      secretKeyRef:
        name: dcache-pool-auth
        key: token
  - name: OTEL_EXPORTER_OTLP_TRACES_LIVE
    value: "1"
```

Use the same token and Cloud base URL in runner/CLI session environments. The Secret must exist in each consuming namespace; namespace-local names are not cross-namespace references. Separately configure one coordinator replica with persistent writable storage, server-only credentials, and a Service. Every engine must also reach the origin in `S3_PUBLIC_ENDPOINT` directly.

This is an illustrative integration fragment, not an exercised Helm chart, GitOps rollout, HA deployment, or permission to restart current engines. Do not route a compatible runner to an older engine behind a mixed-version load balancer. Engine-local mounted cache volumes remain engine-local; this service shares completed exported results, not arbitrary mutable cache directories.

## Diagnose in order

1. Coordinator starts successfully and `/healthz` returns 200.
2. Its authenticated `/debug/state` shows registered peers. Preserve Basic/Bearer authorization and WebSocket upgrade at any proxy.
3. Session traces and engine events arrive, then publication increases. `/v1/metrics` is OTLP ingestion whose payload is dropped; scrape `/metrics` for coordinator metrics instead.
4. Engines can PUT/GET/Range-GET the signed S3 URLs without host/path rewrites.
5. Compare identical immutable workload identities, and allow export/verification time before the consumer. Mutable image tags are session-scoped in this pin and can prevent reuse.
6. Inspect local GC/prune messages, expiry, missing/corrupt objects and bounded export pressure before assuming a protocol failure.

The passing fixture checks correct bytes and external execution counters. A healthy socket, nonzero merge count or a fast repeated scalar query alone is not cache-hit proof. See [verification](../VERIFICATION.md) for exactly what was and was not run.

Implementation references: [engine fixture setup](../tests/functional/run.py), [HTTP routes/authentication](../internal/server/server.go), [WebSocket peer](../internal/peer/peer.go). The bootstrap pin's upstream `cmd/engine/remotecache.go` and `engine/remotecache/remotecache.go` define base-URL handling and token enablement.
