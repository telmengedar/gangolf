#!/usr/bin/env python3
"""
Regression guard: device parameters must come from what a device actually opens.

On real ALSA hardware PortAudio's max_input_channels / default_samplerate are
*hints*; the kernel driver decides which (channels, rate) combinations open and
rejects the rest (PaErrorCode -9998 channels / -9997 sample rate). A backend
fake that accepts any combination (what the other tests use) can never catch
code that trusts the hints. The fake here REJECTS everything except a defined
openable set, like the real driver.

Run with:
    cd audio-transcriber
    python -m unittest test_real_hardware_probe -v
"""

import contextlib
import io
import os
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import audio_transcriber as at


class _DriverRejected(Exception):
    """Stands in for sd.PortAudioError (-9997 / -9998)."""


class _StrictBackend:
    """Fake sounddevice backend: a stream opens only for a (device, channels,
    samplerate) in `openable`. Every open attempt is recorded in `attempts`."""

    def __init__(self, devices, openable):
        self.devices = devices
        self.openable = openable
        self.attempts = []

    def query_devices(self, *a, **kw):
        return self.devices

    def input_stream(self, *args, **kwargs):
        key = (kwargs.get('device'), kwargs.get('channels'), kwargs.get('samplerate'))
        self.attempts.append(key)
        device, channels, rate = key
        if (channels, rate) not in self.openable.get(device, set()):
            raise _DriverRejected("Invalid number of channels / sample rate %r" % (key,))
        return contextlib.nullcontext()


def _dev(name, max_in, rate):
    return {'name': name, 'max_input_channels': max_in, 'default_samplerate': rate}


class _ProbeTestCase(unittest.TestCase):
    def setUp(self):
        at._PROBE_CACHE.clear()
        self.addCleanup(at._PROBE_CACHE.clear)

    def use(self, backend):
        for target, repl in (('audio_transcriber.sd.query_devices', backend.query_devices),
                             ('audio_transcriber.sd.InputStream', backend.input_stream)):
            p = patch(target, side_effect=repl)
            p.start()
            self.addCleanup(p.stop)
        return backend


class TestProbeUsesOpenableCombination(_ProbeTestCase):

    def test_mono_device_whose_advertised_rate_does_not_open(self):
        self.use(_StrictBackend([_dev('mono-card', 1, 44100.0)], {0: {(1, 48000)}}))
        p = at.get_best_device_parameters(0)
        self.assertEqual((p['channels'], p['sample_rate']), (1, 48000))

    def test_stereo_only_device(self):
        self.use(_StrictBackend([_dev('stereo-card', 2, 16000.0)], {0: {(2, 48000)}}))
        p = at.get_best_device_parameters(0)
        self.assertEqual((p['channels'], p['sample_rate']), (2, 48000))

    def test_device_opening_at_both_rates_prefers_advertised(self):
        self.use(_StrictBackend([_dev('two-rate', 1, 44100.0)],
                                    {0: {(1, 16000), (1, 44100)}}))
        p = at.get_best_device_parameters(0)
        self.assertEqual((p['channels'], p['sample_rate']), (1, 44100))

    def test_channels_are_bounded_by_device_max(self):
        b = self.use(_StrictBackend([_dev('mono-only', 1, 44100.0)], {0: {(1, 48000)}}))
        at.get_best_device_parameters(0)
        self.assertNotIn(2, [c for (_, c, _) in b.attempts],
                         "must not try stereo on a 1-channel device: %r" % (b.attempts,))

    def test_reported_parameters_were_actually_opened(self):
        b = self.use(_StrictBackend([_dev('card', 2, 44100.0)], {0: {(2, 44100)}}))
        p = at.get_best_device_parameters(0)
        self.assertIn((0, p['channels'], p['sample_rate']), b.attempts)
        self.assertIn((p['channels'], p['sample_rate']), b.openable[0])

    def test_no_openable_combination_raises(self):
        self.use(_StrictBackend([_dev('dead', 2, 44100.0)], {0: set()}))
        with self.assertRaises(RuntimeError):
            at.get_best_device_parameters(0)


class TestListDevicesStatusIsTruthful(_ProbeTestCase):

    def _list(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            at.list_audio_devices()
        return out.getvalue()

    def test_unopenable_device_reported_unavailable_without_crash(self):
        self.use(_StrictBackend([_dev('dead', 2, 44100.0)], {0: set()}))
        text = self._list()
        self.assertIn('Status: Unavailable', text)
        self.assertNotIn('Available for streaming', text)

    def test_available_line_states_the_combination_that_opened(self):
        self.use(_StrictBackend([_dev('card', 1, 44100.0)], {0: {(1, 48000)}}))
        self.assertIn('Available for streaming (using 1ch @ 48000 Hz)', self._list())

    def test_probe_addresses_full_list_index_not_filtered_index(self):
        b = self.use(_StrictBackend([_dev('speakers', 0, 48000.0), _dev('mic', 1, 16000.0)],
                                    {1: {(1, 16000)}}))
        text = self._list()
        self.assertIn('Available for streaming (using 1ch @ 16000 Hz)', text)
        self.assertTrue(b.attempts)
        self.assertEqual({d for (d, _, _) in b.attempts}, {1})


if __name__ == '__main__':
    unittest.main(verbosity=2)
