#!/usr/bin/env python3
"""Plan, import and verify frozen legacy projections in an isolated private archive graph."""
import argparse
from collections import Counter
from contextlib import contextmanager
import fcntl
import hashlib
import json
import os
from pathlib import Path
import stat
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

FORMAT = 'gangolf-legacy-operational-capture-v1'
TARGET = 'http://gangolf:8091/api'
SOURCE = 'https://gangolf.mamgo.io'
KINDS = ('messages', 'videos', 'triggers', 'channels')
ENDPOINTS = dict(zip(KINDS, ('messages', 'videos', 'videos/triggers', 'channels')))
TYPES = dict(zip(KINDS, ('message', 'video', 'trigger', 'channel')))
FIELDS = 'id,name,type,rootNodeId,ownerId,access,skipEmbedding,content,contentType,linkDetails'


class Refused(Exception):
    pass


def require(ok, reason):
    if not ok:
        raise Refused(reason)


def canonical(value):
    return json.dumps(value, sort_keys=True, ensure_ascii=False, separators=(',', ':'), allow_nan=False).encode()


def same_json(actual, expected):
    return canonical(actual) == canonical(expected)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def decode(data):
    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, 'duplicate JSON field')
            result[key] = value
        return result
    return json.loads(data, object_pairs_hook=pairs, parse_constant=lambda _: require(False, 'non-finite JSON'))


def private_read(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
                and stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1, 'owned regular 0600 file required')
        return stream.read()


def credential(path):
    values = {}
    for line in private_read(path).decode().splitlines():
        require('=' in line, 'credential must contain User/UserId/Key assignments')
        key, value = line.split('=', 1)
        require(key in ('User', 'UserId', 'Key') and key not in values, 'unexpected/duplicate credential field')
        values[key] = value
    require(set(values) == {'User', 'UserId', 'Key'} and values['UserId'] == '4', 'principal4 credential metadata required')
    return values['Key']


def private_write(path, data, replace=False):
    info = path.parent.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid()
            and not info.st_mode & 0o077, 'owned private parent directory required')
    require(replace or not path.exists(), 'output already exists')
    fd, temporary = tempfile.mkstemp(dir=path.parent, prefix='.archive-')
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        fd = os.open(path.parent, os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise Refused('redirect refused')


class HTTP:
    def __init__(self, base, token):
        require('\n' not in token and '\r' not in token and token, 'invalid credential')
        self.base, self.token = base, token
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        self.calls = Counter()

    def request(self, method, path, body=None, query=None, raw=False):
        self.calls[method + (' content' if path.endswith('/content') else ' links' if path.endswith('/links') else '')] += 1
        url = self.base + path + ('?' + urllib.parse.urlencode(query) if query else '')
        request = urllib.request.Request(url, method=method, data=None if body is None else canonical(body),
            headers={'Authorization': 'Bearer ' + self.token, 'Content-Type': 'application/json'})
        try:
            with self.opener.open(request, timeout=60) as response:
                data = response.read(64 * 1024 * 1024 + 1)
                require(len(data) <= 64 * 1024 * 1024, 'response exceeds bound')
                return data if raw else decode(data) if data else None
        except (urllib.error.URLError, OSError, ValueError):
            raise Refused('HTTP failed; mutation outcome may be unknown') from None


def read_capture(directory):
    data = private_read(directory / 'manifest.json')
    require(private_read(directory / 'manifest.sha256').decode().split()[0] == digest(data), 'manifest digest mismatch')
    manifest = decode(data)
    require(manifest.get('format') == FORMAT and manifest.get('source') == SOURCE
            and manifest.get('capture_complete') is True and set(manifest['datasets']) == set(KINDS), 'unsupported/incomplete capture')
    datasets = {}
    for kind in KINDS:
        dataset, rows, total, offset, files = manifest['datasets'][kind], [], None, 0, set()
        require(dataset.get('endpoint') == '/api/v1/' + ENDPOINTS[kind], 'unexpected source endpoint')
        for entry in dataset['pages']:
            relative = Path(entry['file'])
            require(not relative.is_absolute() and relative.parts[0] == kind and '..' not in relative.parts
                    and len(relative.parts) == 2 and str(relative) not in files, 'invalid/repeated capture path')
            files.add(str(relative))
            raw = private_read(directory / relative)
            require(len(raw) == entry['bytes'] and digest(raw) == entry['sha256'], 'capture page digest mismatch')
            page = decode(raw)
            require(isinstance(page.get('result'), list) and all(isinstance(row, dict) for row in page['result'])
                    and type(page.get('total')) is int, 'invalid capture page')
            require((total is None or total == page['total']) and entry['offset'] == offset
                    and entry['rows'] == len(page['result']) and entry['total'] == page['total']
                    and entry.get('continue') == page.get('continue'), 'capture pagination mismatch')
            total = page['total']
            rows.extend(page['result'])
            offset = len(rows)
            require(offset <= total and (page.get('continue') == offset if offset < total else page.get('continue') is None),
                    'capture continuation mismatch')
        counts = Counter(digest(canonical(row)) for row in rows)
        require(offset == total == dataset['rows'] == dataset['total'] and dataset['complete_visible_count'] is True,
                'capture count mismatch')
        multiset = private_read(directory / kind / 'multiset.json')
        require(decode(multiset) == [list(pair) for pair in sorted(counts.items())]
                and digest(multiset) == dataset['canonical_multiset_sha256'], 'capture multiset mismatch')
        datasets[kind] = rows
    return digest(data), datasets, manifest


def plan_capture(directory, limit=None):
    snapshot, datasets, manifest = read_capture(directory)
    require(limit is None or type(limit) is int and 0 < limit <= 100, 'pilot limit must be 1..100')
    entries, index = [], {}
    def add(key, kind, raw=None, extra=None, relations=None):
        require(key not in index, 'duplicate planned key')
        payload = {'schemaVersion': 1, 'snapshot': snapshot, 'key': key}
        if raw is not None:
            payload.update(raw=raw, recordSha256=digest(canonical(raw)))
        payload.update(extra or {})
        entry = {'key': key, 'type': 'legacy-archive-' + kind, 'payload': payload, 'relations': relations or []}
        entries.append(entry)
        index[key] = entry
        return key
    add('manifest', 'manifest', extra={'source': SOURCE, 'limitations': manifest.get('limitations', []),
        'counts': {kind: len(rows) for kind, rows in datasets.items()},
        'coverage': 'API-visible projection; unordered live pages, not a consistent database snapshot'})
    channel_matches, video_ids = {}, {}
    for kind in ('channels', 'videos', 'triggers'):
        counts = Counter()
        for raw in datasets[kind]:
            sha = digest(canonical(raw))
            counts[sha] += 1
            key = kind + ':' + sha + ':' + str(counts[sha])
            relations, extra = [], {}
            if kind == 'channels':
                channel_matches.setdefault(canonical([raw.get('service'), raw.get('name')]), []).append(key)
            elif kind == 'videos':
                require(type(raw.get('id')) is int and raw['id'] not in video_ids, 'missing/duplicate numeric video ID')
                video_ids[raw['id']] = key
                extra['providerInterpretation'] = 'Youtube' if raw.get('source') in (0, 'Youtube') else 'unknown'
            else:
                target = video_ids.get(raw.get('videoId'))
                extra['videoResolution'] = 'resolved-numeric-FK' if target else 'unresolved'
                if target:
                    relations.append(['references-video', target])
            add(key, TYPES[kind], raw, extra, relations)
    counts = Counter()
    for position, raw in enumerate(datasets['messages'][:limit] if limit else datasets['messages']):
        sha = digest(canonical(raw))
        counts[sha] += 1
        key = 'messages:' + sha + ':' + str(counts[sha])
        author = raw.get('user') if isinstance(raw.get('user'), dict) else {}
        author_tuple = [raw.get('service'), author.get('name')]
        author_key = 'observed-author:' + digest(canonical(author_tuple))
        if author_key not in index:
            add(author_key, 'user', extra={'serviceAndName': author_tuple, 'resolution': 'unresolved-original-account',
                'profileSemantics': 'query-time observation; per-message raw profile retained', 'replyCoverage': 'unavailable'})
        channel_tuple = [raw.get('service'), raw.get('channel')]
        matches = channel_matches.get(canonical(channel_tuple), [])
        channel_key = matches[0] if len(matches) == 1 else 'observed-channel:' + digest(canonical(channel_tuple))
        if channel_key not in index:
            add(channel_key, 'channel', extra={'serviceAndName': channel_tuple, 'resolution': 'unresolved' if not matches else 'ambiguous'})
        add(key, 'message', raw, {'sourceEndpoint': '/api/v1/messages', 'recordPosition': position,
            'originalTime': raw.get('time'), 'timeInterpretation': 'timezone-unspecified', 'replyCoverage': 'unavailable'},
            [['authored-by', author_key], ['in-channel', channel_key]])
    return {'format': 'gangolf-legacy-plan-v1', 'snapshot': snapshot, 'pilotLimit': limit,
            'entries': entries, 'planSha256': digest(canonical(entries))}


def access_private(value):
    return type(value) is int and value == 0 or value == 'None'


def inventory(client, query):
    rows, offset, total = [], 0, None
    while True:
        page = client.request('GET', '/nodes', query=query | {'fields': FIELDS, 'sort': 'id', 'descending': 'false', 'count': 500, 'continue': offset})
        require(isinstance(page, dict) and isinstance(page.get('result'), list) and type(page.get('total')) is int, 'invalid target page')
        require(total is None or total == page['total'], 'target inventory changed')
        total = page['total']
        rows.extend(page['result'])
        require(len(rows) <= total, 'target count exceeded')
        if len(rows) == total:
            require(page.get('continue') is None, 'target continuation beyond total')
            break
        require(len(rows) > offset and page.get('continue') == len(rows), 'target continuation gap')
        offset = len(rows)
    require(len({row['id'] for row in rows}) == len(rows), 'duplicate target ID')
    return rows


def normalized_edge(edge):
    return (min(edge[:2]), max(edge[:2]), *edge[2:]) if edge[2] == 'None' else edge


def edge_set(node):
    require(isinstance(node.get('linkDetails'), list), 'target links omitted')
    edges = [normalized_edge((edge['sourceId'], edge['targetId'], edge['linkType'], edge.get('context') or ''))
             for edge in node['linkDetails']]
    require(len(edges) == len(set(edges)), 'duplicate target edge')
    return set(edges)


class Archive:
    def __init__(self, client, plan, journal):
        require(plan.get('format') == 'gangolf-legacy-plan-v1' and digest(canonical(plan['entries'])) == plan['planSha256'], 'invalid plan')
        self.client, self.plan, self.path = client, plan, journal
        self.state = decode(private_read(journal)) if journal.exists() else {'snapshot': plan['snapshot'], 'pending': None}
        require(self.state['snapshot'] == plan['snapshot'], 'journal snapshot mismatch')
        self.names = {}
        self.changes = Counter()

    def save(self):
        private_write(self.path, canonical(self.state), replace=True)

    def identify(self):
        user = self.client.request('GET', '/users/me')
        require(user.get('id') == 4 and user.get('enabled') is True
                and {'read', 'write'} <= set(user.get('permissions', [])), 'enabled read/write principal4 required')

    def refresh(self, query):
        return inventory(self.client, query)

    def check_metadata(self, node, metadata):
        require(node.get('ownerId') == 4 and access_private(node.get('access')) and node.get('skipEmbedding') is True
                and all(node.get(field) == metadata.get(field) for field in ('name', 'type', 'rootNodeId')), 'target metadata conflict')

    def find(self, metadata):
        rows = self.refresh({'name': metadata['name']})
        exact = [row for row in rows if row.get('name') == metadata['name']]
        require(len(exact) <= 1, 'ambiguous deterministic name')
        if exact:
            self.check_metadata(exact[0], metadata)
            return exact[0]
        return None

    def node(self, metadata):
        name = metadata['name']
        node = self.names.get(name)
        if node:
            self.check_metadata(node, metadata)
            if same_json(self.state.get('pending'), metadata):
                self.state['pending'] = None
                self.save()
            return node
        if self.state.get('pending'):
            require(self.state['pending'] == metadata, 'unresolved previous creation; resume original plan first')
            node = self.find(metadata)
            require(node is not None, 'unknown create outcome has zero matches; operator reconciliation required')
        else:
            self.state['pending'] = metadata
            self.save()
            try:
                created = self.client.request('POST', '/nodes', metadata)
                require(isinstance(created, dict) and type(created.get('id')) is int, 'unknown node creation response')
                node = self.read_node(created['id'])
                self.check_metadata(node, metadata)
                self.changes['nodes'] += 1
            except Refused:
                node = self.find(metadata)
                require(node is not None, 'unknown create outcome has zero matches; operator reconciliation required')
        self.state['pending'] = None
        self.save()
        self.names[name] = node
        return node

    def metadata(self, name, kind, root=None):
        return {'name': name, 'type': kind, 'rootNodeId': root, 'access': 0, 'skipEmbedding': True}

    def load(self, create):
        self.identify()
        root_meta = self.metadata('legacy-archive:' + self.plan['snapshot'], 'project')
        root = self.find(root_meta)
        if root is None:
            require(create, 'archive root missing')
            root = self.node(root_meta)
        self.root = root['id']
        require(self.root not in (1, 4), 'active root/group forbidden')
        rows = [row for row in self.refresh({}) if row['id'] == self.root or row.get('rootNodeId') == self.root
                or row.get('name', '').startswith(root_meta['name'] + ':')]
        require(len({row['name'] for row in rows}) == len(rows), 'duplicate archive name')
        require(all(row.get('rootNodeId') == self.root for row in rows if row['id'] != self.root), 'archive inventory scope mismatch')
        self.names = {row['name']: row for row in rows}
        self.root_name = root_meta['name']

    def specifications(self):
        specs = [(self.metadata(self.root_name, 'project'), None, [])]
        groups = sorted({entry['type'] for entry in self.plan['entries']})
        for kind in groups:
            specs.append((self.metadata(self.root_name + ':group:' + kind, 'legacy-archive-group', self.root), None,
                          [('None', '', self.root_name)]))
        for entry in self.plan['entries']:
            name = self.root_name + ':' + entry['key']
            relations = [('None', '', self.root_name + ':group:' + entry['type'])]
            relations.extend(('Unidirectional', relation, self.root_name + ':' + key) for relation, key in entry['relations'])
            specs.append((self.metadata(name, entry['type'], self.root), entry['payload'], relations))
        return specs

    def read_node(self, node_id):
        rows = self.refresh({'id': node_id})
        require(len(rows) == 1 and rows[0]['id'] == node_id, 'target readback missing')
        return rows[0]

    def run(self):
        self.load(True)
        self.state.update(phase='importing', planSha256=self.plan['planSha256'])
        self.save()
        specs = self.specifications()
        for metadata, _, _ in specs:
            self.node(metadata)
        for metadata, payload, relations in specs:
            node = self.names[metadata['name']]
            if payload is not None:
                content = node.get('content')
                require(content in (None, '') or (node.get('contentType', '').split(';')[0] == 'application/json'
                        and same_json(decode(content), payload)), 'target content conflict')
                if content in (None, ''):
                    try:
                        self.client.request('POST', '/nodes/' + str(node['id']) + '/content', payload)
                        self.changes['content'] += 1
                    except Refused:
                        actual = self.read_node(node['id'])
                        require(actual.get('content') and same_json(decode(actual['content']), payload), 'content outcome unresolved; rerun to reconcile')
            for link_type, context, target_name in relations:
                edge = (node['id'], self.names[target_name]['id'], link_type, context)
                if normalized_edge(edge) not in edge_set(node):
                    try:
                        self.client.request('POST', '/nodes/' + str(node['id']) + '/links', edge[1], {'linkType': link_type, 'context': context})
                        self.changes['links'] += 1
                    except Refused:
                        require(normalized_edge(edge) in edge_set(self.read_node(node['id'])), 'link outcome unresolved; rerun to reconcile')
                    node['linkDetails'].append({'sourceId': edge[0], 'targetId': edge[1], 'linkType': edge[2], 'context': edge[3]})
        result = self.verify()
        self.state.update(phase='verified', planSha256=self.plan['planSha256'])
        self.save()
        return result | {'changes': dict(self.changes)}

    def verify(self):
        self.load(False)
        specs = self.specifications()
        require(set(self.names) == {metadata['name'] for metadata, _, _ in specs}, 'full archive key inventory mismatch')
        expected_edges = set()
        for metadata, payload, relations in specs:
            node = self.names[metadata['name']]
            self.check_metadata(node, metadata)
            if payload is None:
                require(node.get('content') in (None, ''), 'unexpected structural content')
            else:
                require(node.get('contentType', '').split(';')[0] == 'application/json'
                        and same_json(decode(node.get('content') or 'null'), payload), 'full content/digest verification failed')
            expected_edges.update(normalized_edge((node['id'], self.names[name]['id'], kind, context))
                                  for kind, context, name in relations)
        incident = {node['id']: set() for node in self.names.values()}
        for edge in expected_edges:
            incident[edge[0]].add(edge)
            incident[edge[1]].add(edge)
        for node in self.names.values():
            require(edge_set(node) == incident[node['id']], 'full edge multiset mismatch')
        return {'verifiedNodes': len(specs), 'verifiedEdges': len(expected_edges), 'snapshot': self.plan['snapshot'],
                'pilotLimit': self.plan['pilotLimit'], 'root': self.root}


@contextmanager
def writer_lock(journal):
    lock = Path(tempfile.gettempdir()) / ('gangolf-legacy-archive-' + str(os.getuid()) + '.lock')
    fd = os.open(lock, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    info = os.fstat(fd)
    require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
            and stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1, 'unsafe writer lock')
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    except BlockingIOError:
        raise Refused('another archive writer holds this journal') from None
    finally:
        os.close(fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=('plan', 'import', 'verify'))
    parser.add_argument('--capture', type=Path, required=True)
    parser.add_argument('--plan', type=Path)
    parser.add_argument('--journal', type=Path)
    parser.add_argument('--credential', type=Path, help='owned 0600 existing User/UserId/Key credential file')
    parser.add_argument('--target', default=TARGET, choices=(TARGET,))
    parser.add_argument('--pilot-limit', type=int)
    parser.add_argument('--full-import-authorized', action='store_true', help='operator attests measured pilot/storage/live-load acceptance')
    args = parser.parse_args()
    try:
        if args.phase == 'plan':
            require(args.plan is not None, 'plan output required')
            plan = plan_capture(args.capture, args.pilot_limit)
            private_write(args.plan, canonical(plan))
            result = {'plannedRecords': len(plan['entries']), 'snapshot': plan['snapshot'], 'pilotLimit': plan['pilotLimit']}
        else:
            require(args.plan is not None and args.journal is not None and args.credential is not None, 'plan/journal/target credential required')
            plan = decode(private_read(args.plan))
            require(same_json(plan, plan_capture(args.capture, plan['pilotLimit'])), 'plan does not match frozen capture')
            require(args.phase != 'import' or plan['pilotLimit'] is not None or args.full_import_authorized,
                    'full import requires measured pilot and explicit operator attestation')
            client = HTTP(args.target, credential(args.credential))
            started = time.monotonic()
            with writer_lock(args.journal):
                archive = Archive(client, plan, args.journal)
                result = archive.run() if args.phase == 'import' else archive.verify()
            result.update(seconds=round(time.monotonic() - started, 3), calls=dict(client.calls))
        print(json.dumps(result, sort_keys=True))
        return 0
    except Refused as error:
        print('Archive refused/incomplete: ' + str(error), file=sys.stderr)
        return 1
    except (OSError, ValueError, KeyError, TypeError):
        print('Archive refused or incomplete; inspect protected capture/journal and reconcile target. No raw payload logged.', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
