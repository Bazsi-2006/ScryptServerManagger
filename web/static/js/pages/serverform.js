// Server definition editor, used for creating servers and for the
// configuration tab. It offers a structured form for every field and a raw
// JSON view of the same document.

import * as api from '../api.js';
import * as store from '../store.js';
import { h, replace, field, withBusy, notify, errorMessage, openModal, chip, confirmDialog } from '../ui.js';

const clone = (v) => JSON.parse(JSON.stringify(v));

export function getPath(obj, path) {
  let cur = obj;
  for (const key of path.split('.')) {
    if (cur === null || cur === undefined) return undefined;
    cur = cur[key];
  }
  return cur;
}

export function setPath(obj, path, value) {
  const keys = path.split('.');
  let cur = obj;
  for (let i = 0; i < keys.length - 1; i++) {
    if (cur[keys[i]] === null || typeof cur[keys[i]] !== 'object') cur[keys[i]] = {};
    cur = cur[keys[i]];
  }
  const last = keys[keys.length - 1];
  if (value === undefined) delete cur[last];
  else cur[last] = value;
}

export function blankDefinition() {
  return {
    id: '', name: '', description: '', category: '', backend: 'custom', working_dir: '',
    custom: { mode: 'foreground', start: { command: '', args: [] } },
    readiness: { method: 'none', interval: '2s' },
    console: {},
    timeouts: { start: '2m0s', stop: '1m0s', backup: '1h0m0s', command: '15s' },
  };
}

// Environment variables are no longer edited in the form (set them in your
// start scripts or service configuration). Existing values in a definition
// are kept untouched, and the JSON view still shows them.

/** Category choices: managed categories plus the current unmanaged value. */
function categorySelect(d, disabled, onChange) {
  const input = h('select', { disabled });
  const fill = (cats, unmanaged) => {
    const cur = d.category || '';
    const opts = [h('option', { value: '' }, 'No category')];
    let found = cur === '';
    for (const c of cats) {
      const match = c.id === cur || c.name.toLowerCase() === cur.toLowerCase();
      if (match) found = true;
      opts.push(h('option', { value: c.id, selected: match }, c.limit ? `${c.name} (max ${c.limit} running)` : c.name));
    }
    for (const v of unmanaged) {
      if (v === cur || cats.some((c) => c.name.toLowerCase() === v.toLowerCase())) continue;
      opts.push(h('option', { value: v }, `${v} (not managed)`));
    }
    if (!found) opts.push(h('option', { value: cur, selected: true }, `${cur} (not managed)`));
    replace(input, opts);
    input.value = found ? (cats.find((c) => c.id === cur || c.name.toLowerCase() === cur.toLowerCase())?.id ?? cur) : cur;
    if (input.value !== cur && !disabled) onChange(input.value);
  };
  fill([], []);
  if (store.isAdmin()) {
    api.get('/admin/categories').then((res) => fill(res.capacity.categories, res.unmanaged.map((u) => u.value))).catch(() => {});
  }
  input.addEventListener('change', () => onChange(input.value));
  return input;
}

/** Modal editor for one custom action. */
function actionDialog({ action, backend, existingIds, onSave }) {
  const isNew = !action;
  const a = action ? clone(action) : { id: '', name: '', method: backend === 'tmux' ? 'tmux' : 'command', command: { command: '', args: [] }, tmux: { text: '', enter: true }, confirm: false };
  a.command = a.command || { command: '', args: [] };
  a.tmux = a.tmux || { text: '', enter: true };
  const id = h('input', { type: 'text', value: a.id, placeholder: 'save_world', spellcheck: 'false', autocapitalize: 'none', disabled: !isNew });
  const name = h('input', { type: 'text', value: a.name, placeholder: 'Save World', maxlength: 100 });
  const icon = h('input', { type: 'text', value: a.icon || '', placeholder: '💾', maxlength: 8 });
  const desc = h('textarea', { rows: 2, value: a.description || '', maxlength: 500 });
  const methods = [['command', 'Run a script or program']];
  if (backend === 'tmux') methods.push(['tmux', 'Send a command to the tmux session']);
  const method = h('select', null, methods.map(([v, l]) => h('option', { value: v, selected: v === a.method }, l)));
  const cmd = h('input', { type: 'text', value: a.command.command, placeholder: '/opt/servers/example/update.sh', spellcheck: 'false' });
  const args = h('textarea', { rows: 3, class: 'code', spellcheck: 'false', value: (a.command.args || []).join('\n') });
  const wd = h('input', { type: 'text', value: a.working_dir || '', placeholder: 'Server working directory', spellcheck: 'false' });
  const timeout = h('input', { type: 'text', value: a.timeout || '10m0s', spellcheck: 'false' });
  const text = h('input', { type: 'text', value: a.tmux.text, placeholder: 'save-all flush', spellcheck: 'false', class: 'code' });
  const enter = h('input', { type: 'checkbox', checked: a.tmux.enter !== false });
  const confirm = h('input', { type: 'checkbox', checked: !!a.confirm });
  const enabled = h('input', { type: 'checkbox', checked: !a.disabled });
  const cmdPart = h('div', null,
    field('Executable', cmd, 'Absolute path, or a program name found in PATH. Run directly, never through a shell.'),
    field('Arguments', args, 'One argument per line, passed exactly as written.'),
    h('div', { class: 'field-row' }, field('Working directory (optional)', wd, 'Absolute path; defaults to the server\'s working directory.'), field('Timeout', timeout, 'The command is killed after this, e.g. 30s or 10m.')));
  const tmuxPart = h('div', null,
    field('Command text', text, 'Typed literally into the server\'s tmux session; no shell is involved.'),
    field('Press Enter after the text', enter, null, { inline: true }));
  const errors = h('div', { class: 'alert alert-error', hidden: true });
  const sync = () => { cmdPart.hidden = method.value !== 'command'; tmuxPart.hidden = method.value !== 'tmux'; };
  method.addEventListener('change', sync);
  sync();
  const save = h('button', { class: 'btn btn-primary', type: 'button' }, isNew ? 'Add action' : 'Apply');
  const m = openModal({
    title: isNew ? 'New custom action' : `Edit ${a.name}`, wide: true,
    body: h('div', { class: 'form' }, errors,
      h('div', { class: 'field-row' },
        field('Action ID', id, 'Permanent: lowercase letters, digits, "_" and "-".'),
        field('Name', name),
        field('Icon (optional)', icon, 'A short symbol, e.g. an emoji.')),
      field('Description (optional)', desc, 'Shown to users on the action button.'),
      field('Execution method', method),
      cmdPart, tmuxPart,
      field('Ask for confirmation before running', confirm, null, { inline: true }),
      field('Enabled', enabled, 'Disabled actions are hidden from users and cannot run.', { inline: true })),
    footer: [h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), save],
  });
  save.addEventListener('click', () => {
    const out = { id: id.value.trim(), name: name.value.trim(), method: method.value, confirm: confirm.checked };
    if (icon.value.trim()) out.icon = icon.value.trim();
    if (desc.value.trim()) out.description = desc.value.trim();
    if (!enabled.checked) out.disabled = true;
    if (out.method === 'command') {
      out.command = { command: cmd.value.trim(), args: args.value.split('\n').filter((l) => l !== '') };
      if (wd.value.trim()) out.working_dir = wd.value.trim();
      if (timeout.value.trim()) out.timeout = timeout.value.trim();
    } else {
      out.tmux = { text: text.value, enter: enter.checked };
    }
    const problems = [];
    if (!/^[a-z0-9][a-z0-9_-]{0,62}$/.test(out.id)) problems.push('The action ID must be lowercase letters, digits, "_" or "-".');
    else if (isNew && existingIds.includes(out.id)) problems.push('Another action already uses this ID.');
    if (!out.name) problems.push('A name is required.');
    if (out.method === 'command' && !out.command.command) problems.push('The executable is required.');
    if (out.method === 'tmux' && !out.tmux.text.trim()) problems.push('The command text is required.');
    if (problems.length) {
      replace(errors, h('ul', null, problems.map((p) => h('li', null, p))));
      errors.hidden = false;
      return;
    }
    onSave(out);
    m.close();
  });
}

/**
 * serverForm({def, scope, isNew, onSave}) -> {el}
 * scope "safe" disables everything that affects execution (the server
 * rejects such changes from non-administrators regardless).
 */
export function serverForm({ def, scope, isNew, onSave, onCancel }) {
  let d = clone(def);
  let jsonMode = false;
  const safeOnly = scope !== 'full';
  const body = h('div', { class: 'form-sections' });
  const errors = h('div', { class: 'alert alert-error', hidden: true, role: 'alert' });
  const jsonArea = h('textarea', { class: 'code json-editor', spellcheck: 'false', rows: 28 });

  // --- input factories bound to the working copy ---
  const ctl = (path, label, hint, input, opts = {}) => {
    if (safeOnly && !opts.safe) input.disabled = true;
    return field(label, input, hint, { name: path, inline: opts.inline });
  };
  const text = (path, label, hint, opts = {}) => {
    const input = h('input', { type: 'text', value: getPath(d, path) ?? '', placeholder: opts.placeholder || '', spellcheck: 'false', autocapitalize: 'none', autocomplete: 'off' });
    input.addEventListener('input', () => setPath(d, path, input.value === '' && opts.optional ? undefined : input.value));
    return ctl(path, label, hint, input, opts);
  };
  const area = (path, label, hint, opts = {}) => {
    const input = h('textarea', { rows: opts.rows || 2, value: getPath(d, path) ?? '' });
    input.addEventListener('input', () => setPath(d, path, input.value));
    return ctl(path, label, hint, input, opts);
  };
  const number = (path, label, hint, opts = {}) => {
    const input = h('input', { type: 'number', value: getPath(d, path) ?? '', min: opts.min, max: opts.max });
    input.addEventListener('input', () => setPath(d, path, input.value === '' ? undefined : Number(input.value)));
    return ctl(path, label, hint, input, opts);
  };
  const check = (path, label, hint, opts = {}) => {
    const cur = getPath(d, path);
    const input = h('input', { type: 'checkbox', checked: cur === undefined || cur === null ? !!opts.def : !!cur });
    input.addEventListener('change', () => setPath(d, path, input.checked));
    return ctl(path, label, hint, input, { ...opts, inline: true });
  };
  const select = (path, label, options, hint, opts = {}) => {
    const cur = getPath(d, path) ?? opts.def ?? '';
    const input = h('select', null, options.map(([v, l]) => h('option', { value: v, selected: v === cur }, l)));
    input.addEventListener('change', () => {
      setPath(d, path, input.value === '' && opts.optional ? undefined : input.value);
      if (opts.rerender) render();
    });
    return ctl(path, label, hint, input, opts);
  };
  // One argument per line keeps spaces inside an argument unambiguous.
  const lines = (path, label, hint, opts = {}) => {
    const input = h('textarea', { rows: opts.rows || 3, class: 'code', spellcheck: 'false', value: (getPath(d, path) || []).join('\n'), placeholder: opts.placeholder || '' });
    input.addEventListener('input', () => {
      const arr = input.value.split('\n').filter((l) => l !== '');
      setPath(d, path, arr.length ? arr : undefined);
    });
    return ctl(path, label, hint, input, opts);
  };
  const command = (path, label, hint) => h('div', { class: 'field-row' },
    text(path + '.command', label, hint, { placeholder: '/opt/servers/example/script.sh' }),
    lines(path + '.args', 'Arguments', 'One argument per line. Passed directly to the program, not through a shell.'));
  const section = (title, intro, ...children) => h('fieldset', { class: 'section' },
    h('legend', null, title), intro ? h('p', { class: 'hint' }, intro) : null, ...children);
  const row = (...children) => h('div', { class: 'field-row' }, ...children);

  function ensure(path, def) {
    if (getPath(d, path) === undefined || getPath(d, path) === null) setPath(d, path, def);
  }

  function backendSection() {
    switch (d.backend) {
      case 'custom':
        ensure('custom', { mode: 'foreground', start: { command: '', args: [] } });
        ensure('custom.start', { command: '' });
        return section('Custom commands', 'Runs the programs you configure here. Each command is executed directly with its argument list.',
          select('custom.mode', 'Process model', [
            ['foreground', 'Foreground – the start command is the server process'],
            ['forking', 'Forking – the start command launches the server and exits'],
          ], 'Foreground servers are tracked directly. Forking servers need a PID file or a port/script status check so the manager can tell whether they are running.', { rerender: true }),
          command('custom.start', 'Start command', 'Absolute path, or a program name found in PATH.'),
          command('custom.stop', 'Stop command (optional)', d.custom.mode === 'forking' ? 'Required for forking servers without a PID file.' : 'If empty, the stop input or signal below is used.'),
          row(
            d.custom.mode === 'foreground' ? text('custom.stop_input', 'Stop input (optional)', 'Text written to the server console to shut it down, e.g. "stop". Needs console input "stdin" or "pty".') : null,
            select('custom.stop_signal', 'Stop signal', ['SIGTERM', 'SIGINT', 'SIGQUIT', 'SIGHUP', 'SIGUSR1', 'SIGUSR2', 'SIGKILL'].map((s) => [s, s]), 'Sent when no stop command or stop input is configured.', { def: 'SIGTERM' })),
          row(
            text('custom.pid_file', 'PID file (optional)', 'For forking servers: file containing the server process ID.'),
            text('custom.log_file', 'Log file to follow (optional)', 'An additional file whose new lines are shown in the console.')),
          row(
            check('custom.kill_on_timeout', 'Kill the server if a graceful stop times out', null, { def: true }),
            select('custom.on_manager_exit', 'When Server Manager itself stops', [['keep', 'Keep the server running (re-adopted on next start)'], ['stop', 'Stop the server']], 'Servers attached to a PTY are always stopped.', { def: 'keep' })));
      case 'tmux':
        ensure('tmux', { session: '' });
        return section('tmux session', 'Manages a server inside a tmux session owned by the service user. Output is streamed live and commands are typed into the pane.',
          row(
            text('tmux.session', 'Session name', 'Letters, digits, "_" and "-".'),
            text('tmux.window', 'Window (optional)', 'Index or name; empty = active window.', { optional: true }),
            text('tmux.pane', 'Pane (optional)', 'Index; empty = active pane.', { optional: true })),
          command('tmux.start', 'Start command (optional)', 'Run in a new detached session. Leave empty for attach-only sessions that are started elsewhere.'),
          row(
            text('tmux.stop_text', 'Stop text (optional)', 'Typed into the pane followed by Enter, e.g. "stop".', { optional: true }),
            lines('tmux.stop_keys', 'Stop keys (optional)', 'tmux key names, one per line, e.g. C-c.', { rows: 2 })),
          row(
            check('tmux.kill_on_timeout', 'Kill the session if the graceful stop times out (or none is configured)', null, { def: true }),
            number('tmux.width', 'Width', null, { min: 20, max: 1000 }),
            number('tmux.height', 'Height', null, { min: 5, max: 500 })),
          row(
            text('tmux.socket_name', 'Socket name (optional)', 'tmux -L', { optional: true }),
            text('tmux.socket_path', 'Socket path (optional)', 'tmux -S', { optional: true })));
      case 'systemd':
        ensure('systemd', { unit: '', scope: 'system' });
        return section('systemd service', 'Controls an existing unit. State comes from systemd; output comes from the journal. The service user needs permission to start and stop this unit (polkit or sudoers).',
          row(
            text('systemd.unit', 'Unit name', 'e.g. minecraft.service'),
            select('systemd.scope', 'Scope', [['system', 'System service'], ['user', 'User service (systemctl --user)']], null, { def: 'system' })),
          row(
            check('systemd.use_sudo', 'Run systemctl through sudo -n', 'Requires a sudoers rule limited to this unit.'),
            check('systemd.allow_restart', 'Allow restart', null, { def: true })));
      case 'docker':
        ensure('docker', { container: '' });
        return section('Docker container', 'Controls an existing container through the Docker socket. Access to that socket is equivalent to root on the host.',
          row(
            text('docker.container', 'Container name or ID'),
            check('docker.allow_restart', 'Allow restart', null, { def: true })));
      default:
        return null;
    }
  }

  function readinessSection() {
    ensure('readiness', { method: 'none' });
    const m = d.readiness.method || 'none';
    let detail = null;
    if (m === 'console') {
      ensure('readiness.console', { pattern: '', regex: false });
      detail = row(
        text('readiness.console.pattern', 'Text that signals readiness', 'Only output produced by the current start attempt is considered. Example: Done (', { safe: true }),
        check('readiness.console.regex', 'Treat as regular expression', null, { safe: true }));
    } else if (m === 'port') {
      ensure('readiness.port', { host: '127.0.0.1', port: 25565, protocol: 'tcp', timeout: '2s' });
      detail = [row(
        text('readiness.port.host', 'Host or IP'),
        number('readiness.port.port', 'Port', null, { min: 1, max: 65535 }),
        select('readiness.port.protocol', 'Protocol', [['tcp', 'TCP'], ['udp', 'UDP']], null, { rerender: true, def: 'tcp' }),
        text('readiness.port.timeout', 'Probe timeout', 'e.g. 2s')),
      d.readiness.port.protocol === 'udp' ? text('readiness.port.udp_payload_hex', 'UDP probe payload (hex)', 'A datagram the server replies to. An open UDP port alone proves nothing, so a reply is required.') : null];
    } else if (m === 'script') {
      ensure('readiness.script', { command: '', timeout: '10s' });
      detail = [h('div', { class: 'field-row' },
        text('readiness.script.command', 'Status script', 'Exit 0 = ready, 1 = not ready, anything else = error/unknown.'),
        lines('readiness.script.args', 'Arguments', 'One per line.')),
      text('readiness.script.timeout', 'Script timeout', 'e.g. 10s')];
    }
    return section('Readiness detection', 'How the manager decides that a starting server is ready (STARTING → RUNNING).',
      row(
        select('readiness.method', 'Method', [
          ['none', 'None – ready as soon as the backend reports it running'],
          ['console', 'Console output – wait for a line of text'],
          ['port', 'Port – wait until a TCP/UDP port answers'],
          ['script', 'Script – run a status command'],
        ], null, { rerender: true }),
        text('readiness.interval', 'Check interval', 'e.g. 2s', { safe: true })),
      detail);
  }

  function consoleSection() {
    ensure('console', {});
    const options = [['', d.backend === 'tmux' ? 'Default (type into the tmux pane)' : 'Default (no input)'], ['none', 'Disabled']];
    if (d.backend === 'custom' && d.custom && d.custom.mode === 'foreground') options.push(['stdin', 'Standard input'], ['pty', 'Pseudo-terminal (PTY)']);
    if (d.backend === 'tmux') options.push(['tmux', 'tmux pane']);
    if (d.backend === 'docker') options.push(['docker_exec', 'docker exec helper inside the container']);
    options.push(['command', 'Helper command on the host']);
    const input = d.console.input || '';
    const needsCmd = input === 'command' || input === 'docker_exec';
    if (needsCmd) ensure('console.input_command', { command: '', args: ['{command}'] });
    return section('Console input', 'How commands typed in the web console reach the server. They are delivered to the server\'s own console, never run as shell commands.',
      select('console.input', 'Input method', options, null, { rerender: true, optional: true }),
      needsCmd ? h('div', { class: 'field-row' },
        text('console.input_command.command', 'Helper program', input === 'docker_exec' ? 'Program inside the container, e.g. rcon-cli.' : 'Program on the host, e.g. an RCON client.'),
        lines('console.input_command.args', 'Arguments', 'Use {command} where the console line should be inserted (as one argument). Without it, the line is sent on standard input.')) : null);
  }

  function render() {
    if (jsonMode) {
      jsonArea.value = JSON.stringify(d, null, 2);
      jsonArea.disabled = safeOnly;
      replace(body, h('p', { class: 'hint' }, 'The complete definition as JSON. This is exactly what is stored in the server\'s definition file. Environment variables, if a definition has any, can only be edited here.'), jsonArea);
      return;
    }
    ensure('timeouts', {});
    replace(body,
      section('General', null,
        row(
          isNew ? text('id', 'Server ID', 'Permanent identifier: lowercase letters, digits, "_" and "-".', { placeholder: 'minecraft_survival' })
            : field('Server ID', h('input', { type: 'text', value: d.id, disabled: true })),
          text('name', 'Display name', null, { safe: true }),
          field('Category', categorySelect(d, !store.isAdmin(), (v) => { d.category = v; }),
            store.isAdmin() ? 'Groups servers and decides which concurrency limit applies. Manage categories under Limits.' : 'Set by an administrator.', { name: 'category' })),
        area('description', 'Description (optional)', null, { safe: true }),
        row(
          select('backend', 'Execution backend', [['custom', 'Custom scripts / commands'], ['tmux', 'tmux session'], ['systemd', 'systemd service'], ['docker', 'Docker container']], null, { rerender: true }),
          text('working_dir', 'Working directory (optional)', 'Absolute path.'))),
      backendSection(),
      readinessSection(),
      consoleSection(),
      backupSection(),
      actionsSection(),
      section('Timeouts and limits', 'Durations such as 30s, 2m or 1h.',
        row(
          text('timeouts.start', 'Start timeout', null, { safe: true }),
          text('timeouts.stop', 'Stop timeout', null, { safe: true }),
          text('timeouts.backup', 'Backup timeout', null, { safe: true }),
          text('timeouts.command', 'Console command timeout', null, { safe: true })),
        row(
          text('cooldown', 'Cooldown between operations (optional)', 'Overrides the global setting for this server.', { safe: true, optional: true }),
          text('status_poll_interval', 'Status check interval (optional)', 'Overrides the global setting.', { safe: true, optional: true }))));
  }

  function backupSection() {
    const method = getPath(d, 'backup.method') || 'command';
    const tmux = d.backend === 'tmux';
    const methods = [['command', 'Run a backup script or program']];
    if (tmux || method === 'tmux') methods.push(['tmux', 'Send command to tmux session']);
    const parts = [
      row(select('backup.method', 'Backup method', methods, tmux ? 'Game servers that back up from their own console can be sent a command instead of running a script.' : null, { def: 'command', rerender: true }),
        check('backup.allow_user_delete', 'Users with BACKUP_DOWNLOAD may delete backup files')),
    ];
    if (method === 'tmux') {
      ensure('backup.tmux', { text: '', enter: true });
      parts.push(
        row(text('backup.directory', 'Backup directory (optional)', 'Where the server writes its backups. Files here can be listed and downloaded, and new ones are picked up automatically.'),
          text('backup.tmux.text', 'Command sent to the console', 'Typed literally into the tmux session, e.g. /backup start.')),
        row(check('backup.tmux.enter', 'Press Enter after the command', null, { def: true }),
          text('backup.completion.pattern', 'Completion text (optional)', 'If the server prints a line when the backup is done, the operation waits for it. Without it, the backup is reported as "submitted": the command was sent, but completion cannot be verified.', { optional: true }),
          check('backup.completion.regex', 'Completion text is a regular expression')));
    } else {
      parts.push(
        text('backup.directory', 'Backup directory', 'Absolute path. Downloads are restricted to this directory.'),
        command('backup.command', 'Backup command (optional)', 'Receives SM_SERVER_ID and SM_BACKUP_DIR in its environment.'));
    }
    return section('Backups', 'Optional. Backups run on request; files in the backup directory can be listed and downloaded.', ...parts);
  }

  function actionsSection() {
    // Custom actions are managed by administrators only; the server never
    // shows them to anyone else.
    if (safeOnly || !store.isAdmin()) return null;
    const list = d.actions || [];
    const edit = (index) => actionDialog({
      action: index === undefined ? null : list[index], backend: d.backend, existingIds: list.map((a) => a.id),
      onSave: (a) => {
        const next = [...list];
        if (index === undefined) next.push(a); else next[index] = a;
        d.actions = next;
        render();
      },
    });
    const rows = list.map((a, i) => h('div', { class: 'action-row' },
      h('div', { class: 'action-row-main' },
        h('strong', null, (a.icon ? a.icon + ' ' : '') + a.name), h('span', { class: 'muted small' }, ` ${a.id}`),
        chip(a.method === 'tmux' ? 'tmux command' : 'script', a.method === 'tmux' ? 'chip-backend' : ''),
        a.confirm ? chip('confirmation', '') : null, a.disabled ? chip('disabled', 'chip-warn') : null,
        h('div', { class: 'muted small break' }, a.method === 'tmux' ? `Sends: ${a.tmux?.text || ''}${a.tmux?.enter === false ? '' : ' ⏎'}` : `Runs: ${[a.command?.command, ...(a.command?.args || [])].join(' ')}`)),
      h('div', { class: 'action-row-buttons' },
        h('button', { class: 'btn btn-small', type: 'button', onclick: () => edit(i) }, 'Edit'),
        h('button', { class: 'btn btn-small btn-danger', type: 'button', onclick: async () => {
          const ok = await confirmDialog({ title: `Remove "${a.name}"?`, message: 'The action and every permission granted for it are removed when you save the server settings.', confirmLabel: 'Remove', danger: true });
          if (!ok) return;
          d.actions = list.filter((_, j) => j !== i);
          render();
        } }, 'Remove'))));
    return section('Custom actions', 'Buttons that run a predefined script or send a predefined command to the server console. Users can only run the actions you grant them in the Access tab, and never change what they execute. Changes are saved with the settings below.',
      rows.length ? rows : h('p', { class: 'muted' }, 'No custom actions yet.'),
      h('div', null, h('button', { class: 'btn btn-small', type: 'button', onclick: () => edit(undefined) }, '+ Add action')));
  }

  function collect() {
    if (jsonMode) {
      try { d = JSON.parse(jsonArea.value); } catch (err) { throw new Error('The JSON is not valid: ' + err.message); }
      return d;
    }
    return d;
  }

  const modeBtn = h('button', { type: 'button', class: 'btn btn-small btn-ghost' }, 'Edit as JSON');
  modeBtn.addEventListener('click', () => {
    try { collect(); } catch (err) { notify.error(errorMessage(err)); return; }
    jsonMode = !jsonMode;
    modeBtn.textContent = jsonMode ? 'Edit as form' : 'Edit as JSON';
    render();
  });

  const saveBtn = h('button', { type: 'submit', class: 'btn btn-primary' }, isNew ? 'Create server' : 'Save changes');
  const form = h('form', { class: 'form server-form', novalidate: true },
    safeOnly ? h('div', { class: 'alert alert-info' }, 'You can edit the name, description, timeouts and console readiness text. Execution settings, the category and custom actions can only be changed by an administrator.') : null,
    h('div', { class: 'form-toolbar' }, modeBtn),
    errors, body,
    h('div', { class: 'form-actions' }, saveBtn, onCancel ? h('button', { type: 'button', class: 'btn', onclick: onCancel }, 'Cancel') : null));

  form.addEventListener('submit', (e) => {
    e.preventDefault();
    errors.hidden = true;
    body.querySelectorAll('.field.has-error').forEach((f) => f.classList.remove('has-error'));
    withBusy(saveBtn, async () => {
      try {
        const saved = await onSave(clone(collect()));
        if (saved) { d = clone(saved); render(); }
      } catch (err) {
        const items = Array.isArray(err.details) ? err.details : [];
        replace(errors, h('strong', null, errorMessage(err)),
          items.length ? h('ul', null, items.map((p) => h('li', null, typeof p === 'string' ? p : [h('code', null, p.field), ': ' + p.message]))) : null);
        errors.hidden = false;
        for (const p of items) {
          if (!p.field) continue;
          const f = body.querySelector(`[data-field="${CSS.escape(p.field)}"]`) || body.querySelector(`[data-field="${CSS.escape(p.field.split('[')[0])}"]`);
          if (f) f.classList.add('has-error');
        }
        errors.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
      }
    });
  });

  render();
  return { el: form };
}

export function newServerPage(ctx) {
  const formHost = h('div');
  const form = serverForm({
    def: blankDefinition(), scope: 'full', isNew: true,
    onCancel: () => ctx.navigate('/'),
    onSave: async (def) => {
      const res = await api.post('/servers', def);
      store.state.servers.set(res.server.id, res.server);
      store.emit('servers');
      notify.ok(`Server "${res.server.name}" created.`);
      ctx.navigate('/servers/' + res.server.id);
      return null;
    },
  });
  formHost.append(form.el);
  const el = h('div', { class: 'page page-narrow' },
    h('div', { class: 'page-head' }, h('div', null, h('a', { class: 'back', href: '#/' }, '← Servers'), h('h1', null, 'Add a server'))),
    h('p', { class: 'muted' }, 'A server definition tells the manager how to start, stop and observe something that is already installed on this machine. Nothing is installed or deleted for you.'),
    formHost);
  return { el };
}
