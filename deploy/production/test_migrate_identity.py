import copy
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import subprocess
from unittest.mock import patch
import unittest

SPEC = importlib.util.spec_from_file_location('migration', Path(__file__).with_name('migrate_identity.py'))
m = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(m)


def edge(source, target, kind='None', context=None):
    return {'sourceId': source, 'targetId': target, 'linkType': kind, 'context': context}


def fixture():
    result = {}
    def node(node_id, kind, name, payload=None):
        result[node_id] = dict(id=node_id, type=kind, name=name, ownerId=3, rootNodeId=1,
                               access=0, skipEmbedding=True, contentType='application/json',
                               content=None if payload is None else json.dumps(payload), linkDetails=[])
    node(1, 'project', 'Gangolf')
    for group, name in m.NAMES.items():
        node(group, None, name)
    node(6, 'processor-config', 'chat')
    node(7, 'processor-config', 'memory')
    node(8, 'agent', 'Gangolf')
    node(42, 'user', 'gangolf-user:WyJUd2l0Y2giLCIxIl0', {'schemaVersion': 1, 'service': 'Twitch', 'serviceId': '1'})
    node(43, 'channel', 'gangolf-channel:WyJUd2l0Y2giLCIyIl0', {'schemaVersion': 1, 'service': 'Twitch', 'channel': '2'})
    node(44, 'gangolf-auth-config', 'auth', {'schemaVersion': 1, 'users': {'42': {}}})
    node(45, 'gangolf-twitch-config', 'profile', {'schemaVersion': 2, 'authConfigId': '44', 'tenantUserId': '42', 'channelId': '43'})
    node(65, 'message', 'message', {'schemaVersion': 1, 'userId': '42', 'channelId': '43', 'time': '2026-11-14T00:00:00Z'})
    node(49, 'session-log', 'Processor archive')
    result[49]['access'] = 'ReadWrite'
    relations = [edge(i, 1) for i in range(2, 6)] + [edge(6, 5), edge(7, 5), edge(49, 8)]
    relations += [edge(i, m.GROUPS[result[i]['type']]) for i in (42, 43, 44, 45, 65)]
    relations += [edge(45, 44, 'Unidirectional', 'uses-auth-config'), edge(45, 43, 'Unidirectional', 'configured-for'),
                  edge(65, 42, 'Unidirectional', 'authored-by'), edge(65, 43, 'Unidirectional', 'in-channel')]
    for relation in relations:
        for node_id in (relation['sourceId'], relation['targetId']):
            result[node_id]['linkDetails'].append(copy.deepcopy(relation))
    return result


class FakeAPI:
    def __init__(self, state, owner=1):
        self.state, self.owner = state, owner
        self.calls = []
        self.failure = None

    def request(self, method, path, body=None, query=None):
        self.calls.append((method, path, copy.deepcopy(body), query))
        query = query or {}
        if method == 'GET' and path == '/nodes':
            selected = [copy.deepcopy(n) for n in self.state.values()
                        if ('rootNodeId' not in query or n['rootNodeId'] == query['rootNodeId'])
                        and ('id' not in query or n['id'] == query['id'])]
            selected.sort(key=lambda n: n['id'])
            start, count = query.get('continue', 0), min(query.get('count', 500), 3)
            end = min(start + count, len(selected))
            return {'result': selected[start:end], 'total': len(selected), 'continue': end if end < len(selected) else None}
        if method == 'POST' and path == '/nodes':
            node_id = max(self.state) + 1
            self.state[node_id] = dict(id=node_id, type=None, rootNodeId=None, ownerId=self.owner,
                access=0, content=None, contentType=None, linkDetails=[], **{k: v for k, v in body.items() if k not in ('type', 'rootNodeId', 'access')})
            self.state[node_id].update(body)
            response = copy.deepcopy(self.state[node_id])
        else:
            parts = path.split('/')
            node_id = int(parts[2])
            if method == 'PATCH':
                for operation in body:
                    self.state[node_id][operation['path'][1:]] = operation['value']
                response = None
            elif method == 'POST' and parts[3] == 'links':
                relation = edge(node_id, body, query.get('linkType', 'None'))
                for endpoint in (node_id, body):
                    if relation not in self.state[endpoint]['linkDetails']:
                        self.state[endpoint]['linkDetails'].append(relation)
                response = None
            elif method == 'DELETE':
                other = int(parts[4])
                for endpoint in (node_id, other):
                    self.state[endpoint]['linkDetails'] = [e for e in self.state[endpoint]['linkDetails'] if set((e['sourceId'], e['targetId'])) != {node_id, other}]
                response = None
            else:
                raise AssertionError((method, path))
        if self.failure and self.failure(method, path):
            self.failure = None
            raise m.Refused('synthetic unknown response after mutation')
        return response


class MigrationChecks(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.directory.chmod(0o700)
        self.original = fixture()
        self.state = copy.deepcopy(self.original)
        self.admin, self.target = FakeAPI(self.state), FakeAPI(self.state, 4)
        self.journal = m.Journal(self.directory / 'journal', {'phase': 'paused', 'snapshot': self.original,
                                  'app': m.classify(self.original)})
        self.migration = m.Migration(self.admin, self.target, self.journal)

    def assert_original(self):
        self.migration.verify(True)
        for node_id, node in self.original.items():
            self.assertEqual(m.stable(self.state[node_id]), m.stable(node))
            self.assertEqual(m.edges(self.state[node_id]), m.edges(node))

    def test_complete_move_keeps_ids_content_semantics_and_processor(self):
        self.migration.stage()
        self.migration.move()
        self.migration.verify(False)
        for node_id in (42, 43, 44, 45, 65):
            self.assertEqual(self.state[node_id]['content'], self.original[node_id]['content'])
            self.assertEqual(self.state[node_id]['ownerId'], 4)
        self.assertEqual(self.state[49], self.original[49])
        self.migration.rollback()
        self.assert_original()

    def test_partial_patch_response_lost_restores_exact_graph(self):
        self.migration.stage()
        self.admin.failure = lambda method, path: method == 'PATCH'
        with self.assertRaises(m.Refused):
            self.migration.move()
        self.migration.rollback()
        self.assert_original()

    def test_membership_response_lost_restores_both_projections(self):
        self.migration.stage()
        self.admin.failure = lambda method, path: method == 'DELETE'
        with self.assertRaises(m.Refused):
            self.migration.move()
        self.migration.rollback()
        self.assert_original()

    def test_unknown_create_is_never_retried(self):
        self.target.failure = lambda method, path: method == 'POST' and path == '/nodes'
        with self.assertRaises(m.Refused):
            self.migration.stage()
        self.assertEqual(self.journal.data['pending'], {'create': 'root'})
        self.assertEqual(len(self.state), len(self.original) + 1)
        with self.assertRaises(m.Refused):
            self.migration.stage()
        self.assertEqual(len(self.state), len(self.original) + 1)
        self.assert_original()

    def test_staging_missing_membership_is_preserved(self):
        staged = copy.deepcopy(self.original[65])
        staged.update(id=66, content=None, linkDetails=[])
        self.state[66] = staged
        self.original[66] = copy.deepcopy(staged)
        self.journal.data['snapshot'] = self.original
        self.journal.data['app'] = m.classify(self.original)
        self.migration.stage()
        self.migration.move()
        self.assertEqual(self.state[66]['linkDetails'], [])
        self.migration.rollback()
        self.assert_original()

    def test_unknown_cross_boundary_edge_refused_before_writes(self):
        relation = edge(65, 8, 'Unidirectional', 'replies-to')
        self.original[65]['linkDetails'].append(relation)
        self.original[8]['linkDetails'].append(relation)
        with self.assertRaises(m.Refused):
            m.classify(self.original)
        self.assertEqual(self.admin.calls, [])

    def test_payload_reference_outside_closed_set_refused(self):
        self.original[65]['content'] = json.dumps({'schemaVersion': 1, 'userId': '8', 'channelId': '43'})
        with self.assertRaises(m.Refused):
            m.classify(self.original)

    def test_rollback_refuses_changed_content_and_post_resume_state(self):
        self.migration.stage()
        self.migration.move()
        self.state[65]['content'] = '{}'
        with self.assertRaises(m.Refused):
            self.migration.rollback()
        self.journal.data['phase'] = 'resumed'
        with self.assertRaises(m.Refused):
            self.migration.rollback()

    def test_private_journal_rejects_unsafe_modes_and_symlink(self):
        path = self.directory / 'journal'
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        path.chmod(0o644)
        with self.assertRaises(m.Refused):
            m.Journal(path)
        link = self.directory / 'symlink'
        link.symlink_to(path)
        with self.assertRaises(OSError):
            m.private_bytes(link)

    def test_duplicate_json_and_nonprogressing_pagination_refused(self):
        with self.assertRaises(m.Refused):
            m.strict_json('{"id":1,"id":2}')
        class Broken:
            def request(self, *args, **kwargs):
                return {'result': [], 'total': 1, 'continue': 0}
        with self.assertRaises(m.Refused):
            m.paged(Broken(), '/nodes', {})

    def test_unknown_outgoing_fate_refused_without_state_rewrite(self):
        app = {65: copy.deepcopy(self.original[65])}
        app[65]['content'] = json.dumps({'schemaVersion': 4, 'delivery': {'state': 'unknown'}})
        before = copy.deepcopy(app)
        with self.assertRaises(m.Refused):
            m.no_pending_intents(app)
        self.assertEqual(app, before)
        for state in ('sent', 'submitted', 'failed'):
            app[65]['content'] = json.dumps({'schemaVersion': 3, 'delivery': {'state': state}})
            m.no_pending_intents(app)


class FakeEngine:
    def __init__(self, environment):
        self.calls = []
        self.state = {
            'old': {'Id': 'old', 'Name': '/' + m.API_NAME, 'Image': 'sha256:pinned',
                    'Config': {'Hostname': 'oldhost', 'Image': 'image-tag', 'Env': [k + '=' + v for k, v in environment.items()],
                               'User': '10001', 'Cmd': ['/gangolf-api'], 'Labels': {'keep': 'yes'}},
                    'HostConfig': {'NetworkMode': 'host', 'RestartPolicy': {'Name': 'unless-stopped'}, 'Binds': ['secret:/run/secrets:ro']},
                    'State': {'Running': False, 'ExitCode': 0}},
            'core': {'Id': 'core', 'State': {'Running': False, 'ExitCode': 0}}}
        self.fail_create = False

    def request(self, method, path, body=None, query=None):
        self.calls.append((method, path, copy.deepcopy(body), query))
        if path == '/containers/create':
            self.state['replacement'] = {'Id': 'replacement', 'Name': '/' + m.API_NAME, 'Image': body['Image'],
                'Config': {k: v for k, v in body.items() if k != 'HostConfig'}, 'HostConfig': body['HostConfig'],
                'State': {'Running': False, 'ExitCode': 0}}
            if self.fail_create:
                raise m.Refused('synthetic API create response lost')
            return {'Id': 'replacement'}
        parts = path.split('/')
        container = next(c for c in self.state.values() if c['Id'] == parts[2] or c.get('Name') == '/' + parts[2])
        if method == 'GET':
            return copy.deepcopy(container)
        if parts[-1] == 'rename':
            container['Name'] = '/' + query['name']
        elif method == 'DELETE':
            del self.state[container['Id']]
        elif parts[-1] == 'stop':
            container['State']['Running'] = False
        elif parts[-1] == 'start':
            container['State']['Running'] = True
        else:
            raise AssertionError(path)


class RuntimeChecks(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.directory.chmod(0o700)
        self.env = {'GANGOLF_DIVOID_TOKEN': 'synthetic-old-secret', 'GANGOLF_DIVOID_ROOT_ID': '1',
                    'GANGOLF_DIVOID_MESSAGES_ID': '4', 'GANGOLF_DIVOID_CONFIG_ID': '44',
                    'API_PEPPER': 'synthetic-keep-pepper', 'OTHER': 'keep'}
        self.raw = ''.join(k + '=' + v + '\n' for k, v in self.env.items()).encode()
        backup = self.directory / 'backup.env'
        m.atomic_private(backup, self.raw)
        m.atomic_private(self.directory / 'api.env', self.raw)
        self.journal = m.Journal(self.directory / 'journal', {'phase': 'moved', 'api_id': 'old',
            'core_id': 'core', 'api_env_backup': str(backup), 'rollback_name': 'old-preserved',
            'containers': {'root': 100, '4': 103}})
        self.engine = FakeEngine(self.env)
        self.runtime = m.Runtime(self.directory, self.journal, self.engine, 'synthetic-new-secret')

    def test_binding_changes_exact_three_env_fields_and_preserves_stopped_old(self):
        self.runtime.bind()
        old = self.engine.state['old']
        new = self.engine.state['replacement']
        self.assertFalse(old['State']['Running'])
        self.assertEqual(old['Name'], '/old-preserved')
        actual = dict(entry.split('=', 1) for entry in new['Config']['Env'])
        changed = {k for k in self.env if self.env[k] != actual[k]}
        self.assertEqual(changed, {'GANGOLF_DIVOID_TOKEN', 'GANGOLF_DIVOID_ROOT_ID', 'GANGOLF_DIVOID_MESSAGES_ID'})
        self.assertEqual(new['HostConfig'], old['HostConfig'])
        self.assertEqual(new['Image'], old['Image'])
        self.assertEqual(new['Config']['User'], '10001')
        self.assertNotIn('synthetic-', self.journal.path.read_text())
        self.runtime.restore_binding()
        self.assertEqual((self.directory / 'api.env').read_bytes(), self.raw)
        self.assertEqual(self.engine.state['old']['Name'], '/' + m.API_NAME)
        self.assertNotIn('replacement', self.engine.state)

    def test_unknown_api_creation_retains_stopped_old_and_pending_journal(self):
        self.engine.fail_create = True
        with self.assertRaises(m.Refused):
            self.runtime.bind()
        self.assertEqual(self.journal.data['pending'], {'createAPI': True})
        self.assertFalse(self.engine.state['old']['State']['Running'])
        self.assertFalse(self.engine.state['replacement']['State']['Running'])
        self.assertNotIn('replacement', self.journal.data)
        self.assertFalse(any(call[1].endswith('/start') for call in self.engine.calls))

    def test_running_writer_refuses_binding_before_env_or_docker_mutation(self):
        self.engine.state['core']['State']['Running'] = True
        with self.assertRaises(m.Refused):
            self.runtime.bind()
        self.assertEqual((self.directory / 'api.env').read_bytes(), self.raw)
        self.assertTrue(all(call[0] == 'GET' for call in self.engine.calls))

    def test_drain_counts_stderr_and_rejects_established_processor_socket(self):
        def result(command, **kwargs):
            if 'logs' in command:
                return subprocess.CompletedProcess(command, 0, stdout=b'', stderr=b'msg="run started"\n')
            return subprocess.CompletedProcess(command, 0, stdout=b'', stderr=b'')
        with patch.object(m.subprocess, 'run', side_effect=result):
            with self.assertRaises(m.Refused):
                m.drain_sample('sha256:pinned', '2026-11-14T00:00:00Z')
        def busy(command, **kwargs):
            output = b'' if 'logs' in command else b'tcp 0 0 127.0.0.1:8080 127.0.0.1:12345 ESTABLISHED\n'
            return subprocess.CompletedProcess(command, 0, stdout=output, stderr=b'')
        with patch.object(m.subprocess, 'run', side_effect=busy):
            with self.assertRaises(m.Refused):
                m.drain_sample('sha256:pinned', '2026-11-14T00:00:00Z')


class LoopbackTransportChecks(unittest.TestCase):
    def test_host_loopback_get_uses_stdin_secret_and_decodes_chunked_response(self):
        calls = []
        def fake(command, **kwargs):
            calls.append((command, kwargs))
            payload = b'{"result":[],"total":0,"continue":null}'
            response = b'HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n' + format(len(payload), 'x').encode() + b'\r\n' + payload + b'\r\n0\r\n\r\n'
            return subprocess.CompletedProcess(command, 0, stdout=response, stderr=b'')
        with patch.object(m.subprocess, 'run', side_effect=fake):
            result = m.LoopbackAPI('sha256:pinned', 'synthetic-api-secret').request('GET', '/v3/messages', query={'continue': 0})
        self.assertEqual(result['total'], 0)
        command, kwargs = calls[0]
        self.assertEqual(command[-2:], ['127.0.0.1', '8092'])
        self.assertIn('host', command)
        self.assertIn('nc', command)
        self.assertIn('-i', command)
        self.assertNotIn('synthetic-api-secret', repr(command))
        self.assertNotIn('env', kwargs)
        self.assertIn(b'Authorization: Bearer synthetic-api-secret\r\n', kwargs['input'])
        self.assertTrue(kwargs['input'].startswith(b'GET /api/v3/messages?continue=0 HTTP/1.1'))

    def test_loopback_refuses_post_redirect_and_header_injection(self):
        client = m.LoopbackAPI('sha256:pinned', 'synthetic-api-secret')
        with patch.object(m.subprocess, 'run') as run:
            with self.assertRaises(m.Refused):
                client.request('POST', '/v3/messages', {})
            with self.assertRaises(m.Refused):
                m.LoopbackAPI('sha256:pinned', 'bad\r\nheader').request('GET', '/v3/messages')
            run.assert_not_called()
        redirect = subprocess.CompletedProcess([], 0, stdout=b'HTTP/1.1 302 Found\r\nLocation: http://elsewhere\r\nContent-Length: 0\r\n\r\n', stderr=b'')
        with patch.object(m.subprocess, 'run', return_value=redirect):
            with self.assertRaises(m.Refused):
                client.request('GET', '/v3/messages')


if __name__ == '__main__':
    unittest.main()
