#!/usr/bin/env python3
"""Copy a reviewed closed Gangolf memory manifest using a private recovery journal.

Credentials arrive as JSON on stdin; all data/journal files require mode 0600.
Only the target receives writes. No request is automatically retried.
"""
import argparse
import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import stat
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request

SOURCE = 'https://divoid.mamgo.io/api'
TARGET = 'http://gangolf:8091/api'
FIELDS = 'id,name,type,status,severity,refinement,rootNodeId,ownerId,access,created,lastUpdate,contentType,skipEmbedding,substance,x,y'
COPY_FIELDS = ('name', 'type', 'status', 'severity', 'refinement', 'substance', 'skipEmbedding', 'x', 'y')


class Refused(Exception):
    pass


class HTTPFailure(Refused):
    def __init__(self, status):
        self.status = status
        super().__init__('HTTP status ' + str(status) + '; outcome requires reconciliation')


def require(condition, reason):
    if not condition:
        raise Refused(reason)


def encode(value):
    return json.dumps(value, sort_keys=True, ensure_ascii=False, separators=(',', ':')).encode()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def progress(stage, completed, total):
    if completed % 25 == 0 or completed == total:
        print(json.dumps({'stage': stage, 'completed': completed, 'total': total}), flush=True)


def read_private(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor, 'rb') as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
                and stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1,
                'owned regular 0600 input required')
        return stream.read()


def load_private(path):
    return json.loads(read_private(path))


def save_private(path, value):
    path = Path(path)
    info = path.parent.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid()
            and info.st_mode & 0o077 == 0, 'owned private directory required')
    descriptor, temporary = tempfile.mkstemp(dir=path.parent, prefix='.memory-')
    try:
        with os.fdopen(descriptor, 'wb') as stream:
            stream.write(encode(value))
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        descriptor = os.open(path.parent, os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise Refused('redirect refused')


class HTTP:
    def __init__(self, origin, key, readonly=False):
        require(origin in (SOURCE, TARGET), 'unqualified origin')
        require(isinstance(key, str) and key and '\r' not in key and '\n' not in key,
                'invalid credential')
        self.origin, self.key, self.readonly = origin, key, readonly
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def request(self, method, path, value=None, query=None, raw=False, content_type=None):
        require(not self.readonly or method == 'GET', 'source is read-only')
        require(path.startswith('/') and not path.startswith('//') and '?' not in path,
                'invalid request path')
        url = self.origin + path
        if query:
            url += '?' + urllib.parse.urlencode(query)
        payload = value if isinstance(value, bytes) else None if value is None else encode(value)
        headers = {'Authorization': 'Bearer ' + self.key,
                   'Content-Type': content_type or 'application/json'}
        request = urllib.request.Request(url, data=payload, headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=30) as response:
                data = response.read(64 * 1024 * 1024 + 1)
                require(len(data) <= 64 * 1024 * 1024, 'response size exceeded')
                return data if raw else json.loads(data) if data else None
        except urllib.error.HTTPError as error:
            error.close()
            raise HTTPFailure(error.code) from None
        except (urllib.error.URLError, OSError, ValueError):
            raise Refused('HTTP transport/parse failure; outcome requires reconciliation') from None


def rows(client, path, query):
    result, cursor = [], 0
    while True:
        page = client.request('GET', path, query=dict(query, count=500, **{'continue': cursor}))
        batch = page['result']
        result.extend(batch)
        if len(result) >= page['total']:
            break
        require(bool(batch), 'incomplete pagination')
        cursor += len(batch)
    return result


def nodes(client, **query):
    return rows(client, '/nodes', dict(query, fields=FIELDS, sort='id'))


def edges(client, ids):
    result = rows(client, '/nodes/links', {'ids': ','.join(map(str, ids)), 'sort': 'sourceid'})
    return sorted([dict(sourceId=e['sourceId'], targetId=e['targetId'],
                        linkType=e.get('linkType', 'None'), context=e.get('context'))
                   for e in result], key=lambda e: (e['sourceId'], e['targetId']))


def access(value):
    require(value in (0, 'None'), 'target must be private')


def metadata(value):
    return {key: value.get(key) for key in FIELDS.split(',')}


def normalized_edges(values):
    return sorted([dict(sourceId=e['sourceId'], targetId=e['targetId'],
                        linkType=e.get('linkType', 'None'), context=e.get('context'))
                   for e in values], key=lambda e: (e['sourceId'], e['targetId']))


def snapshot(client, node_id):
    found = nodes(client, id=node_id)
    require(len(found) == 1 and found[0]['id'] == node_id, 'snapshot node unavailable')
    meta = metadata(found[0])
    try:
        content = client.request('GET', '/nodes/' + str(node_id) + '/content', raw=True)
        require(meta['contentType'] is not None, 'present blob has no content type')
    except HTTPFailure as error:
        require(error.status == 404 and meta['contentType'] is None, 'unexpected content read failure')
        content = None
    return {'metadata': meta, 'contentBase64': None if content is None else base64.b64encode(content).decode(),
            'links': edges(client, [node_id])}


def check_identity(source, target):
    require(source.request('GET', '/users/me')['id'] == 2, 'source principal mismatch')
    require(target.request('GET', '/users/me')['id'] == 3, 'target principal mismatch')
    for node_id, kind in ((1, 'project'), (8, 'agent')):
        found = nodes(target, id=node_id)
        require(len(found) == 1, 'mapped anchor unavailable')
        node = found[0]
        require(node['ownerId'] == 3 and node['type'] == kind
                and (node_id == 1 or node['rootNodeId'] == 1), 'mapped anchor mismatch')
        access(node['access'])


def body(node):
    value = node['contentBase64']
    return None if value is None else base64.b64decode(value, validate=True)


def validate_manifest(plan):
    require(plan['version'] == 1 and plan['sourceOrigin'] == SOURCE and plan['targetOrigin'] == TARGET,
            'manifest identity mismatch')
    ids = [n['metadata']['id'] for n in plan['nodes']]
    require(bool(ids) and len(ids) == len(set(ids)) and not set(ids) & {901, 902}, 'invalid selected identities')
    require(all(type(node_id) is int and node_id > 0 for node_id in ids), 'invalid source identity')
    require(not set(map(str, ids)) & set(plan['excluded']), 'excluded node selected')
    for node in plan['nodes']:
        meta = node['metadata']
        require(meta['type'] in ('thought', 'knowledge') and type(meta['skipEmbedding']) is bool,
                'invalid memory metadata')
        require(meta['rootNodeId'] in (None, 902), 'source outside authorized roots')
        require(digest(body(node) or b'') == node['sha256'], 'manifest content hash mismatch')
        require((node['contentBase64'] is None) == (meta.get('contentType') is None), 'content presence mismatch')
    pairs = set()
    for edge in plan['edges']:
        a, b = edge['sourceId'], edge['targetId']
        require(a in ids or a in (901, 902), 'edge outside closed manifest')
        require(b in ids or b in (901, 902), 'edge outside closed manifest')
        require(a != b and (a in ids or b in ids), 'invalid manifest edge')
        pair = tuple(sorted((a, b)))
        require(pair not in pairs, 'duplicate manifest edge')
        pairs.add(pair)
    allowed = set(ids) | {901, 902}
    for edge in plan['omittedEdges']:
        require(not {edge['sourceId'], edge['targetId']} <= allowed, 'closed edge omitted')
    baseline = [s['metadata']['id'] for s in plan['baseline']]
    require(len(baseline) == 7 and len(set(baseline)) == 7 and not {1, 8} & set(baseline),
            'seven-node baseline required')
    require({n['metadata']['id'] for n in plan['anchors']} == {1, 8}, 'mapped anchor baseline required')


def prepare(capture, baseline, input_hashes):
    require(capture['sourceOrigin'].rstrip('/') in (SOURCE, SOURCE[:-4])
            and capture['targetOrigin'].rstrip('/') in (TARGET, TARGET[:-4])
            and capture['sourcePrincipal'] == 2 and capture['targetPrincipal'] == 3
            and capture['mapping'] == {'901': 8, '902': 1}, 'capture identity mismatch')
    originals = capture['nodes']
    all_edges = normalized_edges(capture['edges'] + capture['omittedEdges'])
    copied = []
    for node in originals:
        meta = metadata(node['metadata'])
        raw = None if node['bodyBase64'] is None else base64.b64decode(node['bodyBase64'], validate=True)
        require((raw is None and node['bodySha256'] is None)
                or (raw is not None and digest(raw) == node['bodySha256']), 'capture content hash mismatch')
        copied.append({'metadata': meta, 'contentBase64': node['bodyBase64'],
                       'sha256': digest(raw or b''),
                       'links': [e for e in all_edges if meta['id'] in (e['sourceId'], e['targetId'])],
                       'membershipProvenance': node['membershipProvenance'],
                       'captureEvidence': {k: v for k, v in node.items()
                                           if k not in ('metadata', 'bodyBase64', 'membershipProvenance')}})
    controls = []
    for node in baseline:
        controls.append({'metadata': metadata(node['metadata']), 'contentBase64': node['bodyBase64'],
                         'links': normalized_edges(node['links'])})
    plan = {'version': 1, 'sourceOrigin': SOURCE, 'targetOrigin': TARGET,
            'inputHashes': input_hashes, 'nodes': copied,
            'edges': normalized_edges(capture['edges']), 'omittedEdges': capture['omittedEdges'],
            'excluded': capture['excluded'],
            'baseline': [n for n in controls if n['metadata']['type'] in ('thought', 'knowledge')],
            'anchors': [n for n in controls if n['metadata']['id'] in (1, 8)]}
    validate_manifest(plan)
    return plan


class Migration:
    def __init__(self, plan, source, target, journal_path):
        validate_manifest(plan)
        self.plan, self.source, self.target = plan, source, target
        self.path = Path(journal_path)
        self.fingerprint = digest(encode(plan))
        self.state = load_private(self.path) if self.path.exists() else {
            'version': 1, 'manifestHash': self.fingerprint, 'entries': {}, 'complete': False}
        require(self.state['manifestHash'] == self.fingerprint, 'journal manifest mismatch')

    def save(self):
        save_private(self.path, self.state)

    def preconditions(self, source_check=True):
        check_identity(self.source, self.target)
        for original in self.plan['baseline']:
            require(snapshot(self.target, original['metadata']['id']) == original, 'existing target memory changed')
        additions = self.expected_edges(partial=True)
        for original in self.plan['anchors']:
            node_id = original['metadata']['id']
            actual = snapshot(self.target, node_id)
            require({k: v for k, v in actual['metadata'].items() if k != 'lastUpdate'}
                    == {k: v for k, v in original['metadata'].items() if k != 'lastUpdate'}
                    and actual['contentBase64'] == original['contentBase64'], 'mapped anchor changed')
            allowed = original['links'] + [e for e in additions if node_id in (e['sourceId'], e['targetId'])]
            require(all(e in actual['links'] for e in original['links'])
                    and all(e in allowed for e in actual['links']), 'mapped anchor links changed')
        if source_check:
            for index, node in enumerate(self.plan['nodes'], 1):
                actual = snapshot(self.source, node['metadata']['id'])
                require(actual['metadata'] == node['metadata']
                        and actual['contentBase64'] == node['contentBase64']
                        and actual['links'] == node['links'], 'source snapshot changed')
                progress('source-verified', index, len(self.plan['nodes']))

    def expected(self, node, marker=False, staged=False):
        result = {key: node['metadata'].get(key) for key in COPY_FIELDS}
        result.update(rootNodeId=1, ownerId=3, access=0)
        if staged:
            result['skipEmbedding'] = True
        if marker:
            result['name'] = self.marker(node)
        return result

    def marker(self, node):
        return 'gangolf-memory-import-' + self.fingerprint + '-' + str(node['metadata']['id'])

    def match_metadata(self, actual, wanted):
        for key, value in wanted.items():
            if key == 'access':
                access(actual.get(key))
            else:
                require(actual.get(key) == value, 'imported metadata mismatch')

    def recover(self, node, entry):
        candidates = nodes(self.target, name=self.marker(node))
        candidates = [n for n in candidates if n['name'] == self.marker(node)]
        require(len(candidates) == 1, 'uncertain create requires operator reconciliation; no retry')
        actual = candidates[0]
        self.match_metadata(actual, self.expected(node, marker=True, staged=True))
        require(actual['id'] not in [n['metadata']['id'] for n in self.plan['baseline']], 'baseline identity collision')
        entry['targetId'], entry['created'] = actual['id'], actual['created']
        self.save()

    def apply(self):
        self.preconditions()
        require(not self.state.get('rollback'), 'journal is rolled back')
        if self.state['complete']:
            self.verify()
            return
        self.save()
        for index, node in enumerate(self.plan['nodes'], 1):
            source_id = str(node['metadata']['id'])
            entry = self.state['entries'].get(source_id)
            if entry is None:
                entry = {'intent': True, 'provenance': node}
                self.state['entries'][source_id] = entry
                self.save()
                created = self.target.request('POST', '/nodes', self.expected(node, marker=True, staged=True))
                entry['targetId'], entry['created'] = created['id'], created['created']
                self.save()
            elif 'targetId' not in entry:
                self.recover(node, entry)
            require(not entry.get('deleted'), 'deleted import entry cannot resume')
            target_id = entry['targetId']
            actual = snapshot(self.target, target_id)
            require(actual['metadata']['created'] == entry['created'], 'created identity changed')
            if actual['metadata']['name'] == self.marker(node):
                wanted = self.expected(node, marker=True, staged=True)
                wanted.pop('substance')
                self.match_metadata(actual['metadata'], wanted)
                require(actual['metadata']['substance'] in (None, node['metadata']['substance'])
                        and actual['contentBase64'] in (None, node['contentBase64']), 'temporary content changed')
                if body(node) is not None and actual['contentBase64'] != node['contentBase64']:
                    self.target.request('POST', '/nodes/' + str(target_id) + '/content', body(node),
                                        content_type=node['metadata']['contentType'])
                self.target.request('PATCH', '/nodes/' + str(target_id),
                                    [{'op': 'replace', 'path': '/name', 'value': node['metadata']['name']},
                                     {'op': 'replace', 'path': '/substance', 'value': node['metadata']['substance']}])
            self.verify_node(node, staged=True)
            entry['staged'] = True
            self.save()
            progress('staged', index, len(self.plan['nodes']))
        mapping = {int(k): v['targetId'] for k, v in self.state['entries'].items()}
        mapping.update({901: 8, 902: 1})
        self.state['edgesStarted'] = True
        self.save()
        for index, edge in enumerate(self.plan['edges'], 1):
            self.target.request('POST', '/nodes/' + str(mapping[edge['sourceId']]) + '/links',
                                mapping[edge['targetId']],
                                {'linkType': edge.get('linkType', 'None'), 'context': edge.get('context') or ''})
            progress('linked', index, len(self.plan['edges']))
        for index, node in enumerate(self.plan['nodes'], 1):
            entry = self.state['entries'][str(node['metadata']['id'])]
            actual = self.verify_node(node, staged=True)
            patches = [{'op': 'replace', 'path': '/' + key, 'value': node['metadata'][key]}
                       for key in ('x', 'y', 'skipEmbedding')
                       if actual['metadata'][key] != node['metadata'][key]]
            if patches:
                self.target.request('PATCH', '/nodes/' + str(entry['targetId']), patches)
            self.verify_node(node)
            entry['verified'] = True
            self.save()
            progress('activated', index, len(self.plan['nodes']))
        self.verify()
        self.state['complete'] = True
        self.save()

    def verify_node(self, node, staged=False):
        entry = self.state['entries'][str(node['metadata']['id'])]
        actual = snapshot(self.target, entry['targetId'])
        wanted = self.expected(node)
        if staged:
            require(actual['metadata']['skipEmbedding'] in (True, node['metadata']['skipEmbedding']), 'eligibility changed')
            wanted.pop('skipEmbedding')
            if self.state.get('edgesStarted'):
                wanted.pop('x')
                wanted.pop('y')
        self.match_metadata(actual['metadata'], wanted)
        require(actual['metadata']['created'] == entry['created'], 'created identity mismatch')
        require(actual['metadata'].get('contentType') == node['metadata'].get('contentType')
                and actual['contentBase64'] == node['contentBase64'], 'imported exact content mismatch')
        return actual

    def expected_edges(self, partial=False):
        mapping = {int(k): v['targetId'] for k, v in self.state['entries'].items() if 'targetId' in v}
        mapping.update({901: 8, 902: 1})
        return [dict(e, sourceId=mapping[e['sourceId']], targetId=mapping[e['targetId']])
                for e in self.plan['edges'] if not partial or e['sourceId'] in mapping and e['targetId'] in mapping]

    def verify(self):
        self.preconditions()
        require(len(self.state['entries']) == len(self.plan['nodes']), 'incomplete import mapping')
        for node in self.plan['nodes']:
            self.verify_node(node)
        ids = [e['targetId'] for e in self.state['entries'].values()]
        require(len(set(ids)) == len(ids), 'duplicate target identity')
        actual = edges(self.target, ids)
        expected = sorted(self.expected_edges(), key=lambda e: (e['sourceId'], e['targetId']))
        require(actual == expected, 'imported links mismatch')

    def rollback(self):
        self.preconditions()
        self.state['rollback'] = True
        self.save()
        allowed = self.expected_edges(partial=True)
        for node in reversed(self.plan['nodes']):
            entry = self.state['entries'].get(str(node['metadata']['id']))
            if entry is None or entry.get('deleted'):
                continue
            if 'targetId' not in entry:
                self.recover(node, entry)
            target_id = entry['targetId']
            found = nodes(self.target, id=target_id)
            if not found:
                require(entry.get('deleteIntent'), 'imported node disappeared')
            else:
                actual = snapshot(self.target, target_id)
                wanted = self.expected(node, marker=actual['metadata']['name'] == self.marker(node))
                wanted.pop('skipEmbedding')
                require(actual['metadata']['skipEmbedding'] in (True, node['metadata']['skipEmbedding']), 'rollback eligibility changed')
                if actual['metadata']['name'] == self.marker(node):
                    wanted.pop('substance')
                    require(actual['metadata']['substance'] in (None, node['metadata']['substance']), 'rollback substance changed')
                if self.state.get('edgesStarted') and not entry.get('verified'):
                    wanted.pop('x')
                    wanted.pop('y')
                self.match_metadata(actual['metadata'], wanted)
                require(actual['metadata']['created'] == entry['created'], 'rollback identity changed')
                require(actual['contentBase64'] in (None, node['contentBase64'])
                        and all(e in allowed for e in actual['links']), 'rollback encountered changed data')
                entry['deleteIntent'] = True
                self.save()
                self.target.request('DELETE', '/nodes/' + str(target_id))
                require(not nodes(self.target, id=target_id), 'rollback deletion unverified')
            entry['deleted'] = True
            self.save()
        self.preconditions()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('prepare', 'apply', 'verify', 'rollback', 'validate'))
    parser.add_argument('--manifest', required=True)
    parser.add_argument('--manifest-sha256', required=True)
    parser.add_argument('--journal')
    parser.add_argument('--baseline')
    parser.add_argument('--baseline-sha256')
    parser.add_argument('--output')
    args = parser.parse_args()
    try:
        raw = read_private(args.manifest)
        require(digest(raw) == args.manifest_sha256, 'reviewed manifest fingerprint mismatch')
        plan = json.loads(raw)
        if args.action == 'prepare':
            baseline = read_private(args.baseline)
            require(digest(baseline) == args.baseline_sha256, 'baseline fingerprint mismatch')
            plan = prepare(plan, json.loads(baseline),
                           {'capture': args.manifest_sha256, 'baseline': args.baseline_sha256})
            require(args.output and not Path(args.output).exists(), 'new output path required')
            save_private(args.output, plan)
            print(json.dumps({'ok': True, 'nodes': len(plan['nodes']), 'edges': len(plan['edges']),
                              'planSha256': digest(encode(plan))}))
            return 0
        validate_manifest(plan)
        if args.action != 'validate':
            require(args.journal is not None, 'journal path required')
            lock_path = Path(args.journal).with_name(Path(args.journal).name + '.lock')
            descriptor = os.open(lock_path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
            with os.fdopen(descriptor, 'rb') as lock:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                credentials = json.load(sys.stdin)
                source = HTTP(SOURCE, credentials['sourceKey'], readonly=True)
                target = HTTP(TARGET, credentials['targetKey'])
                migration = Migration(plan, source, target, args.journal)
                getattr(migration, args.action)()
        print(json.dumps({'ok': True, 'nodes': len(plan['nodes']), 'edges': len(plan['edges'])}))
        return 0
    except (Refused, OSError, ValueError, KeyError, TypeError, KeyboardInterrupt):
        print(json.dumps({'ok': False, 'error': 'migration stopped; inspect private journal and reconcile before continuing'}))
        return 1


if __name__ == '__main__':
    sys.exit(main())
