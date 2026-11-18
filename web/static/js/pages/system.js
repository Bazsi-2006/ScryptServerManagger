// Host system overview: CPU and memory of the machine running the manager.

import * as api from '../api.js';
import * as store from '../store.js';
import { h, replace, loading, emptyState, fmtBytes, fmtDuration } from '../ui.js';
import { meter } from './dashboard.js';

const SVG = 'http://www.w3.org/2000/svg';

/** A small line chart of percentages (0-100) built with SVG DOM nodes. */
function sparkline(values, cls) {
  const w = 600;
  const hgt = 120;
  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('viewBox', `0 0 ${w} ${hgt}`);
  svg.setAttribute('preserveAspectRatio', 'none');
  svg.setAttribute('class', 'spark ' + cls);
  svg.setAttribute('role', 'img');
  for (const y of [0.25, 0.5, 0.75]) {
    const line = document.createElementNS(SVG, 'line');
    line.setAttribute('x1', '0'); line.setAttribute('x2', String(w));
    line.setAttribute('y1', String(hgt * y)); line.setAttribute('y2', String(hgt * y));
    line.setAttribute('class', 'spark-grid');
    svg.append(line);
  }
  if (values.length > 1) {
    const step = w / (values.length - 1);
    const pts = values.map((v, i) => `${(i * step).toFixed(1)},${(hgt - (Math.max(0, Math.min(100, v)) / 100) * (hgt - 4) - 2).toFixed(1)}`);
    const area = document.createElementNS(SVG, 'polygon');
    area.setAttribute('points', `0,${hgt} ${pts.join(' ')} ${w},${hgt}`);
    area.setAttribute('class', 'spark-area');
    const line = document.createElementNS(SVG, 'polyline');
    line.setAttribute('points', pts.join(' '));
    line.setAttribute('class', 'spark-line');
    svg.append(area, line);
  }
  return svg;
}

export function systemPage() {
  const box = h('div', null, loading('Waiting for the first measurement…'));
  const updated = h('span', { class: 'muted small' });
  let poll = null;

  function render() {
    const m = store.state.metrics.latest;
    if (!m) return;
    const hist = store.state.metrics.history;
    const secs = Math.round((store.state.features.metrics_interval_ms || 5000) / 1000);
    updated.textContent = `Updated ${new Date(m.time).toLocaleTimeString()} · refreshes every ${secs}s`;
    replace(box,
      h('div', { class: 'metric-grid' },
        h('div', { class: 'card' },
          h('h2', null, 'CPU'),
          meter('Usage', m.cpu_percent, `${m.cpu_count} logical cores`),
          sparkline(hist.map((x) => x.cpu_percent), 'spark-cpu'),
          h('dl', { class: 'kv' },
            h('dt', null, 'Load average'), h('dd', null, `${m.load1.toFixed(2)} · ${m.load5.toFixed(2)} · ${m.load15.toFixed(2)}`),
            h('dt', null, 'Uptime'), h('dd', null, fmtDuration(m.uptime_seconds)))),
        h('div', { class: 'card' },
          h('h2', null, 'Memory'),
          meter('Usage', m.mem_used_percent, `${fmtBytes(m.mem_used_bytes)} of ${fmtBytes(m.mem_total_bytes)}`),
          sparkline(hist.map((x) => x.mem_used_percent), 'spark-mem'),
          h('dl', { class: 'kv' },
            h('dt', null, 'Total'), h('dd', null, fmtBytes(m.mem_total_bytes)),
            h('dt', null, 'Used'), h('dd', null, fmtBytes(m.mem_used_bytes)),
            h('dt', null, 'Available'), h('dd', null, fmtBytes(m.mem_available_bytes))))),
      h('p', { class: 'hint' }, 'These figures describe the Linux host running Server Manager, read from /proc. "Used" memory is total minus available, so reclaimable caches are not counted as used.'));
  }

  async function fetchNow() {
    try {
      const res = await api.get('/system/metrics');
      if (res.latest) {
        store.state.metrics.latest = res.latest;
        store.state.metrics.history = res.history || [];
        render();
      }
    } catch (err) {
      if (err.status === 403) {
        replace(box, emptyState('Not available', 'Host metrics are only visible to administrators.'));
        clearInterval(poll);
      }
    }
  }

  const off = store.on('metrics', render);
  fetchNow();
  // Live updates arrive over the event stream; poll only while it is down.
  poll = setInterval(() => { if (!store.state.live) fetchNow(); }, store.state.features.metrics_interval_ms || 5000);
  const el = h('div', { class: 'page' }, h('div', { class: 'page-head' }, h('h1', null, 'Host system'), updated), box);
  return { el, destroy: () => { off(); clearInterval(poll); } };
}
