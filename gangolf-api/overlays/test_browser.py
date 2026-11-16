import argparse
import base64
import http.server
import json
import pathlib
import threading
import time
import unittest
import urllib.parse
import urllib.request

ASSETS = pathlib.Path(__file__).parent
FIXTURE_KEY = 'synthetic.channel_read_marker'
state = {}


def message(identifier, channel='main', body='Hello chat', service='twitch'):
    return {'id': str(identifier), 'service': service, 'channel': channel,
            'user': {'name': 'Synthetic viewer'}, 'time': '2026-11-16T12:00:00Z',
            'chunks': [{'type': 0, 'data': body}]}


def envelope(identifier, item):
    return {'schemaVersion': 1, 'id': 'epoch:' + str(identifier),
            'type': 'message.created', 'data': {'resourceVersion': 3, 'message': item}}


class Fixture(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *args):
        pass

    def do_GET(self):
        state = globals()['state']
        parsed = urllib.parse.urlsplit(self.path)
        query = urllib.parse.parse_qs(parsed.query)
        state.setdefault('requests', []).append((parsed.path, query, self.headers.get('Authorization'), self.headers.get('Referer')))
        if parsed.path == '/api/v3/events':
            state['streams'] = state.get('streams', 0) + 1
            if state.get('auth_error'):
                self.respond(401, b'{}', 'application/json')
                return
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.send_header('Cache-Control', 'no-store')
            self.end_headers()
            try:
                self.frame('stream.ready', {'schemaVersion': 1, 'replay': False, 'resyncRequired': True})
                if state.get('malformed'):
                    self.wfile.write(b'event: message.created\ndata: not-json\n\n')
                    self.wfile.flush()
                for event in state.get('events', []):
                    self.frame('message.created', event)
                if state.get('disconnect'):
                    self.close_connection = True
                    return
                while not state.get('shutdown'):
                    if state.get('disconnect_first') and state['streams'] == 1:
                        time.sleep(.5)
                        self.close_connection = True
                        return
                    self.wfile.write(b': heartbeat\n\n')
                    self.wfile.flush()
                    time.sleep(.15)
            except (BrokenPipeError, ConnectionResetError):
                pass
            return
        if parsed.path == '/api/v3/messages':
            state['history_calls'] = state.get('history_calls', 0) + 1
            time.sleep(state.get('history_delay', .1))
            if state.get('history_error'):
                self.respond(state['history_error'], b'{}', 'application/json')
            else:
                self.respond(200, json.dumps({'result': state.get('history', []), 'continue': None}).encode(), 'application/json')
            return
        name = parsed.path.removeprefix('/overlays/')
        if name == 'chat':
            name = 'chat.html'
        if name == 'video':
            name = 'video.html'
        if name in {'chat.html', 'video.html', 'player.html', 'overlay.css', 'shared.js', 'chat.js', 'video.js', 'player.js'} and (ASSETS / name).exists():
            mime = 'text/html' if name.endswith('html') else 'text/css' if name.endswith('css') else 'text/javascript'
            self.respond(200, (ASSETS / name).read_bytes(), mime)
        else:
            self.respond(404, b'{}', 'application/json')

    def frame(self, event, data):
        raw = (': comment\r\nevent: ' + event + '\r\ndata: ' + json.dumps(data, ensure_ascii=False) + '\r\n\r\n').encode()
        for index in range(0, len(raw), 7):
            self.wfile.write(raw[index:index + 7])
            self.wfile.flush()

    def respond(self, code, body, mime):
        self.send_response(code)
        self.send_header('Content-Type', mime)
        self.send_header('Content-Length', str(len(body)))
        self.send_header('Referrer-Policy', 'no-referrer')
        self.send_header('Cache-Control', 'no-store')
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass


class BrowserTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Fixture)
        cls.server.daemon_threads = True
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.origin = 'http://127.0.0.1:' + str(cls.server.server_port)
        cls.session = cls.command('/session', {'capabilities': {'alwaysMatch': {'browserName': 'firefox', 'moz:firefoxOptions': {'args': ['-headless']}}}})['sessionId']

    @classmethod
    def tearDownClass(cls):
        state['shutdown'] = True
        cls.command('/session/' + cls.session, method='DELETE')
        cls.server.shutdown()

    @classmethod
    def command(cls, path, data=None, method='POST'):
        request = urllib.request.Request(WEBDRIVER + path, data=json.dumps(data).encode() if data is not None else None, headers={'Content-Type': 'application/json'}, method=method)
        with urllib.request.urlopen(request, timeout=40) as response:
            return json.load(response)['value']

    def evaluate(self, script):
        return self.command('/session/' + self.session + '/execute/sync', {'script': script, 'args': []})

    def wait_for(self, script, timeout=6):
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            if self.evaluate('return ' + script):
                return
            time.sleep(.05)
        self.fail('Browser assertion did not become true: ' + script)

    def setUp(self):
        global state
        self.command('/session/' + self.session + '/url', {'url': 'about:blank'})
        state['shutdown'] = True
        state = {'history': [], 'events': []}

    def open_chat(self, options=''):
        self.command('/session/' + self.session + '/url', {'url': self.origin + '/overlays/chat?apiKey=' + FIXTURE_KEY + options})

    def test_history_live_race_deduplicates_and_labels_services(self):
        state['history'] = [message(2, 'second', service='youtube'), message(1)]
        state['events'] = [envelope(1, message(2, 'second', service='youtube')), envelope(2, message(3, body='Grüße 👋'))]
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 3")
        self.assertEqual(self.evaluate("return [...document.querySelectorAll('.source')].map(e=>e.textContent)"), ['twitch / main', 'youtube / second', 'twitch / main'])
        self.assertIn('Grüße 👋', self.evaluate('return document.body.textContent'))
        self.assertEqual(state['history_calls'], 1)
        self.assertEqual(state['streams'], 1)

    def test_text_is_literal_bounded_and_creates_no_remote_elements(self):
        attack = '<img src="https://example.invalid/leak" onerror="window.attacked=true">'
        item = message(1, attack, attack + '\n' + '𐐀' * 3000)
        item['user']['name'] = attack
        item['chunks'].append({'type': 1, 'code': '<script>bad()</script>'})
        state['history'] = [item]
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 1")
        self.assertIn(attack, self.evaluate('return document.body.textContent'))
        self.assertEqual(self.evaluate("return document.querySelectorAll('img,script[src^=\"http\"]').length"), 0)
        self.assertEqual(self.evaluate("return Array.from(document.querySelector('.content').textContent).length"), 2048)
        self.assertFalse(self.evaluate('return Boolean(window.attacked)'))

    def test_message_bound_and_bearer_only_requests(self):
        state['events'] = [envelope(i, message(i)) for i in range(1, 76)]
        self.open_chat('&service=twitch&channel=main')
        self.wait_for("document.querySelectorAll('.message').length === 50")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 50)
        api_requests = [request for request in state['requests'] if request[0].startswith('/api/')]
        self.assertEqual(len(api_requests), 2)
        for _, query, auth, referrer in api_requests:
            self.assertEqual(auth, 'Bearer ' + FIXTURE_KEY)
            self.assertNotIn('apiKey', query)
            self.assertEqual(query['service'], ['twitch'])
            self.assertEqual(query['channel'], ['main'])
            self.assertIsNone(referrer)
        self.assertEqual(self.evaluate('return localStorage.length + sessionStorage.length'), 0)

    def test_unauthorized_stream_stops_without_retry_or_key_echo(self):
        state['auth_error'] = True
        self.open_chat()
        self.wait_for("document.getElementById('status').textContent.includes('Key invalid')")
        time.sleep(1.4)
        self.assertEqual(state['streams'], 1)
        self.assertNotIn(FIXTURE_KEY, self.evaluate('return document.documentElement.outerHTML'))
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 0)

    def test_unauthorized_history_also_stops(self):
        state['history_error'] = 403
        self.open_chat()
        self.wait_for("document.getElementById('status').textContent.includes('Key invalid')")
        time.sleep(1.4)
        self.assertEqual(state['streams'], 1)

    def test_invalid_query_never_opens_api(self):
        self.open_chat('&channel=main')
        self.wait_for("document.getElementById('status').textContent.includes('required')")
        self.assertFalse(any(request[0].startswith('/api/') for request in state['requests']))

    def test_malformed_stream_clears_and_reconnects(self):
        state['malformed'] = True
        self.open_chat()
        self.wait_for("document.getElementById('status').textContent.includes('Reconnecting')")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 0)
        state['malformed'] = False
        state['history'] = [message(1, body='Recovered')]
        self.wait_for("document.querySelector('.content')?.textContent === 'Recovered'")
        self.assertEqual(state['streams'], 2)

    def test_bootstrap_overflow_restarts_without_rendering_partial_history(self):
        state['history_delay'] = 2
        state['events'] = [envelope(i, message(i)) for i in range(1, 258)]
        self.open_chat()
        self.wait_for("document.getElementById('status').textContent.includes('Reconnecting')")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 0)
        state['history_delay'] = .1
        state['events'] = []
        state['history'] = [message(300, body='Fresh after overflow')]
        self.wait_for("document.querySelector('.content')?.textContent === 'Fresh after overflow'")
        self.assertEqual(state['streams'], 2)

    def test_duplicate_live_events_render_one_v3_message(self):
        item = message(1, body='Legacy live message')
        event = envelope(1, item)
        state['events'] = [event, event, envelope(2, message(1))]
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 1")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 1)

    def test_reconnect_clears_stale_rows_and_refreshes_context(self):
        state['history'] = [message(1, body='Stale row')]
        state['disconnect_first'] = True
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 1")
        self.wait_for("document.getElementById('status').textContent.includes('Reconnecting')")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 0)
        state['history'] = [message(2, body='Fresh context')]
        self.wait_for("document.querySelector('.content')?.textContent === 'Fresh context'")
        self.assertEqual(state['streams'], 2)
        self.assertEqual(state['history_calls'], 2)

    def test_transparent_responsive_preview(self):
        state['history'] = [message(3, 'studio', 'A clear multistream overlay. Welcome to the stream!', 'youtube'), message(2, 'main', 'Second channel, same conversation.'), message(1, 'main', 'Synthetic preview · no real chat or credentials')]
        self.command('/session/' + self.session + '/window/rect', {'width': 1920, 'height': 1080})
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 3")
        self.assertEqual(self.evaluate("return getComputedStyle(document.body).backgroundColor"), 'rgba(0, 0, 0, 0)')
        if ARTIFACTS:
            pathlib.Path(ARTIFACTS).mkdir(parents=True, exist_ok=True)
            image = self.command('/session/' + self.session + '/screenshot', method='GET')
            (pathlib.Path(ARTIFACTS) / 'chat-1920.png').write_bytes(base64.b64decode(image))
        self.command('/session/' + self.session + '/window/rect', {'width': 480, 'height': 640})
        self.assertTrue(self.evaluate('return document.documentElement.scrollWidth <= innerWidth'))
        self.assertEqual(self.evaluate("return getComputedStyle(document.documentElement).fontSize"), '22px')
        if ARTIFACTS:
            image = self.command('/session/' + self.session + '/screenshot', method='GET')
            (pathlib.Path(ARTIFACTS) / 'chat-480.png').write_bytes(base64.b64decode(image))


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--webdriver', default='http://127.0.0.1:4449')
    parser.add_argument('--artifacts')
    options, remaining = parser.parse_known_args()
    WEBDRIVER = options.webdriver
    ARTIFACTS = options.artifacts
    unittest.main(argv=[__file__] + remaining)
