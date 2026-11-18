// Application state and the bridge from server events to the UI.
//
// Events only say *that* something changed; the store then re-fetches the
// affected data through the API, which applies authorization. Components
// subscribe to topics and update themselves in place - no page reloads.

import * as api from './api.js';
import { openStream } from './sse.js';
import { notify } from './ui.js';

export const PERM = { START: 1, STOP: 2, CONSOLE_READ: 4, CONSOLE_WRITE: 8, BACKUPS: 16, CONFIG: 32 };
export const PERM_LIST = [
  ['START', 1, 'Start the server'], ['STOP', 2, 'Stop and restart (includes Start)'],
  ['CONSOLE_READ', 4, 'View console output'], ['CONSOLE_WRITE', 8, 'Send console commands (includes Read)'],
  ['BACKUPS', 16, 'Create and download backups'], ['CONFIG', 32, 'Edit server settings'],
];

/** Apply the implications: STOP grants START, CONSOLE_WRITE grants CONSOLE_READ. */
export function effective(mask) {
  let m = mask & 63;
  if (m & PERM.STOP) m |= PERM.START;
  if (m & PERM.CONSOLE_WRITE) m |= PERM.CONSOLE_READ;
  return m;
}

export const state = {
  user: null,
  features: {},
  permissions: {},
  sessionId: null,
  servers: new Map(),
  serversLoaded: false,
  metrics: { latest: null, history: [] },
  live: false,
};

const listeners = new Map();

/** on(topic, fn) -> unsubscribe */
export function on(topic, fn) {
  if (!listeners.has(topic)) listeners.set(topic, new Set());
  listeners.get(topic).add(fn);
  return () => listeners.get(topic)?.delete(fn);
}

export function emit(topic, payload) {
  const set = listeners.get(topic);
  if (!set) return;
  for (const fn of [...set]) {
    try { fn(payload); } catch (err) { console.error('listener failed', topic, err); }
  }
}

export function can(server, flag) {
  if (!server) return false;
  return (server.permissions & flag) === flag;
}

export function isAdmin() { return !!(state.user && state.user.is_admin); }

export function setSession(payload) {
  state.user = payload.user;
  state.features = payload.features || {};
  state.permissions = payload.permissions || {};
  if (payload.session_id) state.sessionId = payload.session_id;
  emit('me');
}

export function clearSession() {
  stopEvents();
  state.user = null;
  state.permissions = {};
  state.servers = new Map();
  state.serversLoaded = false;
  state.metrics = { latest: null, history: [] };
}

export async function loadMe() {
  setSession(await api.get('/me'));
}

export async function loadServers() {
  const res = await api.get('/servers');
  state.servers = new Map(res.servers.map((s) => [s.id, s]));
  state.serversLoaded = true;
  emit('servers');
}

async function reloadServer(id) {
  try {
    const res = await api.get('/servers/' + api.enc(id));
    state.servers.set(id, res.server);
  } catch (err) {
    if (err.status === 404) state.servers.delete(id);
    else return;
  }
  emit('servers');
  emit('server:' + id);
}

export function sortedServers() {
  return [...state.servers.values()].sort((a, b) =>
    (a.category || '').localeCompare(b.category || '', undefined, { sensitivity: 'base' }) ||
    a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }));
}

// ---- event stream ----

let stream = null;
let downSince = 0;

function setLive(v) {
  if (state.live !== v) {
    state.live = v;
    emit('live', v);
  }
}

async function resync() {
  try {
    await Promise.all([loadMe(), loadServers()]);
    emit('resync');
  } catch (err) {
    console.warn('resync failed', err);
  }
}

function handle(type, data) {
  switch (type) {
    case 'hello':
      setLive(true);
      // First connection, or a gap that could not be replayed: our copy of
      // the state may be stale, so load it again.
      if (data.resync) resync();
      break;
    case 'bye':
      // The server ended the stream (permissions/session changed, or we
      // lagged). The client reconnects; a fresh state load follows.
      break;
    case 'server.status.changed': {
      const s = state.servers.get(data.server_id);
      if (s) {
        s.status = data;
        emit('servers');
        emit('server:' + data.server_id);
      } else {
        reloadServer(data.server_id);
      }
      break;
    }
    case 'server.created':
    case 'server.updated':
      reloadServer(data.server_id);
      emit('server-config:' + data.server_id);
      break;
    case 'server.deleted':
      state.servers.delete(data.server_id);
      emit('servers');
      emit('server:' + data.server_id);
      break;
    case 'server.operation.started':
      emit('operation:' + data.server_id, { phase: 'started', ...data });
      break;
    case 'server.operation.completed':
    case 'server.operation.failed': {
      emit('operation:' + data.server_id, { phase: 'finished', ...data });
      const s = state.servers.get(data.server_id);
      const name = s ? s.name : data.server_id;
      const mine = state.user && data.operation && data.operation.user === state.user.username;
      const what = { start: 'Start', stop: 'Stop', restart: 'Restart', backup: 'Backup' }[data.operation?.type] || 'Operation';
      if (data.status === 'succeeded') {
        if (mine) notify.ok(`${name}: ${what.toLowerCase()} completed${data.message && data.operation.type !== 'backup' ? ' – ' + data.message : ''}.`);
      } else if (data.status === 'failed') {
        notify.error(`${name}: ${what.toLowerCase()} failed – ${data.message || 'unknown error'}`);
      } else if (mine && data.status === 'cancelled' && data.operation.type === 'backup') {
        notify.info(`${name}: backup cancelled.`);
      }
      break;
    }
    case 'server.backups.changed':
      emit('backups:' + data.server_id);
      break;
    case 'permissions.changed':
    case 'profile.changed':
      // Our own rights or profile changed: reload both, controls follow.
      resync();
      break;
    case 'users.changed':
    case 'invitations.changed':
    case 'settings.changed':
      emit(type);
      break;
    case 'host.metrics':
      state.metrics.latest = data;
      state.metrics.history.push(data);
      if (state.metrics.history.length > 120) state.metrics.history.shift();
      emit('metrics');
      break;
    default:
      break;
  }
}

export function startEvents() {
  if (stream) return;
  stream = openStream('/events', {
    onEvent: handle,
    onOpen: () => { downSince = 0; },
    onDown: () => {
      if (!downSince) downSince = Date.now();
      setLive(false);
    },
  });
}

export function stopEvents() {
  if (stream) stream.close();
  stream = null;
  setLive(false);
}

// When the tab becomes visible again after a long sleep, timers and the
// stream may have been frozen: make sure we are connected and current.
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible' && stream && !state.live) stream.reconnect();
});
