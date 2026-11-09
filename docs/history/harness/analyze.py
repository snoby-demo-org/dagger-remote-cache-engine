#!/usr/bin/env python3
# HISTORICAL DIAGNOSTIC ONLY: not a supported acceptance test. See docs/history/README.md.
"""Gate A analyser: read OTLP collector file-exporter output and answer three questions.

  1. Does the beta.25 engine actually emit the dagger.io/cache.* contract?
  2. What does cache.offer.part really carry (PartAddress, or a content identity)?
  3. What is the repeat rate of cache.pairing_digest across distinct traces
     (the opportunity number Gate A needs)?

Input: the collector's /out/spans.json, which is one JSON object per line
(resourceSpans document) or a JSON array of them.
"""
import json, sys, collections, glob, os, statistics

CACHE_PREFIX = "dagger.io/cache"

def iter_spans(path):
    with open(path, "r", encoding="utf-8", errors="replace") as f:
        text = f.read()
    docs = []
    dec = json.JSONDecoder(strict=False)
    i = 0
    while i < len(text):
        while i < len(text) and text[i] in " \r\n\t":
            i += 1
        if i >= len(text):
            break
        try:
            obj, j = dec.raw_decode(text, i)
        except Exception:
            break
        docs.append(obj)
        i = j
    for d in docs:
        if isinstance(d, list):
            for x in d:
                yield from spans_of(x)
        else:
            yield from spans_of(d)

def spans_of(doc):
    for rs in doc.get("resourceSpans", []):
        res = {}
        for a in rs.get("resource", {}).get("attributes", []):
            res[a["key"]] = a.get("value", {})
        for ss in rs.get("scopeSpans", []):
            for s in ss.get("spans", []):
                attrs = {}
                for a in s.get("attributes", []):
                    attrs[a["key"]] = a.get("value", {})
                yield {
                    "name": s.get("name"),
                    "trace": s.get("traceId"),
                    "start": int(s.get("startTimeUnixNano", "0")),
                    "dur_ms": int(s.get("durationNanos", "0")) / 1e6,
                    "attrs": attrs,
                    "resource": res,
                }

def val(v):
    for k in ("stringValue", "intValue", "doubleValue", "boolValue"):
        if k in v:
            return v[k]
    if "arrayValue" in v:
        return [val(x) for x in v["arrayValue"].get("values", [])]
    return None

paths = sys.argv[1:] or sorted(glob.glob(os.path.join(os.path.dirname(__file__), "out", "*.json")))
spans = []
for p in paths:
    spans.extend(iter_spans(p))
print(f"files={len(paths)} spans={len(spans)}")

cache_keys = collections.Counter()
for s in spans:
    for k in s["attrs"]:
        if k.startswith(CACHE_PREFIX) or k.startswith("cache."):
            cache_keys[k] += 1

print(f"\n1. cache contract attributes seen: {len(cache_keys)} distinct")
for k, c in cache_keys.most_common(30):
    print(f"   {c:6d}  {k}")

outcomes = collections.Counter()
for s in spans:
    for k in ("dagger.io/cache.outcome", "cache.outcome"):
        if k in s["attrs"]:
            outcomes[val(s["attrs"][k])] += 1
print(f"\n   outcomes: {dict(outcomes)}")

pa = collections.Counter()
for s in spans:
    if s["name"] and "offer" in s["name"].lower():
        for k, v in s["attrs"].items():
            if "part" in k:
                pa[k] += 1
                if pa[k] <= 3:
                    print(f"   offer-part attr sample: {k} = {val(v)!r}")
print(f"\n2. part identity attributes on offer spans: {dict(pa)}")

# 3. repeat rate on pairing_digest across distinct traces
for keyname in ("dagger.io/cache.pairing_digest", "cache.pairing_digest"):
    groups = collections.defaultdict(list)
    for s in spans:
        v = s["attrs"].get(keyname)
        if v is None:
            continue
        g = val(v)
        if g:
            groups[g].append(s)
    if not groups:
        continue
    total = sum(x["dur_ms"] for occ in groups.values() for x in occ)
    repeat = 0.0
    occ_n = 0
    for g, occ in groups.items():
        occ.sort(key=lambda x: x["start"])
        seen = set()
        for x in occ:
            if seen and x["trace"] not in seen:
                repeat += x["dur_ms"]
                occ_n += 1
            seen.add(x["trace"])
    print(f"\n3. pairing_digest groups={len(groups)} spans={sum(len(o) for o in groups.values())}")
    print(f"   sampled span time {total/1000:.1f} s; cross-trace repeat {repeat/1000:.1f} s ({occ_n} spans)")
    if total:
        print(f"   cross-session repeat share: {100*repeat/total:.1f} %  <-- upper bound on addressable work")
    break

print("\nVERDICT:")
print(f"   contract present: {'YES' if cache_keys else 'NO'}")
if not cache_keys:
    print("   -> this build does not emit cache.* attributes; Gate A needs a different engine build")
