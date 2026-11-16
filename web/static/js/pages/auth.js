// Sign-in, invitation registration and email verification pages.

import * as api from '../api.js';
import { h, replace, field, withBusy, errorMessage, loading, fmtTime } from '../ui.js';

function authCard(title, ...children) {
  return h('div', { class: 'auth-wrap' },
    h('div', { class: 'auth-card' },
      h('div', { class: 'auth-brand' }, h('img', { src: '/favicon.svg', alt: '', width: 40, height: 40 }), h('h1', null, 'Server Manager')),
      h('h2', null, title), ...children));
}

export function loginPage(ctx) {
  const username = h('input', { type: 'text', autocomplete: 'username', required: true, autocapitalize: 'none', spellcheck: 'false' });
  const password = h('input', { type: 'password', autocomplete: 'current-password', required: true });
  const error = h('div', { class: 'alert alert-error', hidden: true, role: 'alert' });
  const submit = h('button', { class: 'btn btn-primary btn-block', type: 'submit' }, 'Sign in');
  const form = h('form', { class: 'form', novalidate: true },
    error,
    field('Username', username),
    field('Password', password),
    submit,
    h('p', { class: 'hint center' }, 'Accounts are created by invitation only. Ask an administrator for an invitation link.'));
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    error.hidden = true;
    withBusy(submit, async () => {
      try {
        await api.login(username.value.trim(), password.value);
        await ctx.enterApp();
      } catch (err) {
        password.value = '';
        error.textContent = errorMessage(err);
        error.hidden = false;
        password.focus();
      }
    });
  });
  const el = authCard('Sign in', form);
  setTimeout(() => username.focus(), 0);
  return { el };
}

export function registerPage(ctx) {
  const token = ctx.params.get('token') || '';
  const body = h('div', null, loading('Checking your invitation…'));
  const el = authCard('Create your account', body);

  const invalid = (msg) => replace(body,
    h('div', { class: 'alert alert-error' }, msg),
    h('a', { class: 'btn btn-block', href: '#/login' }, 'Go to sign in'));

  if (!token) {
    invalid('This registration link is incomplete. Open the full link from your invitation.');
    return { el };
  }

  api.publicPost('/auth/invitation', { token }).then((info) => {
    const username = h('input', { type: 'text', autocomplete: 'username', required: true, autocapitalize: 'none', spellcheck: 'false', maxlength: 32 });
    const display = h('input', { type: 'text', autocomplete: 'name', maxlength: 80 });
    const pw1 = h('input', { type: 'password', autocomplete: 'new-password', required: true });
    const pw2 = h('input', { type: 'password', autocomplete: 'new-password', required: true });
    const error = h('div', { class: 'alert alert-error', hidden: true, role: 'alert' });
    const submit = h('button', { class: 'btn btn-primary btn-block', type: 'submit' }, 'Create account');
    const form = h('form', { class: 'form', novalidate: true },
      h('p', { class: 'hint' }, `This invitation expires on ${fmtTime(info.expires_at)} and can be used once.`),
      info.email ? h('p', { class: 'hint' }, 'Your account will use the email address ', h('strong', null, info.email), '.') : null,
      error,
      field('Username', username, '3–32 characters: letters, digits, dot, dash or underscore.'),
      field('Display name (optional)', display),
      field('Password', pw1, `At least ${info.min_password_length} characters.`),
      field('Repeat password', pw2),
      submit);
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      error.hidden = true;
      const fail = (m) => { error.textContent = m; error.hidden = false; };
      if (pw1.value !== pw2.value) return fail('The passwords do not match.');
      withBusy(submit, async () => {
        try {
          await api.register({ token, username: username.value.trim(), display_name: display.value.trim(), password: pw1.value });
          // Drop the token from the address bar before entering the app.
          history.replaceState(null, '', '#/');
          await ctx.enterApp();
        } catch (err) {
          fail(errorMessage(err));
        }
      });
      return undefined;
    });
    replace(body, form);
    username.focus();
  }).catch((err) => invalid(errorMessage(err)));

  return { el };
}

export function verifyEmailPage(ctx) {
  const token = ctx.params.get('token') || '';
  const body = h('div', null, loading('Confirming your email address…'));
  const el = authCard('Email verification', body);
  const done = (ok, msg) => replace(body,
    h('div', { class: 'alert ' + (ok ? 'alert-success' : 'alert-error') }, msg),
    h('a', { class: 'btn btn-primary btn-block', href: '#/' }, 'Continue'));
  if (!token) {
    done(false, 'This verification link is incomplete.');
    return { el };
  }
  api.publicPost('/auth/email/verify', { token })
    .then((res) => {
      history.replaceState(null, '', '#/verify-email');
      done(true, `Your email address is now ${res.email}.`);
    })
    .catch((err) => done(false, errorMessage(err)));
  return { el };
}
