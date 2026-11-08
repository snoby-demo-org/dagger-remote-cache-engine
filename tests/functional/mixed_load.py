#!/usr/bin/env python3
"""RealDagger mixed-load functional driver (stdlib only).

One cached shared job plus N-1 unique jobs, each on a fresh independent
engine, with a resource guard before any fixtures are created.
"""

import argparse
import json
import os
import sys
import threading
import time
import urllib.parse
from concurrent.futures import ThreadPoolExecutor, as_completed

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import run as fixture  # noqa: E402


# ---------------------------------------------------------------- helpers

def job_plan(n):
    """Return the job plan: shared jobs first, then unique jobs."""
    if n < 2:
        raise ValueError("n must be >= 2")
    shared = max(1, n // 4)
    jobs = ["shared"] * shared
    for i in range(n - shared):
        jobs.append("unique-%d" % i)
    return jobs


def check_results(jobs, rows, counters):
    """Validate worker rows and hit counters; return list of error strings."""
    errors = []
    n = len(jobs)
    expected_workers = ["worker%d" % i for i in range(n)]

    seen_workers = []
    for row in rows:
        seen_workers.append(row.get("worker"))

    for w in expected_workers:
        matching = [r for r in rows if r.get("worker") == w]
        if len(matching) != 1:
            errors.append("worker %s appears %d times" % (w, len(matching)))
            continue
        row = matching[0]
        if row.get("job") != jobs[expected_workers.index(w)]:
            errors.append("worker %s job mismatch" % w)
        if row.get("error"):
            errors.append("worker %s error: %s" % (w, row.get("error")))
        if row.get("output") != "cache-result-" + str(row.get("job")):
            errors.append("worker %s wrong output" % w)

    for w in seen_workers:
        if w not in expected_workers:
            errors.append("extraneous worker %s" % w)

    for job in set(jobs):
        c = counters.get(job, 0)
        if c != 1:
            errors.append("counter for %s is %d, expected 1" % (job, c))
    for job in counters:
        if job not in set(jobs):
            errors.append("extraneous counter %s" % job)

    return errors


def peak_query_overlap(rows):
    """Sweep-line max concurrent queries over rows with query_start/end."""
    events = []
    for row in rows:
        s = row.get("query_start")
        e = row.get("query_end")
        if s is None or e is None:
            continue
        events.append((s, 1))
        events.append((e, -1))
    # End events before start events at the same timestamp.
    events.sort(key=lambda ev: (ev[0], ev[1]))
    active = 0
    peak = 0
    for _, delta in events:
        active += delta
        if active > peak:
            peak = active
    return peak


def memory_required_bytes(n, per_engine_bytes):
    """Memory required: n engines + producer, plus 2 GiB headroom."""
    return (n + 1) * per_engine_bytes + 2 * 1024 ** 3


def _mem_available():
    with open("/proc/meminfo") as fh:
        for line in fh:
            if line.startswith("MemAvailable:"):
                return int(line.split()[1]) * 1024
    raise RuntimeError("MemAvailable not found in /proc/meminfo")


# ------------------------------------------------------------------- CLI

def parse_args(argv):
    p = argparse.ArgumentParser(description="mixed-load RealDagger driver")
    p.add_argument("--binary", required=True, help="Linux binary")
    p.add_argument("--output", required=True, help="output directory")
    p.add_argument("--engine-image", required=True, help="engine image")
    p.add_argument("--clients", type=int, default=4)
    p.add_argument("--engine-memory-mb", type=int, default=256)
    p.add_argument("--keep", action="store_true")
    p.set_defaults(scenario="mixed-load")
    return p.parse_args(argv)


# ------------------------------------------------------------------ main

def main(argv=None):
    args = parse_args(sys.argv[1:] if argv is None else argv)

    if args.clients < 1:
        print("--clients must be positive", file=sys.stderr)
        return 2
    if args.engine_memory_mb < 1:
        print("--engine-memory-mb must be positive", file=sys.stderr)
        return 2
    if not os.path.isfile(args.binary):
        print("--binary does not exist: %s" % args.binary, file=sys.stderr)
        return 2
    if not os.path.isdir(args.output):
        print("--output does not exist: %s" % args.output, file=sys.stderr)
        return 2

    # Resource guard BEFORE creating fixtures.
    per_engine = args.engine_memory_mb * 1024 * 1024
    required = memory_required_bytes(args.clients, per_engine)
    available = _mem_available()
    if required > available:
        guard = {
            "status": "NOT_RUN",
            "reason": "resource_guard",
            "requested": args.clients,
            "available_bytes": available,
            "required_bytes": required,
        }
        path = os.path.join(args.output, "mixed_load_resource_guard.json")
        with open(path, "w") as fh:
            json.dump(guard, fh, indent=2)
        print("NOT_RUN: insufficient memory (required %d, available %d)"
              % (required, available), file=sys.stderr)
        return 2

    h = fixture.Harness(args)
    evidence = {}
    exit_code = 1
    try:
        h.setup()
        # Tighten engine resources for the engines we create.
        fixture.ENGINE_MEMORY = "%dm" % args.engine_memory_mb
        fixture.ENGINE_CPUS = os.environ.get("DCACHE_TEST_ENGINE_CPUS", "0.25")
        h.evidence["engine_cpu_limit"] = fixture.ENGINE_CPUS
        h.evidence["engine_memory_mb"] = args.engine_memory_mb
        h.evidence["capacity_96_note"] = "See mixed_load requested/completed clients and measured query overlap; this is not a general capacity guarantee."

        start = time.time()

        # Producer: warm the shared cache entry.
        h.start_engine("producer")
        out, elapsed, raw = h.run_query("producer", h.hit_url, job="shared")
        if out != "cache-result-shared":
            raise RuntimeError("producer output wrong: %r" % out)
        if h.hit.count("shared") != 1:
            raise RuntimeError("shared counter != 1 after producer")
        h.wait_published()
        h.discard_engine("producer")

        jobs = job_plan(args.clients)
        n = len(jobs)

        # Start one fresh engine per worker, waiting for warm merge each time.
        for i in range(n):
            baseline = h.merge_count()
            h.start_engine("worker%d" % i)
            h.wait_warm_merge(baseline)

        # Launch all jobs concurrently with a barrier.
        barrier = threading.Barrier(n)
        lock = threading.Lock()
        active = {"count": 0, "peak": 0}
        rows = []
        rows_lock = threading.Lock()
        errors_by_worker = {}

        def do_job(idx):
            worker = "worker%d" % idx
            job = jobs[idx]
            if job == "shared":
                url = h.hit_url.split("?")[0] + "?job=" + urllib.parse.quote("shared", safe="")
            else:
                url = h.hit_url.split("?")[0] + "?job=" + urllib.parse.quote(job, safe="")
            barrier.wait()
            with lock:
                active["count"] += 1
                if active["count"] > active["peak"]:
                    active["peak"] = active["count"]
            query_start = None
            query_end = None
            try:
                out, elapsed, raw = h.run_query(worker, url, job=job,
                                                read_nonce="worker%d" % idx)
                query_end = time.monotonic()
                if elapsed is not None:
                    query_start = query_end - elapsed
                err = None
                if out != "cache-result-" + job:
                    err = "wrong output %r" % out
            except Exception as exc:  # noqa: BLE001
                out, elapsed, raw = None, None, str(exc)
                err = str(exc)
                query_start = None
                query_end = None
            finally:
                with lock:
                    active["count"] -= 1
            row = {"worker": worker, "job": job, "output": out,
                   "error": err, "elapsed": elapsed,
                   "query_start": query_start, "query_end": query_end}
            with rows_lock:
                rows.append(row)
            return row

        with ThreadPoolExecutor(max_workers=n) as pool:
            futures = [pool.submit(do_job, i) for i in range(n)]
            for f in as_completed(futures):
                f.result()

        with h.hit.lock:
            counters = dict(h.hit.counts)

        errors = check_results(jobs, rows, counters)
        peak = peak_query_overlap(rows)
        if peak < n:
            errors.append("peak inflight %d < %d" % (peak, n))

        try:
            debug_state = h._authed_get("/debug/state")
        except Exception as exc:  # noqa: BLE001
            debug_state = {"error": repr(exc)}

        duration = time.time() - start
        evidence.update({
            "requested_clients": args.clients,
            "completed_clients": len(rows),
            "peak_inflight": peak,
            "host_thread_activity": {"active": active},
            "job_plan": jobs,
            "results": rows,
            "counters": counters,
            "debugstate": debug_state,
            "duration": duration,
        })
        if errors:
            evidence["errors"] = errors
            exit_code = 1
        else:
            exit_code = 0
    except Exception as exc:  # noqa: BLE001
        evidence["exception"] = repr(exc)
        exit_code = 1
    finally:
        h.evidence["mixed_load"] = evidence
        h.capture_logs()
        if not args.keep:
            try:
                h.teardown()
            except Exception as exc:  # noqa: BLE001
                evidence["cleanup_error"] = repr(exc)
                exit_code = 1
        try:
            h.write_results(exit_code)
        finally:
            try:
                h.log_fh.close()
            except Exception:  # noqa: BLE001
                pass
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
