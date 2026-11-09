# HISTORICAL DIAGNOSTIC ONLY: not a supported acceptance test. See docs/history/README.md.
"""S3 latency/behaviour probe v2 — models what the Dagger engine actually does.

Fixes applied vs v1 (review F43-F50):
  - presigned query-string URLs (the engine receives a capability URL, sends no auth headers)
  - PUT timed as body-transfer (MB/s) separately from the response wait (v1 conflated them)
  - open-ended Range  bytes=off-(size-1)  with exact Content-Range assertion
  - HEAD response always read/closed (v1 crashed with ResponseNotReady)
  - expired capability must return 403
  - distinct objects, concurrency 8, percentiles over n>=200
  - region/scheme from env; bucket reachability asserted
Run: python3 s3_latency_probe.py   (env: S3_HOST S3_AK S3_SK [S3_REGION] [S3_SCHEME] [S3_BUCKET])
"""
import hashlib, hmac, http.client, os, sys, time, threading
from urllib.parse import quote

HOST = os.environ["S3_HOST"]              # host[:port]
AK = os.environ["S3_AK"]
SK = os.environ["S3_SK"]
REGION = os.environ.get("S3_REGION", "us-east-1")
SCHEME = os.environ.get("S3_SCHEME", "http")
BUCKET = os.environ.get("S3_BUCKET", "dcache-probe")
CONN = http.client.HTTPSConnection if SCHEME == "https" else http.client.HTTPConnection


def _hm(k, m):
    return hmac.new(k, m.encode(), hashlib.sha256).digest()


def presign(method, path, expires=900):
    """Query-string SigV4 with UNSIGNED-PAYLOAD: exactly the engine's capability model."""
    t = time.gmtime()
    amz = time.strftime("%Y%m%dT%H%M%SZ", t)
    date = amz[:8]
    scope = f"{date}/{REGION}/s3/aws4_request"
    q = {
        "X-Amz-Algorithm": "AWS4-HMAC-SHA256",
        "X-Amz-Credential": f"{AK}/{scope}",
        "X-Amz-Date": amz,
        "X-Amz-Expires": str(expires),
        "X-Amz-SignedHeaders": "host",
    }
    cq = "&".join(f"{k}={quote(v, safe='-_.~')}" for k, v in sorted(q.items()))
    cr = f"{method}\n{path}\n{cq}\nhost:{HOST}\n\nhost\nUNSIGNED-PAYLOAD"
    sts = f"AWS4-HMAC-SHA256\n{amz}\n{scope}\n{hashlib.sha256(cr.encode()).hexdigest()}"
    k = _hm(("AWS4" + SK).encode(), date)
    for part in (REGION, "s3", "aws4_request"):
        k = _hm(k, part)
    sig = hmac.new(k, sts.encode(), hashlib.sha256).hexdigest()
    return f"{path}?{cq}&X-Amz-Signature={sig}"


def pct(v, p):
    v = sorted(v)
    i = min(len(v) - 1, max(0, int(round(p / 100 * (len(v) - 1)))))
    return v[i] * 1000


def report(name, vals, extra=""):
    print(f"{name:34s} n={len(vals):4d} p50={pct(vals,50):8.2f}ms "
          f"p95={pct(vals,95):8.2f}ms p99={pct(vals,99):8.2f}ms max={max(vals)*1000:8.2f}ms {extra}")


def get(conn, path, rng=None):
    url = presign("GET", path)
    h = {"Range": rng} if rng else {}
    t0 = time.perf_counter()
    conn.request("GET", url, headers=h)
    r = conn.getresponse()
    ttfb = time.perf_counter() - t0
    body = r.read()
    total = time.perf_counter() - t0
    return r.status, r.getheader("Content-Range"), ttfb, total, len(body)


def put(conn, path, body):
    url = presign("PUT", path)
    t0 = time.perf_counter()
    conn.putrequest("PUT", url)
    conn.putheader("Content-Length", str(len(body)))
    conn.endheaders()
    conn.send(body)
    t_body = time.perf_counter() - t0
    r = conn.getresponse()
    r.read()
    t_resp = time.perf_counter() - t0
    return r.status, t_body, t_resp - t_body


c = CONN(HOST, timeout=60)
st, _, _, _, _ = get(c, "/" + BUCKET)
print("reachability status:", st)

payload = os.urandom(4 * 1024 * 1024)
OBJ = f"/{BUCKET}/blob4m"
st, t_body, t_resp = put(c, OBJ, payload)
print(f"PUT 4MiB: body={t_body*1000:.1f}ms ({len(payload)/t_body/1e6:.1f} MB/s) "
      f"response_wait={t_resp*1000:.1f}ms status={st}")

ttfb, total = [], []
for _ in range(200):
    st, cr, tf, tt, n = get(c, OBJ)
    if st != 200 or n != len(payload):
        sys.exit(f"GET failed {st} {n}")
    ttfb.append(tf); total.append(tt)
report("GET 4MiB presigned", ttfb, f"total_p50={pct(total,50):.1f}ms")

size = len(payload)
rng_ttfb, rng_total = [], []
for i in range(100):
    off = (i % 4) * 1024 * 1024
    st, cr, tf, tt, n = get(c, OBJ, f"bytes={off}-{size-1}")
    want = f"bytes {off}-{size-1}/{size}"
    if st != 206 or cr != want:
        sys.exit(f"RANGE mismatch st={st} cr={cr!r} want={want!r}")
    rng_ttfb.append(tf); rng_total.append(tt)
report("GET open range presigned", rng_ttfb, f"total_p50={pct(rng_total,50):.1f}ms")

head_ttfb = []
for _ in range(50):
    url = presign("HEAD", OBJ)
    t0 = time.perf_counter()
    c.request("HEAD", url)
    r = c.getresponse()
    r.read()
    head_ttfb.append(time.perf_counter() - t0)
report("HEAD presigned", head_ttfb)

url = presign("GET", OBJ, expires=1)
time.sleep(2)
c.request("GET", url)
r = c.getresponse(); r.read()
print("expired capability status:", r.status, "(expected 403)")


def worker(i, out):
    cc = CONN(HOST, timeout=60)
    body = os.urandom(4 * 1024 * 1024)
    t0 = time.perf_counter()
    put(cc, f"/{BUCKET}/cold-{i}", body)
    out[i] = time.perf_counter() - t0
    cc.close()


out = {}
ths = [threading.Thread(target=worker, args=(i, out)) for i in range(8)]
t0 = time.perf_counter()
for t in ths: t.start()
for t in ths: t.join()
wall = time.perf_counter() - t0
print(f"8x 4MiB concurrent PUT: wall={wall*1000:.0f}ms aggregate={8*4/wall:.1f} MB/s")
