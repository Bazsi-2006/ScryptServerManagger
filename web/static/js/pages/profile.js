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

export function profilePage() {
  const top = h('div', { class: 'profile-grid' });
  const sessions = h('div', { class: 'card' }, loading());

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
        ? 'A confirmation link is sent to the new address. Your address only changes after you open that link.'
        : 'Email delivery is not configured, so addresses cannot be verified. Ask an administrator to change yours.', [
        h('dl', { class: 'kv' }, h('dt', null, 'Current address'), h('dd', { class: 'break' }, u.email || 'none')),
        field('New email address', newEmail),
        field('Current password', emailPw),
      ], 'Send confirmation link', async () => {
        const res = await api.post('/me/email', { email: newEmail.value.trim(), current_password: emailPw.value });
        emailPw.value = '';
        newEmail.value = '';
        notify.ok(`A confirmation link was sent to ${res.pending_email}.`);
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
  const el = h('div', { class: 'page' }, h('div', { class: 'page-head' }, h('h1', null, 'Your profile')), top, sessions);
  return { el, destroy: off };
}
