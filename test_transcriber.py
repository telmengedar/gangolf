#!/usr/bin/env python3
"""
Unit tests for audio_transcriber.py — verifies the core bugs are fixed.

Run with:
    cd <repo>/audio_transcriber        # this directory
    sh setup_audio_env.sh            # ensures audio env is up (idempotent)
    python3 -m unittest test_transcriber -v

Design note (sounddevice contract): a real sd.InputStream (a) fires the callback
from a worker thread while (b) the main thread blocks in the `with ...` wait
loop. The fake below models exactly that: __enter__ starts a daemon worker that
delivers a known chunk sequence (speech, then silence) via the callback; the
caller then drives record_audio, whose wait loop exits once the worker has
raised stop_flag. Firing the callback synchronously in __enter__ (the old fake)
deadlocks record_audio's main-thread wait loop — that tested the fake, not the
code.
"""

import os
import sys
import time
import threading
import inspect
import unittest
from unittest.mock import patch, MagicMock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))


class _FakeInputStream:
    """Stand-in for sd.InputStream matching the real threading contract.

    Callbacks are fired from a worker thread (not synchronously in __enter__),
    and __enter__ returns an instance so record_audio's `with` block works.
    """

    def __init__(self, *args, **kwargs):
        callback = kwargs.get('callback')
        assert callback is not None, "record_audio must pass a callback"
        self._cb = callback
        self._n_speech = kwargs.get('n_speech', 1)
        self._n_silence = kwargs.get('n_silence', 0)
        self._worker = None

    def __enter__(self):
        import numpy as np

        def worker():
            fr = 1024
            speech = 0.05 * np.ones((fr, 1), dtype=np.float32)  # RMS 0.05 > 0.01
            silence = np.zeros((fr, 1), dtype=np.float32)       # RMS 0
            for _ in range(self._n_speech):
                self._cb(speech, fr, None, None)
            for _ in range(self._n_silence):
                self._cb(silence, fr, None, None)

        self._worker = threading.Thread(target=worker, daemon=True)
        self._worker.start()
        return self

    def __exit__(self, *exc):
        if self._worker is not None:
            self._worker.join(timeout=2)
        return False


def _record(n_speech=1, n_silence=0, sr=16000.0, max_rec=30.0):
    """Call record_audio against the fake stream; return (arr, still_alive, elapsed)."""
    import numpy as np
    import audio_transcriber as at
    at.MAX_RECORDING_TIME = max_rec
    with patch('audio_transcriber.sd.query_devices',
               return_value=[{'max_input_channels': 1, 'default_samplerate': sr}]):
        with patch('audio_transcriber.sd.InputStream',
                   side_effect=lambda *a, **kw: _FakeInputStream(n_speech=n_speech, n_silence=n_silence, **kw)):
            res = {}
            def run():
                res['arr'] = at.record_audio(device_id=0)
            t = threading.Thread(target=run, daemon=True)
            t0 = time.time()
            t.start()
            t.join(timeout=8)
            alive = t.is_alive()
            arr = res.get('arr', None)
    if alive:
        return None, True, time.time() - t0
    if not isinstance(arr, np.ndarray):
        return None, False, time.time() - t0
    return arr, False, time.time() - t0


class TestBug1_TranscribeEndpoint(unittest.TestCase):
    """BUG 1: transcribe_audio must use POST /asr with form field 'audio_file'."""

    def _call(self, status_code=200, body="Hallo Welt"):
        import audio_transcriber as at
        import numpy as np
        a = 0.5 * np.ones(16000, dtype='float32')
        at.RECORDED_SAMPLE_RATE = 16000.0
        with patch('audio_transcriber.requests.post') as p:
            p.return_value = MagicMock(status_code=status_code, text=body)
            return at.transcribe_audio(a), p

    def test_uses_asr_endpoint_and_audio_file_field(self):
        _, p = self._call()
        self.assertTrue(p.called, "requests.post was not called")
        args = p.call_args
        self.assertEqual(args[0][0], "http://gangolf:9000/asr")
        files = args[1].get('files', {})
        self.assertIn('audio_file', files, "expected 'audio_file'; got %s" % list(files.keys()))
        self.assertNotIn('file', files)
        params = args[1].get('params', {})
        self.assertEqual(params.get('task'), 'transcribe')
        self.assertEqual(params.get('output'), 'txt')

    def test_500_returns_none_not_crash(self):
        txt, _ = self._call(status_code=500, body="Internal Server Error")
        self.assertIsNone(txt, "500 should return None, not raise")

    def test_zero_segment_produces_all_zero_int16(self):
        """All-zero (silent) segment must become an all-zero int16 array, not NaN.

        Catches: (1) the 0/0->NaN -> 'required argument is not an integer' crash,
        (2) float sample rate handed to scipy's wav writer (struct '<H' error).
        """
        import audio_transcriber as at
        import numpy as np
        at.RECORDED_SAMPLE_RATE = 16000.0
        written = {}

        def fake_wav_write(fp, fs, data):
            written['fs'] = fs
            written['data'] = np.asarray(data)

        with patch('scipy.io.wavfile.write', side_effect=fake_wav_write):
            with patch('audio_transcriber.requests.post') as p:
                p.return_value = MagicMock(status_code=200, text="")
                at.transcribe_audio(np.zeros(16000, dtype='float32'))
        self.assertEqual(written.get('fs'), 16000, "WAV sample rate must be an int")
        data = written.get('data')
        self.assertIsNotNone(data, "transcribe_audio must write a WAV for silent input too")
        self.assertEqual(data.dtype, np.int16, "WAV payload must be int16")
        self.assertFalse(np.any(np.isnan(data.astype(np.float64))), "NaN leaked into the WAV payload")
        self.assertTrue(np.all(data == 0), "silence must pass through as zeros")

    def test_live_endpoint_smoke(self):
        """If the whisper service is reachable, a 1.5s 440 Hz POST must return a str."""
        import urllib.request
        try:
            urllib.request.urlopen('http://gangolf:9000/openapi.json', timeout=3)
        except Exception:
            self.skipTest("whisper service unreachable")
        import audio_transcriber as at
        import numpy as np
        a = (0.5 * np.sin(2 * np.pi * 440 * np.arange(16000 * 1.5) / 16000)).astype('float32')
        at.RECORDED_SAMPLE_RATE = 16000.0
        t0 = time.time()
        txt = at.transcribe_audio(a)
        self.assertIsInstance(txt, str,
                              "live tone must transcribe to a str, got %r" % (txt,))
        self.assertLess(time.time() - t0, 60, "live transcription unexpectedly slow")


class TestBug2_ListDevicesNoApikey(unittest.TestCase):
    """BUG 2: --list-devices must not require --apikey."""

    @patch('sys.argv', ['audio_transcriber.py', '--list-devices'])
    @patch('audio_transcriber.list_audio_devices')
    def test_list_devices_without_apikey(self, mock_list):
        import audio_transcriber as at
        at.main()
        mock_list.assert_called_once()

    def test_main_has_none_apikey_guard(self):
        import audio_transcriber as at
        self.assertIn('args.apikey is None', inspect.getsource(at.main),
                      "main() must validate a missing --apikey on the transcription path")

    @patch('sys.argv', ['audio_transcriber.py'])
    def test_transcription_without_key_exits_1(self):
        import audio_transcriber as at
        with self.assertRaises(SystemExit) as cm:
            at.main()
        self.assertEqual(cm.exception.code, 1)


class TestBug4_RecordAudioReturns(unittest.TestCase):
    """BUG 4: once recording is underway, record_audio must RETURN (was infinite loop)."""

    def _cutoff_chunks(self, sr):
        # silence_cutoff = int(MIN_SILENCE_DURATION * sr / CHUNK_SIZE); stop needs count > cutoff
        import audio_transcriber as at
        return int(at.MIN_SILENCE_DURATION * sr / at.CHUNK_SIZE)

    def test_returns_after_silence(self):
        """Speech, then silence past the cutoff => record_audio RETURNS with the array."""
        import audio_transcriber as at
        cutoff = self._cutoff_chunks(16000)
        arr, alive, elapsed = _record(n_speech=1, n_silence=cutoff + 2, sr=16000.0)
        self.assertFalse(alive, "record_audio HUNG — BUG 4 not fixed (silence path)")
        self.assertLess(elapsed, 5, "returning took %ss — looks like a timeout, not a stop" % elapsed)
        self.assertIsNotNone(arr, "expected captured audio after speech")
        self.assertEqual(arr.shape[0], at.CHUNK_SIZE, "one 1024-sample speech chunk expected")

    def test_returns_after_max_time(self):
        arr, alive, elapsed = _record(n_speech=3, n_silence=0, max_rec=0.001)
        self.assertFalse(alive, "record_audio HUNG — BUG 4 not fixed (max-time path)")
        self.assertLess(elapsed, 5, "returning took %ss — looks like a timeout, not a stop" % elapsed)
        self.assertIsNotNone(arr, "expected captured audio (max-time path)")
        self.assertGreaterEqual(arr.shape[0], 2048, "at least two 1024-sample chunks expected")
        self.assertEqual(arr.shape[0], 3 * 1024, "exactly the 3 speech chunks expected")

    def test_continues_listening_without_speech(self):
        # Intended design: with no speech yet, record_audio waits in the loop.
        # So the thread must still be alive after a short join — blocked, not crashed.
        import audio_transcriber as at
        at.MAX_RECORDING_TIME = 30
        with patch('audio_transcriber.sd.query_devices',
                   return_value=[{'max_input_channels': 1, 'default_samplerate': 16000.0}]):
            with patch('audio_transcriber.sd.InputStream',
                       side_effect=lambda *a, **kw: _FakeInputStream(n_speech=0, n_silence=0, **kw)):
                t = threading.Thread(target=lambda: at.record_audio(device_id=0), daemon=True)
                t.start()
                t.join(timeout=2)
                self.assertTrue(t.is_alive(),
                                "should still be listening (blocked) with no speech yet")


class TestBug3_SampleRateConsistency(unittest.TestCase):
    """BUG 3: RECORDED_SAMPLE_RATE must equal the stream's actual sample rate."""

    def test_recorded_sr_reflects_device(self):
        _record(n_speech=1, n_silence=0, sr=44100.0, max_rec=0.001)
        import audio_transcriber as at
        self.assertEqual(at.RECORDED_SAMPLE_RATE, 44100.0,
                         "RECORDED_SAMPLE_RATE should be 44100, got %s" % at.RECORDED_SAMPLE_RATE)


if __name__ == '__main__':
    unittest.main(verbosity=2)


class TestDeviceIdIntConversion(unittest.TestCase):
    """REGRESSION: main() must convert '--device <str>' to int before continuous_capture.

    Before the fix, main() validated the int, then passed args.device (a string)
    to continuous_capture -> record_audio -> sd.InputStream(device='0') -> 'No
    input device matching "0"'. This test pins the bug: with --device='0' and
    a reachable input device, main() must reach continuous_capture with an int.
    """

    @patch('audio_transcriber.continuous_capture')
    @patch('audio_transcriber.sd.query_devices')
    @patch('audio_transcriber.sd.InputStream')
    @patch('audio_transcriber.get_best_device_parameters')
    @patch('sys.argv', ['audio_transcriber.py', '--apikey', 'DUMMY', '--device', '0'])
    def test_main_passes_int_device_to_capture(self, mock_params, mock_stream, mock_qd, mock_cc):
        import audio_transcriber as at
        mock_qd.return_value = [
            {'max_input_channels': 1, 'default_samplerate': 16000.0, 'name': 'x'},
        ]
        mock_params.return_value = {'channels': 1, 'sample_rate': 16000}
        cm = MagicMock()
        mock_stream.return_value.__enter__ = lambda s: cm
        mock_stream.return_value.__exit__ = lambda s, *a: False

        try:
            at.main()
        except SystemExit as e:
            self.fail("main() exited (%s) — expected it to start continuous_capture" % e.code)

        self.assertTrue(mock_cc.called, "continuous_capture was not called")
        args = mock_cc.call_args[0]
        device_arg = args[-1]
        self.assertIsInstance(device_arg, int,
            "expected int device id, got %r (this is the original 'No input device matching' bug)" % (device_arg,))
        self.assertEqual(device_arg, 0)
