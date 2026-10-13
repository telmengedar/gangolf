#!/bin/sh
# Re-creatable audio environment for the transcriber (Alpine, run as root).
# Idempotent + self-healing. Exits 0 only when an input stream actually opens.
#
# Override via env (defaults are the historical single-user layout):
#   HOME    -- home directory used for .asoundrc + Pulse runtime
#
set -e
HOME_DIR="${HOME:-/root}"

# 1) Python deps (PEP 668). Bounded; warn-only on failure (already-present is fine).
timeout 90 pip install --break-system-packages --no-input --quiet \
    sounddevice==0.5.6 soundfile==0.12.1 numpy==2.5.3 requests==2.34.2 scipy==1.18.1 || true

# 2) Runtime audio stack — mandatory; fail loudly if we can't install it.
if ! command -v pulseaudio >/dev/null 2>&1 || ! command -v paplay >/dev/null 2>&1; then
    apk add --no-cache --no-progress \
        portaudio alsa-lib libpulse pulseaudio pulseaudio-ctl pulseaudio-alsa pulseaudio-utils
fi

# 3) Ensure PulseAudio is running (self-heal: relaunch if socket missing).
mkdir -p "$HOME_DIR/xdg"
if [ ! -e "$HOME_DIR/xdg/pulse/native" ]; then
    pkill -9 pulseaudio 2>/dev/null || true
    sleep 1
    rm -rf "$HOME_DIR/xdg/pulse" 2>/dev/null || true
    XDG_RUNTIME_DIR="$HOME_DIR/xdg" setsid pulseaudio -D --exit-idle-time=-1 \
        --use-pid-file=false >"$HOME_DIR/xdg/pulse.log" 2>&1 </dev/null
    i=0
    while [ $i -lt 8 ]; do
        [ -e "$HOME_DIR/xdg/pulse/native" ] && break
        sleep 1; i=$((i+1))
    done
fi
[ -e "$HOME_DIR/xdg/pulse/native" ] || { echo "PulseAudio failed to start"; tail -20 "$HOME_DIR/xdg/pulse.log" 2>/dev/null; exit 1; }

# 4) ALSA -> Pulse bridge.
cat > "$HOME_DIR/.asoundrc" <<R
pcm.!default { type pulse; server "unix:$HOME_DIR/xdg/pulse/native"; }
ctl.!default { type pulse; server "unix:$HOME_DIR/xdg/pulse/native"; }
R

# 5) Self-check: an input stream must open + capture.
export PULSE_RUNTIME_PATH="$HOME_DIR/xdg/pulse"
export PULSE_SERVER="unix:$HOME_DIR/xdg/pulse/native"
python3 - <<'EOF'
import sounddevice as sd, numpy as np, time
d = sd.query_devices()
i = next(k for k, x in enumerate(d) if x['max_input_channels'] > 0)
buf = []
def cb(ind, fr, t, st): buf.append(ind.copy())
with sd.InputStream(device=i, channels=1, samplerate=16000, blocksize=1024, callback=cb):
    time.sleep(0.3)
print("AUDIO ENV READY: device %d (%s), %d chunks" % (i, d[i]['name'], len(buf)))
EOF
