import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).parents[1] / 'scripts/publish-caddy-release.sh'


class ReleasePublicationTests(unittest.TestCase):
    def invoke(self, *, existing=False, draft=True, fail_upload=False, latest=True):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fake = root / 'gh'
            fake.write_text('''#!/usr/bin/env python3
import json, os, sys
with open(os.environ['CALL_LOG'], 'a') as log:
    log.write(json.dumps(sys.argv[1:]) + '\\n')
if sys.argv[1] == 'api':
    # Reproduce GitHub's actual behavior: drafts cannot be fetched by tag.
    if '/releases/tags/' in sys.argv[2]:
        print('gh: Not Found (HTTP 404)', file=sys.stderr)
        sys.exit(1)
    print(os.environ['IS_DRAFT'])
if sys.argv[1:3] == ['release', 'view']:
    print('https://api.github.com/repos/gamesofts/forwardproxy/releases/402428409')
if sys.argv[1:3] == ['release', 'upload'] and os.environ['FAIL_UPLOAD'] == 'true':
    sys.exit(1)
''')
            fake.chmod(0o755)
            env = {**os.environ,
                   'PATH': str(root) + ':' + os.environ['PATH'],
                   'GITHUB_REPOSITORY': 'gamesofts/forwardproxy',
                   'RELEASE_TAG': 'caddy-v2.11.7',
                   'CADDY_VERSION': 'v2.11.7',
                   'SOURCE_SHA': 'a' * 40,
                   'ARCHIVE': 'caddy-v2.11.7-linux-amd64.tar.gz',
                   'RELEASE_EXISTS': str(existing).lower(),
                   'MAKE_LATEST': str(latest).lower(),
                   'IS_DRAFT': str(draft).lower(),
                   'FAIL_UPLOAD': str(fail_upload).lower(),
                   'CALL_LOG': str(root / 'calls')}
            result = subprocess.run(['bash', str(SCRIPT.resolve())], env=env, cwd=root, capture_output=True, text=True)
            calls = [json.loads(line) for line in (root / 'calls').read_text().splitlines()]
            return result.returncode, calls

    def test_draft_upload_then_publish(self):
        code, calls = self.invoke()
        self.assertEqual(code, 0)
        self.assertEqual([c[:2] for c in calls if c[0] == 'release'],
                         [['release', 'create'], ['release', 'view'], ['release', 'upload'], ['release', 'edit']])
        self.assertIn('--draft', calls[0])
        upload = next(c for c in calls if c[:2] == ['release', 'upload'])
        self.assertIn('dist/SHA256SUMS', upload)
        self.assertTrue(any(c[:2] == ['api', 'repos/gamesofts/forwardproxy/releases/402428409'] for c in calls))
        self.assertIn('--draft=false', calls[-1])

    def test_failed_upload_never_publishes(self):
        code, calls = self.invoke(fail_upload=True)
        self.assertNotEqual(code, 0)
        self.assertFalse(any(c[:2] == ['release', 'edit'] for c in calls))

    def test_existing_draft_is_resumed(self):
        code, calls = self.invoke(existing=True)
        self.assertEqual(code, 0)
        self.assertFalse(any(c[:2] == ['release', 'create'] for c in calls))

    def test_published_release_cannot_be_overwritten(self):
        code, calls = self.invoke(existing=True, draft=False)
        self.assertNotEqual(code, 0)
        self.assertFalse(any(c[:2] in [['release', 'upload'], ['release', 'edit']] for c in calls))

    def test_backfill_does_not_replace_latest(self):
        code, calls = self.invoke(latest=False)
        self.assertEqual(code, 0)
        self.assertIn('--latest=false', calls[-1])


if __name__ == '__main__':
    unittest.main()
