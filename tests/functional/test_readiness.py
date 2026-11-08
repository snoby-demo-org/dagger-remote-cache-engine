import unittest
from test_run import _bare_harness

class ReadinessTests(unittest.TestCase):
    def test_publication_waits_for_idle_and_stable_counter(self):
        h=_bare_harness()
        states=iter([(5,1),(5,1),(8,0),(8,0),(8,0)])
        calls=[]
        def count():
            try:n,active=next(states)
            except StopIteration:n,active=8,0
            calls.append((n,active))
            return n,{"exporting":active,"stats":{"published":n}}
        h.published_count=count
        n=h.wait_published(timeout=1,interval=0.01,settle_polls=2)
        self.assertEqual(n,8)
        self.assertGreaterEqual(len(calls),4)
        self.assertEqual(h.evidence["debug_state"]["exporting"],0)

    def test_warm_merge_requires_a_new_completed_merge(self):
        h=_bare_harness()
        calls=[]
        def state(path):
            self.assertEqual(path,"/debug/state")
            calls.append(path)
            return {"stats":{"merges":2 if len(calls)<3 else 3}}
        h._authed_get=state
        n=h.wait_warm_merge(baseline=2,timeout=1,interval=0.01)
        self.assertEqual(n,3)
        self.assertGreaterEqual(len(calls),3)
