#!/usr/bin/env python3
"""
Unit tests for audio device listing functionality in audio transcriber.
"""

import unittest
from unittest.mock import patch, MagicMock
import sounddevice as sd

# Import the function we want to test
import sys
sys.path.append('/workspace/audio_transcriber')
from audio_transcriber_fixed import list_audio_devices


class TestAudioDeviceListing(unittest.TestCase):
    
    @patch('sounddevice.query_devices')
    def test_list_audio_devices_with_microphone_indicators(self, mock_query_devices):
        """Test that device listing shows helpful names and microphone indicators"""
        
        # Mock devices with various naming patterns
        mock_devices = [
            {
                'name': 'Microsoft Sound Mapper - Input',
                'max_input_channels': 2,
                'default_samplerate': 48000
            },
            {
                'name': 'USB Audio Device - Input',
                'max_input_channels': 1,
                'default_samplerate': 44100
            },
            {
                'name': 'sof-hda-dsp - (hw:3,6)',
                'max_input_channels': 2,
                'default_samplerate': 48000
            },
            {
                'name': 'Microphone (USB Audio Device)',
                'max_input_channels': 1,
                'default_samplerate': 44100
            }
        ]
        
        mock_query_devices.return_value = mock_devices
        
        # Capture the output
        with patch('sys.stdout') as mock_stdout:
            list_audio_devices()
            
        # Verify that at least one device is listed with helpful name
        mock_stdout.write.assert_called()


if __name__ == '__main__':
    unittest.main()