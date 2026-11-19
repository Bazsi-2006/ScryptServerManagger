// VM Controller frontend. No frameworks; all dynamic text is set with
// textContent so nothing from the server is ever parsed as HTML.
(() => {
  'use strict';

  const base = document.querySelector('meta[name="app-base"]').content || '/';
  const $ = (sel) => document.querySelector(sel);

  const LABELS = {
    running: 'Running',
    stopped: 'Stopped',
    starting: 'Starting…',
    stopping: 'Stopping…',
    paused: 'Paused',
    busy: 'Busy',
    error: 'Error',
    unknown: 'Unknown',
    unavailable: 'Unavailable',
  };

  let csrf = '';
  let signedIn = false;
  let pollSeconds = 10;
  let pollTimer = 0;
  let refreshing = false;
  let failures = 0;
  const cards = new Map(); // vm id -> card
  const inflight = new Set(); // vm ids with a request in progress

  class ApiError extends Error {
    constructor(status, code, message) {
      super(message);
      this.status = status;
      this.code = code;
    }
  }

  async function request(method, path, body) {
    const opts = {
      method,
      credentials: 'same-origin',
      cache: 'no-store',
      headers: { Accept: 'application/json' },
    };
    if (method !== 'GET') opts.headers['X-CSRF-Token'] = csrf;
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    let res;
    try {
      res = await fetch(base + 'api/' + path, opts);
    } catch (_) {
      throw new ApiError(0, 'network', 'Cannot reach the server. Check your connection.');
    }
    let data = null;
    try { data = await res.json(); } catch (_) { /* not JSON */ }
    if (!res.ok) {
      const err = (data && data.error) || {};
      throw new ApiError(res.status, err.code || 'error', err.message || `Request failed (HTTP ${res.status}).`);
    }
    return data || {};
  }

  // ---- views ---------------------------------------------------------

  function showLogin(message) {
    signedIn = false;
    csrf = '';
    clearTimeout(pollTimer);
    cards.forEach((c) => c.root.remove());
    cards.clear();
    $('#loading').hidden = true;
    $('#dashboard-view').hidden = true;
    $('#account').hidden = true;
    $('#login-view').hidden = false;
    $('#password').value = '';
    setLoginError(message || '');
    ($('#username').value ? $('#password') : $('#username')).focus();
  }

  function showDashboard(username) {
    signedIn = true;
    failures = 0;
    $('#loading').hidden = true;
    $('#login-view').hidden = true;
    $('#dashboard-view').hidden = false;
    $('#account').hidden = false;
    $('#who').textContent = username;
    refresh();
  }

  function setLoginError(text) {
    const el = $('#login-error');
    el.textContent = text;
    el.hidden = !text;
  }

  let toastTimer = 0;
  function toast(text, isError) {
    const el = $('#toast');
    el.textContent = text;
    el.classList.toggle('toast-error', !!isError);
    el.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { el.hidden = true; }, isError ? 8000 : 5000);
  }

  function banner(text) {
    const el = $('#banner');
    el.textContent = text || '';
    el.hidden = !text;
  }

  // ---- session -------------------------------------------------------

  async function boot() {
    try {
      const s = await request('GET', 'session');
      if (s.authenticated) {
        csrf = s.csrf_token;
        showDashboard(s.username);
      } else {
        showLogin();
      }
    } catch (e) {
      $('#loading').textContent = e.message + ' Retrying…';
      setTimeout(boot, 5000);
    }
  }

  function sessionExpired() {
    showLogin('Your session has ended. Please sign in again.');
  }

  $('#login-form').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const username = $('#username').value.trim();
    const password = $('#password').value;
    if (!username || !password) {
      setLoginError('Enter your username and password.');
      return;
    }
    const btn = $('#login-submit');
    btn.disabled = true;
    setLoginError('');
    try {
      const s = await request('POST', 'login', { username, password });
      csrf = s.csrf_token;
      $('#password').value = '';
      showDashboard(s.username);
    } catch (e) {
      setLoginError(e.message);
      $('#password').value = '';
      $('#password').focus();
    } finally {
      btn.disabled = false;
    }
  });

  $('#logout').addEventListener('click', async () => {
    try { await request('POST', 'logout'); } catch (_) { /* signed out either way */ }
    showLogin('You have been signed out.');
  });

  // ---- polling -------------------------------------------------------

  function schedule(seconds) {
    clearTimeout(pollTimer);
    if (!signedIn || document.hidden) return;
    pollTimer = setTimeout(refresh, seconds * 1000);
  }

  async function refresh() {
    if (!signedIn || refreshing) return;
    refreshing = true;
    try {
      const data = await request('GET', 'vms');
      failures = 0;
      banner('');
      pollSeconds = Math.min(Math.max(Number(data.poll_seconds) || 10, 2), 60);
      render(data.vms || []);
    } catch (e) {
      if (e.status === 401) {
        refreshing = false;
        sessionExpired();
        return;
      }
      failures += 1;
      pollSeconds = Math.min(5 * 2 ** (failures - 1), 60);
      banner(`${e.message} Retrying in ${pollSeconds} seconds.`);
    } finally {
      refreshing = false;
    }
    schedule(pollSeconds);
  }

  document.addEventListener('visibilitychange', () => {
    if (document.hidden) clearTimeout(pollTimer);
    else if (signedIn) refresh();
  });

  // ---- cards ---------------------------------------------------------

  function createCard() {
    const root = $('#vm-card').content.firstElementChild.cloneNode(true);
    const card = {
      root,
      vm: null,
      name: root.querySelector('.vm-name'),
      desc: root.querySelector('.vm-desc'),
      message: root.querySelector('.vm-message'),
      state: root.querySelector('.state'),
      label: root.querySelector('.state-label'),
      start: root.querySelector('.act-start'),
      stop: root.querySelector('.act-stop'),
      open: root.querySelector('.act-open'),
    };
    card.start.addEventListener('click', () => act(card, 'start'));
    card.stop.addEventListener('click', () => act(card, 'stop'));
    return card;
  }

  function render(vms) {
    const grid = $('#vms');
    const seen = new Set();
    vms.forEach((vm, i) => {
      let card = cards.get(vm.id);
      if (!card) {
        card = createCard();
        cards.set(vm.id, card);
      }
      if (grid.children[i] !== card.root) grid.insertBefore(card.root, grid.children[i] || null);
      update(card, vm);
      seen.add(vm.id);
    });
    cards.forEach((card, id) => {
      if (!seen.has(id)) {
        card.root.remove();
        cards.delete(id);
      }
    });
    $('#empty').hidden = vms.length > 0;
    $('#updated').textContent = 'Updated ' + new Date().toLocaleTimeString();
  }

  function update(card, vm) {
    card.vm = vm;
    const state = Object.prototype.hasOwnProperty.call(LABELS, vm.state) ? vm.state : 'unknown';
    const actions = Array.isArray(vm.actions) ? vm.actions : [];
    const busy = inflight.has(vm.id);

    card.name.textContent = vm.name;
    card.desc.textContent = vm.description || '';
    card.message.textContent = vm.message || '';
    card.state.className = 'state state-' + state;
    card.label.textContent = LABELS[state];

    const mayStart = vm.permission === 'start' || vm.permission === 'start-stop';
    const mayStop = vm.permission === 'start-stop';
    card.start.hidden = !mayStart;
    card.start.disabled = busy || !actions.includes('start');
    card.start.textContent = state === 'starting' ? 'Starting…' : 'Start';
    card.start.setAttribute('aria-label', 'Start ' + vm.name);
    card.stop.hidden = !mayStop;
    card.stop.disabled = busy || !actions.includes('stop');
    card.stop.textContent = state === 'stopping' ? 'Stopping…' : 'Stop';
    card.stop.setAttribute('aria-label', 'Stop ' + vm.name);

    const showOpen = typeof vm.url === 'string' && vm.url !== '' && state === 'running';
    card.open.hidden = !showOpen;
    if (showOpen) {
      card.open.href = vm.url;
      card.open.setAttribute('aria-label', 'Open ' + vm.name);
    } else {
      card.open.removeAttribute('href');
    }
  }

  function confirmStop(vm) {
    const dlg = $('#confirm');
    if (typeof dlg.showModal !== 'function') {
      return Promise.resolve(window.confirm(`Stop ${vm.name}? The guest will be asked to shut down.`));
    }
    $('#confirm-title').textContent = `Stop ${vm.name}?`;
    return new Promise((resolve) => {
      dlg.returnValue = '';
      dlg.addEventListener('close', () => resolve(dlg.returnValue === 'confirm'), { once: true });
      dlg.showModal();
    });
  }

  async function act(card, action) {
    const vm = card.vm;
    if (!vm || inflight.has(vm.id)) return;
    if (action === 'stop' && !(await confirmStop(vm))) return;
    inflight.add(vm.id);
    update(card, card.vm);
    try {
      const data = await request('POST', 'vms/' + encodeURIComponent(vm.id) + '/' + action);
      if (data.vm) update(card, data.vm);
      toast(action === 'start' ? `Start requested for ${vm.name}.` : `Shutdown requested for ${vm.name}.`);
      pollSeconds = 3;
    } catch (e) {
      if (e.status === 401) {
        inflight.delete(vm.id);
        sessionExpired();
        return;
      }
      if (e.code === 'csrf_invalid') {
        // The session changed in another tab; pick up the new token.
        try {
          const s = await request('GET', 'session');
          if (!s.authenticated) {
            inflight.delete(vm.id);
            sessionExpired();
            return;
          }
          csrf = s.csrf_token;
        } catch (_) { /* reported below */ }
        toast('Your session was refreshed. Please try again.', true);
      } else {
        toast(e.message, true);
      }
    } finally {
      inflight.delete(vm.id);
      if (card.vm) update(card, card.vm);
    }
    schedule(1);
  }

  boot();
})();
