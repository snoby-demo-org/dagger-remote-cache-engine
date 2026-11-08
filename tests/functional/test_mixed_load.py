import unittest
import mixed_load

class MixedLoadHelpers(unittest.TestCase):
    def test_overlap_is_measured_from_real_query_intervals(self):
        serial=[{'query_start':i*2,'query_end':i*2+1} for i in range(4)]
        parallel=[{'query_start':i/10,'query_end':2+i/10} for i in range(4)]
        self.assertEqual(mixed_load.peak_query_overlap(serial),1)
        self.assertEqual(mixed_load.peak_query_overlap(parallel),4)

    def test_cli_supplies_harness_scenario(self):
        args=mixed_load.parse_args(['--binary','/fake','--output','/fake','--engine-image','fake'])
        self.assertEqual(args.scenario,'mixed-load')

    def test_job_plan_has_shared_and_unique_work(self):
        for n in (4,96):
            jobs=mixed_load.job_plan(n)
            self.assertEqual(len(jobs),n)
            self.assertEqual(jobs.count("shared"),max(1,n//4))
            unique=[j for j in jobs if j!="shared"]
            self.assertEqual(len(unique),len(set(unique)))
            self.assertTrue(unique)
        for n in (-1,0,1):
            with self.assertRaises(ValueError):mixed_load.job_plan(n)

    def test_result_checker_rejects_recompute_missing_and_duplicates(self):
        jobs=mixed_load.job_plan(4)
        rows=[{"worker":"worker%d"%i,"job":j,"output":"cache-result-"+j,"error":None} for i,j in enumerate(jobs)]
        counters={j:1 for j in jobs}
        self.assertEqual(mixed_load.check_results(jobs,rows,counters),[])
        bad=dict(counters);bad["shared"]=2
        self.assertTrue(mixed_load.check_results(jobs,rows,bad))
        self.assertTrue(mixed_load.check_results(jobs,rows[:-1],counters))
        self.assertTrue(mixed_load.check_results(jobs,rows+[rows[0]],counters))
        wrong=[dict(r) for r in rows];wrong[0]["output"]="wrong"
        self.assertTrue(mixed_load.check_results(jobs,wrong,counters))

    def test_memory_guard_accounts_for_engines_and_headroom(self):
        per=256*1024*1024
        self.assertEqual(mixed_load.memory_required_bytes(4,per),(4+1)*per+2*1024**3)
        self.assertGreater(mixed_load.memory_required_bytes(96,per),mixed_load.memory_required_bytes(4,per))
