// Administration: user accounts, roles, per-server permissions, sessions.

import * as api from '../api.js';
import * as store from '../store.js';
import {
  h, replace, loading, errorBox, chip, actionButton, confirmDialog, openModal, notify, errorMessage,
  field, fmtRelative, fmtTime, withBusy, emptyState,
} from '../ui.js';
import { permissionEditor } from './server.js';

function editDialog(user, reload) {
  const isSelf = user.id === store.state.user.id;
  const name = h('input', { type: 'text', value: user.display_name, maxlength: 80 });
  const email = h('input', { type: 'email', value: user.email || '', placeholder: 'none' });
  const admin = h('input', { type: 'checkbox', checked: user.is_admin, disabled: isSelf });
  const disabled = h('input', { type: 'checkbox', checked: user.disabled, disabled: isSelf });
  const save = h('button', { class: 'btn btn-primary', type: 'button' }, 'Save');
  const m = openModal({
    title: `Edit ${user.username}`,
    body: h('div', { class: 'form' },
      field('Display name', name),
      field('Email address', email, 'Set directly by an administrator, without verification. Leave empty to remove it.'),
      field('Administrator', admin, isSelf ? 'You cannot change your own role.' : 'Administrators can control every server and manage users and settings.', { inline: true }),
      field('Account disabled', disabled, isSelf ? 'You cannot disable your own account.' : 'A disabled account cannot sign in and is signed out everywhere immediately.', { inline: true })),
    footer: [h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), save],
  });
  save.addEventListener('click', () => withBusy(save, async () => {
    const body = {};
    if (name.value.trim() !== user.display_name) body.display_name = name.value.trim();
    if (email.value.trim() !== (user.email || '')) body.email = email.value.trim();
    if (!isSelf && admin.checked !== user.is_admin) body.is_admin = admin.checked;
    if (!isSelf && disabled.checked !== user.disabled) body.disabled = disabled.checked;
    if (!Object.keys(body).length) { m.close(); return; }
    if (body.is_admin === true) {
      const ok = await confirmDialog({ title: 'Grant administrator role?', message: `${user.username} will be able to control all servers, edit their commands, manage users and change settings.`, confirmLabel: 'Grant administrator', danger: true });
      if (!ok) return;
    }
    try {
      await api.patch(`/admin/users/${user.id}`, body);
      notify.ok('User updated.');
      m.close();
      reload();
    } catch (err) {
      notify.error(errorMessage(err));
    }
  }));
}

function permissionsDialog(user) {
  const body = h('div', null, loading());
  const m = openModal({ title: `Server permissions for ${user.username}`, body, wide: true, footer: [h('button', { class: 'btn btn-primary', onclick: () => m.close() }, 'Done')] });
  api.get(`/admin/users/${user.id}`).then((res) => {
    const servers = store.sortedServers();
    if (user.is_admin) {
      replace(body, h('div', { class: 'alert alert-info' }, 'Administrators have full access to every server; individual permissions do not apply.'));
      return;
    }
    if (!servers.length) { replace(body, emptyState('No servers', 'Add a server first.')); return; }
    replace(body,
      h('p', { class: 'hint' }, 'Everything is denied unless ticked. Changes are saved immediately and apply to the user right away.'),
      servers.map((s) => {
        const status = h('span', { class: 'muted small' });
        let timer = null;
        const granted = new Set((res.action_permissions || {})[s.id] || []);
        const actions = s.actions || [];
        return h('div', { class: 'perm-row' },
          h('div', { class: 'perm-user' }, h('strong', null, s.name), s.category_name ? h('span', { class: 'muted' }, ' · ' + s.category_name) : null, status),
          permissionEditor(res.permissions[s.id] || 0, (mask) => {
            clearTimeout(timer);
            status.textContent = 'Saving…';
            timer = setTimeout(async () => {
              try {
                await api.put(`/admin/users/${user.id}/permissions/${api.enc(s.id)}`, { mask });
                status.textContent = 'Saved';
              } catch (err) {
                status.textContent = '';
                notify.error(errorMessage(err));
              }
            }, 350);
          }),
          actions.length ? h('div', { class: 'perm-actions' }, h('div', { class: 'perm-actions-title' }, 'Custom actions (need at least one permission above)'),
            h('div', { class: 'perm-grid' }, actions.map((a) => {
              const input = h('input', { type: 'checkbox', checked: granted.has(a.id) });
              input.addEventListener('change', async () => {
                input.disabled = true;
                try {
                  await api.put(`/admin/users/${user.id}/actions/${api.enc(s.id)}/${api.enc(a.id)}`, { allowed: input.checked });
                  status.textContent = 'Saved';
                } catch (err) {
                  input.checked = !input.checked;
                  notify.error(errorMessage(err));
                } finally {
                  input.disabled = false;
                }
              });
              return h('label', { class: 'perm' }, input, h('span', null, h('strong', null, (a.icon ? a.icon + ' ' : '') + a.name), h('small', null, a.description || (a.method === 'tmux' ? 'Console command' : 'Script'))));
            }))) : null);
      }));
  }).catch((err) => replace(body, errorBox(err)));
}

function sessionsDialog(user) {
  const body = h('div', null, loading());
  const m = openModal({ title: `Active sessions of ${user.username}`, body, wide: true, footer: [h('button', { class: 'btn', onclick: () => m.close() }, 'Close')] });
  async function load() {
    try {
      const res = await api.get(`/admin/users/${user.id}`);
      if (!res.sessions.length) { replace(body, emptyState('No active sessions', 'This user is not signed in anywhere.')); return; }
      replace(body,
        h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
          h('thead', null, h('tr', null, h('th', null, 'Signed in'), h('th', null, 'Last used'), h('th', null, 'Address'), h('th', null, 'Browser'), h('th', null, ''))),
          h('tbody', null, res.sessions.map((s) => h('tr', null,
            h('td', { class: 'nowrap' }, fmtTime(s.created_at)),
            h('td', { class: 'nowrap' }, fmtRelative(s.last_used_at)),
            h('td', null, s.ip || '–'),
            h('td', { class: 'break small' }, s.user_agent || '–'),
            h('td', { class: 'actions' }, s.current ? chip('this session', 'chip-ok')
              : actionButton('Revoke', 'btn-small btn-danger', async () => { await api.del(`/admin/users/${user.id}/sessions/${api.enc(s.id)}`); load(); }))))))),
        h('div', { class: 'form-actions' }, actionButton('Revoke all sessions', 'btn-danger', async () => {
          const res2 = await api.del(`/admin/users/${user.id}/sessions`);
          notify.ok(`${res2.revoked} session(s) revoked.`);
          load();
        })));
    } catch (err) {
      replace(body, errorBox(err, load));
    }
  }
  load();
}

export function usersPage() {
  const box = h('div', null, loading());
  async function load() {
    try {
      const res = await api.get('/admin/users');
      const me = store.state.user.id;
      replace(box, h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
        h('thead', null, h('tr', null, h('th', null, 'User'), h('th', null, 'Email'), h('th', null, 'Role'), h('th', null, 'Status'), h('th', null, 'Last sign-in'), h('th', null, ''))),
        h('tbody', null, res.users.map((u) => h('tr', { class: u.disabled ? 'is-muted' : '' },
          h('td', null, h('strong', null, u.display_name || u.username), h('div', { class: 'muted small' }, '@' + u.username, u.id === me ? ' (you)' : '')),
          h('td', { class: 'break' }, u.email || h('span', { class: 'muted' }, '–')),
          h('td', null, u.is_admin ? chip('Administrator', 'chip-admin') : chip('User')),
          h('td', null, u.disabled ? chip('Disabled', 'chip-bad') : chip('Active', 'chip-ok')),
          h('td', { class: 'nowrap', title: fmtTime(u.last_login_at) }, fmtRelative(u.last_login_at)),
          h('td', { class: 'actions' },
            h('button', { class: 'btn btn-small', onclick: () => editDialog(u, load) }, 'Edit'),
            h('button', { class: 'btn btn-small', onclick: () => permissionsDialog(u) }, 'Permissions'),
            h('button', { class: 'btn btn-small', onclick: () => sessionsDialog(u) }, 'Sessions'),
            u.id !== me ? actionButton('Delete', 'btn-small btn-danger', async () => {
              const ok = await confirmDialog({ title: `Delete ${u.username}?`, message: 'The account, its sessions and its permission assignments are removed permanently. Audit log entries are kept.', confirmLabel: 'Delete account', danger: true });
              if (!ok) return;
              await api.del(`/admin/users/${u.id}`);
              notify.ok('Account deleted.');
              load();
            }) : null)))))));
    } catch (err) {
      replace(box, errorBox(err, load));
    }
  }
  const off = store.on('users.changed', load);
  load();
  const el = h('div', { class: 'page' },
    h('div', { class: 'page-head' }, h('h1', null, 'Users'),
      h('div', { class: 'page-actions' }, h('a', { class: 'btn btn-primary', href: '#/admin/invitations' }, 'Invite a user'))),
    h('p', { class: 'muted' }, 'Accounts are created through invitations. Regular users only see the servers they have been given permissions for.'),
    box);
  return { el, destroy: off };
}
