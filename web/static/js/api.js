// HTTP client and session handling.
//
// The access token lives only in this module's memory (never localStorage),
// so it is gone when the tab closes and cannot be read by other origins. The
// refresh token is an HttpOnly cookie that JavaScript cannot see at all.

const BASE = '/api/v1';

let accessToken = null;
let expiresAt = 0;
let onAuthLost = () => {};
let onSession = () => {};

export class ApiError extends Error {
  constructor(status, code, message, details) {
    super(message);
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

export function setHandlers(handlers) {
  if (handlers.onAuthLost) onAuthLost = handlers.onAuthLost;
  if (handlers.onSession) onSession = handlers.onSession;
}

export function hasSession() { return !!accessToken; }

// A non-secret hint that a refresh cookie probably exists, so a fresh visitor
// does not trigger a pointless (and noisy) 401 on the login page.
const HINT = 'sm-session';
function setHint(on) {
  try {
    if (on) localStorage.setItem(HINT, '1');
    else localStorage.removeItem(HINT);
  } catch { /* storage unavailable: we simply always try */ }
}
export function maybeHasSession() {
  try { return localStorage.getItem(HINT) === '1'; } catch { return true; }
}

function adopt(payload) {
  setHint(true);
  accessToken = payload.access_token;
  expiresAt = Date.now() + payload.expires_in * 1000;
  onSession(payload);
  return payload;
}

async function parse(res) {
  if (res.status === 204) return null;
  const text = await res.text();
  let body = null;
  if (text) {
    try { body = JSON.parse(text); } catch { body = null; }
  }
  if (!res.ok) {
    const e = body && body.error;
    throw new ApiError(res.status, e ? e.code : 'http_error', e ? e.message : `Request failed (HTTP ${res.status}).`, e ? e.details : undefined);
  }
  return body;
}

async function rawFetch(method, path, body, headers = {}) {
  const init = { method, headers: { Accept: 'application/json', ...headers }, credentials: 'same-origin', cache: 'no-store' };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(BASE + path, init);
  } catch {
    throw new ApiError(0, 'network_error', 'The server could not be reached. Check your connection.');
  }
  return res;
}

let refreshing = null;

async function doRefresh() {
  const res = await rawFetch('POST', '/auth/refresh', undefined, { 'X-Requested-With': 'server-manager' });
  return adopt(await parse(res));
}

/**
 * Exchange the refresh cookie for a new access token. Calls are collapsed
 * within this tab, and serialised across tabs with the Web Locks API so two
 * tabs never race to rotate the same refresh token.
 */
export function refresh() {
  if (!refreshing) {
    const run = () => doRefresh();
    const p = (navigator.locks && navigator.locks.request)
      ? navigator.locks.request('server-manager-refresh', run)
      : run();
    refreshing = p.finally(() => { refreshing = null; });
  }
  return refreshing;
}

/** Returns a currently valid access token, refreshing it if necessary. */
export async function getToken() {
  if (!accessToken || Date.now() > expiresAt - 30000) {
    await refresh();
  }
  return accessToken;
}

export function lostSession() {
  setHint(false);
  accessToken = null;
  expiresAt = 0;
  onAuthLost();
}

/** Authenticated JSON request. */
export async function api(method, path, body) {
  let token;
  try {
    token = await getToken();
  } catch (err) {
    if (err.status === 401 || err.status === 403) lostSession();
    throw err;
  }
  let res = await rawFetch(method, path, body, { Authorization: 'Bearer ' + token });
  if (res.status === 401) {
    // The token may have been invalidated server-side; try once more with a
    // fresh one before giving up on the session.
    try {
      await refresh();
    } catch (err) {
      if (err.status === 401 || err.status === 403) lostSession();
      throw err;
    }
    res = await rawFetch(method, path, body, { Authorization: 'Bearer ' + accessToken });
    if (res.status === 401) {
      lostSession();
    }
  }
  return parse(res);
}

export const get = (path) => api('GET', path);
export const post = (path, body) => api('POST', path, body);
export const put = (path, body) => api('PUT', path, body);
export const patch = (path, body) => api('PATCH', path, body);
export const del = (path) => api('DELETE', path);

/** Unauthenticated JSON request (login, registration, verification). */
export async function publicPost(path, body) {
  return parse(await rawFetch('POST', path, body));
}

export async function login(username, password) {
  return adopt(await publicPost('/auth/login', { username, password }));
}

export async function register(payload) {
  return adopt(await publicPost('/auth/register', payload));
}

export async function logout() {
  const headers = { 'X-Requested-With': 'server-manager' };
  if (accessToken) headers.Authorization = 'Bearer ' + accessToken;
  try { await rawFetch('POST', '/auth/logout', undefined, headers); } catch { /* best effort */ }
  setHint(false);
  accessToken = null;
  expiresAt = 0;
}

export function query(params) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== '' && v !== null && v !== undefined) q.set(k, v);
  }
  const s = q.toString();
  return s ? '?' + s : '';
}

export const enc = encodeURIComponent;
export const API_BASE = BASE;
