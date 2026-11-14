#!/usr/bin/env python3
import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile


def main():
    os.umask(0o077)
    config = Path(os.environ['GANGOLF_CONFIG_DIR'])
    destination = Path(os.environ['GANGOLF_BACKUP_DIR'])
    destination.mkdir(parents=True, exist_ok=True, mode=0o700)
    if destination.stat().st_mode & 0o077:
        raise SystemExit('backup directory must be private')
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')
    pending = destination / ('.pending-' + stamp)
    pending.mkdir(mode=0o700)
    pg = 'divoid-processor-alpha-pg'
    for filename, command in [('divoid.dump', ['pg_dump', '-U', 'postgres', '-d', 'divoid', '-Fc']), ('globals.sql', ['pg_dumpall', '-U', 'postgres', '--globals-only'])]:
        with (pending / filename).open('wb') as output:
            subprocess.run(['docker', 'exec', pg, *command], stdout=output, check=True, timeout=600)
    with (pending / 'containers.json').open('wb') as output:
        subprocess.run(['docker', 'inspect', pg, 'divoid-processor-alpha'], stdout=output, check=True)
    with tarfile.open(pending / 'protected-config.tar.gz', 'w:gz') as archive:
        archive.add(config, arcname='config')
    helper = os.environ['GANGOLF_API_IMAGE']
    with (pending / 'twitch-secrets.tar').open('wb') as output:
        subprocess.run(['docker', 'run', '--rm', '--user', '0', '--entrypoint', 'tar', '--volume', 'gangolf-production-twitch-secrets:/secrets:ro', helper, '-C', '/secrets', '-cf', '-', '.'], stdout=output, check=True)
    checksums = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in pending.iterdir()}
    (pending / 'manifest.json').write_text(json.dumps({'utc': stamp, 'docker_host': os.environ.get('DOCKER_HOST', 'local'), 'sha256': checksums}, indent=2) + '\n')
    pending.rename(destination / stamp)
    completed = sorted(p for p in destination.iterdir() if p.is_dir() and not p.name.startswith('.') and (p / 'manifest.json').is_file())
    for old in completed[:-7]:
        shutil.rmtree(old)
    print(f'backup complete: {stamp}')


if __name__ == '__main__':
    main()
