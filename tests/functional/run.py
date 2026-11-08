#!/usr/bin/env python3
"""Dagger dcache functional harness (stdlib-only, runs directly on Linux).

Executed on the ethpow Linux host where the local Docker daemon runs:

  python3 run.py \
    --binary /data/dcache-dev/bin/dcache \
    --output /data/dcache-dev/results \
    --engine-image registry.dagger.io/engine:v1.0.0-beta.25 \
    [--keep] [--scenario reuse|all]

Creates and cleans up ONLY its uniquely dcache-ft- prefixed containers and
network. Never prunes globally, never touches pre-existing services, never
deletes arbitrary filesystem paths, never publishes images.

Scenario "reuse":
  A produces a cached result; B (fresh engine) must observe a cache hit
  (counter stays 1); C proves the cache survives producer loss; D proves the
  cache survives a graceful coordinator restart (SIGTERM checkpoint).

Scenario "all" additionally exercises S3 fallback via stdlib SigV4 against
the MinIO fixture (delete published parts, fresh consumer still succeeds).

Result metadata always records: "96 independent engine capacity NOT RUN by
this harness".
"""

import argparse
import datetime
import hashlib
import hmac
import json
import os
import re
import shlex
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PREFIX_BASE = "dcache-ft-"
FIXTURE_TOKEN = "dck-functional-test-only"
FIXTURE_HMAC = "dcache-fixture-hmac-secret-0123456789abcdef"
FIXTURE_S3_USER = "dcachetest"
FIXTURE_S3_PASS = "dcachetest-secret"
FIXTURE_S3_BUCKET = "dcache-test"
FIXTURE_S3_REGION = "us-east-1"
S3_FIXTURE_IMAGE = ("rustfs/rustfs@sha256:ed9210d6411d77ffceb70d94b964a3cb"
                    "7d225983368bc25bebe01e995293dcc2")
FIXTURE_CLOUD_TOKEN = "dck-functional-test-only"
HIT_BODY = b"ok"
ENGINE_MEMORY = "2g"
ENGINE_CPUS = "2"
STOP_TIMEOUT = "30"


# --------------------------------------------------------------------------
# helpers: naming / ownership
# --------------------------------------------------------------------------

def make_prefix():
    """Unique per-run prefix, DNS/label safe, <=63 chars."""
    return (PREFIX_BASE + str(int(time.time() * 1000)) + "-" +
            os.urandom(3).hex())


def owned(prefix, name):
    """True only when name belongs to this run's prefix."""
    return bool(prefix) and isinstance(name, str) and name.startswith(prefix)


def cleanup_owned(docker, prefix, names):
    """Force-remove only containers whose names carry our prefix."""
    for name in names:
        if not owned(prefix, name):
            continue
        docker(["rm", "-f", "-v", name])


# --------------------------------------------------------------------------
# helpers: subprocess / docker
# --------------------------------------------------------------------------

def sh(args, capture=True, timeout=600, input_bytes=None, env=None):
    return subprocess.run(
        args,
        capture_output=capture,
        timeout=timeout,
        input=input_bytes,
        env=env,
    )


class Docker:
    def __init__(self, binary="docker"):
        self.binary = binary

    def __call__(self, args, timeout=600, input_bytes=None):
        return sh([self.binary] + list(args), timeout=timeout,
                  input_bytes=input_bytes)

    def out(self, args, timeout=600):
        p = self(args, timeout=timeout)
        if p.returncode != 0:
            raise RuntimeError("docker %s failed: %s" %
                               (" ".join(args), p.stderr.decode(errors="replace")))
        return p.stdout.decode()

    def json(self, args, timeout=600):
        return json.loads(self.out(args, timeout=timeout))


# --------------------------------------------------------------------------
# helpers: query building / parsing
# --------------------------------------------------------------------------

def _gql_str(s):
    """Escape a Python string as a GraphQL string literal."""
    out = s.replace("\\", "\\\\").replace('"', '\\"')
    out = out.replace("\n", "\\n").replace("\r", "\\r").replace("\t", "\\t")
    return '"' + out + '"'


def build_hit_query(image, hit_url, job="shared", read_nonce=None):
    """Identical exec args for producer/consumers: sh -c wget+printf.

    When read_nonce is given, the consumer reads the result back through
    File.withName, forcing an actual snapshot byte-read instead of a
    pre-exported scalar contents cache hit.
    """
    result = "cache-result-" + job
    cmd = ("set -eu; wget -qO- %s >/dev/null; printf %s > /dcache-result; "
           "printf %s" % (shlex.quote(hit_url), shlex.quote(result),
                          shlex.quote(result)))
    args_json = json.dumps(["sh", "-c", cmd])
    if read_nonce is None:
        selection = "stdout"
    else:
        selection = ("file(path: \"/dcache-result\") "
                     "{ withName(name: %s) { contents } }"
                     % _gql_str(read_nonce))
    return (
        "query { container { from(address: %s) { "
        "withExec(args: %s) { %s } } } }"
        % (_gql_str(image), args_json, selection)
    )


def parse_query_stdout(raw):
    """Parse the final JSON object from stdout that may carry progress lines."""
    if raw is None:
        raise ValueError("empty query stdout")
    text = raw.strip()
    if not text:
        raise ValueError("empty query stdout")
    # Scan lines from the end for the last line that parses as a JSON object.
    for line in reversed(text.splitlines()):
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(obj, dict):
            return obj
    # Fallback: last balanced {...} block.
    start = text.rfind("{")
    while start != -1:
        try:
            obj = json.loads(text[start:])
        except json.JSONDecodeError:
            start = text.rfind("{", 0, start)
            continue
        if isinstance(obj, dict):
            return obj
        break
    raise ValueError("no JSON object found in query stdout")


def extract_stdout(payload):
    """Extract stdout, or nested file.withName.contents, from a response."""
    node = payload
    for key in ("container", "from", "withExec", "stdout"):
        if isinstance(node, dict) and key in node:
            node = node[key]
            if isinstance(node, str):
                return node
            continue
        break
    # nested consumer shape: ...withExec.file.withName.contents
    node = payload
    for key in ("container", "from", "withExec", "file", "withName",
                "contents"):
        if not isinstance(node, dict) or key not in node:
            raise ValueError("missing %r in query response" % key)
        node = node[key]
    if not isinstance(node, str):
        raise ValueError("contents is not a string")
    return node


# --------------------------------------------------------------------------
# helpers: bounded waiting
# --------------------------------------------------------------------------

def wait_for(probe, timeout, interval, what="condition"):
    """Poll probe() until true or timeout; raise TimeoutError naming `what`.

    If the probe itself raises, the error is remembered and re-raised at
    timeout so a broken probe is never masked as a plain timeout.
    """
    deadline = time.monotonic() + timeout
    last_err = None
    while True:
        try:
            if probe():
                return True
        except Exception as e:  # keep polling; remember last error
            last_err = e
        if time.monotonic() >= deadline:
            if last_err is not None:
                raise last_err
            raise TimeoutError("%s: timed out after %.1fs" % (what, timeout))
        time.sleep(interval)


# --------------------------------------------------------------------------
# helpers: summary
# --------------------------------------------------------------------------

def summary_ok(stages):
    """True iff there is at least one stage and every stage passed."""
    return bool(stages) and all(ok for _, ok, _ in stages)


# --------------------------------------------------------------------------
# hit counter server (runs on the Linux host, reachable from Dagger exec)
# --------------------------------------------------------------------------

class HitCounter:
    def __init__(self):
        self.counts = {}
        self.lock = threading.Lock()
        self.server = None
        self.thread = None
        self.port = None

    class _Handler(BaseHTTPRequestHandler):
        counter = None

        def do_GET(self):
            job = "unknown"
            m = re.search(r"[?&]job=([^&\s]*)", self.path)
            if m:
                job = m.group(1)
            with self.counter.lock:
                self.counter.counts[job] = self.counter.counts.get(job, 0) + 1
                count = self.counter.counts[job]
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(HIT_BODY)))
            self.end_headers()
            self.wfile.write(HIT_BODY)
            self.log_message("hit job=%s count=%d", job, count)

        def log_message(self, fmt, *args):
            sys.stderr.write("[hitcounter] " + (fmt % args) + "\n")

    def start(self):
        self.server = ThreadingHTTPServer(("0.0.0.0", 0), self._Handler)
        self.server.counter = self
        self._Handler.counter = self
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever,
                                       daemon=True)
        self.thread.start()
        return self.port

    def count(self, job):
        with self.lock:
            return self.counts.get(job, 0)

    def stop(self):
        if self.server:
            self.server.shutdown()
            self.server.server_close()


# --------------------------------------------------------------------------
# SigV4 (stdlib) for the "all" scenario
# --------------------------------------------------------------------------

def _sigv4_sign(key, secret, region, service, method, host, path, query,
                payload):
    t = datetime.datetime.utcnow()
    amz_date = t.strftime("%Y%m%dT%H%M%SZ")
    date_stamp = t.strftime("%Y%m%d")
    payload_hash = hashlib.sha256(payload).hexdigest()
    canonical_headers = ("content-type:application/octet-stream\n"
                         "host:%s\nx-amz-content-sha256:%s\n"
                         "x-amz-date:%s\n" % (host, payload_hash, amz_date))
    signed_headers = "content-type;host;x-amz-content-sha256;x-amz-date"
    canonical_request = "\n".join([method, path, query, canonical_headers,
                                   signed_headers, payload_hash])
    scope = "/".join([date_stamp, region, service, "aws4_request"])
    string_to_sign = "\n".join([
        "AWS4-HMAC-SHA256", amz_date, scope,
        hashlib.sha256(canonical_request.encode()).hexdigest()])

    def _h(key_b, msg):
        return hmac.new(key_b, msg.encode(), hashlib.sha256).digest()

    k = _h(("AWS4" + secret).encode(), date_stamp)
    k = _h(k, region)
    k = _h(k, service)
    k = _h(k, "aws4_request")
    signature = hmac.new(k, string_to_sign.encode(), hashlib.sha256).hexdigest()
    return ("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, "
            "Signature=%s" % (key, scope, signed_headers, signature))


class S3Client:
    """Minimal stdlib S3 client against the MinIO fixture."""

    def __init__(self, endpoint, key=FIXTURE_S3_USER, secret=FIXTURE_S3_PASS,
                 region=FIXTURE_S3_REGION):
        self.endpoint = endpoint.rstrip("/")
        self.key, self.secret, self.region = key, secret, region
        self.host = self.endpoint.split("//", 1)[1]

    def _request(self, method, path, query="", payload=b"",
                 expect=(200, 204)):
        url = self.endpoint + path + (("?" + query) if query else "")
        auth = _sigv4_sign(self.key, self.secret, self.region, "s3", method,
                           self.host, path, query, payload)
        req = urllib.request.Request(url, data=payload if payload else None,
                                     method=method)
        req.add_header("Authorization", auth)
        req.add_header("x-amz-content-sha256",
                       hashlib.sha256(payload).hexdigest())
        req.add_header("x-amz-date",
                       time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()))
        req.add_header("Content-Type", "application/octet-stream")
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                body, code = r.read(), r.status
        except urllib.error.HTTPError as e:
            body, code = e.read(), e.code
        if code not in expect:
            raise RuntimeError("S3 %s %s -> HTTP %d: %s" %
                               (method, path, code, body[:300]))
        return body

    def put(self, key, data):
        self._request("PUT", "/" + FIXTURE_S3_BUCKET + "/" + key, payload=data)

    def delete(self, key):
        self._request("DELETE", "/" + FIXTURE_S3_BUCKET + "/" + key,
                      expect=(200, 204))

    def list(self, prefix=""):
        body = self._request(
            "GET", "/" + FIXTURE_S3_BUCKET,
            query="list-type=2&prefix=" + prefix.replace("/", "%2F"))
        return re.findall(r"<Key>([^<]+)</Key>", body.decode(errors="replace"))


# --------------------------------------------------------------------------
# harness
# --------------------------------------------------------------------------

WORKLOAD_IMAGE = "alpine@sha256:644c1ad8fc45cc581ab1b5ea7ec1bebc9b037fd4801ad697ca124deb3f9c9332"

class Harness:
    def __init__(self, args):
        self.args = args
        self.prefix = make_prefix()
        self.docker = Docker()
        self.stages = []          # (name, ok, detail)
        self.evidence = {
            "prefix": self.prefix,
            "started_utc": datetime.datetime.utcnow().isoformat() + "Z",
            "binary": args.binary,
            "engine_image": args.engine_image,
            "engine_fixture_args": ["--oci-worker-gc-keepstorage=100,0,1000"],
            "engine_gc_note": "Test-only bounded GC thresholds avoid host free-space pressure pruning unused warm entries; production settings are unchanged.",
            "engine_image_id": None,
            "scenario": args.scenario,
            "stages": self.stages,
            "counters": {},
            "timings": {},
            "containers": {},
            "network": None,
            "exit_code": None,
            "capacity_96_note": "96 independent engine capacity NOT RUN by "
                                "this harness",
        }
        self.names = {}
        self.hit = HitCounter()
        self.state_dir = os.path.join(args.output, "state-" + self.prefix)
        os.makedirs(args.output, exist_ok=True)
        os.makedirs(self.state_dir, exist_ok=True)
        self.log_path = os.path.join(args.output, self.prefix + ".log")
        self.log_fh = open(self.log_path, "w")

    # -- logging ----------------------------------------------------------
    def log(self, msg):
        line = "[%s] %s" % (datetime.datetime.utcnow().isoformat() + "Z", msg)
        self.log_fh.write(line + "\n")
        self.log_fh.flush()
        print(line, flush=True)

    def stage(self, name, ok, detail=""):
        self.stages.append((name, bool(ok), str(detail)))
        self.log("STAGE %s: %s %s" % (name, "PASS" if ok else "FAIL", detail))
        return bool(ok)

    # -- docker primitives ------------------------------------------------
    def net_gateway_ip(self):
        info = self.docker.json(["network", "inspect", self.names["net"]])
        return info[0]["IPAM"]["Config"][0]["Gateway"]

    def container_ip(self, name_suffix):
        info = self.docker.json(["inspect", self.names[name_suffix]])
        return info[0]["NetworkSettings"]["Networks"][
            self.names["net"]]["IPAddress"]

    def create_network(self):
        name = self.prefix + "net"
        self.names["net"] = name
        self.docker.out(["network", "create", "--driver", "bridge", name])
        self.evidence["network"] = name
        return name

    def run_container(self, name_suffix, image, args_list, env=None,
                      privileged=False, volumes=None, extra=None):
        name = self.prefix + name_suffix
        self.names[name_suffix] = name
        cmd = ["run", "-d", "--name", name,
               "--network", self.names["net"],
               "--memory", ENGINE_MEMORY, "--cpus", ENGINE_CPUS]
        if privileged:
            cmd.append("--privileged")
        for k, v in (env or {}).items():
            cmd += ["-e", "%s=%s" % (k, v)]
        for spec in (volumes or []):
            cmd += ["-v", spec]
            source = spec.split(":", 1)[0]
            if owned(self.prefix, source):
                tracked = self.evidence.setdefault("volumes", [])
                if source not in tracked:
                    tracked.append(source)
        cmd += (extra or [])
        cmd += [image] + args_list
        self.docker.out(cmd)
        self.evidence["containers"][name_suffix] = {"name": name}
        return name

    def stop_container(self, name_suffix):
        name = self.names[name_suffix]
        self.log("stopping %s (SIGTERM, timeout %s)" % (name, STOP_TIMEOUT))
        self.docker(["stop", "-t", STOP_TIMEOUT, name], timeout=120)
        self.evidence["containers"][name_suffix]["stopped"] = True

    def start_container(self, name_suffix):
        self.docker.out(["start", self.names[name_suffix]])
        self.evidence["containers"][name_suffix]["restarted"] = True

    def capture_logs(self):
        for suffix, info in self.evidence["containers"].items():
            if info.get("removed"):
                continue
            name = info["name"]
            p = self.docker(["logs", "--timestamps", name], timeout=120)
            info["logs_tail"] = (p.stdout.decode(errors="replace") +
                                 p.stderr.decode(errors="replace"))[-8000:]
            info["log_exit"] = p.returncode

    # -- readiness --------------------------------------------------------
    def http_ready(self, url, timeout=120, what="http"):
        def probe():
            try:
                with urllib.request.urlopen(url, timeout=5) as r:
                    return r.status == 200
            except Exception:
                return False
        wait_for(probe, timeout=timeout, interval=0.5, what=what)

    def wait_minio(self):
        ip = self.container_ip("minio")
        self.http_ready("http://%s:9000/health" % ip,
                        timeout=120, what="rustfs health")
        return ip

    def wait_coordinator(self):
        ip = self.container_ip("coord")
        self.http_ready("http://%s:8080/healthz" % ip,
                        timeout=120, what="coordinator healthz")
        return ip

    def wait_engine_ready(self, suffix):
        name = self.names[suffix]

        def probe():
            p = self.docker(["exec", name, "dagger", "version"], timeout=30)
            return p.returncode == 0
        wait_for(probe, timeout=180, interval=2,
                 what="engine %s CLI readiness" % suffix)

    # -- debug/state ------------------------------------------------------
    def _authed_get(self, path):
        req = urllib.request.Request(
            "http://%s:8080%s" % (self.container_ip("coord"), path))
        req.add_header("Authorization", "Bearer " + FIXTURE_TOKEN)
        with urllib.request.urlopen(req, timeout=10) as r:
            return json.loads(r.read().decode())

    def published_count(self):
        """Authenticated /debug/state; tolerate case variants of fields."""
        body = self._authed_get("/debug/state")

        def pick(d, *names):
            for n in names:
                if isinstance(d, dict) and n in d:
                    return d[n]
            return None
        pub = pick(body, "Published", "published")
        if isinstance(pub, dict):
            pub = pick(pub, "Count", "count", "Total", "total", "N", "n")
        if pub is None:
            stats = pick(body, "Stats", "stats") or {}
            pub = pick(stats, "Published", "published")
            if isinstance(pub, dict):
                pub = pick(pub, "Count", "count", "Total", "total")
        if pub is None:
            raise ValueError("no Published field in /debug/state: keys=%s" %
                             sorted(body.keys()))
        return int(pub), body

    def peers_count(self):
        """Authenticated /debug/state; tolerate peers map/list/counter."""
        body = self._authed_get("/debug/state")

        def pick(d, *names):
            for n in names:
                if isinstance(d, dict) and n in d:
                    return d[n]
            return None
        peers = pick(body, "Peers", "peers")
        if peers is None:
            stats = pick(body, "Stats", "stats") or {}
            peers = pick(stats, "Peers", "peers")
        if isinstance(peers, dict):
            return len(peers), body
        if isinstance(peers, (list, tuple)):
            return len(peers), body
        if isinstance(peers, int):
            return peers, body
        raise ValueError("no Peers field in /debug/state: keys=%s" %
                         sorted(body.keys()))

    def wait_for_peer_registration(self, timeout=30, interval=1):
        def probe():
            try:
                n, _ = self.peers_count()
                return n > 0
            except Exception:
                return False
        wait_for(probe, timeout=timeout, interval=interval,
                 what="coordinator peer registration")
        n, body = self.peers_count()
        self.evidence["peer_registration"] = {"count": n}
        return n

    def wait_published(self, timeout=120, interval=0.2, settle_polls=3):
        """Wait until Published>0, exporting==0, and the Published counter is
        unchanged for settle_polls consecutive polls (quiescence)."""
        deadline = time.monotonic() + timeout
        stable_n = None
        stable = 0
        while True:
            n, body = self.published_count()
            exporting = body.get("exporting", body.get("Exporting", 0))
            if n > 0 and not exporting and n == stable_n:
                stable += 1
            else:
                stable = 1 if (n > 0 and not exporting) else 0
                stable_n = n
            if stable >= settle_polls:
                self.evidence["debug_state"] = body
                self.evidence["published_quiescent"] = n
                return n
            if time.monotonic() >= deadline:
                raise TimeoutError("published quiescence: timed out after %.1fs"
                                   % timeout)
            time.sleep(interval)

    def merge_count(self):
        """Authenticated /debug/state; return stats.merges as an integer."""
        body = self._authed_get("/debug/state")
        stats = body.get("stats", body.get("Stats") or {})
        m = stats.get("merges", stats.get("Merges"))
        if m is None:
            raise ValueError("no merges field in /debug/state: keys=%s" %
                             sorted(body.keys()))
        return int(m)

    def wait_warm_merge(self, baseline, timeout=60, interval=0.2):
        """Wait until merge_count() exceeds baseline; return the new count."""
        def probe():
            return self.merge_count() > baseline
        wait_for(probe, timeout=timeout, interval=interval,
                 what="warm merge (baseline=%d)" % baseline)
        n = self.merge_count()
        self.evidence["warm_merges"] = n
        return n

    def discard_engine(self, suffix):
        """Stop and remove an owned engine container, proving absence."""
        name = self.names[suffix]
        if not owned(self.prefix, name):
            raise ValueError("refusing to discard unowned container %r" % name)
        self.stop_container(suffix)
        # capture logs BEFORE removal so evidence is not lost
        info = self.evidence["containers"][suffix]
        p = self.docker(["logs", "--timestamps", name], timeout=120)
        info["logs_tail"] = (p.stdout.decode(errors="replace") +
                             p.stderr.decode(errors="replace"))[-8000:]
        info["log_exit"] = p.returncode
        self.docker(["rm", "-f", "-v", name], timeout=120)
        info["removed"] = True
        # read-back proof: inspect must fail (no such object)
        rb = self.docker(["inspect", name], timeout=60)
        if rb.returncode == 0:
            raise RuntimeError("container %s still exists after removal" % name)
        self.evidence["producer_absent"] = {"suffix": suffix, "name": name,
                                            "inspect_exit": rb.returncode}
        self.log("discarded engine %s (inspect exit=%d proves absent)" %
                 (name, rb.returncode))

    # -- the real query ---------------------------------------------------
    def run_query(self, engine_suffix, hit_url, job="shared",
                  read_nonce=None):
        """Run the identical GraphQL query via docker exec on the engine."""
        name = self.names[engine_suffix]
        coord_ip = self.container_ip("coord")
        # Immutable image: mutable tags are scoped per Dagger session.
        query = build_hit_query(WORKLOAD_IMAGE, hit_url, job=job,
                                read_nonce=read_nonce)
        t0 = time.monotonic()
        cmd = ["dagger", "api", "query", "--no-load-module",
               "--engine", "unix:///run/dagger/engine.sock",
               "--progress", "plain"]
        p = self.docker(
            ["exec", "-i",
             "-e", "DAGGER_CLOUD_URL=http://%s:8080" % coord_ip,
             "-e", "DAGGER_CLOUD_TOKEN=" + FIXTURE_CLOUD_TOKEN,
             name] + cmd, timeout=900, input_bytes=query.encode())
        elapsed = time.monotonic() - t0
        stdout = p.stdout.decode(errors="replace")
        stderr = p.stderr.decode(errors="replace")
        self.log("query on %s exit=%d elapsed=%.2fs" %
                 (engine_suffix, p.returncode, elapsed))
        if p.returncode != 0:
            raise RuntimeError(
                "query failed on %s exit=%d\nstdout=%s\nstderr=%s" %
                (engine_suffix, p.returncode, stdout[-2000:], stderr[-2000:]))
        payload = parse_query_stdout(stdout)
        out = extract_stdout(payload)
        return out, elapsed, {"stdout": stdout[-2000:], "stderr": stderr[-2000:]}

    # -- setup ------------------------------------------------------------
    def setup(self):
        self.create_network()
        gateway = self.net_gateway_ip()
        self.evidence["host_gateway_ip"] = gateway

        # S3 fixture: isolated digest-pinned RustFS image (public MinIO
        # images are unavailable); entrypoint handles /data, args []
        self.run_container(
            "minio", S3_FIXTURE_IMAGE, [],
            env={"RUSTFS_ACCESS_KEY": FIXTURE_S3_USER,
                 "RUSTFS_SECRET_KEY": FIXTURE_S3_PASS,
                 "RUSTFS_VOLUMES": "/data",
                 "RUSTFS_ADDRESS": "0.0.0.0:9000",
                 "RUSTFS_CONSOLE_ENABLE": "false",
                 "RUSTFS_UNSAFE_BYPASS_DISK_CHECK": "true"},
            volumes=[self.state_dir + "/minio:/data"],
            extra=["--user", "0"])
        minio_ip = self.wait_minio()
        self.evidence["minio_ip"] = minio_ip
        self.evidence["s3_provider"] = "rustfs"
        self.evidence["s3_image"] = S3_FIXTURE_IMAGE

        # Coordinator: ubuntu:24.04 runtime, binary mounted read-only /dcache
        coord_env = {
            "DCACHE_LISTEN": ":8080",
            "DCACHE_DB": "/state/cache.db",
            "DCACHE_POOL": "shared",
            "DCACHE_TOKEN": FIXTURE_TOKEN,
            "DCACHE_HMAC_SECRET": FIXTURE_HMAC,
            "S3_ENDPOINT": "http://%s:9000" % minio_ip,
            "S3_PUBLIC_ENDPOINT": "http://%s:9000" % minio_ip,
            "S3_BUCKET": FIXTURE_S3_BUCKET,
            "S3_REGION": FIXTURE_S3_REGION,
            "S3_ACCESS_KEY": FIXTURE_S3_USER,
            "S3_SECRET_KEY": FIXTURE_S3_PASS,
        }
        self.run_container(
            "coord", "ubuntu:24.04",
            ["/dcache"],
            env=coord_env,
            volumes=[self.args.binary + ":/dcache:ro",
                     self.state_dir + "/coord:/state"])
        coord_ip = self.wait_coordinator()
        self.evidence["coord_ip"] = coord_ip

        # hit counter on the host, advertised via the bridge gateway IP
        port = self.hit.start()
        self.hit_url = "http://%s:%d/hit?job=shared" % (gateway, port)
        self.evidence["hit_url"] = self.hit_url
        self.evidence["hit_port"] = port

        # engine image id evidence
        try:
            img = self.docker.json(["image", "inspect", self.args.engine_image])
            self.evidence["engine_image_id"] = img[0].get("Id")
        except Exception as e:
            self.log("engine image inspect failed: %s" % e)

    def start_engine(self, suffix):
        coord_ip = self.container_ip("coord")
        self.run_container(
            suffix, self.args.engine_image,
            ["--oci-worker-gc-keepstorage=100,0,1000"],
            env={
                "_EXPERIMENTAL_DAGGER_REMOTE_CACHE_URL":
                    "http://%s:8080" % coord_ip,
                "DAGGER_CLOUD_URL": "http://%s:8080" % coord_ip,
                "DAGGER_CLOUD_TOKEN": FIXTURE_CLOUD_TOKEN,
                "OTEL_EXPORTER_OTLP_TRACES_LIVE": "1",
            },
            privileged=True,
            volumes=[self.prefix + suffix + "-dagger:/var/lib/dagger"])
        self.wait_engine_ready(suffix)
        return self.names[suffix]

    # -- scenario reuse ---------------------------------------------------
    def scenario_reuse(self):
        job = "shared"
        expected = "cache-result-" + job

        # Stage A: producer
        self.start_engine("engineA")
        out, elapsed, raw = self.run_query("engineA", self.hit_url, job)
        c1 = self.hit.count(job)
        ok = (out == expected) and (c1 == 1)
        self.stage("A", ok, "out=%r counter=%d elapsed=%.2fs" %
                   (out, c1, elapsed))
        self.evidence["counters"]["A"] = c1
        self.evidence["timings"]["A"] = elapsed
        if not ok:
            self.evidence["raw_A"] = raw
            return False

        # publication evidence (quiescence: exports finished, counter stable)
        pub = self.wait_published()
        self.log("Published=%d (quiescent)" % pub)

        # discard producer A BEFORE starting B so B cannot see A as a peer
        self.discard_engine("engineA")

        # Stage B: fresh engine, warm merge must complete before query
        baseline = self.merge_count()
        self.start_engine("engineB")
        merges = self.wait_warm_merge(baseline)
        self.log("warm merge observed: merges=%d (baseline=%d)" %
                 (merges, baseline))

        out, elapsed, raw = self.run_query("engineB", self.hit_url, job,
                                           read_nonce="engineB")
        c2 = self.hit.count(job)
        ok = (out == expected) and (c2 == 1)
        self.stage("B", ok, "out=%r counter=%d elapsed=%.2fs" %
                   (out, c2, elapsed))
        self.evidence["counters"]["B"] = c2
        self.evidence["timings"]["B"] = elapsed
        if not ok:
            self.evidence["raw_B"] = raw
            return False

        # Stage C: LOCAL WARM on the same engineB (no engineC); counter
        # must still be 1 (L1 warm read, no recompute)
        out, elapsed, raw = self.run_query("engineB", self.hit_url, job,
                                           read_nonce="engineB")
        c3 = self.hit.count(job)
        ok = (out == expected) and (c3 == 1)
        self.stage("C", ok, "out=%r counter=%d elapsed=%.2fs" %
                   (out, c3, elapsed))
        self.evidence["counters"]["C"] = c3
        self.evidence["timings"]["C"] = elapsed
        if not ok:
            self.evidence["raw_C"] = raw
            return False

        # quiesce then discard engineB before the restart stage
        self.wait_published()
        self.discard_engine("engineB")

        # Stage D: graceful coordinator restart, fresh engine D
        self.stop_container("coord")
        self.start_container("coord")
        self.wait_coordinator()
        baseline = self.merge_count()
        self.start_engine("engineD")
        self.wait_warm_merge(baseline)
        out, elapsed, raw = self.run_query("engineD", self.hit_url, job,
                                           read_nonce="engineD")
        c4 = self.hit.count(job)
        ok = (out == expected) and (c4 == 1)
        self.stage("D", ok, "out=%r counter=%d elapsed=%.2fs" %
                   (out, c4, elapsed))
        self.evidence["counters"]["D"] = c4
        self.evidence["timings"]["D"] = elapsed
        if not ok:
            self.evidence["raw_D"] = raw
            return False
        return True

    # -- scenario all (S3 fallback via SigV4) ------------------------------
    def scenario_all(self):
        ok = self.scenario_reuse()
        if not ok:
            return False
        job = "shared"
        expected = "cache-result-" + job
        s3 = S3Client("http://%s:9000" % self.container_ip("minio"))
        # Finish exports and remove the last live engine before enumerating
        # objects: a pre-quiescence listing can miss a late completed PUT.
        self.wait_published()
        self.discard_engine("engineD")
        keys = s3.list()
        self.evidence["s3_keys_before"] = keys
        if not keys:
            self.stage("S3-fallback", False, "no published keys to delete")
            return False
        for key in keys:
            s3.delete(key)
        remaining = s3.list()
        self.stage("S3-delete", len(remaining) == 0,
                   "deleted %d keys, remaining=%d" %
                   (len(keys), len(remaining)))
        if remaining:
            return False
        baseline = self.merge_count()
        self.start_engine("engineE")
        self.wait_warm_merge(baseline)
        out, elapsed, raw = self.run_query("engineE", self.hit_url, job,
                                           read_nonce="engineE")
        c = self.hit.count(job)
        # after deletion the fresh consumer must still produce correct output
        # via fallback (recompute -> counter increments)
        ok = (out == expected) and (c == 2)
        self.stage("S3-fallback", ok,
                   "out=%r counter=%d elapsed=%.2fs" % (out, c, elapsed))
        self.evidence["counters"]["S3-fallback"] = c
        self.evidence["timings"]["S3-fallback"] = elapsed
        if not ok:
            self.evidence["raw_S3fallback"] = raw
            return False

        # Stage S3-corrupt-fallback: quiesce E exports/reuploads, discard E,
        # then corrupt every object in the fixture bucket. A fresh consumer
        # (engineF, pinned immutable image) must still succeed via fallback.
        self.wait_published()
        self.discard_engine("engineE")
        keys = s3.list()
        self.evidence["s3_keys_before_corrupt"] = keys
        if not keys:
            self.stage("S3-corrupt-fallback", False,
                       "no keys in fixture bucket to corrupt")
            return False
        for key in keys:
            s3.put(key, b"corrupt-cache-test")
        self.log("corrupted %d fixture-bucket keys" % len(keys))

        baseline = self.merge_count()
        self.start_engine("engineF")
        self.wait_warm_merge(baseline)
        out, elapsed, raw = self.run_query("engineF", self.hit_url, job,
                                           read_nonce="engineF")
        c = self.hit.count(job)
        ok = (out == expected) and (c == 3)
        self.stage("S3-corrupt-fallback", ok,
                   "out=%r counter=%d elapsed=%.2fs" % (out, c, elapsed))
        self.evidence["counters"]["S3-corrupt-fallback"] = c
        self.evidence["timings"]["S3-corrupt-fallback"] = elapsed
        if not ok:
            self.evidence["raw_S3corruptfallback"] = raw
            return False
        return True

    # -- lifecycle ---------------------------------------------------------
    def teardown(self):
        self.log("teardown: removing and verifying only owned Docker resources")
        try:
            self.capture_logs()
        except Exception as e:
            self.log("log capture error: %s" % e)
        self.hit.stop()
        errors, absent = [], []

        def remove_and_verify(name, remove_args, inspect_args, already_removed=False):
            if not owned(self.prefix, name):
                return
            try:
                if not already_removed:
                    result = self.docker(remove_args, timeout=120)
                    if result.returncode:
                        errors.append("remove %s: exit %d" % (name, result.returncode))
                result = self.docker(inspect_args, timeout=60)
                detail = result.stderr.decode(errors="replace").lower()
                if result.returncode == 0:
                    errors.append("still exists: " + name)
                elif "no such" not in detail and "not found" not in detail:
                    errors.append("absence could not be verified: " + name)
                else:
                    absent.append(name)
            except Exception as e:
                errors.append("cleanup %s: %s" % (name, str(e)[:200]))

        for suffix, name in self.names.items():
            if suffix == "net":
                continue
            info = self.evidence.get("containers", {}).get(suffix, {})
            remove_and_verify(name, ["rm", "-f", "-v", name], ["inspect", name],
                              info.get("removed", False))
            if name in absent:
                info["removed"] = True
        # Docker rm -v does not remove explicitly named volumes.
        for name in self.evidence.get("volumes", []):
            remove_and_verify(name, ["volume", "rm", name],
                              ["volume", "inspect", name])
        if "net" in self.names:
            name = self.names["net"]
            remove_and_verify(name, ["network", "rm", name],
                              ["network", "inspect", name])
        self.evidence["cleanup"] = {"verified_absent": absent, "errors": errors}
        # Root-owned forensic state may remain; it is not a live Docker resource.
        try:
            state_dir, output = os.path.abspath(self.state_dir), os.path.abspath(self.args.output)
            if os.path.commonpath([state_dir, output]) == output:
                shutil.rmtree(state_dir)
        except Exception as e:
            self.evidence["state_cleanup_error"] = str(e)
            self.log("state dir retained: %s" % e)
        if errors:
            raise RuntimeError("Docker cleanup failed: " + "; ".join(errors[:3]))
        self.log("teardown complete: Docker resource absence verified")

    def write_results(self, exit_code):
        self.evidence["exit_code"] = exit_code
        self.evidence["finished_utc"] = (
            datetime.datetime.utcnow().isoformat() + "Z")
        path = os.path.join(self.args.output, self.prefix + ".json")
        with open(path, "w") as f:
            json.dump(self.evidence, f, indent=2, default=str)
        self.log("results written to %s" % path)

    def run(self):
        exit_code = 0
        try:
            ok = False
            self.setup()
            if self.args.scenario == "all":
                ok = self.scenario_all()
            else:
                ok = self.scenario_reuse()
            if not summary_ok(self.stages):
                exit_code = 1
                self.log("ACCEPTANCE FAILED: %s" %
                         [s for s in self.stages if not s[1]])
        except Exception as e:
            exit_code = 1
            self.stage("harness-exception", False, repr(e))
            self.log("harness exception: %r" % e)
        finally:
            if not ok and exit_code == 0:
                exit_code = 1
            try:
                self.capture_logs()
            except Exception as e:
                self.log("log capture error: %s" % e)
            if not self.args.keep:
                try:
                    self.teardown()
                except Exception as e:
                    exit_code = 1
                    self.evidence["cleanup_error"] = repr(e)
                    self.log("cleanup failed: %s" % e)
            else:
                self.log("--keep: leaving containers in place (prefix %s)" %
                         self.prefix)
            try:
                self.write_results(exit_code)
            except Exception as e:
                exit_code = 1
                self.log("failed writing results: %r" % e)
        return exit_code


def main(argv=None):
    ap = argparse.ArgumentParser(description="dcache functional harness")
    ap.add_argument("--binary", required=True)
    ap.add_argument("--output", required=True)
    ap.add_argument("--engine-image", required=True)
    ap.add_argument("--keep", action="store_true")
    ap.add_argument("--scenario", choices=["reuse", "all"], default="reuse")
    args = ap.parse_args(argv)
    return Harness(args).run()


if __name__ == "__main__":
    sys.exit(main())
