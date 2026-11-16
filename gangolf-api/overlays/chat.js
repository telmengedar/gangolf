(() => {
  const { create, text } = window.GangolfOverlay;
  const container = document.getElementById('messages');
  const rows = new Map();
  const seenEvents = new Set();
  const seenMessages = new Set();
  let ready = false;
  let cleanup;
  let generation = 0;
  const remember = (ids, id) => {
    ids.add(id);
    if (ids.size > 256) ids.delete(ids.values().next().value);
  };
  const maintain = () => {
    clearTimeout(cleanup);
    cleanup = undefined;
    const now = performance.now();
    let next = Infinity;
    for (const [id, row] of rows) {
      if (row.removeAt <= now) { row.node.remove(); rows.delete(id); }
      else next = Math.min(next, row.removeAt);
    }
    if (Number.isFinite(next)) {
      const current = generation;
      cleanup = setTimeout(() => { if (current === generation) maintain(); }, Math.max(0, next - now));
    }
  };
  const add = message => {
    if (!message || typeof message.id !== 'string' || !/^[1-9]\d*$/.test(message.id) || typeof message.service !== 'string' || typeof message.channel !== 'string' || !Array.isArray(message.chunks)) throw new Error('Invalid message');
    if (seenMessages.has(message.id)) return;
    remember(seenMessages, message.id);
    const received = performance.now();
    const row = document.createElement('article');
    row.className = 'message';
    const source = document.createElement('span');
    source.className = 'source';
    source.textContent = text(message.service, 256) + ' / ' + text(message.channel, 256);
    const author = document.createElement('span');
    author.className = 'author';
    author.textContent = text(message.user?.name, 128) + ': ';
    let content = '';
    for (const chunk of message.chunks) {
      content += text(chunk.type === 0 || chunk.type === 'Text' ? chunk.data : chunk.code);
      if (Array.from(content).length >= 2048) break;
    }
    const body = document.createElement('span');
    body.className = 'content';
    body.textContent = text(content);
    row.append(source, author, body);
    container.append(row);
    rows.set(message.id, { node: row, removeAt: received + 9000 });
    while (rows.size > 50) {
      const oldest = rows.keys().next().value;
      rows.get(oldest).node.remove(); rows.delete(oldest);
    }
    maintain();
  };
  document.addEventListener('visibilitychange', maintain);
  window.addEventListener('pageshow', maintain);
  window.addEventListener('pagehide', () => {
    document.removeEventListener('visibilitychange', maintain);
    window.removeEventListener('pageshow', maintain);
  }, { once: true });
  const client = create('message.created', {
    ready: async () => { ready = true; client.show(''); },
    event: envelope => {
      if (!ready || seenEvents.has(envelope.id) || envelope.data?.resourceVersion === 2) return;
      if (envelope.data?.resourceVersion !== 3) throw new Error('Invalid message event');
      remember(seenEvents, envelope.id);
      add(envelope.data.message);
    },
    clear: () => {
      ready = false; generation++; clearTimeout(cleanup); cleanup = undefined;
      rows.clear(); container.replaceChildren();
    }
  });
})();
