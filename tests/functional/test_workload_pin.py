import re
import unittest
import run

class WorkloadPinTests(unittest.TestCase):
    def test_workload_image_is_immutable(self):
        self.assertRegex(run.WORKLOAD_IMAGE,r"@sha256:[0-9a-f]{64}$")
        query=run.build_hit_query(run.WORKLOAD_IMAGE,"http://counter/hit?job=shared")
        self.assertIn(run.WORKLOAD_IMAGE,query)
