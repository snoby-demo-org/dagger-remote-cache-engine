> **Historical design/review record, not current implementation status.** See [current design](../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Blob ledger — R5

Normative. Supersedes R4. Adopts the R4 structural model unchanged (one writer, one mutex, liveness from `LiveBlobs()`), and closes round-5 findings: authority handoff (GPT F1), ambiguous-operation terminality (GPT F2), the failed/absent allocation mismatch (GPT F3), inherited-owner adoption (GPT F4), store I/O in the continuation budget (GPT F5), durable publish-retry state (GPT F6), retired-generation charging (GPT F7/F10), quarantine transition success (GPT F16), renewal budget (GPT F15).

## 0. Authority model and handoff

```
                    ┌───────────────────────────────────────────────────┐
engine ──WS──► dcache (one authoritative writer per pool, I6, epoch E)   │
                    │  mutex M  ← publish, GC mark, restore serialised    │
                    │  dagql.Cache (in-process graph; the ONLY authority) │
                    └───────────────┬───────────────────────────────────┘
                                    │ durable state
                                    ▼
                              Postgres  `blob`, `blob_object_gc`,
                                        `part_chain`, `pool_state`,
                                        `op_inflight`, `pending_publish`
```

- **The graph is the authority for liveness.** A blob is live iff some stored part names it: `dagql.Cache.LiveBlobs()` (`dagql/cache_stored_queries.go:162`, `egraphMu.RLock`). Recomputed at mark time; no counter.
- **Postgres is the authority for bytes** (`state`, `gen`, `last_url_mint_at`).
- **One mutex `M`** serialises publish, GC mark and restore within the one process. No Postgres lock is ever held across `SetStoredPart`.

### 0.1 The handoff problem, and the protocol

An epoch predicate inside a statement is a **snapshot test, not a promotion fence** (GPT round-5 F1). Under Read Committed a statement keeps the snapshot it started with; `now()` is transaction-start time. So a mark that started under epoch 1, blocked on a row lock, can commit *after* epoch 2 is granted, deleting a blob the new leader's restored graph names. Two processes have two mutexes; a paused process cannot run its self-termination code.

**Protocol, all of it mandatory:**

1. **Every mutation is an operation with a durable lease.** Before executing, the writer autocommits a row into `op_inflight (pool, epoch, op_id, kind, started_at)`; after the mutation's terminal outcome is known, it deletes the row. The row is inserted **outside** the mutation's transaction so a new leader can see it.
2. **Promotion drains.** A new leader takes `epoch = old+1` and sets `pool_state.handoff_from_epoch = old`, then refuses to publish, mark, merge or serve until `SELECT count(*) FROM op_inflight WHERE pool=$1 AND epoch <= $old` is 0.
3. **Drain is bounded.** A stale op row older than `max_op_seconds` (5 s) is terminated with `pg_terminate_backend` and confirmed gone; the leader does not proceed until the backend termination is confirmed by the row disappearing. Every mutation runs with `statement_timeout = 2 s`, so no operation can outlive the drain budget indefinitely.
4. **Inherited marks are reverted, never trusted.** After the drain, before its first mark, the new leader reads every `deleting` row inherited from an earlier epoch and, for each whose digest **is** in its own `LiveBlobs()` and whose object still exists, transitions it back: `deleting → verified` (epoch-fenced). This makes any stale mark that did commit harmless — the failure direction stays "keep bytes". Object deletion only ever happens after a row is `deleting` **and** a post-handoff mark has confirmed non-liveness in the current epoch.
5. **`op_inflight` residue is an alarm, not a retry loop.** If rows survive the drain budget, the leader refuses service and alerts; it never guesses.

This is the fence the R4 text lacked. It costs one autocommitted row per mutation (two extra round trips) and one drain wait at promotion (seconds).

### 0.2 Ambiguous operations

A cancellation dispatch does not prove a command was cancelled (libpq §32.7); a connection loss leaves the terminal outcome unknown (GPT F2). **An ambiguous outcome is neither success nor rollback.**

- The writer enters `pool_failed` (in-process flag + `pool_state.pool_failed_at`).
- **Publishing stops immediately and stays stopped** until a fresh connection proves the outcome: the writer closes the old connection, opens a new one, and reads every digest of the ambiguous operation under `M`. Only that positive read reopens publication.
- If the ambiguous operation was a mark and it did in fact commit, the reopen path runs the inherited-`deleting` revert check (§0.1 step 4) before serving.
- The stale operation's `op_inflight` row is deleted only after its terminal outcome is observed.

## 1. Tables

```sql
CREATE TABLE blob (
  pool             text NOT NULL,
  digest           text NOT NULL,              -- 'sha256:<hex>'
  gen              int  NOT NULL DEFAULT 0,
  state            text NOT NULL DEFAULT 'absent',
  size             bigint,
  sha256           text,
  lease_until      timestamptz,
  reserved_at      timestamptz,
  owner_export     text,
  last_url_mint_at timestamptz NOT NULL DEFAULT now(),
  verified_at      timestamptz,
  actor_epoch      bigint,
  PRIMARY KEY (pool, digest),
  CHECK (state IN ('absent','pending','verified','deleting','failed','quarantined'))
);

CREATE TABLE blob_object_gc (                  -- objects to destroy, drained idempotently
  pool text NOT NULL, digest text NOT NULL, gen int NOT NULL,
  bytes bigint,                                -- charged until destroyed (GPT F7)
  queued_at timestamptz NOT NULL DEFAULT now(),
  attempts int NOT NULL DEFAULT 0,
  PRIMARY KEY (pool, digest, gen)
);

CREATE TABLE pool_leader (
  pool text PRIMARY KEY, holder text, epoch bigint, lease_until timestamptz
);

CREATE TABLE pool_state (
  pool text PRIMARY KEY,
  handoff_from_epoch bigint,
  restore_complete_epoch bigint,               -- NULL until §10 step 4 has run under M
  pool_failed_at timestamptz,
  last_mark_at timestamptz, last_eviction_at timestamptz,
  live_blobs bigint, ckpt_live_blobs bigint
);

CREATE TABLE op_inflight (                     -- §0.1: durable per-mutation lease
  pool text NOT NULL, epoch bigint NOT NULL, op_id uuid NOT NULL,
  kind text NOT NULL,                          -- reserve|verify|publish|mark|mint|sweep
  started_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (pool, op_id)
);

CREATE TABLE part_chain (                      -- service-side part inventory, written at publish
  pool text NOT NULL, chain_hash text NOT NULL,   -- sha256 over ordered layer digests
  layers text[] NOT NULL, bytes bigint,
  published_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (pool, chain_hash)
);
CREATE INDEX part_chain_layers_idx ON part_chain USING GIN (layers);

CREATE TABLE part_cost (                       -- finalised donor cost, keyed by chain
  pool text NOT NULL, chain_hash text NOT NULL,
  seconds double precision,                    -- NULL = unknown, never a silent zero
  samples int NOT NULL DEFAULT 1,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (pool, chain_hash)
);

CREATE TABLE pending_publish (                 -- durable retry state for §5 (GPT F6)
  pool text NOT NULL, chain_hash text NOT NULL,
  entry_numbers bigint[] NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  attempts int NOT NULL DEFAULT 0,
  PRIMARY KEY (pool, chain_hash)
);

CREATE TABLE eviction_cohort (                 -- closed cohort for regret (GPT F14)
  pool text NOT NULL, digest text NOT NULL, gen int NOT NULL,
  evicted_at timestamptz NOT NULL DEFAULT now(),
  refetched_at timestamptz, matured_at timestamptz,
  PRIMARY KEY (pool, digest, gen)
);
```

Object key: `pools/<pool>/blobs/sha256/<hex>/g<gen>`. The generation appears only in the key and the ledger, never in the graph, so a delayed `DELETE` cannot remove a newer generation.

## 2. States and transitions

| From | To | Trigger | Guard |
|---|---|---|---|
| `absent` | `pending` | reserve | none (gen 0 → 1) |
| `failed` | `pending` | reserve, **after** the scanner destroyed the old object | `blob_object_gc` row for the old gen gone |
| `pending` | `pending` (takeover) | reserve | `lease_until < now()`, or owner export dead |
| `pending` | `verified` | verify | `owner_export = $export`, `lease_until >= now()`, `gen = $g` |
| `pending` | `failed` | sweeper | `lease_until < now()` **and** `owner_export = $seen_owner` |
| `verified` | `deleting` | GC mark | under `M`, not live, `last_url_mint_at < now()-G` |
| `verified` | `quarantined` | hash mismatch at gen `$g` | rowcount 1 required before queueing |
| `deleting` | `verified` | inherited-mark revert | §0.1 step 4: digest live in the new epoch, object exists |
| `deleting` | `absent` | object destroyed / 404 | `gen = $g` |
| `failed` | `absent` | object destroyed | via `blob_object_gc` |
| `quarantined` | `deleting` | operator `repair` | under `M` |

`absent` is the only state a fresh generation can be allocated from **by reserve**; `failed` becomes allocatable only after its object is confirmed destroyed (the scanner does this, not the reserve path — §3.2). `gen` is retained after deletion.

### 2.1 Epoch fence on every write

Each statement carries `<epoch>`:

```sql
AND EXISTS (SELECT 1 FROM pool_leader l
             WHERE l.pool = $1 AND l.epoch = $me AND l.lease_until > now()
               AND NOT EXISTS (SELECT 1 FROM pool_state s
                                WHERE s.pool = $1 AND s.pool_failed_at IS NOT NULL))
```

The `pool_failed` clause is what stops an ambiguous operation's successor from proceeding (§0.2). The predicate fenced by `op_inflight` + drain (§0.1), not by this clause alone.

## 3. Reserve (upload path)

```sql
INSERT INTO blob (pool, digest) VALUES ($1, $2) ON CONFLICT (pool, digest) DO NOTHING;
SELECT gen, state, lease_until, owner_export FROM blob
 WHERE pool=$1 AND digest=$2 FOR UPDATE;
```

Branch under that row lock. **Reserve performs no object-store I/O** (GPT F5: a synchronous `DestroyBlob` inside reserve would spend the 5 s upload-continuation budget on a store call).

- **`verified`** → reuse; no URL; mark served (§6).
- **`pending`, owner live** → no URL, no wait; publication is event-driven (§5.2).
- **`pending`, owner export dead** → adopt, then HEAD (below).
- **`pending`, lease expired** → takeover (§3.3).
- **`deleting`, `quarantined`** → decline; return no URL.
- **`absent`** → allocate (§3.5).
- **`failed`** → **decline**, and ensure a `blob_object_gc` row exists for the current gen so the scanner destroys it; the next demand after destruction allocates. This is the corrected `failed` path: R4 said "destroy synchronously then allocate", but the allocation SQL only accepted `absent` (GPT F3, Claude F6).

### 3.3 Takeover and adoption

```sql
-- takeover of an expired lease, same generation
UPDATE blob SET lease_until = now() + interval '10 minutes',
                owner_export = $export, actor_epoch = $me
 WHERE pool=$1 AND digest=$2 AND state='pending' AND lease_until < now() <epoch>;

-- adoption of a dead export's pending row (GPT F4)
UPDATE blob SET lease_until = now() + interval '10 minutes',
                owner_export = $export, actor_epoch = $me
 WHERE pool=$1 AND digest=$2 AND state='pending' AND owner_export = $dead_export <epoch>
 RETURNING gen;
```

Adoption is **required before verify**: verify demands `owner_export = $export`, so a new owner must first take the row, otherwise its HEAD→verify path fails and the blob never publishes (GPT F4). After adoption, HEAD the key: present with the declared size → verify; absent or short → `failed` + queue.

### 3.4 Expiry precedence

Takeover beats expiry-failure. The sweeper may fail a row only when the lease is expired **and** the owner is unchanged since its own read:

```sql
UPDATE blob SET state='failed', reserved_at=now()
 WHERE pool=$1 AND digest=$2 AND gen=$g
   AND state='pending' AND owner_export=$seen_owner AND lease_until < now() <epoch>
 RETURNING gen;
```

Rowcount 0 ⇒ a takeover won; the sweeper must not queue or destroy anything.

### 3.5 Allocation

```sql
UPDATE blob
   SET gen = gen + 1, state = 'pending', size = $size, sha256 = NULL,
       lease_until = now() + interval '10 minutes', owner_export = $export,
       reserved_at = now(), last_url_mint_at = now(),
       verified_at = NULL, actor_epoch = $me
 WHERE pool=$1 AND digest=$2 AND state = 'absent' <epoch>
 RETURNING gen;
```

Rowcount 0 ⇒ re-read and branch again, bounded to 3 iterations.

## 4. Verify (owner-fenced)

```sql
UPDATE blob
   SET state='verified', size=$size, sha256=$sha256, verified_at=now()
 WHERE pool=$1 AND digest=$2 AND gen=$g
   AND state='pending' AND owner_export=$export AND lease_until >= now()
   AND actor_epoch=$me <epoch>
 RETURNING 1;
```

Rowcount 1 or abort the whole verify. `actor_epoch` is the leader epoch and is **not** ownership: both owners run under the same leader, so `owner_export` plus a live lease is the fence (round-5 F6).

## 5. Publish — the only commit point

### 5.1 The operation

```
op_begin("publish")
lock M
  rows := SELECT digest, gen, state FROM blob
           WHERE pool=$1 AND digest = ANY($distinct_chain);
  require every row state='verified'
  SetStoredPart(chain)                       -- in-memory graph write
  INSERT INTO part_chain (pool, chain_hash, layers, bytes) VALUES (...)
                                             ON CONFLICT DO NOTHING
  DELETE FROM pending_publish WHERE pool=$1 AND chain_hash=$h
unlock M
op_end()
```

- **No database transaction around the graph write.** The graph insertion is the commit; a process death leaves no checkpoint, so no part exists on restart and the object is reclaimed after `G`.
- **No DB lock held across `SetStoredPart`**; nothing holding `egraphMu` ever takes a Postgres lock.
- **Publish and mark cannot interleave** — same `M`.
- Digests are deduplicated (`SELECT DISTINCT`); chains may repeat layers.

### 5.2 Retry state is durable (GPT F6)

A chain whose partner digest is still `pending` gets a `pending_publish` row. Publication is retried on: the partner's verify event, every mark cycle, and every scanner pass; the row is deleted only on success. R4 kept this in memory only, so a process restart or a lost in-process notification forgot the chain forever.

A chain that never completes is never published; its digests stay `pending`/`failed` and are reclaimed by lease expiry, the scanner and GC.

## 6. Mint (renew path)

Per digest, one autocommit single-row statement, digests processed in sorted order:

```sql
UPDATE blob SET last_url_mint_at = now()
 WHERE pool=$1 AND digest=$2 AND state='verified' <epoch>
 RETURNING gen;
```

**The chain has one total budget, not a per-row timeout** (GPT F15: twenty rows each waiting 400 ms is 8 s, although no single row hits a 500 ms limit). Policy:

- whole renewal = **1.6 s** for database work, **0.4 s** reserved for signing and reply dispatch, hard engine deadline 2 s;
- each row: `lock_timeout = 100 ms`, `statement_timeout = 250 ms`;
- on any timeout or budget exhaustion: reply `renewed{unavailable:true}` **immediately** with no URL for the remaining digests, never a late reply and never a second reply;
- rows already stamped stay stamped (conservative: keeps bytes longer), which is harmless.

**A zero row is not evidence the blob is missing.** The epoch predicate also returns zero when the lease is a second late; `lock_timeout` returns an error. Neither may trigger a drop:

```
miss := mint(digest)
if miss is timeout/error or lease invalid: reply unavailable   -- no drop
else: st := SELECT state FROM blob WHERE pool,digest          -- positive read
      if st <> 'verified': DropStoredParts(digest)            -- only now
```

Mapping a lease blip to the drop path would drop every part naming a shared base layer — one Postgres hiccup, most of the graph (round-5 F1, Claude F1).

## 7. GC mark

**Gates (all three, under `M`):**
1. `pool_state.restore_complete_epoch` equals the current epoch;
2. no `pool_failed_at`;
3. `|live| >= 0.5 × ckpt_live_blobs` — otherwise alert and skip the mark.

```
op_begin("mark")
lock M
  live := cache.LiveBlobs()
  WITH marked AS (
    UPDATE blob SET state='deleting', actor_epoch=$me
     WHERE pool=$1 AND state='verified'
       AND NOT (digest = ANY($live))
       AND last_url_mint_at < now() - $G        -- G = 2 x GET TTL = 7200 s
       <epoch>
     RETURNING digest, gen, size)
  INSERT INTO blob_object_gc (pool, digest, gen, bytes)
    SELECT $1, digest, gen, size FROM marked
  RETURNING digest, gen
unlock M  -- only after the terminal outcome of this statement is known (§0.2)
op_end()
```

Deletion runs outside `M`, in queue order, but **only for rows whose `queued_at` is newer than the last completed mark in the current epoch**; a queue row inherited from an earlier epoch is revalidated against `LiveBlobs()` (revert pass, §0.1 step 4) before any `DELETE`.

Drain step: `DELETE g<gen>` → success/404 both count → `UPDATE blob SET state='absent', size=NULL, sha256=NULL, reserved_at=NULL, verified_at=NULL WHERE … state IN ('deleting','failed')` → delete the queue row after **two attempts ≥ 20 min apart** (an optimisation; §7.1 is the correctness mechanism).

### 7.1 Normative orphan sweep

| Listed key `…/<hex>/g<n>` | Action |
|---|---|
| `n < row.gen` | delete now (superseded generation) |
| `n = row.gen`, state `absent`\|`failed`, `LastModified` older than PUT-URL TTL + 1 h | delete now (late PUT / expired uploader) |
| no row | delete now (stray object) |
| `n = row.gen`, state `pending`\|`verified`\|`deleting` | leave alone |

Daily, one LIST per 1,000 keys (~$0.005 per run at 1 M objects, listing only). A 2 GB PUT on a slow link can finish long after both attempts (GPT F5/F7), so this sweep — not the retry count — is what bounds orphan bytes.

**Cost, stated:** `LiveBlobs()` is O(parts × layers) under `egraphMu.RLock` while `M` is held, so publishes wait up to scan time. Measure it on the real pool; if > 1 s, move to two-phase mark (snapshot under `M`, re-snapshot, mark under `M`). The `RWMutex` detail matters: a queued writer blocks new readers (Go `sync` docs), so the scan must not run while a publish is queued behind `M`.

## 8. Drop (renew-miss, eviction, restore)

`DropStoredParts(digest)` (`dagql/cache_stored_parts.go:167`) removes **every** part whose chain names that digest, under `egraphMu.Lock`. It is digest-granular: there is no per-part drop primitive in beta.25 (GPT F9, Claude F3).

- All drops under `M`.
- A drop changes no blob state; the next mark recomputes liveness.
- Eviction never deletes bytes directly.
- **Because the actuator is per digest, the eviction candidate unit is a digest, not a part** (`metrics-and-eviction.md` §3.2).

## 9. Corruption

Detected by the 1 % sample (streamed through the endpoint) or by an engine import-digest failure — not a protocol message, so unsampled corruption at rest is undetected until an engine imports it. On detection, under `M`, **for the sampled generation only**, and with the transition's rowcount required before any delete intent (GPT F16):

```sql
UPDATE blob SET state='quarantined', actor_epoch=$me
 WHERE pool=$1 AND digest=$2 AND gen=$g AND state='verified' <epoch>
 RETURNING gen;
-- only when rowcount = 1:
INSERT INTO blob_object_gc (pool,digest,gen,bytes) SELECT $1,$2,$g,size FROM blob WHERE …;
```

Quarantine is immediate (no `G`): a digest known wrong must not be served. Engines holding a URL get 404 and fall back to compute.

## 10. Restore (cold) and handoff

1. `UPDATE pool_leader SET holder=$me, epoch=epoch+1, lease_until=now()+TTL WHERE pool=$1 AND (lease_until < now() OR holder=$me) RETURNING epoch`.
2. Set `pool_state.handoff_from_epoch`, then **drain** (§0.1): wait for `op_inflight` rows of earlier epochs, terminating stragglers past `max_op_seconds`.
3. Load the newest checkpoint. Checkpoints are create-only `ckpt/<pool>/<epoch>-<seq>`; the pointer row is updated with `<epoch>`, so a deposed writer's upload is unreferenced garbage.
4. For every stored part, read its digests' states. If any digest is not `verified`, `DropStoredParts` it **before the first merge, mint or mark**. Then, under `M`, revert inherited `deleting` rows whose digest is live and whose object exists (§0.1 step 4), and set `restore_complete_epoch = $epoch` with `live_blobs` recorded.
5. Rebuild `blob_object_gc` rows for `deleting` rows older than 1 h with no queue row.

## 11. Recovery scanner

Startup and hourly, every step epoch-fenced:
- `blob_object_gc` rows older than 5 min → destroy, transition `deleting|failed → absent`, drop the queue row after the second attempt.
- `blob_object_gc` rows for a `failed` blob → destroy first, **then** the row becomes allocatable (`absent`).
- `blob` rows `deleting` older than 1 h with no queue row → queue them.
- `blob` rows `pending` with `reserved_at` older than 1 h → HEAD the key: matches the declaration → adopt + verify; absent or short → fail + queue. Applies whether or not a demand ever arrives (R4 only handled "object exists").
- `blob` rows `quarantined` → operator repair only; never auto-reused.
- `pending_publish` rows older than 10 min → retry publication once; still unverified after 1 h → delete the row and let the digests fail.
- `eviction_cohort` rows: mark `matured_at` at 24 h; delete rows older than 48 h.

## 12. Quota — charged, projected, retired

**Charged** = Σ `blob.size` over `pending`, `verified`, `deleting`, `quarantined`, `failed` **plus** Σ `blob_object_gc.bytes` for generations whose object has not been confirmed destroyed (GPT F7: R4 dropped retired generations out of the charge as soon as the row returned to `absent`, while the object could still exist). Using the declared size where the actual is unknown over-charges briefly and never under-charges.

**Projected frees** = rows in `deleting` **plus** bytes of `verified` blobs that are absent from the last `LiveBlobs()` snapshot (pre-mark retirement liabilities — Claude F2, GPT F10). Without that term the controller sees the pool unchanged for 2–3 h after an eviction pass and evicts again each tick, down to hot parts only.

**Controller rule:** after an eviction pass, refuse another pass until `last_mark_at > last_eviction_at`; then recompute from durable state. Both timestamps live in `pool_state`, so the rule survives restart.

Watermarks 0.85 ⇒ evict to 0.70; 0.95 ⇒ stop admitting bytes; `min_retention` 24 h by `last_url_mint_at`.

## 13. Invariants

1. Bytes are served only from `verified`.
2. A part exists only if every digest in its chain was `verified` at publish time under `M`.
3. Liveness is recomputed from `LiveBlobs()`, never counted.
4. Deletion requires non-liveness **and** `last_url_mint_at < now()-G`, both evaluated in the current epoch after the handoff drain.
5. A generation is reused only after its object is destroyed (`failed→absent`) or confirmed destroyed (`deleting→absent`).
6. One writer, and authority handoff drains outstanding operations before service (§0.1).
7. An ambiguous operation blocks publication until its terminal outcome is proven (§0.2).
8. Restore serves only verified digests, and no merge, mint or mark runs before `restore_complete_epoch` is set.
9. Every mutation is an `op_inflight` operation bounded by `statement_timeout`.

## 14. What this design cannot do

- **No per-part drop** — digest-granular actuator; eviction is expressed per digest.
- **No unsampled corruption detection** at rest.
- **No warm failover** — cold restore, linear in graph size (#15013), preceded by a drain.
- **No guarantee that `LiveBlobs()` stays cheap** — measure; two-phase mark if > 1 s.
- **No exactly-once export** — duplicate uploads are harmless by single-object atomicity.
- **No proof yet that the handoff protocol works under a real paused leader** — it is specified, not tested; §0.1 and §0.2 need a promotion/paused-leader chaos test before implementation is declared safe.
