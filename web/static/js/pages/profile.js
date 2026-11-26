// The signed-in user's own profile: name, email, password and sessions.

import * as api from '../api.js';
import * as store from '../store.js';
import { h, replace, loading, errorBox, chip, actionButton, notify, errorMessage, field, withBusy, fmtTime, fmtRelative } from '../ui.js';

function formCard(title, intro, fields, submitLabel, onSubmit) {
  const btn = h('button', { class: 'btn btn-primary', type: 'submit' }, submitLabel);
  const form = h('form', { class: 'form card', novalidate: true },
    h('h2', null, title), intro ? h('p', { class: 'hint' }, intro) : null, fields, h('div', { class: 'form-actions' }, btn));
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    withBusy(btn, async () => {
      try { await onSubmit(form); } catch (err) { notify.error(errorMessage(err)); }
    });
  });
  return form;
}

const NOTIFY_KINDS = [
  ['on_ready', 'Server started and ready', 'After the readiness check confirms the server is up.'],
  ['on_stopped', 'Server stopped', 'A normal shutdown, through Server Manager or observed by monitoring.'],
  ['on_crash', 'Server crashed or stopped unexpectedly', 'The server went down without being asked to.'],
  ['on_failure', 'Start or stop failed', 'An operation did not succeed.'],
];

/** Notification preferences: global switch, per-event toggles and per-server muting. */
function notificationsCard() {
  const card = h('div', { class: 'card' }, loading());
  async function load() {
    try {
      render(await api.get('/me/notifications'));
    } catch (err) {
      replace(card, errorBox(err, load));
    }
  }
  function render(res) {
    const p = res.preferences;
    const muted = new Set(p.muted || []);
    const enabled = h('input', { type: 'checkbox', checked: p.enabled });
    const kinds = NOTIFY_KINDS.map(([key, label, hint]) => [key, h('input', { type: 'checkbox', checked: !!p[key] }), label, hint]);
    const servers = store.sortedServers().map((s) => [s.id, h('input', { type: 'checkbox', checked: !muted.has(s.id) }), s.name]);
    const blockers = [];
    if (!res.email_available) blockers.push('Email delivery is not configured on this installation, so no notifications can be sent.');
    else if (!res.globally_enabled) blockers.push('An administrator has turned notifications off for this installation.');
    if (!res.email) blockers.push('Add an email address first.');
    else if (!res.email_verified) blockers.push('Verify your email address first: notifications are only sent to verified addresses.');
    const sync = () => {
      for (const [, input] of kinds) input.disabled = !enabled.checked;
      for (const [, input] of servers) input.disabled = !enabled.checked;
    };
    enabled.addEventListener('change', sync);
    const save = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Save notification settings');
    const form = h('form', { class: 'form', novalidate: true },
      h('h2', null, 'Email notifications'),
      h('p', { class: 'hint' }, 'Optional emails about servers you have access to. Everything is off until you turn it on.'),
      blockers.length ? h('div', { class: 'alert alert-warn' }, blockers.join(' ')) : null,
      field('Send me email notifications', enabled, null, { inline: true }),
      h('fieldset', { class: 'subsection' }, h('legend', null, 'Notify me when'),
        kinds.map(([, input, label, hint]) => field(label, input, hint, { inline: true }))),
      servers.length ? h('fieldset', { class: 'subsection' }, h('legend', null, 'For these servers'),
        h('div', { class: 'check-grid' }, servers.map(([, input, name]) => field(name, input, null, { inline: true })))) : null,
      h('div', { class: 'form-actions' }, save));
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      withBusy(save, async () => {
        try {
          const body = { enabled: enabled.checked, muted: servers.filter(([, input]) => !input.checked).map(([id]) => id) };
          for (const [key, input] of kinds) body[key] = input.checked;
          render(await api.put('/me/notifications', body));
          notify.ok('Notification settings saved.');
        } catch (err) {
          notify.error(errorMessage(err));
        }
      });
    });
    sync();
    replace(card, form);
  }
  load();
  return card;
}

export function profilePage() {
  const top = h('div', { class: 'profile-grid' });
  const sessions = h('div', { class: 'card' }, loading());
  const notifications = notificationsCard();

  function renderForms() {
    const u = store.state.user;
    if (!u) return;
    const name = h('input', { type: 'text', value: u.display_name, maxlength: 80, required: true, autocomplete: 'name' });
    const newEmail = h('input', { type: 'email', placeholder: 'new@example.com', autocomplete: 'email' });
    const emailPw = h('input', { type: 'password', autocomplete: 'current-password' });
    const curPw = h('input', { type: 'password', autocomplete: 'current-password' });
    const pw1 = h('input', { type: 'password', autocomplete: 'new-password' });
    const pw2 = h('input', { type: 'password', autocomplete: 'new-password' });

    replace(top,
      formCard('Profile', null, [
        h('dl', { class: 'kv' }, h('dt', null, 'Username'), h('dd', null, u.username),
          h('dt', null, 'Role'), h('dd', null, u.is_admin ? chip('Administrator', 'chip-admin') : chip('User')),
          h('dt', null, 'Member since'), h('dd', null, fmtTime(u.created_at))),
        field('Display name', name),
      ], 'Save', async () => {
        const res = await api.patch('/me', { display_name: name.value.trim() });
        store.state.user = res.user;
        store.emit('me');
        notify.ok('Profile updated.');
      }),
      formCard('Email address', store.state.features.email_available
        ? 'A confirmation link is sent to the new address. Your current address stays in use until you open that link.'
        : 'Email delivery is not configured, so addresses cannot be verified. Ask an administrator to change yours.', [
        h('dl', { class: 'kv' },
          h('dt', null, 'Current address'), h('dd', { class: 'break' }, u.email || 'none', ' ',
            u.email ? (u.email_verified ? chip('verified', 'chip-ok') : chip('not verified', 'chip-warn')) : null),
          store.state.pendingEmail ? [h('dt', null, 'Awaiting confirmation'), h('dd', { class: 'break' }, store.state.pendingEmail.email,
            h('div', { class: 'muted small' }, `Link valid until ${fmtTime(store.state.pendingEmail.expires_at)}`))] : null),
        u.email && !u.email_verified && store.state.features.email_available ? h('div', { class: 'alert alert-info' },
          'Your address is not verified yet. Verified addresses can receive notifications', u.is_admin ? '.' : ' and password reset links.', ' ',
          actionButton('Send verification email', 'btn-small', async () => {
            const res = await api.post('/me/email/verification');
            notify.ok(`A verification link was sent to ${res.sent_to}.`);
          })) : null,
        field('New email address', newEmail),
        field('Current password', emailPw),
      ], 'Send confirmation link', async () => {
        const res = await api.post('/me/email', { email: newEmail.value.trim(), current_password: emailPw.value });
        emailPw.value = '';
        newEmail.value = '';
        notify.ok(`A confirmation link was sent to ${res.pending_email}.`);
        store.loadMe().catch(() => {});
      }),
      formCard('Password', `At least ${store.state.features.min_password_length || 10} characters. Changing it signs you out on all other devices.`, [
        field('Current password', curPw),
        field('New password', pw1),
        field('Repeat new password', pw2),
      ], 'Change password', async () => {
        if (pw1.value !== pw2.value) throw new Error('The new passwords do not match.');
        const res = await api.post('/me/password', { current_password: curPw.value, new_password: pw1.value });
        curPw.value = ''; pw1.value = ''; pw2.value = '';
        notify.ok(`Password changed. ${res.sessions_revoked} other session(s) were signed out.`);
        loadSessions();
      }));
  }

  async function loadSessions() {
    try {
      const res = await api.get('/me/sessions');
      replace(sessions,
        h('div', { class: 'tab-toolbar' }, h('h2', null, 'Active sessions'),
          res.sessions.length > 1 ? actionButton('Sign out all other sessions', 'btn-small btn-danger', async () => {
            const r = await api.post('/me/sessions/revoke-others');
            notify.ok(`${r.revoked} session(s) signed out.`);
            loadSessions();
          }) : null),
        h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
          h('thead', null, h('tr', null, h('th', null, 'Signed in'), h('th', null, 'Last used'), h('th', null, 'Address'), h('th', null, 'Browser'), h('th', null, ''))),
          h('tbody', null, res.sessions.map((s) => h('tr', null,
            h('td', { class: 'nowrap' }, fmtTime(s.created_at)),
            h('td', { class: 'nowrap' }, fmtRelative(s.last_used_at)),
            h('td', null, s.ip || '–'),
            h('td', { class: 'break small' }, s.user_agent || '–'),
            h('td', { class: 'actions' }, s.current ? chip('this device', 'chip-ok')
              : actionButton('Sign out', 'btn-small', async () => { await api.del('/me/sessions/' + api.enc(s.id)); loadSessions(); }))))))));
    } catch (err) {
      replace(sessions, errorBox(err, loadSessions));
    }
  }

  const off = store.on('me', renderForms);
  renderForms();
  loadSessions();
  const el = h('div', { class: 'page' }, h('div', { class: 'page-head' }, h('h1', null, 'Your profile')), top, notifications, sessions);
  return { el, destroy: off };
}
