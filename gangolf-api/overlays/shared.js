(() => {
  const MAX_FRAME = 8 * 1024 * 1024;
  const encoder = new TextEncoder();
  const text = (value, max = 2048) => {
    let result = '';
    let count = 0;
    for (const point of typeof value === 'string' ? value : '') {
      if (count++ >= max) break;
      result += point;
    }
    return result;
  };
  const create = (kind, hooks) => {
    const params = new URLSearchParams(location.search);
    const key = params.get('apiKey');
    const service = params.get('service');
    const channel = params.get('channel');
    let stopped = false;
    let controller;
    let retry;
    let attempt = 0;
    const status = document.getElementById('status');
    const show = (message) => { status.textContent = message; status.hidden = !message; };
    const valid = key && encoder.encode(key).length <= 4096 && (!channel || service) &&
      encoder.encode(location.search.slice(1)).length <= 8192 && !location.hash &&
      [...params.keys()].every(name => ['apiKey', 'service', 'channel'].includes(name) && params.getAll(name).length === 1 && params.get(name)) &&
      [service, channel].every(value => value === null || Array.from(value).length <= 256);
    const filters = () => {
      const query = new URLSearchParams();
      if (service) query.set('service', service);
      if (channel) query.set('channel', channel);
      return query;
    };
    const request = async (path, query, signal) => {
      const response = await fetch(path + '?' + query, { headers: { Authorization: 'Bearer ' + key }, signal, cache: 'no-store', credentials: 'omit', referrerPolicy: 'no-referrer', redirect: 'error' });
      if (!response.ok) {
        const error = new Error('Request failed');
        error.status = response.status;
        throw error;
      }
      return response;
    };
    const run = async () => {
      const connection = new AbortController();
      controller = connection;
      const signal = connection.signal;
      let bootstrapError;
      try {
        const query = filters();
        query.set('types', kind);
        const response = await request('/api/v3/events', query, signal);
        if (!response.headers.get('content-type')?.toLowerCase().startsWith('text/event-stream') || !response.body) throw new Error('Invalid stream');
        const reader = response.body.getReader();
        const decoder = new TextDecoder('utf-8', { fatal: true });
        let buffer = '';
        let event = '';
        let data = '';
        let frameBytes = 0;
        let ready = false;
        const deliver = () => {
          if (!data) { event = ''; frameBytes = 0; return; }
          const payload = JSON.parse(data);
          if (event === 'stream.ready') {
            if (ready || payload.schemaVersion !== 1 || payload.replay !== false || payload.resyncRequired !== true) throw new Error('Invalid ready');
            ready = true;
            hooks.ready(signal, async () => {
              const q = filters();
              q.set('count', '50'); q.set('sort', 'time'); q.set('descending', 'true');
              return (await request('/api/v3/messages', q, signal)).json();
            }).then(() => { attempt = 0; }).catch(error => { bootstrapError = error; connection.abort(); });
          } else if (event === 'stream.reset') throw new Error('Stream reset');
          else if (event === kind) {
            if (!ready || payload.schemaVersion !== 1 || payload.type !== kind || typeof payload.id !== 'string' || !payload.id || payload.id.length > 128) throw new Error('Invalid event');
            hooks.event(payload);
          }
          data = ''; event = ''; frameBytes = 0;
        };
        while (!signal.aborted) {
          const part = await reader.read();
          if (part.done) throw new Error('Stream ended');
          buffer += decoder.decode(part.value, { stream: true });
          if (encoder.encode(buffer).length > MAX_FRAME) throw new Error('Frame too large');
          let match;
          while ((match = /\r\n|\r|\n/.exec(buffer))) {
            if (match[0] === '\r' && match.index === buffer.length - 1) break;
            const line = buffer.slice(0, match.index);
            buffer = buffer.slice(match.index + match[0].length);
            frameBytes += encoder.encode(line).length + match[0].length;
            if (frameBytes > MAX_FRAME) throw new Error('Frame too large');
            if (!line) { deliver(); continue; }
            if (line.startsWith(':')) continue;
            const colon = line.indexOf(':');
            const name = colon < 0 ? line : line.slice(0, colon);
            let value = colon < 0 ? '' : line.slice(colon + 1);
            if (value.startsWith(' ')) value = value.slice(1);
            if (name === 'event') event = value;
            if (name === 'data') data += (data ? '\n' : '') + value;
          }
        }
      } catch (error) {
        connection.abort();
        hooks.clear();
        if (stopped) return;
        const failure = bootstrapError || error;
        if (failure.status === 401 || failure.status === 403) { stopped = true; show('Key invalid, expired or unavailable for this channel.'); return; }
        show('Reconnecting · recent context will refresh');
        retry = setTimeout(run, Math.min(30000, 1000 * 2 ** Math.min(attempt++, 5)) + Math.random() * 200);
      }
    };
    const stop = () => { stopped = true; clearTimeout(retry); controller?.abort(); hooks.clear(); };
    window.addEventListener('pagehide', stop, { once: true });
    if (valid) run(); else { stopped = true; show('A channel-read key and valid channel options are required.'); }
    return Object.freeze({ show, stop });
  };
  window.GangolfOverlay = Object.freeze({ create, text });
})();
