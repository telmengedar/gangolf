#!/usr/bin/env python3
"""
Unit tests for audio_transcriber.py — verifies the core bugs are fixed.

Run with:
    cd <repo>/audio-transcriber        # this directory
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


_real_sleep = time.sleep


class _FakeInputStream:
    """Stand-in for sd.InputStream matching the real threading contract.

    Callbacks are fired from a worker thread (not synchronously in __enter__),
    and __enter__ returns an instance so record_audio's `with` block works.
    The worker delivers the next chunk only after the wait loop has polled
    twice, and delivers nothing once the stream is closed, so the recording
    ends at the chunk that raised the stop flag.
    """

    def __init__(self, *args, **kwargs):
        self._cb = kwargs.get('callback')
        self._n_speech = kwargs.get('n_speech', 1)
        self._n_silence = kwargs.get('n_silence', 0)
        self._worker = None
        self._closed = threading.Event()
        self._polls = 0

    def poll(self):
        self._polls += 1

    def __enter__(self):
        if self._cb is None:
            return self

        self._worker = threading.Thread(target=self._deliver, daemon=True)
        self._worker.start()
        return self

    def _script(self):
        import numpy as np
        fr = 1024
        for _ in range(self._n_speech):
            yield 0.05 * np.ones((fr, 1), dtype=np.float32)
        for _ in range(self._n_silence):
            yield np.zeros((fr, 1), dtype=np.float32)

    def _deliver(self):
        for data in self._script():
            if self._closed.is_set():
                return
            self._cb(data, data.shape[0], None, None)
            target = self._polls + 2
            while self._polls < target and not self._closed.is_set():
                self._closed.wait(0.001)

    def __exit__(self, *exc):
        self._closed.set()
        if self._worker is not None:
            self._worker.join(timeout=2)
        return False


def _record_stream(stream_cls, max_rec=30.0, sr=16000.0):
    """Run record_audio against `stream_cls`; return the captured array."""
    import audio_transcriber as at
    at._PROBE_CACHE.clear()
    made = []
    res = {}

    def make_stream(*args, **kwargs):
        made.append(stream_cls(*args, **kwargs))
        return made[-1]

    def paced_sleep(seconds):
        if made:
            made[-1].poll()
        _real_sleep(0.001)

    devices = [{'max_input_channels': 1, 'default_samplerate': sr}]
    with patch.object(at, 'MAX_RECORDING_TIME', max_rec):
        with patch('audio_transcriber.sd.query_devices', return_value=devices):
            with patch('audio_transcriber.sd.InputStream', side_effect=make_stream):
                with patch('audio_transcriber.time.sleep', paced_sleep):
                    t = threading.Thread(target=lambda: res.update(arr=at.record_audio(device_id=0)),
                                         daemon=True)
                    t.start()
                    t.join(timeout=8)
    if t.is_alive():
        raise AssertionError("record_audio hung")
    return res.get('arr')


def _fake(n_speech=1, n_silence=0):
    return lambda *a, **kw: _FakeInputStream(*a, n_speech=n_speech, n_silence=n_silence, **kw)


def _tail_chunks(sr=16000.0):
    import audio_transcriber as at
    return int(at.MIN_SILENCE_DURATION * sr / at.CHUNK_SIZE) + 1


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
        """A 1.5s tone sent to the live whisper service gets HTTP 200 and its body is returned as the transcription."""
        import io
        import urllib.request
        import requests
        from scipy.io import wavfile
        try:
            urllib.request.urlopen('http://gangolf:9000/openapi.json', timeout=3)
        except Exception:
            self.skipTest("whisper service unreachable")
        import audio_transcriber as at
        import numpy as np
        a = (0.5 * np.sin(2 * np.pi * 440 * np.arange(16000 * 1.5) / 16000)).astype('float32')
        at.RECORDED_SAMPLE_RATE = 16000.0
        exchanges = []
        real_post = requests.post

        def recording_post(*args, **kwargs):
            response = real_post(*args, **kwargs)
            exchanges.append((kwargs, response))
            return response

        t0 = time.time()
        with patch('audio_transcriber.requests.post', side_effect=recording_post):
            txt = at.transcribe_audio(a)
        self.assertLess(time.time() - t0, 60, "live transcription unexpectedly slow")
        self.assertEqual(len(exchanges), 1)
        kwargs, response = exchanges[0]
        self.assertEqual(response.status_code, 200)
        self.assertEqual(txt, response.text.strip())
        sent_rate, sent_samples = wavfile.read(io.BytesIO(kwargs['files']['audio_file'][1]))
        self.assertEqual(sent_rate, 16000)
        self.assertEqual(len(sent_samples), 24000)


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

    def test_returns_after_silence(self):
        """Speech, then silence past the cutoff => record_audio RETURNS with the array."""
        import audio_transcriber as at
        arr = _record_stream(_fake(n_speech=1, n_silence=_tail_chunks() + 1))
        self.assertIsNotNone(arr, "expected captured audio after speech")
        self.assertEqual(arr.shape[0], (1 + _tail_chunks()) * at.CHUNK_SIZE,
                         "the speech chunk and the quiet run up to the cutoff expected")

    def test_returns_after_max_time(self):
        import audio_transcriber as at
        arr = _record_stream(_fake(n_speech=3), max_rec=0.001)
        self.assertIsNotNone(arr, "expected captured audio (max-time path)")
        self.assertEqual(arr.shape[0], at.CHUNK_SIZE, "recording stops at the first chunk over the limit")

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
        _record_stream(_fake(n_speech=1), max_rec=0.001, sr=44100.0)
        import audio_transcriber as at
        self.assertEqual(at.RECORDED_SAMPLE_RATE, 44100.0,
                         "RECORDED_SAMPLE_RATE should be 44100, got %s" % at.RECORDED_SAMPLE_RATE)


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


class _ReusedBufferInputStream(_FakeInputStream):
    """sd.InputStream double that overwrites one shared `indata` buffer in
    place before every callback, as PortAudio does."""

    LEVELS = (0.05, 0.10, 0.15, 0.20)
    QUIET_BEFORE = 0
    SILENT_CHUNKS = 10

    def _script(self):
        import numpy as np
        reused = np.zeros((1024, 1), dtype=np.float32)
        for level in [0.0] * self.QUIET_BEFORE + list(self.LEVELS) + [0.0] * self.SILENT_CHUNKS:
            reused[:] = level
            yield reused


def _chunk_levels(arr, precision=3):
    import audio_transcriber as at
    return [round(float(arr[i]), precision) for i in range(0, arr.shape[0], at.CHUNK_SIZE)]


class TestPortAudioReusedBufferContract(unittest.TestCase):
    """record_audio must copy `indata`, not keep a view of it."""

    def test_recorded_chunks_keep_their_own_data(self):
        import numpy as np
        import audio_transcriber as at
        arr = _record_stream(_ReusedBufferInputStream)
        self.assertIsNotNone(arr, "expected captured audio")
        levels = _ReusedBufferInputStream.LEVELS
        self.assertEqual(arr.shape[0], (len(levels) + _tail_chunks()) * at.CHUNK_SIZE)
        self.assertEqual(_chunk_levels(arr, 2), list(levels) + [0.0] * _tail_chunks(),
                         "chunks alias the reused PortAudio buffer (missing .copy()?)")
        for i, level in enumerate(levels):
            window = arr[i * at.CHUNK_SIZE:(i + 1) * at.CHUNK_SIZE]
            self.assertTrue(np.allclose(window, level), "window %d is not uniformly %s" % (i, level))


class _PauseInputStream(_ReusedBufferInputStream):
    LEVELS = (0.05, 0.002, 0.004, 0.05, 0.003, 0.002, 0.20)


class _PreSpeechInputStream(_ReusedBufferInputStream):
    LEVELS = (0.05, 0.10)
    QUIET_BEFORE = 20


class TestPausesAreKept(unittest.TestCase):
    """Everything from the first speech to the silence cutoff belongs to the recording."""

    def test_pauses_and_trailing_quiet_survive_in_order(self):
        arr = _record_stream(_PauseInputStream)
        self.assertIsNotNone(arr, "expected captured audio")
        expected = list(_PauseInputStream.LEVELS) + [0.0] * _tail_chunks()
        self.assertEqual(_chunk_levels(arr), expected)

    def test_max_recording_time_counts_quiet_chunks(self):
        import audio_transcriber as at
        chunk_seconds = at.CHUNK_SIZE / 16000.0
        arr = _record_stream(_fake(n_speech=1, n_silence=5), max_rec=2.5 * chunk_seconds)
        self.assertEqual(arr.shape[0], 3 * at.CHUNK_SIZE, "recording stops at the first chunk over 2.5 chunks")

    def test_quiet_before_first_speech_is_not_recorded_nor_counted(self):
        import audio_transcriber as at
        chunk_seconds = at.CHUNK_SIZE / 16000.0
        arr = _record_stream(_PreSpeechInputStream, max_rec=15 * chunk_seconds)
        self.assertIsNotNone(arr, "pre-speech time counted toward MAX_RECORDING_TIME")
        expected = list(_PreSpeechInputStream.LEVELS) + [0.0] * _tail_chunks()
        self.assertEqual(_chunk_levels(arr), expected)


if __name__ == '__main__':
    unittest.main(verbosity=2)
