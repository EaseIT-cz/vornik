#!/usr/bin/env python3
"""Configure the single-operator HTTPS gateway without displaying secrets."""
import argparse
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
import yaml


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--hostname', required=True)
    parser.add_argument('--http-port', type=int, default=8080)
    parser.add_argument('--config-dir', type=Path, default=Path.home() / '.config/vornik')
    parser.add_argument('--restore-api-key', action='store_true',
                        help='restore the API key from browser.json on a replacement host')
    args = parser.parse_args()
    if not re.fullmatch(r'[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?', args.hostname):
        parser.error('hostname must be a DNS name')

    if not 1 <= args.http_port <= 65535:
        parser.error('http-port must be between 1 and 65535')
    root = args.config_dir / 'secrets'
    config_file = root.parent / 'config.yaml'
    config_text = config_file.read_text()
    try:
        daemon_config = yaml.safe_load(config_text)
    except yaml.YAMLError:
        raise SystemExit('Cannot parse Vornik config; no credentials were changed')
    if not isinstance(daemon_config, dict) or not isinstance(daemon_config.get('api'), dict):
        raise SystemExit('Vornik config must contain an api mapping')
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    api_file = root / 'api.env'
    if not api_file.exists():
        fd = os.open(api_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'w') as stream:
            stream.write('VORNIK_API_KEY=' + secrets.token_hex(32) + '\n')
    match = re.search(r'^VORNIK_API_KEY=[\"\x27]?([A-Za-z0-9_-]+)[\"\x27]?$', api_file.read_text(), re.M)
    if not match:
        raise SystemExit('api.env must contain a VORNIK_API_KEY; no credentials were changed')
    api_key = match.group(1)
    api_file.chmod(0o600)
    bundle_file = root / 'browser.json'
    if bundle_file.exists():
        bundle = json.loads(bundle_file.read_text())
    else:
        bundle = {'username': 'admin', 'password': secrets.token_urlsafe(32)}
    username, password = bundle['username'], bundle['password']
    if not re.fullmatch(r'[A-Za-z0-9_-]+', username) or not isinstance(password, str) or len(password) < 16 or any(c in password for c in '\r\n'):
        raise SystemExit('Invalid browser credentials; no configuration was installed')
    if args.restore_api_key:
        restored = bundle.get('api_key', '')
        if not re.fullmatch(r'[A-Za-z0-9_-]{32,}', restored):
            raise SystemExit('Restored bundle must contain a valid API key')
        if restored != api_key:
            api_file.write_text(api_file.read_text().replace(match.group(0), 'VORNIK_API_KEY=' + restored))
            api_file.chmod(0o600)
            api_key = restored
    # Read the current backend key each run. API-key rotation never changes
    # the browser password. Keep the bundle suitable for off-host restoration.
    bundle['api_key'] = api_key
    fd = os.open(bundle_file, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'w') as stream:
        json.dump(bundle, stream)
        stream.write('\n')
    bundle_file.chmod(0o600)
    # Patch only the API block, retaining the rest of the install config.
    api = daemon_config['api']
    api['auth_enabled'] = True
    keys = api.setdefault('api_keys', [])
    if not isinstance(keys, list):
        raise SystemExit('api.api_keys must be a list')
    if '${VORNIK_API_KEY}' not in keys:
        keys.append('${VORNIK_API_KEY}')
    api_block = re.search(r'^api:\s*\n(?:(?:[ \t].*|[ \t]*)\n)*', config_text, re.M)
    if not api_block:
        raise SystemExit('Expected a block-style api mapping in config.yaml')
    updated_config = (config_text[:api_block.start()] + yaml.safe_dump({'api': api}, sort_keys=False)
                      + '\n' + config_text[api_block.end():])
    api_changed = daemon_config['api'] != yaml.safe_load(config_text)['api']
    if api_changed:
        config_file.write_text(updated_config)
        config_file.chmod(0o600)
    # Restart only for bootstrap/explicit restoration; proxy-only refreshes
    # and routine runs must not interrupt tasks.
    if args.restore_api_key or api_changed:
        subprocess.run(['systemctl', '--user', 'restart', 'vornik'], check=True)
    hashed = subprocess.run(['caddy', 'hash-password'], input=password + '\n', text=True,
                            capture_output=True, check=True).stdout.strip()
    # Never put credentials in systemd Environment: caddy run --environ
    # logs the environment. Import a root-owned private config instead.
    snippet = ('basic_auth {\n\t' + username + ' ' + hashed + '\n}\n'
               'vars vornik_api_key ' + api_key + '\n')
    config = Path(__file__).with_name('Caddyfile').read_text()
    config = config.replace('__VORNIK_HOSTNAME__', args.hostname).replace('__VORNIK_HTTP_PORT__', str(args.http_port))
    with tempfile.TemporaryDirectory(prefix='vornik-browser-') as directory:
        private = Path(directory) / 'auth.caddy'
        private.write_text(snippet)
        private.chmod(0o600)
        public = Path(directory) / 'Caddyfile'
        public.write_text(config)
        # Validate against the staged import before touching live files.
        validation = Path(directory) / 'validate.Caddyfile'
        validation.write_text(config.replace('/etc/caddy/vornik-auth.caddy', str(private)))
        result = subprocess.run(['sudo', 'caddy', 'validate', '--adapter', 'caddyfile',
                                 '--config', str(validation)], capture_output=True, text=True)
        if result.returncode:
            raise SystemExit('Caddy validation failed; live configuration was not replaced')
        subprocess.run(['sudo', 'install', '-o', 'root', '-g', 'caddy', '-m', '640',
                        str(private), '/etc/caddy/vornik-auth.caddy'], check=True)
        subprocess.run(['sudo', 'install', '-m', '644', str(public), '/etc/caddy/Caddyfile'], check=True)
        subprocess.run(['sudo', 'systemctl', 'reload', 'caddy'], check=True)
    print('Browser authentication configured. Credentials: ' + str(bundle_file))
    print('Routine reruns preserve the browser password and refresh the backend key.')
    print('After HTTPS verification, close external access to the backend port in your firewall/security group; preserve SSH access.')


if __name__ == '__main__':
    main()
