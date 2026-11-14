#!/usr/bin/env python3
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('backup', type=Path)
    parser.add_argument('--name', required=True)
    args = parser.parse_args()
    if not args.name.startswith('gangolf-restore-') or not all(c.isalnum() or c == '-' for c in args.name):
        raise SystemExit('isolated name must start gangolf-restore- and contain letters/digits/hyphens')
    manifest = json.loads((args.backup / 'manifest.json').read_text())
    for name, expected in manifest['sha256'].items():
        if Path(name).name != name or hashlib.sha256((args.backup / name).read_bytes()).hexdigest() != expected:
            raise SystemExit('backup checksum mismatch')
    image = 'sha256:ac08538c6f8b9904c33c8224c5e5706dbe760aca29db1d096972b4052c22a75d'
    for kind, name in [('container', args.name), ('volume', args.name + '-data')]:
        if subprocess.run(['docker', kind, 'inspect', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
            raise SystemExit('restore target already exists')
    envfile = Path(os.environ['GANGOLF_CONFIG_DIR']) / 'postgres.env'
    if envfile.stat().st_mode & 0o077:
        raise SystemExit('protected postgres.env required')
    subprocess.run(['docker', 'volume', 'create', args.name + '-data'], check=True, stdout=subprocess.DEVNULL)
    subprocess.run(['docker', 'run', '-d', '--name', args.name, '--network', 'none', '--env-file', str(envfile), '--volume', args.name + '-data:/var/lib/postgresql/data', image], check=True, stdout=subprocess.DEVNULL)
    for attempt in range(60):
        if subprocess.run(['docker', 'exec', args.name, 'pg_isready', '-U', 'postgres', '-d', 'divoid'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
            break
        if attempt == 59:
            raise SystemExit('isolated postgres readiness timed out')
        time.sleep(2)
    globals_sql = (args.backup / 'globals.sql').read_bytes()
    globals_sql = b'\n'.join(line for line in globals_sql.split(b'\n') if line != b'CREATE ROLE postgres;')
    subprocess.run(['docker', 'exec', '-i', args.name, 'psql', '-U', 'postgres', '-d', 'postgres', '-v', 'ON_ERROR_STOP=1'], input=globals_sql, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    with (args.backup / 'divoid.dump').open('rb') as archive:
        subprocess.run(['docker', 'exec', '-i', args.name, 'pg_restore', '-U', 'postgres', '-d', 'divoid', '--exit-on-error'], stdin=archive, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    print(f'isolated restore loaded: {args.name}; application/auth/count verification still required')


if __name__ == '__main__':
    main()
