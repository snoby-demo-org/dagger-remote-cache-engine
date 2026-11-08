import unittest
from unittest.mock import Mock, patch
import run as fixture

class CorruptionScenario(unittest.TestCase):
    def test_all_scenario_corrupts_then_uses_fresh_consumer(self):
        h=object.__new__(fixture.Harness)
        h.scenario_reuse=Mock(return_value=True)
        h.container_ip=Mock(return_value='127.0.0.1')
        for name in ('log','stage','wait_published','discard_engine','start_engine','wait_warm_merge'):
            setattr(h,name,Mock())
        h.merge_count=Mock(return_value=0)
        h.hit=Mock();h.hit.count.side_effect=[2,3]
        h.hit_url='http://counter/hit?job=shared'
        h.run_query=Mock(side_effect=[('cache-result-shared',1,{}),('cache-result-shared',1,{})])
        h.evidence={'counters':{},'timings':{}}
        s3=Mock();s3.list.side_effect=[['blob'],[],['blob']]
        with patch.object(fixture,'S3Client',return_value=s3):
            self.assertTrue(h.scenario_all())
        self.assertTrue(s3.put.called,'no corruption injected')
        self.assertIn(unittest.mock.call('engineF'),h.start_engine.call_args_list)
        self.assertEqual(h.evidence['counters']['S3-corrupt-fallback'],3)
