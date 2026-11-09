"""Offline checks for the published setup examples and documentation index."""
import re
import unittest
from pathlib import Path
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[2]


def env_sample(name):
    return dict(line.split("=", 1) for line in
                (ROOT / "examples" / name).read_text().splitlines()
                if line and not line.startswith("#"))


class DocumentationTests(unittest.TestCase):
    def test_engine_configuration_uses_service_base_url(self):
        sample = env_sample("engine.env.example")
        self.assertEqual(sample["_EXPERIMENTAL_DAGGER_REMOTE_CACHE_URL"],
                         "http://dcache:8080")
        self.assertNotIn("/v1/engine", sample["_EXPERIMENTAL_DAGGER_REMOTE_CACHE_URL"])
        self.assertNotIn("ws://dcache:8080/v1/engine", (ROOT / "README.md").read_text())

    def test_engine_sample_does_not_receive_server_credentials(self):
        engine = env_sample("engine.env.example")
        server = env_sample("coordinator.env.example")
        self.assertEqual(engine["DAGGER_CLOUD_TOKEN"], "")
        for key in ("DCACHE_TOKEN", "DCACHE_HMAC_SECRET", "S3_ACCESS_KEY", "S3_SECRET_KEY"):
            self.assertNotIn(key, engine)
            self.assertEqual(server[key], "")
        self.assertFalse(any(key.startswith("S3_") for key in engine))

    def test_documentation_inventory_and_local_links(self):
        required = ["docs/README.md", "docs/design.md", "docs/s3.md", "docs/engines.md",
                    "docs/testing.md", "docs/history/README.md"]
        for name in required:
            self.assertTrue((ROOT / name).is_file(), name)
        inventory = (ROOT / "docs/testing.md").read_text()
        tests = list((ROOT / "cmd").rglob("*_test.go")) + list((ROOT / "internal").rglob("*_test.go"))
        tests += list((ROOT / "tests").rglob("test_*.py"))
        for path in tests:
            self.assertIn(str(path.relative_to(ROOT)), inventory)
        docs = [ROOT / n for n in ("README.md", "VERIFICATION.md", "CONTRACTS.md")]
        docs += list((ROOT / "docs").glob("*.md")) + [ROOT / "docs/history/README.md"]
        for path in docs:
            for target in re.findall(r"(?<!!)\[[^\]]+\]\(([^\s)]+)\)", path.read_text()):
                url = urlsplit(target)
                if url.scheme or url.netloc or not url.path:
                    continue
                self.assertNotIn(".upstream", Path(url.path).parts,
                                 "Do not publish links to the untracked upstream checkout")
                self.assertTrue((path.parent / unquote(url.path)).exists(),
                                "%s links to missing %s" % (path.relative_to(ROOT), target))
