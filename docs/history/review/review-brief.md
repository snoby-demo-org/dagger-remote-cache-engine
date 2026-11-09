> **Historical design/review record, not current implementation status.** See [current design](../../design.md) and the repository verification report. Some proposals were superseded; private development paths/addresses are replaced with documentation placeholders.

# Review brief: dcache design + the only code that exists

You are the independent reviewer. Your job is to find what is wrong, not to praise.

## Context you must hold

- Live system today: Dagger engine **v0.21.9**, Helm DaemonSet, 3 pods on 3 persistent k3s nodes, node-local hostPath cache (~30 GB/node), 27 days up. No shared cache exists; v0.21.x cannot join the beta remote-cache protocol (the S3 export path was removed upstream).
- The proposal under review moves to **v1.0.0-beta.25** engines on ephemeral nodes + a self-hosted Go service `dcache` + an external S3-compatible object store. **No `dcache` code exists.** It is a paper design. Do not review imagined code as if it existed; review the design's internal consistency and the claims it makes.
- Ground truth for the protocol is `recon-protocol.md` (cited as RP §n) — source-pinned to dagger commit `7e9eaac` / `v1.0.0-beta.25`. If a design claim contradicts RP, RP wins and the design is wrong.
- Measured facts (design cluster, in-cluster pod → MinIO): GET 4 MiB TTFB p50 4.29 ms / p99 6.90 ms; GET 1 MiB range p50 4.01 ms; PUT 4 MiB TTFB p50 84.6 ms / p99 143 ms; 4 MiB GET total p50 42.9 ms (≈93 MB/s per connection).

## Review targets

| Path | What it is |
|---|---|
| `../decision.md` | decision brief, invariants, phases, week-version fallback |
| `../architecture.md` | full design: components, indexes, flows, failure table, red team |
| `../metrics-and-eviction.md` | Prometheus catalogue, hit/miss definitions, eviction policy, safe-eject fence |
| `../latency-and-azure.md` | measured latency, Azure variant, breaker thresholds |
| `../s3_latency_probe.py` | the only executable code — SigV4 probe used for the numbers above |
| `live-helmrelease.yaml` | the live v0.21.9 deployment (Flux-managed) |
| `live-networkpolicy.yaml` | live netpol |

## Adversarial questions — answer each with evidence

**Protocol conformance**
1. Does the design ever assume a pull/on-miss lookup, a REST endpoint, multipart, or a `.zip`-style cache key that RP says does not exist? Cite RP §.
2. `export{roots}` takes **engine entry numbers**. Does the design actually obtain them (OTLP only)? What happens on the first session of a brand-new engine with no telemetry history — can it ever seed anything?
3. The engine has 4 export slots and 8 concurrent PUTs; renewal mailbox is 64 exchanges with a 2 s deadline. Are the service's scheduling numbers (5 s upload watchdog, 48 MiB merge cap, "N roots") internally consistent with those limits and with RP's 64 MiB frame cap?
4. RP says an accepted offer is not proof of completion, and `executionStarted` means not admitted. Does any flow in the design treat `accepted` as a hit (that would double-count hits and mislead the value ratio)?

**Invariants (try to break I1–I9)**
5. Find a hole in I1: can a part become published while one blob in its ordered chain is still `pending` or `failed` under some interleaving?
6. Merge-before-bytes: after `merge`, other engines get metadata with no stored part. Is that always harmless, or can an engine get stuck on a description pointing at a blob that is later deleted?
7. GC vs publish race: the sweep snapshot, `last_ref_at`, and `last_url_mint_at` are three different times. Construct an interleaving where the sweeper deletes bytes for which a GET URL is still in an engine's hand.
8. Is `G ≥ GET TTL + 30 s + skew` actually sufficient when the engine may retry via `renew` after a 403 (renewal mints a *fresh* URL, resetting nothing in the design's `last_url_mint_at` model)? Does the design's fence account for renewal-minted URLs at all?

**Metrics**
9. Is `value ratio = saved/(saved+spent)` computable from the attributes the engine actually emits, or does it need the span duration of an *executed* op that the service never requested? Check units (ns vs s) in `cache.offer.duration_ns`.
10. Label cardinality: does any metric label carry a digest, repo path, or call name?
11. Does the hit/miss definition double-count `joined` or `uncached` anywhere, and does the "addressable ceiling" formula double-count spans?

**Eviction**
12. Shared blobs: a digest referenced by 5 parts. Does the score model evict it correctly, and does the regret counter attribute a refetch to the right reason?
13. Can admission control (≥2 sessions or ≥60 s recompute) starve exactly the workloads the cache is meant to help — e.g. a 45 s monorepo build that runs once a day?
14. Is "mark-only for 2 weeks" actually safe, given the ledger state machine has `evicting` as a terminal-looking state? What reverts a marked blob?

**Latency / Azure**
15. The 5% rule ("if p99 GET > 5% of median recompute, the cache stops paying") — is it statistically meaningful, and does the measured PUT cost (84 ms p50) invalidate the claim that "round-trip latency is negligible"?
16. Azure: is MinIO on a dedicated AKS node pool actually defensible, or is running stateful storage on AKS the anti-pattern Azure docs warn about? What is the strongest counter-argument, and what would change your mind?

**Code**
17. `s3_latency_probe.py`: list every real bug. Include the HEAD path (it raised `ResponseNotReady` in the live run — explain exactly why) and any SigV4, keep-alive, range, or measurement-bias defect that would make the reported numbers wrong.
18. `live-helmrelease.yaml`: what must change before a beta.25 ephemeral engine can run — name the exact fields, and flag anything in it that would break the design (e.g. hostPath persistence, grace period, node pinning, missing metrics port, telemetry egress).

## Output contract

- Write the full review to `<DESIGN_WORKSPACE>/review/claude-review.md`.
- Structure: **Verdict** (one of PROCEED / PROCEED-WITH-CHANGES / STOP, one sentence), then per section a PASS/FAIL/UNVERIFIED line, then numbered findings. Each finding: `[SEVERITY P0|P1|P2] file:line or RP § — claim — why it is wrong — exact fix`.
- Then **Top 5 changes before any code**, and **what you would cut from the design**.
- Then **Verified-correct list** (things you checked and that hold), so the author gets credit where due.
- Chat reply: max 200 words. Give the verdict, the three worst findings by number, and any question you need the human to answer. Do not quote code.
