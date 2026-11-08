#!/usr/bin/env python3
"""RED/GREEN unit tests for tests/functional/run.py helpers.

These tests exercise ONLY the pure helper layer of the functional harness
(name ownership, query construction, output parsing, bounded waiting) using
subprocess mocking. They are NOT functional evidence; the real harness is
run.py executed against ethpow with the actual service binary.

Run:  python3 -m unittest tests.functional.test_run -v
"""

import io
import json
import os
import re
import sys
import unittest
from unittest import mock

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__))))

import run as harness  # noqa: E402


def _fake_docker(h, exec_stdout=b""):
    """Docker mock keyed by the harness's real network/container names."""
    def side(args, **kw):
        joined = " ".join(args)
        if joined.startswith("network inspect"):
            return mock.Mock(returncode=0, stdout=json.dumps(
                [{"IPAM": {"Config": [{"Gateway": "172.19.0.1"}]}}]
            ).encode(), stderr=b"")
        if joined.startswith("inspect"):
            net = h.names.get("net", "net")
            return mock.Mock(returncode=0, stdout=json.dumps(
                [{"NetworkSettings": {"Networks": {
                    net: {"IPAddress": "172.19.0.5"}}}}]
            ).encode(), stderr=b"")
        if args and args[0] == "exec":
            return mock.Mock(returncode=0, stdout=exec_stdout, stderr=b"")
        return mock.Mock(returncode=0, stdout=b"", stderr=b"")
    return side


def _config_docker(h):
    """Wire the mocked docker callable's .json/.out accessors to match the
    real helper: json(args) -> decoded JSON, out(args) -> stdout text. Both
    delegate to h.docker(...) so call recording and _fake_docker dispatch
    stay intact."""
    h.docker.json.side_effect = lambda args, **kw: json.loads(
        h.docker(args, **kw).stdout.decode())
    h.docker.out.side_effect = lambda args, **kw: h.docker(
        args, **kw).stdout.decode()


def _bare_harness(scenario="reuse"):
    h = harness.Harness.__new__(harness.Harness)
    h.args = mock.Mock(scenario=scenario, output="/tmp/x", binary="b",
                       engine_image="e", keep=False)
    h.prefix = harness.make_prefix()
    h.names = {}
    h.stages = []
    h.evidence = {"containers": {}, "counters": {}, "timings": {}}
    # Deterministic fixture path: only ever appears inside mocked Docker
    # mount arguments; these tests never create or read it on the real
    # filesystem.
    h.state_dir = "/tmp/dcache-ft-fixture-state"
    h.log_fh = io.StringIO()
    h.hit = mock.Mock()
    h.hit.start.return_value = 12345
    return h


def _patch_urlopen(testcase, h):
    """Record every URL probed via urllib and answer 200 (no real network)."""
    h.urlopen_calls = []

    def fake_urlopen(url, *a, **k):
        h.urlopen_calls.append(url)
        resp = mock.MagicMock()
        resp.status = 200
        resp.read.return_value = b"{}"
        resp.__enter__.return_value = resp
        return resp

    p = mock.patch("urllib.request.urlopen", fake_urlopen)
    p.start()
    testcase.addCleanup(p.stop)


class TestNameOwnership(unittest.TestCase):
    """Container/network name ownership and cleanup safeguards."""

    def test_prefix_is_unique_and_stable_per_run(self):
        a = harness.make_prefix()
        b = harness.make_prefix()
        self.assertTrue(a.startswith("dcache-ft-"))
        self.assertNotEqual(a, b)
        self.assertLessEqual(len(a), 63)

    def test_owned_names_carry_prefix(self):
        prefix = harness.make_prefix()
        self.assertTrue(harness.owned(prefix, prefix + "-minio"))
        self.assertTrue(harness.owned(prefix, prefix + "coord"))

    def test_unowned_names_rejected(self):
        prefix = harness.make_prefix()
        self.assertFalse(harness.owned(prefix, "dcache"))
        self.assertFalse(harness.owned(prefix, "dcache-other-abc-coord"))
        self.assertFalse(harness.owned(prefix, ""))
        self.assertFalse(harness.owned(prefix, "minio"))

    def test_cleanup_only_touches_owned_names(self):
        prefix = harness.make_prefix()
        docker = mock.Mock()
        docker.side_effect = lambda args, **kw: mock.Mock(
            returncode=0, stdout="", stderr=""
        )
        harness.cleanup_owned(docker, prefix, ["dcache", "dcache-ft-evil"])
        for call in docker.call_args_list:
            args = call[0][0]
            self.assertIn(prefix, " ".join(args), "cleanup touched a non-owned name")

    def test_cleanup_never_global_prune(self):
        prefix = harness.make_prefix()
        docker = mock.Mock(returncode=0, stdout="", stderr="")
        harness.cleanup_owned(lambda a, **k: docker, prefix, [])
        for call in docker.call_args_list:
            args = call[0][0]
            joined = " ".join(args)
            self.assertNotIn("prune", joined)
            self.assertNotIn("rm -f $(docker", joined)

    def test_no_arbitrary_filesystem_deletion(self):
        with open(os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "run.py")) as f:
            src = f.read()
        self.assertNotIn("os.system(", src)
        self.assertNotIn("rm -rf /", src)
        # rmtree only permitted on the harness's own per-run state dir
        for m in re.finditer(r"shutil\.rmtree\(([^)]*)\)", src):
            self.assertIn("state_dir", m.group(1))


class TestQueryBuilding(unittest.TestCase):
    """GraphQL query construction with proper escaping."""

    def test_query_shape(self):
        q = harness.build_hit_query("alpine:3.23", "http://10.0.0.1:8000/hit?job=shared")
        self.assertIn("container", q)
        self.assertIn("from", q)
        self.assertIn("withExec", q)
        self.assertIn("stdout", q)

    def test_query_escapes_quotes_and_backslashes(self):
        q = harness.build_hit_query('al"pine\\3', "http://h/x?a=b&c=d")
        self.assertIn('\\"', q)
        self.assertIn("\\\\", q)
        # The raw unescaped quote must not appear inside a string literal.
        self.assertNotIn('"al"', q)

    def test_query_escapes_newlines(self):
        q = harness.build_hit_query("al\npine", "http://h/x")
        self.assertNotIn("al\npine", q)
        self.assertIn("\\n", q)

    def test_exec_command_present(self):
        q = harness.build_hit_query("alpine:3.23", "http://1.2.3.4:8000/hit?job=shared")
        self.assertIn("wget -qO-", q)
        self.assertIn("cache-result-shared", q)


class TestStdoutParse(unittest.TestCase):
    """Robust parsing of the final JSON object after progress lines."""

    def test_parse_plain_json(self):
        self.assertEqual(harness.parse_query_stdout('{"a": 1}'), {"a": 1})

    def test_parse_after_progress_lines(self):
        raw = (
            "✔ EngineConn 0.1s\n"
            "✔ load 0.2s\n"
            '{"container": {"from": {"withExec": {"stdout": "cache-result-shared"}}}}\n'
        )
        out = harness.parse_query_stdout(raw)
        self.assertEqual(
            out["container"]["from"]["withExec"]["stdout"], "cache-result-shared"
        )


class TestFixtureTokens(unittest.TestCase):
    """Blocker 1: cloud token must equal the coordinator fixture token,
    otherwise every real WS/telemetry request 401s."""

    def test_cloud_token_equals_coordinator_token(self):
        self.assertEqual(harness.FIXTURE_CLOUD_TOKEN, harness.FIXTURE_TOKEN)

    def test_cloud_token_is_fixture_value(self):
        self.assertEqual(harness.FIXTURE_CLOUD_TOKEN, "dck-functional-test-only")


class TestRunQueryStdin(unittest.TestCase):
    """Blocker 2: the query must be delivered on STDIN (docker exec -i +
    input_bytes=query.encode()), never as a positional `operation` argument."""

    RESP = (b'{"container":{"from":{"withExec":'
            b'{"stdout":"cache-result-shared"}}}}')

    def _make(self):
        h = _bare_harness()
        h.names = {"engineA": h.prefix + "engineA", "coord": h.prefix + "coord", "net": h.prefix + "net"}
        h.docker = mock.Mock()
        h.docker.side_effect = _fake_docker(h, exec_stdout=self.RESP)
        _config_docker(h)
        return h

    def test_query_passed_via_stdin_not_positional(self):
        h = self._make()
        out, _, _ = h.run_query("engineA", "http://1.2.3.4:8000/hit?job=shared")
        self.assertEqual(out, "cache-result-shared")
        args = h.docker.call_args[0][0]
        self.assertIn("exec", args)
        self.assertIn("-i", args)
        for a in args:
            self.assertNotIn("container", a, "query leaked into argv: %r" % (a,))
        payload = h.docker.call_args[1].get("input_bytes")
        self.assertIsNotNone(payload, "query must be passed as input_bytes")
        self.assertIn(b"container", payload)

    def test_stdin_payload_is_the_built_query(self):
        h = self._make()
        h.run_query("engineA", "http://1.2.3.4:8000/hit?job=shared", job="shared")
        payload = h.docker.call_args[1]["input_bytes"]
        self.assertEqual(payload.decode(), harness.build_hit_query(
            harness.WORKLOAD_IMAGE, "http://1.2.3.4:8000/hit?job=shared", job="shared"))

    def test_no_operation_flag(self):
        h = self._make()
        h.run_query("engineA", "http://1.2.3.4:8000/hit?job=shared")
        args = h.docker.call_args[0][0]
        self.assertNotIn("operation", args)


class TestS3ProviderRustfs(unittest.TestCase):
    """Blocker 3: S3 fixture is the isolated digest-pinned RustFS image
    (public MinIO images are unavailable), health at GET /health."""

    DIGEST = ("rustfs/rustfs@sha256:ed9210d6411d77ffceb70d94b964a3cb"
              "7d225983368bc25bebe01e995293dcc2")

    def _make(self):
        h = _bare_harness()
        h.docker = mock.Mock()
        h.docker.side_effect = _fake_docker(h)
        _config_docker(h)
        _patch_urlopen(self, h)
        return h

    def _run_args(self, h):
        runs = [c[0][0] for c in h.docker.call_args_list
                if c[0][0] and c[0][0][0] == "run"]
        self.assertTrue(runs, "no docker run issued")
        return runs

    def test_rustfs_image_digest_pinned(self):
        h = self._make()
        h.setup()
        runs = self._run_args(h)
        self.assertTrue(any(self.DIGEST in r for r in runs),
                        "rustfs digest image not used: %r" % (runs,))

    def test_no_minio_image(self):
        h = self._make()
        h.setup()
        for call in h.docker.call_args_list:
            joined = " ".join(call[0][0])
            self.assertNotIn("minio/minio", joined)
            self.assertNotIn("quay.io/minio", joined)

    def test_rustfs_env_user_and_empty_args(self):
        h = self._make()
        h.setup()
        target = None
        for r in self._run_args(h):
            if self.DIGEST in r:
                target = r
        self.assertIsNotNone(target)
        joined = " ".join(target)
        for env in ("RUSTFS_ACCESS_KEY=dcachetest",
                    "RUSTFS_SECRET_KEY=dcachetest-secret",
                    "RUSTFS_VOLUMES=/data",
                    "RUSTFS_ADDRESS=0.0.0.0:9000",
                    "RUSTFS_CONSOLE_ENABLE=false",
                    "RUSTFS_UNSAFE_BYPASS_DISK_CHECK=true"):
            self.assertIn(env, joined)
        self.assertIn("--user", target)
        self.assertEqual(target[target.index("--user") + 1], "0")
        # entrypoint handles /data: no positional container args
        self.assertEqual(target[-1], self.DIGEST)

    def test_provider_evidence_and_health(self):
        h = self._make()
        h.setup()
        self.assertEqual(h.evidence.get("s3_provider"), "rustfs")
        self.assertEqual(h.evidence.get("s3_image"), self.DIGEST)
        self.assertTrue(h.urlopen_calls,
                        "no health probe issued")
        self.assertTrue(any(u.endswith(":9000/health") for u in h.urlopen_calls),
                        "no /health probe: %r" % h.urlopen_calls)
        self.assertFalse(any("/minio/health" in u for u in h.urlopen_calls))

    def test_bucket_env_unchanged(self):
        h = self._make()
        h.setup()
        self.assertEqual(harness.FIXTURE_S3_BUCKET, "dcache-test")
        self.assertEqual(harness.FIXTURE_S3_USER, "dcachetest")
        self.assertEqual(harness.FIXTURE_S3_PASS, "dcachetest-secret")


class TestPeerRegistration(unittest.TestCase):
    """Blocker 4: B registration waits on authenticated /debug/state peers
    (the endpoint that actually exists), not a 404-prone /debug/peers poll."""

    def test_no_debug_peers_endpoint(self):
        with open(os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "run.py")) as f:
            src = f.read()
        self.assertNotIn("/debug/peers", src)

    def test_wait_for_peer_registration_uses_debug_state(self):
        h = _bare_harness()
        h.names = {"coord": h.prefix + "coord"}
        calls = {"n": 0}

        def fake_authed(path):
            calls["n"] += 1
            self.assertEqual(path, "/debug/state")
            if calls["n"] < 3:
                return {"Peers": {}}
            return {"Peers": {"peer-1": {"id": "peer-1"}}}

        h._authed_get = fake_authed
        n = h.wait_for_peer_registration(timeout=10, interval=0.05)
        self.assertEqual(n, 1)
        self.assertGreaterEqual(calls["n"], 3)

    def test_registration_timeout_raises(self):
        h = _bare_harness()
        h.names = {"coord": h.prefix + "coord"}
        h._authed_get = lambda path: {"Peers": {}}
        with self.assertRaises(Exception):
            h.wait_for_peer_registration(timeout=0.3, interval=0.05)

    def test_peers_count_variants(self):
        h = _bare_harness()
        h.names = {"coord": h.prefix + "coord"}
        h._authed_get = lambda path: {"peers": ["a", "b"]}
        n, _ = h.peers_count()
        self.assertEqual(n, 2)
        h._authed_get = lambda path: {"Stats": {"Peers": 3}}
        n, _ = h.peers_count()
        self.assertEqual(n, 3)


class TestEvidenceAndExitCode(unittest.TestCase):
    """Blocker 5: logs captured BEFORE write_results (so saved JSON carries
    logs); exit nonzero whenever the scenario returns False even if every
    stage string says PASS."""

    def _make(self):
        h = _bare_harness()
        h.capture_logs = mock.Mock()
        h.write_results = mock.Mock()
        h.teardown = mock.Mock()
        return h

    def test_run_captures_logs_before_writing_results(self):
        h = self._make()
        order = []
        h.capture_logs.side_effect = lambda: order.append("capture_logs")
        h.write_results.side_effect = lambda code: order.append("write_results")
        h.setup = lambda: order.append("setup")
        h.scenario_reuse = lambda: order.append("scenario") or False
        code = h.run()
        self.assertEqual(code, 1, "scenario False must yield exit code 1")
        self.assertIn("capture_logs", order)
        self.assertLess(order.index("capture_logs"),
                        order.index("write_results"),
                        "logs must be captured before results are written")

    def test_exit_nonzero_when_scenario_false_despite_passing_stages(self):
        h = self._make()
        h.setup = lambda: None
        # scenario returns False but (accidentally) records all-pass stages
        h.scenario_reuse = lambda: (h.stage("A", True, ""), False)[1]
        self.assertEqual(h.run(), 1)
        self.assertTrue(harness.summary_ok(h.stages),
                        "precondition: stages all pass")

    def test_exit_zero_when_scenario_true(self):
        h = self._make()
        h.setup = lambda: None
        h.scenario_reuse = lambda: (h.stage("A", True, ""), True)[1]
        self.assertEqual(h.run(), 0)


class TestCoordinatorEntrypoint(unittest.TestCase):
    """Blocker 6: coordinator entrypoint is just /dcache (CLI is env-driven),
    no 'server' argument."""

    def test_no_server_argument(self):
        with open(os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "run.py")) as f:
            src = f.read()
        self.assertNotIn('"/dcache", "server"', src)
        self.assertNotIn("'/dcache', 'server'", src)
