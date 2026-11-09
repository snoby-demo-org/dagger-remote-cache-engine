> **Historical design/review record, not current implementation status.** See [current design](../../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Blob ledger — R4

Normative. Supersedes R2/R3. Round-4 finding 1 (P0) and 2–8 forced a structural change; see `revision-r4.md` §"Adopted Q8".

## 0. Authority model — one writer, one mutex, no cross-store commit

```
                    ┌──────────────────────────────────────────────┐
engine ──WS──► dcache (single fenced writer per pool, I6)            │
                    │  mutex M  ← serialises publish, GC mark, restore
                    │  dagql.Cache (in-process graph, SQLite working copy)
                    └───────────────┬──────────────────────────────┘
                                    │ durable state (advisory-but-authoritative for bytes)
                                    ▼
                              Postgres  `blob`
```

- **The graph is the source of truth for liveness.** A blob is live iff some stored part names it. That is exactly `dagql.Cache.LiveBlobs()` (`dagql/cache_stored_queries.go:162`, takes `egraphMu.RLock`, returns sorted digests). It is recomputed at GC time; nothing maintains a counter.
- **Postgres is the source of truth for bytes.** `blob.state` decides whether an object may be served, deleted or overwritten, and `blob.last_url_mint_at` decides when deletion is safe.
- **One in-process mutex `M` serialises publish, GC mark and restore.** No Postgres locks are held across `SetStoredPart`. There is no expiring publish pin, no `graph_refs`, no `blob_head` mirror: R3's publish design created a two-store commit problem with no compensation path, and `graph_refs` had no observable decrement (round-4 F1/F2/F3).
- **Why one writer is enough:** I6 (pool secret + single fenced writer). A deposed leader's writes fail the epoch predicate (§2.1); its graph is not authoritative and it must not checkpoint (§10). Engines cannot write metadata — the protocol has no metadata message from the engine side.
- **Conservatism rule.** Every race in this design resolves toward *keeping bytes longer*, never toward deleting bytes that might be served. Where a rule could go either way, it defers.

## 1. Tables

```sql
CREATE TABLE blob (
  pool             text NOT NULL,
  digest           text NOT NULL,              -- 'sha256:<hex>'
  gen              int  NOT NULL DEFAULT 0,    -- monotonic per (pool,digest)
  state            text NOT NULL DEFAULT 'absent',
  size             bigint,                     -- declared at reserve, actual at verify
  sha256           text,                       -- set at verify
  lease_until      timestamptz,
  owner_export     text,                       -- the export that holds the pending lease
  last_url_mint_at timestamptz NOT NULL DEFAULT now(),
  verified_at      timestamptz,
  actor_epoch      bigint,
  PRIMARY KEY (pool, digest),
  CHECK (state IN ('absent','pending','verified','deleting','failed','quarantined'))
);

CREATE TABLE blob_object_gc (                  -- objects to destroy, drained idempotently
  pool text NOT NULL, digest text NOT NULL, gen int NOT NULL,
  queued_at timestamptz NOT NULL DEFAULT now(),
  attempts int NOT NULL DEFAULT 0,
  PRIMARY KEY (pool, digest, gen)
);

CREATE TABLE pool_leader (
  pool text PRIMARY KEY, holder text, epoch bigint, lease_until timestamptz
);

CREATE TABLE part_cost (                       -- finalised donor cost, per part
  pool text NOT NULL, part_digest text NOT NULL,
  seconds double precision NOT NULL, samples int NOT NULL DEFAULT 1,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (pool, part_digest)
);
```

Object key: `pools/<pool>/blobs/sha256/<hex>/g<gen>`. The generation is in the key and **only** the ledger, never in the graph, so a delayed `DELETE` of `g1` cannot remove `g2`. `last_url_mint_at` is `NOT NULL DEFAULT now()` so a blob verified but never served is still deletable (round-2 F; it must not be immortal).

## 2. States and transitions

| From | To | Trigger | Guard |
|---|---|---|---|
| `absent` | `pending` | reserve | none (gen 0 → 1) |
| `failed` | `pending` | reserve | **object of current gen destroyed first** (§3.2) |
| `pending` | `pending` (takeover) | reserve | `lease_until < now()`; same gen, same key |
| `pending` | `verified` | verify | `actor_epoch = $me`, `gen = $g` |
| `pending` | `failed` | upload error, lease expiry | `gen = $g` |
| `verified` | `deleting` | GC mark | under `M`, digest not in `LiveBlobs()`, `last_url_mint_at < now()-G` |
| `verified` | `quarantined` | hash mismatch | under `M` |
| `deleting` | `absent` | object destroyed (or 404) | `gen = $g` |
| `quarantined` | `deleting` | operator `repair` | under `M` |
| `failed` | `absent` | object destroyed | via `blob_object_gc`; `gen` retained |

`deleted` is not a state: a destroyed object's row returns to `absent` with its `gen` retained. Legal next states are exhaustive; every other transition is refused with rowcount 0.

### 2.1 Epoch fence on every write

Every statement below ends with the same predicate, written `<epoch>`:

```sql
AND EXISTS (SELECT 1 FROM pool_leader l
             WHERE l.pool = $1 AND l.epoch = $me AND l.lease_until > now())
```

A deposed writer's statements affect 0 rows. This replaces R3's "durable deadline revokes authority" (which was wrong — a deadline does not cancel anything) and its holder-only check (round-4 F20: pod names repeat across epochs, so compare `epoch`, never `holder`).

## 3. Reserve (upload path)

```sql
BEGIN;
INSERT INTO blob (pool, digest) VALUES ($1, $2) ON CONFLICT (pool, digest) DO NOTHING;
SELECT gen, state, lease_until FROM blob WHERE pool=$1 AND digest=$2 FOR UPDATE;
```

Then branch **in the service, under that row lock**:

### 3.1 `verified` → reuse
Commit. Return no PUT URL; the digest is already servable at its current generation; mark it served (§6).

### 3.2 `failed` → destroy, then allocate
The transition is legal only after the object of the current generation is gone (invariant 5). Call `DestroyBlob(pool, digest, gen)` synchronously:
- success or 404 → continue;
- any other error → rollback, leave `failed`, return no URL, retry on the next demand. Never allocate over a possibly-live object.

### 3.3 `pending` → takeover or decline
- `lease_until >= now()` **and the owner's export is live** → return no URL and no wait. Publication is triggered later by the verify event (§5.2), not by a blocked merge path (round-4 F30: any fixed waiter budget loses to a 26 s partner upload).
- `lease_until >= now()` **and the owner's export is dead** (WS closed, `EndRemoteProcess`, or no event for that export) → HEAD the key immediately: object present with the declared size → verify (§4) as the new owner and continue; absent or short → `failed` and queue the object. A dead owner must not suppress a digest for the rest of its 10-minute lease (round-4 GPT F23, Claude F14).
- `lease_until < now()` → take over the *same generation*:
  ```sql
  UPDATE blob SET lease_until = now() + interval '10 minutes', owner_export = $export, actor_epoch = $me
   WHERE pool=$1 AND digest=$2 AND state='pending' AND lease_until < now() <epoch>;
  ```
  Both writers PUT identical bytes to the same key; single-object atomicity makes the last writer's object complete (I3). The later verify sees `state='verified'` with `gen=$g` and is refused by the `state='pending'` guard — the object is already verified, no harm.

### 3.4 `deleting`, `quarantined` → decline
Return no URL. GC finishes; the next demand re-reserves. A `quarantined` digest needs `dcache-admin repair` — it is never auto-reused, because the mismatch reason is unknown.

### 3.5 `absent` → allocate a new generation

```sql
UPDATE blob
   SET gen = gen + 1, state = 'pending', size = $size, sha256 = NULL,
       lease_until = now() + interval '10 minutes', owner_export = $export,
       last_url_mint_at = now(), verified_at = NULL, actor_epoch = $me
 WHERE pool=$1 AND digest=$2 AND state = 'absent' <epoch>
 RETURNING gen;
```

Rowcount 0 → another path moved it; re-read and branch again, bounded to 3 iterations.

## 4. Verify (owner-fenced)

```sql
UPDATE blob
   SET state='verified', size=$size, sha256=$sha256, verified_at=now()
 WHERE pool=$1 AND digest=$2 AND gen=$g
   AND state='pending' AND actor_epoch=$me <epoch>
 RETURNING 1;
```

Rowcount 1 or abort the whole verify (no partial verification). A verify whose lease expired is refused; the sweeper has already failed it or another owner has taken over.

## 5. Publish — the only commit point

### 5.1 The operation

```
lock M
  rows := SELECT digest, gen, state FROM blob
           WHERE pool=$1 AND digest = ANY($distinct_chain);
  require every row state = 'verified'
  SetStoredPart(chain)                       -- in-memory graph write
unlock M
```

- **No database transaction.** The graph insertion is the commit. If the process dies immediately after, no checkpoint contains the part: on restart it does not exist, the object stays `verified`, and GC reclaims it after `G`. There is nothing to roll back and no orphan (round-4 F2).
- **No DB lock held across the graph write.** `M` is in-process; `SetStoredPart` takes `egraphMu` internally, and nothing that holds `egraphMu` ever takes a Postgres lock. The undetectable cross-resource deadlock of R3 is gone (round-4 F3).
- **Publish and GC mark cannot interleave** because both take `M`, so the "all digests verified" read and the graph write are one atomic step with respect to deletion.
- **The publish check is a state read, not a lock.** Because GC mark runs only under `M`, and `verified→deleting` is the only transition publish cares about, a state read under `M` is sufficient. `pending→verified` transitions outside `M` only ever *enable* publication.
- Digests are deduplicated before the check (`SELECT DISTINCT`), and the graph insert needs distinct digests only (round-4 F31: `|chain|` is wrong; the chain may repeat layers).

### 5.2 Publication is event-driven, not awaited

Publication is attempted on `uploaded{done}` for an export **and** on every successful verify (§4) for a digest that some chain is waiting on:
- the service keeps an in-memory `waiting_chains` index (chain digest-set → entry numbers) for chains whose uploads completed but whose partner digests are still `pending`;
- each verify that completes a digest re-checks the chains whose sets contain it, publishing any that are now all-verified.

A chain that never completes is simply never published; its digests stay `pending`/`failed` and are reclaimed by lease expiry + GC. No merge reply waits on another export.

## 6. Mint (renew path)

Per digest, **one autocommit statement, digests processed in sorted order** so concurrent mints never hold two rows at once:

```sql
UPDATE blob SET last_url_mint_at = now()
 WHERE pool=$1 AND digest=$2 AND state='verified' <epoch>
 RETURNING gen;
```

`rowcount 0` → `renew_unavailable` and no URL. URL is built from the returned `gen`. A chain of 20 digests is 20 statements ≈ 8–20 ms, inside the 2 s renewal deadline; `lock_timeout='500ms'` on this statement guarantees a mint never queues behind anything long. No `ORDER BY` inside a multi-row `UPDATE` is relied on — the ordering is the statement sequence itself (round-4 F4).

## 7. GC mark

```
lock M
  live := cache.LiveBlobs()                     -- egraphMu.RLock, in-process scan
  WITH marked AS (
    UPDATE blob SET state='deleting', actor_epoch=$me
     WHERE pool=$1 AND state='verified'
       AND NOT (digest = ANY($live))
       AND last_url_mint_at < now() - $G        -- G = 2 x GET TTL = 7200 s
       <epoch>
     RETURNING digest, gen)
  INSERT INTO blob_object_gc (pool, digest, gen)
    SELECT $1, digest, gen FROM marked
  RETURNING digest, gen;
unlock M
```

Then, outside `M`, drain `blob_object_gc` in queue order:
1. `DELETE pool/blobs/sha256/<hex>/g<gen>` — success or 404 both count as destroyed;
2. `UPDATE blob SET state='absent', size=NULL, sha256=NULL WHERE pool=$1 AND digest=$2 AND gen=$g AND state='deleting' <epoch>`, increment `attempts`;
3. on a store error: leave the queue row; retry next cycle.

**Two attempts, 20 minutes apart.** A queue row is only dropped after a second successful (or 404) delete at least 20 minutes after the first, because a late PUT from a lease-expired writer can complete *after* the first delete and recreate the object at the old-generation key (round-4 GPT F8). Two attempts cover the 10-minute lease plus its tail; a third only if the pool's uploads are slower than that, measured.

**What this buys:** liveness is a snapshot of the actual graph, so a part that dagql pruned, that eviction dropped via `DropStoredParts`, or that restore did not recreate is reclaimed automatically. No counter to drift, no reconciliation to quarantine on, nothing to repair (round-4 F1/F7).

**Cost, stated:** `LiveBlobs()` is O(parts × layers) under `egraphMu.RLock` and it runs hourly while holding `M`, so publishes wait up to the scan time. For a pool with 50k parts this is milliseconds-to-tens-of-milliseconds; it must be measured on the real pool size before the first production GC window, and if the scan exceeds 1 s the GC moves to a two-phase mark (snapshot under `M`, mark under `M` after re-snapshot). It is the same graph lock `SetStoredPart` already takes.

**Deposed leader:** its `live` snapshot is its own stale graph and its mark statement affects 0 rows because of `<epoch>`. Its in-memory graph must never be checkpointed (§10).

## 8. Drop (renew-miss, eviction, quarantine)

`DropStoredParts(digest)` (`dagql/cache_stored_parts.go:167`) removes **every** stored part whose chain names that digest, under `egraphMu.Lock`. It is digest-granular and coarse: dropping one digest may drop unrelated parts. That is accepted, because:
- dropping a part is always safe (it recomputes);
- the alternative — a per-part drop API — does not exist in beta.25, and maintaining a part→generation counter in the service is exactly the `graph_refs` trap.

Rules:
- All drops happen **under `M`** so no publish observes a half-dropped state.
- A drop does **not** change any blob's state, size, or `last_url_mint_at`. The next GC mark recomputes liveness, so dropped-only blobs become deletable after `G`.
- Eviction never deletes bytes directly: it drops parts, then GC reclaims. One deletion path only (round-4 F9).

## 9. Corruption

Detected only by the §5.1 sample (1 % of blobs, streamed through the endpoint) or by an engine import-digest failure report — which is not a protocol message, so unsampled corruption at rest is **undetected until an engine imports it**. On detection, under `M`:

```sql
UPDATE blob SET state='quarantined', actor_epoch=$me
 WHERE pool=$1 AND digest=$2 AND state='verified' <epoch>;
INSERT INTO blob_object_gc (pool,digest,gen) VALUES ($1,$2,$g) ON CONFLICT DO NOTHING;
```

Quarantine is immediate and does not wait for `G`: a digest known to be wrong must not be served again. Engines holding a live URL get a 404 and fall back to compute (RP §7.2).

## 10. Restore (cold)

1. Take the lease: `UPDATE pool_leader SET holder=$me, epoch=epoch+1, lease_until=now()+TTL WHERE pool=$1 AND (lease_until < now() OR holder=$me) RETURNING epoch`. Half-TTL self-termination (`architecture.md` §11) unchanged.
2. Load the newest checkpoint. **Only this epoch may upload a checkpoint.** Checkpoint uploads are create-only objects `ckpt/<pool>/<epoch>-<seq>`; the pointer row is updated with `<epoch>`, so a deposed writer's upload is unreferenced garbage that retention deletes.
3. For every stored part, read its digests' states (one query, `ANY($digests)`). If any digest is not `verified`, call `DropStoredParts(digest)` for it **before serving any merge** (round-1 F14 / round-4 F7). No gen is involved; no reconciliation, nothing quarantined for accounting drift; the independent digest of a part is not representable in the graph and is not needed.
4. Rebuild `blob_object_gc` for rows left `deleting` older than 1 h (§11).

## 11. Recovery scanner

Startup and hourly:
- `blob_object_gc` rows older than 5 min → destroy + transition (§7 step 2–3).
- `blob` rows in `deleting` older than 1 h with no queue row → queue them (an interrupted mark).
- `blob` rows in `pending` older than 1 h whose object exists → HEAD; if size matches the declaration, verify (§4) and continue; else fail (§2) and queue the object.
- `blob` rows in `failed` with a queued object → destroy (a late PUT completion).

Every scan is fenced by `<epoch>` and idempotent: destroying an already-destroyed object is a 404, and `state='deleting'` guards double transitions.

## 12. Quota

Charged bytes = `SUM(size)` over rows in `pending`, `verified`, `deleting`, `quarantined` and `failed`-with-queued-object, using the declared size where the actual is unknown. This over-charges briefly and never under-charges; there is no `pending_free` projection table (round-4 F29: it duplicated `deleting`), and no "new references cancel a projection" rule (impossible after a mark, and no longer needed).

Watermarks 0.85 ⇒ evict to 0.70 by dropping parts (never bytes); 0.95 ⇒ stop admitting new bytes (`urls:{}`); `min_retention` 24 h respected by `last_url_mint_at`.

## 13. Invariants

1. **Bytes are served only from `verified`.** Mint and publish both read `state='verified'`.
2. **A part exists only if every digest in its chain was `verified` at publish time under `M`.**
3. **Liveness is recomputed, never counted.** `LiveBlobs()` at mark time.
4. **Deletion requires both non-liveness and `last_url_mint_at < now()-G`.** Either alone is insufficient.
5. **A generation is reused only after its object is gone** (`failed→pending` and `deleting→absent`).
6. **One writer.** Every statement carries an epoch predicate; a stale writer changes nothing.
7. **Restore serves only verified digests** and drops parts that fail that test, before the first merge.

## 14. What this design cannot do

- **No per-part drop.** `DropStoredParts` is digest-granular; precision is lost, correctness is not.
- **No unsampled corruption detection** at rest.
- **No warm failover.** Restore is cold and linear in graph size (#15013).
- **No protection against a slow `LiveBlobs()`** beyond the stated measurement gate; if the pool is huge, GC moves to two-phase mark.
- **No exactly-once export.** Duplicate uploads are accepted; single-object atomicity makes them harmless.
