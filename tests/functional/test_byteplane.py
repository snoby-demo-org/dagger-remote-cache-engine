import json
import re
import unittest

import run as harness


ARGS_RE = re.compile(r'withExec\(args:\s*(\[[^\]]*\])')


class ByteplaneTests(unittest.TestCase):
    def _exec_args(self, query):
        match = ARGS_RE.search(query)
        self.assertIsNotNone(match, f"no withExec(args: ...) in query: {query!r}")
        return json.loads(match.group(1))

    def test_build_hit_query_uses_shell_command(self):
        query = harness.build_hit_query(
            "alpine:3.23",
            "http://counter/hit?job=shared",
        )
        args = self._exec_args(query)
        self.assertEqual(args[:2], ["sh", "-c"])
        command = args[2]
        self.assertTrue(command.startswith("set -eu"), command)
        self.assertIn("/dcache-result", command)
        self.assertIn("wget", command)

    def test_read_nonce_keeps_producer_args_and_selects_consumer_file(self):
        image = "alpine:3.23"
        url = "http://counter/hit?job=shared"

        default_query = harness.build_hit_query(image, url)
        engine_b_query = harness.build_hit_query(image, url, read_nonce="engineB")
        engine_e_query = harness.build_hit_query(image, url, read_nonce="engineE")

        default_args = self._exec_args(default_query)
        engine_b_args = self._exec_args(engine_b_query)
        engine_e_args = self._exec_args(engine_e_query)

        self.assertEqual(default_args, engine_b_args)
        self.assertEqual(default_args, engine_e_args)

        file_match = re.search(r'file\(path:\s*"/dcache-result"\)', engine_b_query)
        name_match = re.search(r'withName\(name:\s*"engineB"\)', engine_b_query)
        contents_match = re.search(r'\bcontents\b', engine_b_query)
        self.assertIsNotNone(file_match, engine_b_query)
        self.assertIsNotNone(name_match, engine_b_query)
        self.assertIsNotNone(contents_match, engine_b_query)
        self.assertLess(file_match.start(), name_match.start())
        self.assertLess(name_match.start(), contents_match.start())

        self.assertRegex(engine_e_query, r'withName\(name:\s*"engineE"\)')
        self.assertNotEqual(engine_b_query, engine_e_query)
        self.assertNotIn("engineE", engine_b_query)
        self.assertNotIn("engineB", engine_e_query)

    def test_extract_stdout_from_nested_contents(self):
        fixture = {
            "container": {
                "from": {
                    "withExec": {
                        "file": {
                            "withName": {
                                "contents": "cache-result-shared",
                            }
                        }
                    }
                }
            }
        }
        self.assertEqual(harness.extract_stdout(fixture), "cache-result-shared")


if __name__ == "__main__":
    unittest.main()
