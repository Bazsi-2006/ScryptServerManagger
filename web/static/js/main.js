// Application shell: session bootstrap, hash router and page layout.

import * as api from './api.js';
import * as store from './store.js';
import { h, replace, notify, loading, errorMessage } from './ui.js';
import { loginPage, registerPage, verifyEmailPage } from './pages/auth.js';
import { dashboardPage } from './pages/dashboard.js';
import { serverPage } from './pages/server.js';
import { newServerPage } from './pages/serverform.js';
import { usersPage } from './pages/users.js';
import { invitationsPage } from './pages/invitations.js';
import { auditPage } from './pages/audit.js';
import { settingsPage } from './pages/settings.js';
import { profilePage } from './pages/profile.js';
import { systemPage } from './pages/system.js';

const app = document.getElementById('app');

// ---- theme ----

function applyTheme(theme) {
  if (theme === 'light' || theme === 'dark') document.documentElement.dataset.theme = theme;
  else delete document.documentElement.dataset.theme;
}

function currentTheme() {
  const explicit = document.documentElement.dataset.theme;
  if (explicit) return explicit;
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function toggleTheme() {
  const next = currentTheme() === 'dark' ? 'light' : 'dark';
  applyTheme(next);
  // Only the theme preference is stored locally; never credentials.
  try { localStorage.setItem('sm-theme', next); } catch { /* private mode */ }
}

try { applyTheme(localStorage.getItem('sm-theme')); } catch { /* ignore */ }

// ---- routing ----

const routes = [
  { re: /^\/$/, page: dashboardPage, nav: 'dashboard' },
  { re: /^\/servers\/([a-z0-9_-]+)(?:\/([a-z]+))?$/, page: serverPage, nav: 'dashboard' },
  { re: /^\/system$/, page: systemPage, nav: 'system' },
  { re: /^\/profile$/, page: profilePage, nav: 'profile' },
  { re: /^\/admin\/servers\/new$/, page: newServerPage, nav: 'dashboard', admin: true },
  { re: /^\/admin\/users$/, page: usersPage, nav: 'users', admin: true },
  { re: /^\/admin\/invitations$/, page: invitationsPage, nav: 'invitations', admin: true },
  { re: /^\/admin\/audit$/, page: auditPage, nav: 'audit', admin: true },
  { re: /^\/admin\/settings$/, page: settingsPage, nav: 'settings', admin: true },
];

const publicRoutes = [
  { re: /^\/login$/, page: loginPage },
  { re: /^\/register$/, page: registerPage },
  { re: /^\/verify-email$/, page: verifyEmailPage },
];

function parseHash() {
  const raw = location.hash.replace(/^#/, '') || '/';
  const [path, qs] = raw.split('?');
  return { path: path || '/', params: new URLSearchParams(qs || '') };
}

export function navigate(path) {
  if (location.hash === '#' + path) render();
  else location.hash = path;
}

let current = null; // { destroy? }
let shell = null;

function destroyCurrent() {
  if (current && current.destroy) {
    try { current.destroy(); } catch (err) { console.error(err); }
  }
  current = null;
}

function mount(container, page) {
  current = page;
  replace(container, page.el);
  window.scrollTo(0, 0);
}

function render() {
  const { path, params } = parseHash();
  destroyCurrent();

  const pub = publicRoutes.find((r) => r.re.test(path));
  if (pub && !(store.state.user && path === '/login')) {
    shell = null;
    document.title = 'Server Manager';
    mount(app, pub.page({ params, navigate, enterApp }));
    return;
  }
  if (!store.state.user) {
    if (path !== '/login') sessionStorage.setItem('sm-return', location.hash);
    location.hash = '/login';
    return;
  }
  if (path === '/login') { location.hash = '/'; return; }

  let match = null;
  let route = null;
  for (const r of routes) {
    match = path.match(r.re);
    if (match) { route = r; break; }
  }
  if (!shell) shell = buildShell();
  if (shell.root.parentNode !== app) replace(app, shell.root);
  shell.setActive(route ? route.nav : '');
  shell.closeMenu();
  if (!route || (route.admin && !store.isAdmin())) {
    mount(shell.main, { el: h('div', { class: 'page' }, h('div', { class: 'empty' },
      h('h3', null, route ? 'Administrators only' : 'Page not found'),
      h('p', null, route ? 'You do not have access to this page.' : 'The page you are looking for does not exist.'),
      h('a', { class: 'btn btn-primary', href: '#/' }, 'Back to the dashboard'))) });
    return;
  }
  mount(shell.main, route.page({ params, navigate, args: match.slice(1) }));
}

// ---- layout ----

function buildShell() {
  const links = new Map();
  const link = (key, href, label) => {
    const a = h('a', { href, class: 'nav-link' }, label);
    links.set(key, a);
    return a;
  };
  const nav = h('nav', { class: 'nav', 'aria-label': 'Main' });
  const liveDot = h('span', { class: 'live-indicator', title: 'Connecting…' }, h('span', { class: 'dot' }), h('span', { class: 'live-text' }, 'Connecting'));
  const userName = h('span', { class: 'user-name' });
  const themeBtn = h('button', { class: 'icon-btn', title: 'Toggle dark mode', 'aria-label': 'Toggle dark mode', onclick: toggleTheme }, '◐');
  const menuBtn = h('button', { class: 'icon-btn menu-btn', 'aria-label': 'Menu', 'aria-expanded': 'false' }, '☰');
  const main = h('main', { class: 'main', id: 'main' });
  const header = h('header', { class: 'topbar' },
    h('a', { class: 'brand', href: '#/' }, h('img', { src: '/favicon.svg', alt: '', width: 26, height: 26 }), h('span', null, 'Server Manager')),
    menuBtn, nav,
    h('div', { class: 'topbar-right' }, liveDot, themeBtn,
      h('a', { class: 'user-link', href: '#/profile', title: 'Your profile' }, userName),
      h('button', { class: 'btn btn-small', onclick: doLogout }, 'Log out')));
  const root = h('div', { class: 'shell' }, header, main);

  menuBtn.addEventListener('click', () => {
    const open = header.classList.toggle('menu-open');
    menuBtn.setAttribute('aria-expanded', String(open));
  });

  function buildNav() {
    links.clear();
    replace(nav,
      link('dashboard', '#/', 'Servers'),
      store.state.features.host_metrics ? link('system', '#/system', 'System') : null,
      store.isAdmin() ? [
        link('users', '#/admin/users', 'Users'),
        link('invitations', '#/admin/invitations', 'Invitations'),
        link('audit', '#/admin/audit', 'Audit log'),
        link('settings', '#/admin/settings', 'Settings'),
      ] : null,
      link('profile', '#/profile', 'Profile'));
    userName.textContent = store.state.user ? (store.state.user.display_name || store.state.user.username) : '';
    setActive(active);
  }
  let active = '';
  function setActive(key) {
    active = key;
    for (const [k, a] of links) a.classList.toggle('active', k === key);
  }
  function setLive(v) {
    liveDot.classList.toggle('is-live', v);
    liveDot.title = v ? 'Live updates are connected' : 'Live updates are disconnected – reconnecting…';
    liveDot.querySelector('.live-text').textContent = v ? 'Live' : 'Offline';
  }
  store.on('me', () => {
    const wasAdmin = links.has('users');
    buildNav();
    // Losing (or gaining) administrator rights changes which pages exist.
    if (wasAdmin !== store.isAdmin()) render();
  });
  store.on('live', setLive);
  buildNav();
  setLive(store.state.live);
  return { root, main, setActive, closeMenu: () => header.classList.remove('menu-open') };
}

async function doLogout() {
  await api.logout();
  store.clearSession();
  shell = null;
  location.hash = '/login';
  render();
}

// ---- session ----

api.setHandlers({
  onSession: (payload) => store.setSession(payload),
  onAuthLost: () => {
    if (!store.state.user) return;
    store.clearSession();
    shell = null;
    notify.warn('Your session has ended. Please sign in again.');
    location.hash = '/login';
    render();
  },
});

/** Called by the auth pages after a successful login or registration. */
async function enterApp() {
  shell = null;
  store.startEvents();
  try {
    await store.loadServers();
  } catch (err) {
    notify.error(errorMessage(err));
  }
  const back = sessionStorage.getItem('sm-return');
  sessionStorage.removeItem('sm-return');
  const target = back && !/^#\/(login|register|verify-email)/.test(back) ? back.slice(1) : '/';
  if (location.hash === '#' + target) render();
  else location.hash = target;
}

async function boot() {
  replace(app, h('div', { class: 'boot' }, loading('Loading…')));
  const { path } = parseHash();
  if (api.maybeHasSession()) {
    try {
      // A valid refresh cookie restores the session without a login prompt.
      await api.refresh();
      store.startEvents();
      await store.loadServers().catch(() => {});
    } catch (err) {
      // Not signed in (any more).
      if (err.status === 401 || err.status === 403) api.lostSession();
    }
  }
  window.addEventListener('hashchange', render);
  if (!store.state.user && !publicRoutes.some((r) => r.re.test(path))) {
    if (path !== '/') sessionStorage.setItem('sm-return', location.hash);
    location.hash = '/login';
  }
  render();
}

boot();
