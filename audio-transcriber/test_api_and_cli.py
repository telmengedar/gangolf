"""Tests for send_to_api, request timeouts and the --device exit codes.

Run with:
    cd audio-transcriber
    python -m unittest test_api_and_cli -v
"""

import json
import os
import sys
import unittest
from unittest.mock import MagicMock, patch

import numpy as np
import requests

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import audio_transcriber as at


class TestSendToApi(unittest.TestCase):

    def _send(self, text="hello", status_code=200, body="ok", **post_kwargs):
        with patch('audio_transcriber.requests.post') as post:
            post.return_value = MagicMock(status_code=status_code, text=body)
            post.configure_mock(**post_kwargs)
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

    def test_request_carries_timeout(self):
        _, post = self._send()
        self.assertEqual(post.call_args[1].get('timeout'), at.API_TIMEOUT)


class TestAsrTimeout(unittest.TestCase):

    def _transcribe(self):
        at.RECORDED_SAMPLE_RATE = 16000.0
        with patch('audio_transcriber.requests.post') as post:
            post.return_value = MagicMock(status_code=200, text="text")
            at.transcribe_audio(0.5 * np.ones(16000, dtype='float32'))
        return post

    def test_request_carries_timeout(self):
        post = self._transcribe()
        self.assertEqual(post.call_args[1].get('timeout'), at.ASR_TIMEOUT)

    def test_read_timeout_outlasts_a_max_length_clip(self):
        connect, read = at.ASR_TIMEOUT
        self.assertGreaterEqual(read, 2 * at.MAX_RECORDING_TIME)

    def test_timeout_returns_none(self):
        at.RECORDED_SAMPLE_RATE = 16000.0
        with patch('audio_transcriber.requests.post', side_effect=requests.exceptions.ReadTimeout("slow")):
            self.assertIsNone(at.transcribe_audio(0.5 * np.ones(16000, dtype='float32')))


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


if __name__ == '__main__':
    unittest.main(verbosity=2)
