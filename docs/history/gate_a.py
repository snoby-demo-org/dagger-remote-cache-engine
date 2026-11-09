#!/usr/bin/env python3
# HISTORICAL DIAGNOSTIC ONLY: not a supported acceptance test. See docs/history/README.md.
"""Gate A: addressable-cache opportunity from existing Tempo telemetry.

The live engine (v0.21.9) emits no dagger.io/cache.* attributes, so this measures
the weaker but still real signal: repeated (call, result digest) pairs across
DISTINCT traces, i.e. work the same pool performed more than once.

Caveats baked into the output, not hidden:
  - repeats may already be served by the node-local warm cache (27 days up), so this
    is an UPPER bound on addressable remote-cache work, not a prediction.
  - no cache.outcome means hits and misses are indistinguishable here.
  - span duration is the call's duration, not the part's recompute cost.
"""
import json, sys, urllib.parse, urllib.request, collections, time

TEMPO = "http://192.0.2.11:3200"
DAYS = float(sys.argv[1]) if len(sys.argv) > 1 else 7.0
LIMIT = int(sys.argv[2]) if len(sys.argv) > 2 else 200

def get(url, tries=3):
    for i in range(tries):
        try:
            with urllib.request.urlopen(url, timeout=30) as r:
                return json.loads(r.read().decode())
        except Exception as e:
            if i == tries - 1:
                raise
            time.sleep(1.5)

now = int(time.time())
start = now - int(DAYS * 86400)
q = urllib.parse.quote('{resource.service.name="dagger-engine"}')
search = get(f"{TEMPO}/api/search?q={q}&start={start}&end={now}&limit={LIMIT}")
traces = search.get("traces", [])
print(f"traces listed: {len(traces)} over {DAYS}d (limit {LIMIT})")

spans = []          # (traceID, key, dur_ms, ts)
fails = 0
for t in traces:
    tid = t["traceID"]
    try:
        d = get(f"{TEMPO}/api/traces/{tid}")
    except Exception:
        fails += 1
        continue
    if not d:
        fails += 1
        continue
    for b in d.get("batches", []):
        for ss in b.get("scopeSpans", []):
            for s in ss.get("spans", []):
                at = {a["key"]: a.get("value", {}) for a in s.get("attributes", [])}
                dig = at.get("dagger.io/dag.digest", {}).get("stringValue")
                call = at.get("dagger.io/dag.call", {}).get("stringValue")
                if not dig or not call:
                    continue
                dur = int(s.get("durationNanos", "0")) / 1e6
                ts = int(s.get("startTimeUnixNano", "0")) / 1e9
                spans.append((tid, (call, dig), dur, ts))

print(f"resolved traces: {len(traces)-fails}, fetch failures: {fails}")
print(f"spans with dag.digest+call: {len(spans)}")

total_ms = sum(s[2] for s in spans)
by_key = collections.defaultdict(list)
for tid, key, dur, ts in spans:
    by_key[key].append((tid, dur, ts))

repeat_ms = 0.0
repeat_occ = 0
cross_trace_repeat_ms = 0.0
cross_trace_occ = 0
for key, occ in by_key.items():
    occ.sort(key=lambda x: x[2])
    seen_traces = set()
    for tid, dur, ts in occ:
        if len(seen_traces) > 0 and tid not in seen_traces:
            cross_trace_occ += 1
            cross_trace_repeat_ms += dur
        elif len(seen_traces) > 0:
            repeat_occ += 1
            repeat_ms += dur
        seen_traces.add(tid)

keys = len(by_key)
dup_keys = sum(1 for k, v in by_key.items() if len(v) > 1)
print(f"distinct (call,digest) keys: {keys}; keys with >1 occurrence: {dup_keys}")
print(f"total span time sampled: {total_ms/1000:.1f} s")
print(f"repeat time, same trace:  {repeat_ms/1000:.1f} s ({repeat_occ} spans)")
print(f"repeat time, other trace: {cross_trace_repeat_ms/1000:.1f} s ({cross_trace_occ} spans)")
if total_ms:
    print(f"cross-session repeat share of sampled span time: {100*cross_trace_repeat_ms/total_ms:.1f} %")
print()
print("TOP KEYS by total sampled time (call truncated):")
agg = sorted(by_key.items(), key=lambda kv: -sum(x[1] for x in kv[1]))[:8]
for (call, dig), occ in agg:
    print(f"  {sum(x[1] for x in occ)/1000:7.1f}s  n={len(occ):3d} traces={len({x[0] for x in occ}):3d}  {call[:60]}  {dig[:14]}")
