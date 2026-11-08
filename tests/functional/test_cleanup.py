import subprocess
import unittest
from types import SimpleNamespace
from unittest.mock import Mock
import run as fixture

class CleanupVerification(unittest.TestCase):
    def make(self, docker):
        h=object.__new__(fixture.Harness)
        h.prefix='dcache-ft-123456-test00'
        h.names={'engine':h.prefix+'engine','net':h.prefix+'net'}
        h.evidence={'containers':{'engine':{'name':h.names['engine']}},'volumes':[h.prefix+'engine-dagger']}
        h.capture_logs=Mock();h.hit=Mock();h.log=Mock()
        h.args=SimpleNamespace(output='/owned');h.state_dir='/not-owned'
        h.docker=docker
        return h

    def test_nonzero_removal_fails_cleanup(self):
        h=self.make(lambda args,**kwargs: subprocess.CompletedProcess(args,1,b'',b'permission denied'))
        with self.assertRaises(RuntimeError):h.teardown()

    def test_readback_that_still_exists_fails_cleanup(self):
        h=self.make(lambda args,**kwargs: subprocess.CompletedProcess(args,0,b'[]',b''))
        with self.assertRaises(RuntimeError):h.teardown()

    def test_named_volumes_are_removed_and_absence_checked(self):
        commands=[]
        def docker(args,**kwargs):
            commands.append(args)
            if 'inspect' in args:return subprocess.CompletedProcess(args,1,b'',b'No such object or network not found')
            return subprocess.CompletedProcess(args,0,b'',b'')
        h=self.make(docker);h.teardown()
        self.assertIn(['volume','rm',h.prefix+'engine-dagger'],commands)
        self.assertIn(['volume','inspect',h.prefix+'engine-dagger'],commands)
        self.assertIn(['network','inspect',h.prefix+'net'],commands)

    def test_run_reports_cleanup_failure_before_results(self):
        h=self.make(Mock());h.args.scenario='reuse';h.args.keep=False
        h.stages=[('proof',True,'')];h.setup=Mock();h.scenario_reuse=Mock(return_value=True)
        h.teardown=Mock(side_effect=RuntimeError('cleanup failed'))
        h.write_results=Mock()
        self.assertEqual(h.run(),1)
        h.write_results.assert_called_once_with(1)
        self.assertIn('cleanup_error',h.evidence)
