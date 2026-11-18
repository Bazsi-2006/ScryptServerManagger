// Small DOM toolkit: element builder, toasts, modal dialogs, formatting.
// Everything is built with DOM APIs (never innerHTML with data), so server
// names, console output and other untrusted text cannot inject markup.

/** h('div', {class: 'x', onclick: fn}, child, ...) */
export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === null || v === undefined || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'dataset') Object.assign(el.dataset, v);
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else if (k === 'value') el.value = v;
      else if (k === 'checked' || k === 'disabled' || k === 'selected' || k === 'readOnly' || k === 'required' || k === 'hidden') el[k] = !!v;
      else if (v === true) el.setAttribute(k, '');
      else el.setAttribute(k, v);
    }
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue;
    if (Array.isArray(c)) append(el, c);
    else el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

export function replace(el, ...children) {
  clear(el);
  append(el, children);
  return el;
}

// ---- toasts ----

export function toast(message, kind = 'info', ms) {
  const root = document.getElementById('toasts');
  const el = h('div', { class: `toast toast-${kind}`, role: kind === 'error' ? 'alert' : 'status' },
    h('span', { class: 'toast-msg' }, message),
    h('button', { class: 'toast-close', 'aria-label': 'Dismiss', onclick: () => el.remove() }, '×'));
  root.append(el);
  setTimeout(() => el.remove(), ms ?? (kind === 'error' ? 9000 : 4500));
  while (root.children.length > 5) root.firstChild.remove();
}

export const notify = {
  ok: (m) => toast(m, 'success'),
  info: (m) => toast(m, 'info'),
  warn: (m) => toast(m, 'warn', 7000),
  error: (m) => toast(m, 'error'),
};

/** Turn any thrown value into a message for the user. */
export function errorMessage(err) {
  if (!err) return 'Something went wrong.';
  if (err.message) return err.message;
  return String(err);
}

// ---- modal dialogs ----

let modalStack = 0;

/** openModal({title, body, footer, wide}) -> {el, close} */
export function openModal({ title, body, footer, wide, onClose }) {
  const root = document.getElementById('modal-root');
  const previous = document.activeElement;
  const close = () => {
    overlay.remove();
    document.removeEventListener('keydown', onKey);
    modalStack--;
    if (!modalStack) document.body.classList.remove('modal-open');
    if (onClose) onClose();
    if (previous && previous.focus) previous.focus();
  };
  const onKey = (e) => {
    if (e.key === 'Escape' && overlay === root.lastElementChild) { e.preventDefault(); close(); }
  };
  const dialog = h('div', { class: 'modal' + (wide ? ' modal-wide' : ''), role: 'dialog', 'aria-modal': 'true', 'aria-label': title },
    h('div', { class: 'modal-head' }, h('h2', null, title),
      h('button', { class: 'icon-btn', 'aria-label': 'Close', onclick: close }, '×')),
    h('div', { class: 'modal-body' }, body),
    footer ? h('div', { class: 'modal-foot' }, footer) : null);
  const overlay = h('div', { class: 'modal-overlay', onmousedown: (e) => { if (e.target === overlay) close(); } }, dialog);
  root.append(overlay);
  modalStack++;
  document.body.classList.add('modal-open');
  document.addEventListener('keydown', onKey);
  const focusable = dialog.querySelector('input, select, textarea, button.btn');
  if (focusable) focusable.focus();
  return { el: dialog, close };
}

/** confirmDialog(...) resolves true when the user confirms. */
export function confirmDialog({ title, message, confirmLabel = 'Confirm', danger = false, extra }) {
  return new Promise((resolve) => {
    let done = false;
    const finish = (v) => { if (!done) { done = true; resolve(v); } };
    const m = openModal({
      title,
      body: h('div', null, h('p', null, message), extra || null),
      footer: [
        h('button', { class: 'btn', onclick: () => { finish(false); m.close(); } }, 'Cancel'),
        h('button', { class: 'btn ' + (danger ? 'btn-danger' : 'btn-primary'), onclick: () => { finish(true); m.close(); } }, confirmLabel),
      ],
      onClose: () => finish(false),
    });
  });
}

// ---- busy buttons ----

/** Run an async action with the button disabled and a spinner shown. */
export async function withBusy(button, fn) {
  if (button.disabled) return undefined;
  button.disabled = true;
  button.classList.add('is-busy');
  try {
    return await fn();
  } finally {
    button.disabled = false;
    button.classList.remove('is-busy');
  }
}

/** A button whose click handler is an async action with error toasts. */
export function actionButton(label, cls, fn, attrs = {}) {
  const b = h('button', { class: 'btn ' + (cls || ''), type: 'button', ...attrs }, label);
  b.addEventListener('click', () => withBusy(b, async () => {
    try { await fn(b); } catch (err) { notify.error(errorMessage(err)); }
  }));
  return b;
}

// ---- formatting ----

export function fmtBytes(n) {
  if (n === null || n === undefined) return '–';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = Number(n);
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(v >= 100 ? 0 : 1)) + ' ' + units[i];
}

export function fmtTime(value) {
  if (!value) return '–';
  const d = new Date(value);
  if (isNaN(d)) return '–';
  return d.toLocaleString(undefined, { year: 'numeric', month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

export function fmtRelative(value) {
  if (!value) return 'never';
  const d = new Date(value);
  const s = Math.round((Date.now() - d.getTime()) / 1000);
  if (isNaN(s)) return '–';
  if (s < 5) return 'just now';
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  if (s < 86400 * 30) return `${Math.floor(s / 86400)}d ago`;
  return d.toLocaleDateString();
}

export function fmtDuration(seconds) {
  const s = Math.max(0, Math.floor(seconds));
  const d = Math.floor(s / 86400);
  const hh = Math.floor((s % 86400) / 3600);
  const mm = Math.floor((s % 3600) / 60);
  if (d) return `${d}d ${hh}h`;
  if (hh) return `${hh}h ${mm}m`;
  if (mm) return `${mm}m`;
  return `${s}s`;
}

// ---- shared widgets ----

const STATE_LABEL = { STOPPED: 'Stopped', STARTING: 'Starting', RUNNING: 'Running', STOPPING: 'Stopping', ERROR: 'Error', UNKNOWN: 'Unknown' };

export function statusBadge(state) {
  const s = STATE_LABEL[state] ? state : 'UNKNOWN';
  return h('span', { class: `badge state-${s.toLowerCase()}`, title: `State: ${STATE_LABEL[s]}` },
    h('span', { class: 'dot', 'aria-hidden': 'true' }), STATE_LABEL[s]);
}

export function chip(text, cls = '') {
  return h('span', { class: 'chip ' + cls }, text);
}

export function emptyState(title, text, action) {
  return h('div', { class: 'empty' }, h('h3', null, title), text ? h('p', null, text) : null, action || null);
}

export function loading(text = 'Loading…') {
  return h('div', { class: 'loading' }, h('div', { class: 'spinner', 'aria-hidden': 'true' }), h('span', null, text));
}

export function errorBox(err, retry) {
  return h('div', { class: 'alert alert-error' }, h('span', null, errorMessage(err)),
    retry ? h('button', { class: 'btn btn-small', onclick: retry }, 'Retry') : null);
}

/** Labelled form field wrapper. */
export function field(label, control, hint, opts = {}) {
  const id = control.id || (control.id = 'f' + Math.random().toString(36).slice(2, 9));
  return h('div', { class: 'field' + (opts.inline ? ' field-inline' : ''), dataset: opts.name ? { field: opts.name } : {} },
    opts.inline ? [control, h('label', { for: id }, label)] : [h('label', { for: id }, label), control],
    hint ? h('div', { class: 'hint' }, hint) : null);
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    notify.ok('Copied to clipboard.');
  } catch {
    notify.warn('Could not copy automatically; select the text and copy it manually.');
  }
}
