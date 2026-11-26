// Administration: concurrency limits and server categories.

import * as api from '../api.js';
import * as store from '../store.js';
import {
  h, replace, loading, errorBox, actionButton, confirmDialog, openModal, notify, errorMessage, field, withBusy, emptyState, chip,
} from '../ui.js';

const slug = (s) => s.toLowerCase().normalize('NFKD').replace(/[^a-z0-9_-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 63) || 'category';

const limitText = (n) => (n ? String(n) : 'unlimited');

function usageCell(u) {
  if (!u.limit) return h('span', null, `${u.in_use} active`);
  const full = u.in_use >= u.limit;
  return h('span', null, h('strong', { class: full ? 'text-bad' : '' }, `${u.in_use} / ${u.limit}`), full ? ' ' : null, full ? chip('full', 'chip-bad') : null);
}

function categoryDialog({ category, preset, onDone }) {
  const isNew = !category;
  const c = category || { id: preset ? slug(preset) : '', name: preset || '', description: '', max_concurrent: 0 };
  const id = h('input', { type: 'text', value: c.id, disabled: !isNew, spellcheck: 'false', autocapitalize: 'none', placeholder: 'minecraft' });
  const name = h('input', { type: 'text', value: c.name, maxlength: 60, placeholder: 'Minecraft' });
  const desc = h('textarea', { rows: 2, value: c.description || '', maxlength: 500 });
  const limit = h('input', { type: 'number', min: 0, max: 10000, value: c.max_concurrent || 0 });
  if (isNew) name.addEventListener('input', () => { if (!id.dataset.touched) id.value = slug(name.value); });
  id.addEventListener('input', () => { id.dataset.touched = '1'; });
  const error = h('div', { class: 'alert alert-error', hidden: true });
  const save = h('button', { class: 'btn btn-primary', type: 'button' }, isNew ? 'Create category' : 'Save');
  const m = openModal({
    title: isNew ? 'New category' : `Edit ${c.name}`,
    body: h('div', { class: 'form' }, error,
      field('Name', name, preset ? `Servers that use "${preset}" as their category join this category.` : null),
      field('Category ID', id, 'Permanent: lowercase letters, digits, "_" and "-". Server definitions refer to it.'),
      field('Description (optional)', desc),
      field('Maximum concurrently running servers', limit, '0 means unlimited. Servers that are starting, running or stopping count.')),
    footer: [h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), save],
  });
  save.addEventListener('click', () => withBusy(save, async () => {
    const body = { id: id.value.trim(), name: name.value.trim(), description: desc.value.trim(), max_concurrent: Number(limit.value) || 0 };
    try {
      if (isNew) await api.post('/admin/categories', body);
      else await api.put(`/admin/categories/${api.enc(c.id)}`, body);
      notify.ok(isNew ? 'Category created.' : 'Category saved.');
      m.close();
      onDone();
    } catch (err) {
      const items = Array.isArray(err.details) ? err.details : [];
      replace(error, h('strong', null, errorMessage(err)), items.length ? h('ul', null, items.map((p) => h('li', null, String(p)))) : null);
      error.hidden = false;
    }
  }));
}

export function limitsPage() {
  const box = h('div', null, loading());

  async function load() {
    try {
      const [cats, settings] = await Promise.all([api.get('/admin/categories'), api.get('/admin/settings')]);
      render(cats, settings.settings);
    } catch (err) {
      replace(box, errorBox(err, load));
    }
  }

  function render(res, settings) {
    const cap = res.capacity;
    const servers = new Map([...store.state.servers.values()].map((s) => [s.id, s]));
    const names = (ids) => (ids || []).map((id) => servers.get(id)?.name || id).join(', ');

    // --- global limit ---
    const global = h('input', { type: 'number', min: 0, max: 10000, value: settings.limits.max_concurrent_servers || 0 });
    const saveGlobal = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Save');
    const globalForm = h('form', { class: 'form', novalidate: true },
      h('div', { class: 'field-row' },
        field('Maximum concurrently running servers', global, '0 means unlimited. Applies to all servers, together with the category limits.'),
        h('div', { class: 'field' }, h('label', null, 'Currently active'), h('div', { class: 'usage-now' }, usageCell(cap.global)))),
      h('div', { class: 'form-actions' }, saveGlobal));
    globalForm.addEventListener('submit', (e) => {
      e.preventDefault();
      withBusy(saveGlobal, async () => {
        try {
          await api.put('/admin/settings', { limits: { max_concurrent_servers: Number(global.value) || 0 } });
          notify.ok('Global limit saved.');
          load();
        } catch (err) {
          notify.error(errorMessage(err));
        }
      });
    });

    // --- categories ---
    const table = cap.categories.length ? h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
      h('thead', null, h('tr', null, h('th', null, 'Category'), h('th', null, 'Limit'), h('th', null, 'Active'), h('th', null, 'Servers'), h('th', null, ''))),
      h('tbody', null, cap.categories.map((c) => h('tr', null,
        h('td', null, h('strong', null, c.name), h('div', { class: 'muted small' }, c.id), c.description ? h('div', { class: 'muted small' }, c.description) : null),
        h('td', { class: 'nowrap' }, limitText(c.limit)),
        h('td', { class: 'nowrap' }, usageCell(c)),
        h('td', { class: 'break small' }, c.servers ? names(res.members[c.id]) : h('span', { class: 'muted' }, 'none')),
        h('td', { class: 'actions' },
          h('button', { class: 'btn btn-small', type: 'button', onclick: () => categoryDialog({ category: { ...c, max_concurrent: c.limit }, onDone: load }) }, 'Edit'),
          actionButton('Delete', 'btn-small btn-danger', async () => {
            const ok = await confirmDialog({ title: `Delete "${c.name}"?`, message: 'Its servers keep the name as an unmanaged category without a limit. You can assign them elsewhere afterwards.', confirmLabel: 'Delete category', danger: true });
            if (!ok) return;
            await api.del(`/admin/categories/${api.enc(c.id)}`);
            notify.ok('Category deleted.');
            load();
          }))))))) : emptyState('No categories yet', 'Create categories to group servers and limit how many of each kind may run at once.');

    const unmanaged = res.unmanaged.length ? h('fieldset', { class: 'section' }, h('legend', null, 'Unmanaged categories'),
      h('p', { class: 'hint' }, 'These category names are used by server definitions but are not managed yet, so no limit applies to them.'),
      h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
        h('tbody', null, res.unmanaged.map((u) => h('tr', null,
          h('td', null, h('strong', null, u.value)),
          h('td', { class: 'break small' }, names(u.servers)),
          h('td', { class: 'actions' }, h('button', { class: 'btn btn-small btn-primary', type: 'button', onclick: () => categoryDialog({ preset: u.value, onDone: load }) }, 'Manage')))))))) : null;

    replace(box,
      h('fieldset', { class: 'section' }, h('legend', null, 'Global limit'), globalForm),
      h('fieldset', { class: 'section' }, h('legend', null, 'Categories'),
        h('p', { class: 'hint' }, 'Both limits apply at the same time: a start is refused when either the global limit or the server\'s category limit is reached. A server keeps its slot until it is confirmed stopped; servers started outside Server Manager count too. Limits apply to administrators as well, who can override them explicitly (the override is audited).'),
        h('div', { class: 'tab-toolbar' }, h('span'), h('button', { class: 'btn btn-primary', type: 'button', onclick: () => categoryDialog({ onDone: load }) }, '+ Add category')),
        table,
        h('p', { class: 'hint' }, 'Assign a server to a category in its Settings tab.')),
      unmanaged);
  }

  const offs = [store.on('capacity', load), store.on('categories.changed', load)];
  load();
  const el = h('div', { class: 'page page-narrow' },
    h('div', { class: 'page-head' }, h('h1', null, 'Limits and categories')),
    h('p', { class: 'muted' }, 'Prevent too many servers from running at once, across everything and per category.'),
    box);
  return { el, destroy: () => offs.forEach((off) => off()) };
}
