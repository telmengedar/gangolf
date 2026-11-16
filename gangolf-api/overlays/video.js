(() => {
  const { create } = window.GangolfOverlay;
  const host = document.getElementById('player-host');
  const actions = document.getElementById('actions');
  const originValue = document.querySelector('meta[name="gangolf-player-origin"]')?.content;
  const parentOrigin = document.querySelector('meta[name="gangolf-parent-origin"]')?.content;
  const fail = (code, message) => { document.getElementById('status').textContent = message + ' (' + code + ')'; };
  let playerOrigin;
  if (!originValue || !parentOrigin) { fail('video_config_missing', 'Video player configuration is unavailable.'); return; }
  try {
    const url = new URL(originValue);
    const parent = new URL(parentOrigin);
    if (![url, parent].every(value => ['https:', 'http:'].includes(value.protocol) && value.href === value.origin + '/') || originValue !== url.origin || parentOrigin !== parent.origin) throw new Error('Invalid origin metadata');
    playerOrigin = url.origin;
  } catch {
    fail('video_config_invalid', 'Video player configuration is unavailable.');
    return;
  }
  if (parentOrigin !== location.origin) {
    fail('video_origin_mismatch', 'This video overlay is configured for ' + parentOrigin + '. Current origin: ' + (['http:', 'https:'].includes(location.protocol) ? location.origin : 'unavailable') + '.');
    return;
  }
  if (playerOrigin === parentOrigin) { fail('video_origin_same', 'Video playback requires a separate player origin.'); return; }
  let queue = [];
  let active;
  let frame;
  let nonce;
  let generation = 0;
  let startup;
  let watchdog;
  let ready = false;
  const seen = new Set();
  const exact = (value, fields) => value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === fields.length && fields.every(name => Object.hasOwn(value, name));
  const send = (type) => {
    if (!frame || !active) return;
    frame.contentWindow.postMessage({ version: 1, type, nonce, generation, ...(type === 'play' ? { clip: active.clip } : {}) }, playerOrigin);
  };
  const clearPlayer = () => {
    clearTimeout(startup); clearTimeout(watchdog);
    send('stop');
    frame?.remove(); frame = undefined; active = undefined; ready = false;
    actions.hidden = true;
  };
  const next = (message = '') => {
    clearPlayer();
    queue = queue.filter(item => Date.now() - item.time <= 120000);
    if (!queue.length) { client.show(message); return; }
    active = queue.shift();
    generation++;
    nonce = [...crypto.getRandomValues(new Uint8Array(16))].map(byte => byte.toString(16).padStart(2, '0')).join('');
    frame = document.createElement('iframe');
    frame.title = 'Video playback';
    frame.setAttribute('sandbox', 'allow-scripts allow-same-origin');
    frame.setAttribute('allow', 'autoplay');
    frame.referrerPolicy = 'no-referrer';
    frame.src = playerOrigin + '/overlays/player.html';
    const loadingFrame = frame;
    const loadingNonce = nonce;
    const loadingGeneration = generation;
    frame.addEventListener('load', () => {
      if (frame === loadingFrame) loadingFrame.contentWindow.postMessage({ version: 1, type: 'init', nonce: loadingNonce, generation: loadingGeneration }, playerOrigin);
    }, { once: true });
    host.replaceChildren(frame);
    client.show('Loading video…');
    startup = setTimeout(() => next('Video player did not become ready. (video_player_timeout)'), 20000);
    watchdog = setTimeout(() => next('Video skipped · playback time limit.'), 300000);
  };
  window.addEventListener('message', event => {
    if (!frame || event.source !== frame.contentWindow || event.origin !== playerOrigin) return;
    const data = event.data;
    if (!exact(data, ['version', 'type', 'nonce', 'generation', 'state']) || data.version !== 1 || data.type !== 'status' || data.nonce !== nonce || data.generation !== generation || !['ready', 'playing', 'ended', 'error', 'blocked'].includes(data.state)) return;
    if (data.state === 'ready') { if (!ready) { ready = true; send('play'); } return; }
    if (data.state === 'error') { next('Video unavailable · waiting for the next event.'); return; }
    if (!ready) return;
    if (data.state === 'playing') { clearTimeout(startup); actions.hidden = true; client.show(''); }
    if (data.state === 'blocked') { clearTimeout(startup); actions.hidden = false; client.show('Playback blocked · use OBS Interact and Start playback, or skip.'); }
    if (data.state === 'ended') next();
  });
  document.getElementById('skip').addEventListener('click', () => next());
  const client = create('video.triggered', {
    ready: async () => { client.show(''); },
    event: envelope => {
      if (seen.has(envelope.id)) return;
      seen.add(envelope.id);
      while (seen.size > 256) seen.delete(seen.values().next().value);
      const clip = envelope.data?.clip;
      const time = Date.parse(envelope.time);
      const validTime = value => value === null || typeof value === 'number' && Number.isFinite(value) && value >= 0;
      if (!clip || clip.provider !== 'youtube' || typeof clip.remoteVideoId !== 'string' || !/^[A-Za-z0-9_-]{11}$/.test(clip.remoteVideoId) || !validTime(clip.startSeconds) || !validTime(clip.endSeconds) || clip.endSeconds !== null && clip.endSeconds <= (clip.startSeconds || 0) || !Number.isFinite(time) || time > Date.now() + 30000 || Date.now() - time > 120000) return;
      if (queue.length >= 32) { client.show('Video queue full · newest event skipped.'); return; }
      queue.push({ time, clip: { remoteVideoId: clip.remoteVideoId, startSeconds: clip.startSeconds, endSeconds: clip.endSeconds } });
      if (!active) next();
    },
    clear: () => { queue = []; seen.clear(); clearPlayer(); }
  });
})();
