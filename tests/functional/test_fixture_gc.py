import unittest
from unittest.mock import Mock
from types import SimpleNamespace
import run as fixture

class EngineFixtureGC(unittest.TestCase):
    def test_fixture_gc_thresholds_are_explicit_and_bounded(self):
        h=object.__new__(fixture.Harness)
        h.args=SimpleNamespace(engine_image='fixture-engine')
        h.prefix='fixture-';h.names={'worker':'fixture-worker'}
        h.container_ip=Mock(return_value='127.0.0.1')
        h.run_container=Mock();h.wait_engine_ready=Mock();h.evidence={}
        h.start_engine('worker')
        self.assertIn('--oci-worker-gc-keepstorage=100,0,1000',h.run_container.call_args.args[2])
