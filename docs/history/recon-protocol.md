> **Historical design/review record, not current implementation status.** See [current design](../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Dagger 1.0-beta remote-cache protocol: implementer's specification

## 0. Scope, evidence, and an important limitation

**This is a source-derived specification of the public engine transport and public service-side cache primitives, not a recovered implementation of Dagger Cloud's private cache server.** In particular, `engine/server/remote_cache*.go` is the **engine's control adapter**, not an HTTP cache-service listener. Its own integration documentation explicitly says it is not a listener or service. The separate Cloud service is referenced by the dogfood PR as a stack in `dagger/dagger.io`; that repository returned authenticated HTTP 404 in this investigation. Its HTTP handler implementation, token database, deployment paths, physical blob-store layout, and choice of object-store backend cannot be established from these sources. Do not turn the test-fixture directory layout into a claim about the production service. [engine/server/remote_cache.go:14-23; ../dagger-evidence/14508.md:7-11; ../dagger-evidence/scope.md:1-8]

### Revision convention

- All repository `file:line` citations below refer to the shallow checkout `<HISTORICAL_SCRATCH>/dagger-src` at **`7e9eaacc376e5c3bed3efdca9e1ba750bc8d9233`**. A durable source link can be constructed from the repository, this commit, file path and line range. All `../dagger-evidence/*.md:line` citations are saved GitHub issue/PR bodies in the sibling evidence directory; they describe author-reported behavior, not fresh test execution. [../dagger-evidence/scope.md:1-8]
- The inspected release tag **`v1.0.0-beta.25`** resolves to **`a45241b20a5e07fbfc5954479ef0b61da843499c`**. A targeted `git diff` against the checkout showed no changes to `engine/remotecache`, `engine/server/remote_cache*`, `dagql/cache_transfer*` or `dagql/cache_part*`; the specification is pinned to the inspected commit, not a promise that all 1.0 betas use identical formats. [../dagger-evidence/scope.md:1-6]
- Retrieval completed on **2026-11-06 UTC** (2026-11-05 evening EST). Current PR status was fetched using authenticated `gh api`, not inferred from titles. Supporting raw JSON is in `<HISTORICAL_SCRATCH>/dagger-{issues,pulls,comments}-<number>.json`.
- **Observed** below means directly established by the cited implementation. **Recommendation** means a design for a self-hosted service inferred from those constraints, not a feature observed in Dagger Cloud.

## 1. Architecture: three channels, not an S3 cache importer

1. **Control:** the engine initiates one persistent WebSocket connection to the cache service and handles service-initiated export, merge, offer and upload commands. The engine sends hello and address-renewal requests. Many requests may be in flight in both directions. [engine/remotecache/remotecache.go:1-5,123-181; engine/remotecache/protocol/protocol.go:1-8,49-82]
2. **Blob data:** independently authorized HTTP PUT and GET URLs carry immutable snapshot **layer blobs**. Control messages carry descriptors, graph metadata and signed URLs, not blob bytes. The service chooses the URLs and can point directly at an object store; there is no prescribed `/blobs/...` HTTP route in this control protocol. [engine/remotecache/protocol/protocol.go:175-180,194-198,311-324; dagql/cache_transfer_types.go:40-56; engine/remotecache/remotecache.go:242-279; dagql/cache_part_content.go:510-535]
3. **Discovery/lifecycle:** the service learns engine entries through exported telemetry and engine cache events, not a WebSocket inventory message. Enabling remote cache also enables engine events; ordinary telemetry supplies recipe, entry, dependency and completed-part facts. A self-hosted service with only `/v1/engine` can execute explicitly chosen commands but does not automatically learn which roots/parts to request. [cmd/engine/remotecache.go:49-62; engine/telemetryattrs/attrs.go:601-621,691-770,858-942; dagql/cache_remote_entries.go:17-30,68-112]

The new integration is not the older Helm `magicache`/`_EXPERIMENTAL_DAGGER_CACHESERVICE_*` configuration. See section 11. [helm/dagger/templates/engine-daemonset.yaml:82-100; cmd/engine/remotecache.go:14-46]

## 2. HTTP endpoint/verb inventory

The following is the complete **publicly observable endpoint surface for this integration**, including its telemetry dependency. Private server administrative routes, provisioning/token-creation routes, health routes and bucket key conventions are **unknown**, not invented here.

| Endpoint/address | Verb and request | Response/semantics | Evidence |
|---|---|---|---|
| `<REMOTE_CACHE_BASE>/v1/engine` | WebSocket opening handshake (HTTP GET Upgrade); `Authorization: Basic base64(token + ":")`; no JSON HTTP body | Standard WebSocket upgrade; following messages are JSON text frames. Engine requests permessage-deflate with no context takeover. HTTP refusal status/body is not specified by the engine protocol. | engine/remotecache/protocol/protocol.go:22-39; engine/remotecache/remotecache.go:167-181,193-209 |
| Each value of `upload.body.urls[digest]` | HTTP **PUT**, raw bytes of exactly the exported descriptor blob, `Content-Length` from local descriptor size; the upload code adds no Basic/Bearer auth, content-type, checksum header or extra provider-specific signed headers | Any 2xx is success; non-2xx records a failed blob. No multipart/chunk/retry protocol is implemented in this client. URLs are capabilities. | engine/remotecache/session.go:284-330; engine/remotecache/remotecache.go:242-279 |
| Each `BlobAddress.url` in offered/merged/renewed chain | HTTP **GET**, no body; always `Range: bytes=<offset>-<descriptor.size-1>`; no engine auth header | 206 must have exact `Content-Range: bytes <offset>-<size-1>/<size>`; 200 is accepted as whole object, validates known Content-Length against full size, and streams/discards prefix for nonzero offset. Other statuses fail, with limited renewal rules below. | dagql/cache_part_content.go:432-565 |
| `<CLOUD_BASE>/v1/traces` | OTLP/HTTP trace export POST, standard OTLP ExportTraceServiceRequest, built by official OTel exporter; auth configured separately from remote-cache URL | Standard OTLP response, not a cache-control Envelope; supplies session call/evaluation facts. | engine/telemetry/cloud_exporters.go:23-35,44-83; engine/telemetryattrs/attrs.go:601-793 |
| `<CLOUD_BASE>/v1/logs` | OTLP/HTTP log export POST, standard OTLP ExportLogsServiceRequest; engine-event body is a **JSON string in the log record**, not a top-level JSON event POST | Lifecycle/prune/share events and session logs; decode the OTLP envelope first. | engine/telemetry/cloud_exporters.go:84-90; cmd/engine/telemetry.go:149-183; engine/server/engine_events.go:104-124 |
| `<CLOUD_BASE>/v1/metrics` | OTLP/HTTP metric export POST, standard OTLP ExportMetricsServiceRequest | General Cloud telemetry, not an inventory/control API. The engine-event setup explicitly shuts unused span and metric exporters down. | engine/telemetry/cloud_exporters.go:91-102; cmd/engine/telemetry.go:171-178 |
| `<CLOUD_BASE>/v1/traces` connectivity probe | Unauthenticated **HEAD**, no body | Any HTTP response is considered connectivity success, including redirects; this is not token validation or cache lookup. | engine/telemetry/cloud_exporters.go:117-144 |

**No REST `POST /export`, `/merge`, `/offer`, `/renew`, `/lease`, `/cache/<key>`, S3 ListObjects requirement, or registry `/v2` requirement appears in the new wire protocol.** Export/merge/offer/upload/renew are WebSocket message types; blob GET/PUT addresses are opaque. [engine/remotecache/protocol/protocol.go:22-24,49-82,175-180,311-324]

### 2.1 Telemetry facts a proactive service needs (public producer contract)

This table maps producer facts to public graph primitives; it is not a claim to reproduce the private service's complete ingest pipeline. Read cache facts only when `dagger.io/cache.contract` is the supported string `"1"`; unknown contracts are ineligible. Profile-skipped/suppressed calls may not emit facts. [engine/telemetryattrs/attrs.go:595-625]

| Attribute/location | Representation and meaning |
|---|---|
| Resource `dagger.io/engine.cache` | String `<cacheID>/<generation>`; split off generation and associate entry numbers with cacheID. |
| Resource `dagger.io/engine.session` | String session identity; do not conflate multiple sessions sharing a trace. |
| Span `dagger.io/cache.result.id` | Decimal-string engine-local uint64 result number. |
| `dagger.io/cache.outcome` | `hit`, `executed`, `joined`, or `uncached`; a hit on a lazy shell is not evidence of byte completion. |
| `dagger.io/cache.deps` | Native OTLP string array of decimal result numbers; empty array means no dependencies. |
| `dagger.io/cache.parts` | Native OTLP string array of part-address strings; includes metadata and absent parts, which have no bytes. Omitted if none. |
| `dagger.io/cache.replacements` | Decimal-string replacement count, omitted at zero; orders the co-observed value state. |
| `dagger.io/cache.retained` | String `"true"` when retained; retention expiry is separate `dagger.io/cache.retention.expires` in decimal Unix seconds. |
| `dagger.io/cache.expires` | Own value expiry as decimal Unix seconds when present. |
| `dagger.io/cache.type` | Result's GraphQL type name. |
| `dagger.io/cache.output.content_digest` | Recorded content-equivalence digest when known; not an object-store blob address. |
| `dagger.io/cache.self_digest`, `.structural_inputs` | Structural self digest and exact ordered native string-array inputs; order is semantic and differs from deduplicated DAG inputs. |
| `dagger.io/cache.hit.remote`, `.hit.merged_at_unix_ns` | String `"true"` and decimal nanoseconds identifying a remote-origin hit; origin survives restart, merge timestamp does not. |
| `dagger.io/cache.offer.part`, `.offer.bytes`, `.offer.duration_ns` | Actual offer installation's address, downloaded bytes and duration nanoseconds on the installing lazy-evaluation span; bytes/duration are decimal strings. |

Evidence for table: [engine/telemetryattrs/attrs.go:52-64,612-654,691-793]. Lazy evaluation spans carry result/deps/parts/content facts but no lookup outcome; use `UpdateRemoteHolding` only after a holding exists, since lazy/share updates intentionally never create one. Use ordinary call facts to attach holding identity, session add/release to maintain session ownership, engine prune/retention observations for retained ownership, and merged replies for imported entries with no ordinary call span. [engine/telemetryattrs/attrs.go:738-742; dagql/cache_remote_entries.go:382-440,488-607,726-805]

Call-frame telemetry is **not the transfer `ResultCall` JSON object**: `dagger.io/dag.call` is an encoded call protobuf string; call payloads exceeding 1MiB on a span use a protobuf-byte log lane. A consumer implementing call identity reconstruction must support the pinned call protobuf definitions/decoder and not JSON-decode that attribute as ValueBundle. The log records are identified by content type `application/vnd.dagger.call+proto` and `dagger.io/dag.call.digest`. Exact decoder/codecs, not arbitrary JSON parsing, are needed for recipe identity. [core/telemetry.go:235-261; engine/telemetryattrs/attrs.go:289-313; engine/clientdb/call_index.go:181-216]

Engine-event log metadata: instrumentation scope `dagger.io/engine.cache`, record attributes `dagger.io/engine.event` and `dagger.io/engine.cache`, process resource `service.instance.id`, engine timestamp. Body is an OTLP **string containing JSON** with these exact shapes:

```text
engine.start {engineVersion:string, engineName:string, restored:bool, restoredEntries:int, wipedCache?:string}
engine.prune {drops:[{resultId:uint64, droppedAtUnixNano:int64}]}
engine.share {parts:[{resultId:uint64, part:string, deps:uint64[], replacements?:uint64}]}
engine.stop  {clean:bool, savedEntries:int}
```

Note lowercase `resultId` in event bodies versus `resultID` in persisted records. Empty prune/share passes emit no event. A clean stop is not a promise all event uploads reached the service; event shutdown has its own bounded drain/export. [engine/telemetryattrs/attrs.go:858-942; engine/server/engine_events.go:141-188]

## 3. Authentication and tenancy

### 3.1 What the engine actually implements

- Remote cache starts only when a URL is configured **and** `DAGGER_CLOUD_TOKEN` is nonempty and not the literal string `oidc`. URL alone warns and leaves it disabled; token alone does nothing. The client does not validate token format, mint a token, exchange it, or enforce a `dag_` prefix. [cmd/engine/remotecache.go:18-46]
- The entire token is the Basic **username**, with an empty password. It is not sent as a Bearer token on the WebSocket handshake. [engine/remotecache/remotecache.go:167-173]
- `hello.cacheID`/generation/name/version describe a cache, not an authenticated tenant; hello contains no org identifier. The protocol defines WebSocket close **4001** for revoked credentials. The engine reconnect loop has no special permanent-disable handling for that code: connections continue through the generic reconnect path until its context ends. [engine/remotecache/protocol/protocol.go:36-38,117-127; engine/remotecache/remotecache.go:130-152]
- Blob access uses only the supplied URL. The engine does not forward its engine token to the object-store GET/PUT code. Upload errors deliberately avoid printing signed URLs. [engine/remotecache/remotecache.go:242-279; dagql/cache_part_content.go:510-535]
- Telemetry credentials are related but not identical logic: engine Basic credentials become the same Basic username/empty-password header; OIDC becomes Bearer; OAuth may refresh. Telemetry can add `X-Dagger-Org` from auth metadata. `DAGGER_CLOUD_URL` controls its destination separately. [engine/telemetry/cloud_exporters.go:23-35,54-63,105-115,147-166]

### 3.2 Consequences for a self-hosted server (recommendations)

Authenticate before accepting the socket; map the opaque token to a trust domain/pool server-side. Use TLS and tenant-scoped blob capabilities. Do not trust an engine-supplied cache ID to select an organization. Re-check revocation for long-lived sessions if desired, close with 4001, and refuse future handshakes. Protect renewal keys against cross-tenant use and arbitrary digest signing. These are service security requirements inferred from the Basic handshake, missing tenant field and URL-capability data plane, not a recovered Cloud token model. [engine/remotecache/remotecache.go:167-181; engine/remotecache/protocol/protocol.go:117-124,311-324]

Treat metadata/control writers in a shared pool as mutually trusted: transferred values and extra digests make equivalence claims, and descriptions are not an untrusted-sandbox interchange format. Source comments explicitly assume equivalence means the same across engines of an organization. Do not expose one shared cache namespace to mutually hostile users merely because its blobs are hash-addressed. [dagql/cache_value_codec.go:12-23; hack/designs/remote-cache/remote-cache-data-flows.md:25-27]

## 4. WebSocket framing, ordering, errors and liveness

### 4.1 Common envelope

Every message is one JSON object in one WebSocket **text** frame:

```json
{"id":1,"type":"hello","body":{"cacheID":"cache-uuid","generation":1,"engineVersion":"...","engineName":"..."}}
```

Replies have their own sender-local `id` and `re` equal to the original request's ID:

```json
{"id":1,"re":1,"type":"welcome","body":{}}
```

Exact outer shape: `id:uint64`, optional `re:uint64` (zero means request), `type:string`, `body:raw JSON`. IDs number **all messages per sender from 1**, not globally across both peers; use separate peer namespaces. Reply pairing: hello→welcome, export→exported, upload→uploaded, merge→merged, offer→offered, renew→renewed. Request failure is `type:"error", body:{"message":string}` correlated by `re`. It ends one request, not the connection. [engine/remotecache/protocol/protocol.go:6-8,49-115]

**Precision warning:** uint64 entry numbers/IDs and int64 Unix nanoseconds are JSON numbers. Implement lossless integer decoding; do not silently round through JavaScript Number. The source supplies a lossless JSON decoder for persisted data. [engine/remotecache/protocol/protocol.go:88-92,202-221; dagql/cache_persistence_codec.go:321-338]

### 4.2 Session rules and limits

- On connection the engine sends hello and waits at most **30 seconds** for welcome. It starts read handling before hello completes; it starts ping and renewal pumps after welcome. [engine/remotecache/remotecache.go:43-44; engine/remotecache/session.go:67-113]
- Maximum accepted message is **`64 << 20` bytes**. A compliant sender substitutes correlated `error {message:"too large"}` for an oversized reply; an oversized received frame closes the socket. This is a message limit, not a blob-size/chunk-size limit. [engine/remotecache/protocol/protocol.go:26-29,41-44; engine/remotecache/session.go:394-419]
- Engine pings every **15s** with **15s** pong timeout. Reconnect backoff starts at **1s**, doubles to **30s**, ±20% jitter, resets after a successfully welcomed session. Engine operation does not wait for the service to be available. [engine/remotecache/protocol/protocol.go:31-34; engine/remotecache/remotecache.go:1-5,37-44,130-152,214-228; engine/remotecache/session.go:159-178]
- Reply dispatch uses `re` to remove one pending waiter; unmatched replies are ignored. Malformed JSON is warned about and skipped; unknown request types receive an error. Incoming merge/offer/export handlers run independently; the socket must not assume FIFO completion. Binary WebSocket messages are ignored by this engine wrapper. [engine/remotecache/remotecache.go:195-209; engine/remotecache/session.go:116-156]
- Session termination cancels work, fails in-flight engine-side requests and releases exported chains; the next connection starts over with hello. There is no replay/resumption/ack-log field in the Envelope. An application must reconcile ambiguous commits rather than assume a lost reply means no mutation. [engine/remotecache/session.go:25-27,81-87,232-259; dagql/cache_value_merge.go:199-219]

## 5. Complete control message payload catalog

Notation: `?` marks Go `omitempty` fields, not an additional JSON key. Unmarked fields are emitted even when zero; nil slices may still encode as null unless the implementation initializes them. Exact source declarations are included in the source appendix.

### 5.1 Engine → service: hello; service → engine: welcome

```text
hello   { cacheID:string, generation:uint64, engineVersion:string, engineName:string }
welcome {}
```

Cache ID is random persistent-database identity. Generation starts at 1 and increases each database open; result numbers are advanced beyond all previously allocated numbers, including unsaved ones, so `(cacheID, number)` can identify an entry across restart. Telemetry names the process generation as `<cacheID>/<generation>`. [engine/remotecache/protocol/protocol.go:117-127; dagql/cache.go:678-768]

### 5.2 Service → engine: export; engine → service: exported

```text
export {
  roots: uint64[],
  outputs?: [{ number:uint64, address:PartAddress }]
}
exported {
  exportID?: string,
  bundle?: ValueBundle,
  skipped?: [{ number:uint64, reason:"gone"|"busy"|"refused", message?:string }]
}
```

- Roots are **the target engine's entry numbers**, not service numbers or digests. Each requested root gets a temporary cache hold; missing roots are `gone`. Selected outputs are captured only within the surviving closure; a selected output whose root is gone or outside that closure is omitted. Roots can be exported as metadata without selecting blob outputs. [engine/server/remote_cache_protocol.go:36-89]
- Capture may not evaluate busy rows. On a capture failure (other than request cancellation/cache close), probe roots separately: busy→`busy`, non-busy refusal→`refused`, other ready roots captured together. Busy/refused messages are truncated to **1024 bytes at a UTF-8 rune boundary**. The final multi-root capture can still fail the whole request on another non-busy error. [engine/server/remote_cache_protocol.go:92-155]
- No surviving root → no bundle/exportID. Otherwise a random **12-byte, hex-encoded** exportID identifies held chain providers on **that connection**. Service must continue with upload, including an empty upload URL map if nothing is needed. Chains remain pinned until upload completes or connection ends. [engine/remotecache/protocol/protocol.go:158-180; engine/remotecache/session.go:219-282,445-451]
- Export roots/selected entries are released after the adapter consume callback; the callback waits through upload, so the hold is intentional for actual transfer, not a durable availability lease. At most **4 exports** can occupy slots, and each slot stays occupied while waiting for upload. A service that never sends upload can exhaust those slots indefinitely while the socket remains open. [engine/server/remote_cache_protocol.go:46-53,163-188; engine/remotecache/session.go:225-259; engine/remotecache/remotecache.go:29-35]

### 5.3 Service → engine: upload; engine → service: uploaded

```text
upload   { exportID:string, urls:{ "algorithm:hex-digest": "presigned PUT URL", ... } }
uploaded { done:Digest[], failed?:[{ digest:Digest, message:string }] }
```

This is a **new request**, not an `exported` reply: use a fresh service message ID and `re=0`. The engine looks up and deletes exportID at first upload dispatch. Unknown or already-consumed ID errors. One upload request consumes the export; it may name only a subset of its blobs. A second upload cannot retry failed blobs from the same exportID; re-export to obtain new providers. `urls:{}` releases chains and returns `done:[]`. Requested unknown digest fails locally because it is outside the captured chains. [engine/remotecache/protocol/protocol.go:175-192; engine/remotecache/session.go:265-330; engine/server/remote_cache_protocol.go:245-267]

Uploads run with **8 concurrent PUTs across all exports of the session**. Results can be unordered. Each PUT has **1 minute body-read idle** and **1 minute response-header wait after request write** bounds. It uses one raw PUT attempt; no resumable multipart handshake or object-store credentials are added by the engine. A 2xx indicates the upload target accepted the HTTP operation, **not** that the service has committed the graph or independently verified durable bytes. [engine/remotecache/remotecache.go:29-51,230-279; engine/remotecache/session.go:284-321]

### 5.4 Service → engine: merge; engine → service: merged

```text
merge { bundle:ValueBundle }
merged {
  generation:uint64,
  engineTime:int64,                       // Unix nanoseconds at commit
  values:[{
    ordinal:uint64, number:uint64, replacements:uint64, expiresAtUnix:int64,
    deps:uint64[], complete:PartAddress[], offered:PartAddress[]
  }],
  retained:[{ ordinal:uint64, expiresAtUnix:int64 }],
  skipped?:[{ ordinal:uint64, reason:"expired" }]
}
```

The bundle's sender numbers are **service/Cloud numbers**. The engine merges from the reserved cache identity `cloud`; reply `number` and `deps` are **engine-local**. `ordinal` maps each reply back to the request bundle, and target state is read under the committing graph lock. `complete` and `offered` mean local completion versus attached download descriptions, not interchangeable states. The adapter makes `values`, `retained`, and each value's deps/complete/offered empty arrays rather than null. [engine/remotecache/protocol/protocol.go:194-235; engine/server/remote_cache_protocol.go:275-344; dagql/cache_remote_entries.go:32-49]

An engine can commit successfully and then encounter release/cleanup error. The adapter still sends `merged` for such a committed operation, logging cleanup error, so the service learns the changed state. Do not discard state merely because a direct `MergeValues` API also returned an error: check `MergeReply.Committed`. [engine/server/remote_cache_protocol.go:275-299; dagql/cache_value_merge.go:199-219]

Core merge rules, by recipe's existing entry: none→new; holding-only entry→store record; unexpired value→keep local value; expired existing + unexpired incoming→replace in place on Cloud, or on engine retire/create new if existing is in use; both expired→keep and add only missing information. Retain live roots, skip expired roots, merge only live roots' required closure. Engine admits incoming blob descriptions as offers; Cloud does not call them stored until bytes are present and `SetStoredPart` is applied. [dagql/cache_value_merge.go:14-50]

### 5.5 Service → engine: offer; engine → service: offered

```text
offer { items:[{
  number:uint64,
  offers:[{
    offer:PartOffer,
    cloudNumber:uint64, cloudExpiresAtUnix:int64, cloudStored:bool
  }]
}] }
offered { items:[{
  number:uint64,
  gone?:bool,
  replacements?:uint64,
  deps?:uint64[],
  parts?:[{ address:PartAddress, outcome:OfferOutcome, message?:string }]
}] }
OfferOutcome = "accepted" | "alreadyComplete" | "executionStarted" | "unavailable" | "invalid"
```

One reply item per input item in order. `number` selects the receiving engine root. Offer address may resolve to that root or a descendant/result-ref. `cloudNumber` names the **Cloud counterpart of the resolved receiving entry**, not necessarily the entry donating the blob. `cloudStored` says whether that counterpart has a stored value record; an equivalent class donor can supply bytes even when it does not. Ignore cloud expiry when cloudStored is false. [engine/remotecache/protocol/protocol.go:237-292; dagql/cache_offer.go:44-59; engine/server/remote_cache_protocol.go:346-401]

Outcome meanings:

| Outcome | Meaning / service action |
|---|---|
| accepted | Description attached (or same existing description retained). **No proof of a download/completion**. Record offered state, wait for actual completion facts. |
| alreadyComplete | Gate settled or record probe reports local complete; can advance held part state to complete at returned replacement count. A record's absent/no-byte part can be complete. |
| executionStarted | Competing lazy group is running/evaluated, or output is installed **but not settled**; description is not admitted. Do not mark complete. Retry/reconcile later. |
| unavailable | Receiver/capture/revision/state cannot accept now; retry under bounded scheduling policy. |
| invalid | Undeclared/wrong kind/malformed reference/no Cloud number/ownership issue or invalid chain; fix the description, not a blind hot-loop. |

The implementation's exact admission checks and gate decisions are in `cache_offer.go`; the adapter reports a missing root as `gone:true`. Per-part error message is returned in the outcome; request cancellation ends the request. Replacement count and dependencies are read where outcomes were decided; item state uses the last disposition's latest state. [dagql/cache_offer.go:69-129,140-351; engine/server/remote_cache_protocol.go:365-401]

### 5.6 Engine → service: renew; service → engine: renewed

```text
renew {
  renewalKey:string,
  layers:ExportLayer[],
  neededBlob:Digest
}
renewed {
  addresses?:{ Digest: {url:string, expiresAtUnix?:int64} },
  unavailable?:bool
}
```

Key format is **opaque/application-defined**, not a protocol JWT or a documented database key. `StoredBundle` exposes a donor number for each output specifically so a service can encode the storing entry into renewalKey; no canonical encoding or signing algorithm is supplied. Return addresses for the requested chain, never arbitrary other blobs; negative replies carry no addresses. Returning all chain addresses is robust, although reply validation permits a subset and later demand fails if its required address is still unusable. `neededBlob` identifies the immediate target. [engine/remotecache/protocol/protocol.go:311-324; dagql/cache_stored_bundle.go:10-19; dagql/cache_part_renewal.go:268-282; dagql/cache_part_content.go:250-277]

Engine-side renewal is best-effort, not a durable RPC queue: **64 outstanding exchanges**, no waiting for capacity, **2s deadline** or earlier caller deadline; a single renewal episode per demand/target/ordered blob content. Replacements of descriptions/annotations cannot mint a fresh retry budget for identical bytes. Failure, disconnect, cancellation, timeout, missing bridge/key, mailbox full or unusable renewed URL makes source unavailable and allows ordinary acquisition fallback. [dagql/cache_part_renewal.go:17-26,97-152,313-375; dagql/cache_part_content.go:280-326; engine/remotecache/session.go:180-217]

## 6. Nested JSON data model (exact shapes)

### 6.1 ValueBundle and its identifiers

```text
ValueBundle {
  version:3,
  roots:[{ ordinal:uint64, expiresAtUnix?:int64 }],
  values:[{
    ordinal:uint64, senderNumber:uint64, senderReplacements?:uint64,
    record:PersistedRecord,
    dependencyIDs?:uint64[], expiresAtUnix?:int64
  }],
  outputs?:[{
    ordinal:uint64, address:PartAddress,
    state:"pending"|"completed"|"absent"|"metadata",
    value?:SnapshotValue, chain?:OfferedChain, owner?:OfferOwner
  }]
}
PersistedRecord {
  resultID:uint64,
  envelope:PersistedResultEnvelope,
  call:ResultCall,                       // required, non-null in a valid transfer
  snapshotLinks?:SnapshotLink[]          // generic persisted type only; transfer requires empty/omitted
}
```

Source: [dagql/cache_transfer_types.go:58-94; dagql/cache_persistence_codec.go:625-632; dagql/cache_value_import.go:130-169].

**Separate namespaces:** bundle ordinal is contiguous positive `1..len(values)` and `record.resultID` equals its ordinal; senderNumber is the stable entry number in the sending cache; reply number is the receiver's entry number; CloudNumber identifies a service counterpart; snapshot IDs are local storage identifiers, not blob digests. All embedded declared numeric references, dependency IDs, output service refs and offer-owner refs within a bundle use **ordinals**, and must be relocated when merging. Zero ordinal means optional absence at the type level, but cannot identify an actual row. Do not string-replace integers in arbitrary JSON; use codec-declared references. [dagql/cache_transfer_types.go:11-12,58-69; dagql/cache_value_import.go:25-52,145-148,172-215; dagql/cache_stored_bundle.go:26-32,111-122,168-200; dagql/cache_persistence_codec.go:634-677]

### 6.2 Part identity and offers

```text
PartAddress {
  outputPath?:[{ field?:string, index?:int, isIndex?:bool }],
  part:string
}
SnapshotValue {
  kind:"directory"|"file"|"snapshot",
  path?:string,
  platform?:OCIPlatform,
  services?:[{ serviceResultID:uint64, hostname:string, aliases?:string[] }]
}
OfferOwner { dependencyIDs?:uint64[] }
OfferedChain {
  layers:ExportLayer[],
  addresses?:{ Digest:BlobAddress },
  renewalKey?:string
}
BlobAddress { url:string, expiresAtUnix?:int64 }
PartOffer { address:PartAddress, value:SnapshotValue, chain:OfferedChain, owner:OfferOwner }
```

Source: [dagql/cache_transfer_types.go:25-57; dagql/cache_persistence_codec.go:367-377; dagql/cache_value_codec.go:364-406].

`part` is nonempty and `outputPath` is a declared structural path, not an object-store key or filesystem path. An inline list item path uses field `items` followed by an index; a `result_ref` switches to a separately owned row and resets consumed path. Internal part-key identity is JSON marshaling of the validated address. Concrete Container keys include `metadata`, `fs`, `execMeta`, and named mount parts; File/Directory use `snapshot`. Do not invent a fixed list of parts for every value type: use each codec's declared outputs, and distinguish absent from pending. [dagql/cache_offer_owner.go:110-119; dagql/cache_value_codec.go:409-451; core/container_parts.go:20-35; core/part_open.go:17-19,58-78; core/schema/remote_cache_fixture.go:355-365]

### 6.3 Layer/blob representation — JSON casing matters

```text
ExportLayer {
  "Descriptor": OCIDescriptor,
  "Description": string,
  "CreatedAt": RFC3339Timestamp|null
}
OCIDescriptor (core fields) {
  "mediaType":string,
  "digest":"algorithm:encoded-hash",
  "size":int64,
  "annotations"?:{string:string}, ... other OCI descriptor fields
}
```

**`Descriptor`, `Description`, `CreatedAt` are capitalized** because `snapshots.ExportLayer` has no JSON tags. The nested OCI descriptor uses its own OCI JSON field names. A Go implementation should import the pinned official types instead of hand-writing a reduced struct which loses annotations or optional descriptor fields. [engine/snapshots/remote_type.go:11-15; engine/remotecache/protocol/protocol_test.go:25-43]

There is **no fixed-size chunking layer** here. A part describes an ordered chain of whole immutable snapshot-layer blobs, each addressed by its compressed-byte digest and sized by its OCI descriptor. Different parts/chains can reference the same digest, and upload URLs are keyed by digest, allowing deduplication. A chain is reconstructed in order from parent/root to leaf; empty layers means scratch/no blob. Export walks snapshot parents and may omit a scratch root or a snapshot adding no layer; compression is the engine's export default (not the fixture's forced uncompressed setting). A layer digest is not the same thing as a tree-content equivalence digest. [engine/snapshots/remote.go:32-98; engine/server/remote_cache_protocol.go:25-27,245-267; engine/snapshots/import.go:8-42; hack/designs/remote-cache/remote-cache-data-flows.md:19]

**No protocol-mandated blob maximum, S3 multipart part size, key prefix, bucket, or manifest-object format is defined.** Descriptor size must be nonnegative and mediaType nonempty, digest syntactically valid. Extra address-map entries for digests outside the layers are invalid. Service must preserve annotations/descriptor data required by reconstruction. [dagql/cache_value_codec.go:391-405; engine/snapshots/remote_type.go:11-24]

### 6.4 Persisted envelope/call representation

```text
PersistedResultEnvelope {
  imported?:bool,
  pendingOffers?:PartOffer[],
  persistDeclined?:bool, storedParts?:PartOffer[],
  version:5,
  kind:"null"|"object_self"|"scalar_json"|"list"|"result_ref",
  typeName?:string, objectCodec?:string, resultID?:uint64,
  sessionResourceHandle?:SessionResourceHandle,
  objectJSON?:raw JSON, scalarJSON?:raw JSON,
  items?:PersistedResultEnvelope[]
}
```

`persistDeclined` and `storedParts` are **saved-local-store fields only**, not transfer metadata. `objectJSON` belongs to its registered family codec, not a free-form container the service can blindly rewrite. Call structure includes field/synthetic kind, recursive result type, field/synthetic op, view, nth, effects, extra digests, receiver, module, args, implicit inputs, and profiling flag; references can have resultID and/or nested call. Full declarations are in the source appendix. [dagql/cache_persistence_self.go:13-71; dagql/result_call_frame.go:18-78,107-156,189-222]

For a compatible service in Go, **reuse `dagql` plus core's registered transfer codecs pinned to the engine revision**. They normalize foreign payloads, derive recipes, validate declared references and relocate typed handles. A language-neutral faithful reimplementation additionally requires all participating codec families' schemas and digest/call semantics; storing arbitrary JSON plus hashes does not implement this protocol. [dagql/cache_transfer_types.go:96-112; dagql/cache_persistence_codec.go:567-591,634-677; dagql/cache_value_codec.go:92-210]

### 6.5 Validation invariants a service must preserve

- Version exactly 3; at least one root; unique positive ordinals in the allowed range; nonzero senderNumber; valid record headers and payloads. Every value is reachable through root ownership closure; dependencies and offer-owner refs form no cycles. [dagql/cache_value_import.go:25-52,172-215]
- Record self/envelope IDs must agree, recorded call and its result type are required, Query itself is not transferable, and nonempty local `snapshotLinks` are rejected. Foreign normalization strips local storage links; transfer carries chains instead. [dagql/cache_value_codec.go:149-179,232-247]
- No duplicate output address, duplicate pending-offer slot, dangling output ordinal or service reference. Output state must match declared codec state, except a pending declared output may have a selected completed chain. A completed output requires chain, value and owner; owner without chain is invalid. [dagql/cache_value_import.go:53-170]
- Every declared non-self reference in a record must be a **direct dependency**, not merely a transitive row inside the same bundle. Owner dependency IDs are nonzero and unique, and every service reference must be covered by owner dependencies. [dagql/cache_value_codec.go:182-198; dagql/cache_offer_owner.go:255-273]
- Metadata cannot carry a snapshot chain. Kind must match the declared part. Do not convert an `absent` part into a fabricated zero-byte blob or treat `accepted` as completed. [dagql/cache_value_codec.go:364-406; dagql/cache_stored_queries.go:33-65; dagql/cache_offer.go:107-129]

## 7. Byte-read, renewal and expiry semantics

### 7.1 Four different kinds of freshness/ownership

1. **Value own expiry** (`TransferredValue.expiresAtUnix`, merged value expiry, Cloud record expiry): Unix **seconds**, 0=none; determines whether a value is interchangeable/fresh and whether stored parts can be borrowed for another entry. [dagql/cache_transfer_types.go:58-74; dagql/cache_value_merge.go:30-46; dagql/cache_stored_queries.go:56-68]
2. **Retention edge expiry** (`roots[].expiresAtUnix`, `merged.retained[].expiresAtUnix`): ownership deadline for retaining a root; not a signed URL TTL and not always identical to value own expiry. StoredBundle chooses retained-edge expiry if present, otherwise entry expiry. [engine/remotecache/protocol/protocol.go:224-229; dagql/cache_stored_bundle.go:82-87; dagql/cache_remote_entries.go:168-178]
3. **Download URL expiry** (`BlobAddress.expiresAtUnix`): Unix seconds, 0=not time-limited by this field; valid only when > current engine Unix second. An absolute http(s) URL with a host is required. Missing/expired URLs can renew; non-http(s) addresses are never opened or renewed by that provider path. [dagql/cache_part_content.go:159-189,250-277]
4. **Transfer/snapshot pins and offer owners:** local ownership keeping snapshots, bytes or dependency rows alive during actual work. These are not HTTP availability leases, no wire acquire/extend/release-lease API exists, and renewal changes download addresses rather than extending value freshness. [engine/snapshots/lease.go:16-61,85-98; dagql/cache_offer_owner.go:176-239; engine/remotecache/protocol/protocol.go:311-324]

### 7.2 Read algorithm and failure behavior

Availability is a pure check with no I/O. Empty chain is available; otherwise all blobs need usable URLs, or a renewal key plus attached bridge. Provider copies the layers and address map without making a request; actual network begins at byte-read boundary. Reader validates requested size/media type matches offered descriptor. Default transport disables automatic HTTP decompression to preserve compressed-byte verification. [dagql/cache_part_content.go:102-156,214-247]

The reader reuses a sequential HTTP response for contiguous reads. A noncontiguous offset closes/reopens with Range. A network operation stalled without byte progress times out after **30s** (not a total download timeout; local writer backpressure is not counted). 206 Content-Range is checked exactly; 200 whole object is supported. EOF before expected bytes is failure. Chain import uses the content-copy path with expected descriptor digest and size, so corrupted bytes cannot silently become the promised layer. [dagql/cache_part_content.go:342-407,432-565; engine/snapshots/pull.go:433-473; engine/snapshots/chain_error.go:73-97]

Only initial **401, 403, 404, 410** responses request new addresses. Other status errors, wrong range/length, digest/copy failure or stalls do not cause unbounded URL renewal. A renewed address never renews again in that demand episode; a failed URL must be replaced with a different usable URL. The bridge validates epoch/sequence/content; late, duplicate, old-epoch, wrong-chain or invalid replies are discarded. [dagql/cache_part_content.go:27-39,250-277,474-507; dagql/cache_part_renewal.go:224-282]

On chain-content failure the source is exhausted for the demand; ordinary acquisition can choose another source or run the original lazy computation. Availability failures are not a global cache miss HTTP lookup. Keep metadata and original runnable operations even if some remote bytes are missing. [dagql/cache_part_content.go:574-584,653-684; hack/designs/remote-cache/remote-cache-data-flows.md:7-9,21-27]

## 8. Public service-side reference primitives

The public repository supplies enough graph machinery to build a service, **but not its network scheduler or object-store implementation**. A recommended self-hosted loop is described in section 12. These are the actual reusable contracts:

### 8.1 Cloud graph and observations

`dagql.NewCache(..., WithBlobStore())` makes a cache accept stored snapshot-chain descriptions. **WithBlobStore is a boolean option, not an S3 driver/interface/factory.** It does not create a bucket, read blobs, issue presigned URLs or persist byte content. [dagql/cache_stored_parts.go:17-28,30-45]

Represent each engine's entry as `HolderKey{Cache:cacheID, Number:engineNumber}`. The Cloud graph relates recipes by digest/term equivalence; holdings track sessions, retention, dependencies, parts and replacement counter. Holding-only entries have no value and cannot be served. After a batch of observation changes, call `CollectRemoteHoldings` on all returned candidates; individual updates do not collect on their own. [dagql/cache_remote_entries.go:17-49,114-129,240-278,310-328,382-440,833-875]

Replacement ordering: lower count is stale value state (content equivalence can still be learned); higher count replaces parts/expiry/dependency state; equal count merges part advancement and dependencies, earlier nonzero expiry wins. Parts advance pending→offered→complete. Retention ordering is lexicographic `(generation, engineTimeUnixNano)`; at equal time retained beats dropped and earlier nonzero expiry wins between retained observations. Never order different engines by raw local wall-clock timestamps. [dagql/cache_remote_entries.go:150-231,644-724]

Useful service APIs include `AttachRemoteHolding`, `UpdateRemoteHolding`, `AddRemoteHold`, `ReleaseRemoteSession`, `ObserveRemoteRetention`, `EndRemoteProcess`, `ReleaseRemoteCache`, `ApplyMergedReply`, `ApplyHeldValueState`, `CollectRemoteHoldings`, `EquivalentEntries`, and `RemoteHoldingInfo`. Process end releases the process's session holds but preserves retention/dependency ownership for a cache restart; releasing the entire cache removes its holds and retention. [dagql/cache_remote_entries.go:382-440,488-607,726-875,1062-1101; dagql/cache_stored_queries.go:12-31; ../dagger-evidence/14761.md:20-40]

### 8.2 Storing exported metadata and bytes

Merge engine exports using `MergeValues(ctx, engineCacheID, bundle)` on the blob-backed cache. The graph commit retains live roots and maps ordinals to Cloud entries but **does not admit output chains as stored merely from the export metadata**. Request/verify missing blob uploads, relocate owner/service refs through merge mappings, then call `SetStoredPart(number, offer, copyExpiresAtUnix)`. It admits parts against the current value, rejects metadata parts/no-value rows/missing or cyclic dependencies, and adds part owner/service dependencies to value ownership. An expired exported copy's part attaches only while current value is expired too. [dagql/cache_value_merge.go:14-46,96-104; dagql/cache_stored_parts.go:30-113]

`PrepareValueMerge` can run outside a service-level work lock; `CommitValueMerge` decides against current graph state under egraph lock and finishes/releases after unlock. Exactly one terminal action, commit or `PreparedValueMerge.Release`; a raced second terminal action does not apply twice. Caller must bound outstanding prepared merges and keep input unchanged until terminal action. An open preparation holds a cache operation and delays close. [dagql/cache_value_merge.go:52-197]

`SetStoredPart` is a metadata assertion that bytes are already present; it has no object-store client and does not verify their existence. **Recommendation:** never call it solely because an untrusted engine listed a digest in `uploaded.done`; verify object size and digest/authorized upload association in the storage tier, and only publish completed chains when every required blob is durable. [dagql/cache_stored_parts.go:30-89; engine/remotecache/session.go:307-321]

### 8.3 Serving stored values and live offers

- `StoredBundle(ctx, roots)` captures record closure under one graph read lock, normalizes/relocates it to ordinals, sets Cloud sender numbers/replacements and creates outputs. Its result includes `Gone`, `NoValue`, and per-output `Donors`; output chains intentionally have **no GET addresses or renewal keys**. Add capabilities in the service before sending merge. [dagql/cache_stored_bundle.go:10-40,41-99,309-318]
- Own parts travel with the value, expired or not; missing declared pending parts can be borrowed from a nonexpired equivalent donor (lowest entry number). A borrowed service binding must map into an equivalent row in the closure or it is omitted. [dagql/cache_stored_bundle.go:34-40,264-305,321-364]
- `AvailablePart` returns stored/absent/unavailable and donor. Own expired stored bytes are not considered available for general offering, while structural absence can still be reported from an expired own record. [dagql/cache_stored_queries.go:33-125]
- `PlacePart`/`PlaceOffer` relocate service references into receiving-engine numbers, select unexpired equivalent service holdings, and set the offer owner to exactly those mapped services. If any service cannot be mapped, do not place the offer. Do not forward Cloud-local owner IDs directly into an engine offer. [dagql/cache_part_placement.go:9-82]

### 8.4 Byte loss, pruning, and restore

`DropStoredParts(digest)` removes all descriptions on all entries that mention a missing blob. Values and dependency entries remain. `StoredState` reports retained roots/part addresses after restore or prune. `LiveBlobs()` lists **all stored-part blobs**, pool or not, for storage sweep; this is distinct from pool-only disk usage accounting. BlobCacheUsage reports pool record/entry estimates, distinct pool blob bytes, all entries/terms/class slots/holdings and stored-record bytes. [dagql/cache_stored_parts.go:162-191; dagql/cache_stored_queries.go:128-229]

**Recommendation:** when renewal detects a missing object, invalidate every affected part with DropStoredParts, reconcile dependent offers, and return unavailable rather than handing out a fresh URL for bytes known missing. Serialize GC publication checks with new stored-part commits or use storage generations; an S3 sweep must not delete objects between verifying an upload and publishing its references. These safeguards follow from LiveBlobs and SetStoredPart being separate metadata-only operations; no distributed sweep transaction is supplied. [dagql/cache_stored_parts.go:30-89,162-191; dagql/cache_stored_queries.go:159-175]

## 9. Persistence: what can and cannot be said about layout

### 9.1 Actual reference Cloud service

**Unknown from accessible source:** service root path; per-pool database filename/layout; whether production bytes are on disk/S3/GCS/another backend; directory/object naming convention; service process locks/leader election; upload admission leases; capability-key format. `dagger/dagger.io` was not accessible. The dogfood and fixes PRs explicitly refer to companion service changes there. [../dagger-evidence/scope.md:1-8; ../dagger-evidence/14508.md:7-11; ../dagger-evidence/14761.md:20-40,100-128,142-154]

### 9.2 Public blob-backed graph's disk store

Caller provides a SQLite DB path to NewCache. Blob-backed cache writes `journal_mode=WAL`, `busy_timeout=10000`, `_txlock=immediate`, `synchronous=NORMAL`; ordinary engine cache uses OFF. DB companions are `<path>-wal` and `<path>-shm`. `Checkpoint` saves closure of stored roots with stored parts in a single snapshot/transaction and `wal_checkpoint(TRUNCATE)`; it serializes checkpoints with checkpointMu. A busy truncation leaves successful save standing and logs warning. Restore is from last saved state even after process crash; OS/power loss can lose latest save but should not corrupt DB under the documented NORMAL/WAL contract. No checkpoint frequency is prescribed by this library. [dagql/cache.go:2242-2293,2322-2339; dagql/cache_persistence_worker.go:15-56,77-87]

It is **not** an automatically live durable record-by-record database of every remote holding. Snapshot selection is rooted in retained/stored closure; use `StoredState` to rebuild service maps on restore, and re-ingest ephemeral engine observations. A service requiring durable tenant registry, outstanding transfer state, URL secret keys or per-process session inventory must persist those separately. [dagql/cache_persistence_worker.go:15-23,77-87; dagql/cache_stored_queries.go:128-156; dagql/cache_remote_entries.go:17-30]

### 9.3 Ordinary engine local layout (not server storage)

Under engine state root: `dagql-cache.db`; worker subpaths `worker/snapshots`, `worker/snapshots/metadata.db`, `worker/content`, `worker/containerdmeta.db`, `worker/metadata_v2.db`, `worker/cachemounts`, `worker/executor`. Engine opens a containerd local content store under worker/content. These are **engine local caches**, not remote-service object keys. [engine/server/server.go:610-617,740-810]

### 9.4 Test fixture layout (not a production service)

`_DAGGER_TEST_REMOTE_CACHE_FIXTURE_ROOT` enables a hidden GraphQL fixture only for an existing absolute directory. It writes:

```text
<fixture-root>/bundles/<requested-relative-path>   # ValueBundle JSON
<fixture-root>/blobs/<digest-algorithm>/<encoded-digest>   # raw layer bytes
<fixture-root>/body-entries/<random>.json          # test invocation observation
<fixture-root>/persistence.json                   # test persistence report read by fixture
```

Bundle JSON is written to a temporary file and renamed; blob files use create/truncate/write. The fixture exports **uncompressed** chains and installs an in-process filesystem provider, so its existence does not demonstrate a concurrent HTTP cache server, upload leases, S3 support or production-safe blob publication. [core/remote_cache_fixture.go:1-5; core/schema/remote_cache_fixture.go:57-80,83-149,350-380,433-475; core/schema/remote_cache_fixture_content.go:16-21,37-61,70-107]

## 10. Concurrency, locking, ownership and leases

### 10.1 What is actually bounded

Per engine WebSocket session: 4 in-flight exports, 8 concurrent blob PUTs, 64 MiB message read cap. Renewal mailbox: 64 exchanges with 2s deadline. Merge/offer dispatch creates independent goroutines without an additional global handler limit; prepared merges are caller-memory bounded, not library bounded. **Recommendation:** rate-limit service-issued commands and total closure bytes across many engines; the export bound alone does not bound all metadata memory or server workload. [engine/remotecache/remotecache.go:29-51; engine/remotecache/session.go:134-156,219-259,284-321; dagql/cache_part_renewal.go:17-21; dagql/cache_value_merge.go:63-69]

### 10.2 Graph/gate atomicity

Value merge decides, relocates, validates and commits under graph write lock after expensive preparation outside it. Offer admission captures/probes, then rechecks captured row/gate/payload revision under graph+part-gate locks; a changed revision makes it unavailable. Gate only reports alreadyComplete when settled; installed-unsettled closes admission as executionStarted. This prevents a remote offer from overwriting work already running. State replies include replacement/dependency facts from the same decision lock scope. [dagql/cache_value_merge.go:14-50,121-183; dagql/cache_offer.go:105-129,279-351]

Offer owners independently retain referenced dependency rows while descriptions exist. Publication transfers preparation hold into the slot; replacement/retirement drops old owner after slot changes and releases callbacks after graph unlock. Settlement validates owning task token plus installation generation, then retires offer and marks complete. **Accepted offer does not consume a remote byte lease.** [dagql/cache_offer_owner.go:90-107,176-239; dagql/cache_part_content.go:756-785]

### 10.3 Snapshot transfer pins

Snapshot export creates an independent temporary containerd lease labeled `dagger.io/snapshot-transfer`, attaches snapshot resources and pins exported blobs. Import uses its own transfer pin; returned ref owns it until released/handed off to installed-row cleanup. On engine restart transfer leases are released only after durable owners are restored and before new transfers start; these are process-local storage liveness protections, not a remote engine availability promise. [engine/snapshots/lease.go:16-61,85-98,137-147; engine/snapshots/remote.go:19-29,73-90; engine/snapshots/import.go:18-42; dagql/cache_part_content.go:686-714]

### 10.4 Renewal synchronization

Bridge attachment/exchange mutex is deliberately **never nested** with graph, gate, lazy, payload or demand mutexes. Each bridge gets a fresh random epoch and sequence; detach atomically retires pending exchanges. A stale detach cannot affect a newer bridge. Per-demand renewal state shares one result across sources for identical target/content. Do not hold a cache operation waiting indefinitely for future service requests; integration Stop refuses new controls, detaches and cancels, then joins within shutdown context. [dagql/cache_part_renewal.go:31-90,224-282,334-437,444-503; engine/server/remote_cache.go:14-22,67-94]

### 10.5 No distributed lock protocol

The wire carries no object-store locks, lease tokens, multipart upload IDs, leader-election generations or consensus primitives. Engine-local mutexes and SQLite WAL do not make a many-replica cache-service safe merely by sharing an S3 bucket. **Recommendation:** start with one authoritative metadata writer per tenant/pool (many concurrent engine sockets), or explicitly implement fenced/transactional metadata ownership and a routing layer. Concurrent immutable blob uploads can be deduplicated, but metadata commits, GC, and donor/retention observations need coherent ordering. [engine/remotecache/protocol/protocol.go:49-93,175-180,311-324; dagql/cache_value_merge.go:121-183; dagql/cache_stored_parts.go:63-89]

Snapshot sharing is an **engine-local** opportunistic fill path for imported rows, enabled for remote-cache integration/fixture after local restore and schema preparation, not a separate network peer protocol. Its preparation is explicitly barred from initiating chain content acquisition. [engine/server/snapshot_sharing.go:15-50,70-92; dagql/cache_part_content.go:129-138,653-661]

## 11. Configuration: engines, clients and Helm

### 11.1 Engine environment/config

| Name | Meaning |
|---|---|
| `_EXPERIMENTAL_DAGGER_REMOTE_CACHE_URL` | Service base URL; nonempty env wins over `remoteCache.url`. Engine appends `/v1/engine` after removing one trailing slash. |
| `DAGGER_CLOUD_TOKEN` | Required engine token; literal `oidc` disables this cache integration. Used as Basic username. |
| JSON `remoteCache: {"url":"..."}` | Alternative service URL in engine config. Does not supply token. |
| `DAGGER_CLOUD_URL` | Separate Cloud OTLP destination for engine events; empty→default `https://api.dagger.cloud`. **Setting remote-cache URL does not redirect telemetry.** |
| `_EXPERIMENTAL_DAGGER_ENGINE_EVENTS` / JSON `telemetry.engineEvents` | Boolean event switch. Remote-cache integration sets EngineEvents true automatically. |
| `_DAGGER_TEST_REMOTE_CACHE_FIXTURE_ROOT` | Test-only hidden fixture filesystem root; not server storage config. |

Evidence: [cmd/engine/remotecache.go:14-62; engine/config/config.go:89-103; engine/remotecache/remotecache.go:167-173; engine/telemetry/cloud_exporters.go:105-115; cmd/engine/telemetry.go:147-183; core/remote_cache_fixture.go:1-5].

Remote-cache activation is evaluated by the **engine process**, not by an SDK request. Token alone is routinely forwarded by clients provisioning engines and intentionally does not activate this integration. For an already running shared engine, put the variables/config into that engine deployment; do not assume exporting a variable only in a CLI shell reconfigures its existing server. [cmd/engine/remotecache.go:18-23; engine/client/drivers/driver.go:40-53]

### 11.2 Helm: avoid the obsolete magicache wiring

The chart has generic `engine.env` and `engine.configJson`. It **does not** have a first-class new `remoteCache` Helm values section in the inspected values file. `magicache.enabled`, `magicache.url`, `magicache.token`, `magicache.secretName` wire the old variables `_EXPERIMENTAL_DAGGER_CACHESERVICE_URL` and `_EXPERIMENTAL_DAGGER_CACHESERVICE_TOKEN`; they do not wire the new names. [helm/dagger/values.yaml:4-25,228-237; helm/dagger/templates/engine-daemonset.yaml:82-100; helm/dagger/templates/engine-statefulset.yaml:86-105; helm/dagger/templates/magicache-secret.yaml:1-13]

**Recommended values example** (configuration inferred from actual generic chart fields; use a real Secret, not literal production credentials):

```yaml
magicache:
  enabled: false
engine:
  configJson: |
    {"remoteCache":{"url":"https://cache.example.invalid"}}
  env:
    - name: DAGGER_CLOUD_TOKEN
      valueFrom:
        secretKeyRef:
          name: remote-cache-engine-auth
          key: token
    - name: DAGGER_CLOUD_URL
      value: https://cache.example.invalid
```

Alternatively set `_EXPERIMENTAL_DAGGER_REMOTE_CACHE_URL` in engine.env and omit remoteCache.url. If the same self-hosted front end will ingest telemetry, `DAGGER_CLOUD_URL` points at its **OTLP base**, with `/v1/traces`, `/v1/logs`, `/v1/metrics` routed separately from `/v1/engine`; URL bases need not be identical. This sample does not prove all CLI/client telemetry routes are redirected—explicit per-session Cloud URL can take precedence over the engine environment and must be configured/verified in the deployment. [helm/dagger/templates/engine-daemonset.yaml:82-100; helm/dagger/templates/engine-config.yaml:1-18; engine/telemetry/cloud_exporters.go:23-25,105-115; engine/server/session_cloud_telemetry.go:66-73]

## 12. Recommended S3-backed implementation blueprint (not claimed Cloud internals)

The following describes a compatible **new implementation design** derived from the observed contracts, not a source-discovered S3 backend.

### 12.1 Components

1. **Authenticated WebSocket front end** with one engine-session state machine, peer-local message counters/waiter maps, hello identity/generation, ping handling, and a bounded command scheduler. A single metadata authority can serve many sockets concurrently. [engine/remotecache/protocol/protocol.go:6-8,22-39,85-93; engine/remotecache/session.go:25-63]
2. **OTLP ingestion/reconciliation** for call/lazy spans plus lifecycle log events. Extract only known contract versions, preserve resource cache/process/session identity and exact integers; order updates using replacement counter and generation/time rules. Reconcile missing/late observations; telemetry is not a perfectly reliable durable inventory stream. [engine/telemetryattrs/attrs.go:52-64,601-621,731-793,858-942; engine/server/engine_events.go:39-45,66-79; dagql/cache_remote_entries.go:644-724]
3. **Pinned dagql+core metadata pool** (`WithBlobStore`) with per-pool SQLite persistence, checkpoints, bounded memory, and registered codecs. Keep tenant auth, pool mapping, signer keys, in-flight reservations and administrative policy in your own storage/config. [dagql/cache_stored_parts.go:17-28; dagql/cache_persistence_worker.go:15-56; dagql/cache_persistence_codec.go:567-591]
4. **Immutable object store** with your chosen tenant-scoped key layout, e.g. an application-defined `<tenant>/blobs/<algorithm>/<encodedDigest>`. This is a suggested layout, **not** a protocol key. Use GET/PUT presigned URLs needing no engine-added headers; do not require SDK-multipart operations or checksum headers the engine does not send. [engine/remotecache/remotecache.go:242-279; dagql/cache_part_content.go:510-535]

### 12.2 Seed/export path

- Use observations/equivalence/availability to pick a bounded group of engine-local roots and selected completed parts. Issue export. Reconcile gone/busy/refused separately; unknown skip reasons should receive safe backoff, not be treated as successful roots. [engine/remotecache/protocol/protocol.go:129-173; ../dagger-evidence/14761.md:142-154]
- Validate export graph, prepare metadata merge outside work lock, commit under coherent metadata scheduling, preserve ordinal→service-entry mapping and original copy expiry. Inspect actual descriptor objects to enumerate needed blob digests/sizes. [dagql/cache_value_merge.go:52-197; dagql/cache_value_import.go:25-215; dagql/cache_transfer_types.go:58-88]
- Determine already durable objects; reserve/pin pending digests against GC; send one upload request with a presigned PUT per missing blob (or empty map). Different engines can reference the same immutable object; serialize/deduplicate publication as needed rather than holding the metadata graph lock during network I/O. [engine/remotecache/protocol/protocol.go:175-180; engine/remotecache/session.go:265-330; dagql/cache_stored_parts.go:30-89]
- After uploaded reply, verify all chain blobs required by a part are durable and have expected size/content identity. Relocate owner/service refs and register completed parts. Failed/missing blobs prevent publishing those parts but need not discard unrelated valid metadata/parts. Ambiguous disconnect needs recheck/re-export; do not assume exported chain providers survive reconnect. [engine/remotecache/session.go:25-27,284-330; dagql/cache_stored_parts.go:30-113]

### 12.3 Fresh-engine merge and later live offers

- Build StoredBundle for desired stored roots, attach fresh presigned GETs and donor-bound renewal keys to each chain, then send merge. Apply engine reply using the **actual kept target state**, not presumed sent state. Metadata merge can yield hits without downloads; downloads occur only on demanded parts. [dagql/cache_stored_bundle.go:10-40,309-318; dagql/cache_remote_entries.go:488-575; dagql/cache_part_content.go:129-156]
- For already-existing receiving entries, identify an available equivalent part, use PlaceOffer to map service owner refs, set cloudNumber to receiver's counterpart and renewalKey to **donor** authorization, send offer, then classify each outcome. Keep accepted as offered; promote to complete only with alreadyComplete/actual completion evidence at valid replacement count. [dagql/cache_part_placement.go:9-82; dagql/cache_offer.go:44-59,107-129; engine/remotecache/protocol/protocol.go:251-292]
- On renew, authenticate/authorize key against pool/donor/exact ordered layers, ensure required blobs are still present, and respond quickly with fresh addresses. Target much less than the engine's 2s renewal deadline; never perform a large blocking graph rebuild on this hot path. A renewal key should not allow arbitrary S3 key signing. [dagql/cache_part_renewal.go:17-26,36-46; engine/remotecache/protocol/protocol.go:311-324]

### 12.4 GC/crash/concurrent-engine correctness

- Persist graph checkpoints and control graph mutations independently of immutable byte uploads. Store successful part references only after byte durability. After crash, reconstruct roots/parts via StoredState and refresh ephemeral engine holdings from observations. [dagql/cache_persistence_worker.go:15-56; dagql/cache_stored_queries.go:128-156; dagql/cache_stored_parts.go:30-45]
- Mark all LiveBlobs plus **in-flight upload/publication reservations** and any advertised-URL grace policy before sweep; compare/fence again at deletion. Library LiveBlobs alone has no knowledge of objects uploaded but not yet registered. If you delete bytes while valid URLs are outstanding, engines can fall back, but you lose expected cache availability and must drop stale stored-part metadata. [dagql/cache_stored_queries.go:159-175; dagql/cache_stored_parts.go:162-191; engine/remotecache/session.go:284-321]
- Do not use bucket object TTL as value or retention TTL. Shared blobs can outlive one value's expiry and remain live through another stored part; deduplicate by immutable digest while managing value freshness/retention separately. [dagql/cache_transfer_types.go:40-47,58-74; dagql/cache_stored_queries.go:159-175,184-225]
- For multi-replica metadata service, add your own fenced leader/shard ownership, transactional update model and GC coordination. Neither WebSocket IDs nor cache generation is a distributed storage fencing token. [engine/remotecache/protocol/protocol.go:85-93,117-124; dagql/cache_value_merge.go:121-183]

### 12.5 Acceptance tests for a new service

These are proposed tests, not tests executed during this reconnaissance:

- Two isolated real engines: A seeds metadata/bytes, B merges and reads same content with no original execution; metadata-only read performs no blob GET; independent engine result numbers relocate correctly. [hack/designs/remote-cache/remote-cache-data-flows.md:31-33]
- Multiple concurrent engines export same digest; empty-upload release; failed PUT; lost uploaded/merged reply; reconnect invalidates old export IDs; service never leaves all export slots waiting without continuation. [engine/remotecache/session.go:219-330]
- Range reads at zero/nonzero offsets, exact 206 Content-Range, 200 fallback, short body, corrupt digest, 30s stall, 401/403/404/410 then success, renewed URL failure without infinite renewal. [dagql/cache_part_content.go:27-39,432-565; engine/snapshots/chain_error.go:73-97]
- Missing/expired URL with working renewal within 2s; mailbox saturation; delayed/duplicate old replies; service reconnect; missing blob drops all affected parts. [dagql/cache_part_renewal.go:17-26,224-282; dagql/cache_stored_parts.go:162-191]
- Live offer before/during execution; installed-unsettled reply must be executionStarted; accepted never treated as complete; mapped service dependencies and cycles validated. [dagql/cache_offer.go:69-129,279-351; dagql/cache_offer_owner.go:121-173]
- No-exec Container absent execMeta and absent FS cases, merged and live-offered; engine restart; root expiry vs URL expiry; replacement count rollback attempts; out-of-order generations and retention observations. [core/container_persistence_test.go:362-432; dagql/cache_remote_entries.go:168-219,644-724]
- Checkpoint/crash/restore while uploads and GC race; tenant token revoke and cross-tenant renewal/key misuse; message >64MiB; many simultaneous merge preparations with bounded memory. [dagql/cache_persistence_worker.go:15-56; engine/remotecache/protocol/protocol.go:26-38; dagql/cache_value_merge.go:63-69]

## 13. PR/issue findings, known gaps, and non-guarantees

### 13.1 Requested PRs/issues (actual status is in saved GitHub metadata)

Exact status, merge timestamps and SHAs: [../dagger-evidence/github-state.md:1-9].

- **#14241**, “remote cache: verification fixture, integration suite and CI”: still **open** when fetched, not evidence the entire proposed integration suite/TLA work is merged or continuously proven. Author describes fixture controls, barriers, in-process scripted HTTP and nested two-engine scenarios. Explicitly states full lifecycle runs, four remote TLA model configs, and opt-in disk-pressure diagnostic are not proved by CI. Current main's fixture code exists, but do not equate open PR description with all current behavior. [../dagger-evidence/14241.md:3-5,7-21,39-56; ../dagger-evidence/14241-comments.md:1-5]
- **#14508**, “remote cache: dogfood pull request”: still **open**, a deliberate empty commit to route only that PR's native CI engines through the private `dagger.io#5405` dogfood stack; explicitly “not meant to merge.” It is not a deployable public reference service. [../dagger-evidence/14508.md:3-11]
- **#14761**, merged **2026-10-20**: 13 fixes include narrow holding reads, complete/offered inventory, sharing race correction, installed-unsettled offer response, remote hit/install telemetry, split merge preparation/commit, refused-root skip, WAL truncation/NORMAL sync, mounted-service dependencies, normalized record/part copy optimization and shape validation. Source reflects those contracts; author-reported service work-lock and batch measurements are not protocol constants. The author's companion service retries executionStarted with 10s doubling to 5min and batches export roots up to 32; these are **service policy described by the PR**, not negotiated/public engine limits. Service must understand new `refused` skip before rolling out engines sending it. [../dagger-evidence/14761.md:7-18,20-248, especially 68-76,100-128,142-154,166-202]
- **#14884**, merged **2026-10-27**: fresh remote-cache engines on a large-module workload had author-reported ~15.9GB OOM from repeated telemetry cause-link sets, not S3/blob contents. Fix interns/share-sets in client telemetry index, releases completed part cleanup closure and logs bounded span-lane drops. Author reports fresh-engine peak ~6.0–6.1GB after fix and no dropped spans in that run. This does not establish a universal memory ceiling or justify sizing each engine/service at those measurements. A follow-up to reduce repeated links at source is explicitly separate. [../dagger-evidence/14884.md:7-43]
- **#14657**, still **open**: reproducible no-exec Container error reported on beta.17 and main reporting beta.18, including a self-hosted service using public primitives. **Do not report this as necessarily unfixed at inspected revision:** current `openNativeFinalPart` now calls `liveAbsentPart` when descriptor missing; it verifies computation completed, recognizes absent FS/execMeta and opens nil accessors. Current regression tests cover direct and demand-after-other-part-offer paths. Issue open/closed status and presence of a matching fix are separate facts. [../dagger-evidence/14657.md:7-49,66-79; core/part_open.go:137-191; core/container_persistence_test.go:362-432]

### 13.2 Explicit limitations/TODOs relevant to implementation

1. **Private reference server unavailable:** physical production layout, S3/object-store backend, server token format/issuance, revocation polling, renewal-key encoding, scheduler/GC/replica-lock policy are unverified. Public WithBlobStore is not an object-store backend. [../dagger-evidence/scope.md:1-8; dagql/cache_stored_parts.go:17-28]
2. **No complete public formal interoperability document/version negotiation:** wire reuses internal persisted/codec types, bundle version is 3, envelope version 5, hello only reports engineVersion. Pin engine/library revisions and test interoperability; don't assume every beta is compatible. [engine/remotecache/protocol/protocol.go:117-127; dagql/cache_transfer_types.go:94-101; dagql/cache_persistence_self.go:24-34]
3. **No automatic on-miss cache-service lookup:** service proactively transfers/offers; ordinary engine read uses local/offered chain/original operation. Full self-hosting needs discovery/telemetry or a separate explicit root strategy, not only an S3 bucket and WebSocket handler. [hack/designs/remote-cache/remote-cache-data-flows.md:7-9; cmd/engine/remotecache.go:49-62]
4. **No end-to-end exactly-once delivery:** socket loss forgets transfer providers and replies can be lost after committed mutation; event queue is bounded and drops, shutdown may lose unflushed events. Handle replays/out-of-order observations and repair drift. [engine/remotecache/session.go:25-27; engine/server/remote_cache_protocol.go:275-299; engine/server/engine_events.go:39-45,167-188]
5. **Caller owns memory/concurrency bounds:** prepared merges can be arbitrarily many/long-lived; remote handler dispatch has no separate merge/offer cap. #14884 fixes one engine memory cause, not arbitrary service scalability. [dagql/cache_value_merge.go:63-69; engine/remotecache/session.go:145-156; ../dagger-evidence/14884.md:11-34]
6. **Service performance claims remain scoped:** narrow holding read still clones/sorts recipes; production recipe count unknown. Large existing-cache commit scaling and end-to-end service effect of split merge were not measured in #14761. Busy checkpoint branch was reviewed but not exercised, and NORMAL durability relies on SQLite contract, not a power-loss experiment. Shape-only StoredBundle depends on codec visitor rewriting references only; receiving engine still fully validates. [../dagger-evidence/14761.md:36-40,126-128,180-182,226-248]
7. **Retry policy must avoid hot loops:** busy/refused/unknown skip reasons and executionStarted are not completions. Installed-but-unsettled owner-sync failure can cause indefinite service retries; reported policy is bounded in interval, not count. [../dagger-evidence/14761.md:68-76,142-154; dagql/cache_offer.go:107-129]
8. **Historical design documents are not the current wire:** the checked-in foundations markdown says no service/background availability leases/durable queues in its scope and contains earlier extra-digest/selection discussion. Current `transferExtras` transfers all nonempty extras, explicitly unlike the older markdown's content-derived-extra omission statement. Prefer current source when they conflict. [hack/designs/remote-cache/remote-cache-data-flows.md:17-29; dagql/cache_value_codec.go:12-23]
9. **Engine persistence documentation has narrower scope:** `internal-docs/cache_persistence.md` describes ordinary engines as in-memory with graceful-shutdown-only snapshots, unclean-start wipe and synchronous=OFF. Do not apply those statements to the blob-backed service pool: current code explicitly exempts blob-backed caches from unclean-shutdown wipe and gives them runtime Checkpoint/NORMAL. [internal-docs/cache_persistence.md:19-64,75-90,193-206; dagql/cache.go:617-627,2242-2265; dagql/cache_persistence_worker.go:15-56]
10. **Shared cacheVolumes.dir is separate:** config describes a plain-directory cache-volume backing where one engine uses the directory at a time; it is not this multi-engine remote immutable-layer protocol or an S3 backend. [engine/config/config.go:50-65]
11. **Engine GC is not remote blob GC:** gcmu/local memory/disk policies operate on engine cache/snapshot resources; service uses stored roots/parts/LiveBlobs and its own scheduling/storage deletion. Do not lift local 4GiB/3GiB metadata defaults or disk policy into a claim about server retention. [engine/server/gc.go:24-30,98-166; dagql/cache_stored_queries.go:128-229]

## 14. Verification performed for this document

- Cloned public repository, pinned commit and fetched beta.25 tag; read implementation and test code, not just code-search snippets.
- Retrieved requested PR/issue bodies, statuses and comments using `gh api`; raw responses and line-citable text snapshots saved alongside report.
- Compared protocol/adapter/part transfer sources against beta.25; recorded source-scope evidence.
- Identified inaccessible private service rather than fabricating server HTTP handlers, filesystem layout or S3 support.
- The appendix contains verbatim public type definitions as an exact wire-shape cross-check. A separate validation artifact checks source citation bounds and appendices against the checkout.
- **Not performed:** build/run a self-hosted cache service, execute Dagger integration tests, probe Dagger Cloud's private service, run S3 end-to-end interoperability or replicate the PR benchmark measurements. This deliverable is reconnaissance/specification, not a tested server implementation.

## Appendix A. Exact pinned wire declarations and supporting source excerpts

The following appendix is generated directly from the checked-out files, retaining original line numbers. It is the authoritative spelling/casing cross-check for the prose schemas, including nested call/envelope types. Treat these as source excerpts, not synthetic sample output.

### `engine/remotecache/protocol/protocol.go:1-324`

```go
   1 | // Package protocol defines the messages an engine and the remote cache
   2 | // service exchange over one WebSocket. Both sides import it, so a change to
   3 | // the wire format fails to build on the other side. Bundles, offers and
   4 | // addresses are dagql's own transfer types.
   5 | //
   6 | // Every message is one JSON object in one text message: an Envelope. Either
   7 | // side may send requests, and many may be in flight both ways. A reply names
   8 | // its request's ID in Re; a reply of type TypeError ends that request only.
   9 | package protocol
  10 | 
  11 | import (
  12 | 	"encoding/json"
  13 | 	"fmt"
  14 | 	"time"
  15 | 
  16 | 	"github.com/opencontainers/go-digest"
  17 | 
  18 | 	"github.com/dagger/dagger/dagql"
  19 | 	"github.com/dagger/dagger/engine/snapshots"
  20 | )
  21 | 
  22 | const (
  23 | 	// EnginePath is the service endpoint an engine dials.
  24 | 	EnginePath = "/v1/engine"
  25 | 
  26 | 	// MaxMessageBytes is the largest message either side accepts, set as
  27 | 	// each side's read limit. A reader closes the connection on a larger
  28 | 	// message, so a sender answers TypeError with TooLargeMessage instead.
  29 | 	MaxMessageBytes = 64 << 20
  30 | 
  31 | 	// PingInterval is how often the engine pings the service. A ping with
  32 | 	// no pong within PingTimeout closes the connection.
  33 | 	PingInterval = 15 * time.Second
  34 | 	PingTimeout  = 15 * time.Second
  35 | 
  36 | 	// CloseTokenRevoked is the WebSocket close status of a connection whose
  37 | 	// token the service found revoked.
  38 | 	CloseTokenRevoked = 4001
  39 | )
  40 | 
  41 | // Error messages both sides recognize.
  42 | const (
  43 | 	TooLargeMessage = "too large"
  44 | )
  45 | 
  46 | // Type names a message.
  47 | type Type string
  48 | 
  49 | const (
  50 | 	TypeHello    Type = "hello"
  51 | 	TypeWelcome  Type = "welcome"
  52 | 	TypeExport   Type = "export"
  53 | 	TypeExported Type = "exported"
  54 | 	TypeUpload   Type = "upload"
  55 | 	TypeUploaded Type = "uploaded"
  56 | 	TypeMerge    Type = "merge"
  57 | 	TypeMerged   Type = "merged"
  58 | 	TypeOffer    Type = "offer"
  59 | 	TypeOffered  Type = "offered"
  60 | 	TypeRenew    Type = "renew"
  61 | 	TypeRenewed  Type = "renewed"
  62 | 	TypeError    Type = "error"
  63 | )
  64 | 
  65 | // ReplyType returns the type of a successful reply to a request, and false
  66 | // for a type that is not a request.
  67 | func ReplyType(request Type) (Type, bool) {
  68 | 	switch request {
  69 | 	case TypeHello:
  70 | 		return TypeWelcome, true
  71 | 	case TypeExport:
  72 | 		return TypeExported, true
  73 | 	case TypeUpload:
  74 | 		return TypeUploaded, true
  75 | 	case TypeMerge:
  76 | 		return TypeMerged, true
  77 | 	case TypeOffer:
  78 | 		return TypeOffered, true
  79 | 	case TypeRenew:
  80 | 		return TypeRenewed, true
  81 | 	}
  82 | 	return "", false
  83 | }
  84 | 
  85 | // Envelope frames every message. ID numbers a message per sender, from 1; a
  86 | // reply names its request's ID in Re, and Re 0 means the message is not a
  87 | // reply.
  88 | type Envelope struct {
  89 | 	ID   uint64          `json:"id"`
  90 | 	Re   uint64          `json:"re,omitempty"`
  91 | 	Type Type            `json:"type"`
  92 | 	Body json.RawMessage `json:"body"`
  93 | }
  94 | 
  95 | // NewEnvelope encodes body into an envelope.
  96 | func NewEnvelope(id, re uint64, typ Type, body any) (Envelope, error) {
  97 | 	raw, err := json.Marshal(body)
  98 | 	if err != nil {
  99 | 		return Envelope{}, fmt.Errorf("encode %s body: %w", typ, err)
 100 | 	}
 101 | 	return Envelope{ID: id, Re: re, Type: typ, Body: raw}, nil
 102 | }
 103 | 
 104 | // Decode decodes the envelope's body into v.
 105 | func (e Envelope) Decode(v any) error {
 106 | 	if err := json.Unmarshal(e.Body, v); err != nil {
 107 | 		return fmt.Errorf("decode %s body: %w", e.Type, err)
 108 | 	}
 109 | 	return nil
 110 | }
 111 | 
 112 | // Error is the body of a TypeError reply.
 113 | type Error struct {
 114 | 	Message string `json:"message"`
 115 | }
 116 | 
 117 | // Hello is the engine's first message on a connection.
 118 | type Hello struct {
 119 | 	// CacheID and Generation are the engine cache's identity and start count.
 120 | 	CacheID       string `json:"cacheID"`
 121 | 	Generation    uint64 `json:"generation"`
 122 | 	EngineVersion string `json:"engineVersion"`
 123 | 	EngineName    string `json:"engineName"`
 124 | }
 125 | 
 126 | // Welcome is the service's reply to Hello.
 127 | type Welcome struct{}
 128 | 
 129 | // Export asks the engine for the values of some of its entries, by number,
 130 | // and the selected parts' bytes.
 131 | type Export struct {
 132 | 	Roots   []uint64       `json:"roots"`
 133 | 	Outputs []ExportOutput `json:"outputs,omitempty"`
 134 | }
 135 | 
 136 | // ExportOutput selects one part of an entry for upload.
 137 | type ExportOutput struct {
 138 | 	Number  uint64                     `json:"number"`
 139 | 	Address dagql.PersistedPartAddress `json:"address"`
 140 | }
 141 | 
 142 | // SkipReason says why a root was left out of an export or a merge.
 143 | type SkipReason string
 144 | 
 145 | const (
 146 | 	// SkipGone: the engine no longer has the entry.
 147 | 	SkipGone SkipReason = "gone"
 148 | 	// SkipBusy: the entry could not be captured now; ask again later. The
 149 | 	// skip's Message says what was busy.
 150 | 	SkipBusy SkipReason = "busy"
 151 | 	// SkipRefused: the engine refused to capture the entry, and would again;
 152 | 	// the skip's Message says why.
 153 | 	SkipRefused SkipReason = "refused"
 154 | 	// SkipExpired: a merged root had expired when the merge decided.
 155 | 	SkipExpired SkipReason = "expired"
 156 | )
 157 | 
 158 | // Exported answers Export. Bundle is nil when no root survived. The engine
 159 | // keeps the bundle's chains open under ExportID until Upload, or until the
 160 | // connection ends.
 161 | type Exported struct {
 162 | 	ExportID string             `json:"exportID,omitempty"`
 163 | 	Bundle   *dagql.ValueBundle `json:"bundle,omitempty"`
 164 | 	Skipped  []SkippedRoot      `json:"skipped,omitempty"`
 165 | }
 166 | 
 167 | // SkippedRoot is an exported root left out, by number. Message is the
 168 | // engine's error for a busy or refused root, cut to a bounded length.
 169 | type SkippedRoot struct {
 170 | 	Number  uint64     `json:"number"`
 171 | 	Reason  SkipReason `json:"reason"`
 172 | 	Message string     `json:"message,omitempty"`
 173 | }
 174 | 
 175 | // Upload gives the engine one presigned PUT per blob the service needs from
 176 | // an export. An empty map needs nothing, and the engine releases the chains.
 177 | type Upload struct {
 178 | 	ExportID string                   `json:"exportID"`
 179 | 	URLs     map[digest.Digest]string `json:"urls"`
 180 | }
 181 | 
 182 | // Uploaded answers Upload.
 183 | type Uploaded struct {
 184 | 	Done   []digest.Digest `json:"done"`
 185 | 	Failed []UploadFailure `json:"failed,omitempty"`
 186 | }
 187 | 
 188 | // UploadFailure is one blob the engine could not upload.
 189 | type UploadFailure struct {
 190 | 	Digest  digest.Digest `json:"digest"`
 191 | 	Message string        `json:"message"`
 192 | }
 193 | 
 194 | // Merge gives the engine a bundle of the Cloud's values. Its SenderNumbers
 195 | // are the Cloud's numbers, and its outputs carry GET addresses.
 196 | type Merge struct {
 197 | 	Bundle dagql.ValueBundle `json:"bundle"`
 198 | }
 199 | 
 200 | // Merged answers Merge with what the merge's commit left, read under the lock
 201 | // that decided it.
 202 | type Merged struct {
 203 | 	// Generation and EngineTime (Unix nanoseconds) are the engine cache's
 204 | 	// start count and clock at the commit. They order the reply's retention.
 205 | 	Generation uint64         `json:"generation"`
 206 | 	EngineTime int64          `json:"engineTime"`
 207 | 	Values     []MergedValue  `json:"values"`
 208 | 	Retained   []RetainedRoot `json:"retained"`
 209 | 	Skipped    []SkippedValue `json:"skipped,omitempty"`
 210 | }
 211 | 
 212 | // MergedValue is the engine entry a bundle record landed on, and its value
 213 | // state at the replacement count.
 214 | type MergedValue struct {
 215 | 	Ordinal       dagql.TransferOrdinal        `json:"ordinal"`
 216 | 	Number        uint64                       `json:"number"`
 217 | 	Replacements  uint64                       `json:"replacements"`
 218 | 	ExpiresAtUnix int64                        `json:"expiresAtUnix"`
 219 | 	Deps          []uint64                     `json:"deps"`
 220 | 	Complete      []dagql.PersistedPartAddress `json:"complete"`
 221 | 	Offered       []dagql.PersistedPartAddress `json:"offered"`
 222 | }
 223 | 
 224 | // RetainedRoot is a merged root the engine retains, until ExpiresAtUnix
 225 | // (0: never).
 226 | type RetainedRoot struct {
 227 | 	Ordinal       dagql.TransferOrdinal `json:"ordinal"`
 228 | 	ExpiresAtUnix int64                 `json:"expiresAtUnix"`
 229 | }
 230 | 
 231 | // SkippedValue is a bundle root left out, by ordinal.
 232 | type SkippedValue struct {
 233 | 	Ordinal dagql.TransferOrdinal `json:"ordinal"`
 234 | 	Reason  SkipReason            `json:"reason"`
 235 | }
 236 | 
 237 | // Offer places the Cloud's parts on the engine's own entries, by the
 238 | // engine's numbers.
 239 | type Offer struct {
 240 | 	Items []OfferItem `json:"items"`
 241 | }
 242 | 
 243 | // OfferItem offers parts to one engine entry. Each offer names one of the
 244 | // item entry's own parts; OfferedItem's Replacements and Deps are that
 245 | // entry's.
 246 | type OfferItem struct {
 247 | 	Number uint64      `json:"number"`
 248 | 	Offers []CloudPart `json:"offers"`
 249 | }
 250 | 
 251 | // CloudPart is one offered part and the Cloud counterpart of the entry its
 252 | // address resolves to (dagql.CloudPartOffer).
 253 | type CloudPart struct {
 254 | 	Offer              dagql.PersistedPartOffer `json:"offer"`
 255 | 	CloudNumber        uint64                   `json:"cloudNumber"`
 256 | 	CloudExpiresAtUnix int64                    `json:"cloudExpiresAtUnix"`
 257 | 	CloudStored        bool                     `json:"cloudStored"`
 258 | }
 259 | 
 260 | // Offered answers Offer, one item per offered item, in order.
 261 | type Offered struct {
 262 | 	Items []OfferedItem `json:"items"`
 263 | }
 264 | 
 265 | // OfferedItem answers one OfferItem: Gone, or the entry's replacement count
 266 | // and direct dependencies with each part's outcome, read where the outcome
 267 | // was decided.
 268 | type OfferedItem struct {
 269 | 	Number       uint64        `json:"number"`
 270 | 	Gone         bool          `json:"gone,omitempty"`
 271 | 	Replacements uint64        `json:"replacements,omitempty"`
 272 | 	Deps         []uint64      `json:"deps,omitempty"`
 273 | 	Parts        []OfferedPart `json:"parts,omitempty"`
 274 | }
 275 | 
 276 | // OfferedPart is one offered part's outcome.
 277 | type OfferedPart struct {
 278 | 	Address dagql.PersistedPartAddress `json:"address"`
 279 | 	Outcome OfferOutcome               `json:"outcome"`
 280 | 	Message string                     `json:"message,omitempty"`
 281 | }
 282 | 
 283 | // OfferOutcome is what the engine did with an offered part.
 284 | type OfferOutcome string
 285 | 
 286 | const (
 287 | 	OfferAccepted         OfferOutcome = "accepted"
 288 | 	OfferAlreadyComplete  OfferOutcome = "alreadyComplete"
 289 | 	OfferExecutionStarted OfferOutcome = "executionStarted"
 290 | 	OfferUnavailable      OfferOutcome = "unavailable"
 291 | 	OfferInvalid          OfferOutcome = "invalid"
 292 | )
 293 | 
 294 | // OfferOutcomeOf names a dagql offer outcome on the wire.
 295 | func OfferOutcomeOf(outcome dagql.OfferOutcome) (OfferOutcome, error) {
 296 | 	switch outcome {
 297 | 	case dagql.OfferAccepted:
 298 | 		return OfferAccepted, nil
 299 | 	case dagql.OfferAlreadyComplete:
 300 | 		return OfferAlreadyComplete, nil
 301 | 	case dagql.OfferExecutionStarted:
 302 | 		return OfferExecutionStarted, nil
 303 | 	case dagql.OfferUnavailable:
 304 | 		return OfferUnavailable, nil
 305 | 	case dagql.OfferInvalid:
 306 | 		return OfferInvalid, nil
 307 | 	}
 308 | 	return "", fmt.Errorf("unknown offer outcome %d", outcome)
 309 | }
 310 | 
 311 | // Renew asks the service for fresh download addresses of an offered chain's
 312 | // layers.
 313 | type Renew struct {
 314 | 	RenewalKey string                  `json:"renewalKey"`
 315 | 	Layers     []snapshots.ExportLayer `json:"layers"`
 316 | 	NeededBlob digest.Digest           `json:"neededBlob"`
 317 | }
 318 | 
 319 | // Renewed answers Renew: an address per layer, or Unavailable when a layer is
 320 | // missing.
 321 | type Renewed struct {
 322 | 	Addresses   map[digest.Digest]dagql.BlobAddress `json:"addresses,omitempty"`
 323 | 	Unavailable bool                                `json:"unavailable,omitempty"`
 324 | }
```

### `dagql/cache_transfer_types.go:11-112`

```go
  11 | // TransferOrdinal names a row within a bundle. Zero denotes optional absence.
  12 | type TransferOrdinal uint64
  13 | 
  14 | type ValueSelection struct {
  15 | 	Roots   []AnyResult
  16 | 	Outputs []SelectedValueOutput
  17 | 	// LeaveOutOutputsOutsideClosure leaves out a selected output whose entry
  18 | 	// the captured closure doesn't hold, instead of refusing the selection.
  19 | 	LeaveOutOutputsOutsideClosure bool
  20 | }
  21 | type SelectedValueOutput struct {
  22 | 	Result  AnyResult
  23 | 	Address PersistedPartAddress
  24 | }
  25 | type PersistedPartAddress struct {
  26 | 	OutputPath PersistedRefPath `json:"outputPath,omitempty"`
  27 | 	Part       PartKey          `json:"part"`
  28 | }
  29 | type SnapshotValue struct {
  30 | 	Kind     string                      `json:"kind"`
  31 | 	Path     string                      `json:"path,omitempty"`
  32 | 	Platform *ocispecs.Platform          `json:"platform,omitempty"`
  33 | 	Services []TransferredServiceBinding `json:"services,omitempty"`
  34 | }
  35 | type TransferredServiceBinding struct {
  36 | 	ServiceResultID uint64   `json:"serviceResultID"`
  37 | 	Hostname        string   `json:"hostname"`
  38 | 	Aliases         []string `json:"aliases,omitempty"`
  39 | }
  40 | type BlobAddress struct {
  41 | 	URL           string `json:"url"`
  42 | 	ExpiresAtUnix int64  `json:"expiresAtUnix,omitempty"`
  43 | }
  44 | type OfferedChain struct {
  45 | 	Layers     []snapshots.ExportLayer       `json:"layers"`
  46 | 	Addresses  map[digest.Digest]BlobAddress `json:"addresses,omitempty"`
  47 | 	RenewalKey string                        `json:"renewalKey,omitempty"`
  48 | }
  49 | type PersistedOfferOwner struct {
  50 | 	DependencyIDs []uint64 `json:"dependencyIDs,omitempty"`
  51 | }
  52 | type PersistedPartOffer struct {
  53 | 	Address PersistedPartAddress `json:"address"`
  54 | 	Value   SnapshotValue        `json:"value"`
  55 | 	Chain   OfferedChain         `json:"chain"`
  56 | 	Owner   PersistedOfferOwner  `json:"owner"`
  57 | }
  58 | type TransferredValue struct {
  59 | 	Ordinal TransferOrdinal `json:"ordinal"`
  60 | 	// SenderNumber is the entry's number in the sending cache. An engine that
  61 | 	// imports the value keeps it as its Cloud holding's number.
  62 | 	SenderNumber uint64 `json:"senderNumber"`
  63 | 	// SenderReplacements is the entry's replacement count in the sending
  64 | 	// cache, read with the record: it orders the value state the record
  65 | 	// describes.
  66 | 	SenderReplacements uint64          `json:"senderReplacements,omitempty"`
  67 | 	Record             PersistedRecord `json:"record"`
  68 | 	DependencyIDs      []uint64        `json:"dependencyIDs,omitempty"`
  69 | 	ExpiresAtUnix      int64           `json:"expiresAtUnix,omitempty"`
  70 | }
  71 | type TransferredRoot struct {
  72 | 	Ordinal       TransferOrdinal `json:"ordinal"`
  73 | 	ExpiresAtUnix int64           `json:"expiresAtUnix,omitempty"`
  74 | }
  75 | type TransferredOutput struct {
  76 | 	Ordinal TransferOrdinal      `json:"ordinal"`
  77 | 	Address PersistedPartAddress `json:"address"`
  78 | 	State   string               `json:"state"`
  79 | 	Value   *SnapshotValue       `json:"value,omitempty"`
  80 | 	Chain   *OfferedChain        `json:"chain,omitempty"`
  81 | 	Owner   *PersistedOfferOwner `json:"owner,omitempty"`
  82 | }
  83 | type ValueBundle struct {
  84 | 	Version int                 `json:"version"`
  85 | 	Roots   []TransferredRoot   `json:"roots"`
  86 | 	Values  []TransferredValue  `json:"values"`
  87 | 	Outputs []TransferredOutput `json:"outputs,omitempty"`
  88 | }
  89 | type ImportedValue struct {
  90 | 	Ordinal  TransferOrdinal
  91 | 	ResultID uint64
  92 | }
  93 | 
  94 | const valueBundleVersion = 3
  95 | 
  96 | // PersistedTransferCodec operates only on encoded copies, without resolving
  97 | // schemas, operations, result references or storage.
  98 | type PersistedTransferCodec interface {
  99 | 	NormalizeForeign(PersistedPayloadVisit) (ForeignPayload, error)
 100 | 	ValidateForeign(PersistedPayloadVisit) error
 101 | 	MapSnapshotParts(PersistedPayloadVisit) ([]CapturedCodecOutput, error)
 102 | }
 103 | type ForeignPayload struct{ JSON json.RawMessage }
 104 | type CapturedCodecOutput struct {
 105 | 	Address PersistedPartAddress
 106 | 	State   string
 107 | 	// ValueKind remains known when pending output metadata is not yet known.
 108 | 	ValueKind  string
 109 | 	Value      *SnapshotValue
 110 | 	Role       string
 111 | 	SnapshotID string
 112 | }
```

### `engine/snapshots/remote_type.go:1-35`

```go
   1 | package snapshots
   2 | 
   3 | import (
   4 | 	"context"
   5 | 	"time"
   6 | 
   7 | 	"github.com/containerd/containerd/v2/core/content"
   8 | 	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
   9 | )
  10 | 
  11 | type ExportLayer struct {
  12 | 	Descriptor  ocispecs.Descriptor
  13 | 	Description string
  14 | 	CreatedAt   *time.Time
  15 | }
  16 | 
  17 | type ExportChain struct {
  18 | 	Layers []ExportLayer
  19 | 	// Prefixes maps each snapshot the export walked, from the root to the
  20 | 	// exported one, to the number of Layers that rebuild it: the layers
  21 | 	// emitted up to and including it. A snapshot that added no layer maps to
  22 | 	// the prefix before it, and a scratch root to none.
  23 | 	Prefixes map[string]int
  24 | 	Provider content.InfoReaderProvider
  25 | 	pin      *resourcePin
  26 | }
  27 | 
  28 | // Release ends provider consumption. Copies of a locally exported chain share
  29 | // the same ownership, so Release is safe to call more than once.
  30 | func (c *ExportChain) Release(ctx context.Context) error {
  31 | 	if c == nil {
  32 | 		return nil
  33 | 	}
  34 | 	return c.pin.release(ctx)
  35 | }
```

### `dagql/cache_persistence_self.go:13-71`

```go
  13 | const (
  14 | 	persistedResultKindNull   = "null"
  15 | 	persistedResultKindObject = "object_self"
  16 | 	persistedResultKindScalar = "scalar_json"
  17 | 	persistedResultKindList   = "list"
  18 | 	// persistedResultKindRef names a separately attached row from inside a
  19 | 	// list. It carries only the row identity; the row's own envelope holds
  20 | 	// its body.
  21 | 	persistedResultKindRef = "result_ref"
  22 | )
  23 | 
  24 | // persistedResultEnvelopeVersion is the current interpretation of
  25 | // PersistedResultEnvelope. Every change to how an envelope is read cuts a new
  26 | // version, including scoped (OutputPath, Role) snapshot links; older versions are never read (see cachePersistenceSchemaVersion).
  27 | //
  28 | // 3: attached absent values keep their row identity as null envelopes;
  29 | // scalars decode losslessly; lists rebuild their declared recursive type from
  30 | // the recorded call instead of a flattened element name; object envelopes
  31 | // name their codec family; list items naming another row are result_ref
  32 | // envelopes without a duplicated body.
  33 | // 4: root origin and independent pending offer ownership.
  34 | const persistedResultEnvelopeVersion = 5
  35 | 
  36 | // PersistedResultEnvelope is the shared on-disk payload envelope for persisted
  37 | // result self values.
  38 | //
  39 | // This is intentionally opaque at the DB level (stored as self_payload bytes),
  40 | // while still carrying enough structured data to decode common SDK-return
  41 | // shapes (scalars, object IDs, lists, nested combinations).
  42 | type PersistedResultEnvelope struct {
  43 | 	Imported      bool                 `json:"imported,omitempty"`
  44 | 	PendingOffers []PersistedPartOffer `json:"pendingOffers,omitempty"`
  45 | 	// PersistDeclined is the entry's mark that its call declined to persist
  46 | 	// its value (sharedResult.persistDeclined). Only a saved envelope
  47 | 	// carries it: restore takes it back onto the entry, and no record sent
  48 | 	// to another cache carries it.
  49 | 	PersistDeclined bool `json:"persistDeclined,omitempty"`
  50 | 	// StoredParts are a blob-backed cache's stored parts of the value, which
  51 | 	// only its saved envelope carries: in memory they stay on the entry, so
  52 | 	// no record sent to an engine carries them.
  53 | 	StoredParts []PersistedPartOffer `json:"storedParts,omitempty"`
  54 | 	Version     int                  `json:"version"`
  55 | 	Kind        string               `json:"kind"`
  56 | 	// TypeName identifies the GraphQL value type of object and scalar
  57 | 	// envelopes.
  58 | 	TypeName string `json:"typeName,omitempty"`
  59 | 	// ObjectCodec identifies the Go payload family that produced ObjectJSON.
  60 | 	// Every user-named module object shares one family; TypeName still names
  61 | 	// the concrete GraphQL type.
  62 | 	ObjectCodec string `json:"objectCodec,omitempty"`
  63 | 	// ResultID is the attached row identity of the value. The root envelope
  64 | 	// of a row repeats that row's ID; a null root keeps the identity of an
  65 | 	// attached absent value; a result_ref item names another attached row.
  66 | 	ResultID              uint64                    `json:"resultID,omitempty"`
  67 | 	SessionResourceHandle SessionResourceHandle     `json:"sessionResourceHandle,omitempty"`
  68 | 	ObjectJSON            json.RawMessage           `json:"objectJSON,omitempty"`
  69 | 	ScalarJSON            json.RawMessage           `json:"scalarJSON,omitempty"`
  70 | 	Items                 []PersistedResultEnvelope `json:"items,omitempty"`
  71 | }
```

### `dagql/cache_persistence_codec.go:367-377`

```go
 367 | // PersistedRefPathElem is one declared position: a field name or a list
 368 | // index relative to the enclosing owner.
 369 | type PersistedRefPathElem struct {
 370 | 	Field   string `json:"field,omitempty"`
 371 | 	Index   int    `json:"index,omitempty"`
 372 | 	IsIndex bool   `json:"isIndex,omitempty"`
 373 | }
 374 | 
 375 | // PersistedRefPath is the declared position of a reference relative to its
 376 | // exact owner row, interpreted by codec family and envelope version.
 377 | type PersistedRefPath []PersistedRefPathElem
```

### `dagql/cache_persistence_codec.go:625-632`

```go
 625 | // PersistedRecord is one persisted row as the reference visitor sees it: its
 626 | // identity, generic envelope, recorded call and declared storage links.
 627 | type PersistedRecord struct {
 628 | 	ResultID      uint64                     `json:"resultID"`
 629 | 	Envelope      PersistedResultEnvelope    `json:"envelope"`
 630 | 	Call          *ResultCall                `json:"call"`
 631 | 	SnapshotLinks []PersistedSnapshotRefLink `json:"snapshotLinks,omitempty"`
 632 | }
```

### `dagql/result_call_frame.go:18-78`

```go
  18 | type ResultCallKind string
  19 | 
  20 | const (
  21 | 	ResultCallKindField     ResultCallKind = "field"
  22 | 	ResultCallKindSynthetic ResultCallKind = "synthetic"
  23 | )
  24 | 
  25 | type ResultCallType struct {
  26 | 	NamedType string          `json:"namedType,omitempty"`
  27 | 	NonNull   bool            `json:"nonNull,omitempty"`
  28 | 	Elem      *ResultCallType `json:"elem,omitempty"`
  29 | }
  30 | 
  31 | func NewResultCallType(gqlType *ast.Type) *ResultCallType {
  32 | 	if gqlType == nil {
  33 | 		return nil
  34 | 	}
  35 | 	return &ResultCallType{
  36 | 		NamedType: gqlType.NamedType,
  37 | 		NonNull:   gqlType.NonNull,
  38 | 		Elem:      NewResultCallType(gqlType.Elem),
  39 | 	}
  40 | }
  41 | 
  42 | func (typ *ResultCallType) clone() *ResultCallType {
  43 | 	if typ == nil {
  44 | 		return nil
  45 | 	}
  46 | 	return &ResultCallType{
  47 | 		NamedType: typ.NamedType,
  48 | 		NonNull:   typ.NonNull,
  49 | 		Elem:      typ.Elem.clone(),
  50 | 	}
  51 | }
  52 | 
  53 | func (typ *ResultCallType) toAST() *ast.Type {
  54 | 	if typ == nil {
  55 | 		return nil
  56 | 	}
  57 | 	return &ast.Type{
  58 | 		NamedType: typ.NamedType,
  59 | 		NonNull:   typ.NonNull,
  60 | 		Elem:      typ.Elem.toAST(),
  61 | 	}
  62 | }
  63 | 
  64 | type ResultCallRef struct {
  65 | 	ResultID uint64      `json:"resultID,omitempty"`
  66 | 	Call     *ResultCall `json:"call,omitempty"`
  67 | 
  68 | 	// shared is a runtime-only fast path for attached result refs. It is not
  69 | 	// persisted and must never be the sole source of truth for identity.
  70 | 	shared *sharedResult
  71 | }
  72 | 
  73 | type ResultCallModule struct {
  74 | 	ResultRef *ResultCallRef `json:"resultRef,omitempty"`
  75 | 	Name      string         `json:"name,omitempty"`
  76 | 	Ref       string         `json:"ref,omitempty"`
  77 | 	Pin       string         `json:"pin,omitempty"`
  78 | }
```

### `dagql/result_call_frame.go:107-156`

```go
 107 | type ResultCallArg struct {
 108 | 	Name        string             `json:"name,omitempty"`
 109 | 	IsSensitive bool               `json:"isSensitive,omitempty"`
 110 | 	Value       *ResultCallLiteral `json:"value,omitempty"`
 111 | }
 112 | 
 113 | func (arg *ResultCallArg) cloneWith(memo resultCallCloneMemo) *ResultCallArg {
 114 | 	if arg == nil {
 115 | 		return nil
 116 | 	}
 117 | 	return &ResultCallArg{
 118 | 		Name:        arg.Name,
 119 | 		IsSensitive: arg.IsSensitive,
 120 | 		Value:       arg.Value.cloneWith(memo),
 121 | 	}
 122 | }
 123 | 
 124 | type ResultCallLiteralKind string
 125 | 
 126 | const (
 127 | 	ResultCallLiteralKindNull           ResultCallLiteralKind = "null"
 128 | 	ResultCallLiteralKindBool           ResultCallLiteralKind = "bool"
 129 | 	ResultCallLiteralKindInt            ResultCallLiteralKind = "int"
 130 | 	ResultCallLiteralKindFloat          ResultCallLiteralKind = "float"
 131 | 	ResultCallLiteralKindString         ResultCallLiteralKind = "string"
 132 | 	ResultCallLiteralKindBytes          ResultCallLiteralKind = "bytes"
 133 | 	ResultCallLiteralKindEnum           ResultCallLiteralKind = "enum"
 134 | 	ResultCallLiteralKindDigestedString ResultCallLiteralKind = "digested_string"
 135 | 	ResultCallLiteralKindResultRef      ResultCallLiteralKind = "result_ref"
 136 | 	ResultCallLiteralKindList           ResultCallLiteralKind = "list"
 137 | 	ResultCallLiteralKindObject         ResultCallLiteralKind = "object"
 138 | )
 139 | 
 140 | type ResultCallLiteral struct {
 141 | 	Kind ResultCallLiteralKind `json:"kind"`
 142 | 
 143 | 	BoolValue   bool    `json:"boolValue,omitempty"`
 144 | 	IntValue    int64   `json:"intValue,omitempty"`
 145 | 	FloatValue  float64 `json:"floatValue,omitempty"`
 146 | 	StringValue string  `json:"stringValue,omitempty"`
 147 | 	BytesValue  []byte  `json:"bytesValue,omitempty"`
 148 | 	EnumValue   string  `json:"enumValue,omitempty"`
 149 | 
 150 | 	DigestedStringValue  string        `json:"digestedStringValue,omitempty"`
 151 | 	DigestedStringDigest digest.Digest `json:"digestedStringDigest,omitempty"`
 152 | 
 153 | 	ResultRef    *ResultCallRef       `json:"resultRef,omitempty"`
 154 | 	ListItems    []*ResultCallLiteral `json:"listItems,omitempty"`
 155 | 	ObjectFields []*ResultCallArg     `json:"objectFields,omitempty"`
 156 | }
```

### `dagql/result_call_frame.go:189-222`

```go
 189 | type ResultCall struct {
 190 | 	Kind        ResultCallKind  `json:"kind"`
 191 | 	Type        *ResultCallType `json:"type,omitempty"`
 192 | 	Field       string          `json:"field,omitempty"`
 193 | 	SyntheticOp string          `json:"syntheticOp,omitempty"`
 194 | 	View        call.View       `json:"view,omitempty"`
 195 | 	Nth         int64           `json:"nth,omitempty"`
 196 | 	EffectIDs   []string        `json:"effectIDs,omitempty"`
 197 | 	// ExtraDigests are the original extra digests explicitly attached when this
 198 | 	// call/result was first created. They are useful provenance, but they are
 199 | 	// not the authoritative merged digest state. The cache/e-graph remains the
 200 | 	// source of truth for the full merged output-equivalence digest set.
 201 | 	ExtraDigests   []call.ExtraDigest `json:"extraDigests,omitempty"`
 202 | 	Receiver       *ResultCallRef     `json:"receiver,omitempty"`
 203 | 	Module         *ResultCallModule  `json:"module,omitempty"`
 204 | 	Args           []*ResultCallArg   `json:"args,omitempty"`
 205 | 	ImplicitInputs []*ResultCallArg   `json:"implicitInputs,omitempty"`
 206 | 
 207 | 	// ProfileSkip records that the wcprof OTel second source must NOT profile this
 208 | 	// call: the reflection / schema-walk class that normal telemetry already hides
 209 | 	// and that, when profiled, dominates OTel volume on a module-load workload.
 210 | 	// (Native wcprof is opt-in/dev-only and keeps full detail — it ignores this bit.) It is set once, before the frame is
 211 | 	// used, by core.AroundFunc from the static profileSkip(receiverType, field)
 212 | 	// predicate, and it travels with the frame through clone()/fork() and JSON
 213 | 	// persistence — so every singleflight joiner, every derived (nth-element) child,
 214 | 	// every adopted/copied/imported result carries the producer's decision without a
 215 | 	// per-site provenance audit. It is a pure function of the recipe, so it is
 216 | 	// EXCLUDED from every digest (recipeDigest / contentPreferredDigest /
 217 | 	// selfDigestAndInputRefs / callPB / recipeID) — two frames with the same recipe
 218 | 	// necessarily agree on it, so excluding it is consistent, and it must never enter
 219 | 	// callKey/callDigest/concurrencyKey. Only the OTel emit gates in cache.go read it;
 220 | 	// native wcprof is opt-in/dev-only and keeps full detail (not gated). See
 221 | 	// profileSkip in core/telemetry.go.
 222 | 	ProfileSkip bool `json:"profileSkip,omitempty"`
```

### `engine/telemetryattrs/attrs.go:858-942`

```go
 858 | // Engine cache events (dagger.io/engine.cache).
 859 | //
 860 | // Outside any session, the engine reports what happens to its dagql cache as
 861 | // OpenTelemetry log records on its own logger provider, exported to Dagger
 862 | // Cloud under the engine's own token when the engine-events switch is on.
 863 | // Each record's instrumentation scope is EngineEventScope, its body is the
 864 | // JSON encoding of the event's type below (an OTLP string), and it carries
 865 | // EngineEventAttr (the event's kind) and EngineCacheAttr (the cache's
 866 | // identity and generation). The record's timestamp is the engine's clock when
 867 | // the event happened; the process resource names the engine instance
 868 | // (service.instance.id).
 869 | const (
 870 | 	// EngineEventScope is the instrumentation scope of engine cache events.
 871 | 	EngineEventScope = "dagger.io/engine.cache"
 872 | 	// EngineEventAttr is the record attribute holding the event's kind.
 873 | 	EngineEventAttr = "dagger.io/engine.event"
 874 | 
 875 | 	// EngineEventStart: the engine opened, and possibly restored, its cache
 876 | 	// (EngineStartEvent).
 877 | 	EngineEventStart = "engine.start"
 878 | 	// EngineEventPrune: a prune run dropped retention edges
 879 | 	// (EnginePruneEvent). Runs that drop none send nothing.
 880 | 	EngineEventPrune = "engine.prune"
 881 | 	// EngineEventShare: a snapshot-sharing pass completed parts
 882 | 	// (EngineShareEvent).
 883 | 	EngineEventShare = "engine.share"
 884 | 	// EngineEventStop: the engine closed its cache (EngineStopEvent).
 885 | 	EngineEventStop = "engine.stop"
 886 | )
 887 | 
 888 | // EngineStartEvent is the body of an engine.start event.
 889 | type EngineStartEvent struct {
 890 | 	EngineVersion string `json:"engineVersion"`
 891 | 	EngineName    string `json:"engineName"`
 892 | 	// Restored reports that the engine opened the cache's existing database
 893 | 	// rather than starting empty.
 894 | 	Restored bool `json:"restored"`
 895 | 	// RestoredEntries counts the entries the restore installed, not
 896 | 	// counting type definitions.
 897 | 	RestoredEntries int `json:"restoredEntries"`
 898 | 	// WipedCache is the identity of a cache database this start wiped, if
 899 | 	// any.
 900 | 	WipedCache string `json:"wipedCache,omitempty"`
 901 | }
 902 | 
 903 | // EnginePruneEvent is the body of an engine.prune event: every retention
 904 | // edge one prune run dropped.
 905 | type EnginePruneEvent struct {
 906 | 	Drops []EngineRetentionDrop `json:"drops"`
 907 | }
 908 | 
 909 | // EngineRetentionDrop is one retention edge a prune run dropped.
 910 | type EngineRetentionDrop struct {
 911 | 	// ResultID is the entry's result number.
 912 | 	ResultID uint64 `json:"resultId"`
 913 | 	// DroppedAtUnixNano is the engine's clock when it dropped the edge.
 914 | 	DroppedAtUnixNano int64 `json:"droppedAtUnixNano"`
 915 | }
 916 | 
 917 | // EngineShareEvent is the body of an engine.share event: every part one
 918 | // snapshot-sharing pass completed.
 919 | type EngineShareEvent struct {
 920 | 	Parts []EngineSharedPart `json:"parts"`
 921 | }
 922 | 
 923 | // EngineSharedPart is one part a snapshot-sharing pass completed.
 924 | type EngineSharedPart struct {
 925 | 	// ResultID is the entry's result number.
 926 | 	ResultID uint64 `json:"resultId"`
 927 | 	// Part is the part's address, as in dagger.io/cache.parts.
 928 | 	Part string `json:"part"`
 929 | 	// Deps are the result numbers of the entry's dependencies.
 930 | 	Deps []uint64 `json:"deps"`
 931 | 	// Replacements is the entry's replacement count (CacheReplacementsAttr),
 932 | 	// when not 0: the part and the dependencies are its current value's.
 933 | 	Replacements uint64 `json:"replacements,omitempty"`
 934 | }
 935 | 
 936 | // EngineStopEvent is the body of an engine.stop event.
 937 | type EngineStopEvent struct {
 938 | 	// Clean reports that the engine saved its cache for its next start.
 939 | 	Clean bool `json:"clean"`
 940 | 	// SavedEntries counts the entries the save wrote.
 941 | 	SavedEntries int `json:"savedEntries"`
 942 | }
```

### `dagql/types.go:100-100`

```go
 100 | type SessionResourceHandle string
```

### `dagql/cache_persistence_self.go:689-702`

```go
 689 | // PersistedSnapshotRefLink is a generic non-opaque link from a persisted result
 690 | // self payload to one durable snapshot ref key.
 691 | type PersistedSnapshotRefLink struct {
 692 | 	RefKey     string
 693 | 	Role       string
 694 | 	OutputPath PersistedRefPath `json:"outputPath,omitempty"`
 695 | 	// Borrowed marks a snapshot the result holds only as a copy of a part
 696 | 	// of a result it depends on, which owns it: that result outlives this
 697 | 	// one (a dependency is kept while its dependents keep their values), so
 698 | 	// its owner lease keeps the snapshot, and the owner lease sync attaches
 699 | 	// none for this result. It isn't persisted: after a restart, the copy's
 700 | 	// imported links attach leases of their own again.
 701 | 	Borrowed bool `json:"borrowed,omitempty"`
 702 | }
```

### `engine/telemetryattrs/attrs.go:289-313`

```go
 289 | 
 290 | // Call payloads over OTel logs.
 291 | //
 292 | // A client rebuilds a dagql call ID by walking the chain a call references
 293 | // and looking up a payload for EVERY frame it reaches (dagui's
 294 | // extractIntoDAG). New engines therefore publish a call's root and complete
 295 | // transitive closure as log records when they emit that call's span, minus
 296 | // frames already sent to the same delivery domain.
 297 | //
 298 | // CallPayloadContentType is the dagger.io/content.type value identifying such
 299 | // a record: its body is one deterministic protobuf encoding of callpbv1.Call.
 300 | // The payload carries its own Call.Digest, which consumers use verbatim. Like
 301 | // every content type, it describes the body — records carry it under the
 302 | // ordinary core instrumentation scope.
 303 | const CallPayloadContentType = "application/vnd.dagger.call+proto"
 304 | 
 305 | // CallPayloadDigestAttr repeats a call payload record's embedded Call.Digest
 306 | // as a log attribute, so a log store can find a payload by digest without
 307 | // decoding bodies. The body stays authoritative; consumers that decode it
 308 | // need not read this.
 309 | //
 310 | // It is deliberately not dagger.io/dag.digest: on a log record that key
 311 | // already means "render this beneath the call that created the digest"
 312 | // (dagui's routeLog), which a payload record must never trigger.
 313 | const CallPayloadDigestAttr = "dagger.io/dag.call.digest"
```

### `engine/telemetryattrs/attrs.go:595-625`

```go
 595 | // Producer condition: the attributes are stamped by core.AroundFunc's completion
 596 | // callback from a request-only evidence carrier (dagql.CacheDecision) that
 597 | // core allocates only when the call's span records and the call is not
 598 | // ProfileSkip-classified — so suppressed, deduplicated, introspection and
 599 | // profile-skipped calls record nothing, and no new spans are ever created.
 600 | const (
 601 | 	// CacheContractAttr is the producer-contract version marker. Consumers must
 602 | 	// read cache.* facts only from spans carrying this marker and treat unknown
 603 | 	// versions as not eligible. Semantics of every fact within a version are
 604 | 	// frozen once released; new optional facts may be added under the same
 605 | 	// version (presence-detected); any semantic change to an existing fact
 606 | 	// requires bumping the version. Stamped whenever the evidence carrier
 607 | 	// stamps, so it governs every other cache.* attribute on the span.
 608 | 	CacheContractAttr = "dagger.io/cache.contract"
 609 | 	// CacheContractV1 is the current (and first) contract version value.
 610 | 	CacheContractV1 = "1"
 611 | 
 612 | 	// CacheOutcomeAttr is what the cache decided for this call: one of
 613 | 	// CacheOutcomeHit (reused a cached result), CacheOutcomeExecuted (miss —
 614 | 	// the resolver ran), CacheOutcomeJoined (deduplicated into concurrent
 615 | 	// identical in-flight work under the same concurrency key), or
 616 | 	// CacheOutcomeUncached (DoNotCache policy: no lookup, no dedupe, no
 617 | 	// publication). A hit on a still-pending lazy shell stamps
 618 | 	// CacheOutcomeHit — the lookup fact — while the separate evaluation facts
 619 | 	// (dagger.io/dag.cached, dagger.io/dag.pending) keep their existing
 620 | 	// meaning.
 621 | 	CacheOutcomeAttr     = "dagger.io/cache.outcome"
 622 | 	CacheOutcomeHit      = "hit"
 623 | 	CacheOutcomeExecuted = "executed"
 624 | 	CacheOutcomeJoined   = "joined"
 625 | 	CacheOutcomeUncached = "uncached"
```

### `engine/telemetryattrs/attrs.go:691-793`

```go
 691 | 	// CacheSelfDigestAttr is the engine-derived structural self digest of the
 692 | 	// call: the operation, its literal arguments (sensitive values redacted),
 693 | 	// implicit inputs, list selection and schema view — with reference-valued
 694 | 	// inputs factored out into CacheStructuralInputsAttr. Two calls with equal
 695 | 	// self digests are "the same operation over possibly different inputs".
 696 | 	CacheSelfDigestAttr = "dagger.io/cache.self_digest"
 697 | 
 698 | 	// CacheStructuralInputsAttr is the exact ordered structural-input digest
 699 | 	// list the engine's equivalence lookup keys on — receiver, reference-valued
 700 | 	// arguments in argument order, digest-witnessed strings, then the module
 701 | 	// reference — emitted as a native OTLP string ARRAY (StringSliceValue),
 702 | 	// the one non-STRING value in this contract. Order is semantic:
 703 | 	// CacheMissUnknownInputAttr indexes into this list. An empty-but-present
 704 | 	// array means "structural identity stamped, no structural inputs";
 705 | 	// absence means no structural identity was stamped at all. It is a
 706 | 	// different projection than dagger.io/dag.inputs (which deduplicates and
 707 | 	// omits the module), and that attribute is unchanged.
 708 | 	CacheStructuralInputsAttr = "dagger.io/cache.structural_inputs"
 709 | 
 710 | 	// CachePairingDigestAttr is the self digest computed with implicit inputs
 711 | 	// excluded: "this operation, these literal arguments, this view/selection —
 712 | 	// over whatever inputs, in whatever scope". It is the engine-authored
 713 | 	// cross-run pairing anchor: equal pairing digests are counterpart
 714 | 	// CANDIDATES for run-to-run comparison (an index, not proof), so
 715 | 	// per-client/per-session implicit inputs can never break candidate
 716 | 	// discovery, while remaining visible by name in dagger.io/dag.call. For a
 717 | 	// call with no implicit inputs it equals CacheSelfDigestAttr.
 718 | 	CachePairingDigestAttr = "dagger.io/cache.pairing_digest"
 719 | 
 720 | 	// CacheOutputContentDigestAttr is the completed result's RECORDED content
 721 | 	// digest — the last content-labeled extra digest on the result's
 722 | 	// authoritative call frame at span completion — emitted only when
 723 | 	// non-empty, for any stamped outcome that returned a result (hit,
 724 | 	// executed, and joined calls alike). It is deliberately the recorded fact
 725 | 	// only (never the derived content-preferred digest): absence means "no
 726 | 	// recorded content identity at completion", nothing more. A lazy result may
 727 | 	// gain its content digest only after this span ends; that later fact is
 728 | 	// simply not claimed here.
 729 | 	CacheOutputContentDigestAttr = "dagger.io/cache.output.content_digest"
 730 | 
 731 | 	// CacheResultIDAttr is the decimal-string engine-local result number of
 732 | 	// the call's cache-backed result, stamped for any stamped outcome that
 733 | 	// returned one. Together with the cache identity resource attribute
 734 | 	// (EngineCacheAttr) it names one entry of one cache for the cache's
 735 | 	// lifetime.
 736 | 	CacheResultIDAttr = "dagger.io/cache.result.id"
 737 | 
 738 | 	// The entry's state in its cache, stamped beside CacheResultIDAttr when
 739 | 	// the span ends, so a consumer can follow the cache's ownership without
 740 | 	// the engine's internals. A lazy-evaluation span carries CacheResultIDAttr,
 741 | 	// CacheDepsAttr, CachePartsAttr and CacheOutputContentDigestAttr for the
 742 | 	// entry it evaluated, and no CacheOutcomeAttr.
 743 | 
 744 | 	// CacheDepsAttr lists the result numbers of the entry's dependencies, of
 745 | 	// every kind, as decimal strings (native string array). An empty list
 746 | 	// records that the entry has none.
 747 | 	CacheDepsAttr = "dagger.io/cache.deps"
 748 | 	// CacheRetainedAttr is "true" when the entry has a retention edge, the
 749 | 	// record that keeps it after its session, or when this call's publication
 750 | 	// adds one; absent otherwise. It states the edge at the span's end.
 751 | 	CacheRetainedAttr = "dagger.io/cache.retained"
 752 | 	// CacheRetentionExpiresAttr is the retention edge's expiry, in decimal
 753 | 	// Unix seconds, when retained and the edge expires.
 754 | 	CacheRetentionExpiresAttr = "dagger.io/cache.retention.expires"
 755 | 	// CacheExpiresAttr is the entry's own expiry, when it stops serving as a
 756 | 	// cache hit, in decimal Unix seconds, when it has one.
 757 | 	CacheExpiresAttr = "dagger.io/cache.expires"
 758 | 	// CachePartsAttr lists the entry's complete parts: the parts its record
 759 | 	// reports complete, including absent and metadata ones, which own no
 760 | 	// filesystem bytes. Each is its part address (native string array). On a
 761 | 	// lazy-evaluation span, it lists them as the attempt left them. The
 762 | 	// attribute is omitted when there are none.
 763 | 	CachePartsAttr = "dagger.io/cache.parts"
 764 | 	// CacheTypeAttr is the name of the entry's type, such as "Container".
 765 | 	CacheTypeAttr = "dagger.io/cache.type"
 766 | 	// CacheReplacementsAttr is the decimal-string count of the times the
 767 | 	// entry's value was replaced in place under the same result number, read
 768 | 	// with the state beside it: the parts, dependencies and expiry it names
 769 | 	// belong to that value. Absent while the count is 0.
 770 | 	CacheReplacementsAttr = "dagger.io/cache.replacements"
 771 | 
 772 | 	// A part installed from a Cloud offer, stamped on the lazy-evaluation
 773 | 	// span of the task that installed it, which names the receiving entry
 774 | 	// (CacheResultIDAttr). That task installs one part, and the demanding
 775 | 	// call is an ancestor of the span by causal parent (WcprofParentAttr,
 776 | 	// else the parent). Calls that join an install in progress wait on that
 777 | 	// span.
 778 | 	//
 779 | 	// CacheOfferPartAttr is the installed part's address, as in
 780 | 	// CachePartsAttr.
 781 | 	CacheOfferPartAttr = "dagger.io/cache.offer.part"
 782 | 	// CacheOfferBytesAttr is the decimal-string count of bytes downloaded for
 783 | 	// the part: layers the engine already had are not downloaded.
 784 | 	CacheOfferBytesAttr = "dagger.io/cache.offer.bytes"
 785 | 	// CacheOfferDurationAttr is the decimal-string count of nanoseconds from
 786 | 	// the start of the part's import to its install, over which the download
 787 | 	// and the layers' application interleave.
 788 | 	CacheOfferDurationAttr = "dagger.io/cache.offer.duration_ns"
 789 | 
 790 | 	// CacheSessionSpansAttr is the decimal-string count of the spans of one
 791 | 	// session that carry CacheResultIDAttr, stamped on the session's
 792 | 	// WcprofSessionCompleteAttr carrier span, which marks the session's end.
 793 | 	CacheSessionSpansAttr = "dagger.io/cache.session.spans"
```

