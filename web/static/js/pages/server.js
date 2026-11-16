// Server detail page: status header and the console, backups, activity,
// settings and access tabs.

import * as api from '../api.js';
import * as store from '../store.js';
import { openStream } from '../sse.js';
import {
  h, replace, statusBadge, chip, loading, errorBox, emptyState, actionButton, confirmDialog, notify,
  errorMessage, fmtBytes, fmtTime, fmtRelative, withBusy,
} from '../ui.js';
import { serverActions } from './dashboard.js';
import { serverForm } from './serverform.js';

const BACKEND_LABEL = { custom: 'Custom commands', tmux: 'tmux session', systemd: 'systemd service', docker: 'Docker container' };
const MAX_LINES = 5000;

// ---- console tab ----

function consoleTab(server) {
  const id = server.id;
  const canWrite = () => store.can(store.state.servers.get(id), store.PERM.CONSOLE_WRITE);
  const out = h('div', { class: 'console-output', tabindex: '0', role: 'log', 'aria-label': 'Console output' });
  const state = h('span', { class: 'console-state' }, 'Connecting…');
  const note = h('div', { class: 'console-note', hidden: true });
  const jump = h('button', { class: 'btn btn-small console-jump', hidden: true, type: 'button' }, '↓ New output');
  let stream = null;
  let live = true;
  let follow = true;
  let pending = [];
  let raf = 0;

  function atBottom() { return out.scrollHeight - out.scrollTop - out.clientHeight < 40; }
  out.addEventListener('scroll', () => {
    follow = atBottom();
    if (follow) jump.hidden = true;
  });
  jump.addEventListener('click', () => { follow = true; out.scrollTop = out.scrollHeight; jump.hidden = true; });

  function flush() {
    raf = 0;
    if (!pending.length) return;
    const frag = document.createDocumentFragment();
    for (const text of pending) {
      // textContent: console output is untrusted and must never be parsed
      // as HTML.
      const line = document.createElement('div');
      line.className = 'console-line' + (text.startsWith('[server-manager]') ? ' is-system' : text.startsWith('> ') ? ' is-input' : '');
      line.textContent = text === '' ? ' ' : text;
      frag.append(line);
    }
    pending = [];
    out.append(frag);
    while (out.childElementCount > MAX_LINES) out.firstElementChild.remove();
    if (follow) out.scrollTop = out.scrollHeight;
    else jump.hidden = false;
  }
  function add(lines) {
    for (const l of lines) pending.push(typeof l === 'string' ? l : l.text);
    if (!raf) raf = requestAnimationFrame(flush);
  }
  function reset() {
    pending = [];
    replace(out);
  }
  function setState(text, cls) {
    state.textContent = text;
    state.className = 'console-state ' + (cls || '');
  }

  function connect() {
    disconnect();
    setState('Connecting…');
    stream = openStream(`/servers/${api.enc(id)}/console/stream`, {
      onEvent: (type, data) => {
        if (type === 'reset') reset();
        else if (type === 'lines') add(data.lines);
        else if (type === 'ready') {
          setState('● Live', 'is-live');
          note.hidden = out.childElementCount > 0 || pending.length > 0;
          if (!note.hidden) note.textContent = 'No console output yet. New output will appear here automatically.';
        } else if (type === 'bye' && data.reason === 'unauthorized') {
          setState('Access removed', 'is-off');
        }
      },
      onOpen: () => { note.hidden = true; },
      onDown: () => setState('Reconnecting…', 'is-off'),
      onFatal: (status) => {
        setState('Unavailable', 'is-off');
        note.hidden = false;
        note.textContent = status === 403 ? 'You no longer have permission to view this console.' : 'This console is not available.';
      },
    });
  }
  function disconnect() {
    if (stream) stream.close();
    stream = null;
  }

  async function snapshot() {
    setState('Snapshot', 'is-off');
    try {
      const res = await api.get(`/servers/${api.enc(id)}/console`);
      reset();
      add(res.lines);
      note.hidden = !res.note;
      if (res.note) note.textContent = res.note;
      setState(`Snapshot · ${new Date().toLocaleTimeString()}`, 'is-off');
    } catch (err) {
      notify.error(errorMessage(err));
    }
  }

  const liveToggle = h('input', { type: 'checkbox', checked: true, id: 'console-live' });
  const refreshBtn = actionButton('Refresh snapshot', 'btn-small', snapshot, { hidden: true });
  liveToggle.addEventListener('change', () => {
    live = liveToggle.checked;
    refreshBtn.hidden = live;
    if (live) { reset(); connect(); } else { disconnect(); snapshot(); }
  });

  // --- command input ---
  const history = [];
  let histPos = -1;
  let draft = '';
  const input = h('input', { type: 'text', class: 'console-input', placeholder: 'Type a server command and press Enter', autocomplete: 'off', spellcheck: 'false', autocapitalize: 'none', maxlength: store.state.features.max_command_length || 1024, 'aria-label': 'Console command' });
  const send = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Send');
  const form = h('form', { class: 'console-form' }, h('span', { class: 'console-prompt', 'aria-hidden': 'true' }, '›'), input, send);
  const inputNote = h('div', { class: 'hint' });

  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const cmd = input.value;
    if (!cmd.trim()) return;
    withBusy(send, async () => {
      try {
        await api.post(`/servers/${api.enc(id)}/console/command`, { command: cmd });
        if (history[history.length - 1] !== cmd) history.push(cmd);
        histPos = -1;
        input.value = '';
        follow = true;
      } catch (err) {
        notify.error(errorMessage(err));
      }
    }).then(() => input.focus());
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowUp' && history.length) {
      e.preventDefault();
      if (histPos === -1) { draft = input.value; histPos = history.length; }
      histPos = Math.max(0, histPos - 1);
      input.value = history[histPos];
    } else if (e.key === 'ArrowDown' && histPos !== -1) {
      e.preventDefault();
      histPos++;
      if (histPos >= history.length) { histPos = -1; input.value = draft; } else input.value = history[histPos];
    }
  });

  function renderInput() {
    const s = store.state.servers.get(id);
    if (!s) return;
    const allowed = canWrite();
    const supported = s.capabilities.console_input;
    form.hidden = !(allowed && supported);
    if (!allowed) inputNote.textContent = 'You can view this console but do not have permission to send commands.';
    else if (!supported) inputNote.textContent = 'Console input is not configured for this server, so commands cannot be sent from here.';
    else inputNote.textContent = 'Commands go to the server\'s own console. Use ↑/↓ to recall previous commands.';
  }
  if (canWrite()) {
    api.get(`/servers/${api.enc(id)}/console/history`).then((res) => {
      for (const item of res.history) history.push(item.command);
    }).catch(() => {});
  }

  const el = h('div', { class: 'console' },
    h('div', { class: 'console-bar' }, state,
      h('div', { class: 'console-tools' },
        h('label', { class: 'toggle', for: 'console-live' }, liveToggle, ' Live'),
        refreshBtn,
        h('button', { class: 'btn btn-small', type: 'button', onclick: () => { reset(); note.hidden = true; } }, 'Clear'))),
    h('div', { class: 'console-frame' }, out, jump), note, form, inputNote);

  renderInput();
  connect();
  const off = store.on('server:' + id, renderInput);
  return { el, destroy: () => { off(); disconnect(); if (raf) cancelAnimationFrame(raf); } };
}

// ---- backups tab ----

function backupsTab(server) {
  const id = server.id;
  const box = h('div', null, loading());
  let data = null;

  async function load() {
    try {
      data = await api.get(`/servers/${api.enc(id)}/backups`);
      render();
    } catch (err) {
      replace(box, errorBox(err, load));
    }
  }

  async function download(file) {
    // An authorized call mints a short-lived, single-use link; the browser
    // then downloads natively, so large files never sit in page memory.
    const res = await api.post(`/servers/${api.enc(id)}/backups/${api.enc(file.name)}/download`);
    const a = h('a', { href: res.url, download: file.name, hidden: true });
    document.body.append(a);
    a.click();
    a.remove();
  }

  async function remove(file) {
    const ok = await confirmDialog({ title: 'Delete backup?', message: `"${file.name}" (${fmtBytes(file.size)}) will be permanently deleted from the server.`, confirmLabel: 'Delete backup', danger: true });
    if (!ok) return;
    await api.del(`/servers/${api.enc(id)}/backups/${api.enc(file.name)}`);
    notify.ok('Backup deleted.');
    load();
  }

  function render() {
    const op = store.state.servers.get(id)?.status.operation;
    const running = op && op.type === 'backup';
    const head = h('div', { class: 'tab-toolbar' },
      h('div', null,
        running ? h('span', { class: 'chip chip-op' }, h('span', { class: 'spinner spinner-small' }), ` Backup in progress (started ${fmtRelative(op.started_at)} by ${op.user || 'system'})`) : h('span', { class: 'muted' }, `${data.files.length} file${data.files.length === 1 ? '' : 's'}`)),
      h('div', { class: 'page-actions' },
        running ? actionButton('Cancel backup', 'btn-small btn-danger', async () => { await api.post(`/servers/${api.enc(id)}/backups/cancel`); }) : null,
        data.can_create ? actionButton('Create backup', 'btn-primary', async () => {
          await api.post(`/servers/${api.enc(id)}/backups`);
          notify.info('Backup started.');
        }, { disabled: !!op, title: op ? 'Another operation is in progress' : 'Run the configured backup command' }) : null,
        actionButton('Refresh', 'btn-small', load)));
    let list;
    if (data.note && !data.files.length) {
      list = emptyState(data.configured ? 'Backup directory unavailable' : 'Backups are not configured', data.note);
    } else if (!data.files.length) {
      list = emptyState('No backups yet', data.can_create ? 'Create the first backup with the button above.' : 'Backup files placed in the configured directory will appear here.');
    } else {
      list = h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
        h('thead', null, h('tr', null, h('th', null, 'File'), h('th', { class: 'num' }, 'Size'), h('th', null, 'Created'), h('th', null, ''))),
        h('tbody', null, data.files.map((f) => h('tr', null,
          h('td', { class: 'break' }, f.name),
          h('td', { class: 'num nowrap' }, fmtBytes(f.size)),
          h('td', { class: 'nowrap', title: fmtTime(f.modified_at) }, fmtTime(f.modified_at)),
          h('td', { class: 'actions' },
            actionButton('Download', 'btn-small', () => download(f)),
            data.can_delete ? actionButton('Delete', 'btn-small btn-danger', () => remove(f)) : null))))));
    }
    replace(box, head,
      !data.can_create && data.configured ? h('p', { class: 'hint' }, 'No backup command is configured, so backups cannot be started from here; existing files can still be downloaded.') : null,
      list);
  }

  const offs = [store.on('backups:' + id, load), store.on('server:' + id, () => { if (data) render(); })];
  load();
  return { el: box, destroy: () => offs.forEach((off) => off()) };
}

// ---- activity tab ----

function activityTab(server) {
  const id = server.id;
  const box = h('div', null, loading());
  const STATUS = { running: 'chip-op', succeeded: 'chip-ok', failed: 'chip-bad', cancelled: '', interrupted: 'chip-warn' };
  async function load() {
    try {
      const res = await api.get(`/servers/${api.enc(id)}/operations?limit=50`);
      if (!res.operations.length) { replace(box, emptyState('No activity yet', 'Start, stop, restart and backup operations will be listed here.')); return; }
      replace(box, h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
        h('thead', null, h('tr', null, h('th', null, 'When'), h('th', null, 'Operation'), h('th', null, 'By'), h('th', null, 'Result'), h('th', null, 'Details'))),
        h('tbody', null, res.operations.map((op) => h('tr', null,
          h('td', { class: 'nowrap', title: fmtTime(op.started_at) }, fmtTime(op.started_at)),
          h('td', null, op.type),
          h('td', null, op.username || 'system'),
          h('td', null, chip(op.status, STATUS[op.status] || '')),
          h('td', { class: 'break' }, op.message || (op.finished_at ? `${((new Date(op.finished_at) - new Date(op.started_at)) / 1000).toFixed(1)}s` : ''))))))));
    } catch (err) {
      replace(box, errorBox(err, load));
    }
  }
  const off = store.on('operation:' + id, load);
  load();
  return { el: box, destroy: off };
}

// ---- settings tab ----

function settingsTab(server, ctx) {
  const id = server.id;
  const box = h('div', null, loading());
  async function load() {
    try {
      const res = await api.get(`/servers/${api.enc(id)}/config`);
      const form = serverForm({
        def: res.config, scope: res.edit_scope, isNew: false,
        onSave: async (def) => {
          const saved = await api.put(`/servers/${api.enc(id)}/config`, def);
          store.state.servers.set(id, saved.server);
          store.emit('servers');
          store.emit('server:' + id);
          notify.ok('Settings saved.');
          return saved.config;
        },
      });
      const danger = store.isAdmin() ? h('fieldset', { class: 'section section-danger' },
        h('legend', null, 'Remove server'),
        h('p', { class: 'hint' }, 'Removes this server from the manager, including its permission assignments. The server\'s own files and data are not touched.'),
        actionButton('Delete server definition', 'btn-danger', async () => {
          const current = store.state.servers.get(id);
          const active = current && ['RUNNING', 'STARTING', 'STOPPING'].includes(current.status.state);
          const ok = await confirmDialog({
            title: `Delete "${server.name}"?`,
            message: active
              ? 'This server is currently running. Deleting the definition will leave it running without management. Its files are not deleted.'
              : 'The definition and its permission assignments will be removed. The server\'s files are not deleted.',
            confirmLabel: active ? 'Delete anyway' : 'Delete definition', danger: true,
          });
          if (!ok) return;
          await api.del(`/servers/${api.enc(id)}${active ? '?force=true' : ''}`);
          notify.ok('Server definition deleted.');
          ctx.navigate('/');
        })) : null;
      replace(box, form.el, danger);
    } catch (err) {
      replace(box, errorBox(err, load));
    }
  }
  load();
  return { el: box };
}

// ---- access tab (administrators) ----

export function permissionEditor(mask, onChange, disabled) {
  let raw = mask;
  const boxes = [];
  const wrap = h('div', { class: 'perm-grid' });
  function sync() {
    const eff = store.effective(raw);
    for (const [flag, input] of boxes) {
      const implied = !(raw & flag) && (eff & flag);
      input.checked = !!(eff & flag);
      input.disabled = !!disabled || !!implied;
      input.parentElement.title = implied ? 'Granted automatically by a higher permission' : '';
      input.parentElement.classList.toggle('is-implied', !!implied);
    }
  }
  for (const [name, flag, desc] of store.PERM_LIST) {
    const input = h('input', { type: 'checkbox' });
    input.addEventListener('change', () => {
      raw = input.checked ? (raw | flag) : (raw & ~flag);
      sync();
      onChange(raw);
    });
    boxes.push([flag, input]);
    wrap.append(h('label', { class: 'perm' }, input, h('span', null, h('strong', null, name), h('small', null, desc))));
  }
  sync();
  return wrap;
}

function accessTab(server) {
  const id = server.id;
  const box = h('div', null, loading());
  async function load() {
    try {
      const res = await api.get(`/servers/${api.enc(id)}/permissions`);
      const users = res.permissions.filter((p) => !p.is_admin);
      const admins = res.permissions.filter((p) => p.is_admin);
      if (!users.length) {
        replace(box, emptyState('No regular users', 'Invite users first; administrators already have full access to every server.',
          h('a', { class: 'btn btn-primary', href: '#/admin/invitations' }, 'Manage invitations')));
        return;
      }
      replace(box,
        h('p', { class: 'hint' }, 'Permissions are denied unless granted here. STOP includes START, and CONSOLE_WRITE includes CONSOLE_READ. Changes take effect immediately, including for users who are currently connected.'),
        users.map((u) => {
          const status = h('span', { class: 'muted small' });
          let timer = null;
          const editor = permissionEditor(u.mask, (mask) => {
            clearTimeout(timer);
            status.textContent = 'Saving…';
            timer = setTimeout(async () => {
              try {
                await api.put(`/admin/users/${u.user_id}/permissions/${api.enc(id)}`, { mask });
                status.textContent = 'Saved';
              } catch (err) {
                status.textContent = '';
                notify.error(errorMessage(err));
                load();
              }
            }, 350);
          });
          return h('div', { class: 'perm-row' },
            h('div', { class: 'perm-user' }, h('strong', null, u.display_name || u.username), h('span', { class: 'muted' }, ' @' + u.username),
              u.disabled ? chip('disabled', 'chip-bad') : null, status),
            editor);
        }),
        admins.length ? h('p', { class: 'hint' }, `Administrators with full access: ${admins.map((a) => a.username).join(', ')}.`) : null);
    } catch (err) {
      replace(box, errorBox(err, load));
    }
  }
  const off = store.on('users.changed', load);
  load();
  return { el: box, destroy: off };
}

// ---- page ----

export function serverPage(ctx) {
  const [id, tabArg] = ctx.args;
  const header = h('div', { class: 'server-header' });
  const tabsNav = h('div', { class: 'tabs', role: 'tablist' });
  const tabBody = h('div', { class: 'tab-body' });
  const el = h('div', { class: 'page' }, header, tabsNav, tabBody);
  let tab = null;
  let tabName = '';
  let gone = false;

  function availableTabs(s) {
    const tabs = [];
    if (store.can(s, store.PERM.CONSOLE_READ)) tabs.push(['console', 'Console', consoleTab]);
    if (store.can(s, store.PERM.BACKUPS)) tabs.push(['backups', 'Backups', backupsTab]);
    tabs.push(['activity', 'Activity', activityTab]);
    if (store.can(s, store.PERM.CONFIG)) tabs.push(['settings', 'Settings', settingsTab]);
    if (store.isAdmin()) tabs.push(['access', 'Access', accessTab]);
    return tabs;
  }

  function renderHeader(s) {
    const st = s.status;
    replace(header,
      h('a', { class: 'back', href: '#/' }, '← Servers'),
      h('div', { class: 'server-title' },
        h('div', null,
          h('h1', null, s.name, ' ', statusBadge(st.state)),
          h('div', { class: 'card-meta' },
            chip(BACKEND_LABEL[s.backend] || s.backend, 'chip-backend'),
            s.category ? chip(s.category) : null,
            h('span', { class: 'muted small' }, `ID ${s.id} · ${st.state.toLowerCase()} since ${fmtRelative(st.since)}`),
            st.operation ? chip(`${st.operation.type} in progress`, 'chip-op') : null)),
        h('div', { class: 'server-actions' }, serverActions(s))),
      s.description ? h('p', { class: 'muted' }, s.description) : null,
      st.message ? h('div', { class: 'alert ' + (st.state === 'ERROR' ? 'alert-error' : st.state === 'UNKNOWN' ? 'alert-warn' : 'alert-info') }, st.message) : null);
  }

  function destroyTab() {
    if (tab && tab.destroy) tab.destroy();
    tab = null;
  }

  function render() {
    if (gone) return;
    const s = store.state.servers.get(id);
    if (!s) {
      destroyTab();
      tabName = '';
      if (!store.state.serversLoaded) { replace(header, loading()); replace(tabsNav); replace(tabBody); return; }
      replace(header, h('a', { class: 'back', href: '#/' }, '← Servers'));
      replace(tabsNav);
      replace(tabBody, emptyState('Server not available', 'This server does not exist or you no longer have access to it.',
        h('a', { class: 'btn btn-primary', href: '#/' }, 'Back to servers')));
      return;
    }
    document.title = `${s.name} · Server Manager`;
    renderHeader(s);
    const tabs = availableTabs(s);
    const wanted = tabs.find((t) => t[0] === (tabArg || tabName)) || tabs[0];
    replace(tabsNav, tabs.map(([key, label]) => h('a', {
      class: 'tab' + (key === wanted[0] ? ' active' : ''), role: 'tab', href: `#/servers/${id}/${key}`, 'aria-selected': String(key === wanted[0]),
    }, label)));
    // Rebuild the tab only when it actually changes; state updates must not
    // tear down a live console or a half-edited form.
    if (wanted[0] !== tabName) {
      destroyTab();
      tabName = wanted[0];
      tab = wanted[2](s, ctx);
      replace(tabBody, tab.el);
    }
  }

  const offs = [store.on('server:' + id, render), store.on('me', render), store.on('resync', render)];
  if (!store.state.serversLoaded) offs.push(store.on('servers', () => { if (!store.state.servers.has(id) || !tabName) render(); }));
  render();
  return {
    el,
    destroy: () => { gone = true; offs.forEach((off) => off()); destroyTab(); document.title = 'Server Manager'; },
  };
}
