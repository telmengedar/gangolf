import datetime
import http.server
import json
import pathlib
import threading
import time
import unittest
import urllib.parse

from playwright.sync_api import sync_playwright

ASSETS = pathlib.Path(__file__).parent
KEY = 'synthetic.channel_read_marker'

SDK = '''
window.fixturePrivacy={};
for (const [name,read] of Object.entries({location:()=>parent.location.href,document:()=>parent.document.body,fetch:()=>parent.fetch})) {
  try {read();fixturePrivacy[name]=false} catch {fixturePrivacy[name]=true}
}
window.fixtureIncoming=[];
window.addEventListener('message',e=>fixtureIncoming.push(e.data));
window.YT={PlayerState:{PLAYING:1,ENDED:0},Player:function(id,options){
  const p=this;window.fixturePlayer=p;p.loads=[];p.time=0;p.destroyed=false;
  p.unMute=()=>{p.audible=true};p.setVolume=v=>{p.volume=v};
  p.getCurrentTime=()=>p.time;p.stopVideo=()=>{};
  p.destroy=()=>{p.destroyed=true};
  p.end=()=>options.events.onStateChange({data:0});
  p.error=()=>options.events.onError({data:153});
  p.block=()=>options.events.onAutoplayBlocked();
  p.playVideo=()=>options.events.onStateChange({data:1});
  p.loadVideoById=input=>{p.loads.push(input);p.time=input.startSeconds;
    if (MODE==='blocked') p.block();else if(MODE==='error') p.error();else p.playVideo();};
  if(MODE!=='hang')setTimeout(()=>options.events.onReady({target:p}),0);
}};
window.onYouTubeIframeAPIReady();
'''


class Fixture(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *args):
        pass

    def do_GET(self):
        current = self.server.current
        path = urllib.parse.urlsplit(self.path).path
        current['requests'].append({'path': path, 'query': urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query), 'auth': self.headers.get('Authorization'), 'referer': self.headers.get('Referer')})
        if path == '/api/v3/events':
            current['streams'] += 1
            if current.get('auth_error'):
                self.send_response(401)
                self.send_header('Content-Length', '0')
                self.end_headers()
                return
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.end_headers()
            try:
                self.write_frame('stream.ready', {'schemaVersion': 1, 'replay': False, 'resyncRequired': True})
                index = 0
                while not current['stop']:
                    if current.get('disconnect'):
                        current['disconnect'] = False
                        self.close_connection = True
                        return
                    while index < len(current['events']):
                        event = current['events'][index]
                        index += 1
                        self.write_frame(event['type'], event)
                    time.sleep(.02)
            except (BrokenPipeError, ConnectionResetError):
                pass
            return
        name = path.removeprefix('/overlays/')
        if name == 'video':
            name = 'video.html'
        if name not in {'video.html', 'player.html', 'overlay.css', 'shared.js', 'video.js', 'player.js'}:
            self.send_error(404)
            return
        body = (ASSETS / name).read_text()
        body = body.replace('{{.PlayerOrigin}}', self.server.player_origin).replace('{{.ParentOrigin}}', self.server.parent_origin)
        mime = 'text/html' if name.endswith('html') else 'text/css' if name.endswith('css') else 'text/javascript'
        raw = body.encode()
        self.send_response(200)
        self.send_header('Content-Type', mime)
        self.send_header('Content-Length', str(len(raw)))
        self.send_header('Referrer-Policy', 'strict-origin-when-cross-origin' if name == 'player.html' else 'no-referrer')
        if name == 'player.html':
            self.send_header('Content-Security-Policy', "default-src 'none'; script-src 'self' https://www.youtube.com https://s.ytimg.com; style-src 'self'; frame-src https://www.youtube.com; connect-src 'none'; frame-ancestors " + self.server.parent_origin + "; object-src 'none'; base-uri 'none'; form-action 'none'")
        elif name == 'video.html':
            self.send_header('Content-Security-Policy', "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-src " + self.server.player_origin + "; object-src 'none'; base-uri 'none'; form-action 'none'")
        self.end_headers()
        try:
            self.wfile.write(raw)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def write_frame(self, event, data):
        self.wfile.write(('event: ' + event + '\ndata: ' + json.dumps(data) + '\n\n').encode())
        self.wfile.flush()


class VideoBrowserTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.servers = [http.server.ThreadingHTTPServer(('127.0.0.1', 0), Fixture) for _ in range(2)]
        cls.parent_origin, cls.player_origin = ['http://127.0.0.1:' + str(server.server_port) for server in cls.servers]
        for server in cls.servers:
            server.daemon_threads = True
            server.parent_origin = cls.parent_origin
            server.player_origin = cls.player_origin
            threading.Thread(target=server.serve_forever, daemon=True).start()
        cls.playwright = sync_playwright().start()
        cls.browser = cls.playwright.chromium.launch(headless=True)

    @classmethod
    def tearDownClass(cls):
        cls.browser.close()
        cls.playwright.stop()
        for server in cls.servers:
            server.shutdown()

    def setUp(self):
        self.state = {'events': [], 'requests': [], 'streams': 0, 'stop': False}
        for server in self.servers:
            server.current = self.state
        self.mode = 'playing'
        self.provider_requests = []
        self.context = self.browser.new_context(viewport={'width': 1280, 'height': 720})
        self.context.route('https://www.youtube.com/iframe_api', self.sdk)
        self.page = self.context.new_page()
        self.page.goto(self.parent_origin + '/overlays/video?apiKey=' + KEY)
        self.page.wait_for_function("()=>document.getElementById('status').hidden")

    def tearDown(self):
        self.state['stop'] = True
        self.context.close()

    def sdk(self, route):
        self.provider_requests.append({'url': route.request.url, 'headers': route.request.all_headers()})
        route.fulfill(status=200, content_type='text/javascript', body='const MODE=' + json.dumps(self.mode) + ';' + SDK)

    def event(self, number, **changes):
        now = self.page.evaluate('Date.now()')
        event = {'schemaVersion': 1, 'id': 'epoch:' + str(number), 'type': 'video.triggered', 'time': datetime.datetime.fromtimestamp(now / 1000, datetime.UTC).isoformat(), 'data': {'clip': {'provider': 'youtube', 'remoteVideoId': 'M7lc1UVf-VE', 'startSeconds': number, 'endSeconds': number + 3, 'title': 'PRIVATE_TITLE_MUST_STAY_IN_PARENT'}, 'trigger': {'text': 'PRIVATE_TRIGGER'}}}
        event.update(changes)
        return event

    def emit(self, event):
        self.state['events'].append(event)

    def player(self, start=None):
        until = time.monotonic() + 5
        while time.monotonic() < until:
            self.page.wait_for_timeout(20)
            for frame in self.page.frames:
                if '/overlays/player.html' in frame.url:
                    try:
                        if frame.evaluate('Boolean(window.fixturePlayer?.loads.length)') and (start is None or frame.evaluate('fixturePlayer.loads[0].startSeconds') == start):
                            return frame
                    except Exception:
                        pass
        self.fail('Expected mock provider clip did not start')

    def test_valid_snapshot_starts_audible_range_and_fifo_advances(self):
        self.emit(self.event(1))
        first = self.player(1)
        self.assertEqual(first.evaluate('fixturePlayer.loads[0]'), {'videoId': 'M7lc1UVf-VE', 'startSeconds': 1, 'endSeconds': 4})
        self.assertEqual(first.evaluate('[fixturePlayer.audible,fixturePlayer.volume]'), [True, 100])
        self.emit(self.event(2))
        self.page.wait_for_timeout(60)
        self.assertEqual(len(self.provider_requests), 1)
        first.evaluate('fixturePlayer.end()')
        second = self.player(2)
        self.assertNotEqual(first, second)

    def test_provider_boundary_has_no_credentials_or_private_text(self):
        self.emit(self.event(1))
        frame = self.player()
        self.assertEqual(frame.evaluate('fixturePrivacy'), {'location': True, 'document': True, 'fetch': True})
        incoming = frame.evaluate('fixtureIncoming')
        self.assertEqual(list(incoming[-1]['clip']), ['remoteVideoId', 'startSeconds', 'endSeconds'])
        serialized = json.dumps(incoming) + json.dumps(self.provider_requests)
        for private in [KEY, 'PRIVATE_TITLE', 'PRIVATE_TRIGGER', 'apiKey']:
            self.assertNotIn(private, serialized)
        self.assertEqual(self.provider_requests[0]['headers'].get('referer'), self.player_origin + '/')
        self.assertEqual(urllib.parse.urlsplit(frame.url).query, '')
        self.assertEqual(self.page.locator('iframe').get_attribute('sandbox'), 'allow-scripts allow-same-origin')
        self.assertEqual(self.page.evaluate('localStorage.length + sessionStorage.length'), 0)
        self.assertNotIn(KEY, self.page.content())
        api_requests = [request for request in self.state['requests'] if request['path'].startswith('/api/')]
        self.assertEqual(len(api_requests), 1)
        self.assertEqual(api_requests[0]['query'], {'types': ['video.triggered']})
        self.assertEqual(api_requests[0]['auth'], 'Bearer ' + KEY)
        self.assertIsNone(api_requests[0]['referer'])

    def test_same_origin_player_configuration_fails_before_api_or_sdk(self):
        streams = self.state['streams']
        for server in self.servers:
            server.player_origin = self.parent_origin
        try:
            self.page.goto(self.parent_origin + '/overlays/video?apiKey=' + KEY)
            self.page.wait_for_function("()=>document.getElementById('status').textContent.includes('separate trusted')")
            self.assertEqual(self.state['streams'], streams)
            self.assertEqual(len(self.provider_requests), 0)
        finally:
            for server in self.servers:
                server.player_origin = self.player_origin

    def test_wrong_source_nonce_generation_and_unknown_status_do_not_advance(self):
        self.emit(self.event(1))
        frame = self.player(1)
        self.emit(self.event(2))
        self.page.wait_for_timeout(60)
        init = frame.evaluate('fixtureIncoming[0]')
        status = {key: value for key, value in init.items() if key != 'clip'}
        status.update(type='status', state='ended')
        self.page.evaluate('(data)=>window.postMessage(data,location.origin)', status)
        for delta in [{'nonce': '0' * 32}, {'generation': init['generation'] + 1}, {'state': 'navigate'}, {'url': 'https://example.invalid'}]:
            frame.evaluate('(data)=>parent.postMessage(data,' + json.dumps(self.parent_origin) + ')', {**status, **delta})
        self.page.wait_for_timeout(100)
        self.assertEqual(len(self.provider_requests), 1)
        self.assertEqual(frame.evaluate('fixturePlayer.loads[0].startSeconds'), 1)
        frame.evaluate('fixturePlayer.end()')
        self.player(2)

    def test_child_rejects_stop_from_wrong_source_nonce_and_generation(self):
        self.emit(self.event(1))
        frame = self.player(1)
        init = frame.evaluate('fixtureIncoming[0]')
        stop = {name: init[name] for name in ['version', 'nonce', 'generation']}
        stop['type'] = 'stop'
        frame.evaluate('(data)=>window.postMessage(data,location.origin)', stop)
        for delta in [{'nonce': '0' * 32}, {'generation': init['generation'] + 1}, {'extra': True}]:
            self.page.evaluate('(args)=>document.querySelector("iframe").contentWindow.postMessage(args.data,args.origin)', {'data': {**stop, **delta}, 'origin': self.player_origin})
        self.page.wait_for_timeout(100)
        self.assertFalse(frame.evaluate('fixturePlayer.destroyed'))
        self.page.evaluate('(args)=>document.querySelector("iframe").contentWindow.postMessage(args.data,args.origin)', {'data': stop, 'origin': self.player_origin})
        frame.wait_for_function('()=>fixturePlayer.destroyed')

    def test_invalid_provider_id_times_and_stale_intents_never_load_sdk(self):
        cases = [{'provider': 'other'}, {'remoteVideoId': 'https://bad.invalid'}, {'startSeconds': -1}, {'endSeconds': 0}, {'startSeconds': '1'}, {'endSeconds': None, 'startSeconds': None, 'remoteVideoId': '<script>x</script>'}]
        for number, changes in enumerate(cases, 1):
            event = self.event(number)
            event['data']['clip'].update(changes)
            self.emit(event)
        self.emit(self.event(20, time='2000-01-01T00:00:00Z'))
        self.page.wait_for_timeout(200)
        self.assertEqual(len(self.provider_requests), 0)
        self.assertEqual(self.page.locator('iframe').count(), 0)
        self.emit(self.event(21))
        self.player(21)

    def test_queue_bounds_32_pending_and_suppresses_duplicate_events(self):
        for number in range(1, 36):
            self.emit(self.event(number))
        self.emit(self.event(1))
        for number in range(1, 34):
            frame = self.player(number)
            frame.evaluate('fixturePlayer.end()')
        self.page.wait_for_timeout(150)
        self.assertEqual(len(self.provider_requests), 33)
        self.assertEqual(self.page.locator('iframe').count(), 0)

    def test_queued_intents_expire_after_120_seconds(self):
        self.page.clock.install()
        self.emit(self.event(1))
        frame = self.player(1)
        self.emit(self.event(2))
        self.page.wait_for_timeout(60)
        self.page.clock.fast_forward(120001)
        frame.evaluate('fixturePlayer.end()')
        self.page.wait_for_timeout(100)
        self.assertEqual(self.page.locator('iframe').count(), 0)
        self.assertEqual(len(self.provider_requests), 1)

    def test_deduplication_window_is_bounded_to_256_event_ids(self):
        first = self.event(1)
        self.emit(first)
        frame = self.player(1)
        for number in range(2, 259):
            self.emit(self.event(number, time='2000-01-01T00:00:00Z'))
        self.page.wait_for_timeout(150)
        frame.evaluate('fixturePlayer.end()')
        self.emit(first)
        self.player(1)
        self.assertEqual(len(self.provider_requests), 2)

    def test_provider_error_advances_and_blocked_requires_interaction(self):
        self.emit(self.event(1))
        frame = self.player(1)
        self.emit(self.event(2))
        self.page.wait_for_timeout(60)
        frame.evaluate('fixturePlayer.error()')
        second = self.player(2)
        second.evaluate('fixturePlayer.block()')
        self.page.get_by_role('button', name='Skip video').wait_for(state='visible')
        second.get_by_role('button', name='Start playback').wait_for(state='visible')
        second.get_by_role('button', name='Start playback').click()
        self.page.wait_for_function("()=>document.getElementById('actions').hidden && document.getElementById('status').hidden")
        self.assertEqual(second.evaluate('fixturePlayer.audible'), True)

    def test_startup_timeout_and_hard_watchdog_destroy_frames(self):
        self.page.clock.install()
        self.mode = 'hang'
        self.emit(self.event(1))
        self.page.wait_for_function("()=>document.querySelector('iframe') !== null")
        self.page.clock.fast_forward(20001)
        self.assertEqual(self.page.locator('iframe').count(), 0)
        self.assertIn('did not start', self.page.locator('#status').inner_text())
        self.mode = 'playing'
        self.emit(self.event(2))
        self.player(2)
        self.page.clock.fast_forward(300001)
        self.assertEqual(self.page.locator('iframe').count(), 0)
        self.assertIn('time limit', self.page.locator('#status').inner_text())

    def test_end_time_fallback_and_skip_leave_no_old_frame(self):
        self.emit(self.event(1))
        frame = self.player(1)
        self.emit(self.event(2))
        self.page.wait_for_timeout(60)
        frame.evaluate('fixturePlayer.time=4')
        self.player(2)
        self.emit(self.event(3))
        self.page.wait_for_timeout(60)
        self.page.get_by_role('button', name='Skip video').is_hidden()
        current = self.player(2)
        current.evaluate('fixturePlayer.block()')
        self.page.get_by_role('button', name='Skip video').click()
        self.player(3)

    def test_disconnect_stops_current_drops_queue_and_reconnects_empty(self):
        self.emit(self.event(1))
        self.player(1)
        self.emit(self.event(2))
        self.page.wait_for_timeout(60)
        self.state['disconnect'] = True
        self.page.wait_for_function("()=>document.getElementById('status').textContent.includes('Reconnecting')")
        self.assertEqual(self.page.locator('iframe').count(), 0)
        self.state['events'] = []
        self.page.wait_for_function("()=>document.getElementById('status').hidden")
        self.assertEqual(self.page.locator('iframe').count(), 0)
        self.assertEqual(len(self.provider_requests), 1)
        self.assertEqual(self.state['streams'], 2)

    def test_revocation_clears_playback_and_stops_reconnecting(self):
        self.emit(self.event(1))
        self.player(1)
        self.emit(self.event(2))
        self.page.wait_for_timeout(60)
        self.state['auth_error'] = True
        self.state['disconnect'] = True
        self.page.wait_for_function("()=>document.getElementById('status').textContent.includes('Key invalid')")
        self.assertEqual(self.page.locator('iframe').count(), 0)
        self.page.wait_for_timeout(1400)
        self.assertEqual(self.state['streams'], 2)
        self.assertEqual(len(self.provider_requests), 1)


if __name__ == '__main__':
    unittest.main()
