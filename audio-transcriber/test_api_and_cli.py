"""Tests for send_to_api, endpoint configuration, request timeouts, the --device exit codes and the probe's stderr handling."""

import contextlib
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch

import numpy as np
import requests

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import audio_transcriber as at


def _is_finite_timeout_pair(timeout):
    return (isinstance(timeout, tuple) and len(timeout) == 2
            and all(isinstance(value, (int, float)) and value > 0 for value in timeout))


class TestSendToApi(unittest.TestCase):

    def _send(self, text="hello", status_code=200, body="ok"):
        with patch('audio_transcriber.requests.post') as post:
            post.return_value = MagicMock(status_code=status_code, text=body)
            return at.send_to_api(text, "KEY"), post

    def test_posts_message_list_to_messages_endpoint_with_bearer_key(self):
        ok, post = self._send("hello world")
        self.assertTrue(ok)
        self.assertEqual(post.call_args[0][0], at.GANGOLF_API_URL + "/v1/messages")
        headers = post.call_args[1]['headers']
        self.assertEqual(headers['Authorization'], "Bearer KEY")
        self.assertEqual(headers['Content-Type'], "application/json")
        body = json.loads(post.call_args[1]['data'])
        self.assertIsInstance(body, list)
        self.assertEqual(len(body), 1)
        self.assertEqual(body[0]['chunks'], [{"type": 0, "data": "hello world"}])
        self.assertEqual(body[0]['user'], {"name": "Rugenforth", "service": "localhost", "serviceId": "host"})
        self.assertEqual(body[0]['service'], "localhost")
        self.assertEqual(body[0]['channel'], "host")

    def test_200_and_201_succeed(self):
        self.assertTrue(self._send(status_code=200)[0])
        self.assertTrue(self._send(status_code=201)[0])

    def test_other_status_fails(self):
        ok, _ = self._send(status_code=500, body="boom")
        self.assertFalse(ok)

    def test_empty_text_is_not_posted(self):
        ok, post = self._send(text="")
        self.assertFalse(ok)
        post.assert_not_called()

    def test_request_exception_returns_false(self):
        with patch('audio_transcriber.requests.post', side_effect=requests.exceptions.Timeout("slow")):
            self.assertFalse(at.send_to_api("hello", "KEY"))

    def test_message_request_carries_finite_connect_and_read_timeouts(self):
        _, post = self._send()
        self.assertEqual(post.call_args[1].get('timeout'), at.API_TIMEOUT)
        self.assertTrue(_is_finite_timeout_pair(at.API_TIMEOUT))


class TestEndpointResolution(unittest.TestCase):

    def _endpoints(self, **environment):
        env = {key: value for key, value in os.environ.items()
               if key not in ("WHISPER_ASR_URL", "GANGOLF_API_URL")}
        env.update(environment)
        code = "import audio_transcriber as at; print(at.WHISPER_ASR_URL); print(at.GANGOLF_API_URL)"
        result = subprocess.run([sys.executable, "-c", code], env=env, capture_output=True, text=True,
                                cwd=os.path.dirname(os.path.abspath(__file__)), timeout=60)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.split()

    def test_endpoint_defaults_when_unset(self):
        self.assertEqual(self._endpoints(), ["http://gangolf:9000", "https://gangolf.mamgo.io/api"])

    def test_endpoint_blank_value_uses_default(self):
        for blank in ("", "   "):
            with self.subTest(blank=blank):
                self.assertEqual(self._endpoints(WHISPER_ASR_URL=blank, GANGOLF_API_URL=blank),
                                 ["http://gangolf:9000", "https://gangolf.mamgo.io/api"])

    def test_endpoint_slashes_only_value_uses_default(self):
        self.assertEqual(self._endpoints(WHISPER_ASR_URL="//", GANGOLF_API_URL=" // "),
                         ["http://gangolf:9000", "https://gangolf.mamgo.io/api"])

    def test_endpoint_env_override_trims_and_strips_trailing_slash(self):
        self.assertEqual(self._endpoints(WHISPER_ASR_URL="  http://asr.test:9000//  ",
                                         GANGOLF_API_URL="  http://api.test/api//  "),
                         ["http://asr.test:9000", "http://api.test/api"])

    def test_endpoint_override_keeps_a_path(self):
        self.assertEqual(self._endpoints(WHISPER_ASR_URL="http://asr.test/whisper/"),
                         ["http://asr.test/whisper", "https://gangolf.mamgo.io/api"])

    def test_each_variable_redirects_only_its_own_endpoint(self):
        self.assertEqual(self._endpoints(WHISPER_ASR_URL="http://asr.test:1/"),
                         ["http://asr.test:1", "https://gangolf.mamgo.io/api"])
        self.assertEqual(self._endpoints(GANGOLF_API_URL="http://api.test/api/"),
                         ["http://gangolf:9000", "http://api.test/api"])


class TestConfiguredEndpointsAreUsed(unittest.TestCase):

    def test_transcribe_posts_to_configured_asr_base(self):
        with patch.object(at, 'WHISPER_ASR_URL', 'http://asr.test:1234/whisper'), \
                patch.object(at, 'RECORDED_SAMPLE_RATE', 16000.0), \
                patch('audio_transcriber.requests.post') as post:
            post.return_value = MagicMock(status_code=200, text="text")
            at.transcribe_audio(0.5 * np.ones(16000, dtype='float32'))
        self.assertEqual(post.call_args[0][0], "http://asr.test:1234/whisper/asr")

    def test_send_posts_to_configured_api_base(self):
        with patch.object(at, 'GANGOLF_API_URL', 'http://api.test:5/api'), \
                patch('audio_transcriber.requests.post') as post:
            post.return_value = MagicMock(status_code=200, text="ok")
            at.send_to_api("hello", "KEY")
        self.assertEqual(post.call_args[0][0], "http://api.test:5/api/v1/messages")


class TestMainReportsEndpoints(unittest.TestCase):

    def _run_main(self, *arguments, devices=None, params_error=None):
        output = io.StringIO()
        code = None
        self.printed_at_capture = []
        with patch('sys.argv', ['audio_transcriber.py', *arguments]), \
                patch.object(at, 'WHISPER_ASR_URL', 'http://asr.test:1234'), \
                patch.object(at, 'GANGOLF_API_URL', 'http://api.test:5/api'), \
                patch('audio_transcriber.sd.query_devices', return_value=devices or []), \
                patch('audio_transcriber.get_best_device_parameters', side_effect=params_error), \
                patch('audio_transcriber.list_audio_devices'), \
                patch('audio_transcriber.continuous_capture') as capture, \
                patch.dict(os.environ, {k: v for k, v in os.environ.items() if k != 'GANGOLF_API_KEY'}, clear=True), \
                contextlib.redirect_stdout(output):
            capture.side_effect = lambda *_: self.printed_at_capture.append(output.getvalue())
            try:
                at.main()
            except SystemExit as exit_:
                code = exit_.code
        return output.getvalue(), capture, code

    def test_main_prints_effective_endpoints_before_capturing(self):
        output, capture, _ = self._run_main('--apikey', 'KEY')
        self.assertIn('http://asr.test:1234/asr', self.printed_at_capture[0])
        self.assertIn('http://asr.test:1234/asr', output)
        self.assertIn('http://api.test:5/api/v1/messages', output)
        capture.assert_called_once_with('KEY', None)

    def test_failing_device_checks_print_no_endpoints(self):
        microphone = {'max_input_channels': 1, 'name': 'mic'}
        cases = {
            'not an integer': dict(arguments=('--device', 'abc'), devices=[]),
            'out of range': dict(arguments=('--device', '99'), devices=[microphone]),
            'no input channels': dict(arguments=('--device', '0'),
                                      devices=[{'max_input_channels': 0, 'name': 'speakers'}]),
            'not openable': dict(arguments=('--device', '0'), devices=[microphone],
                                 params_error=RuntimeError("cannot open")),
        }
        for name, case in cases.items():
            with self.subTest(name):
                output, capture, code = self._run_main(
                    '--apikey', 'KEY', *case['arguments'],
                    devices=case['devices'], params_error=case.get('params_error'))
                self.assertEqual(code, 1)
                self.assertNotIn('asr.test', output)
                self.assertNotIn('api.test', output)
                capture.assert_not_called()

    def test_list_devices_prints_no_endpoints(self):
        output, capture, _ = self._run_main('--list-devices')
        self.assertNotIn('asr.test', output)
        self.assertNotIn('api.test', output)
        capture.assert_not_called()

    def test_missing_key_prints_no_endpoints(self):
        output, _, code = self._run_main()
        self.assertEqual(code, 1)
        self.assertNotIn('asr.test', output)

    def test_help_names_both_variables_and_their_defaults(self):
        output, _, code = self._run_main('--help')
        self.assertEqual(code, 0)
        flattened = ' '.join(output.split())
        for expected in ('WHISPER_ASR_URL', 'GANGOLF_API_URL', at.DEFAULT_ASR_URL, at.DEFAULT_API_URL):
            self.assertIn(expected, flattened)


class TestApiKeyFromEnvironment(unittest.TestCase):

    def _run_main(self, *arguments, environment=None):
        env = {k: v for k, v in os.environ.items() if k != 'GANGOLF_API_KEY'}
        env.update(environment or {})
        output = io.StringIO()
        code = None
        with patch('sys.argv', ['audio_transcriber.py', *arguments]), \
                patch('audio_transcriber.continuous_capture') as capture, \
                patch.dict(os.environ, env, clear=True), \
                contextlib.redirect_stdout(output):
            try:
                at.main()
            except SystemExit as exit_:
                code = exit_.code
        return output.getvalue(), capture, code

    def test_environment_key_is_used_when_no_flag_is_given(self):
        _, capture, code = self._run_main(environment={'GANGOLF_API_KEY': 'ENV-KEY'})
        self.assertIsNone(code)
        capture.assert_called_once_with('ENV-KEY', None)

    def test_flag_wins_over_environment(self):
        _, capture, _ = self._run_main('--apikey', 'FLAG-KEY', environment={'GANGOLF_API_KEY': 'ENV-KEY'})
        capture.assert_called_once_with('FLAG-KEY', None)

    def test_flag_alone_still_works(self):
        _, capture, _ = self._run_main('--apikey', 'FLAG-KEY')
        capture.assert_called_once_with('FLAG-KEY', None)

    def test_environment_key_is_trimmed(self):
        _, capture, _ = self._run_main(environment={'GANGOLF_API_KEY': '  ENV-KEY \t'})
        capture.assert_called_once_with('ENV-KEY', None)

    def test_blank_environment_key_counts_as_unset(self):
        for blank in ('', '   ', '\t'):
            with self.subTest(blank=repr(blank)):
                _, capture, code = self._run_main(environment={'GANGOLF_API_KEY': blank})
                self.assertEqual(code, 1)
                capture.assert_not_called()

    def test_blank_environment_key_leaves_the_flag_in_charge(self):
        _, capture, _ = self._run_main('--apikey', 'FLAG-KEY', environment={'GANGOLF_API_KEY': '   '})
        capture.assert_called_once_with('FLAG-KEY', None)

    def test_blank_flag_counts_as_unset_and_the_environment_key_is_used(self):
        for blank in ('', '   '):
            with self.subTest(blank=repr(blank)):
                _, capture, _ = self._run_main('--apikey', blank, environment={'GANGOLF_API_KEY': 'ENV-KEY'})
                capture.assert_called_once_with('ENV-KEY', None)

    def test_blank_flag_without_environment_key_exits_one(self):
        for blank in ('', '   '):
            with self.subTest(blank=repr(blank)):
                _, capture, code = self._run_main('--apikey', blank)
                self.assertEqual(code, 1)
                capture.assert_not_called()

    def test_flag_key_is_trimmed(self):
        _, capture, _ = self._run_main('--apikey', '  FLAG-KEY ')
        capture.assert_called_once_with('FLAG-KEY', None)

    def test_missing_key_exits_one_naming_both_ways_to_supply_it(self):
        output, capture, code = self._run_main()
        self.assertEqual(code, 1)
        self.assertIn('--apikey', output)
        self.assertIn('GANGOLF_API_KEY', output)
        capture.assert_not_called()

    def test_key_is_never_printed(self):
        cases = {
            'environment': dict(arguments=(), environment={'GANGOLF_API_KEY': 'SECRET-ENV'}),
            'flag': dict(arguments=('--apikey', 'SECRET-FLAG'), environment={}),
            'both': dict(arguments=('--apikey', 'SECRET-FLAG'), environment={'GANGOLF_API_KEY': 'SECRET-ENV'}),
        }
        for name, case in cases.items():
            with self.subTest(name):
                output, capture, code = self._run_main(*case['arguments'], environment=case['environment'])
                self.assertIsNone(code)
                capture.assert_called_once()
                self.assertIn('ASR: ', output)
                self.assertNotIn('SECRET', output)

    def test_list_devices_needs_no_key(self):
        with patch('audio_transcriber.list_audio_devices') as listing:
            _, capture, code = self._run_main('--list-devices')
        self.assertIsNone(code)
        listing.assert_called_once_with()
        capture.assert_not_called()

    def test_help_names_the_key_variable(self):
        output, _, code = self._run_main('--help')
        self.assertEqual(code, 0)
        self.assertIn('GANGOLF_API_KEY', ' '.join(output.split()))


class TestAsrTimeout(unittest.TestCase):

    def setUp(self):
        patcher = patch.object(at, 'RECORDED_SAMPLE_RATE', 16000.0)
        patcher.start()
        self.addCleanup(patcher.stop)

    def _transcribe(self):
        with patch('audio_transcriber.requests.post') as post:
            post.return_value = MagicMock(status_code=200, text="text")
            at.transcribe_audio(0.5 * np.ones(16000, dtype='float32'))
        return post

    def test_asr_request_carries_finite_connect_and_read_timeouts(self):
        post = self._transcribe()
        self.assertEqual(post.call_args[1].get('timeout'), at.ASR_TIMEOUT)
        self.assertTrue(_is_finite_timeout_pair(at.ASR_TIMEOUT))

    def test_timeout_returns_none(self):
        with patch('audio_transcriber.requests.post', side_effect=requests.exceptions.ReadTimeout("slow")):
            self.assertIsNone(at.transcribe_audio(0.5 * np.ones(16000, dtype='float32')))


class TestAsrTimeoutLeavesModuleStateAlone(unittest.TestCase):

    def test_recorded_sample_rate_is_restored_after_the_tests_run(self):
        sentinel = 12345.0
        with patch.object(at, 'RECORDED_SAMPLE_RATE', sentinel):
            result = unittest.TestResult()
            unittest.TestLoader().loadTestsFromTestCase(TestAsrTimeout).run(result)
            self.assertEqual(result.testsRun, 2)
            self.assertTrue(result.wasSuccessful())
            self.assertEqual(at.RECORDED_SAMPLE_RATE, sentinel)


class TestDeviceErrorsExitNonZero(unittest.TestCase):

    def _run_main(self, device, devices=None, params_error=None):
        argv = ['audio_transcriber.py', '--apikey', 'KEY', '--device', device]
        with patch('sys.argv', argv), \
                patch('audio_transcriber.sd.query_devices', return_value=devices or []), \
                patch('audio_transcriber.get_best_device_parameters',
                      side_effect=params_error) as params, \
                patch('audio_transcriber.continuous_capture') as capture:
            with self.assertRaises(SystemExit) as cm:
                at.main()
        capture.assert_not_called()
        return cm.exception.code, params

    def test_non_integer_device(self):
        code, _ = self._run_main('abc')
        self.assertEqual(code, 1)

    def test_out_of_range_device(self):
        code, _ = self._run_main('3', devices=[{'max_input_channels': 1, 'name': 'mic'}])
        self.assertEqual(code, 1)

    def test_device_id_equal_to_device_count_is_out_of_range(self):
        code, params = self._run_main('1', devices=[{'max_input_channels': 1, 'name': 'mic'}])
        self.assertEqual(code, 1)
        params.assert_not_called()

    def test_negative_device_is_out_of_range_and_never_probed(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            code, params = self._run_main('-1', devices=[{'max_input_channels': 1, 'name': 'mic'}])
        self.assertEqual(code, 1)
        self.assertIn('out of range', output.getvalue())
        params.assert_not_called()

    def test_device_without_input_channels(self):
        code, params = self._run_main('0', devices=[{'max_input_channels': 0, 'name': 'speakers'}])
        self.assertEqual(code, 1)
        params.assert_not_called()

    def test_unopenable_device(self):
        code, params = self._run_main('0', devices=[{'max_input_channels': 1, 'name': 'mic'}],
                                      params_error=RuntimeError("cannot open"))
        self.assertEqual(code, 1)
        params.assert_called_once_with(0)


class _OpeningStream:
    def __init__(self, **kwargs):
        os.write(2, b'portaudio-diagnostic')

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


class TestProbeStderrHandling(unittest.TestCase):

    def setUp(self):
        at._PROBE_CACHE.clear()
        self.addCleanup(at._PROBE_CACHE.clear)

    def _probe(self):
        devices = [{'max_input_channels': 1, 'default_samplerate': 16000}]
        with patch('audio_transcriber.sd.query_devices', return_value=devices), \
                patch('audio_transcriber.sd.InputStream', _OpeningStream):
            return at._probe_openable_params(0)

    def test_probe_works_when_stderr_has_no_file_descriptor(self):
        with patch('sys.stderr', io.StringIO()):
            self.assertEqual(self._probe(), {'sample_rate': 16000, 'channels': 1})

    def test_raw_stderr_is_silenced_during_probe_and_restored_after(self):
        with tempfile.TemporaryFile() as captured:
            saved = os.dup(2)
            try:
                os.dup2(captured.fileno(), 2)
                with patch('sys.stderr', sys.__stderr__):
                    self._probe()
                os.write(2, b'after-probe')
            finally:
                os.dup2(saved, 2)
                os.close(saved)
            captured.seek(0)
            self.assertEqual(captured.read(), b'after-probe')

    def test_duplicated_descriptor_is_closed(self):
        duplicated = []
        closed = []
        real_dup, real_close = os.dup, os.close

        def recording_dup(fd):
            new_fd = real_dup(fd)
            duplicated.append(new_fd)
            return new_fd

        def recording_close(fd):
            closed.append(fd)
            real_close(fd)

        with patch('sys.stderr', sys.__stderr__), \
                patch('audio_transcriber.os.dup', recording_dup), \
                patch('audio_transcriber.os.close', recording_close):
            self._probe()
        self.assertEqual(len(duplicated), 1)
        self.assertIn(duplicated[0], closed)


if __name__ == '__main__':
    unittest.main(verbosity=2)
