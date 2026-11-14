#!/usr/bin/env python3
"""Privileged, journaled Gangolf API identity cutover; never replays messages."""
import argparse
import base64
import copy
import getpass
import http.client
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

GROUPS = {'user': 2, 'channel': 3, 'message': 4, 'gangolf-message-batch': 4,
          'gangolf-auth-config': 5, 'gangolf-twitch-config': 5}
NAMES = {2: 'Users', 3: 'Channels', 4: 'Messages', 5: 'Configuration'}
FIELDS = 'id,name,type,status,severity,refinement,rootNodeId,ownerId,access,created,content,contentType,skipEmbedding,position,substance,linkDetails'
API_NAME = 'gangolf-production-api'
CORE_NAME = 'gangolf-production-ai-core'


class Refused(Exception):
    pass


def require(condition, reason):
    if not condition:
        raise Refused(reason)


def strict_json(data):
    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, 'duplicate JSON field')
            result[key] = value
        return result
    return json.loads(data, object_pairs_hook=pairs, parse_constant=lambda _: require(False, 'non-finite JSON'))


def private_bytes(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_nlink == 1
                and stat.S_IMODE(info.st_mode) == 0o600, 'owned regular 0600 file required')
        return stream.read()


def env_values(data):
    result = {}
    for line in data.decode().splitlines():
        if not line or line.startswith('#'):
            continue
        require('=' in line, 'explicit environment assignments required')
        key, value = line.split('=', 1)
        require(key not in result and key and '\x00' not in value, 'invalid environment assignment')
        result[key] = value
    return result


def atomic_private(path, data):
    parent = path.parent
    info = parent.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid()
            and not info.st_mode & 0o077, 'owned private directory required')
    fd, temporary = tempfile.mkstemp(dir=parent, prefix='.identity-')
    try:
        with os.fdopen(fd, 'wb') as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        directory = os.open(parent, os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


class HTTP:
    def __init__(self, base, token):
        self.base, self.token = base.rstrip('/'), token
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def request(self, method, path, body=None, query=None):
        url = self.base + path
        if query:
            url += '?' + urllib.parse.urlencode(query)
        headers = {'Authorization': 'Bearer ' + self.token,
                   'Content-Type': 'application/json'}
        request = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(),
                                         headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=20) as response:
                data = response.read(64 * 1024 * 1024 + 1)
                require(len(data) <= 64 * 1024 * 1024, 'response size exceeded')
                return strict_json(data) if data else None
        except (urllib.error.URLError, TimeoutError, OSError, ValueError) as error:
            raise Refused('HTTP request failed; mutation outcome may be unknown') from None


class LoopbackAPI:
    def __init__(self, image, token):
        self.image, self.token = image, token

    def request(self, method, path, body=None, query=None):
        require(method == 'GET' and body is None and path in ('/i1/readiness', '/v3/messages'), 'loopback transport is GET-only')
        require(self.image.startswith('sha256:') and '\r' not in self.token and '\n' not in self.token, 'invalid loopback transport parameters')
        target = '/api' + path
        if query:
            target += '?' + urllib.parse.urlencode(query)
        request = ('GET ' + target + ' HTTP/1.1\r\nHost: 127.0.0.1:8092\r\nConnection: close\r\n'
                   + ('Authorization: Bearer ' + self.token + '\r\n' if self.token else '') + '\r\n').encode()
        try:
            result = subprocess.run(['docker', '--host', 'tcp://gangolf:2375', 'run', '--pull', 'never',
                '--rm', '-i', '--network', 'host', '--read-only', '--entrypoint', 'nc', self.image,
                '-w', '20', '127.0.0.1', '8092'], input=request, capture_output=True, timeout=30, check=False)
            require(result.returncode == 0 and len(result.stdout) <= 64 * 1024 * 1024, 'loopback API transport failed')
            class Socket:
                def makefile(self, *args, **kwargs):
                    return io.BytesIO(result.stdout)
            response = http.client.HTTPResponse(Socket())
            response.begin()
            require(200 <= response.status < 300, 'loopback API request failed')
            data = response.read(64 * 1024 * 1024 + 1)
            require(len(data) <= 64 * 1024 * 1024, 'loopback API response exceeded limit')
            return strict_json(data) if data else None
        except (subprocess.SubprocessError, OSError, ValueError, http.client.HTTPException):
            raise Refused('loopback API transport failed') from None


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise Refused('HTTP redirect refused')


def paged(client, path, query):
    result, offset, total = [], 0, None
    while True:
        page = client.request('GET', path, query=query | {'count': 500, 'continue': offset})
        require(isinstance(page, dict) and isinstance(page.get('result'), list)
                and type(page.get('total')) is int and page['total'] >= 0, 'invalid page')
        require(total is None or total == page['total'], 'inventory changed during pagination')
        total = page['total']
        result.extend(page['result'])
        next_offset = len(result)
        require(next_offset <= total, 'page exceeds total')
        if next_offset == total:
            require(page.get('continue') is None, 'unexpected continuation')
            return result
        require(next_offset > offset and page.get('continue') == next_offset, 'invalid continuation')
        offset = next_offset


def nodes(client, **query):
    rows = paged(client, '/nodes', {'fields': FIELDS, 'sort': 'id', 'descending': 'false'} | query)
    ids = [row['id'] for row in rows]
    require(ids == sorted(set(ids)), 'duplicate or unordered node IDs')
    return {row['id']: row for row in rows}


def edge_key(edge):
    return (edge['sourceId'], edge['targetId'], edge['linkType'], edge.get('context') or '')


def edges(node):
    require(isinstance(node.get('linkDetails'), list), 'missing incident edge inventory')
    result = [edge_key(edge) for edge in node['linkDetails']]
    require(len(result) == len(set(result)), 'duplicate edges')
    require(all(node['id'] in edge[:2] for edge in result), 'nonincident edge')
    return set(result)


def plain(edge):
    return edge[2:] == ('None', '')


def membership(edge, node_id, group):
    return plain(edge) and set(edge[:2]) == {node_id, group}


def stable(node):
    return {key: value for key, value in node.items() if key not in ('lastUpdate', 'linkDetails')}


def positive(value):
    require(isinstance(value, str) and value.isascii() and value.isdigit()
            and str(int(value)) == value and int(value) > 0, 'invalid referenced ID')
    return int(value)


def classify(snapshot):
    require(1 in snapshot and all(i in snapshot for i in range(2, 9)), 'missing Processor context')
    root = snapshot[1]
    require(root['name'] == 'Gangolf' and root['type'] == 'project' and root['ownerId'] == 3
            and root['access'] in (0, 'None'), 'unexpected original root')
    for group, name in NAMES.items():
        node = snapshot[group]
        require(node['name'] == name and node['type'] is None and node['rootNodeId'] == 1
                and node['ownerId'] == 3 and node['access'] in (0, 'None')
                and any(membership(edge, group, 1) for edge in edges(node)), 'unexpected original group')
    require(all(i <= 8 or node['type'] in GROUPS or node['type'] in ('processor-config', 'agent', 'session-log')
                for i, node in snapshot.items()), 'unclassified root resource type')
    app = {i: node for i, node in snapshot.items() if node['type'] in GROUPS}
    require(44 in app and app[44]['type'] == 'gangolf-auth-config', 'auth config44 absent')
    require(not set(range(1, 9)) & app.keys(), 'Processor context misclassified')
    payloads = {}
    for node_id, node in app.items():
        require(node['rootNodeId'] == 1 and node['ownerId'] == 3 and node['access'] in (0, 'None'), 'app ownership/privacy mismatch')
        content = node.get('content')
        if content is None or content == '':
            require(node['type'] in ('message', 'gangolf-message-batch', 'user', 'channel'), 'incomplete config')
            payloads[node_id] = None
        else:
            require(node.get('contentType', '').split(';')[0] == 'application/json', 'non-JSON app payload')
            payload = strict_json(content)
            require(isinstance(payload, dict) and type(payload.get('schemaVersion')) is int, 'invalid schema')
            payloads[node_id] = payload
    expected = {}
    for node_id, node in app.items():
        payload, kind = payloads[node_id], node['type']
        refs = {}
        def ref(field, target_kind, relation=None, source=None):
            data = payload if source is None else source
            target = positive(data[field])
            require(target in app and app[target]['type'] == target_kind, 'cross-boundary/missing payload reference')
            if relation:
                refs[relation] = target
            return target
        if payload is not None:
            schema = payload['schemaVersion']
            if kind in ('user', 'channel'):
                require(schema == 1 and payload.get('service'), 'invalid identity payload')
                key = payload.get('serviceId' if kind == 'user' else 'channel')
                require(isinstance(key, str) and key, 'invalid identity key')
                identity = base64.urlsafe_b64encode(json.dumps([payload['service'], key], ensure_ascii=False, separators=(',', ':')).encode()).decode().rstrip('=')
                require(node['name'] == 'gangolf-' + kind + ':' + identity, 'identity name mismatch')
            elif kind == 'message':
                require(schema in (1, 2, 3, 4), 'unknown message schema')
                ref('userId', 'user', 'authored-by')
                ref('channelId', 'channel', 'in-channel')
                if payload.get('replyTo') is not None:
                    parent = ref('replyTo', 'message', 'replies-to')
                    require(parent < node_id and payloads[parent] is not None
                            and payloads[parent]['channelId'] == payload['channelId'], 'invalid reply ancestor')
                if schema in (2, 3, 4):
                    source = payload.get('source' if schema == 2 else 'delivery')
                    require(isinstance(source, dict), 'missing message provenance')
                    profile = ref('configId', 'gangolf-twitch-config', source=source)
                    require(payloads[profile]['channelId'] == payload['channelId'], 'message profile/channel mismatch')
            elif kind == 'gangolf-twitch-config':
                require(schema in (1, 2), 'unknown Twitch schema')
                ref('authConfigId', 'gangolf-auth-config', 'uses-auth-config')
                ref('tenantUserId', 'user')
                ref('channelId', 'channel', 'configured-for')
            elif kind == 'gangolf-auth-config':
                require(schema == 1 and isinstance(payload.get('users'), dict), 'invalid auth schema')
                for tenant in payload['users']:
                    require(positive(tenant) in app and app[int(tenant)]['type'] == 'user', 'auth tenant outside closure')
            elif kind == 'gangolf-message-batch':
                require(schema == 1 and isinstance(payload.get('messages'), list), 'invalid legacy batch')
        expected[node_id] = refs
    all_edges = set().union(*(edges(node) for node in snapshot.values()))
    for node_id, node in app.items():
        old_group = GROUPS[node['type']]
        own_edges = edges(node)
        require(sum(membership(edge, node_id, old_group) for edge in own_edges) <= 1, 'duplicate membership')
        for edge in own_edges:
            source, target, relation_type, context = edge
            other = target if source == node_id else source
            require(other in snapshot and edge in edges(snapshot[other]), 'missing reciprocal edge projection')
            if membership(edge, node_id, old_group):
                continue
            staged_relation = (source in app and payloads[source] is None
                               and app[source]['type'] == 'message' and target in app
                               and {'authored-by': 'user', 'in-channel': 'channel', 'replies-to': 'message'}.get(context) == app[target]['type'])
            require(other in app and relation_type == 'Unidirectional'
                    and (expected[source].get(context) == target or staged_relation), 'unknown semantic/cross-boundary edge')
        payload = payloads[node_id]
        if payload is not None:
            for relation, target in expected[node_id].items():
                require((node_id, target, 'Unidirectional', relation) in own_edges, 'incomplete published semantic closure')
    require(all(not (set(edge[:2]) & app.keys()) or edge in set().union(*(edges(n) for n in app.values()))
                for edge in all_edges), 'inconsistent edge inventory')
    return app


class Journal:
    def __init__(self, path, data=None):
        self.path = path
        self.data = data if data is not None else strict_json(private_bytes(path))
        if data is not None:
            require(not path.exists(), 'journal already exists; use recover or verify')
            self.save()

    def save(self):
        atomic_private(self.path, json.dumps(self.data, separators=(',', ':')).encode())

    def phase(self, value):
        self.data['phase'] = value
        self.save()

    def intent(self, value):
        self.data['pending'] = value
        self.save()


class Migration:
    def __init__(self, admin, target, journal):
        self.admin, self.target, self.journal = admin, target, journal

    def stage(self):
        require(not self.journal.data.get('pending') and not self.journal.data.get('containers'), 'stage already attempted; reconcile journal instead of retry')
        containers = self.journal.data.setdefault('containers', {})
        for key, name in [('root', 'Gangolf'), *[(str(i), n) for i, n in NAMES.items()]]:
            require(key not in containers, 'stage already started; reconcile journal instead of retry')
            body = {'name': name, 'access': 0, 'skipEmbedding': True}
            if key == 'root':
                body['type'] = 'project'
            else:
                body['rootNodeId'] = containers['root']
            self.journal.intent({'create': key})
            node = self.target.request('POST', '/nodes', body)
            require(isinstance(node, dict) and type(node.get('id')) is int, 'creation outcome unknown')
            containers[key] = node['id']
            self.journal.intent(None)
            if key != 'root':
                self.journal.intent({'linkContainer': node['id']})
                self.target.request('POST', '/nodes/' + str(containers['root']) + '/links', node['id'], {'linkType': 'None'})
                self.journal.intent(None)
        self.validate_containers()
        self.journal.phase('staged')

    def validate_containers(self):
        containers = self.journal.data['containers']
        root = nodes(self.target, id=containers['root'])[containers['root']]
        require(root['name'] == 'Gangolf' and root['type'] == 'project' and root['ownerId'] == 4
                and root['access'] in (0, 'None'), 'new root metadata mismatch')
        children = nodes(self.target, rootNodeId=containers['root'])
        for group, name in NAMES.items():
            group_id = containers[str(group)]
            require(group_id in children and sum(n['name'] == name and n['type'] is None for n in children.values()) == 1,
                    'new group missing/ambiguous')
            child = children[group_id]
            require(child['name'] == name and child['type'] is None and child['ownerId'] == 4
                    and child['access'] in (0, 'None') and child.get('skipEmbedding') is True
                    and any(membership(e, group_id, containers['root']) for e in edges(child))
                    and any(membership(e, group_id, containers['root']) for e in edges(root)), 'new group metadata/edge mismatch')

    def move(self):
        data = self.journal.data
        containers = data['containers']
        for node in data['app'].values():
            node_id = node['id']
            self.journal.intent({'move': node_id})
            self.admin.request('PATCH', '/nodes/' + str(node_id), [
                {'op': 'replace', 'path': '/rootNodeId', 'value': containers['root']},
                {'op': 'replace', 'path': '/ownerId', 'value': 4}])
            for edge in edges(node):
                old_group = GROUPS[node['type']]
                if membership(edge, node_id, old_group):
                    new_group = containers[str(old_group)]
                    self.admin.request('POST', '/nodes/' + str(node_id) + '/links', new_group, {'linkType': 'None'})
                    self.admin.request('DELETE', '/nodes/' + str(node_id) + '/links/' + str(old_group))
            self.journal.intent(None)
        self.verify(False)
        self.journal.phase('moved')

    def verify(self, original):
        data = self.journal.data
        if not original:
            self.validate_containers()
        current = nodes(self.admin, rootNodeId=1)
        current[1] = nodes(self.admin, id=1)[1]
        if not original:
            current.update(nodes(self.target, rootNodeId=data['containers']['root']))
        expected = {int(i): copy.deepcopy(n) for i, n in data['snapshot'].items()}
        app_ids = {int(i) for i in data['app']}
        edge_set = set().union(*(edges(n) for n in expected.values()))
        if not original:
            for node_id in app_ids:
                node = expected[node_id]
                group = GROUPS[node['type']]
                old_edges = {edge for edge in edges(node) if membership(edge, node_id, group)}
                edge_set -= old_edges
                if old_edges:
                    edge_set.add((node_id, data['containers'][str(group)], 'None', ''))
                node['ownerId'], node['rootNodeId'] = 4, data['containers']['root']
        for node_id, wanted in expected.items():
            require(node_id in current and stable(current[node_id]) == stable(wanted), 'metadata/content preservation failed')
            wanted_edges = {edge for edge in edge_set if node_id in edge[:2]}
            actual = edges(current[node_id])
            require(actual == wanted_edges, 'edge preservation failed')
        expected_ids = set(expected)
        actual_ids = set(current) - set(data.get('containers', {}).values())
        require(expected_ids == actual_ids, 'unexpected graph inventory drift')

    def rollback(self):
        data = self.journal.data
        require(data['phase'] != 'resumed', 'post-resume rollback requires fresh owner reconciliation')
        self.journal.phase('recovering')
        for original in data['app'].values():
            node_id = original['id']
            current = nodes(self.admin, id=node_id)[node_id]
            wanted = stable(original)
            actual = stable(current)
            require({k: v for k, v in actual.items() if k not in ('ownerId', 'rootNodeId')}
                    == {k: v for k, v in wanted.items() if k not in ('ownerId', 'rootNodeId')}, 'rollback encountered changed content/metadata')
            require(current['ownerId'] in (3, 4) and current['rootNodeId'] in (1, data.get('containers', {}).get('root')), 'rollback encountered foreign ownership')
            old_group = GROUPS[original['type']]
            new_group = data.get('containers', {}).get(str(old_group))
            if new_group:
                current_memberships = {e for e in edges(current) if membership(e, node_id, new_group)}
                for edge in current_memberships:
                    self.admin.request('DELETE', '/nodes/' + str(node_id) + '/links/' + str(new_group))
            old_edges = {e for e in edges(original) if membership(e, node_id, old_group)}
            if old_edges and not any(membership(e, node_id, old_group) for e in edges(current)):
                edge = next(iter(old_edges))
                self.admin.request('POST', '/nodes/' + str(edge[0]) + '/links', edge[1], {'linkType': 'None'})
            self.admin.request('PATCH', '/nodes/' + str(node_id), [
                {'op': 'replace', 'path': '/rootNodeId', 'value': 1},
                {'op': 'replace', 'path': '/ownerId', 'value': 3}])
        self.verify(True)
        self.journal.intent(None)
        self.journal.phase('rolled-back')


class Runtime:
    def __init__(self, config, journal, docker, target_key):
        self.config, self.journal, self.docker = config, journal, docker
        self.target_key = target_key

    def inspect(self, name):
        return self.docker.request('GET', '/containers/' + name + '/json')

    def stopped(self):
        require(not self.inspect(self.journal.data['core_id'])['State']['Running']
                and not self.inspect(self.journal.data.get('replacement') or self.journal.data['api_id'])['State']['Running'], 'writers must be stopped')

    def stop(self, name):
        current = self.inspect(name)
        if current['State']['Running']:
            self.docker.request('POST', '/containers/' + current['Id'] + '/stop', query={'t': 60})
        stopped = self.inspect(current['Id'])
        require(not stopped['State']['Running'] and not stopped['State'].get('OOMKilled')
                and stopped['State']['ExitCode'] == 0, 'unclean writer shutdown; manual reconciliation required')

    def preserve_services(self):
        for name in ('processor', 'divoid-processor-alpha'):
            current = self.inspect(name)
            require(current['Id'] == self.journal.data['services'][name] and current['State']['Running'], 'Processor/DiVoid runtime changed')

    def bind(self):
        self.stopped()
        data = self.journal.data
        old = self.inspect(data['api_id'])
        values = env_values(private_bytes(Path(data['api_env_backup'])))
        values.update(GANGOLF_DIVOID_TOKEN=self.target_key,
                      GANGOLF_DIVOID_ROOT_ID=str(data['containers']['root']),
                      GANGOLF_DIVOID_MESSAGES_ID=str(data['containers']['4']))
        require(all('\n' not in value and '\r' not in value for value in values.values()), 'unrepresentable env value')
        atomic_private(self.config / 'api.env', ''.join(key + '=' + value + '\n' for key, value in sorted(values.items())).encode())
        self.journal.phase('binding')
        self.docker.request('POST', '/containers/' + old['Id'] + '/rename', query={'name': data['rollback_name']})
        body = copy.deepcopy(old['Config'])
        body.pop('Hostname', None)
        original_env = env_values(private_bytes(Path(data['api_env_backup'])))
        runtime_env = dict(entry.split('=', 1) for entry in body['Env'])
        require(all(runtime_env.get(key) == value for key, value in original_env.items()), 'canonical/runtime environment mismatch')
        runtime_env.update({key: values[key] for key in ('GANGOLF_DIVOID_TOKEN', 'GANGOLF_DIVOID_ROOT_ID', 'GANGOLF_DIVOID_MESSAGES_ID')})
        body['Env'] = [key + '=' + value for key, value in sorted(runtime_env.items())]
        body['Image'] = old['Image']
        body['HostConfig'] = copy.deepcopy(old['HostConfig'])
        self.journal.intent({'createAPI': True})
        created = self.docker.request('POST', '/containers/create', body, {'name': API_NAME})
        require(isinstance(created.get('Id'), str), 'API creation outcome unknown')
        data['replacement'] = created['Id']
        self.journal.intent(None)
        actual = self.inspect(created['Id'])
        require(actual['Image'] == old['Image'] and actual['HostConfig'] == old['HostConfig'], 'replacement topology differs')
        require({k: v for k, v in actual['Config'].items() if k not in ('Hostname', 'Image', 'Env')}
                == {k: v for k, v in old['Config'].items() if k not in ('Hostname', 'Image', 'Env')}, 'replacement process configuration differs')
        require(dict(entry.split('=', 1) for entry in actual['Config']['Env']) == runtime_env, 'replacement environment differs')
        self.journal.phase('bound')

    def start_api(self):
        self.docker.request('POST', '/containers/' + self.journal.data['replacement'] + '/start')

    def restore_binding(self):
        data = self.journal.data
        if data.get('replacement'):
            self.stop(API_NAME)
            self.docker.request('DELETE', '/containers/' + data['replacement'])
            data.pop('replacement')
            self.journal.save()
        old = self.inspect(data['api_id'])
        if old['Name'] != '/' + API_NAME:
            self.docker.request('POST', '/containers/' + old['Id'] + '/rename', query={'name': API_NAME})
        atomic_private(self.config / 'api.env', private_bytes(Path(data['api_env_backup'])))

    def resume_old(self):
        self.restore_binding()
        self.preserve_services()
        self.docker.request('POST', '/containers/' + self.journal.data['api_id'] + '/start')
        wait_ready(self.inspect(self.journal.data['api_id'])['Image'])
        self.docker.request('POST', '/containers/' + self.journal.data['core_id'] + '/start')
        self.journal.phase('restored')


def wait_ready(image):
    client = LoopbackAPI(image, '')
    for attempt in range(30):
        try:
            client.request('GET', '/i1/readiness')
            return
        except Refused:
            if attempt == 29:
                raise
            time.sleep(1)


def identify(client, owner=None, admin=False):
    user = client.request('GET', '/users/me')
    require(user.get('enabled') is True and (owner is None or user.get('id') == owner), 'principal mismatch/disabled')
    permissions = set(user.get('permissions', []))
    require('admin' in permissions if admin else {'read', 'write'} <= permissions, 'required permissions absent')


def preflight(docker, config):
    api = docker.request('GET', '/containers/' + API_NAME + '/json')
    core = docker.request('GET', '/containers/' + CORE_NAME + '/json')
    for container in (api, core):
        require(container['State']['Running'] and container['HostConfig']['NetworkMode'] == 'host'
                and container['HostConfig']['RestartPolicy']['Name'] == 'unless-stopped', 'unexpected writer runtime')
    all_containers = docker.request('GET', '/containers/json', query={'all': 'true'})
    require(not any(c['Id'] != core['Id'] and c['State'] == 'running' and c['ImageID'] == core['Image']
                    for c in all_containers), 'another responder is running')
    raw = private_bytes(config / 'api.env')
    values = env_values(raw)
    require(values.get('GANGOLF_DIVOID_ROOT_ID') == '1' and values.get('GANGOLF_DIVOID_MESSAGES_ID') == '4'
            and values.get('GANGOLF_DIVOID_CONFIG_ID') == '44', 'unexpected original API binding')
    actual = dict(entry.split('=', 1) for entry in api['Config']['Env'])
    require(all(actual.get(key) == value for key, value in values.items()), 'canonical/runtime API environment differs')
    require(any(m.get('Name') == 'gangolf-production-twitch-secrets' and not m['RW']
                and m['Destination'] == '/run/gangolf-credentials' for m in api['Mounts']), 'Twitch mount mismatch')
    services = {}
    for name in ('processor', 'divoid-processor-alpha'):
        service = docker.request('GET', '/containers/' + name + '/json')
        require(service['State']['Running'], 'required dependency stopped')
        services[name] = service['Id']
    return api, core, raw, values, services


def no_pending_intents(app):
    for node in app.values():
        if node['type'] == 'message' and not node.get('content'):
            require(not node['name'].startswith('gangolf-twitch-outbound:'), 'staged outgoing shell requires owner reconciliation')
        if node['type'] == 'message' and node.get('content'):
            payload = strict_json(node['content'])
            if payload.get('schemaVersion') in (3, 4):
                require(payload.get('delivery', {}).get('state') in ('sent', 'submitted', 'failed'), 'outbound intent requires owner reconciliation')


def drain_sample(api_image, started_at):
    def capture(arguments):
        result = subprocess.run(['docker', '--host', 'tcp://gangolf:2375', *arguments],
                                capture_output=True, timeout=30, check=False)
        require(result.returncode == 0, 'drain probe failed')
        return result.stdout + (result.stderr if arguments[0] == 'logs' else b'')
    logs = capture(['logs', '--since', started_at, 'processor'])
    started = logs.count(b'msg="run started"')
    finished = logs.count(b'msg="run finished"')
    require(started == finished, 'Processor run still active or run history ambiguous')
    sockets = capture(['run', '--pull', 'never', '--rm', '--network', 'host', '--read-only',
                       '--entrypoint', 'netstat', api_image, '-tan']).decode()
    for line in sockets.splitlines():
        fields = line.split()
        if len(fields) >= 6 and fields[0] in ('tcp', 'tcp6') and fields[3].rsplit(':', 1)[-1] == '8080':
            require(fields[5] != 'ESTABLISHED', 'Processor connection is still active')
    return started, finished


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['check', 'apply', 'recover', 'verify'])
    parser.add_argument('--journal', type=Path, required=True)
    args = parser.parse_args()
    require(sys.stdin.isatty() and sys.stderr.isatty(), 'interactive terminal required')
    require(os.environ.get('DOCKER_HOST', 'tcp://gangolf:2375') == 'tcp://gangolf:2375', 'unexpected Docker host')
    config = Path.home() / '.codex/secrets/gangolf-production-canonical'
    credential = env_values(private_bytes(Path.home() / '.codex/secrets/.gangolf-divoid-gangolf'))
    require(credential.get('UserId') == '4' and credential.get('Key'), 'principal4 credential required')
    target = HTTP('http://gangolf:8091/api', credential['Key'])
    identify(target, 4)
    admin_key = getpass.getpass('Existing DiVoid administrator key (hidden; memory only): ')
    require(admin_key and '\n' not in admin_key and '\r' not in admin_key, 'invalid admin key')
    admin = HTTP('http://gangolf:8091/api', admin_key)
    identify(admin, admin=True)
    docker = HTTP('http://gangolf:2375', '')
    if args.action in ('recover', 'verify'):
        journal = Journal(args.journal)
        runtime = Runtime(config, journal, docker, credential['Key'])
        migration = Migration(admin, target, journal)
        if args.action == 'verify':
            require(journal.data['phase'] == 'resumed', 'migration not completed')
            migration.verify(False)
            print('preservation_verified=true')
            return
        require(journal.data['phase'] not in ('resumed', 'resuming', 'api-starting', 'traffic-reconciliation-required'), 'traffic may have resumed; owner reconciliation required')
        require(not (journal.data.get('pending') or {}).get('createAPI'), 'unknown API creation outcome; reconcile container ID before recovery')
        runtime.stop(CORE_NAME)
        runtime.stop(journal.data.get('replacement') or journal.data['api_id'])
        runtime.preserve_services()
        if journal.data.get('app'):
            migration.rollback()
        runtime.resume_old()
        print('rollback_verified=true previous_binding_resumed=true')
        return
    require(not args.journal.exists(), 'journal exists; verify/recover instead')
    api, core, raw, values, services = preflight(docker, config)
    old = HTTP('http://gangolf:8091/api', values['GANGOLF_DIVOID_TOKEN'])
    identify(old, 3)
    online = nodes(admin, rootNodeId=1)
    online.update(nodes(admin, id=1))
    no_pending_intents(classify(online))
    require(not nodes(target, id=1) and not nodes(target, id=8), 'principal4 unexpectedly accesses Processor private context')
    print('preflight_ok=true service_still_online=true')
    if args.action == 'check':
        print('app_nodes=' + str(len(classify(online))) + ' production_writes=false')
        return
    drain = drain_sample(api['Image'], core['State']['StartedAt'])
    time.sleep(3)
    require(drain_sample(api['Image'], core['State']['StartedAt']) == drain, 'Processor activity changed during quiet sample')
    require(input('Operations must verify no active Processor turn, send or receive writer; type QUIESCENT to cut over: ') == 'QUIESCENT', 'cutover not confirmed')
    backup_path = args.journal.with_name(args.journal.name + '.api.env')
    require(not backup_path.exists(), 'API environment backup already exists')
    atomic_private(backup_path, raw)
    journal = Journal(args.journal, {'phase': 'prepared', 'api_id': api['Id'], 'api_env_backup': str(backup_path.absolute()),
        'core_id': core['Id'], 'services': services,
        'rollback_name': API_NAME + '-identity-rollback-' + str(time.time_ns())})
    runtime, migration = Runtime(config, journal, docker, credential['Key']), Migration(admin, target, journal)
    try:
        require(drain_sample(api['Image'], core['State']['StartedAt']) == drain, 'Processor activity changed before pause')
        journal.phase('pausing')
        runtime.stop(CORE_NAME)
        require(drain_sample(api['Image'], core['State']['StartedAt']) == drain, 'Processor activity raced with shutdown')
        history_client = LoopbackAPI(api['Image'], env_values(private_bytes(config / 'ai-core.env'))['GANGOLF_API_KEY'])
        history = paged(history_client, '/v3/messages', {'descending': 'false'})
        runtime.stop(API_NAME)
        runtime.stopped()
        runtime.preserve_services()
        snapshot = nodes(admin, rootNodeId=1)
        snapshot.update(nodes(admin, id=1))
        app = classify(snapshot)
        no_pending_intents(app)
        require(snapshot == online, 'online inventory changed; review fresh snapshot and retry with new journal')
        journal.data.update(snapshot=snapshot, app=app, history=history)
        journal.phase('paused')
        migration.stage()
        runtime.stopped()
        migration.move()
        runtime.bind()
        journal.phase('api-starting')
        runtime.start_api()
        wait_ready(api['Image'])
        after = paged(history_client, '/v3/messages', {'descending': 'false'})
        require(after == history, 'retained v3 history differs')
        migration.verify(False)
        require(not nodes(old, id=44) and not nodes(target, id=8), 'principal separation verification failed')
        runtime.preserve_services()
        journal.phase('resuming')
        docker.request('POST', '/containers/' + core['Id'] + '/start')
        journal.phase('resumed')
        print('migration_verified=true retained_v3_history=true responder_started=true')
        print('no_replay_runtime=unverified human_turn=unverified maintenance_gap_possible=true')
    except BaseException:
        phase = journal.data['phase']
        if phase == 'prepared':
            raise Refused('pre-pause verification failed; existing service remains online') from None
        if phase in ('api-starting', 'resuming', 'resumed'):
            for writer in (journal.data['core_id'], journal.data['replacement']):
                try:
                    runtime.stop(writer)
                except BaseException:
                    pass
            journal.phase('traffic-reconciliation-required')
            try:
                runtime.stopped()
            except BaseException:
                raise Refused('API may have accepted traffic; writer state unverified; owner must stop and reconcile') from None
            raise Refused('both writers verified stopped; API may have accepted traffic; explicit owner reconciliation required') from None
        try:
            clean = True
            for writer in (journal.data['core_id'], journal.data.get('replacement') or journal.data['api_id']):
                try:
                    runtime.stop(writer)
                except BaseException:
                    clean = False
            runtime.stopped()
            require(clean, 'writer shutdown requires reconciliation')
            if journal.data.get('app'):
                migration.rollback()
            runtime.resume_old()
        except BaseException:
            for writer in (journal.data['core_id'], journal.data['api_id'], journal.data.get('replacement')):
                if writer:
                    try:
                        runtime.stop(writer)
                    except BaseException:
                        pass
            try:
                runtime.stopped()
            except BaseException:
                raise Refused('cutover incomplete; writer state unverified; inspect and stop journaled writers before recovery') from None
            raise Refused('cutover incomplete; both writers verified stopped; recover with the private journal') from None
        raise Refused('cutover failed; rollback verified and previous binding resumed') from None


if __name__ == '__main__':
    try:
        main()
    except (Exception, KeyboardInterrupt) as error:
        reason = str(error) if isinstance(error, Refused) else 'unexpected failure; inspect private journal and service state'
        print('identity helper stopped: ' + reason, file=sys.stderr)
        sys.exit(1)
