// Server-Sent Events over fetch().
//
// The native EventSource cannot send an Authorization header, which would
// force the token into the URL. Reading the stream with fetch keeps the
// token in a header. This client reconnects with backoff, resumes with
// Last-Event-ID and detects silently dead connections.

import { getToken, refresh, lostSession, API_BASE } from './api.js';

const STALE_MS = 50000; // the server pings every 20s

/**
 * openStream(path, handlers) -> { close() }
 * handlers: onEvent(type, data, id), onOpen(), onDown(), onFatal(status)
 */
export function openStream(path, handlers) {
  let closed = false;
  let controller = null;
  let lastId = '';
  let retryMs = 1000;
  let attempt = 0;
  let timer = null;

  const wait = (ms) => new Promise((resolve) => { timer = setTimeout(resolve, ms); });

  async function run() {
    while (!closed) {
      let fatal = false;
      try {
        const token = await getToken();
        controller = new AbortController();
        const headers = { Accept: 'text/event-stream', Authorization: 'Bearer ' + token };
        if (lastId) headers['Last-Event-ID'] = lastId;
        const res = await fetch(API_BASE + path, { headers, signal: controller.signal, cache: 'no-store', credentials: 'same-origin' });
        if (res.status === 401) {
          // Token rejected (session revoked, account disabled, ...). If the
          // refresh cookie no longer works either, the session is over.
          try {
            await refresh();
          } catch (err) {
            if (err.status === 401 || err.status === 403) {
              fatal = true;
              lostSession();
              return;
            }
          }
          throw new Error('unauthorized');
        }
        if (!res.ok || !res.body) {
          if (res.status === 403 || res.status === 404) {
            fatal = true;
            if (handlers.onFatal) handlers.onFatal(res.status);
            return;
          }
          throw new Error('HTTP ' + res.status);
        }
        attempt = 0;
        if (handlers.onOpen) handlers.onOpen();
        await read(res.body.getReader());
      } catch (err) {
        if (closed || fatal) return;
      }
      if (closed) return;
      if (handlers.onDown) handlers.onDown();
      attempt++;
      const backoff = Math.min(retryMs * Math.pow(2, Math.min(attempt - 1, 4)), 15000);
      await wait(backoff + Math.random() * 400);
    }
  }

  async function read(reader) {
    const decoder = new TextDecoder();
    let buffer = '';
    let ev = { id: null, type: 'message', data: [] };
    let stale = null;
    const arm = () => {
      clearTimeout(stale);
      stale = setTimeout(() => { if (controller) controller.abort(); }, STALE_MS);
    };
    arm();
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) return;
        arm();
        buffer += decoder.decode(value, { stream: true });
        let nl;
        while ((nl = buffer.indexOf('\n')) >= 0) {
          let line = buffer.slice(0, nl);
          buffer = buffer.slice(nl + 1);
          if (line.endsWith('\r')) line = line.slice(0, -1);
          if (line === '') {
            if (ev.data.length) dispatch(ev);
            ev = { id: null, type: 'message', data: [] };
            continue;
          }
          if (line[0] === ':') continue; // comment / heartbeat
          const c = line.indexOf(':');
          const name = c < 0 ? line : line.slice(0, c);
          let val = c < 0 ? '' : line.slice(c + 1);
          if (val[0] === ' ') val = val.slice(1);
          if (name === 'data') ev.data.push(val);
          else if (name === 'event') ev.type = val;
          else if (name === 'id') ev.id = val;
          else if (name === 'retry' && /^\d+$/.test(val)) retryMs = Math.max(500, Number(val));
        }
      }
    } finally {
      clearTimeout(stale);
      reader.cancel().catch(() => { /* already closed or aborted */ });
    }
  }

  function dispatch(ev) {
    if (ev.id) lastId = ev.id;
    let data = null;
    try { data = JSON.parse(ev.data.join('\n')); } catch { return; }
    try {
      handlers.onEvent(ev.type, data, ev.id);
    } catch (err) {
      console.error('event handler failed', err);
    }
  }

  run();

  return {
    close() {
      closed = true;
      clearTimeout(timer);
      if (controller) controller.abort();
    },
    /** Forget the resume position (forces a fresh snapshot on reconnect). */
    resetPosition() { lastId = ''; },
    reconnect() { if (controller) controller.abort(); },
  };
}
