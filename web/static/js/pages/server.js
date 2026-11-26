// Server detail page: status header and the console, backups, activity,
// settings and access tabs.

import * as api from '../api.js';
import * as store from '../store.js';
import { openStream } from '../sse.js';
import {
  h, replace, statusBadge, chip, loading, errorBox, emptyState, actionButton, confirmDialog, notify,
  errorMessage, fmtBytes, fmtTime, fmtRelative, withBusy, openModal,
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
    if (lines.length) note.hidden = true;
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
    const tmux = data.method === 'tmux';
    const count = data.can_list ? `${data.files.length} file${data.files.length === 1 ? '' : 's'}` : '';
    const head = h('div', { class: 'tab-toolbar' },
      h('div', null,
        running ? h('span', { class: 'chip chip-op' }, h('span', { class: 'spinner spinner-small' }), ` Backup in progress (started ${fmtRelative(op.started_at)} by ${op.user || 'system'})`) : h('span', { class: 'muted' }, count)),
      h('div', { class: 'page-actions' },
        running && data.can_create ? actionButton(tmux ? 'Stop waiting' : 'Cancel backup', 'btn-small btn-danger', async () => { await api.post(`/servers/${api.enc(id)}/backups/cancel`); }) : null,
        data.can_create ? actionButton('Create backup', 'btn-primary', async () => {
          await api.post(`/servers/${api.enc(id)}/backups`);
          notify.info(tmux ? 'Sending the backup command to the server console…' : 'Backup started.');
        }, { disabled: !!op, title: op ? 'Another operation is in progress' : (tmux ? 'Send the configured backup command to the server console' : 'Run the configured backup command') }) : null,
        data.can_list ? actionButton('Refresh', 'btn-small', load) : null));
    const hints = [];
    if (data.can_create && tmux) {
      hints.push(h('p', { class: 'hint' }, 'Backups are started by typing the configured command into the server console. The server decides when the backup is finished; new files appear here automatically as they are written.'));
    }
    if (!data.can_list) {
      replace(box, head, hints, emptyState('Backup files are not shown',
        data.can_create ? 'You can start backups, but you do not have the BACKUP_DOWNLOAD permission to view or download backup files.' : 'You do not have permission to view backup files.'));
      return;
    }
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
    replace(box, head, hints,
      !data.create_supported && data.configured ? h('p', { class: 'hint' }, 'No backup command is configured, so backups cannot be started from here; existing files can still be downloaded.') : null,
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
  const STATUS = { running: 'chip-op', succeeded: 'chip-ok', submitted: 'chip-backend', failed: 'chip-bad', cancelled: '', interrupted: 'chip-warn' };
  const actionName = (aid) => {
    const a = (store.state.servers.get(id)?.actions || []).find((x) => x.id === aid);
    return a ? a.name : aid;
  };
  async function load() {
    try {
      const res = await api.get(`/servers/${api.enc(id)}/operations?limit=50`);
      if (!res.operations.length) { replace(box, emptyState('No activity yet', 'Start, stop, restart and backup operations will be listed here.')); return; }
      replace(box, h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
        h('thead', null, h('tr', null, h('th', null, 'When'), h('th', null, 'Operation'), h('th', null, 'By'), h('th', null, 'Result'), h('th', null, 'Details'))),
        h('tbody', null, res.operations.map((op) => h('tr', null,
          h('td', { class: 'nowrap', title: fmtTime(op.started_at) }, fmtTime(op.started_at)),
          h('td', null, op.type === 'action' ? `action: ${actionName(op.action_id)}` : op.type),
          h('td', null, op.username || 'system'),
          h('td', null, chip(op.status, STATUS[op.status] || '')),
          h('td', { class: 'break' }, op.message || (op.finished_at ? `${((new Date(op.finished_at) - new Date(op.started_at)) / 1000).toFixed(1)}s` : ''))))))));
    } catch (err) {
      replace(box, errorBox(err, load));
    }
  }
  const offs = [store.on('operation:' + id, load), store.on('action:' + id, (ev) => { if (ev.phase === 'finished') load(); })];
  load();
  return { el: box, destroy: () => offs.forEach((off) => off()) };
}

// ---- custom actions ----

const RUN_STATUS = { succeeded: ['completed', 'chip-ok'], submitted: ['sent', 'chip-backend'], failed: ['failed', 'chip-bad'], cancelled: ['cancelled', ''], interrupted: ['interrupted', 'chip-warn'], running: ['running', 'chip-op'] };

function showOutput(a, run) {
  openModal({
    title: `Output of ${a.name}`, wide: true,
    body: h('div', null,
      h('p', { class: 'muted small' }, `${fmtTime(run.started_at)} · ${run.username || 'system'} · ${run.message || run.status}`),
      // textContent: output is untrusted text.
      h('pre', { class: 'code output-box' }, run.output || '(no output)')),
  });
}

/**
 * The custom actions the user may run, as compact cards above the tabs.
 * What an action executes is never shown here; only its name and result.
 */
function actionsPanel(server) {
  const id = server.id;
  const box = h('section', { class: 'action-panel', hidden: true, 'aria-label': 'Custom actions' });
  let list = [];

  async function load() {
    try {
      list = (await api.get(`/servers/${api.enc(id)}/actions`)).actions;
      render();
    } catch (err) {
      if (err.status !== 404) console.warn('could not load actions', err);
    }
  }

  async function run(a, button) {
    if (a.confirm) {
      const ok = await confirmDialog({ title: `Run "${a.name}"?`, message: a.description || 'This action will be executed on the server.', confirmLabel: 'Run action', danger: true });
      if (!ok) return;
    }
    await withBusy(button, async () => {
      try {
        await api.post(`/servers/${api.enc(id)}/actions/${api.enc(a.id)}/run`, a.confirm ? { confirm: true } : undefined);
      } catch (err) {
        notify.error(`${a.name}: ${errorMessage(err)}`);
      }
    });
  }

  function card(a) {
    const btn = h('button', { class: 'btn btn-small btn-primary', type: 'button', disabled: !!a.running || a.disabled,
      title: a.disabled ? 'This action is disabled' : (a.running ? 'Already running' : (a.description || 'Run this action')) }, a.running ? 'Running…' : 'Run');
    btn.addEventListener('click', () => run(a, btn));
    let status = null;
    if (a.running) {
      status = h('div', { class: 'action-status' }, h('span', { class: 'spinner spinner-small' }), ` running since ${fmtRelative(a.running.started_at)} (${a.running.user || 'system'})`);
    } else if (a.last_run) {
      const r = a.last_run;
      const [label, cls] = RUN_STATUS[r.status] || [r.status, ''];
      status = h('div', { class: 'action-status' }, chip(label, cls), ` ${fmtRelative(r.finished_at || r.started_at)} · ${r.username || 'system'}`,
        r.output || (r.status === 'failed' && r.message) ? h('button', { class: 'link-btn', type: 'button', onclick: () => showOutput(a, r) }, 'details') : null);
    }
    return h('div', { class: 'action-card' + (a.disabled ? ' is-disabled' : '') },
      h('div', { class: 'action-head' },
        h('span', { class: 'action-name' }, a.icon ? h('span', { class: 'action-icon', 'aria-hidden': 'true' }, a.icon) : null, a.name,
          a.disabled ? chip('disabled', 'chip-warn') : null, a.method === 'tmux' ? chip('console', '') : null),
        btn),
      a.description ? h('div', { class: 'action-desc' }, a.description) : null,
      status);
  }

  function render() {
    box.hidden = !list.length;
    if (!list.length) { replace(box); return; }
    replace(box, h('div', { class: 'action-grid' }, list.map(card)));
  }

  const offs = [
    store.on('action:' + id, load),
    store.on('server-config:' + id, load),
    store.on('resync', load),
  ];
  // Grants change through permissions.changed -> resync; the list in the
  // server view tells us whether there is anything to load at all.
  if ((server.actions || []).length) load();
  // Relative times ("2m ago") stay current.
  const tick = setInterval(() => { if (list.length) render(); }, 30000);
  return { el: box, destroy: () => { offs.forEach((off) => off()); clearInterval(tick); } };
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
      const actions = res.actions || [];
      if (!users.length) {
        replace(box, emptyState('No regular users', 'Invite users first; administrators already have full access to every server.',
          h('a', { class: 'btn btn-primary', href: '#/admin/invitations' }, 'Manage invitations')));
        return;
      }
      replace(box,
        h('p', { class: 'hint' }, 'Permissions are denied unless granted here. STOP includes START, and CONSOLE_WRITE includes CONSOLE_READ. BACKUP_CREATE and BACKUP_DOWNLOAD are independent. Changes take effect immediately, including for users who are currently connected.'),
        actions.length ? h('p', { class: 'hint' }, 'Custom actions are granted one by one and need no other permission than access to the server: a user with an action grant can run that predefined command without CONSOLE_WRITE.') : null,
        users.map((u) => {
          const status = h('span', { class: 'muted small' });
          const warn = h('span');
          let timer = null;
          let mask = u.mask;
          const granted = new Set(u.actions || []);
          const syncWarn = () => replace(warn, granted.size && !mask ? chip('needs a server permission to use its actions', 'chip-warn') : null);
          const editor = permissionEditor(u.mask, (next) => {
            mask = next;
            syncWarn();
            clearTimeout(timer);
            status.textContent = 'Saving…';
            timer = setTimeout(async () => {
              try {
                await api.put(`/admin/users/${u.user_id}/permissions/${api.enc(id)}`, { mask: next });
                status.textContent = 'Saved';
              } catch (err) {
                status.textContent = '';
                notify.error(errorMessage(err));
                load();
              }
            }, 350);
          });
          const actionBoxes = actions.length ? h('div', { class: 'perm-actions' }, h('div', { class: 'perm-actions-title' }, 'Custom actions'),
            h('div', { class: 'perm-grid' }, actions.map((a) => {
              const input = h('input', { type: 'checkbox', checked: granted.has(a.id) });
              input.addEventListener('change', async () => {
                input.disabled = true;
                try {
                  await api.put(`/admin/users/${u.user_id}/actions/${api.enc(id)}/${api.enc(a.id)}`, { allowed: input.checked });
                  if (input.checked) granted.add(a.id); else granted.delete(a.id);
                  syncWarn();
                  status.textContent = 'Saved';
                } catch (err) {
                  input.checked = !input.checked;
                  notify.error(errorMessage(err));
                } finally {
                  input.disabled = false;
                }
              });
              return h('label', { class: 'perm' }, input, h('span', null, h('strong', null, (a.icon ? a.icon + ' ' : '') + a.name), h('small', null, a.description || (a.method === 'tmux' ? 'Console command' : 'Script'))));
            }))) : null;
          syncWarn();
          return h('div', { class: 'perm-row' },
            h('div', { class: 'perm-user' }, h('strong', null, u.display_name || u.username), h('span', { class: 'muted' }, ' @' + u.username),
              u.disabled ? chip('disabled', 'chip-bad') : null, warn, status),
            editor, actionBoxes);
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
  const actionsHost = h('div');
  const tabsNav = h('div', { class: 'tabs', role: 'tablist' });
  const tabBody = h('div', { class: 'tab-body' });
  const el = h('div', { class: 'page' }, header, actionsHost, tabsNav, tabBody);
  let tab = null;
  let tabName = '';
  let gone = false;
  let panel = null;
  let panelKey = '';

  // The actions panel is rebuilt only when the set of visible actions
  // changes, so status updates do not flicker it.
  function renderActions(s) {
    const key = (s.actions || []).map((a) => `${a.id}:${a.name}:${a.icon}:${a.disabled}:${a.confirm}`).join('|');
    if (key === panelKey && panel) return;
    if (panel) panel.destroy();
    panelKey = key;
    panel = actionsPanel(s);
    replace(actionsHost, panel.el);
  }

  function availableTabs(s) {
    const tabs = [];
    if (store.can(s, store.PERM.CONSOLE_READ)) tabs.push(['console', 'Console', consoleTab]);
    if (store.canAny(s, store.PERM.BACKUP_CREATE | store.PERM.BACKUP_DOWNLOAD)) tabs.push(['backups', 'Backups', backupsTab]);
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
            s.category_name ? chip(s.category_name) : null,
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
      if (panel) { panel.destroy(); panel = null; panelKey = ''; replace(actionsHost); }
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
    renderActions(s);
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
    destroy: () => { gone = true; offs.forEach((off) => off()); destroyTab(); if (panel) panel.destroy(); document.title = 'Server Manager'; },
  };
}
