// Administration: application settings.

import * as api from '../api.js';
import * as store from '../store.js';
import { h, replace, loading, errorBox, actionButton, notify, errorMessage, field, withBusy } from '../ui.js';
import { getPath, setPath } from './serverform.js';

// [path, label, type, hint]
const SECTIONS = [
  ['General', 'Where users reach this application.', [
    ['public_url', 'Public URL', 'text', 'Used in invitation and verification emails, e.g. https://servers.example.com. Required in production.'],
    ['trusted_proxies', 'Trusted reverse proxies', 'list', 'IP addresses or CIDR ranges whose X-Forwarded-For / X-Forwarded-Proto headers are honoured. One per line.'],
    ['cors.allowed_origins', 'Additional allowed origins (CORS)', 'list', 'Leave empty for same-origin only (recommended). One origin per line.'],
  ]],
  ['Sign-in and sessions', null, [
    ['auth.access_token_ttl', 'Access token lifetime', 'text', 'e.g. 10m'],
    ['auth.refresh_token_ttl', 'Session lifetime without activity', 'text', 'e.g. 720h'],
    ['auth.invitation_ttl', 'Default invitation validity', 'text', 'e.g. 72h'],
    ['auth.email_change_ttl', 'Email verification link validity', 'text', 'e.g. 24h'],
    ['auth.password_reset_ttl', 'Password reset link validity', 'text', 'e.g. 1h. Email password reset is only offered to regular accounts with a verified address, never to administrators.'],
    ['auth.min_password_length', 'Minimum password length', 'number'],
    ['auth.cookie_secure', 'Secure cookie flag', 'select:auto|always|never', '"auto" marks the cookie Secure whenever the site is served over HTTPS.'],
  ]],
  ['Email (SMTP)', 'The SMTP password is never stored here: set SM_SMTP_PASSWORD or smtp.password_file in the configuration file.', [
    ['smtp.host', 'SMTP host', 'text'],
    ['smtp.port', 'SMTP port', 'number'],
    ['smtp.username', 'SMTP username', 'text'],
    ['smtp.from', 'Sender address', 'text'],
    ['smtp.tls', 'Transport security', 'select:starttls|tls|none', 'STARTTLS (587), implicit TLS (465), or none for a trusted local relay.'],
  ]],
  ['Rate limits', 'Requests allowed per window, per user (or per address for sign-in).', [
    ['rate_limits.login', 'Sign-in attempts', 'rate'],
    ['rate_limits.refresh', 'Token refreshes', 'rate'],
    ['rate_limits.api', 'API requests', 'rate'],
    ['rate_limits.server_start', 'Server starts (per user and server)', 'rate'],
    ['rate_limits.server_stop', 'Server stops (per user and server)', 'rate'],
    ['rate_limits.console_command', 'Console commands (per user and server)', 'rate'],
    ['rate_limits.backup_create', 'Backup requests (per user and server)', 'rate'],
    ['rate_limits.action_run', 'Custom action runs (per user and server)', 'rate'],
    ['rate_limits.password_reset', 'Password reset and verification emails (per address and per email)', 'rate'],
    ['rate_limits.server_cooldown', 'Cooldown between operations on one server', 'text', 'e.g. 3s; 0s disables it.'],
  ]],
  ['Monitoring', null, [
    ['monitoring.host_metrics_interval', 'Host metrics interval', 'text', 'e.g. 5s'],
    ['monitoring.status_poll_interval', 'Server status check interval', 'text', 'e.g. 5s'],
    ['monitoring.host_metrics_for_users', 'Show host CPU and memory to all signed-in users', 'bool'],
  ]],
  ['Notifications', 'Optional emails about server activity. Users opt in individually in their profile; only verified addresses receive them. Concurrency limits are managed on the Limits page.', [
    ['notifications.enabled', 'Allow server activity notifications', 'bool'],
    ['notifications.cooldown', 'Cooldown per server and event type', 'text', 'e.g. 10m. Repeated events of the same kind (a server that keeps crashing) are not mailed again within this time; 0s disables the cooldown.'],
  ]],
  ['Console', 'Buffer sizes apply to servers when they are next (re)loaded.', [
    ['console.buffer_lines', 'Lines kept in memory per server', 'number'],
    ['console.snapshot_lines', 'Lines shown when a console is opened', 'number'],
    ['console.max_line_bytes', 'Maximum line length (bytes)', 'number'],
    ['console.max_command_length', 'Maximum command length (bytes)', 'number'],
    ['console.max_log_file_bytes', 'Maximum capture file size (bytes)', 'number'],
  ]],
  ['Backups, audit and security', null, [
    ['backups.max_download_bytes', 'Maximum download size (bytes, 0 = unlimited)', 'number'],
    ['backups.max_concurrent_downloads', 'Concurrent downloads', 'number'],
    ['audit.retention_days', 'Audit log retention (days, 0 = keep forever)', 'number'],
    ['security.max_request_bytes', 'Maximum request body size (bytes)', 'number'],
    ['logging.level', 'Application log level', 'select:debug|info|warn|error'],
    ['security.config_permission_full_access', 'Let non-administrators with the CONFIG permission edit execution settings', 'bool',
      'Dangerous: this lets those users change commands and therefore run arbitrary programs as the service user. Leave off unless you fully trust them.'],
  ]],
];

export function settingsPage() {
  const box = h('div', null, loading());

  function build(payload) {
    const s = JSON.parse(JSON.stringify(payload.settings));
    const info = payload.info;
    const errors = h('div', { class: 'alert alert-error', hidden: true, role: 'alert' });

    const control = ([path, label, type, hint]) => {
      const cur = getPath(s, path);
      if (type === 'bool') {
        const input = h('input', { type: 'checkbox', checked: !!cur });
        input.addEventListener('change', () => setPath(s, path, input.checked));
        return field(label, input, hint, { inline: true });
      }
      if (type === 'number') {
        const input = h('input', { type: 'number', value: cur ?? '' });
        input.addEventListener('input', () => setPath(s, path, Number(input.value)));
        return field(label, input, hint);
      }
      if (type === 'list') {
        const input = h('textarea', { rows: 2, class: 'code', value: (cur || []).join('\n'), spellcheck: 'false' });
        input.addEventListener('input', () => setPath(s, path, input.value.split('\n').map((l) => l.trim()).filter(Boolean)));
        return field(label, input, hint);
      }
      if (type === 'rate') {
        const req = h('input', { type: 'number', min: 1, value: cur.requests, 'aria-label': label + ' requests' });
        const win = h('input', { type: 'text', value: cur.window, 'aria-label': label + ' window' });
        req.addEventListener('input', () => setPath(s, path + '.requests', Number(req.value)));
        win.addEventListener('input', () => setPath(s, path + '.window', win.value));
        return h('div', { class: 'field' }, h('label', null, label), h('div', { class: 'rate-row' }, req, h('span', null, 'per'), win));
      }
      if (type.startsWith('select:')) {
        const input = h('select', null, type.slice(7).split('|').map((o) => h('option', { value: o, selected: o === cur }, o)));
        input.addEventListener('change', () => setPath(s, path, input.value));
        return field(label, input, hint);
      }
      const input = h('input', { type: 'text', value: cur ?? '', spellcheck: 'false' });
      input.addEventListener('input', () => setPath(s, path, input.value));
      return field(label, input, hint);
    };

    const save = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Save settings');
    const form = h('form', { class: 'form', novalidate: true },
      errors,
      SECTIONS.map(([title, intro, fields]) => h('fieldset', { class: 'section' },
        h('legend', null, title), intro ? h('p', { class: 'hint' }, intro) : null,
        h('div', { class: 'settings-grid' }, fields.map(control)))),
      h('div', { class: 'form-actions' }, save));
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      errors.hidden = true;
      withBusy(save, async () => {
        try {
          const res = await api.put('/admin/settings', s);
          notify.ok('Settings saved. They are in effect now.');
          build(res);
          store.loadMe().catch(() => {});
        } catch (err) {
          const items = Array.isArray(err.details) ? err.details : [];
          replace(errors, h('strong', null, errorMessage(err)), items.length ? h('ul', null, items.map((p) => h('li', null, String(p)))) : null);
          errors.hidden = false;
          errors.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
        }
      });
    });

    // --- tools ---
    const testTo = h('input', { type: 'email', placeholder: 'you@example.com', value: store.state.user.email || '' });
    const invalid = Object.entries(info.invalid_server_files || {});
    const tools = h('fieldset', { class: 'section' }, h('legend', null, 'Maintenance'),
      h('div', { class: 'settings-grid' },
        h('div', { class: 'field' }, h('label', null, 'Send a test email'),
          h('div', { class: 'copy-row' }, testTo, actionButton('Send', '', async () => {
            const res = await api.post('/admin/settings/test-email', { to: testTo.value.trim() });
            if (res.sent) notify.ok('Test email sent.');
            else notify.info('Development mode: the message was written to the application log.');
          })),
          h('div', { class: 'hint' }, info.smtp_configured ? (info.smtp_password_set ? 'SMTP is configured with a password.' : 'SMTP is configured without a password.') : 'SMTP is not configured.')),
        h('div', { class: 'field' }, h('label', null, 'Server definition files'),
          h('div', null, actionButton('Reload from disk', '', async () => {
            const res = await api.post('/admin/servers/reload');
            notify.ok(`Reloaded: ${res.added.length} added, ${res.updated.length} refreshed, ${res.removed.length} removed.`);
            for (const [id, why] of Object.entries(res.skipped || {})) notify.warn(`${id}: ${why}`);
            await store.loadServers();
            load();
          })),
          h('div', { class: 'hint' }, 'Use after editing the JSON files by hand.'))),
      invalid.length ? h('div', { class: 'alert alert-warn' }, h('strong', null, 'Definition files that could not be loaded:'),
        h('ul', null, invalid.map(([file, why]) => h('li', null, h('code', null, file), ': ' + why)))) : null);

    const readOnly = h('fieldset', { class: 'section' }, h('legend', null, 'Installation (read-only)'),
      h('p', { class: 'hint' }, 'These values come from the configuration file and take effect after a restart.'),
      h('dl', { class: 'kv' },
        ...[['Version', info.version], ['Mode', info.mode], ['Listen address', info.listen], ['Configuration file', info.config_file],
          ['Data directory', info.paths.data_dir], ['Database', info.paths.database], ['Server definitions', info.paths.servers_dir],
          ['Runtime directory', info.paths.runtime_dir], ['Docker socket', info.tools.docker_socket]]
          .flatMap(([k, v]) => [h('dt', null, k), h('dd', { class: 'break' }, h('code', null, v || '–'))])));

    replace(box, info.mode === 'development' ? h('div', { class: 'alert alert-warn' }, 'Development mode is active: outgoing email is printed to the application log. Do not use this mode in production.') : null,
      form, tools, readOnly);
  }

  async function load() {
    try {
      build(await api.get('/admin/settings'));
    } catch (err) {
      replace(box, errorBox(err, load));
    }
  }
  load();
  const el = h('div', { class: 'page page-narrow' },
    h('div', { class: 'page-head' }, h('h1', null, 'Settings')),
    h('p', { class: 'muted' }, 'Changes are written to the configuration file and applied immediately.'),
    box);
  return { el };
}
