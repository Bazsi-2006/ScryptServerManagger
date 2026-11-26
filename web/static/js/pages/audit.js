// Administration: audit log viewer with filters.

import * as api from '../api.js';
import * as store from '../store.js';
import { h, replace, loading, errorBox, chip, actionButton, field, fmtTime, emptyState } from '../ui.js';

const RESULT_CHIP = { SUCCESS: 'chip-ok', FAILURE: 'chip-bad', DENIED: 'chip-warn', REQUESTED: 'chip-op', SUBMITTED: 'chip-backend' };

function detailText(details) {
  const parts = [];
  for (const [k, v] of Object.entries(details || {})) {
    parts.push(`${k.replace(/_/g, ' ')}: ${typeof v === 'object' ? JSON.stringify(v) : v}`);
  }
  return parts.join(' · ');
}

function toRFC3339(localValue) {
  if (!localValue) return '';
  const d = new Date(localValue);
  return isNaN(d) ? '' : d.toISOString();
}

export function auditPage() {
  const user = h('input', { type: 'text', placeholder: 'any', autocapitalize: 'none' });
  const server = h('select', null, h('option', { value: '' }, 'Any server'),
    store.sortedServers().map((s) => h('option', { value: s.id }, s.name)));
  const action = h('select', null, h('option', { value: '' }, 'Any action'));
  const result = h('select', null, ['', 'SUCCESS', 'FAILURE', 'DENIED', 'REQUESTED', 'SUBMITTED'].map((r) => h('option', { value: r }, r || 'Any result')));
  const from = h('input', { type: 'datetime-local' });
  const to = h('input', { type: 'datetime-local' });
  const tbody = h('tbody');
  const more = h('div', { class: 'center' });
  const box = h('div', null, loading());
  let nextBefore = null;
  let actionsLoaded = false;

  function rows(entries) {
    return entries.map((e) => h('tr', null,
      h('td', { class: 'nowrap' }, fmtTime(e.time)),
      h('td', null, e.username || h('span', { class: 'muted' }, 'system')),
      h('td', null, e.server_id || h('span', { class: 'muted' }, '–')),
      h('td', null, h('code', null, e.action)),
      h('td', null, chip(e.result, RESULT_CHIP[e.result] || '')),
      h('td', { class: 'break small' }, detailText(e.details), e.ip ? h('span', { class: 'muted' }, (Object.keys(e.details || {}).length ? ' · ' : '') + 'from ' + e.ip) : null)));
  }

  async function load(append) {
    const params = {
      username: user.value.trim(), server_id: server.value, action: action.value, result: result.value,
      from: toRFC3339(from.value), to: toRFC3339(to.value), limit: 100,
    };
    if (append && nextBefore) params.before_id = nextBefore;
    try {
      const res = await api.get('/admin/audit' + api.query(params));
      if (!actionsLoaded) {
        for (const a of res.actions) action.append(h('option', { value: a }, a));
        actionsLoaded = true;
      }
      nextBefore = res.next_before_id;
      if (!append) {
        replace(tbody);
        if (!res.entries.length) { replace(box, emptyState('No matching entries', 'No audit entries match these filters.')); replace(more); return; }
        replace(box, h('div', { class: 'table-wrap' }, h('table', { class: 'table table-dense' },
          h('thead', null, h('tr', null, h('th', null, 'Time'), h('th', null, 'User'), h('th', null, 'Server'), h('th', null, 'Action'), h('th', null, 'Result'), h('th', null, 'Details'))),
          tbody)));
      }
      tbody.append(...rows(res.entries));
      replace(more, nextBefore ? actionButton('Load older entries', '', () => load(true)) : h('span', { class: 'muted small' }, 'End of results'));
    } catch (err) {
      replace(box, errorBox(err, () => load(false)));
    }
  }

  const form = h('form', { class: 'form filter-form' },
    field('User', user), field('Server', server), field('Action', action), field('Result', result),
    field('From', from), field('To', to),
    h('div', { class: 'field field-submit' },
      h('button', { class: 'btn btn-primary', type: 'submit' }, 'Apply'),
      h('button', { class: 'btn', type: 'reset' }, 'Reset')));
  form.addEventListener('submit', (e) => { e.preventDefault(); load(false); });
  form.addEventListener('reset', () => setTimeout(() => load(false), 0));
  load(false);

  const el = h('div', { class: 'page' },
    h('div', { class: 'page-head' }, h('h1', null, 'Audit log'),
      h('div', { class: 'page-actions' }, actionButton('Refresh', '', () => load(false)))),
    h('p', { class: 'muted' }, 'A permanent record of sign-ins, administrative changes, server operations and console commands.'),
    h('div', { class: 'card' }, form), box, more);
  return { el };
}
