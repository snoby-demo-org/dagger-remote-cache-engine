#!/usr/bin/env python3
# HISTORICAL DIAGNOSTIC ONLY: not a supported acceptance test. See docs/history/README.md.
"""Gate A probe: what does the live engine actually emit, and how much of it repeats?

Tempo can stream multiple JSON documents in one response, so decode with raw_decode
in a loop rather than json.loads on the whole body.
"""
import json, sys, urllib.parse, urllib.request, collections, time

TEMPO = "http://192.0.2.11:3200"

def fetch(path):
    with urllib.request.urlopen(TEMPO + path, timeout=60) as r:
        body = r.read().decode("utf-8", "replace")
    dec = json.JSONDecoder(strict=False)
    out, i = [], 0
    while i < len(body):
        while i < len(body) and body[i] in " \r\n\t":
            i += 1
        if i >= len(body):
            break
        try:
            obj, j = dec.raw_decode(body, i)
        except Exception:
            break
        out.append(obj)
        i = j
    return out

def search(expr, days, limit=1000):
    now = int(time.time())
    q = urllib.parse.quote(expr)
    docs = fetch(f"/api/search?q={q}&start={now-int(days*86400)}&end={now}&limit={limit}")
    traces = []
    for d in docs:
        traces.extend(d.get("traces", []))
    return traces

days = float(sys.argv[1]) if len(sys.argv) > 1 else 7.0
tr = search('{resource.service.name="dagger-engine"}', days)
names = collections.Counter(t.get("rootTraceName") or "?" for t in tr)
durs = sorted((t.get("durationMs") or 0) for t in tr)
print(f"{days}d: traces={len(tr)} max={durs[-1] if durs else 0}ms p50={durs[len(durs)//2] if durs else 0}ms p95={durs[int(len(durs)*0.95)] if durs else 0}ms")
print("root names:")
for n, c in names.most_common(10):
    print(f"  {c:5d}  {n[:70]!r}")

# how many traces have any span carrying a dag attribute at all
with_dag = 0
for t in tr[:80]:
    try:
        docs = fetch(f"/api/traces/{t['traceID']}")
    except Exception:
        continue
    hit = False
    for d in docs:
        for b in d.get("batches", []):
            for ss in b.get("scopeSpans", []):
                for s in ss.get("spans", []):
                    for a in s.get("attributes", []):
                        if a.get("key", "").startswith("dagger.io/dag"):
                            hit = True
    with_dag += 1 if hit else 0
print(f"traces with any dagger.io/dag* attribute (of first 80 sampled): {with_dag}")
