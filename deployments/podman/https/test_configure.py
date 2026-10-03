"""Credential lifecycle checks; never print generated credentials."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import yaml

spec = importlib.util.spec_from_file_location('browser_setup', Path(__file__).with_name('configure.py'))
setup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(setup)


class BrowserSetupTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.home = Path(self.directory.name)
        self.config = self.home / '.config/vornik/config.yaml'
        self.config.parent.mkdir(parents=True)
        self.config.write_text('server:\n  address: "0.0.0.0:8080"\napi:\n  auth_enabled: false\nmetrics:\n  enabled: false\n')
        self.root = self.config.parent / 'secrets'
        self.calls = []

    def run_setup(self, *arguments, validation_ok=True):
        def run(command, **kwargs):
            self.calls.append(command)
            code = 1 if 'validate' in command and not validation_ok else 0
            return subprocess.CompletedProcess(command, code, stdout='$2a$testhash\n', stderr='')
        output = io.StringIO()
        with patch.object(setup.Path, 'home', return_value=self.home), \
                patch('sys.argv', ['configure.py', '--hostname', 'example.test', *arguments]), \
                patch.object(setup.subprocess, 'run', side_effect=run), \
                contextlib.redirect_stdout(output):
            setup.main()
        return output.getvalue()

    def test_fresh_install_then_rerun_preserves_credentials(self):
        output = self.run_setup()
        before = (self.root / 'browser.json').read_text()
        bundle = json.loads(before)
        self.assertTrue(bundle['password'] not in output and bundle['api_key'] not in output)
        self.assertEqual((self.root / 'browser.json').stat().st_mode & 0o777, 0o600)
        self.assertEqual((self.root / 'api.env').stat().st_mode & 0o777, 0o600)
        api = yaml.safe_load(self.config.read_text())['api']
        self.assertTrue(api['auth_enabled'])
        self.assertIn('${VORNIK_API_KEY}', api['api_keys'])
        self.assertTrue(any(command[:3] == ['systemctl', '--user', 'restart'] for command in self.calls))
        self.calls.clear()
        self.run_setup()
        self.assertTrue(before == (self.root / 'browser.json').read_text())
        self.assertFalse(any(command[0] == 'systemctl' for command in self.calls))

    def test_backend_rotation_preserves_browser_password(self):
        self.run_setup()
        before = json.loads((self.root / 'browser.json').read_text())
        rotated = 'rotation-test-value-' + 'x' * 32
        (self.root / 'api.env').write_text('VORNIK_API_KEY=' + rotated + '\n')
        output = self.run_setup()
        after = json.loads((self.root / 'browser.json').read_text())
        self.assertTrue(before['password'] == after['password'])
        self.assertTrue(after['api_key'] == rotated)
        self.assertNotIn(rotated, output)

    def test_explicit_restoration_restores_backend_key(self):
        self.run_setup()
        bundle = json.loads((self.root / 'browser.json').read_text())
        (self.root / 'api.env').write_text('VORNIK_API_KEY=' + 'y' * 64 + '\n')
        self.run_setup('--restore-api-key')
        self.assertTrue(bundle['api_key'] in (self.root / 'api.env').read_text())

    def test_validation_failure_does_not_install_live_config(self):
        with self.assertRaises(SystemExit):
            self.run_setup(validation_ok=False)
        self.assertFalse(any(command[:2] == ['sudo', 'install'] for command in self.calls))
        self.assertFalse(any(command[:3] == ['sudo', 'systemctl', 'reload'] for command in self.calls))


if __name__ == '__main__':
    unittest.main()
