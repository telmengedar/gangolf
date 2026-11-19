"""Isolated HTTP-boundary checks; no credentials or production service required."""
import base64
import copy
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import urllib.error

import import_memory as tool


def meta(node_id, kind='thought', owner=2, root=902, name='same memory'):
    return tool.metadata({'id': node_id, 'name': name, 'type': kind, 'ownerId': owner,
                          'rootNodeId': root, 'access': 3 if owner == 2 else 0,
                          'created': 'original-' + str(node_id), 'lastUpdate': 'original-' + str(node_id),
                          'skipEmbedding': False, 'x': 0, 'y': 0})


class FakeHTTP:
    def __init__(self, principal):
        self.principal, self.data, self.blobs, self.links, self.calls = principal, {}, {}, [], []
        self.next_id = 500
        self.fail_create = None
        self.fail_content = False
        self.fail_delete = False
        self.fail_link = False
        self.fail_activation = False
        self.embeddings = 0
        self.edge_queries = []

    def request(self, method, path, value=None, query=None, raw=False, content_type=None):
        self.calls.append((method, path))
        if self.principal == 2:
            tool.require(method == 'GET', 'source write attempted')
        if path == '/users/me':
            return {'id': self.principal}
        if path == '/nodes' and method == 'GET':
            found = list(self.data.values())
            for key in ('id', 'name'):
                if query and key in query:
                    found = [n for n in found if n[key] == query[key]]
            start = query.get('continue', 0)
            return {'result': copy.deepcopy(found[start:start + query.get('count', 500)]), 'total': len(found)}
        if path == '/nodes/links':
            self.edge_queries.append(copy.deepcopy(query))
            ids = set(map(int, query['ids'].split(',')))
            found = [e for e in self.links if ids & {e['sourceId'], e['targetId']}]
            start = query.get('continue', 0)
            return {'result': copy.deepcopy(found[start:start + query.get('count', 500)]), 'total': len(found)}
        if path == '/nodes' and method == 'POST':
            if self.fail_create == 'before':
                raise tool.Refused('uncertain')
            node_id = self.next_id
            self.next_id += 1
            result = tool.metadata(dict(value, id=node_id, created='new-' + str(node_id), lastUpdate='new'))
            self.data[node_id] = result
            if not result['skipEmbedding']:
                self.embeddings += 1
            if self.fail_create in ('after', 'cancel'):
                if self.fail_create == 'cancel':
                    raise KeyboardInterrupt()
                raise tool.Refused('uncertain')
            return copy.deepcopy(result)
        parts = path.split('/')
        node_id = int(parts[2])
        if len(parts) == 4 and parts[3] == 'content':
            if method == 'GET':
                if node_id not in self.blobs:
                    raise tool.HTTPFailure(404)
                return self.blobs[node_id]
            self.blobs[node_id] = value
            self.data[node_id]['contentType'] = content_type
            self.data[node_id]['substance'] = None
            if not self.data[node_id]['skipEmbedding']:
                self.embeddings += 1
            if self.fail_content:
                raise tool.Refused('uncertain')
            return None
        if len(parts) == 4 and parts[3] == 'links':
            edge = dict(sourceId=node_id, targetId=value, linkType=query['linkType'], context=query.get('context') or None)
            if not any({e['sourceId'], e['targetId']} == {node_id, value} for e in self.links):
                self.links.append(edge)
                for candidate_id, anchor_id in ((node_id, value), (value, node_id)):
                    candidate, anchor = self.data[candidate_id], self.data[anchor_id]
                    if candidate['x'] == 0 and candidate['y'] == 0 and (anchor['x'] != 0 or anchor['y'] != 0):
                        candidate['x'], candidate['y'] = anchor['x'] + 42, anchor['y'] + 17
            if self.fail_link:
                raise tool.Refused('uncertain link')
            return None
        if method == 'PATCH':
            for operation in value:
                self.data[node_id][operation['path'][1:]] = operation['value']
            if any(op['path'] in ('/name', '/skipEmbedding') for op in value) and not self.data[node_id]['skipEmbedding']:
                self.embeddings += 1
            if self.fail_activation and any(p['path'] == '/skipEmbedding' and p['value'] is False for p in value):
                raise tool.Refused('uncertain activation')
            return None
        if method == 'DELETE':
            self.data.pop(node_id)
            self.blobs.pop(node_id, None)
            self.links = [e for e in self.links if node_id not in (e['sourceId'], e['targetId'])]
            if self.fail_delete:
                raise tool.Refused('uncertain')
            return None
        raise AssertionError('unexpected HTTP operation')


class ImportChecks(unittest.TestCase):
    def setUp(self):
        patch.object(tool, 'progress').start()
        self.addCleanup(patch.stopall)
        self.directory = tempfile.TemporaryDirectory()
        self.path = Path(self.directory.name) / 'journal.json'
        self.source, self.target = FakeHTTP(2), FakeHTTP(3)
        for node_id in (10, 11, 12):
            self.source.data[node_id] = meta(node_id)
        text = 'Exact UTF8 ä —\r\nwith whitespace  \n'.encode()
        for node_id in (10, 11):
            self.source.blobs[node_id] = text
            self.source.data[node_id]['contentType'] = 'text/markdown; charset=utf-8'
            self.source.data[node_id]['substance'] = 'unchanged substance'
        self.source.data[11]['skipEmbedding'] = True
        self.source.data[11]['status'] = 'obsolete'
        self.source.links = [dict(sourceId=10, targetId=11, linkType='Unidirectional', context='recalls'),
                             dict(sourceId=901, targetId=10, linkType='None', context=None),
                             dict(sourceId=12, targetId=902, linkType='None', context=None),
                             dict(sourceId=10, targetId=999, linkType='None', context=None)]
        self.target.data[1] = meta(1, kind='project', owner=3, root=None, name='Gangolf')
        self.target.data[8] = meta(8, kind='agent', owner=3, root=1, name='Gangolf')
        self.target.links = [dict(sourceId=1, targetId=8, linkType='None', context=None)]
        for node_id in range(100, 107):
            self.target.data[node_id] = meta(node_id, owner=3, root=1)
        originals = []
        for node_id in (10, 11, 12):
            captured = tool.snapshot(self.source, node_id)
            originals.append(dict(metadata=captured['metadata'], bodyBase64=captured['contentBase64'],
                                  bodySha256=tool.digest(self.source.blobs[node_id]) if node_id in self.source.blobs else None,
                                  membershipProvenance=['typed closure'], contentGETStatus=200 if node_id in self.source.blobs else 404))
        self.capture = dict(sourceOrigin=tool.SOURCE, sourcePrincipal=2, targetOrigin=tool.TARGET,
                            targetPrincipal=3, mapping={'901': 8, '902': 1}, nodes=originals,
                            edges=self.source.links[:-1], omittedEdges=self.source.links[-1:], excluded={'999': 'outside'})
        baseline = []
        for node_id in (1, 8, *range(100, 107)):
            captured = tool.snapshot(self.target, node_id)
            baseline.append(dict(metadata=captured['metadata'], bodyBase64=captured['contentBase64'], links=captured['links']))
        self.plan = tool.prepare(self.capture, baseline, {'capture': 'test', 'baseline': 'test'})
        self.original_source = copy.deepcopy((self.source.data, self.source.blobs, self.source.links))
        self.original_target = copy.deepcopy((self.target.data, self.target.blobs, self.target.links))

    def tearDown(self):
        self.directory.cleanup()

    def migration(self):
        return tool.Migration(self.plan, self.source, self.target, self.path)

    def test_exact_copy_provenance_private_mapping_and_idempotency(self):
        migration = self.migration()
        migration.apply()
        self.assertTrue(migration.state['complete'])
        self.assertEqual(len(migration.state['entries']), 3)
        self.assertEqual(migration.state['entries']['10']['provenance']['metadata']['ownerId'], 2)
        self.assertEqual(migration.state['entries']['10']['provenance']['metadata']['access'], 3)
        self.assertEqual(len([c for c in self.target.calls if c == ('POST', '/nodes')]), 3)
        calls_before_repeat = len(self.target.calls)
        self.migration().apply()
        self.assertEqual([call for call in self.target.calls[calls_before_repeat:] if call[0] != 'GET'], [])
        self.assertEqual(len([c for c in self.target.calls if c == ('POST', '/nodes')]), 3)
        self.assertEqual((self.source.data, self.source.blobs, self.source.links), self.original_source)
        self.assertFalse(any(method != 'GET' for method, _ in self.source.calls))
        for entry in migration.state['entries'].values():
            actual = self.target.data[entry['targetId']]
            self.assertEqual((actual['ownerId'], actual['access'], actual['rootNodeId']), (3, 0, 1))
        self.assertNotIn(migration.state['entries']['12']['targetId'], self.target.blobs)
        self.assertEqual(self.target.embeddings, 2)

    def test_ambiguous_create_reconciles_without_duplicate(self):
        self.target.fail_create = 'after'
        with self.assertRaises(tool.Refused):
            self.migration().apply()
        self.target.fail_create = None
        self.migration().apply()
        self.assertEqual(len([c for c in self.target.calls if c == ('POST', '/nodes')]), 3)

    def test_cancel_after_create_leaves_recoverable_intent(self):
        self.target.fail_create = 'cancel'
        with self.assertRaises(KeyboardInterrupt):
            self.migration().apply()
        self.assertNotIn('targetId', tool.load_private(self.path)['entries']['10'])
        self.target.fail_create = None
        self.migration().apply()
        self.assertEqual(len([c for c in self.target.calls if c == ('POST', '/nodes')]), 3)

    def test_unknown_create_with_zero_or_multiple_matches_never_retries(self):
        self.target.fail_create = 'before'
        with self.assertRaises(tool.Refused):
            self.migration().apply()
        self.target.fail_create = None
        with self.assertRaises(tool.Refused):
            self.migration().apply()
        self.assertEqual(len([c for c in self.target.calls if c == ('POST', '/nodes')]), 1)
        migration = self.migration()
        for node_id in (700, 701):
            self.target.data[node_id] = tool.metadata(dict(migration.expected(self.plan['nodes'][0], marker=True),
                                                         id=node_id, created='new', lastUpdate='new'))
        with self.assertRaises(tool.Refused):
            self.migration().apply()
        self.assertEqual(len([c for c in self.target.calls if c == ('POST', '/nodes')]), 1)

    def test_partial_content_failure_resumes_without_reposting_exact_bytes(self):
        self.target.fail_content = True
        with self.assertRaises(tool.Refused):
            self.migration().apply()
        self.target.fail_content = False
        self.migration().apply()
        self.assertEqual(len([c for c in self.target.calls if c == ('POST', '/nodes/500/content')]), 1)

    def test_uncertain_link_can_resume_or_rollback_without_foreign_deletion(self):
        self.target.fail_link = True
        with self.assertRaises(tool.Refused):
            self.migration().apply()
        self.target.fail_link = False
        self.migration().rollback()
        self.assertEqual((self.target.data, self.target.blobs, self.target.links), self.original_target)

    def test_uncertain_activation_resumes_and_restores_exact_coordinates(self):
        self.target.fail_activation = True
        with self.assertRaises(tool.Refused):
            self.migration().apply()
        self.target.fail_activation = False
        self.migration().apply()
        migration = self.migration()
        migration.verify()
        for node in self.plan['nodes']:
            target_id = migration.state['entries'][str(node['metadata']['id'])]['targetId']
            self.assertEqual(self.target.data[target_id]['x'], node['metadata']['x'])

    def test_zero_length_present_blob_differs_from_absence(self):
        self.plan['nodes'][0]['contentBase64'] = ''
        self.plan['nodes'][0]['sha256'] = tool.digest(b'')
        self.source.blobs[10] = b''
        self.migration().apply()
        self.assertEqual(self.target.blobs[500], b'')
        self.assertNotIn(502, self.target.blobs)

    def test_zero_coordinate_anchor_with_existing_links_is_preserved(self):
        self.source.data[10].update(x=17, y=23)
        self.plan['nodes'][0]['metadata'].update(x=17, y=23)
        original_anchor = tool.snapshot(self.target, 8)
        self.assertTrue(original_anchor['links'])
        self.migration().apply()
        self.assertEqual(tool.snapshot(self.target, 8)['metadata'], original_anchor['metadata'])
        self.assertFalse(any(method == 'PATCH' and path in ('/nodes/1', '/nodes/8')
                             for method, path in self.target.calls))
        self.assertEqual((self.target.data[500]['x'], self.target.data[500]['y']), (17, 23))

    def test_external_anchor_move_during_linking_is_refused_without_overwrite(self):
        request = self.target.request
        moved = False

        def concurrent_move(method, path, *args, **kwargs):
            nonlocal moved
            if method == 'POST' and path.endswith('/links') and not moved:
                self.target.data[8]['x'] = 77
                moved = True
            return request(method, path, *args, **kwargs)

        with patch.object(self.target, 'request', side_effect=concurrent_move):
            with self.assertRaises(tool.Refused):
                self.migration().apply()
        self.assertEqual(self.target.data[8]['x'], 77)
        self.assertFalse(any(method == 'PATCH' and path in ('/nodes/1', '/nodes/8')
                             for method, path in self.target.calls))

    def test_edge_batches_bound_urls_deduplicate_and_retain_paginated_incidence(self):
        self.target.links = [dict(sourceId=1000, targetId=n, linkType='Unidirectional', context='kept')
                             for n in range(1100, 1700)]
        self.target.links += [dict(sourceId=9999, targetId=2900, linkType='None', context=None)]
        actual = tool.edges(self.target, list(range(1000, 3026)))
        self.assertEqual(actual, tool.normalized_edges(self.target.links))
        self.assertTrue(any(q['continue'] == 500 for q in self.target.edge_queries))
        for query in self.target.edge_queries:
            self.assertLessEqual(len(query['ids'].split(',')), 100)
            self.assertLess(len(('/api/nodes/links?' + tool.urllib.parse.urlencode(query)).encode()), 4096)

    def test_conflicting_edge_batches_are_refused(self):
        self.target.links = [dict(sourceId=1000, targetId=1100, linkType='Unidirectional', context='kept')]
        request = self.target.request

        def conflicting_read(method, path, *args, **kwargs):
            response = request(method, path, *args, **kwargs)
            if path == '/nodes/links' and kwargs['query']['ids'].startswith('1100,'):
                response['result'][0]['context'] = 'concurrently changed'
            return response

        with patch.object(self.target, 'request', side_effect=conflicting_read):
            with self.assertRaises(tool.Refused):
                tool.edges(self.target, list(range(1000, 1200)))

    def pending_finalization(self):
        with patch.object(tool.Migration, 'verify', side_effect=tool.Refused('terminal check interrupted')):
            with self.assertRaises(tool.Refused):
                self.migration().apply()
        self.assertFalse(tool.load_private(self.path)['complete'])

    def test_finalize_verifies_without_target_writes_and_is_idempotent(self):
        self.pending_finalization()
        boundary = len(self.target.calls)
        self.migration().finalize()
        self.assertTrue(tool.load_private(self.path)['complete'])
        self.assertEqual([c for c in self.target.calls[boundary:] if c[0] != 'GET'], [])
        journal = self.path.read_bytes()
        self.migration().finalize()
        self.assertEqual(self.path.read_bytes(), journal)
        self.assertEqual([c for c in self.target.calls[boundary:] if c[0] != 'GET'], [])

    def test_finalize_failed_guards_do_not_mark_complete_or_write_target(self):
        self.pending_finalization()
        source_data, source_blobs = copy.deepcopy((self.source.data, self.source.blobs))
        target_data, target_blobs, target_links = copy.deepcopy((self.target.data, self.target.blobs, self.target.links))
        journal = self.path.read_bytes()
        for field in ('source-body', 'imported-body', 'baseline', 'anchor-created', 'extra-link'):
            with self.subTest(field=field):
                self.source.data, self.source.blobs = copy.deepcopy((source_data, source_blobs))
                self.target.data, self.target.blobs, self.target.links = copy.deepcopy((target_data, target_blobs, target_links))
                if field == 'source-body':
                    self.source.blobs[10] += b'changed'
                elif field == 'imported-body':
                    self.target.blobs[500] += b'changed'
                elif field == 'baseline':
                    self.target.data[100]['name'] = 'changed'
                elif field == 'anchor-created':
                    self.target.data[8]['created'] = 'changed'
                else:
                    self.target.links.append(dict(sourceId=500, targetId=100, linkType='None', context=None))
                boundary = len(self.target.calls)
                with self.assertRaises(tool.Refused):
                    self.migration().finalize()
                self.assertEqual(self.path.read_bytes(), journal)
                self.assertFalse(tool.load_private(self.path)['complete'])
                self.assertEqual([c for c in self.target.calls[boundary:] if c[0] != 'GET'], [])

    def test_changed_source_or_existing_target_blocks_before_create(self):
        self.source.blobs[10] += b'changed'
        with self.assertRaises(tool.Refused):
            self.migration().apply()
        self.assertFalse(any(m == 'POST' for m, _ in self.target.calls))
        self.source.blobs[10] = self.original_source[1][10]
        self.target.data[100]['name'] = 'changed'
        with self.assertRaises(tool.Refused):
            self.migration().apply()
        self.assertFalse(any(m == 'POST' for m, _ in self.target.calls))

    def test_changed_owned_content_is_not_overwritten_on_resume(self):
        self.target.fail_content = True
        with self.assertRaises(tool.Refused):
            self.migration().apply()
        self.target.blobs[500] = b'other authored content'
        self.target.fail_content = False
        with self.assertRaises(tool.Refused):
            self.migration().apply()
        self.assertEqual(self.target.blobs[500], b'other authored content')

    def test_rollback_deletes_only_created_ids_and_recovers_unknown_delete(self):
        self.migration().apply()
        self.target.fail_delete = True
        with self.assertRaises(tool.Refused):
            self.migration().rollback()
        self.target.fail_delete = False
        self.migration().rollback()
        self.assertEqual((self.target.data, self.target.blobs, self.target.links), self.original_target)
        deleted_ids = [int(path.split('/')[2]) for method, path in self.target.calls if method == 'DELETE']
        self.assertEqual(set(deleted_ids), {500, 501, 502})
        with self.assertRaises(tool.Refused):
            self.migration().apply()

    def test_rollback_refuses_foreign_changes(self):
        migration = self.migration()
        migration.apply()
        self.target.data[502]['ownerId'] = 4
        with self.assertRaises(tool.Refused):
            self.migration().rollback()
        self.assertFalse(any(m == 'DELETE' for m, _ in self.target.calls))

    def test_manifest_drift_and_boundary_edge_refused(self):
        self.migration().apply()
        changed = copy.deepcopy(self.plan)
        changed['nodes'][0]['metadata']['name'] = 'drift'
        with self.assertRaises(tool.Refused):
            tool.Migration(changed, self.source, self.target, self.path)
        changed['edges'].append(dict(sourceId=10, targetId=999))
        with self.assertRaises(tool.Refused):
            tool.validate_manifest(changed)

    def test_credential_and_http_error_redaction_and_source_guard(self):
        client = tool.HTTP(tool.SOURCE, 'synthetic-key', readonly=True)
        with self.assertRaises(tool.Refused):
            client.request('POST', '/nodes', {})
        failure = urllib.error.HTTPError(tool.SOURCE, 400, 'private body synthetic-key', {}, io.BytesIO(b'private'))
        with patch.object(client.opener, 'open', side_effect=failure):
            with self.assertRaises(tool.Refused) as caught:
                client.request('GET', '/nodes')
        self.assertNotIn('synthetic-key', str(caught.exception))
        self.assertNotIn('private', str(caught.exception))
        with self.assertRaises(tool.Refused):
            tool.HTTP('http://unqualified/api', 'key')

    def test_private_artifacts_required(self):
        tool.save_private(self.path, self.plan)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        self.path.chmod(0o644)
        with self.assertRaises(tool.Refused):
            tool.load_private(self.path)


if __name__ == '__main__':
    unittest.main()
