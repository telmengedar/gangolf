#!/usr/bin/env python3
import argparse
import json
import os
from pathlib import Path
import subprocess


def write_env(path, values):
    if any('\n' in value or '\r' in value for value in values.values()):
        raise SystemExit('multiline env value cannot be represented safely')
    with open(path, 'x', opener=lambda name, flags: os.open(name, flags, 0o600)) as output:
        output.write(''.join(f'{name}={value}\n' for name, value in sorted(values.items())))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--staging', action='store_true')
    parser.add_argument('--chat-config', required=True, type=int)
    parser.add_argument('--memory-config', required=True, type=int)
    args = parser.parse_args()
    if args.chat_config <= 0 or args.memory_config <= 0:
        raise SystemExit('qualified positive policy IDs required')
    os.umask(0o077)
    directory = Path(os.environ['GANGOLF_CONFIG_DIR'])
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    if directory.stat().st_mode & 0o077:
        raise SystemExit('configuration directory must be private')
    services = json.loads(Path(__file__).with_name('stack.json').read_text())
    if args.staging:
        services = [service | service.get('staging', {}) for service in services]
    if any((directory / service['env']).exists() for service in services):
        raise SystemExit('refusing to overwrite existing service configuration')
    values = {}
    for service in services:
        if service.get('responder'):
            continue
        result = subprocess.run(['docker', 'inspect', service['name']], capture_output=True, text=True, check=True)
        metadata = json.loads(result.stdout)[0]
        values[service['env']] = dict(item.split('=', 1) for item in metadata['Config']['Env'] if '=' in item)
    processor_address = '127.0.0.1:8093' if args.staging else '127.0.0.1:8080'
    expected = {
        'divoid.env': {'Database__User': 'gangolf_app'},
        'processor.env': {'PROCESSOR_HTTP_ADDR': processor_address, 'PROCESSOR_DIVOID_URL': 'http://127.0.0.1:8091', 'PROCESSOR_MODEL_URL': 'http://127.0.0.1:11434', 'PROCESSOR_MODEL_ID': 'gemma4:latest'},
        'api.env': {'GANGOLF_LISTEN': '127.0.0.1:8092', 'GANGOLF_DIVOID_API_URL': 'http://127.0.0.1:8091/api', 'GANGOLF_DIVOID_ROOT_ID': '1', 'GANGOLF_DIVOID_MESSAGES_ID': '4', 'GANGOLF_DIVOID_CONFIG_ID': '44', 'GANGOLF_TWITCH_CREDENTIAL_DIR': '/run/gangolf-credentials'},
    }
    for name, fields in expected.items():
        for key, value in fields.items():
            if values[name].get(key) != value:
                raise SystemExit(f'qualified local configuration mismatch: {name}: {key}')
    source = Path.home() / '.codex/secrets/.gangolf-api-production'
    if source.stat().st_mode & 0o077:
        raise SystemExit('protected API credential file required')
    credentials = dict(line.split('=', 1) for line in source.read_text().splitlines() if line and not line.startswith('#'))
    values['ai-core.env'] = {
        'GANGOLF_API_URL': 'http://127.0.0.1:8092/api',
        'GANGOLF_API_KEY': credentials['GANGOLF_API_KEY'],
        'GANGOLF_TWITCH_BOT_USER_ID': '195319930',
        'GANGOLF_CHANNELS': 'Twitch/85767945',
        'PROCESSOR_URL': 'http://127.0.0.1:8093' if args.staging else 'http://127.0.0.1:8080',
        'PROCESSOR_SUBJECT': '8',
        'PROCESSOR_CONFIG': str(args.chat_config),
        'PROCESSOR_MEMORY_CONFIG': str(args.memory_config),
    }
    for name, environment in values.items():
        write_env(directory / name, environment)
    print('protected service configuration materialized; no runtime changes')


if __name__ == '__main__':
    main()
