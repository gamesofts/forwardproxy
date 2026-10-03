import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch
import subprocess
import base64
import os
import tempfile

spec = importlib.util.spec_from_file_location(
    "detect", Path(__file__).parents[1] / "scripts/detect-caddy-release.py"
)
detect = importlib.util.module_from_spec(spec)
spec.loader.exec_module(detect)


class ReleaseDetectionTests(unittest.TestCase):
    def setUp(self):
        self.upstream = {"tag_name": "v2.11.7", "draft": False, "prerelease": False}

    def test_new_stable_version(self):
        result = detect.select_release(self.upstream, None)
        self.assertEqual(result["tag"], "caddy-v2.11.7")
        self.assertEqual(result["should_build"], "true")
        self.assertEqual(result["release_exists"], "false")

    def test_complete_release_is_not_rebuilt(self):
        existing = {"draft": False, "assets": [
            {"name": "caddy-v2.11.7-linux-amd64.tar.gz"}, {"name": "SHA256SUMS"}
        ]}
        self.assertEqual(detect.select_release(self.upstream, existing)["should_build"], "false")

    def test_partial_draft_is_retried(self):
        result = detect.select_release(self.upstream, {"draft": True, "assets": []})
        self.assertEqual(result["should_build"], "true")
        self.assertEqual(result["release_exists"], "true")

    def test_incomplete_public_release_is_not_overwritten(self):
        with self.assertRaises(ValueError):
            detect.select_release(self.upstream, {"draft": False, "assets": []})

    def test_prerelease_draft_and_invalid_tags_are_rejected(self):
        for change in ({"prerelease": True}, {"draft": True}, {"tag_name": "v2.12.0-beta.1"}, {"tag_name": "v2.11.7\nshould_build=false"}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                detect.select_release({**self.upstream, **change}, None)

    def test_not_found_is_distinct_from_permission_failure(self):
        with patch.object(detect.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "gh: Not Found (HTTP 404)")):
            self.assertIsNone(detect.gh_api("repos/example/release"))
        with patch.object(detect.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "gh: Forbidden (HTTP 403)")):
            with self.assertRaises(RuntimeError):
                detect.gh_api("repos/example/release")

    def test_detection_pins_draft_source_and_required_go_family(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "outputs"
            summary = Path(directory) / "summary"
            module = {"content": base64.b64encode(b"module caddy\n\ngo 1.26.0\n").decode()}
            existing = {"draft": True, "target_commitish": "b" * 40, "assets": []}
            env = {"REQUESTED_VERSION": "", "SOURCE_SHA": "a" * 40, "GITHUB_EVENT_NAME": "schedule",
                   "GITHUB_REPOSITORY": "gamesofts/forwardproxy",
                   "GITHUB_OUTPUT": str(output), "GITHUB_STEP_SUMMARY": str(summary)}
            with patch.dict(os.environ, env), patch.object(detect, "gh_api", side_effect=[self.upstream, existing, module]):
                detect.main()
            text = output.read_text()
            self.assertIn("source_sha=" + "b" * 40, text)
            self.assertIn("go_version=1.26.x", text)
            self.assertIn("make_latest=true", text)

    def test_invalid_version_input_does_not_call_api(self):
        with patch.dict(os.environ, {"REQUESTED_VERSION": "v2.11.7; touch file"}), patch.object(detect, "gh_api") as api:
            with self.assertRaises(ValueError):
                detect.main()
            api.assert_not_called()


if __name__ == "__main__":
    unittest.main()
