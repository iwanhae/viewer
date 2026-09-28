"""Protocol tests for the external WebP encoder worker."""

from __future__ import annotations

import hashlib
from io import BytesIO
import json
from pathlib import Path
import unittest
from unittest.mock import patch

import httpx

from encoder.cli import EncoderClient, _process_one


class EncoderProtocolTests(unittest.TestCase):
    def test_output_posts_raw_body_with_bearer_auth_and_lease_headers(self):
        requests = []

        def handle(request: httpx.Request) -> httpx.Response:
            requests.append(request)
            return httpx.Response(202, request=request)

        api = EncoderClient("https://viewer.test", "worker-secret", timeout=5)
        api._client.close()
        api._client = httpx.Client(
            base_url="https://viewer.test",
            headers={"Authorization": "Bearer worker-secret"},
            follow_redirects=False,
            transport=httpx.MockTransport(handle),
        )
        job = {"hash": "abc123", "token": "lease-secret"}
        payload = b"RIFF\x00\x00\x00\x00WEBPencoded-image"
        try:
            response = api.upload_output(job, BytesIO(payload))
        finally:
            api._client.close()

        self.assertEqual(response.status_code, 202)
        self.assertEqual(len(requests), 1)
        request = requests[0]
        self.assertEqual(request.method, "POST")
        self.assertEqual(request.url.path, "/api/encoding/output")
        self.assertEqual(request.headers["authorization"], "Bearer worker-secret")
        self.assertEqual(request.headers["content-type"], "image/webp")
        self.assertEqual(request.headers["x-encoding-hash"], "abc123")
        self.assertEqual(request.headers["x-encoding-token"], "lease-secret")
        self.assertEqual(request.content, payload)

    def test_claim_and_renew_do_not_use_legacy_put_urls(self):
        requests = []

        def handle(request: httpx.Request) -> httpx.Response:
            requests.append(request)
            if request.url.path.endswith("/claim"):
                return httpx.Response(200, json={"claimed": [{
                    "hash": "abc123",
                    "token": "lease-secret",
                    "getUrl": "https://storage.test/source",
                    "putUrl": "https://legacy-put.test/ignored",
                }]}, request=request)
            return httpx.Response(200, json={
                "leaseUntil": "2099-01-01T00:00:00Z",
                "putUrl": "https://legacy-put.test/also-ignored",
            }, request=request)

        api = EncoderClient("https://viewer.test", "worker-secret", timeout=5)
        api._client.close()
        api._client = httpx.Client(
            base_url="https://viewer.test",
            headers={"Authorization": "Bearer worker-secret"},
            follow_redirects=False,
            transport=httpx.MockTransport(handle),
        )
        try:
            jobs = api.claim(1)
            renewed = api.renew(jobs[0])
        finally:
            api._client.close()

        self.assertIn("putUrl", jobs[0])  # A mixed rollout may include it; the worker ignores it.
        self.assertIsNone(renewed)
        self.assertEqual([request.url.path for request in requests], [
            "/api/encoding/claim", "/api/encoding/renew",
        ])
        self.assertEqual(json.loads(requests[0].read()), {"limit": 1, "outputMode": "api"})
        self.assertEqual(json.loads(requests[1].read()), {
            "hash": "abc123", "token": "lease-secret", "outputMode": "api",
        })

    def _run_upload(self, response_status: int) -> tuple[str, list[bytes], list[tuple]]:
        source_data = b"\xff\xd8\xff" + b"source-image" * 8
        encoded_data = b"RIFF\x00\x00\x00\x00WEBPcompressed"
        job = {
            "hash": hashlib.sha256(source_data).hexdigest(),
            "token": "lease-secret",
            "getUrl": "https://storage.test/source",
            "putUrl": "https://legacy-put.test/must-not-be-used",
        }

        class FakeTransfer:
            def __init__(self, *args, **kwargs):
                pass

            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def put(self, *args, **kwargs):
                raise AssertionError("encoder must not PUT to the legacy presigned URL")

        class FakeAPI:
            def __init__(self):
                self.uploaded = []
                self.completions = []

            def upload_output(self, upload_job, body):
                self.uploaded.append(body.read())
                return httpx.Response(response_status)

            def complete(self, complete_job, outcome, error=""):
                self.completions.append((complete_job, outcome, error))

        def download(_client, _url, path: Path) -> int:
            path.write_bytes(source_data)
            return len(source_data)

        def encode(args, **kwargs):
            Path(args[-1]).write_bytes(encoded_data)

        api = FakeAPI()
        with patch("encoder.cli.httpx.Client", FakeTransfer), \
                patch("encoder.cli._download", side_effect=download), \
                patch("encoder.cli.subprocess.run", side_effect=encode):
            with self.assertLogs("encoder.cli", level="INFO") as captured:
                outcome = _process_one(api, job, timeout=5)

        if response_status == 202:
            self.assertTrue(any("accepted for async validation" in line for line in captured.output))
            self.assertFalse(any("saved" in line.lower() for line in captured.output))
        return outcome, api.uploaded, api.completions

    def test_202_is_counted_as_accepted_not_done(self):
        outcome, uploaded, completions = self._run_upload(202)

        self.assertEqual(outcome, "accepted")
        self.assertEqual(uploaded, [b"RIFF\x00\x00\x00\x00WEBPcompressed"])
        self.assertEqual(completions, [])

    def test_non_202_success_is_not_counted_as_accepted(self):
        outcome, uploaded, completions = self._run_upload(200)

        self.assertEqual(outcome, "retry")
        self.assertEqual(uploaded, [b"RIFF\x00\x00\x00\x00WEBPcompressed"])
        self.assertEqual(completions, [])


if __name__ == "__main__":
    unittest.main()
