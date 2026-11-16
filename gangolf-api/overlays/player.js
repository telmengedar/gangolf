(() => {
  if (window.parent === window || location.search || location.hash) return;
  const allowedParent = document.querySelector('meta[name="gangolf-parent-origin"]').content;
  const playerOrigin = document.querySelector('meta[name="gangolf-player-origin"]').content;
  try {
    const url = new URL(allowedParent);
    if (playerOrigin !== location.origin || allowedParent !== url.origin || !['https:', 'http:'].includes(url.protocol) || url.origin === location.origin || url.href !== url.origin + '/') return;
  } catch { return; }
  let nonce;
  let generation;
  let clip;
  let player;
  let ticker;
  let sdkReady = false;
  let initialized = false;
  const start = document.getElementById('start');
  const exact = (value, fields) => value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === fields.length && fields.every(name => Object.hasOwn(value, name));
  const report = state => parent.postMessage({ version: 1, type: 'status', nonce, generation, state }, allowedParent);
  const stop = () => { clearInterval(ticker); player?.destroy(); player = undefined; start.hidden = true; };
  const play = () => {
    if (player || !sdkReady || !clip) return;
    player = new YT.Player('provider', {
      width: '100%', height: '100%', videoId: clip.remoteVideoId,
      playerVars: { autoplay: 1, controls: 0, disablekb: 1, fs: 0, playsinline: 1, origin: location.origin },
      events: {
        onReady: event => {
          event.target.unMute(); event.target.setVolume(100);
          event.target.loadVideoById({ videoId: clip.remoteVideoId, startSeconds: clip.startSeconds ?? 0, ...(clip.endSeconds === null ? {} : { endSeconds: clip.endSeconds }) });
          ticker = setInterval(() => {
            if (clip.endSeconds !== null && player && player.getCurrentTime() >= clip.endSeconds) { player.stopVideo(); report('ended'); }
          }, 250);
        },
        onStateChange: event => {
          if (event.data === YT.PlayerState.PLAYING) { start.hidden = true; report('playing'); }
          if (event.data === YT.PlayerState.ENDED) report('ended');
        },
        onError: () => report('error'),
        onAutoplayBlocked: () => { start.hidden = false; report('blocked'); }
      }
    });
  };
  window.addEventListener('message', event => {
    if (event.source !== parent || event.origin !== allowedParent) return;
    const data = event.data;
    if (exact(data, ['version', 'type', 'nonce', 'generation']) && data.version === 1 && data.type === 'init' && !initialized && typeof data.nonce === 'string' && /^[a-f0-9]{32}$/.test(data.nonce) && Number.isSafeInteger(data.generation) && data.generation > 0) {
      initialized = true; nonce = data.nonce; generation = data.generation;
      window.onYouTubeIframeAPIReady = () => { sdkReady = true; report('ready'); };
      const script = document.createElement('script');
      script.src = 'https://www.youtube.com/iframe_api';
      script.addEventListener('error', () => report('error'), { once: true });
      document.head.append(script);
      return;
    }
    if (!initialized || data?.version !== 1 || data.nonce !== nonce || data.generation !== generation) return;
    if (exact(data, ['version', 'type', 'nonce', 'generation']) && data.type === 'stop') { stop(); return; }
    if (!exact(data, ['version', 'type', 'nonce', 'generation', 'clip']) || data.type !== 'play' || clip) return;
    const input = data.clip;
    const validTime = value => value === null || typeof value === 'number' && Number.isFinite(value) && value >= 0;
    if (!exact(input, ['remoteVideoId', 'startSeconds', 'endSeconds']) || typeof input.remoteVideoId !== 'string' || !/^[A-Za-z0-9_-]{11}$/.test(input.remoteVideoId) || !validTime(input.startSeconds) || !validTime(input.endSeconds) || input.endSeconds !== null && input.endSeconds <= (input.startSeconds || 0)) return;
    clip = input;
    play();
  });
  start.addEventListener('click', () => player?.playVideo());
  window.addEventListener('pagehide', stop, { once: true });
})();
