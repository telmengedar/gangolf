(() => {
  const { create, text } = window.GangolfOverlay;
  const container = document.getElementById('messages');
  let pending = [];
  let bytes = 0;
  let bootstrapping = true;
  const rows = new Map();
  const seen = new Set();
  const messageKey = envelope => {
    if (envelope.data.resourceVersion === 3 && /^[1-9]\d*$/.test(envelope.data.message.id)) return 'v3:' + envelope.data.message.id;
    throw new Error('Invalid message identity');
  };
  const render = () => {
    container.replaceChildren();
    for (const message of rows.values()) {
      const row = document.createElement('article');
      row.className = 'message';
      const source = document.createElement('span');
      source.className = 'source';
      source.textContent = text(message.service, 256) + ' / ' + text(message.channel, 256);
      const author = document.createElement('span');
      author.className = 'author';
      author.textContent = message.author + ': ';
      const content = document.createElement('span');
      content.className = 'content';
      content.textContent = message.content;
      row.append(source, author, content);
      container.append(row);
    }
  };
  const add = (id, message) => {
    if (!message || typeof message.service !== 'string' || typeof message.channel !== 'string' || !Array.isArray(message.chunks)) throw new Error('Invalid message');
    if (rows.has(id)) return;
    let content = '';
    for (const chunk of message.chunks) {
      content += text(chunk.type === 0 || chunk.type === 'Text' ? chunk.data : chunk.code);
      if (Array.from(content).length >= 2048) break;
    }
    rows.set(id, { service: text(message.service, 256), channel: text(message.channel, 256), author: text(message.user?.name, 128), content: text(content) });
    while (rows.size > 50) rows.delete(rows.keys().next().value);
  };
  const apply = envelope => {
    if (envelope.data?.resourceVersion !== 3 || !envelope.data.message) throw new Error('Invalid message event');
    add(messageKey(envelope), envelope.data.message);
  };
  const client = create('message.created', {
    ready: async (signal, history) => {
      const result = await history();
      if (signal.aborted) return;
      if (!Array.isArray(result.result)) throw new Error('Invalid history');
      for (const message of result.result.slice(0, 50).reverse()) {
        if (!/^[1-9]\d*$/.test(message.id)) throw new Error('Invalid history identity');
        add('v3:' + message.id, message);
      }
      for (const envelope of pending) apply(envelope);
      pending = []; bytes = 0; bootstrapping = false;
      render(); client.show('');
    },
    event: envelope => {
      if (seen.has(envelope.id)) return;
      seen.add(envelope.id);
      while (seen.size > 256) seen.delete(seen.values().next().value);
      if (bootstrapping) {
        bytes += JSON.stringify(envelope).length * 2;
        if (pending.length >= 256 || bytes > 2 * 1024 * 1024) throw new Error('Bootstrap overflow');
        pending.push(envelope);
      } else { apply(envelope); render(); }
    },
    clear: () => { pending = []; bytes = 0; bootstrapping = true; rows.clear(); seen.clear(); container.replaceChildren(); }
  });
})();
