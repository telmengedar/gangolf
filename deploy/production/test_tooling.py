import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).parent


def load(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ToolingChecks(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.config = self.root / 'config'
        self.config.mkdir(mode=0o700)
        for name in ['postgres', 'divoid', 'processor', 'api', 'ai-core']:
            path = self.config / (name + '.env')
            path.write_text('TOKEN=synthetic-private\n')
            path.chmod(0o600)
        env = patch.dict(os.environ, GANGOLF_CONFIG_DIR=str(self.config), GANGOLF_API_IMAGE='sha256:api', GANGOLF_AI_CORE_IMAGE='sha256:core')
        env.start()
        self.addCleanup(env.stop)
        self.calls = []

    def invoke(self, module, args, fake):
        output = io.StringIO()
        with patch.object(sys, 'argv', [module.__file__, *args]), patch.object(module.subprocess, 'run', side_effect=fake), contextlib.redirect_stdout(output):
            module.main()
        self.assertNotIn('synthetic-private', output.getvalue())

    def existing(self, mutation=lambda metadata: None, staging=False):
        services = json.loads((ROOT / 'stack.json').read_text())
        if staging:
            services = [s | s.get('staging', {}) for s in services]
        def fake(command, **kwargs):
            self.calls.append(command)
            if command[1] == 'inspect':
                service = next(s for s in services if s['name'] == command[2])
                bindings = {}
                for port in service.get('ports', []):
                    host, published, target = port.rsplit(':', 2)
                    bindings[target + '/tcp'] = [{'HostIp': host, 'HostPort': published}]
                metadata = {'Image': service.get('image') or os.environ[service['image_env']], 'HostConfig': {'NetworkMode': service['network'], 'RestartPolicy': {'Name': 'unless-stopped'}, 'PortBindings': bindings}, 'Config': {'Env': ['TOKEN=synthetic-private']}, 'Mounts': [{'Type': 'volume', 'Name': mount.split(':')[0], 'Destination': mount.split(':')[1], 'RW': not mount.endswith(':ro')} for mount in service['mounts']]}
                mutation(metadata)
                return subprocess.CompletedProcess(command, 0, stdout=json.dumps([metadata]))
            return subprocess.CompletedProcess(command, 0)
        return fake

    def test_matching_canonical_topology_adopted(self):
        self.invoke(load('start'), [], self.existing())
        self.assertIn(['docker', 'start', 'processor'], self.calls)
        self.assertFalse(any('gangolf-production-ai-core' in call for call in self.calls))

    def test_matching_staging_topology_adopted(self):
        self.invoke(load('start'), ['--staging'], self.existing(staging=True))
        self.assertIn(['docker', 'start', 'gangolf-production-processor'], self.calls)
        self.assertTrue(any('http://127.0.0.1:8093/health' in call for call in self.calls))

    def test_wrong_network_denied_before_start(self):
        with self.assertRaises(SystemExit):
            self.invoke(load('start'), [], self.existing(lambda m: m['HostConfig'].update(NetworkMode='wrong-network')))
        self.assertFalse(any(call[1] == 'start' for call in self.calls))

    def test_unexpected_public_binding_denied_before_start(self):
        def public(metadata):
            metadata['HostConfig']['PortBindings'] = {'80/tcp': [{'HostIp': '0.0.0.0', 'HostPort': '8091'}]}
        with self.assertRaises(SystemExit):
            self.invoke(load('start'), [], self.existing(public))
        self.assertFalse(any(call[1] == 'start' for call in self.calls))

    def bundle(self, coverage):
        directory = self.root / 'backup'
        directory.mkdir()
        data = {'globals.sql': b'--synthetic globals\n', 'divoid.dump': b'synthetic dump'}
        for name, value in data.items():
            (directory / name).write_bytes(value)
        (directory / 'manifest.json').write_text(json.dumps({'sha256': {name: hashlib.sha256(data[name]).hexdigest() for name in coverage}}))
        return directory

    def test_missing_restore_coverage_denied_before_docker(self):
        for coverage in [[], ['globals.sql'], ['divoid.dump']]:
            with self.subTest(coverage=coverage):
                directory = self.bundle(coverage)
                with self.assertRaises(SystemExit):
                    self.invoke(load('restore'), [str(directory), '--name', 'gangolf-restore-check'], lambda *args, **kwargs: self.fail('Docker reached'))
                for path in directory.iterdir():
                    path.unlink()
                directory.rmdir()

    def test_complete_restore_coverage_reaches_existing_target_guard(self):
        directory = self.bundle(['globals.sql', 'divoid.dump'])
        def exists(command, **kwargs):
            self.calls.append(command)
            return subprocess.CompletedProcess(command, 0)
        with self.assertRaisesRegex(SystemExit, 'target already exists'):
            self.invoke(load('restore'), [str(directory), '--name', 'gangolf-restore-check'], exists)
        self.assertEqual(len(self.calls), 1)

    def test_consumed_restore_tamper_denied_before_docker(self):
        directory = self.bundle(['globals.sql', 'divoid.dump'])
        (directory / 'globals.sql').write_bytes(b'tampered')
        with self.assertRaisesRegex(SystemExit, 'checksum mismatch'):
            self.invoke(load('restore'), [str(directory), '--name', 'gangolf-restore-check'], lambda *args, **kwargs: self.fail('Docker reached'))

    def test_materialize_staging_local_config_and_fresh_core(self):
        module = load('materialize')
        target = self.root / 'materialized'
        home = self.root / 'home'
        secrets = home / '.codex/secrets'
        secrets.mkdir(parents=True)
        credential = secrets / '.gangolf-api-production'
        credential.write_text('GANGOLF_API_KEY=synthetic-private\nGANGOLF_API_KEY_PEPPER=synthetic-pepper\n')
        credential.chmod(0o600)
        environments = {
            'divoid-processor-alpha-pg': {'POSTGRES_DB': 'divoid'},
            'divoid-processor-alpha': {'Database__User': 'gangolf_app'},
            'gangolf-production-processor': {'PROCESSOR_HTTP_ADDR': '127.0.0.1:8093', 'PROCESSOR_DIVOID_URL': 'http://127.0.0.1:8091', 'PROCESSOR_MODEL_URL': 'http://127.0.0.1:11434', 'PROCESSOR_MODEL_ID': 'gemma4:latest'},
            'gangolf-production-api': {'GANGOLF_LISTEN': '127.0.0.1:8092', 'GANGOLF_DIVOID_API_URL': 'http://127.0.0.1:8091/api', 'GANGOLF_DIVOID_ROOT_ID': '1', 'GANGOLF_DIVOID_MESSAGES_ID': '4', 'GANGOLF_DIVOID_CONFIG_ID': '44', 'GANGOLF_TWITCH_CREDENTIAL_DIR': '/run/gangolf-credentials'},
        }
        def inspect(command, **kwargs):
            return subprocess.CompletedProcess(command, 0, stdout=json.dumps([{'Config': {'Env': [key + '=' + value for key, value in environments[command[2]].items()]}}]))
        with patch.dict(os.environ, GANGOLF_CONFIG_DIR=str(target)), patch.object(module.Path, 'home', return_value=home):
            self.invoke(module, ['--staging', '--chat-config', '101', '--memory-config', '102'], inspect)
        core = dict(line.split('=', 1) for line in (target / 'ai-core.env').read_text().splitlines())
        self.assertEqual(core['PROCESSOR_CONFIG'], '101')
        self.assertEqual(core['PROCESSOR_MEMORY_CONFIG'], '102')
        self.assertEqual(core['PROCESSOR_URL'], 'http://127.0.0.1:8093')
        self.assertEqual(core['GANGOLF_API_URL'], 'http://127.0.0.1:8092/api')
        self.assertNotIn('GANGOLF_ROUTES', core)
        self.assertEqual(len(list(target.iterdir())), 5)
        self.assertTrue(all(path.stat().st_mode & 0o077 == 0 for path in target.iterdir()))

    def test_materialize_writes_private_env_and_refuses_overwrite(self):
        module = load('materialize')
        path = self.root / 'service.env'
        module.write_env(path, {'TOKEN': 'synthetic-private'})
        self.assertEqual(path.stat().st_mode & 0o077, 0)
        with self.assertRaises(FileExistsError):
            module.write_env(path, {'TOKEN': 'replacement'})
        self.assertEqual(path.read_text(), 'TOKEN=synthetic-private\n')


if __name__ == '__main__':
    unittest.main()
