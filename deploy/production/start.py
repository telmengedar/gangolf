#!/usr/bin/env python3
import argparse
import json
import os
from pathlib import Path
import stat
import subprocess
import time


def docker(*args, **kwargs):
    return subprocess.run(['docker', *args], check=True, **kwargs)


def protected(path):
    if not path.is_file() or stat.S_IMODE(path.stat().st_mode) & 0o077:
        raise SystemExit(f'protected regular file required: {path}')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--responder', action='store_true')
    args = parser.parse_args()
    if args.responder and os.environ.get('GANGOLF_PREVIOUS_RESPONDER_STOPPED') != 'yes':
        raise SystemExit('observe previous responder stopped; set GANGOLF_PREVIOUS_RESPONDER_STOPPED=yes')
    config = Path(os.environ['GANGOLF_CONFIG_DIR'])
    helper = os.environ['GANGOLF_API_IMAGE']
    if not helper.startswith('sha256:'):
        raise SystemExit('GANGOLF_API_IMAGE must be an immutable sha256 image ID')
    services = json.loads(Path(__file__).with_name('stack.json').read_text())
    for service in services:
        if service.get('responder') and not args.responder:
            continue
        image = service.get('image') or os.environ[service['image_env']]
        if not image.startswith('sha256:'):
            raise SystemExit('immutable sha256 image ID required')
        envfile = config / service['env']
        protected(envfile)
        for mount in service['mounts']:
            docker('volume', 'inspect', mount.split(':')[0], stdout=subprocess.DEVNULL)
        if service['network'] != 'host':
            docker('network', 'inspect', service['network'], stdout=subprocess.DEVNULL)
        exists = subprocess.run(['docker', 'container', 'inspect', service['name']], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
        if exists:
            metadata = json.loads(docker('inspect', service['name'], capture_output=True, text=True).stdout)[0]
            if metadata['Image'] != image:
                raise SystemExit(f"image mismatch for {service['name']}; operations must reconcile preserved container")
            if metadata['HostConfig']['RestartPolicy']['Name'] != 'unless-stopped':
                raise SystemExit(f"restart policy mismatch for {service['name']}")
            mounted = {m['Name'] + ':' + m['Destination'] + (':ro' if not m['RW'] else '') for m in metadata['Mounts'] if m['Type'] == 'volume'}
            if not set(service['mounts']).issubset(mounted):
                raise SystemExit(f"volume mismatch for {service['name']}")
            runtime_env = dict(item.split('=', 1) for item in metadata['Config']['Env'] if '=' in item)
            for line in envfile.read_text().splitlines():
                if not line or line.startswith('#'):
                    continue
                if '=' not in line:
                    raise SystemExit(f"explicit KEY=value required in {envfile}")
                key, value = line.split('=', 1)
                if runtime_env.get(key) != value:
                    raise SystemExit(f"environment mismatch for {service['name']}: {key}")

        else:
            command = ['create', '--name', service['name'], '--restart', 'unless-stopped', '--network', service['network'], '--env-file', str(envfile)]
            for mount in service['mounts']:
                command += ['--volume', mount]
            for port in service.get('ports', []):
                command += ['--publish', port]
            docker(*command, image, stdout=subprocess.DEVNULL)
        docker('start', service['name'], stdout=subprocess.DEVNULL)
        health = service.get('health')
        if not health:
            continue
        for attempt in range(60):
            command = ['exec', service['name'], 'pg_isready', '-U', 'postgres', '-d', 'divoid'] if health == 'postgres' else ['run', '--rm', '--network', 'host', '--entrypoint', 'wget', helper, '-q', '-T', '5', '-O', '/dev/null', health]
            result = subprocess.run(['docker', *command], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
            if result.returncode == 0:
                break
            if attempt == 59:
                raise SystemExit(f"readiness timed out: {service['name']}")
            time.sleep(2)
        print(f"ready: {service['name']}", flush=True)


if __name__ == '__main__':
    main()
