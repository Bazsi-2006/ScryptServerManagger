// Administration: invitations.

import * as api from '../api.js';
import * as store from '../store.js';
import { h, replace, loading, errorBox, chip, actionButton, confirmDialog, openModal, notify, errorMessage, field, fmtTime, withBusy, emptyState, copyText } from '../ui.js';

const STATUS_CHIP = { pending: 'chip-op', used: 'chip-ok', revoked: 'chip-bad', expired: 'chip-warn' };

/** Show the outcome of creating or re-sending an invitation. */
function showResult(res) {
  const parts = [];
  if (res.email && res.email.sent) {
    parts.push(h('div', { class: 'alert alert-success' }, `The invitation was emailed to ${res.invitation.email}.`));
  } else if (res.email && res.email.logged) {
    parts.push(h('div', { class: 'alert alert-info' }, 'Development mode: the email was written to the application log instead of being sent.'));
  } else if (res.email && res.email.error) {
    parts.push(h('div', { class: 'alert alert-warn' }, res.email.error));
  }
  if (res.link) {
    const box = h('input', { type: 'text', class: 'code', readOnly: true, value: res.link, 'aria-label': 'Invitation link' });
    box.addEventListener('focus', () => box.select());
    parts.push(
      h('p', null, 'Give this link to the person you are inviting. It works once and is shown only now.'),
      h('div', { class: 'copy-row' }, box, h('button', { class: 'btn btn-primary', type: 'button', onclick: () => copyText(res.link) }, 'Copy')));
  }
  parts.push(h('p', { class: 'hint' }, `Expires ${fmtTime(res.invitation.expires_at)}.`));
  const m = openModal({ title: 'Invitation ready', body: h('div', null, parts), footer: [h('button', { class: 'btn btn-primary', onclick: () => m.close() }, 'Done')] });
}

export function invitationsPage() {
  const box = h('div', null, loading());
  const email = h('input', { type: 'email', placeholder: 'person@example.com (optional)' });
  const hours = h('input', { type: 'number', min: 1, max: 2160, placeholder: 'default' });
  const create = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Create invitation');
  const form = h('form', { class: 'form inline-form' },
    field('Recipient email', email, store.state.features.email_available ? 'If given, the link is emailed to this address.' : 'Email delivery is not configured; you will get a link to share manually.'),
    field('Valid for (hours)', hours),
    h('div', { class: 'field field-submit' }, create));
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    withBusy(create, async () => {
      try {
        const body = {};
        if (email.value.trim()) body.email = email.value.trim();
        if (hours.value) body.expires_in_hours = Number(hours.value);
        const res = await api.post('/admin/invitations', body);
        email.value = '';
        showResult(res);
        load();
      } catch (err) {
        notify.error(errorMessage(err));
      }
    });
  });

  async function load() {
    try {
      const res = await api.get('/admin/invitations');
      if (!res.invitations.length) { replace(box, emptyState('No invitations yet', 'Create an invitation to let someone register an account.')); return; }
      replace(box, h('div', { class: 'table-wrap' }, h('table', { class: 'table' },
        h('thead', null, h('tr', null, h('th', null, 'Recipient'), h('th', null, 'Status'), h('th', null, 'Created'), h('th', null, 'Expires'), h('th', null, 'Used by'), h('th', null, ''))),
        h('tbody', null, res.invitations.map((inv) => h('tr', { class: inv.status === 'pending' ? '' : 'is-muted' },
          h('td', { class: 'break' }, inv.email || h('span', { class: 'muted' }, 'link only')),
          h('td', null, chip(inv.status, STATUS_CHIP[inv.status] || '')),
          h('td', { class: 'nowrap' }, fmtTime(inv.created_at), h('div', { class: 'muted small' }, 'by ' + (inv.created_by_name || 'deleted user'))),
          h('td', { class: 'nowrap' }, fmtTime(inv.expires_at)),
          h('td', null, inv.used_by_name || (inv.used_at ? 'deleted user' : '–')),
          h('td', { class: 'actions' }, inv.status === 'pending' || inv.status === 'expired' ? [
            actionButton(inv.email ? 'Resend' : 'New link', 'btn-small', async () => {
              const ok = await confirmDialog({ title: 'Issue a new link?', message: 'A fresh link is generated and the previous one stops working.' + (inv.email ? ` It will be emailed to ${inv.email}.` : ''), confirmLabel: 'Issue new link' });
              if (!ok) return;
              showResult(await api.post(`/admin/invitations/${inv.id}/resend`));
              load();
            }),
            actionButton('Revoke', 'btn-small btn-danger', async () => {
              const ok = await confirmDialog({ title: 'Revoke invitation?', message: 'The link will stop working immediately.', confirmLabel: 'Revoke', danger: true });
              if (!ok) return;
              await api.del(`/admin/invitations/${inv.id}`);
              notify.ok('Invitation revoked.');
              load();
            })] : null)))))));
    } catch (err) {
      replace(box, errorBox(err, load));
    }
  }
  const off = store.on('invitations.changed', load);
  load();
  const el = h('div', { class: 'page' },
    h('div', { class: 'page-head' }, h('h1', null, 'Invitations')),
    h('p', { class: 'muted' }, 'There is no public registration. Each invitation link lets exactly one person create an account before it expires. New accounts start without access to any server.'),
    h('div', { class: 'card' }, form),
    box);
  return { el, destroy: off };
}
