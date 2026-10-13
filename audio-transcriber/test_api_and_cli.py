"""Tests for send_to_api, request timeouts, the --device exit codes and the probe's stderr handling."""

import io
import json
import os
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
        self.assertEqual(post.call_args[0][0], "https://gangolf.mamgo.io/api/v1/messages")
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
