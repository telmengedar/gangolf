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
                for event in state.get('before_ready', []):
                    self.frame('message.created', event)
                self.frame('stream.ready', {'schemaVersion': 1, 'replay': False, 'resyncRequired': True})
                if state.get('malformed'):
                    self.wfile.write(b'event: message.created\ndata: not-json\n\n')
                    self.wfile.flush()
                if state.get('disconnect'):
                    self.close_connection = True
                    return
                index = 0
                while not state.get('shutdown'):
                    if state.pop('reset', False):
                        self.frame('stream.reset', {'schemaVersion': 1})
                    while index < len(state.get('events', [])):
                        event = state['events'][index]
                        index += 1
                        self.frame('message.created', event)
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
        self.send_header('Content-Security-Policy', "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-src 'self'; base-uri 'none'; form-action 'none'; object-src 'none'")
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
        if CHROMIUM:
            from playwright.sync_api import sync_playwright
            cls.playwright = sync_playwright().start()
            cls.browser = cls.playwright.chromium.launch(headless=True)
            cls.page = cls.browser.new_page()
        cls.session = cls.command('/session', {'capabilities': {'alwaysMatch': {'browserName': 'firefox', 'moz:firefoxOptions': {'args': ['-headless']}}}})['sessionId']

    @classmethod
    def tearDownClass(cls):
        state['shutdown'] = True
        cls.command('/session/' + cls.session, method='DELETE')
        cls.server.shutdown()

    @classmethod
    def command(cls, path, data=None, method='POST'):
        if CHROMIUM:
            if path == '/session':
                return {'sessionId': 'chromium'}
            if path.endswith('/url'):
                cls.page.goto(data['url'])
            elif path.endswith('/execute/sync'):
                return cls.page.evaluate('()=>{' + data['script'] + '}')
            elif path.endswith('/window/rect'):
                cls.page.set_viewport_size(data)
            elif path.endswith('/screenshot'):
                return base64.b64encode(cls.page.screenshot(omit_background=True)).decode()
            elif method == 'DELETE':
                cls.browser.close()
                cls.playwright.stop()
            return None
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
        if CHROMIUM:
            self.page.close()
            type(self).page = self.browser.new_page()
        else:
            self.command('/session/' + self.session + '/url', {'url': 'about:blank'})
        state['shutdown'] = True
        state = {'history': [], 'events': []}

    def open_chat(self, options=''):
        self.command('/session/' + self.session + '/url', {'url': self.origin + '/overlays/chat?apiKey=' + FIXTURE_KEY + options})

    def test_live_only_ignores_existing_history_and_displays_new_event(self):
        state['history'] = [message(1, body='Historical row must not display')]
        state['events'] = [envelope(2, message(2, body='New live row'))]
        self.open_chat()
        self.wait_for("document.body.textContent.includes('New live row')")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 1)
        self.assertNotIn('Historical row must not display', self.evaluate('return document.body.textContent'))
        self.assertEqual(state.get('history_calls', 0), 0)

    def test_two_services_and_split_utf8_frames_keep_channel_cues(self):
        state['events'] = [envelope(1, message(1)), envelope(2, message(2, 'second', 'Grüße', 'youtube'))]
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 2")
        self.assertEqual(self.evaluate("return [...document.querySelectorAll('.source')].map(e=>e.textContent)"), ['twitch / main', 'youtube / second'])
        self.assertIn('Grüße', self.evaluate('return document.body.textContent'))
        self.assertEqual(state.get('history_calls', 0), 0)

    def test_text_is_literal_bounded_and_creates_no_remote_elements(self):
        attack = '<img src="https://example.invalid/leak" onerror="window.attacked=true">'
        item = message(1, attack, attack + '\n' + '𐐀' * 3000)
        item['user']['name'] = attack
        item['chunks'].append({'type': 1, 'code': '<script>bad()</script>'})
        state['events'] = [envelope(1, item)]
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 1")
        self.assertIn(attack, self.evaluate('return document.body.textContent'))
        self.assertEqual(self.evaluate("return document.querySelectorAll('img,script[src^=\"http\"]').length"), 0)
        self.assertEqual(self.evaluate("return Array.from(document.querySelector('.content').textContent).length"), 2048)
        self.assertFalse(self.evaluate('return Boolean(window.attacked)'))

    def test_message_bound_and_bearer_only_single_stream(self):
        state['events'] = [envelope(i, message(i)) for i in range(1, 76)]
        self.open_chat('&service=twitch&channel=main')
        self.wait_for("document.querySelectorAll('.message').length === 50")
        api_requests = [request for request in state['requests'] if request[0].startswith('/api/')]
        self.assertEqual(len(api_requests), 1)
        path, query, auth, referrer = api_requests[0]
        self.assertEqual(path, '/api/v3/events')
        self.assertEqual(auth, 'Bearer ' + FIXTURE_KEY)
        self.assertEqual(query, {'service': ['twitch'], 'channel': ['main'], 'types': ['message.created']})
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

    def test_invalid_query_never_opens_api(self):
        self.open_chat('&channel=main')
        self.wait_for("document.getElementById('status').textContent.includes('required')")
        self.assertFalse(any(request[0].startswith('/api/') for request in state['requests']))

    def test_server_bounded_long_query_is_not_rejected_by_client(self):
        state['events'] = [envelope(1, message(1))]
        self.command('/session/' + self.session + '/url', {'url': self.origin + '/overlays/chat?apiKey=id.' + 'A' * 4093 + '&service=' + '%E6%97%A5' * 256})
        self.wait_for("document.querySelectorAll('.message').length === 1")
        self.assertEqual(state.get('history_calls', 0), 0)

    def test_data_before_ready_and_legacy_v2_are_ignored(self):
        state['before_ready'] = [envelope(1, message(1, body='Before ready'))]
        state['events'] = [{'schemaVersion': 1, 'id': 'epoch:2', 'type': 'message.created', 'data': {'resourceVersion': 2, 'batchId': '2', 'index': 0, 'message': message(2, body='Legacy excluded')}}, envelope(3, message(3, body='Eligible live'))]
        self.open_chat()
        self.wait_for("document.body.textContent.includes('Eligible live')")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 1)

    def test_malformed_stream_clears_then_reconnects_without_history(self):
        state['malformed'] = True
        self.open_chat()
        self.wait_for("document.getElementById('status').textContent.includes('Reconnecting')")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 0)
        state['malformed'] = False
        state['events'] = [envelope(1, message(1, body='Recovered live'))]
        self.wait_for("document.querySelector('.content')?.textContent === 'Recovered live'")
        self.assertEqual(state['streams'], 2)
        self.assertEqual(state.get('history_calls', 0), 0)

    def test_reconnect_retains_event_and_resource_dedupe_without_revival(self):
        state['events'] = [envelope(1, message(1, body='Original live'))]
        state['disconnect_first'] = True
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 1")
        self.wait_for("document.getElementById('status').textContent.includes('Reconnecting')")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 0)
        state['events'] = [envelope(1, message(1, body='Original live')), envelope(2, message(1, body='Duplicate resource')), envelope(3, message(3, body='New after reconnect'))]
        self.wait_for("document.body.textContent.includes('New after reconnect')")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 1)
        self.assertEqual(state.get('history_calls', 0), 0)

    def test_old_and_future_source_times_use_receipt_lifetime(self):
        if not CHROMIUM: self.skipTest('Controlled clock requires Chromium')
        self.page.clock.install()
        old = envelope(1, message(1)); old['time'] = '2000-01-01T00:00:00Z'
        future = envelope(2, message(2)); future['data']['message']['time'] = '2100-01-01T00:00:00Z'
        state['events'] = [old, future]
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 2")
        self.page.clock.fast_forward(7900)
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 2)
        self.page.clock.fast_forward(1200)
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 0)

    def test_stream_reset_clears_without_history_or_duplicate_revival(self):
        state['events'] = [envelope(1, message(1, body='Before reset'))]
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 1")
        state['reset'] = True
        self.wait_for("document.getElementById('status').textContent.includes('Reconnecting')")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 0)
        state['events'].append(envelope(2, message(2, body='New after reset')))
        self.wait_for("document.body.textContent.includes('New after reset')")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 1)
        self.assertEqual(state.get('history_calls', 0), 0)

    def test_both_dedupe_windows_evict_after_256_recent_ids(self):
        state['events'] = [envelope(i, message(i)) for i in range(1, 258)]
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 50")
        state['events'].append(envelope(1, message(1, body='New lifetime after bounded eviction')))
        self.wait_for("document.body.textContent.includes('New lifetime after bounded eviction')")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 50)

    def test_actual_fade_and_new_arrival_does_not_restart_old_animation(self):
        state['events'] = [envelope(1, message(1, body='First fading row'))]
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 1")
        self.evaluate("window.originalRow=document.querySelector('.message'); return true")
        self.assertEqual(self.evaluate("return Number(getComputedStyle(originalRow).opacity)"), 1)
        time.sleep(7.7)
        self.assertEqual(self.evaluate("return Number(getComputedStyle(originalRow).opacity)"), 1)
        time.sleep(.55)
        fading = self.evaluate("return Number(getComputedStyle(originalRow).opacity)")
        self.assertGreater(fading, 0)
        self.assertLess(fading, 1)
        state['events'].append(envelope(2, message(2, body='Second fresh row')))
        self.wait_for("document.querySelectorAll('.message').length === 2")
        self.assertTrue(self.evaluate("return originalRow === document.querySelector('.message')"))
        time.sleep(.15)
        self.assertLess(self.evaluate("return Number(getComputedStyle(originalRow).opacity)"), fading)
        self.assertEqual(self.evaluate("return Number(getComputedStyle(document.querySelectorAll('.message')[1]).opacity)"), 1)
        self.wait_for("!originalRow.isConnected", timeout=2)
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 1)

    def test_reduced_motion_keeps_deadline_without_animation(self):
        if not CHROMIUM: self.skipTest('Controlled clock requires Chromium')
        self.page.emulate_media(reduced_motion='reduce')
        self.page.clock.install()
        state['events'] = [envelope(1, message(1))]
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 1")
        self.assertEqual(self.evaluate("return getComputedStyle(document.querySelector('.message')).animationName"), 'none')
        self.page.clock.fast_forward(9100)
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 0)
        self.page.emulate_media(reduced_motion='no-preference')

    def test_visibility_resume_prunes_overdue_and_expired_id_does_not_reappear(self):
        state['events'] = [envelope(1, message(1))]
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 1")
        self.evaluate("const now=performance.now.bind(performance); performance.now=()=>now()+10000; document.dispatchEvent(new Event('visibilitychange')); return true")
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 0)
        state['events'].extend([envelope(1, message(1)), envelope(2, message(1))])
        time.sleep(.2)
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 0)

    def test_teardown_cancels_expiry_work_and_rows(self):
        if not CHROMIUM: self.skipTest('Controlled clock requires Chromium')
        self.page.clock.install()
        self.page.add_init_script("window.fixtureTimers=new Set; const schedule=window.setTimeout; const cancel=window.clearTimeout; window.setTimeout=(fn,delay,...args)=>{let id=schedule(()=>{fixtureTimers.delete(id);fn(...args)},delay);fixtureTimers.add(id);return id};window.clearTimeout=id=>{fixtureTimers.delete(id);cancel(id)}")
        state['events'] = [envelope(i, message(i)) for i in range(1, 60)]
        self.open_chat()
        self.wait_for("document.querySelectorAll('.message').length === 50")
        self.assertEqual(self.evaluate('return fixtureTimers.size'), 1)
        self.evaluate("window.dispatchEvent(new PageTransitionEvent('pagehide')); return true")
        self.assertEqual(self.evaluate('return fixtureTimers.size'), 0)
        self.assertEqual(self.evaluate("return document.querySelectorAll('.message').length"), 0)

    def test_transparent_responsive_preview(self):
        state['events'] = [envelope(1, message(1, 'main', 'Synthetic preview · new live messages only')), envelope(2, message(2, 'main', 'Eight seconds, then fade away.')), envelope(3, message(3, 'studio', 'One conversation across channels.', 'youtube'))]
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
    parser.add_argument('--chromium', action='store_true')
    options, remaining = parser.parse_known_args()
    WEBDRIVER = options.webdriver
    ARTIFACTS = options.artifacts
    CHROMIUM = options.chromium
    unittest.main(argv=[__file__] + remaining)
