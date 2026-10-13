#!/usr/bin/env python3
"""
Audio capture and transcription tool that:
1. Records audio from microphone continuously
2. Detects speech segments (filters out silence)
3. Sends segments to whisper service at http://gangolf:9000
4. Posts transcribed text to https://gangolf.mamgo.io/api/v1/messages
"""

import sounddevice as sd
import numpy as np
import requests
import time
import json
import argparse
import sys
import os

# Audio recording parameters - will be adapted based on device capabilities
CHUNK_SIZE = 1024
SAMPLE_RATE = 16000
CHANNELS = 1
THRESHOLD = 0.01  # Silence threshold for speech detection
MIN_SILENCE_DURATION = 0.5  # Minimum silence duration before sending
MAX_RECORDING_TIME = 30  # Maximum recording time in seconds
ASR_TIMEOUT = (5, 180)
API_TIMEOUT = (5, 30)

# Sample rate of the stream most recently opened by record_audio.
# record_audio sets it right before the stream opens; transcribe_audio uses it
# so the WAV is written at the rate the audio was actually captured at.
RECORDED_SAMPLE_RATE = SAMPLE_RATE

_PROBE_RATES = (16000, 48000, 44100)
_PROBE_CHANNELS = (1, 2)

_PROBE_CACHE = {}


def _probe_cache_key(device_id):
    hints = (None, None)
    if device_id is not None:
        try:
            dev = sd.query_devices()[device_id]
            hints = (dev.get('max_input_channels'), dev.get('default_samplerate'))
        except Exception:
            pass
    return (device_id, hints[0], hints[1])


def _probe_openable_params(device_id):
    """Return the first probed (channels, sample_rate) that opens an input stream on the device, or raise."""
    max_input_channels = 0
    advertised_rate = 0
    try:
        device = sd.query_devices()[device_id]
        max_input_channels = int(device.get('max_input_channels') or 0)
        advertised_rate = int(device.get('default_samplerate') or 0)
    except Exception:
        pass

    rates = []
    if advertised_rate > 0:
        rates.append(advertised_rate)
    for r in _PROBE_RATES:
        if r not in rates:
            rates.append(r)

    real_fd2 = sys.stderr.fileno()
    saved = os.dup(real_fd2)
    try:
        with open(os.devnull, 'wb') as _devnull:
            os.dup2(_devnull.fileno(), real_fd2)
            for rate in rates:
                for channels in _PROBE_CHANNELS:
                    if max_input_channels and channels > max_input_channels:
                        continue
                    try:
                        with sd.InputStream(device=device_id, channels=channels, samplerate=rate):
                            return {'sample_rate': rate, 'channels': channels}
                    except Exception:
                        continue
    finally:
        os.dup2(saved, real_fd2)
        os.close(saved)

    raise RuntimeError(
        f"device {device_id!r} could not be opened with any probed (channels, sample_rate) combination")


def get_best_device_parameters(device_id=None):
    """Return the stream parameters for the device; 1ch/16k for the default device, a probed pair otherwise."""
    cache_key = _probe_cache_key(device_id)
    cached = _PROBE_CACHE.get(cache_key)
    if cached is not None:
        return cached

    if device_id is None:
        probed = {'sample_rate': 16000, 'channels': 1}
    else:
        probed = _probe_openable_params(device_id)

    result = {
        'sample_rate': probed['sample_rate'],
        'channels': probed['channels'],
        'whisper_sample_rate': 16000,
        'whisper_channels': 1
    }
    _PROBE_CACHE[cache_key] = result
    return result


def get_system_device_info(device_name):
    """Try to get additional system information about the device"""
    # For now, just return a generic info string - more complex implementation would query 
    # directly from sysfs or other system sources
    
    # Handle specific cases where we can provide better names
    if "sof-hda-dsp" in device_name:
        return "Intel High Definition Audio"
    elif "HDA Intel" in device_name:
        return "Intel HD Audio"
    elif "USB Audio" in device_name:
        return "USB Audio Device"
    elif "AC100" in device_name:
        return "ARM Audio Codec"
    
    # For unknown devices, try to extract information from name
    if device_name.startswith('hw:'):
        # Hardware identifier format 'hw:X,Y'
        try:
            parts = device_name[3:].split(',')
            if len(parts) == 2:
                card = int(parts[0])
                device = int(parts[1])
                return f"Audio Device (Card {card}, Device {device})"
        except ValueError:
            pass
            
    # Fall back to original name for now
    return device_name


def get_enhanced_device_name(device_name, hardware_id):
    """Get a more user-friendly device name"""
    
    # Direct replacements based on known patterns
    replacements = {
        "sof-hda-dsp": "Intel High Definition Audio",
        "HDA Intel": "Intel HD Audio",
        "USB Audio Device": "USB Audio Device", 
        "AC100": "ARM Audio Codec",
        "default": "Default Device"
    }
    
    # Special handling for various manufacturers
    if "Shure" in device_name:
        return "Shure Microphone"
    elif "Audio" in device_name and "Device" in device_name:
        return device_name.replace("Audio Device", "Audio")
    elif "USB Audio" in device_name:
        return device_name.replace("USB Audio", "USB") 
    elif "Microsoft" in device_name and "Sound Mapper" in device_name:
        return "Default Microphone"
    
    # Check replacements first
    for pattern, replacement in replacements.items():
        if pattern in device_name:
            return replacement
    
    # If no specific match, clean up the name by removing technical prefixes
    if device_name.startswith('hw:'):
        return f"Hardware Device {device_name}"
    
    return device_name


def list_audio_devices():
    """List all available audio input devices with clear, helpful naming"""
    print("Available audio input devices:")
    
    try:
        devices = sd.query_devices()
        input_devices = [(i, device) for i, device in enumerate(devices)
                         if device['max_input_channels'] > 0]
        
        if not input_devices:
            print("  No input devices found")
            return
            
        for i, device in input_devices:
            # Get the enhanced device name
            device_name = device.get('name', 'Unknown Device')
            
            # This is a simplified version - in practice, we'd have more sophisticated 
            # system-level information lookup here
            enhanced_name = get_enhanced_device_name(device_name, device.get('name'))
            
            # Try to get additional information from system sources when possible  
            system_info = get_system_device_info(device_name)
            
            input_channels = device['max_input_channels']
            samplerate = device.get('default_samplerate', 'unknown')
            
            # Try to identify likely microphone devices by looking for indicators in the name
            is_microphone = False
            if "mic" in device_name.lower() or "microphone" in device_name.lower() or \
               "recording" in device_name.lower():
                is_microphone = True
            
            print(f"  ID: {i}")
            print(f"  Name: {enhanced_name}")
            print(f"  Hardware identifier: {device_name}")
            
            # Show system information if available
            if system_info and system_info != enhanced_name:
                print(f"  System info: {system_info}")
                
            print(f"  Input channels: {input_channels}")
            print(f"  Sample rate: {samplerate} Hz")
            
            try:
                device_params = get_best_device_parameters(i)
                print(f"  Status: Available for streaming (using "
                      f"{device_params['channels']}ch @ {int(device_params['sample_rate'])} Hz)")
            except Exception as e:
                print(f"  Status: Unavailable - {e}")
                
            if is_microphone:
                print("  Note: This device likely contains a microphone")
            print("  ---")
            
    except Exception as e:
        print(f"Error listing devices: {e}")


def is_speech_detected(audio_data, threshold):
    """Detect if audio contains speech by checking RMS"""
    rms = np.sqrt(np.mean(audio_data**2))
    return rms > threshold


def record_audio(device_id=None):
    """Capture audio from microphone with speech detection"""
    
    # Buffer to store audio data
    audio_buffer = []
    silence_count = 0
    recording_started = False
    
    print("Listening for speech... Speak now (silence will trigger sending)")
    
    # The stream's sample rate, resolved before the stream opens. Used by the
    # callback to compute the silence/max-time cutoffs (BUG 5/7 — the stream
    # may run at a rate different from the global SAMPLE_RATE), and recorded
    # into RECORDED_SAMPLE_RATE for transcribe_audio (BUG 3).
    stream_samplerate = SAMPLE_RATE
    stop_flag = False

    def callback(indata, frames, time, status):
        nonlocal silence_count, recording_started, stop_flag

        audio_data = indata[:, 0].copy()

        if is_speech_detected(audio_data, THRESHOLD):
            silence_count = 0
            if not recording_started:
                print("Speech detected, starting recording...")
                recording_started = True
        else:
            silence_count += 1
            silence_cutoff = int(MIN_SILENCE_DURATION * stream_samplerate / CHUNK_SIZE)
            if silence_count > silence_cutoff and recording_started:
                print("Silence detected. Sending audio...")
                stop_flag = True

        if recording_started:
            audio_buffer.append(audio_data)

        if len(audio_buffer) > MAX_RECORDING_TIME * stream_samplerate / CHUNK_SIZE:
            print(f"Maximum recording time ({MAX_RECORDING_TIME}s) reached")
            stop_flag = True

    try:
        # Get adaptive parameters for this device if specified
        device_params = get_best_device_parameters(device_id)
        # Fallback to default parameters in case of error or None result (though function should never return None now)
        if device_params is None:
            device_params = {
                'sample_rate': SAMPLE_RATE,
                'channels': CHANNELS,
                'whisper_sample_rate': 16000,
                'whisper_channels': 1
            }

        # Resolve the stream sample rate up front so the cutoffs and the WAV
        # write rate agree with what the stream actually produces (BUG 3).
        stream_samplerate = device_params['sample_rate']
        global RECORDED_SAMPLE_RATE
        RECORDED_SAMPLE_RATE = stream_samplerate

        # Start streaming audio capture with adaptive parameters
        with sd.InputStream(callback=callback, channels=device_params['channels'], samplerate=device_params['sample_rate'],
                           blocksize=CHUNK_SIZE, device=device_id):

            # BUG 4: sd.InputStream ignores the callback return value, so
            # `return False` in the callback never stopped the stream and the
            # old `while True: time.sleep(...)` loop hung forever. Instead the
            # callback sets stop_flag and we exit this loop (and thus the
            # `with` block) once it is raised.
            while not stop_flag:
                time.sleep(0.05)

        # Convert buffer to numpy array and return it
        if len(audio_buffer) > 0:
            audio_array = np.concatenate(audio_buffer)
            print(f"Recorded {len(audio_array) / stream_samplerate:.2f} seconds of audio")
            return audio_array
        else:
            print("No audio recorded")
            return None
            
    except sd.PortAudioError as e:
        # Handle PortAudio specific errors (common with device access issues)
        print(f"Audio device error: {e}")
        print("This may be due to the device not being accessible or in use by another application.")
        return None
    except Exception as e:
        print(f"Error recording audio: {e}")
        return None


def transcribe_audio(audio_array):
    """Send audio to whisper service at http://gangolf:9000 for transcription"""
    
    try:
        # Ensure the audio is 16-bit
        if audio_array.dtype != np.int16:
            # Normalize audio data to range [-1, 1] and convert to int16
            m = np.max(np.abs(audio_array))
            if m == 0:
                m = 1  # silence passes through as zero
            normalized = audio_array / m
            audio_int16 = (normalized * 32767).astype(np.int16)
        else:
            audio_int16 = audio_array
            
        # Convert to WAV in memory using BytesIO
        import io
        from scipy.io.wavfile import write as wav_write
        
        wav_buffer = io.BytesIO()
        # BUG 3: write at the rate the audio was captured at, not a hardcoded 16kHz.
        # scipy's wav writer packs the rate into a 32-bit int (struct('<H') would
        # reject a float), so cast — the value is always a whole-Hz rate.
        fs = int(RECORDED_SAMPLE_RATE)
        if fs <= 0:
            fs = int(SAMPLE_RATE)
        wav_write(wav_buffer, fs, audio_int16)

        response = requests.post(
            "http://gangolf:9000/asr",
            params={"task": "transcribe", "language": "en", "output": "txt", "initial_prompt": "vesper, dapper, gangolf"},
            files={"audio_file": ("audio.wav", wav_buffer.getvalue(), "audio/wav")},
            timeout=ASR_TIMEOUT,
        )
        
        if response.status_code == 200:
            transcription = response.text.strip()
            print(f"Transcription: {transcription}")
            return transcription
        else:
            print(f"Error in transcription: {response.status_code} - {response.text}")
            return None
            
    except Exception as e:
        print(f"Error in transcription: {e}")
        return None


def send_to_api(transcribed_text, api_key):
    """Send transcribed text to mamgo.io API"""
    
    if not transcribed_text:
        print("No text to send")
        return False
    
    try:
        payload = {
            "user": {
                "name": "Rugenforth",
                "service": "localhost",
                "serviceId": "host"
            },
            "service": "localhost",
            "channel": "host",
            "chunks": [{
                "type": 0,
                "data": transcribed_text
            }]
        }
        
        response = requests.post(
            "https://gangolf.mamgo.io/api/v1/messages",
            data=json.dumps([payload]),
            headers={
                "Authorization": f"Bearer {api_key}",
                "Content-Type": "application/json"
            },
            timeout=API_TIMEOUT,
        )
        
        if response.status_code == 200 or response.status_code == 201:
            print("Message sent successfully")
            return True
        else:
            print(f"Error sending message: {response.status_code} - {response.text}")
            return False
            
    except Exception as e:
        print(f"Error sending to API: {e}")
        return False


def continuous_capture(api_key, device_id=None):
    """Continuously capture audio and transcribe"""
    print("Starting continuous audio transcription tool...")
    print("Press Ctrl+C to stop")
    
    try:
        while True:
            # Record audio
            audio_data = record_audio(device_id)
            
            if audio_data is None:
                print("No audio captured")
                time.sleep(1)  # Brief pause before next attempt
                continue
            
            # Transcribe
            transcribed_text = transcribe_audio(audio_data)
            
            if transcribed_text:
                # Send to API
                success = send_to_api(transcribed_text, api_key)
                if success:
                    print("Process completed successfully")
                else:
                    print("Failed to send message")
            else:
                print("No transcription received")
                
            # Brief pause before next recording
            time.sleep(0.5)
            
    except KeyboardInterrupt:
        print("\nStopping continuous capture...")


def main():
    """Main function to run the audio capture and transcription process"""
    
    parser = argparse.ArgumentParser(description="Audio transcriber tool")
    parser.add_argument("--apikey", help="API key for authentication (required for transcription, not for --list-devices)")
    parser.add_argument("--device", help="Microphone device ID (optional)")
    parser.add_argument("--list-devices", action="store_true", help="List available audio devices")
    
    args = parser.parse_args()
    
    if args.list_devices:
        list_audio_devices()
        return
    
    if args.apikey is None:
        print("Error: --apikey is required for transcription. Use --list-devices to inspect devices without a key.")
        sys.exit(1)

    device_id = None
    if args.device is not None:
        try:
            device_id = int(args.device)
        except ValueError:
            print("Error: Device ID must be an integer")
            sys.exit(1)

        devices = sd.query_devices()
        if device_id >= len(devices):
            print(f"Error: Device ID {device_id} is out of range. Use --list-devices to see available devices.")
            sys.exit(1)

        if devices[device_id]['max_input_channels'] <= 0:
            print(f"Error: Device ID {device_id} ({devices[device_id].get('name', 'unknown')}) "
                  f"has no input channels — it is not a microphone. Use --list-devices to see available devices.")
            sys.exit(1)

        try:
            device_params = get_best_device_parameters(device_id)
            print(f"Device ID {device_id} is valid and available for streaming "
                  f"({device_params['channels']}ch @ {int(device_params['sample_rate'])} Hz)")
        except Exception as e:
            print(f"Error using device ID {device_id}: Device not accessible - {e}")
            sys.exit(1)

    continuous_capture(args.apikey, device_id)


if __name__ == "__main__":
    main()
