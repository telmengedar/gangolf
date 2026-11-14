import copy
import json
from pathlib import Path
import tempfile
import unittest

import archive_legacy as a


def frozen(directory, messages):
    data = {
        'messages': messages,
        'channels': [{'id': 7, 'service': 'Twitch', 'name': 'room'}],
        'videos': [{'id': 8, 'source': 'Youtube', 'videoId': 'remote-99', 'endSeconds': None}],
        'triggers': [{'id': 1, 'text': 'clip', 'videoId': 8}, {'id': 2, 'text': 'missing', 'videoId': 99}],
    }
    manifest = {'format': a.FORMAT, 'source': a.SOURCE, 'capture_complete': True, 'datasets': {}}
    for kind, rows in data.items():
        (directory / kind).mkdir(mode=0o700)
        raw = a.canonical({'result': rows, 'total': len(rows)})
        a.private_write(directory / kind / 'page-0000.json', raw)
        multiset = a.canonical(sorted(a.Counter(a.digest(a.canonical(row)) for row in rows).items()))
        a.private_write(directory / kind / 'multiset.json', multiset)
        manifest['datasets'][kind] = {'endpoint': '/api/v1/' + a.ENDPOINTS[kind], 'rows': len(rows),
            'total': len(rows), 'complete_visible_count': True, 'canonical_multiset_sha256': a.digest(multiset),
            'pages': [{'file': kind + '/page-0000.json', 'offset': 0, 'rows': len(rows), 'total': len(rows),
                       'bytes': len(raw), 'sha256': a.digest(raw)}]}
    raw = a.canonical(manifest)
    a.private_write(directory / 'manifest.json', raw)
    a.private_write(directory / 'manifest.sha256', (a.digest(raw) + '  manifest.json\n').encode())


class Graph:
    def __init__(self):
        self.nodes = {}
        self.next_id = 50
        self.posts = a.Counter()
        self.fault = None
        self.owner = 4

    def request(self, method, path, body=None, query=None):
        query = query or {}
        if path == '/users/me':
            return {'id': self.owner, 'enabled': True, 'permissions': ['read', 'write']}
        if method == 'GET' and path == '/nodes':
            rows = sorted(self.nodes.values(), key=lambda n: n['id'])
            for field in ('id', 'name', 'rootNodeId'):
                if field in query:
                    rows = [n for n in rows if n.get(field) == query[field]]
            offset = query.get('continue', 0)
            page = rows[offset:offset + query.get('count', 500)]
            end = offset + len(page)
            return copy.deepcopy({'result': page, 'total': len(rows), 'continue': end if end < len(rows) else None})
        if method == 'POST' and path == '/nodes':
            self.posts['nodes'] += 1
            if self.fault == 'create-before':
                self.fault = None
                raise a.Refused('lost')
            node = body | {'id': self.next_id, 'ownerId': 4, 'access': 'None', 'linkDetails': []}
            self.nodes[node['id']] = node
            self.next_id += 1
            if self.fault == 'create-after':
                self.fault = None
                raise a.Refused('lost')
            return copy.deepcopy(node)
        node_id = int(path.split('/')[2])
        node = self.nodes[node_id]
        if path.endswith('/content'):
            self.posts['content'] += 1
            node.update(content=a.canonical(body).decode(), contentType='application/json; charset=utf-8')
            if self.fault == 'content-after':
                self.fault = None
                raise a.Refused('lost')
            return None
        if path.endswith('/links'):
            self.posts['links'] += 1
            edge = {'sourceId': node_id, 'targetId': body, 'linkType': query['linkType'], 'context': query['context']}
            if edge not in node['linkDetails']:
                node['linkDetails'].append(edge)
                self.nodes[body]['linkDetails'].append(edge)
            if self.fault == 'link-after':
                self.fault = None
                raise a.Refused('lost')
            return None
        raise AssertionError((method, path))


class ArchiveTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.directory = Path(self.tmp.name)
        self.capture = self.directory / 'capture'
        self.capture.mkdir(mode=0o700)
        row = {'service': 'Twitch', 'channel': 'room', 'user': {'id': 0, 'name': 'author'},
               'time': '2021-01-01T12:03:04.1234567', 'tts': False,
               'chunks': [{'type': 'Image', 'data': None}, {'type': 'Command'}, {'type': 'User', 'unknown': [None, 4]}],
               'unknown': None}
        self.rows = [row, copy.deepcopy(row), row | {'time': '2021-01-01T12:03:05.1234567', 'unknown': 'different'}]
        frozen(self.capture, self.rows)
        self.plan = a.plan_capture(self.capture)
        self.graph = Graph()
        self.journal = self.directory / 'journal.json'

    def tearDown(self):
        self.tmp.cleanup()

    def archive(self, plan=None):
        return a.Archive(self.graph, plan or self.plan, self.journal)

    def test_raw_fidelity_duplicates_unresolved_identity_and_video_foreign_keys(self):
        entries = self.plan['entries']
        messages = [e for e in entries if e['type'] == 'legacy-archive-message']
        self.assertEqual([e['payload']['raw'] for e in messages], self.rows)
        self.assertEqual(len({e['key'] for e in messages}), 3)
        self.assertNotEqual(messages[0]['key'], messages[1]['key'])
        self.assertEqual(messages[0]['payload']['originalTime'], self.rows[0]['time'])
        self.assertEqual(messages[0]['payload']['timeInterpretation'], 'timezone-unspecified')
        self.assertNotIn('data', messages[0]['payload']['raw']['chunks'][1])
        self.assertIsNone(messages[0]['payload']['raw']['chunks'][0]['data'])
        authors = [e for e in entries if e['type'] == 'legacy-archive-user']
        self.assertEqual(len(authors), 1)
        self.assertEqual(authors[0]['payload']['resolution'], 'unresolved-original-account')
        self.assertFalse(any(r[0] == 'replies-to' for e in entries for r in e['relations']))
        triggers = [e for e in entries if e['type'] == 'legacy-archive-trigger']
        video = next(e for e in entries if e['type'] == 'legacy-archive-video')
        self.assertEqual(triggers[0]['relations'], [['references-video', video['key']]])
        self.assertEqual(triggers[1]['relations'], [])
        self.assertEqual(triggers[1]['payload']['videoResolution'], 'unresolved')
        self.assertNotIn('volume', video['payload']['raw'])

    def test_import_noop_and_excluded_from_live_messages(self):
        result = self.archive().run()
        before = copy.deepcopy(self.graph.nodes)
        posts = self.graph.posts.copy()
        self.assertGreater(result['changes']['nodes'], 0)
        self.assertEqual(self.archive().run()['changes'], {})
        self.assertEqual(self.graph.posts, posts)
        self.assertEqual(self.graph.nodes, before)
        for node in self.graph.nodes.values():
            self.assertNotEqual(node['type'], 'message')
            self.assertTrue(node['skipEmbedding'])
            self.assertEqual(node['access'], 'None')
            self.assertFalse(any(e['targetId'] == 4 or e['sourceId'] == 4 for e in node['linkDetails']))

    def test_unknown_create_reconciles_and_zero_match_never_reposts(self):
        self.graph.fault = 'create-after'
        self.archive().run()
        self.assertEqual(len(self.graph.nodes), self.graph.posts['nodes'])
        other = self.directory / 'other.json'
        archive = a.Archive(Graph(), self.plan, other)
        archive.client.fault = 'create-before'
        with self.assertRaises(a.Refused):
            archive.run()
        with self.assertRaises(a.Refused):
            a.Archive(archive.client, self.plan, other).run()
        self.assertEqual(archive.client.posts['nodes'], 1)

    def test_committed_create_temporarily_invisible_then_visible_resumes_once(self):
        class DelayedGraph(Graph):
            hidden = False

            def request(self, method, path, body=None, query=None):
                if method == 'GET' and path == '/nodes' and query and 'name' in query and self.hidden:
                    self.hidden = False
                    return {'result': [], 'total': 0, 'continue': None}
                try:
                    return super().request(method, path, body, query)
                except a.Refused:
                    if method == 'POST' and path == '/nodes':
                        self.hidden = True
                    raise

        self.graph = DelayedGraph()
        self.graph.fault = 'create-after'
        with self.assertRaises(a.Refused):
            self.archive().run()
        self.assertEqual(len(self.graph.nodes), 1)
        self.assertTrue(json.loads(self.journal.read_bytes())['pending'])
        self.assertEqual(self.graph.posts['nodes'], 1)
        self.archive().run()
        self.assertIsNone(json.loads(self.journal.read_bytes())['pending'])
        self.assertEqual(self.graph.posts['nodes'], len(self.graph.nodes))
        self.archive().verify()
        posts = self.graph.posts.copy()
        self.assertEqual(self.archive().run()['changes'], {})
        self.assertEqual(posts, self.graph.posts)

    def test_content_link_unknown_results_reconcile(self):
        for fault in ('content-after', 'link-after'):
            with self.subTest(fault=fault):
                self.graph.fault = fault
                self.archive().run()
                self.archive().verify()

    def test_partial_staging_resumes_and_pilot_extends_to_full(self):
        pilot = a.plan_capture(self.capture, 1)
        self.archive(pilot).run()
        before = set(self.graph.nodes)
        self.archive().run()
        self.assertTrue(before < set(self.graph.nodes))
        victim = next(n for n in self.graph.nodes.values() if n['type'] == 'legacy-archive-message')
        victim.pop('content')
        self.archive().run()
        self.archive().verify()

    def test_conflicting_owner_root_content_and_ambiguous_name_stop(self):
        self.archive().run()
        original = copy.deepcopy(self.graph.nodes)
        victim = next(n for n in self.graph.nodes.values() if n['type'] == 'legacy-archive-message')
        for field, value in [('ownerId', 3), ('rootNodeId', 1), ('content', '{}'), ('access', 'Read, Write'), ('type', 'message')]:
            with self.subTest(field=field):
                self.graph.nodes = copy.deepcopy(original)
                self.graph.nodes[victim['id']][field] = value
                posts = self.graph.posts.copy()
                with self.assertRaises(a.Refused):
                    self.archive().run()
                self.assertEqual(posts, self.graph.posts)
        self.graph.nodes = copy.deepcopy(original)
        duplicate = copy.deepcopy(victim)
        duplicate['id'] = 9999
        self.graph.nodes[9999] = duplicate
        with self.assertRaises(a.Refused):
            self.archive().run()

    def test_full_verification_detects_omitted_node_edge_and_raw_corruption(self):
        self.archive().run()
        original = copy.deepcopy(self.graph.nodes)
        victim = next(n for n in self.graph.nodes.values() if n['type'] == 'legacy-archive-message')
        del self.graph.nodes[victim['id']]
        with self.assertRaises(a.Refused):
            self.archive().verify()
        self.graph.nodes = copy.deepcopy(original)
        self.graph.nodes[victim['id']]['linkDetails'].pop()
        with self.assertRaises(a.Refused):
            self.archive().verify()
        self.graph.nodes = copy.deepcopy(original)
        payload = json.loads(victim['content'])
        payload['raw']['time'] += 'Z'
        self.graph.nodes[victim['id']]['content'] = json.dumps(payload)
        with self.assertRaises(a.Refused):
            self.archive().verify()

    def test_raw_boolean_number_corruption_refused_despite_unchanged_digest(self):
        self.archive().run()
        victim = next(n for n in self.graph.nodes.values() if n['type'] == 'legacy-archive-message')
        original = victim['content']
        for altered_id in (False, 0.0):
            with self.subTest(altered_type=type(altered_id).__name__):
                payload = json.loads(original)
                payload['raw']['user']['id'] = altered_id
                self.assertNotEqual(a.digest(a.canonical(payload['raw'])), payload['recordSha256'])
                victim['content'] = json.dumps(payload)
                posts = self.graph.posts.copy()
                with self.assertRaises(a.Refused):
                    self.archive().verify()
                with self.assertRaises(a.Refused):
                    self.archive().run()
                self.assertEqual(self.graph.posts, posts)
        self.assertFalse(a.same_json({'id': 0}, {'id': False}))
        self.assertFalse(a.same_json({'id': 0}, {'id': 0.0}))

    def test_existing_structured_credential_preserves_equals_without_logging(self):
        path = self.directory / 'credential'
        a.private_write(path, b'User=synthetic\nUserId=4\nKey=synthetic==\n')
        self.assertEqual(a.credential(path), 'synthetic==')
        a.private_write(path, b'User=synthetic\nUserId=3\nKey=synthetic==\n', replace=True)
        with self.assertRaises(a.Refused):
            a.credential(path)

    def test_large_inventory_paginates_and_verifies_all_records(self):
        for number in range(510):
            self.graph.nodes[1000 + number] = {'id': 1000 + number, 'name': 'unrelated-' + str(number),
                'type': 'unrelated', 'ownerId': 4, 'rootNodeId': None, 'access': 'None',
                'skipEmbedding': True, 'linkDetails': []}
        unrelated = copy.deepcopy(self.graph.nodes)
        self.archive().run()
        self.archive().verify()
        self.assertEqual({key: self.graph.nodes[key] for key in unrelated}, unrelated)

    def test_capture_hash_and_principal_are_enforced(self):
        self.graph.owner = 3
        with self.assertRaises(a.Refused):
            self.archive().run()
        self.assertFalse(self.graph.nodes)
        a.private_write(self.capture / 'messages' / 'page-0000.json', b'{}', replace=True)
        with self.assertRaises(a.Refused):
            a.plan_capture(self.capture)


if __name__ == '__main__':
    unittest.main()
