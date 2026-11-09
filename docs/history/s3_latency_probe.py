#!/usr/bin/env python3
# HISTORICAL DIAGNOSTIC ONLY: not a supported acceptance test. See docs/history/README.md.
"""S3 probe v3 — presigned-URL latency with integrity ASSERTIONS.

Changes from v2 (round-2 finding 18):
  * every GET/PUT/HEAD status and size is asserted, not printed
  * HEAD size checked; open-ended range asserts exact Content-Range AND byte count
  * expiry asserted (403); bucket reachability asserted
  * 8 concurrent PUTs must ALL succeed or the probe exits non-zero
  * throughput reported in both MiB/s and MB/s
  * percentile index documented as an order statistic (n=200 -> 3rd largest for p99)

Env: S3_HOST, S3_AK, S3_SK, S3_REGION (default us-east-1), S3_SCHEME (http|https),
     S3_BUCKET (default dagger-cache-probe).
Exit: 0 = all assertions passed, 1 = any assertion failed.
"""
import hashlib, hmac, http.client, os, sys, threading, time
from urllib.parse import quote

HOST   = os.environ.get("S3_HOST", "minio.minio.svc.cluster.local:9000")
SCHEME = os.environ.get("S3_SCHEME", "http")
REGION = os.environ.get("S3_REGION", "us-east-1")
BUCKET = os.environ.get("S3_BUCKET", "dagger-cache-probe")
AK     = os.environ["S3_AK"]
SK     = os.environ["S3_SK"]

GET_N, RANGE_N, HEAD_N, CONC = 200, 100, 50, 8
SIZE = 4 << 20

failures = []
def check(cond, msg):
    if not cond:
        failures.append(msg)
        print("FAIL:", msg)


def presign(method, path, expires=3600):
    t = time.gmtime()
    amzdate = time.strftime("%Y%m%dT%H%M%SZ", t)
    datestamp = time.strftime("%Y%m%d", t)
    scope = f"{datestamp}/{REGION}/s3/aws4_request"
    q = {"X-Amz-Algorithm": "AWS4-HMAC-SHA256",
         "X-Amz-Credential": f"{AK}/{scope}",
         "X-Amz-Date": amzdate,
         "X-Amz-Expires": str(expires),
         "X-Amz-SignedHeaders": "host"}
    cq = "&".join(f"{k}={quote(v, safe='-_.~')}" for k, v in sorted(q.items()))
    cr = f"{method}\n{path}\n{cq}\nhost:{HOST}\n\nhost\nUNSIGNED-PAYLOAD"
    sts = (f"AWS4-HMAC-SHA256\n{amzdate}\n{scope}\n"
           + hashlib.sha256(cr.encode()).hexdigest())
    k = hmac.new(("AWS4" + SK).encode(), datestamp.encode(), hashlib.sha256).digest()
    for part in (REGION, "s3", "aws4_request"):
        k = hmac.new(k, part.encode(), hashlib.sha256).digest()
    sig = hmac.new(k, sts.encode(), hashlib.sha256).hexdigest()
    return f"{path}?{cq}&X-Amz-Signature={sig}"


def conn():
    cls = http.client.HTTPSConnection if SCHEME == "https" else http.client.HTTPConnection
    return cls(HOST, timeout=30)


def do(c, method, url, body=None, headers=None):
    """Returns (status, headers, body_bytes, ttfb_s, total_s).

    TTFB is measured when response headers arrive (after getresponse()), not at
    the end of request serialisation -- round-3 finding 19.
    """
    t0 = time.perf_counter()
    c.request(method, url, body=body, headers=headers or {})
    r = c.getresponse()
    ttfb = time.perf_counter() - t0
    data = r.read()          # always read (HEAD included) and close the response
    hdrs = {k.lower(): v for k, v in r.getheaders()}
    return r.status, hdrs, data, ttfb, time.perf_counter() - t0


def pct(vals, p):
    """Order statistic. n=200, p=.99 -> index 197 -> 3rd largest, not 2nd."""
    v = sorted(vals)
    idx = int(round(p * (len(v) - 1)))
    return v[max(0, min(idx, len(v) - 1))]


def put_object(key, size=SIZE):
    c = conn()
    url = presign("PUT", f"/{BUCKET}/{key}")
    body = os.urandom(size)
    t0 = time.perf_counter()
    c.request("PUT", url, body=body)
    body_s = time.perf_counter() - t0
    r = c.getresponse()
    status = r.status
    r.read()
    resp_s = time.perf_counter() - t0 - body_s
    c.close()
    e2e = body_s + resp_s
    return status, size / 2**20 / body_s if body_s else 0, body_s, resp_s, \
        (size / 2**20 / e2e if e2e else 0)


def main():
    # 0. bucket reachable / creatable
    c = conn()
    st, _, _, _, _ = do(c, "PUT", presign("PUT", f"/{BUCKET}"))
    check(st in (200, 409), f"bucket create/head status {st}")
    st, _, _, _, _ = do(c, "GET", presign("GET", f"/{BUCKET}"))
    check(st == 200, f"bucket list status {st}")
    c.close()

    # 1. seed one object, assert
    key = "probe/base-4MiB.bin"
    status, mibs, body_s, resp_s, e2e_mibs = put_object(key)
    check(status in (200, 201), f"seed PUT status {status}")
    print(f"PUT 4 MiB (n=1): body {body_s*1e3:.1f} ms ({mibs:.1f} MiB/s), "
          f"response {resp_s*1e3:.1f} ms, "
          f"END-TO-END {e2e_mibs:.1f} MiB/s ({e2e_mibs*1.048576:.1f} MB/s)  <-- transfer rate")

    # 2. GET latency, asserted
    c = conn()
    url = presign("GET", f"/{BUCKET}/{key}")
    ttfbs, totals = [], []
    for _ in range(GET_N):
        st, hdrs, data, ttfb, total = do(c, "GET", url)
        check(st == 200, f"GET status {st}")
        check(len(data) == SIZE, f"GET length {len(data)} != {SIZE}")
        ttfbs.append(ttfb); totals.append(total)
    c.close()
    print(f"GET 4 MiB  TTFB p50 {pct(ttfbs,.5)*1e3:.2f}  p95 {pct(ttfbs,.95)*1e3:.2f}  "
          f"p99 {pct(ttfbs,.99)*1e3:.2f} ms;  total p50 {pct(totals,.5)*1e3:.1f} ms "
          f"({SIZE/2**20/pct(totals,.5):.1f} MiB/s)")

    # 3. open-ended ranges, asserted exactly
    c = conn()
    rt = []
    for i in range(RANGE_N):
        off = (i % 4) * (SIZE // 4)
        url = presign("GET", f"/{BUCKET}/{key}")
        st, hdrs, data, ttfb, _ = do(c, "GET", url, headers={"Range": f"bytes={off}-{SIZE-1}"})
        check(st == 206, f"range status {st}")
        check(hdrs.get("content-range") == f"bytes {off}-{SIZE-1}/{SIZE}",
              f"range Content-Range {hdrs.get('content-range')}")
        check(len(data) == SIZE - off, f"range bytes {len(data)} != {SIZE-off}")
        rt.append(ttfb)
    c.close()
    print(f"GET range  TTFB p50 {pct(rt,.5)*1e3:.2f}  p95 {pct(rt,.95)*1e3:.2f}  p99 {pct(rt,.99)*1e3:.2f} ms")

    # 4. HEAD (verifier path), size asserted
    c = conn()
    ht = []
    for _ in range(HEAD_N):
        st, hdrs, _, ttfb, _ = do(c, "HEAD", presign("HEAD", f"/{BUCKET}/{key}"))
        check(st == 200, f"HEAD status {st}")
        check(hdrs.get("content-length") == str(SIZE),
              f"HEAD Content-Length {hdrs.get('content-length')} != {SIZE}")
        ht.append(ttfb)
    c.close()
    print(f"HEAD       TTFB p50 {pct(ht,.5)*1e3:.2f}  p95 {pct(ht,.95)*1e3:.2f}  p99 {pct(ht,.99)*1e3:.2f} ms")

    # 5. expiry asserted (403)
    c = conn()
    exp = presign("GET", f"/{BUCKET}/{key}", expires=1)
    time.sleep(2)
    st, _, _, _, _ = do(c, "GET", exp)
    check(st == 403, f"expired capability status {st} (expected 403)")
    c.close()
    print(f"expired    status {st} (403 asserted)")

    # 6. concurrency: all writes must succeed
    results = [None] * CONC
    def worker(i):
        try:
            results[i] = put_object(f"probe/conc-{i}.bin")
        except Exception as e:                       # noqa: BLE001
            results[i] = (0, 0.0, 0.0, 0.0)
            print(f"FAIL: concurrent PUT {i} raised {e!r}")
    t0 = time.perf_counter()
    ths = [threading.Thread(target=worker, args=(i,)) for i in range(CONC)]
    for t in ths: t.start()
    for t in ths: t.join()
    wall = time.perf_counter() - t0
    check(all(r and r[0] in (200, 201) for r in results), "not all 8 concurrent PUTs succeeded")
    total_mib = CONC * SIZE / 2**20
    print(f"8x PUT wall {wall*1e3:.0f} ms -> {total_mib/wall:.1f} MiB/s "
          f"({total_mib*1.048576/wall:.1f} MB/s aggregate, all 8 asserted)")

    if failures:
        print(f"\n{len(failures)} ASSERTION(S) FAILED")
        return 1
    print("\nall assertions passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
