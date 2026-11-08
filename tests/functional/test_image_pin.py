import inspect
import unittest
import run as fixture


class TestImmutableWorkload(unittest.TestCase):
    def test_real_query_uses_digest_addressed_image(self):
        # Dagger's fromSessionScopeInput scopes mutable tags per session.
        # Independent sessions must use an immutable reference for this test.
        self.assertRegex(fixture.WORKLOAD_IMAGE, r'@sha256:[0-9a-f]{64}$')
        self.assertIn('WORKLOAD_IMAGE', inspect.getsource(fixture.Harness.run_query))


class TestS3SignedHeaders(unittest.TestCase):
    def test_empty_get_sends_signed_content_type(self):
        from unittest.mock import patch, MagicMock
        response = MagicMock()
        response.__enter__.return_value.status = 200
        response.__enter__.return_value.read.return_value = b''
        with patch.object(fixture.urllib.request, 'urlopen', return_value=response) as send:
            fixture.S3Client('http://localhost:9000')._request('GET', '/bucket')
        request = send.call_args.args[0]
        self.assertEqual(request.get_header('Content-type'), 'application/octet-stream')


if __name__ == '__main__':
    unittest.main()
