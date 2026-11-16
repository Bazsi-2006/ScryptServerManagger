// Dashboard: server cards grouped by category, plus a compact host summary.

import * as api from '../api.js';
import * as store from '../store.js';
import { h, replace, statusBadge, chip, emptyState, actionButton, confirmDialog, loading, fmtRelative } from '../ui.js';

const BACKEND_LABEL = { custom: 'Custom', tmux: 'tmux', systemd: 'systemd', docker: 'Docker' };

/**
 * Start/stop/restart buttons for a server. Which buttons exist depends on
 * the caller's permissions and the backend's capabilities; which are
 * enabled depends on the current state. The server enforces all of it
 * again.
 */
export function serverActions(server, { small = false } = {}) {
  const st = server.status.state;
  const op = server.status.operation;
  const lifecycleBusy = !!op && op.type !== 'start';
  const cls = small ? 'btn-small ' : '';
  const out = [];
  const call = (verb) => async () => { await api.post(`/servers/${api.enc(server.id)}/${verb}`); };

  if (store.can(server, store.PERM.START) && server.capabilities.start) {
    const disabled = !!op || st === 'RUNNING' || st === 'STARTING' || st === 'STOPPING';
    out.push(actionButton('Start', cls + 'btn-success', call('start'), { disabled, title: disabled ? 'The server is not stopped' : 'Start the server' }));
  }
  if (store.can(server, store.PERM.STOP)) {
    const stoppable = st !== 'STOPPED' && !lifecycleBusy && st !== 'STOPPING';
    out.push(actionButton('Stop', cls + 'btn-danger', async () => {
      const ok = await confirmDialog({ title: `Stop ${server.name}?`, message: 'The server will be shut down. Connected players or clients will be disconnected.', confirmLabel: 'Stop server', danger: true });
      if (ok) await call('stop')();
    }, { disabled: !stoppable, title: stoppable ? 'Stop the server' : 'The server is not running' }));
    if (server.capabilities.restart) {
      const restartable = st === 'RUNNING' && !op;
      out.push(actionButton('Restart', cls, async () => {
        const ok = await confirmDialog({ title: `Restart ${server.name}?`, message: 'The server will be stopped and started again.', confirmLabel: 'Restart server', danger: true });
        if (ok) await call('restart')();
      }, { disabled: !restartable, title: restartable ? 'Restart the server' : 'Only a running server can be restarted' }));
    }
  }
  return out;
}

function card(server) {
  const st = server.status;
  const msg = st.message && (st.state === 'ERROR' || st.state === 'UNKNOWN' || st.state === 'STOPPED' || st.state === 'STARTING')
    ? h('p', { class: 'card-msg ' + (st.state === 'ERROR' ? 'is-error' : '') }, st.message) : null;
  return h('article', { class: `card server-card state-${st.state.toLowerCase()}` },
    h('div', { class: 'card-head' },
      h('a', { class: 'card-title', href: `#/servers/${server.id}` }, server.name),
      statusBadge(st.state)),
    h('div', { class: 'card-meta' },
      chip(BACKEND_LABEL[server.backend] || server.backend, 'chip-backend'),
      st.operation ? chip(`${st.operation.type} in progress`, 'chip-op') : null,
      h('span', { class: 'muted small' }, `since ${fmtRelative(st.since)}`)),
    server.description ? h('p', { class: 'card-desc' }, server.description) : null,
    msg,
    h('div', { class: 'card-actions' },
      serverActions(server, { small: true }),
      h('a', { class: 'btn btn-small btn-ghost push-right', href: `#/servers/${server.id}` }, 'Details →')));
}

function meter(label, percent, detail) {
  const bar = h('div', { class: 'meter-bar' });
  const fill = h('div', { class: 'meter-fill' + (percent >= 90 ? ' is-crit' : percent >= 75 ? ' is-warn' : '') });
  fill.style.width = Math.max(0, Math.min(100, percent)) + '%';
  bar.append(fill);
  return h('div', { class: 'meter' },
    h('div', { class: 'meter-top' }, h('span', null, label), h('strong', null, percent.toFixed(1) + '%')),
    bar, detail ? h('div', { class: 'muted small' }, detail) : null);
}

export { meter };

export function dashboardPage() {
  const grid = h('div', { class: 'server-groups' });
  const summary = h('div', { class: 'host-summary' });
  const filter = h('input', { type: 'search', class: 'search', placeholder: 'Filter servers…', 'aria-label': 'Filter servers' });
  const el = h('div', { class: 'page' },
    h('div', { class: 'page-head' },
      h('h1', null, 'Servers'),
      h('div', { class: 'page-actions' }, filter,
        store.isAdmin() ? h('a', { class: 'btn btn-primary', href: '#/admin/servers/new' }, '+ Add server') : null)),
    summary, grid);

  function renderSummary() {
    const m = store.state.metrics.latest;
    if (!store.state.features.host_metrics || !m) { replace(summary); return; }
    const gb = (b) => (b / 1073741824).toFixed(1) + ' GB';
    replace(summary,
      meter('Host CPU', m.cpu_percent, `${m.cpu_count} cores · load ${m.load1.toFixed(2)}`),
      meter('Host memory', m.mem_used_percent, `${gb(m.mem_used_bytes)} of ${gb(m.mem_total_bytes)} used`));
  }

  function renderGrid() {
    if (!store.state.serversLoaded) { replace(grid, loading('Loading servers…')); return; }
    const q = filter.value.trim().toLowerCase();
    const all = store.sortedServers();
    const list = all.filter((s) => !q || `${s.name} ${s.description} ${s.category} ${s.id}`.toLowerCase().includes(q));
    if (!all.length) {
      replace(grid, emptyState('No servers yet',
        store.isAdmin() ? 'Add your first server definition to start managing it from here.' : 'You have not been given access to any server. Ask an administrator.',
        store.isAdmin() ? h('a', { class: 'btn btn-primary', href: '#/admin/servers/new' }, '+ Add server') : null));
      return;
    }
    if (!list.length) { replace(grid, emptyState('No matches', 'No server matches your filter.')); return; }
    const groups = new Map();
    for (const s of list) {
      const key = s.category || 'Uncategorized';
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(s);
    }
    replace(grid, [...groups.entries()].map(([name, items]) => h('section', { class: 'group' },
      groups.size > 1 || name !== 'Uncategorized' ? h('h2', { class: 'group-title' }, name, h('span', { class: 'count' }, String(items.length))) : null,
      h('div', { class: 'card-grid' }, items.map(card)))));
  }

  filter.addEventListener('input', renderGrid);
  const offs = [store.on('servers', renderGrid), store.on('metrics', renderSummary), store.on('me', renderGrid)];
  renderGrid();
  renderSummary();
  if (store.state.features.host_metrics && !store.state.metrics.latest) {
    api.get('/system/metrics').then((res) => {
      if (res.latest && !store.state.metrics.latest) {
        store.state.metrics.latest = res.latest;
        store.state.metrics.history = res.history || [];
        renderSummary();
      }
    }).catch(() => {});
  }
  // Keep "since 5m ago" labels fresh without refetching anything.
  const tick = setInterval(renderGrid, 60000);
  return { el, destroy: () => { offs.forEach((off) => off()); clearInterval(tick); } };
}
