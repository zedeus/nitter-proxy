'use strict';

const $ = id => document.getElementById(id);
const tooltip = $('tooltip');
const tooltipTime = tooltip.querySelector('.tt-time');
const tooltipBody = tooltip.querySelector('.tt-body');

// ── State ──

let snapshots = [];
let startedAt = 0;
let cacheConfig = {};
let requestLog = [];
let viewMode = 'live';                        // 'live' = auto-scroll, 'zoomed' = frozen view
let viewMinSec = Date.now() / 1000 - 3600;    // x-axis left edge (absolute seconds)
let viewMaxSec = Date.now() / 1000;            // x-axis right edge (absolute seconds)
let liveRangeMs = 3600000;                     // time range when in live mode
let panState = null;                           // drag-to-pan tracking
let hasEverReceivedData = false;
let ws = null;
let reconnectDelay = 200;
let requestSourceFilter = '';
let requestStatusFilter = '';
let globalEndpoint = '';
let globalSourceFilter = ''; // '', 'cache', 'upstream'
let requestSort = null;
let requestSortClicks = 0;
let requestScrollPaused = false;
let requestSearchText = '';
let expandedEndpoints = new Set();

// Cached per-cycle data (populated at the top of updateAll, consumed by all render functions)
let _filtered = [];      // snapshots in view range with 2 edge-pad each side (for rate charts)
let _visible = [];       // snapshots strictly within [viewMinSec, viewMaxSec] (for stats, response codes)
let _statsBase = null;   // 1 snapshot before viewMin (baseline for windowed stats)
let _stats = null;       // windowed stats object (deltas when zoomed, latest snapshot when live)
let _viewReqs = [];      // request log entries within the view range (when zoomed)
let _rafPending = false; // rAF gate for drag-to-pan throttling
let _reqUpdatePending = false; // rAF gate for request-driven updateAll

// ── Formatting ──

function formatNumber(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(1) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
  return String(n);
}

function formatBytes(b) {
  if (b >= 1073741824) return (b / 1073741824).toFixed(2) + ' GB';
  if (b >= 1048576) return (b / 1048576).toFixed(1) + ' MB';
  if (b >= 1024) return (b / 1024).toFixed(1) + ' KB';
  return b + ' B';
}

function formatPercent(v) { return (v * 100).toFixed(1) + '%'; }
function formatRate(v) { return v < 0.1 && v > 0 ? v.toFixed(2) : v.toFixed(1); }
function commafy(n) { return Math.round(n).toLocaleString('en-US'); }

function formatTime(ts) {
  const d = new Date(ts);
  return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
}

function formatTimeFull(ts) {
  return formatTime(ts) + ':' + String(new Date(ts).getSeconds()).padStart(2, '0');
}

function formatDuration(ms) {
  const s = Math.floor(ms / 1000);
  const days = Math.floor(s / 86400);
  const hours = Math.floor((s % 86400) / 3600);
  const mins = Math.floor((s % 3600) / 60);
  if (days) return days + 'd ' + hours + 'h';
  if (hours) return hours + 'h ' + mins + 'm';
  return mins + 'm ' + (s % 60) + 's';
}

function formatLatency(microseconds) {
  const ms = microseconds / 1000;
  if (ms < 1) return ms.toFixed(2) + 'ms';
  if (ms < 100) return ms.toFixed(1) + 'ms';
  return Math.round(ms) + 'ms';
}

// ── Data helpers ──

/** Advance the view window when in live mode. No-op when zoomed. */
function updateViewBounds() {
  if (viewMode === 'live') {
    viewMaxSec = Date.now() / 1000;
    viewMinSec = viewMaxSec - liveRangeMs / 1000;
    // Don't show empty time before the oldest data
    if (snapshots.length > 0) {
      const oldest = snapshots[0].ts / 1000;
      if (viewMinSec < oldest) viewMinSec = oldest;
    }
  }
}

/**
 * Compute filtered snapshots for the current view range.
 * Returns { filtered, visible }:
 *   filtered: includes 1 pad snapshot before AND after the view (for rate charts).
 *             The before-pad ensures rate computation captures events at the left edge.
 *             The after-pad ensures the chart line extends past the right edge (no gaps).
 *   visible:  strictly within [viewMinSec, viewMaxSec] (for stats, response codes)
 * Uses binary search since snapshots are sorted by timestamp.
 */
function computeFiltered() {
  const minMs = viewMinSec * 1000;
  const maxMs = viewMaxSec * 1000;

  // Binary search for first snapshot >= minMs
  let lo = 0, hi = snapshots.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (snapshots[mid].ts < minMs) lo = mid + 1;
    else hi = mid;
  }
  const startIdx = lo;

  // Binary search for last snapshot <= maxMs
  lo = startIdx; hi = snapshots.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (snapshots[mid].ts <= maxMs) lo = mid + 1;
    else hi = mid;
  }
  const endIdx = lo; // exclusive

  const visible = snapshots.slice(startIdx, endIdx);
  // Pad: 2 snapshots before AND after the view for chart line continuity
  const padStart = Math.max(0, startIdx - 2);
  const padEnd = Math.min(snapshots.length, endIdx + 2);
  const filtered = snapshots.slice(padStart, padEnd);
  // Stats baseline: exactly 1 snapshot before viewMin (not 2) to avoid including
  // data from too far outside the visible range in stat computations.
  const statsBase = startIdx > 0 ? snapshots[startIdx - 1] : null;
  return { filtered, visible, statsBase };
}

/** Prevent the view from extending past available data without collapsing the zoom range. */
function clampView() {
  if (snapshots.length < 2) return;
  const oldest = snapshots[0].ts / 1000;
  const newest = snapshots[snapshots.length - 1].ts / 1000;
  const range = viewMaxSec - viewMinSec;
  const dataSpan = newest - oldest;
  if (range >= dataSpan) {
    // Center data in view, preserve the zoom range
    const mid = (oldest + newest) / 2;
    viewMinSec = mid - range / 2;
    viewMaxSec = mid + range / 2;
  } else {
    if (viewMinSec < oldest) { viewMinSec = oldest; viewMaxSec = oldest + range; }
    if (viewMaxSec > newest) { viewMaxSec = newest; viewMinSec = newest - range; }
  }
}

function updateTimeRangeHighlight() {
  document.querySelectorAll('#timeRange button').forEach(btn => {
    btn.classList.toggle('on', viewMode === 'live' && +btn.dataset.r === liveRangeMs);
  });
  $('pausedBar').style.display = viewMode === 'zoomed' ? 'flex' : 'none';
}

/** Average data points into larger time buckets. */
function downsample(ts, vals, bucketSec) {
  if (bucketSec <= 1 || ts.length < 2) return [ts, vals];
  const out_ts = [], out_vals = [];
  let i = 0;
  while (i < ts.length) {
    const bucketStart = ts[i];
    const bucketEnd = bucketStart + bucketSec;
    let sum = 0, count = 0;
    while (i < ts.length && ts[i] < bucketEnd) {
      sum += vals[i];
      count++;
      i++;
    }
    out_ts.push(bucketStart + (count > 1 ? (ts[i - 1] - bucketStart) / 2 : 0));
    out_vals.push(sum / count);
  }
  return [out_ts, out_vals];
}

/** Compute per-second rates from a cumulative counter field. Uses cached _filtered. */
function computeRates(field) {
  const snaps = _filtered;
  if (snaps.length < 2) return [[], []];
  const ts = [], vals = [];
  for (let i = 1; i < snaps.length; i++) {
    const dt = (snaps[i].ts - snaps[i - 1].ts) / 1000;
    ts.push(snaps[i].ts / 1000);
    vals.push(dt > 0 ? Math.max(0, (snaps[i][field] - snaps[i - 1][field]) / dt) : 0);
  }
  return [ts, vals];
}

function lastRate(field) {
  // Average over the last 30 seconds for a stable rate
  if (snapshots.length < 2) return 0;
  const curr = snapshots[snapshots.length - 1];
  const windowMs = 30000;
  let prev = null;
  for (let i = snapshots.length - 2; i >= 0; i--) {
    if (curr.ts - snapshots[i].ts >= windowMs) { prev = snapshots[i]; break; }
  }
  if (!prev) prev = snapshots[0];
  const dt = (curr.ts - prev.ts) / 1000;
  return dt > 0 ? Math.max(0, (curr[field] - prev[field]) / dt) : 0;
}

function sumErrors(snap) {
  let total = snap.fetchErrors || 0;
  for (const [code, count] of Object.entries(snap.responseCodes || {})) {
    if (+code >= 400) total += count;
  }
  return total;
}

/** Error rates as [timestamps[], values[]] using cached _filtered */
function errorRatesSeries() {
  const snaps = _filtered;
  if (snaps.length < 2) return [[], []];
  const ts = [], vals = [];
  for (let i = 1; i < snaps.length; i++) {
    const dt = (snaps[i].ts - snaps[i - 1].ts) / 1000;
    ts.push(snaps[i].ts / 1000);
    vals.push(dt > 0 ? Math.max(0, (sumErrors(snaps[i]) - sumErrors(snaps[i - 1])) / dt) : 0);
  }
  return [ts, vals];
}

/** Average latency per interval as [timestamps[], values[]] in ms. Uses cached _filtered. */
function latencySeriesData() {
  const snaps = _filtered;
  if (snaps.length < 2) return [[], []];
  const ts = [], vals = [];
  for (let i = 1; i < snaps.length; i++) {
    const deltaUs = snaps[i].latencyUsTotal - snaps[i - 1].latencyUsTotal;
    const deltaCount = snaps[i].latencyCount - snaps[i - 1].latencyCount;
    ts.push(snaps[i].ts / 1000);
    vals.push(deltaCount > 0 ? Math.max(0, (deltaUs / deltaCount) / 1000) : 0);
  }
  return [ts, vals];
}

function sparklinePoints(data, width, height) {
  if (data.length < 2) return '';
  const recent = data.slice(-16);
  const min = Math.min(...recent.map(d => d.y));
  const max = Math.max(...recent.map(d => d.y));
  const range = max - min || 1;
  return recent.map((d, i) => {
    const x = (i / (recent.length - 1)) * width;
    const y = height - 1.5 - ((d.y - min) / range) * (height - 3);
    return x.toFixed(1) + ',' + y.toFixed(1);
  }).join(' ');
}

function getEndpoints() {
  // When zoomed, derive from cached _viewReqs (matches other panels)
  if (viewMode === 'zoomed') {
    const epData = {};
    for (const r of _viewReqs) {
      if (globalSourceFilter && r.source !== globalSourceFilter) continue;
      const ep = r.endpoint || 'unknown';
      if (!epData[ep]) epData[ep] = { hits: 0, misses: 0, errors: {}, latUs: 0, latN: 0, bytes: 0 };
      const d = epData[ep];
      if (r.source === 'cache') d.hits++;
      else d.misses++;
      if (r.status >= 400) {
        d.errors[r.status] = (d.errors[r.status] || 0) + 1;
      }
      if (r.source !== 'cache' && r.latencyUs > 0) {
        d.latUs += r.latencyUs;
        d.latN++;
      }
    }
    return Object.entries(epData).map(([name, d]) => {
      const total = d.hits + d.misses;
      const rate = total > 0 ? d.hits / total : 0;
      const ttl = (cacheConfig.endpointTTLs || {})[name] || cacheConfig.defaultTTL || '-';
      const errors = Object.values(d.errors).reduce((a, b) => a + b, 0);
      const errDetail = Object.entries(d.errors).map(([c, n]) => n + 'x ' + c);
      const avgLat = d.latN > 0 ? (d.latUs / d.latN) / 1000 : 0;
      return { name, hits: d.hits, misses: d.misses, total, rate, ttl, errors, errDetail, avgLat, bytes: d.bytes };
    });
  }

  const latest = snapshots[snapshots.length - 1];
  if (!latest) return [];

  const names = new Set([
    ...Object.keys(latest.epHits || {}),
    ...Object.keys(latest.epMisses || {}),
  ]);

  const endpointErrors = {};
  for (const [key, count] of Object.entries(latest.epErrors || {})) {
    const sep = key.indexOf('\t');
    if (sep < 0) continue;
    const ep = key.slice(0, sep), code = key.slice(sep + 1);
    if (!endpointErrors[ep]) endpointErrors[ep] = {};
    endpointErrors[ep][code] = count;
  }

  return [...names].map(name => {
    const hits = (latest.epHits || {})[name] || 0;
    const misses = (latest.epMisses || {})[name] || 0;
    const total = hits + misses;
    const rate = total > 0 ? hits / total : 0;
    const ttl = (cacheConfig.endpointTTLs || {})[name] || cacheConfig.defaultTTL || '-';
    const errMap = endpointErrors[name] || {};
    const errors = Object.values(errMap).reduce((a, b) => a + b, 0);
    const errDetail = Object.entries(errMap).map(([c, n]) => n + 'x ' + c);
    const avgLat = endpointAvgLatency(name, latest);
    const bytes = endpointTotalBytes(name, latest);
    return { name, hits, misses, total, rate, ttl, errors, errDetail, avgLat, bytes };
  });
}

// ── Per-endpoint data helpers (all use cached _filtered) ──

/** Per-endpoint rates from a map field (epHits, epMisses). */
function endpointMapRates(mapField, endpoint) {
  const snaps = _filtered;
  if (snaps.length < 2) return [[], []];
  const ts = [], vals = [];
  for (let i = 1; i < snaps.length; i++) {
    const dt = (snaps[i].ts - snaps[i - 1].ts) / 1000;
    const curr = (snaps[i][mapField] || {})[endpoint] || 0;
    const prev = (snaps[i - 1][mapField] || {})[endpoint] || 0;
    ts.push(snaps[i].ts / 1000);
    vals.push(dt > 0 ? Math.max(0, (curr - prev) / dt) : 0);
  }
  return [ts, vals];
}

function endpointErrorRatesSeries(endpoint) {
  const snaps = _filtered;
  if (snaps.length < 2) return [[], []];
  const prefix = endpoint + '\t';
  const ts = [], vals = [];
  for (let i = 1; i < snaps.length; i++) {
    const dt = (snaps[i].ts - snaps[i - 1].ts) / 1000;
    let currTotal = 0, prevTotal = 0;
    for (const [k, v] of Object.entries(snaps[i].epErrors || {})) if (k.startsWith(prefix)) currTotal += v;
    for (const [k, v] of Object.entries(snaps[i - 1].epErrors || {})) if (k.startsWith(prefix)) prevTotal += v;
    ts.push(snaps[i].ts / 1000);
    vals.push(dt > 0 ? Math.max(0, (currTotal - prevTotal) / dt) : 0);
  }
  return [ts, vals];
}

function endpointHitRateSeries(endpoint) {
  const snaps = _filtered;
  const ts = [], vals = [];
  for (const s of snaps) {
    const h = (s.epHits || {})[endpoint] || 0;
    const m = (s.epMisses || {})[endpoint] || 0;
    ts.push(s.ts / 1000);
    vals.push((h + m) > 0 ? (h / (h + m)) * 100 : 0);
  }
  return [ts, vals];
}

/** Response codes for an endpoint, using windowed stats when zoomed. */
function endpointResponseCodes(endpoint, stats) {
  const source = stats || snapshots[snapshots.length - 1];
  if (!source) return {};
  const codes = {};
  const prefix = endpoint + '\t';
  for (const [k, v] of Object.entries(source.epErrors || {})) {
    if (k.startsWith(prefix)) codes[k.slice(prefix.length)] = v;
  }
  return codes;
}

// ── New feature data helpers ──

/** Latency percentile series: p50, p95, p99 over time. Uses _filtered. */
function latencyPercentileSeries() {
  const snaps = _filtered;
  const ts = [], p50 = [], p95 = [], p99 = [];
  for (const s of snaps) {
    ts.push(s.ts / 1000);
    p50.push((s.latP50 || 0) / 1000); // us -> ms
    p95.push((s.latP95 || 0) / 1000);
    p99.push((s.latP99 || 0) / 1000);
  }
  return [ts, p50, p95, p99];
}

/** Cache vs upstream latency as [ts[], cachMs[], upstreamMs[]]. Uses _filtered. */
function cacheVsUpstreamLatency() {
  const snaps = _filtered;
  if (snaps.length < 2) return [[], [], []];
  const ts = [], cacheMs = [], upMs = [];
  for (let i = 1; i < snaps.length; i++) {
    const dt = (snaps[i].ts - snaps[i - 1].ts) / 1000;
    if (dt <= 0) continue;
    ts.push(snaps[i].ts / 1000);
    // Cache latency
    const cDeltaUs = (snaps[i].cacheLatUs || 0) - (snaps[i - 1].cacheLatUs || 0);
    const cDeltaN = (snaps[i].cacheLatN || 0) - (snaps[i - 1].cacheLatN || 0);
    cacheMs.push(cDeltaN > 0 ? Math.max(0, (cDeltaUs / cDeltaN) / 1000) : 0);
    // Upstream latency
    const uDeltaUs = snaps[i].latencyUsTotal - snaps[i - 1].latencyUsTotal;
    const uDeltaN = snaps[i].latencyCount - snaps[i - 1].latencyCount;
    upMs.push(uDeltaN > 0 ? Math.max(0, (uDeltaUs / uDeltaN) / 1000) : 0);
  }
  return [ts, cacheMs, upMs];
}

/** Per-code error rates over time. Returns { ts, series: {code: vals[]} }. Uses _filtered. */
function errorTimelineSeries() {
  const snaps = _filtered;
  if (snaps.length < 2) return { ts: [], series: {} };
  // Collect all error codes present across the range
  const allCodes = new Set();
  for (const s of snaps) {
    for (const [code] of Object.entries(s.responseCodes || {})) {
      if (+code >= 400) allCodes.add(+code);
    }
  }
  if (snaps[snaps.length - 1].fetchErrors > 0) allCodes.add('fetch');
  const ts = [];
  const series = {};
  for (const code of allCodes) series[code] = [];
  for (let i = 1; i < snaps.length; i++) {
    const dt = (snaps[i].ts - snaps[i - 1].ts) / 1000;
    ts.push(snaps[i].ts / 1000);
    for (const code of allCodes) {
      if (code === 'fetch') {
        const delta = (snaps[i].fetchErrors || 0) - (snaps[i - 1].fetchErrors || 0);
        series[code].push(dt > 0 ? Math.max(0, delta / dt) : 0);
      } else {
        const curr = (snaps[i].responseCodes || {})[code] || 0;
        const prev = (snaps[i - 1].responseCodes || {})[code] || 0;
        series[code].push(dt > 0 ? Math.max(0, (curr - prev) / dt) : 0);
      }
    }
  }
  return { ts, series };
}

/** Per-endpoint avg latency from cumulative snapshot data. */
function endpointAvgLatency(endpoint, stats) {
  const source = stats || snapshots[snapshots.length - 1];
  if (!source) return 0;
  const us = (source.epLatUs || {})[endpoint] || 0;
  const n = (source.epLatN || {})[endpoint] || 0;
  return n > 0 ? (us / n) / 1000 : 0; // ms
}

/** Per-endpoint bytes from cumulative snapshot data. */
function endpointTotalBytes(endpoint, stats) {
  const source = stats || snapshots[snapshots.length - 1];
  if (!source) return 0;
  return (source.epBytes || {})[endpoint] || 0;
}

// ── Chart colors (Nitter palette) ──

const COLORS = {
  green: '#3fb950',
  blue: '#1da1f2',
  amber: '#d29922',
  red: '#ff6c60',
  purple: '#bc8cff',
  cyan: '#39d2c0',
};

const nullify = arr => arr.map(() => null);

// ── Chart metadata (for donut hover) ──

const chartMeta = {};

// ── uPlot chart management ──

const uplotInstances = {};

/** Shared uPlot axis/grid config for the Nitter dark theme */
function darkAxes(yLabel) {
  return [
    { // x-axis
      stroke: '#888889',
      grid: { stroke: '#28282880', width: 1 },
      ticks: { stroke: '#28282880', width: 1 },
      font: '11px sans-serif',
      values: (u, vals) => {
        const range = u.scales.x.max - u.scales.x.min;
        const showSec = range < 600 || (vals.length > 1 &&
          new Date(vals[0] * 1000).getMinutes() === new Date(vals[1] * 1000).getMinutes());
        let prev = '';
        return vals.map(v => {
          const d = new Date(v * 1000);
          const h = String(d.getHours()).padStart(2, '0');
          const m = String(d.getMinutes()).padStart(2, '0');
          const label = showSec ? h + ':' + m + ':' + String(d.getSeconds()).padStart(2, '0') : h + ':' + m;
          if (label === prev) return '';  // deduplicate adjacent identical labels
          prev = label;
          return label;
        });
      },
    },
    { // y-axis
      stroke: '#888889',
      grid: { stroke: '#28282880', width: 1 },
      ticks: { stroke: '#28282880', width: 1 },
      font: '11px sans-serif',
      values: yLabel || ((u, vals) => vals.map(v => formatNumber(v))),
      size: 48,
    },
  ];
}

/**
 * Create or update a uPlot time-series chart.
 * seriesDefs: [{label, color, fill?, width?}]
 * data: [timestamps[], ...seriesValues[]]
 * yValues: formatter for y-axis labels
 */
function renderUPlot(containerId, seriesDefs, data, opts = {}) {
  const container = $(containerId);
  if (!container) return;

  // Downsample all series to reduce density at large time ranges
  if (data[0] && data[0].length > 2) {
    // Use actual data span (not view range) for bucket size
    const dataRange = data[0][data[0].length - 1] - data[0][0];
    const bs = dataRange <= 300 ? 1 : dataRange <= 900 ? 5 : dataRange <= 1800 ? 10 : 30;
    if (bs > 1) {
      const ts = data[0];
      const [dsTs] = downsample(ts, ts, bs);
      const downsampled = [dsTs];
      for (let si = 1; si < data.length; si++) {
        if (!data[si] || data[si].some(v => v === null)) {
          downsampled.push(dsTs.map(() => null));
        } else {
          const [, dsVals] = downsample(ts, data[si], bs);
          downsampled.push(dsVals);
        }
      }
      data = downsampled;
    }
  }

  const width = container.clientWidth;
  const isLarge = container.classList.contains('uchart-lg');
  const chartHeight = isLarge ? 220 : 180;

  // If data is empty, show placeholder or flat zero line
  let isSynthetic = false;
  if (!data[0] || data[0].length < 2) {
    if (!hasEverReceivedData) {
      container.innerHTML = `<div style="display:flex;align-items:center;justify-content:center;height:${chartHeight}px;color:#888889;font-size:14px">Waiting for data...</div>`;
      return;
    }
    const lo = viewMinSec, mid = (viewMinSec + viewMaxSec) / 2, hi = viewMaxSec;
    data = [
      [lo, mid, hi],
      ...seriesDefs.map(() => [0, 0, 0]),
    ];
    isSynthetic = true;
  }

  // No synthetic edge extension needed: _filtered includes pad snapshots before/after
  // the view, so rate computation produces data points that extend past both edges
  // naturally. The chart line is continuous through the view boundaries.

  const fewPoints = data[0].length < 20;
  const seriesKey = seriesDefs.map(s => s.label + '|' + s.color).join(',');
  const series = [
    { label: 'Time' },
    ...seriesDefs.map(s => ({
      label: s.label,
      stroke: s.color,
      fill: s.fill || (s.color + '50'),
      width: s.width || 2.5,
      points: { show: !isSynthetic, size: fewPoints ? 8 : 4 },
      spanGaps: true,
    })),
  ];

  const uOpts = {
    width,
    height: chartHeight,
    cursor: {
      drag: { x: false, y: false },
      focus: { prox: 30 },
      points: { show: false },
    },
    legend: { show: true },
    series,
    axes: darkAxes(opts.yValues),
    scales: {
      x: { time: true, range: () => [viewMinSec, viewMaxSec] },
      y: opts.yMax != null
        ? { range: [opts.yMin ?? 0, opts.yMax] }
        : {
          range: (u, min, max) => {
            // Include pad data in y-range so spikes from outside the x-scale
            // range aren't clipped at the top of the chart
            let yMax = max;
            for (let si = 1; si < u.data.length; si++) {
              if (!u.data[si]) continue;
              for (const v of u.data[si]) {
                if (v !== null && v > yMax) yMax = v;
              }
            }
            return [opts.yMin ?? 0, (yMax == null || yMax <= 0) ? 1 : yMax];
          },
        },
    },
    hooks: {
      init: [u => {
        const over = u.over;
        over.style.cursor = 'grab';

        // Scroll zoom: anchored at cursor position
        over.addEventListener('wheel', e => {
          e.preventDefault();
          const left = u.cursor.left;
          if (left == null || left < 0) return;

          const scaleMin = u.scales.x.min;
          const scaleMax = u.scales.x.max;
          const pxW = over.clientWidth;
          let xVal;
          if (left < pxW * 0.02) xVal = scaleMin;
          else if (left > pxW * 0.98) xVal = scaleMax;
          else xVal = u.posToVal(left, 'x');

          const curRange = scaleMax - scaleMin;
          const factor = e.deltaY > 0 ? 1.1 : 1 / 1.1;
          // Cap zoom-out at actual data span instead of hard 3600
          const maxRange = snapshots.length >= 2
            ? (snapshots[snapshots.length - 1].ts - snapshots[0].ts) / 1000
            : 3600;
          const newRange = Math.max(10, Math.min(Math.max(maxRange, 60), curRange * factor));

          const frac = (xVal - scaleMin) / curRange;
          viewMinSec = xVal - frac * newRange;
          viewMaxSec = xVal + (1 - frac) * newRange;
          clampView();
          viewMode = 'zoomed';

          updateTimeRangeHighlight();
          updateAll();
        }, { passive: false });

        // Drag to pan
        over.addEventListener('mousedown', e => {
          if (e.button !== 0) return;
          e.preventDefault();
          const sMin = viewMinSec, sMax = viewMaxSec;
          panState = {
            sx: e.clientX,
            minSec: sMin,
            maxSec: sMax,
            range: sMax - sMin,
            pxWidth: over.clientWidth,
            moved: false,
          };
        });

        over.addEventListener('mouseleave', () => { tooltip.style.display = 'none'; });

        // Double-click resets to 1h live view
        over.addEventListener('dblclick', e => {
          e.preventDefault();
          viewMode = 'live';
          liveRangeMs = 3600000;
          updateViewBounds();
          updateTimeRangeHighlight();
          updateAll();
        });
      }],
      setCursor: [u => {
        const { left, top, idx } = u.cursor;
        if (idx == null || left < 0 || top < 0) {
          tooltip.style.display = 'none';
          return;
        }
        const ts = u.data[0][idx];
        if (ts == null) { tooltip.style.display = 'none'; return; }

        tooltipTime.textContent = formatTimeFull(ts * 1000);
        let html = '';
        for (let si = 1; si < u.series.length; si++) {
          const s = u.series[si];
          if (!s.show) continue;
          const v = u.data[si][idx];
          if (v == null) continue;
          const color = s._stroke || s.stroke || '#888';
          html += `<div class="tt-row"><span class="tt-dot" style="background:${color}"></span>${s.label}<span class="tt-val">${typeof v === 'number' ? formatRate(v) : v}</span></div>`;
        }
        if (!html) { tooltip.style.display = 'none'; return; }
        tooltipBody.innerHTML = html;
        tooltip.style.display = 'block';
        const rect = u.over.getBoundingClientRect();
        tooltip.style.left = (rect.left + left + 12) + 'px';
        tooltip.style.top = (rect.top + top - 20) + 'px';
      }],
    },
  };

  // Skip rendering if container is hidden (collapsed panel)
  if (width < 10 || chartHeight < 10) return;

  // Reuse existing instance if series fingerprint + dimensions match
  const existing = uplotInstances[containerId];
  if (existing && existing._seriesKey === seriesKey && existing.series.length === series.length
      && existing.width === width && existing.height === chartHeight) {
    existing.setData(data);
    return;
  }

  // Recreate: clear container, create fresh
  if (existing) existing.destroy();
  container.innerHTML = '';
  const inst = new uPlot(uOpts, data, container);
  inst._seriesKey = seriesKey;
  uplotInstances[containerId] = inst;
}

// ── Canvas charts (donut + bar only) ──

function setupCanvas(canvas) {
  const rect = canvas.getBoundingClientRect();
  const dpr = devicePixelRatio || 1;
  canvas.width = rect.width * dpr;
  canvas.height = rect.height * dpr;
  const ctx = canvas.getContext('2d');
  ctx.scale(dpr, dpr);
  return { ctx, w: rect.width, h: rect.height };
}

function drawDonut(canvasId, segments) {
  const canvas = $(canvasId);
  const { ctx, w, h } = setupCanvas(canvas);
  const cx = w * 0.35, cy = h / 2;
  const radius = Math.min(cx - 16, cy - 16, 80);
  const innerRadius = radius * 0.6;

  ctx.fillStyle = '#121212';
  ctx.fillRect(0, 0, w, h);

  const total = segments.reduce((a, s) => a + s.value, 0);
  if (!total) {
    ctx.fillStyle = '#888889';
    ctx.font = '14px sans-serif';
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText('No data', cx, cy);
    chartMeta[canvasId] = { cx, cy, rad: 0, inn: 0, segs: [], tot: 0, hoveredSeg: -1 };
    return;
  }

  const prevMeta = chartMeta[canvasId];
  const hoveredIdx = prevMeta?.hoveredSeg ?? -1;
  const segmentAngles = [];
  let angle = -Math.PI / 2;
  for (let si = 0; si < segments.length; si++) {
    const seg = segments[si];
    const sweep = (seg.value / total) * Math.PI * 2;
    segmentAngles.push({ ...seg, start: angle, end: angle + sweep });
    const isHovered = (si === hoveredIdx);
    const r = isHovered ? radius + 4 : radius;
    const ir = isHovered ? innerRadius - 2 : innerRadius;
    ctx.beginPath();
    ctx.arc(cx, cy, r, angle, angle + sweep);
    ctx.arc(cx, cy, ir, angle + sweep, angle, true);
    ctx.closePath();
    ctx.fillStyle = seg.color;
    ctx.globalAlpha = isHovered ? 1 : 0.8;
    ctx.fill();
    ctx.globalAlpha = 1;
    angle += sweep;
  }
  chartMeta[canvasId] = { cx, cy, rad: radius, inn: innerRadius, segs: segmentAngles, tot: total, hoveredSeg: hoveredIdx };

  ctx.fillStyle = '#f8f8f2';
  ctx.font = 'bold 22px sans-serif';
  ctx.textAlign = 'center';
  ctx.textBaseline = 'middle';
  ctx.fillText(formatPercent(segments[0].value / total), cx, cy - 5);
  ctx.font = '12px sans-serif';
  ctx.fillStyle = '#888889';
  ctx.fillText('accepted', cx, cy + 14);

  const legendX = w * 0.58;
  let legendY = cy - segments.length * 14;
  for (const seg of segments) {
    ctx.fillStyle = seg.color;
    ctx.beginPath();
    ctx.arc(legendX, legendY, 4, 0, Math.PI * 2);
    ctx.fill();
    ctx.fillStyle = '#f8f8f2';
    ctx.font = '13px sans-serif';
    ctx.textAlign = 'left';
    ctx.fillText(seg.label, legendX + 11, legendY);
    ctx.fillStyle = '#888889';
    ctx.font = '12px sans-serif';
    ctx.fillText(commafy(seg.value), legendX + 11, legendY + 16);
    legendY += 34;
  }
}

function drawBarChart(canvasId, bars) {
  const canvas = $(canvasId);
  const { ctx, w, h } = setupCanvas(canvas);

  ctx.fillStyle = '#121212';
  ctx.fillRect(0, 0, w, h);

  if (!bars.length) {
    ctx.fillStyle = '#888889';
    ctx.font = '14px sans-serif';
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText('No responses yet', w / 2, h / 2);
    return;
  }

  const maxVal = Math.max(...bars.map(b => b.value));
  const total = bars.reduce((a, b) => a + b.value, 0);
  const pad = { left: 56, right: 56, top: 10, bottom: 10 };
  const barHeight = Math.min(24, (h - pad.top - pad.bottom - (bars.length - 1) * 3) / bars.length);
  const plotWidth = w - pad.left - pad.right;

  const barPositions = [];

  bars.forEach((bar, i) => {
    const y = pad.top + i * (barHeight + 3);
    const barWidth = maxVal > 0 ? (bar.value / maxVal) * plotWidth : 0;
    barPositions.push({ ...bar, y, barWidth, barHeight });

    const isHighlighted = (chartMeta[canvasId]?.hoveredBar === i);
    ctx.fillStyle = bar.color;
    ctx.globalAlpha = isHighlighted ? 0.85 : 0.4;
    ctx.beginPath();
    ctx.roundRect(pad.left, y, Math.max(barWidth, 2), barHeight, 3);
    ctx.fill();
    ctx.globalAlpha = 1;
    if (isHighlighted) {
      ctx.strokeStyle = bar.color;
      ctx.lineWidth = 1.5;
      ctx.beginPath();
      ctx.roundRect(pad.left, y, Math.max(barWidth, 2), barHeight, 3);
      ctx.stroke();
    }

    ctx.fillStyle = '#888889';
    ctx.font = '12px sans-serif';
    ctx.textAlign = 'right';
    ctx.textBaseline = 'middle';
    ctx.fillText(bar.label, pad.left - 8, y + barHeight / 2);

    ctx.fillStyle = '#f8f8f2';
    ctx.textAlign = 'left';
    ctx.fillText(commafy(bar.value), pad.left + barWidth + 8, y + barHeight / 2);
  });

  chartMeta[canvasId] = { bars: barPositions, pad, total };
}

// Bar chart hover handler
let barHoverInitialized = {};
function initBarHover(canvasId) {
  if (barHoverInitialized[canvasId]) return;
  barHoverInitialized[canvasId] = true;
  const canvas = $(canvasId);
  canvas.addEventListener('mousemove', e => {
    const meta = chartMeta[canvasId];
    if (!meta || !meta.bars) { tooltip.style.display = 'none'; return; }
    const rect = canvas.getBoundingClientRect();
    const my = e.clientY - rect.top;
    const hitIdx = meta.bars.findIndex(b => my >= b.y && my < b.y + b.barHeight);
    const hit = hitIdx >= 0 ? meta.bars[hitIdx] : null;
    const prevHover = meta.hoveredBar;
    meta.hoveredBar = hitIdx >= 0 ? hitIdx : null;
    if (meta.hoveredBar !== prevHover) renderResponseCodesChart();
    if (!hit) { tooltip.style.display = 'none'; canvas.style.cursor = ''; return; }
    canvas.style.cursor = 'pointer';
    const pct = meta.total > 0 ? formatPercent(hit.value / meta.total) : '-';
    tooltipTime.textContent = 'Status ' + hit.label;
    tooltipBody.innerHTML = `<div>${commafy(hit.value)} responses (${pct})</div>`;
    tooltip.style.display = 'block';
    tooltip.style.left = (e.clientX + 12) + 'px';
    tooltip.style.top = (e.clientY - 20) + 'px';
  });
  canvas.addEventListener('mouseleave', () => {
    tooltip.style.display = 'none'; canvas.style.cursor = '';
    const meta = chartMeta[canvasId];
    if (meta && meta.hoveredBar != null) { meta.hoveredBar = null; renderResponseCodesChart(); }
  });
}

// ── Donut hover interaction ──

let donutInitialized = false;
function initDonutHover(canvasId) {
  const canvas = $(canvasId);
  canvas.addEventListener('mousemove', e => {
    const rect = canvas.getBoundingClientRect();
    const mx = e.clientX - rect.left, my = e.clientY - rect.top;
    const meta = chartMeta[canvasId];
    if (!meta || !meta.segs || !meta.tot) { tooltip.style.display = 'none'; canvas.style.cursor = ''; return; }
    const dx = mx - meta.cx, dy = my - meta.cy;
    const dist = Math.sqrt(dx * dx + dy * dy);
    if (dist < meta.inn || dist > meta.rad) { tooltip.style.display = 'none'; canvas.style.cursor = ''; return; }
    const mouseAngle = Math.atan2(dy, dx);
    let hit = null, hitIdx = -1;
    for (let si = 0; si < meta.segs.length; si++) {
      const seg = meta.segs[si];
      if (seg.value === 0) continue;
      let relative = mouseAngle - seg.start;
      const sweep = seg.end - seg.start;
      while (relative < 0) relative += Math.PI * 2;
      while (relative >= Math.PI * 2) relative -= Math.PI * 2;
      if (relative < sweep) { hit = seg; hitIdx = si; break; }
    }
    const prevHover = meta.hoveredSeg;
    meta.hoveredSeg = hitIdx;
    if (hitIdx !== prevHover) renderAdmissionChart();
    if (!hit) { tooltip.style.display = 'none'; canvas.style.cursor = ''; return; }
    canvas.style.cursor = 'pointer';
    tooltipTime.textContent = hit.label;
    tooltipBody.innerHTML = `<div>${commafy(hit.value)} (${formatPercent(hit.value / meta.tot)})</div>`;
    tooltip.style.display = 'block';
    tooltip.style.left = (e.clientX + 12) + 'px';
    tooltip.style.top = (e.clientY - 20) + 'px';
  });
  canvas.addEventListener('mouseleave', () => {
    tooltip.style.display = 'none'; canvas.style.cursor = '';
    const meta = chartMeta[canvasId];
    if (meta && meta.hoveredSeg !== -1) { meta.hoveredSeg = -1; renderAdmissionChart(); }
  });
}

// ── Sticky header shadow ──

const hdr = document.querySelector('.hdr');
addEventListener('scroll', () => {
  hdr.classList.toggle('scrolled', window.scrollY > 4);
}, { passive: true });

// ── Paused indicator reset ──

$('pausedReset').addEventListener('click', () => {
  viewMode = 'live';
  liveRangeMs = 3600000;
  updateViewBounds();
  updateTimeRangeHighlight();
  updateAll();
});

// ── Panel collapse ──

document.addEventListener('click', e => {
  const header = e.target.closest('.panel-header');
  if (!header) return;
  const panel = header.closest('.panel');
  const wasCollapsed = panel.classList.contains('closed');
  panel.classList.toggle('closed');

  if (wasCollapsed) {
    const chartContainers = panel.querySelectorAll('.uchart');
    chartContainers.forEach(container => {
      if (uplotInstances[container.id]) {
        uplotInstances[container.id].destroy();
        delete uplotInstances[container.id];
      }
    });
    setTimeout(() => renderAllCharts(), 100);
  }
});

// ── Global endpoint filter ──

$('epGlobal').addEventListener('change', e => {
  globalEndpoint = e.target.value;
  updateAll(); // includes updateRequestLog()
});

function setGlobalEndpoint(ep) {
  globalEndpoint = ep;
  $('epGlobal').value = ep;
  updateAll();
}

// ── Global source filter ──

$('globalSource').addEventListener('click', e => {
  const btn = e.target.closest('button');
  if (!btn) return;
  globalSourceFilter = btn.dataset.gs;
  $('globalSource').querySelectorAll('button').forEach(b => b.classList.toggle('on', b.dataset.gs === globalSourceFilter));
  updateAll();
});

let _lastEndpointKey = '';
function populateEndpointSelect() {
  const select = $('epGlobal');
  const endpoints = getEndpoints().map(e => e.name).sort();
  const key = endpoints.join('\t');
  if (key === _lastEndpointKey) return;
  _lastEndpointKey = key;
  const previous = select.value;
  const options = ['<option value="">All endpoints</option>'];
  for (const ep of endpoints) {
    const selected = ep === previous ? ' selected' : '';
    options.push(`<option value="${ep}"${selected}>${ep}</option>`);
  }
  select.innerHTML = options.join('');
  globalEndpoint = select.value;
}

// ── Main update loop ──

/**
 * Compute windowed stats: deltas between edge-pad baseline and last visible snapshot.
 * Uses _filtered[0] (the snapshot just before viewMin) as the "before" baseline so that
 * events at the first visible snapshot are captured in the delta. Without this, a request
 * that lands exactly at viewMin's snapshot would be in _visible[0]'s cumulative value but
 * its delta would be 0 (since it's both the baseline and the first point).
 */
function computeWindowedStats() {
  // Use _statsBase (1 snapshot before viewMin) as baseline and _visible[last]
  // as end. The request log also uses _statsBase.ts as its lower bound so
  // stats and request log always agree.
  const f = _statsBase || (_visible.length > 0 ? _visible[0] : null);
  if (!f || _visible.length < 1) return null;
  const l = _visible[_visible.length - 1];
  const d = (a, b) => Math.max(0, a - b);
  const dmap = (a, b) => {
    const r = {};
    for (const [k, v] of Object.entries(a || {})) {
      const delta = Math.max(0, v - ((b || {})[k] || 0));
      if (delta > 0) r[k] = delta; // omit zero-delta entries
    }
    return r;
  };
  const hits = d(l.hits, f.hits), misses = d(l.misses, f.misses);
  return {
    ts: l.ts, hits, misses, upstream: d(l.upstream, f.upstream),
    avoided: d(l.avoided, f.avoided), bytes: d(l.bytes, f.bytes),
    coalesced: d(l.coalesced, f.coalesced), stale: d(l.stale, f.stale),
    negative: d(l.negative, f.negative), errors: d(l.errors, f.errors),
    admAccepted: d(l.admAccepted, f.admAccepted), admRejected: d(l.admRejected, f.admRejected),
    fetchErrors: d(l.fetchErrors, f.fetchErrors),
    latencyUsTotal: d(l.latencyUsTotal, f.latencyUsTotal), latencyCount: d(l.latencyCount, f.latencyCount),
    hitRate: (hits + misses) > 0 ? hits / (hits + misses) : 0,
    responseCodes: dmap(l.responseCodes, f.responseCodes),
    epHits: dmap(l.epHits, f.epHits), epMisses: dmap(l.epMisses, f.epMisses),
    epErrors: dmap(l.epErrors, f.epErrors),
  };
}

function updateAll() {
  updateViewBounds();
  const latest = snapshots[snapshots.length - 1];
  if (!latest) return;

  // Compute filtered snapshots ONCE for this cycle
  const computed = computeFiltered();
  _filtered = computed.filtered;    // with 2-pad each side, for rate charts
  _visible = computed.visible;      // strict view range, for stats
  _statsBase = computed.statsBase;  // 1 snapshot before viewMin, for windowed stats baseline

  // Compute windowed stats: edge-pad baseline to last visible snapshot
  if (viewMode === 'zoomed') {
    _stats = computeWindowedStats() || latest;
    // Cache the filtered request log for all zoomed-mode consumers
    const lo = viewMinSec * 1000, hi = viewMaxSec * 1000;
    _viewReqs = requestLog.filter(r => r.ts >= lo && r.ts <= hi);
  } else {
    _stats = latest;
    _viewReqs = requestLog;
  }

  $('uptime').textContent = '↑ ' + formatDuration(Date.now() - startedAt);

  renderAllCharts();

  if (globalEndpoint) {
    updateStatsForEndpoint(_stats);
  } else {
    updateStatsGlobal(_stats);
  }

  updateLatencyStats();
  {
    // Bytes and coalesced use cumulative deltas (no request log equivalent).
    // When zoomed and _viewReqs is empty, show 0 to stay consistent.
    const visReqs = viewMode === 'zoomed' ? countVisibleRequests() : (_stats.hits + _stats.misses);
    let displayBytes = visReqs > 0 ? _stats.bytes : 0;
    if (globalSourceFilter === 'cache' && visReqs > 0) displayBytes = Math.round(_stats.bytes * (_stats.hits / Math.max(1, _stats.hits + _stats.misses)));
    else if (globalSourceFilter === 'upstream' && visReqs > 0) displayBytes = Math.round(_stats.bytes * (_stats.misses / Math.max(1, _stats.hits + _stats.misses)));
    $('vBytes').textContent = formatBytes(displayBytes);
    $('sBytes').textContent = formatBytes(lastRate('bytes')) + '/s';
  }
  $('vCoal').textContent = commafy(viewMode === 'zoomed' && countVisibleRequests() === 0 ? 0 : _stats.coalesced);
  const totalRequests = viewMode === 'zoomed' ? countVisibleRequests() : (_stats.hits + _stats.misses);
  $('sCoal').textContent = totalRequests > 0
    ? formatPercent(_stats.coalesced / Math.max(1, _stats.hits + _stats.misses)) + ' of requests' : '--';

  populateEndpointSelect();
  updateEndpoints();
  updateInsights();
  updateRequestLog();
}

/** Count requests in the view, applying global source/endpoint filters. */
function countVisibleRequests() {
  let reqs = _viewReqs;
  if (globalSourceFilter) reqs = reqs.filter(r => r.source === globalSourceFilter);
  if (globalEndpoint) reqs = reqs.filter(r => r.endpoint === globalEndpoint);
  return reqs.length;
}

function updateStatsGlobal(stats) {
  let totalRequests, reqRate;
  if (viewMode === 'zoomed') {
    // When zoomed, use request log count (authoritative for individual events)
    // instead of cumulative snapshot deltas which can desync due to sub-second
    // timing between request timestamps and 1-second snapshot intervals.
    totalRequests = countVisibleRequests();
  } else if (globalSourceFilter === 'cache') {
    totalRequests = stats.hits;
  } else if (globalSourceFilter === 'upstream') {
    totalRequests = stats.misses;
  } else {
    totalRequests = stats.hits + stats.misses;
  }
  if (globalSourceFilter === 'cache') {
    reqRate = lastRate('hits');
  } else if (globalSourceFilter === 'upstream') {
    reqRate = lastRate('misses');
  } else {
    reqRate = lastRate('hits') + lastRate('misses');
  }
  $('vReqs').textContent = commafy(totalRequests);
  $('sReqs').textContent = formatRate(reqRate) + '/s';

  const codes = getViewResponseCodes();
  const errorCount = Object.entries(codes).filter(([c]) => +c >= 400).reduce((a, [, n]) => a + n, 0) + (stats.fetchErrors || 0);
  const errParts = [];
  for (const [code, count] of Object.entries(codes).sort((a, b) => b[1] - a[1])) {
    if (+code >= 400 && count > 0) errParts.push(count + 'x ' + code);
  }
  if (stats.fetchErrors) errParts.push(stats.fetchErrors + ' fetch');
  $('sErr').textContent = errParts.length ? errParts.join(', ') : 'none';

  {
    const allResponses = Object.values(codes).reduce((a, b) => a + b, 0) + (viewMode === 'zoomed' ? 0 : (stats.fetchErrors || 0));
    const errorPct = allResponses > 0 ? errorCount / allResponses : 0;
    $('vErr').textContent = allResponses > 0 ? formatPercent(errorPct) : '--';
    $('vErr').className = 'stat-num ' + (errorPct < 0.01 ? 'c-green' : errorPct < 0.05 ? 'c-amber' : 'c-accent');
  }

  if (globalSourceFilter === 'cache') {
    $('vHit').textContent = '100%';
    $('sHit').textContent = 'showing cached only';
    $('vHit').className = 'stat-num c-green';
  } else if (globalSourceFilter === 'upstream') {
    $('vHit').textContent = '0%';
    $('sHit').textContent = 'showing upstream only';
    $('vHit').className = 'stat-num c-accent';
  } else if (viewMode === 'zoomed') {
    // When zoomed, derive from request log (same source as request count)
    let cacheHits = 0, totalHits = 0;
    for (const r of _viewReqs) {
      totalHits++;
      if (r.source === 'cache') cacheHits++;
    }
    const hr = totalHits > 0 ? cacheHits / totalHits : 0;
    $('vHit').textContent = totalHits > 0 ? formatPercent(hr) : '--';
    $('sHit').textContent = commafy(cacheHits) + ' / ' + commafy(totalHits);
    $('vHit').className = 'stat-num ' + (hr >= 0.7 ? 'c-green' : hr >= 0.4 ? 'c-amber' : 'c-accent');
  } else {
    $('vHit').textContent = formatPercent(stats.hitRate);
    $('sHit').textContent = commafy(stats.hits) + ' / ' + commafy(stats.hits + stats.misses);
    $('vHit').className = 'stat-num ' + (stats.hitRate >= 0.7 ? 'c-green' : stats.hitRate >= 0.4 ? 'c-amber' : 'c-accent');
  }

  // Sparklines: use _filtered for consistency with charts
  const [, hitRates] = computeRates('hits');
  const [, missRates] = computeRates('misses');
  if (globalSourceFilter === 'cache') {
    $('skReqs').setAttribute('points', sparklinePoints(hitRates.map(v => ({ y: v })), 80, 22));
  } else if (globalSourceFilter === 'upstream') {
    $('skReqs').setAttribute('points', sparklinePoints(missRates.map(v => ({ y: v })), 80, 22));
  } else {
    $('skReqs').setAttribute('points', sparklinePoints(
      hitRates.map((v, i) => ({ y: v + (missRates[i] || 0) })), 80, 22));
  }
  const [, errRates] = errorRatesSeries();
  $('skErr').setAttribute('points', sparklinePoints(errRates.map(v => ({ y: v })), 80, 22));
  // Hit rate sparkline uses _visible (strict view range) so it matches stat cards
  $('skHit').setAttribute('points', sparklinePoints(_visible.map(s => ({ y: s.hitRate * 100 })), 80, 22));
  $('skLat').setAttribute('points', sparklinePoints(latencySeriesData()[1].map(v => ({ y: v })), 80, 22));
  $('trBadge').textContent = formatRate(lastRate('hits') + lastRate('upstream')) + ' req/s';
}

function updateStatsForEndpoint(stats) {
  // When zoomed, derive everything from request log for this endpoint
  let epHits, epMisses, epTotal, epErrs;
  if (viewMode === 'zoomed') {
    epHits = 0; epMisses = 0; epErrs = {};
    for (const r of _viewReqs) {
      if (r.endpoint !== globalEndpoint) continue;
      if (globalSourceFilter && r.source !== globalSourceFilter) continue;
      if (r.source === 'cache') epHits++;
      else epMisses++;
      if (r.status >= 400) epErrs[r.status] = (epErrs[r.status] || 0) + 1;
    }
    epTotal = epHits + epMisses;
  } else {
    epHits = (stats.epHits || {})[globalEndpoint] || 0;
    epMisses = (stats.epMisses || {})[globalEndpoint] || 0;
    epTotal = epHits + epMisses;
    epErrs = endpointResponseCodes(globalEndpoint, stats);
  }

  $('vReqs').textContent = commafy(epTotal);

  // Rate from cumulative snapshots (ok for rate display)
  if (_visible.length >= 2) {
    const first = _visible[0];
    const last = _visible[_visible.length - 1];
    const dt = (last.ts - first.ts) / 1000;
    const firstH = (first.epHits || {})[globalEndpoint] || 0;
    const firstM = (first.epMisses || {})[globalEndpoint] || 0;
    const lastH = (last.epHits || {})[globalEndpoint] || 0;
    const lastM = (last.epMisses || {})[globalEndpoint] || 0;
    let delta;
    if (globalSourceFilter === 'cache') delta = lastH - firstH;
    else if (globalSourceFilter === 'upstream') delta = lastM - firstM;
    else delta = (lastH + lastM) - (firstH + firstM);
    $('sReqs').textContent = dt > 0 ? formatRate(Math.max(0, delta / dt)) + '/s' : '-';
  } else {
    $('sReqs').textContent = '-';
  }

  const epErrCount = Object.values(epErrs).reduce((a, b) => a + b, 0);
  const epErrRate = epTotal > 0 ? epErrCount / epTotal : 0;
  $('vErr').textContent = epTotal > 0 ? formatPercent(epErrRate) : '--';
  $('sErr').textContent = Object.entries(epErrs).map(([c, n]) => n + 'x ' + c).join(', ') || 'none';
  $('vErr').className = 'stat-num ' + (epErrRate < 0.01 ? 'c-green' : epErrRate < 0.05 ? 'c-amber' : 'c-accent');

  if (globalSourceFilter === 'cache') {
    $('vHit').textContent = '100%';
    $('sHit').textContent = 'showing cached only';
    $('vHit').className = 'stat-num c-green';
  } else if (globalSourceFilter === 'upstream') {
    $('vHit').textContent = '0%';
    $('sHit').textContent = 'showing upstream only';
    $('vHit').className = 'stat-num c-accent';
  } else {
    const epRate = epTotal > 0 ? epHits / epTotal : 0;
    $('vHit').textContent = epTotal > 0 ? formatPercent(epRate) : '--';
    $('sHit').textContent = commafy(epHits) + ' / ' + commafy(epTotal);
    $('vHit').className = 'stat-num ' + (epRate >= 0.7 ? 'c-green' : epRate >= 0.4 ? 'c-amber' : 'c-accent');
  }

  const [, hitRates] = endpointMapRates('epHits', globalEndpoint);
  const [, missRates] = endpointMapRates('epMisses', globalEndpoint);
  if (globalSourceFilter === 'cache') {
    $('skReqs').setAttribute('points', sparklinePoints(hitRates.map(v => ({ y: v })), 80, 22));
  } else if (globalSourceFilter === 'upstream') {
    $('skReqs').setAttribute('points', sparklinePoints(missRates.map(v => ({ y: v })), 80, 22));
  } else {
    $('skReqs').setAttribute('points', sparklinePoints(
      hitRates.map((v, i) => ({ y: v + (missRates[i] || 0) })), 80, 22));
  }
  const [, epErrRates] = endpointErrorRatesSeries(globalEndpoint);
  $('skErr').setAttribute('points', sparklinePoints(epErrRates.map(v => ({ y: v })), 80, 22));
  const [, epHitRates] = endpointHitRateSeries(globalEndpoint);
  $('skHit').setAttribute('points', sparklinePoints(epHitRates.map(v => ({ y: v })), 80, 22));
  $('skLat').setAttribute('points', sparklinePoints(latencySeriesData()[1].map(v => ({ y: v })), 80, 22));
  $('trBadge').textContent = globalEndpoint;
}

function updateLatencyStats() {
  if (snapshots.length < 2) return;
  const curr = snapshots[snapshots.length - 1];
  let prev = null;
  for (let i = snapshots.length - 2; i >= 0; i--) {
    if (curr.ts - snapshots[i].ts >= 30000) { prev = snapshots[i]; break; }
  }
  if (!prev) prev = snapshots[0];
  const deltaUs = curr.latencyUsTotal - prev.latencyUsTotal;
  const deltaCount = curr.latencyCount - prev.latencyCount;
  if (deltaCount > 0) {
    const ms = (deltaUs / deltaCount) / 1000;
    $('vLat').textContent = formatLatency(ms * 1000);
    $('vLat').className = 'stat-num ' + (ms < 200 ? 'c-green' : ms < 1000 ? 'c-amber' : 'c-accent');
    $('sLat').textContent = commafy(deltaCount) + ' upstream reqs';
  } else {
    $('vLat').textContent = '--';
    $('sLat').textContent = 'no upstream traffic';
  }
}

// ── Chart rendering ──

function renderAllCharts() {
  renderTrafficChart();
  renderLatencyPctChart();
  renderLatCompareChart();
  renderErrTimelineChart();
  renderHitRateChart();
  renderThroughputChart();
  renderEdgeCasesChart();
  renderAdmissionChart();
  renderResponseCodesChart();
}

function renderTrafficChart() {
  const rateLabel = (u, vals) => vals.map(v => formatRate(v) + '/s');
  if (globalEndpoint) {
    const [ts, hits] = endpointMapRates('epHits', globalEndpoint);
    const [, misses] = endpointMapRates('epMisses', globalEndpoint);
    const [, errors] = endpointErrorRatesSeries(globalEndpoint);
    const showHits = globalSourceFilter !== 'upstream';
    const showUpstream = globalSourceFilter !== 'cache';
    renderUPlot('chartTraffic',
      [{ label: 'Hits', color: COLORS.green }, { label: 'Misses', color: COLORS.blue },
       { label: 'Stale', color: COLORS.amber }, { label: 'Errors', color: COLORS.red }],
      [ts, showHits ? hits : nullify(hits), showUpstream ? misses : nullify(misses),
       nullify(ts), showUpstream ? errors : nullify(errors)],
      { yValues: rateLabel });
  } else {
    const [ts, hits] = computeRates('hits');
    const [, upstream] = computeRates('upstream');
    const [, stale] = computeRates('stale');
    const [, errors] = errorRatesSeries();
    const showHits = globalSourceFilter !== 'upstream';
    const showUpstream = globalSourceFilter !== 'cache';
    renderUPlot('chartTraffic',
      [{ label: 'Cache Hits', color: COLORS.green }, { label: 'Upstream', color: COLORS.blue },
       { label: 'Stale', color: COLORS.amber }, { label: 'Errors', color: COLORS.red }],
      [ts, showHits ? hits : nullify(hits), showUpstream ? upstream : nullify(upstream),
       showUpstream ? stale : nullify(stale), showUpstream ? errors : nullify(errors)],
      { yValues: rateLabel });
  }
}

function renderLatencyPctChart() {
  const msLabel = (u, vals) => {
    const range = Math.max(...vals) - Math.min(...vals);
    const dec = range < 2 ? 1 : 0;
    return vals.map(v => v.toFixed(dec) + 'ms');
  };
  const [ts, p50, p95, p99] = latencyPercentileSeries();
  renderUPlot('chartLatencyPct',
    [{ label: 'p50', color: COLORS.green }, { label: 'p95', color: COLORS.amber }, { label: 'p99', color: COLORS.red }],
    [ts, p50, p95, p99],
    { yValues: msLabel });
  // Update legend values
  const latest = snapshots[snapshots.length - 1];
  if (latest) {
    $('latP50').textContent = latest.latP50 ? formatLatency(latest.latP50) : '--';
    $('latP95').textContent = latest.latP95 ? formatLatency(latest.latP95) : '--';
    $('latP99').textContent = latest.latP99 ? formatLatency(latest.latP99) : '--';
  }
}

function renderLatCompareChart() {
  const [ts, cacheMs, upMs] = cacheVsUpstreamLatency();
  const msLabel = (u, vals) => vals.map(v => v < 1 ? v.toFixed(2) + 'ms' : v.toFixed(1) + 'ms');
  renderUPlot('chartLatCompare',
    [{ label: 'Cache', color: COLORS.green }, { label: 'Upstream', color: COLORS.blue }],
    [ts, cacheMs, upMs],
    { yValues: msLabel });
}

function renderErrTimelineChart() {
  const { ts, series } = errorTimelineSeries();
  const codeColors = { 429: COLORS.amber, 401: '#e06c75', 403: '#e06c75', 500: COLORS.red, 502: COLORS.red, 503: COLORS.red, fetch: COLORS.purple };
  const codes = Object.keys(series).sort();
  if (codes.length === 0) {
    // Render empty chart with a placeholder series
    const [rts] = computeRates('hits');
    renderUPlot('chartErrTimeline',
      [{ label: 'No errors', color: '#888889' }],
      [rts, rts.map(() => 0)],
      { yValues: (u, vals) => vals.map(v => formatRate(v) + '/s') });
    return;
  }
  const seriesDefs = codes.map(c => ({
    label: String(c), color: codeColors[c] || COLORS.red,
  }));
  const data = [ts, ...codes.map(c => series[c])];
  renderUPlot('chartErrTimeline', seriesDefs, data,
    { yValues: (u, vals) => vals.map(v => formatRate(v) + '/s') });
}

function renderHitRateChart() {
  if (globalEndpoint) {
    const [ts, vals] = endpointHitRateSeries(globalEndpoint);
    renderUPlot('chartHitRate',
      [{ label: globalEndpoint, color: COLORS.green }],
      [ts, vals],
      { yMin: 0, yMax: 100, yValues: (u, vals) => vals.map(v => v.toFixed(0) + '%') });
  } else {
    const snaps = _visible;
    const ts = snaps.map(s => s.ts / 1000);
    const vals = snaps.map(s => s.hitRate * 100);
    renderUPlot('chartHitRate',
      [{ label: 'Hit Rate', color: COLORS.green }],
      [ts, vals],
      { yMin: 0, yMax: 100, yValues: (u, vals) => vals.map(v => v.toFixed(0) + '%') });
  }
}

function renderThroughputChart() {
  const rateLabel = (u, vals) => vals.map(v => formatRate(v));
  if (globalEndpoint) {
    const [ts, hits] = endpointMapRates('epHits', globalEndpoint);
    const [, misses] = endpointMapRates('epMisses', globalEndpoint);
    const showHits = globalSourceFilter !== 'upstream';
    const showUp = globalSourceFilter !== 'cache';
    const combined = hits.map((v, i) => (showHits ? v : 0) + (showUp ? (misses[i] || 0) : 0));
    renderUPlot('chartThruput', [{ label: globalEndpoint + ' req/s', color: COLORS.blue }],
      [ts, combined], { yValues: rateLabel });
  } else {
    const [ts, hitRates] = computeRates('hits');
    const [, upRates] = computeRates('upstream');
    const showHits = globalSourceFilter !== 'upstream';
    const showUp = globalSourceFilter !== 'cache';
    const combined = hitRates.map((v, i) => (showHits ? v : 0) + (showUp ? (upRates[i] || 0) : 0));
    renderUPlot('chartThruput', [{ label: 'Req/s', color: COLORS.blue }],
      [ts, combined], { yValues: rateLabel });
  }
}

function renderEdgeCasesChart() {
  const [ts, coalesced] = computeRates('coalesced');
  const [, stale] = computeRates('stale');
  const [, negative] = computeRates('negative');
  const [, errors] = computeRates('errors');
  const nullify = arr => arr.map(() => null);
  const showCache = globalSourceFilter !== 'upstream';
  const showUpstream = globalSourceFilter !== 'cache';
  renderUPlot('chartEdge',
    [{ label: 'Coalesced', color: COLORS.purple }, { label: 'Stale', color: COLORS.amber },
     { label: 'Negative', color: COLORS.cyan }, { label: 'Errors', color: COLORS.red }],
    [ts, showCache ? coalesced : nullify(coalesced), stale,
     showCache ? negative : nullify(negative), showUpstream ? errors : nullify(errors)],
    { yValues: (u, vals) => vals.map(v => formatRate(v) + '/s') });
}

function renderAdmissionChart() {
  if (!_stats) return;
  // Zero out when zoomed and no requests in the view time range
  const noReqs = viewMode === 'zoomed' && _viewReqs.length === 0;
  drawDonut('chartAdmission', [
    { label: 'Accepted', value: noReqs ? 0 : _stats.admAccepted, color: COLORS.green },
    { label: 'Rejected', value: noReqs ? 0 : _stats.admRejected, color: COLORS.red },
  ]);
  if (!donutInitialized) { donutInitialized = true; initDonutHover('chartAdmission'); }
}

/** Get response code counts for the current view, derived from request log when zoomed. */
function getViewResponseCodes() {
  if (viewMode !== 'zoomed') return _stats.responseCodes || {};
  const codes = {};
  for (const r of _viewReqs) {
    if (!globalSourceFilter || r.source === globalSourceFilter) {
      if (!globalEndpoint || r.endpoint === globalEndpoint) {
        codes[r.status] = (codes[r.status] || 0) + 1;
      }
    }
  }
  return codes;
}

function renderResponseCodesChart() {
  if (!_stats) return;

  const codeColors = { 2: COLORS.green, 3: COLORS.cyan, 4: COLORS.amber, 5: COLORS.red };
  const bars = [];
  const codes = getViewResponseCodes();

  if (globalEndpoint && viewMode !== 'zoomed') {
    const epCodes = endpointResponseCodes(globalEndpoint, _stats);
    const errorTotal = Object.values(epCodes).reduce((a, b) => a + b, 0);
    const epHits = (_stats.epHits || {})[globalEndpoint] || 0;
    const epMisses = (_stats.epMisses || {})[globalEndpoint] || 0;
    const successCount = Math.max(0, epHits + epMisses - errorTotal);
    if (successCount > 0) bars.push({ label: '2xx', value: successCount, color: codeColors[2] });
    for (const [code, count] of Object.entries(epCodes).sort((a, b) => b[1] - a[1])) {
      bars.push({ label: code, value: count, color: codeColors[Math.floor(+code / 100)] || COLORS.red });
    }
    if (!bars.length) bars.push({ label: 'none', value: 0, color: '#888889' });
  } else {
    const grouped = {};
    for (const [code, count] of Object.entries(codes)) {
      if (count <= 0) continue;
      const cls = Math.floor(+code / 100);
      if (!grouped[cls]) grouped[cls] = {};
      grouped[cls][code] = count;
    }
    if (grouped[2]) { const v = Object.values(grouped[2]).reduce((a, b) => a + b, 0); if (v > 0) bars.push({ label: '2xx', value: v, color: codeColors[2] }); }
    if (grouped[3]) { const v = Object.values(grouped[3]).reduce((a, b) => a + b, 0); if (v > 0) bars.push({ label: '3xx', value: v, color: codeColors[3] }); }
    if (grouped[4]) for (const [c, n] of Object.entries(grouped[4]).sort((a, b) => b[1] - a[1])) if (n > 0) bars.push({ label: c, value: n, color: codeColors[4] });
    if (grouped[5]) for (const [c, n] of Object.entries(grouped[5]).sort((a, b) => b[1] - a[1])) if (n > 0) bars.push({ label: c, value: n, color: codeColors[5] });
    if (!bars.length && viewMode !== 'zoomed' && _stats.fetchErrors > 0) bars.push({ label: 'fetch', value: _stats.fetchErrors, color: COLORS.red });
  }

  const errorTotal = bars.filter(b => !b.label.endsWith('xx') || +b.label[0] >= 4).reduce((a, b) => a + b.value, 0);
  $('rcBadge').textContent = errorTotal > 0 ? errorTotal + ' errors' : (bars.length && bars[0].value > 0 ? 'all ok' : '--');
  drawBarChart('chartCodes', bars);
  initBarHover('chartCodes');
}

// ── Endpoint table ──

let endpointSort = { col: 'total', asc: false };
let endpointFilterText = '';

function updateEndpoints() {
  let endpoints = getEndpoints();
  $('epCount').textContent = endpoints.length;

  if (endpointFilterText) {
    const query = endpointFilterText.toLowerCase();
    endpoints = endpoints.filter(e => e.name.toLowerCase().includes(query));
  }
  if (globalEndpoint) {
    endpoints = endpoints.filter(e => e.name === globalEndpoint);
  }

  endpoints.sort((a, b) => {
    const va = a[endpointSort.col], vb = b[endpointSort.col];
    if (typeof va === 'string') return endpointSort.asc ? va.localeCompare(vb) : -va.localeCompare(vb);
    return endpointSort.asc ? va - vb : vb - va;
  });

  $('epBody').innerHTML = endpoints.map(ep => {
    const rateColor = ep.rate >= 0.7 ? COLORS.green : ep.rate >= 0.4 ? COLORS.amber : ep.total > 0 ? COLORS.red : '#888889';
    const errColor = ep.errors > 0 ? COLORS.red : '#888889';
    const isOpen = expandedEndpoints.has(ep.name);

    return `<tr class="ep-row" data-ep="${ep.name}">
      <td class="ep-name"><a href="#" class="ep-link" data-ep="${ep.name}">${ep.name}</a></td>
      <td class="num">${commafy(ep.hits)}</td>
      <td class="num">${commafy(ep.misses)}</td>
      <td class="num">${commafy(ep.total)}</td>
      <td><div class="rate-bar">
        <div class="rate-track"><div class="rate-fill" style="width:${(ep.rate * 100).toFixed(1)}%;background:${rateColor}"></div></div>
        <span style="color:${rateColor}">${ep.total > 0 ? formatPercent(ep.rate) : '--'}</span>
      </div></td>
      <td class="num" style="color:${errColor}">${ep.errors > 0 ? commafy(ep.errors) : '-'}</td>
      <td class="num">${ep.avgLat > 0 ? formatLatency(ep.avgLat * 1000) : '-'}</td>
      <td class="num">${ep.bytes > 0 ? formatBytes(ep.bytes) : '-'}</td>
      <td>${ep.ttl}</td>
    </tr>
    <tr class="ep-detail${isOpen ? ' open' : ''}" data-ep="${ep.name}"><td colspan="9">${
      ep.errDetail.length
        ? ep.errDetail.map(d => `<span class="pill-tag pill-err">${d}</span>`).join('')
        : '<span class="pill-tag pill-ok">No errors</span>'
    }</td></tr>`;
  }).join('');
}

$('epFilter').addEventListener('input', e => { endpointFilterText = e.target.value; updateEndpoints(); });

$('epHead').addEventListener('click', e => {
  const th = e.target.closest('th');
  if (!th) return;
  const col = th.dataset.col;
  if (!col) return;
  if (endpointSort.col === col) endpointSort.asc = !endpointSort.asc;
  else { endpointSort.col = col; endpointSort.asc = col === 'name'; }
  $('epHead').querySelectorAll('th').forEach(t => {
    t.classList.toggle('sorted', t.dataset.col === col);
    const arrow = t.querySelector('.arrow');
    if (arrow) arrow.textContent = t.dataset.col === col ? (endpointSort.asc ? '▲' : '▼') : '';
  });
  updateEndpoints();
});

$('epBody').addEventListener('click', e => {
  const link = e.target.closest('.ep-link');
  if (link) { e.preventDefault(); setGlobalEndpoint(link.dataset.ep); return; }
  const row = e.target.closest('tr.ep-row');
  if (!row) return;
  const ep = row.dataset.ep;
  if (expandedEndpoints.has(ep)) expandedEndpoints.delete(ep);
  else expandedEndpoints.add(ep);
  updateEndpoints();
});

// ── Request log ──

function updateRequestLog() {
  let filtered = _viewReqs;
  const srcFilter = requestSourceFilter || globalSourceFilter;
  if (srcFilter) filtered = filtered.filter(r => r.source === srcFilter);
  if (globalEndpoint) filtered = filtered.filter(r => r.endpoint === globalEndpoint);
  if (requestStatusFilter) {
    if (requestStatusFilter.length === 1) {
      const cls = +requestStatusFilter;
      filtered = filtered.filter(r => Math.floor(r.status / 100) === cls);
    } else {
      filtered = filtered.filter(r => r.status === +requestStatusFilter);
    }
  }
  if (requestSearchText) {
    const q = requestSearchText.toLowerCase();
    filtered = filtered.filter(r =>
      (r.endpoint || '').toLowerCase().includes(q) ||
      String(r.status).includes(q) ||
      (r.source || '').toLowerCase().includes(q));
  }

  if (requestSort) {
    filtered = [...filtered].sort((a, b) => {
      const va = a[requestSort.col], vb = b[requestSort.col];
      if (typeof va === 'string') return requestSort.asc ? va.localeCompare(vb) : -va.localeCompare(vb);
      return requestSort.asc ? va - vb : vb - va;
    });
  } else {
    filtered = filtered.slice().reverse();
  }

  const chips = [];
  if (requestSort) chips.push(`<span class="chip chip-warn">sorted: ${requestSort.col} <span class="x" data-act="clrSort">x</span></span>`);
  $('reqChips').innerHTML = chips.join(' ');

  const body = $('reqBody');
  if (!filtered.length) {
    body.innerHTML = '<tr><td colspan="5" class="req-empty">No requests</td></tr>';
    $('reqBadge').textContent = '0';
    return;
  }

  body.innerHTML = filtered.slice(0, 100).map(req => {
    const statusColor = req.status >= 500 ? COLORS.red : req.status >= 400 ? COLORS.amber : COLORS.green;
    const sourceColor = req.source === 'cache' ? COLORS.green
      : req.source === 'stale' ? COLORS.amber
      : req.source === 'error' ? COLORS.red
      : COLORS.blue;
    return `<tr>
      <td>${formatTimeFull(req.ts)}</td>
      <td class="ep-name"><a href="#" class="ep-link" data-ep="${req.endpoint || ''}">${req.endpoint || '-'}</a></td>
      <td style="color:${statusColor};font-weight:600">${req.status}</td>
      <td style="color:${sourceColor}">${req.source}</td>
      <td class="num">${formatLatency(req.latencyUs)}</td>
    </tr>`;
  }).join('');

  $('reqBadge').textContent = filtered.length;
  if (!requestSort && !requestScrollPaused) $('reqScroll').scrollTop = 0;
}

$('reqSource').addEventListener('click', e => {
  const btn = e.target.closest('button');
  if (!btn) return;
  requestSourceFilter = btn.dataset.s;
  $('reqSource').querySelectorAll('button').forEach(b => b.classList.toggle('on', b.dataset.s === requestSourceFilter));
  updateRequestLog();
});

$('reqStatusFilter').addEventListener('change', e => {
  requestStatusFilter = e.target.value;
  updateRequestLog();
});

$('reqSearchInput').addEventListener('input', e => {
  requestSearchText = e.target.value;
  updateRequestLog();
});

$('reqHead').addEventListener('click', e => {
  const th = e.target.closest('th');
  if (!th) return;
  const col = th.dataset.c;
  if (!col) return;
  if (requestSort && requestSort.col === col) {
    requestSortClicks++;
    if (requestSortClicks >= 3) { requestSort = null; requestSortClicks = 0; }
    else { requestSort.asc = !requestSort.asc; }
  } else {
    requestSort = { col, asc: col === 'ts' || col === 'endpoint' };
    requestSortClicks = 1;
  }
  $('reqHead').querySelectorAll('th').forEach(t => {
    t.classList.toggle('sorted', requestSort && t.dataset.c === requestSort.col);
    const arrow = t.querySelector('.arrow');
    if (arrow) arrow.textContent = (requestSort && t.dataset.c === requestSort.col) ? (requestSort.asc ? '▲' : '▼') : '';
  });
  updateRequestLog();
});

$('reqChips').addEventListener('click', e => {
  const x = e.target.closest('.x');
  if (!x) return;
  if (x.dataset.act === 'clrSort') {
    requestSort = null;
    requestSortClicks = 0;
    $('reqHead').querySelectorAll('th').forEach(t => {
      t.classList.remove('sorted');
      const arrow = t.querySelector('.arrow');
      if (arrow) arrow.textContent = '';
    });
  }
  updateRequestLog();
});

$('reqScroll').addEventListener('mouseenter', () => requestScrollPaused = true);
$('reqScroll').addEventListener('mouseleave', () => requestScrollPaused = false);

$('reqBody').addEventListener('click', e => {
  const link = e.target.closest('.ep-link');
  if (!link) return;
  e.preventDefault();
  const ep = link.dataset.ep;
  if (ep) setGlobalEndpoint(ep);
});

// ── Insights ──

function updateInsights() {
  const latest = snapshots[snapshots.length - 1];
  if (!latest) return;

  const insights = [];
  const totalRequests = latest.hits + latest.misses;

  if (snapshots.length >= 2) {
    const prev = snapshots[snapshots.length - 2];
    const deltaUs = latest.latencyUsTotal - prev.latencyUsTotal;
    const deltaCount = latest.latencyCount - prev.latencyCount;
    if (deltaCount > 0) {
      const ms = (deltaUs / deltaCount) / 1000;
      if (ms > 2000) insights.push({ type: 'w', cat: 'health', title: 'Latency', text: `Avg upstream latency is ${(ms / 1000).toFixed(1)}s` });
      else if (ms > 500) insights.push({ type: 'i', cat: 'health', title: 'Latency', text: `Avg upstream latency is ${ms.toFixed(0)}ms` });
    }
  }

  const codes = latest.responseCodes || {};
  if (codes[429]) insights.push({ type: 'w', cat: 'health', title: 'Rate Limiting', text: `${commafy(codes[429])} rate-limited responses (429), account may be throttled` });
  if (codes[401] || codes[403]) {
    const parts = [];
    if (codes[401]) parts.push(codes[401] + 'x 401');
    if (codes[403]) parts.push(codes[403] + 'x 403');
    insights.push({ type: 'w', cat: 'health', title: 'Authentication', text: `${commafy((codes[401] || 0) + (codes[403] || 0))} auth errors (${parts.join(', ')}), session may need refresh` });
  }
  const serverErrors = Object.entries(codes).filter(([c]) => +c >= 500).reduce((a, [, n]) => a + n, 0);
  if (serverErrors) insights.push({ type: 'w', cat: 'health', title: 'Server Errors', text: `${commafy(serverErrors)} server errors (5xx) from upstream` });
  if (latest.fetchErrors) insights.push({ type: 'w', cat: 'health', title: 'Fetch Failures', text: `${commafy(latest.fetchErrors)} fetch failures (network/TLS/timeout)` });

  if (totalRequests > 0) {
    if (latest.hitRate >= 0.8) insights.push({ type: 'g', cat: 'cache', title: 'Hit Rate', text: `Cache hit rate is ${formatPercent(latest.hitRate)}, excellent` });
    else if (latest.hitRate >= 0.5) insights.push({ type: 'i', cat: 'cache', title: 'Hit Rate', text: `Cache hit rate is ${formatPercent(latest.hitRate)}, room for improvement` });
    else insights.push({ type: 'w', cat: 'cache', title: 'Hit Rate', text: `Cache hit rate is ${formatPercent(latest.hitRate)}, consider adjusting TTLs or thresholds` });
  }

  if (totalRequests > 0 && latest.coalesced > 0 && latest.coalesced / totalRequests >= 0.05) {
    insights.push({ type: 'g', cat: 'cache', title: 'Singleflight', text: `Singleflight coalesced ${formatPercent(latest.coalesced / totalRequests)} of requests` });
  }
  if (latest.avoided) insights.push({ type: 'g', cat: 'cache', title: 'Cache Savings', text: `${commafy(latest.avoided)} upstream requests avoided by cache` });
  if (latest.bytes) insights.push({ type: 'g', cat: 'cache', title: 'Bandwidth', text: `${formatBytes(latest.bytes)} served from cache` });
  if (latest.stale) insights.push({ type: 'w', cat: 'cache', title: 'Stale Data', text: `${commafy(latest.stale)} stale responses served, upstream may be having issues` });

  const admTotal = latest.admAccepted + latest.admRejected;
  if (admTotal > 0) {
    const rejPct = latest.admRejected / admTotal;
    if (rejPct > 0.5) insights.push({ type: 'w', cat: 'cache', title: 'Admission', text: `${formatPercent(rejPct)} rejected by popularity filter, threshold may be too high` });
    else if (latest.admRejected) insights.push({ type: 'i', cat: 'cache', title: 'Admission', text: `Popularity filter: ${formatPercent(1 - rejPct)} accepted, ${formatPercent(rejPct)} rejected` });
  }

  const endpoints = getEndpoints().filter(e => e.total >= 10);
  if (endpoints.length >= 2) {
    const best = endpoints.reduce((a, b) => a.rate > b.rate ? a : b);
    const worst = endpoints.reduce((a, b) => a.rate < b.rate ? a : b);
    if (best.rate > 0.5) insights.push({ type: 'g', cat: 'endpoints', title: 'Best Endpoint', text: `${best.name}: best hit rate at ${formatPercent(best.rate)} (${commafy(best.total)} reqs)` });
    if (worst.rate < 0.5 && worst.name !== best.name) insights.push({ type: 'w', cat: 'endpoints', title: 'Worst Endpoint', text: `${worst.name}: lowest hit rate at ${formatPercent(worst.rate)} (${commafy(worst.total)} reqs)` });
  }

  const icons = { g: '✓', w: '⚠', i: 'ℹ' };
  const classes = { g: 'insight-good', w: 'insight-warn', i: 'insight-info' };

  if (!insights.length) {
    $('insightsGrid').innerHTML = '<div class="insight insight-info"><span class="insight-dot">ℹ</span><div><div class="insight-title">Status</div><div class="insight-text">Collecting data...</div></div></div>';
    return;
  }

  $('insightsGrid').innerHTML = insights.map(ins =>
    `<div class="insight ${classes[ins.type]}"><span class="insight-dot">${icons[ins.type]}</span><div><div class="insight-title">${ins.title}</div><div class="insight-text">${ins.text}</div></div></div>`
  ).join('');
}

// ── Config display ──

function showConfig() {
  const cfg = cacheConfig;
  const yesNo = v => v ? '<span class="config-on">&#10003; enabled</span>' : '<span class="config-off">&#10007; disabled</span>';

  const mainItems = [
    { label: 'Cache', value: yesNo(cfg.enabled) },
    { label: 'Default TTL', value: cfg.defaultTTL || '-' },
    { label: 'Popularity Threshold', value: (cfg.popularityThreshold ?? '-') + ' req' },
    { label: 'Popularity Window', value: cfg.popularityWindow || '-' },
    { label: 'Stale-if-error', value: yesNo(cfg.staleIfError) },
    { label: 'Negative Caching', value: yesNo(cfg.negativeCaching) },
    { label: 'Negative TTL', value: cfg.negativeTTL || '-' },
    { label: 'Negative Threshold', value: (cfg.negativeThreshold ?? '-') + ' req' },
  ];
  $('cfgGrid').innerHTML = mainItems.map(item =>
    `<div class="config-row"><span class="config-label">${item.label}</span><span class="config-val">${item.value}</span></div>`
  ).join('');

  const ttls = cfg.endpointTTLs || {};
  const thresholds = cfg.endpointThresholds || {};
  const allEps = [...new Set([...Object.keys(ttls), ...Object.keys(thresholds)])].sort();
  const defaultThreshold = cfg.popularityThreshold ?? '-';

  const rows = allEps.map(ep => ({
    name: ep,
    ttl: ttls[ep] || cfg.defaultTTL || '-',
    threshold: thresholds[ep] ?? defaultThreshold,
  }));
  rows.sort((a, b) => {
    const va = a[_epCfgSort.col], vb = b[_epCfgSort.col];
    if (typeof va === 'number' && typeof vb === 'number') return _epCfgSort.asc ? va - vb : vb - va;
    return _epCfgSort.asc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
  });

  $('epCfgBody').innerHTML = rows.map(r =>
    `<tr><td class="ep-name">${r.name}</td><td>${r.ttl}</td><td>${r.threshold}</td></tr>`
  ).join('') || '<tr><td colspan="3" style="color:var(--text-dim);text-align:center;padding:12px">No endpoint overrides</td></tr>';
}

let _epCfgSort = { col: 'name', asc: true };
$('epCfgHead').addEventListener('click', e => {
  const th = e.target.closest('th');
  if (!th || !th.dataset.col) return;
  const col = th.dataset.col;
  if (_epCfgSort.col === col) _epCfgSort.asc = !_epCfgSort.asc;
  else { _epCfgSort = { col, asc: col === 'name' }; }
  $('epCfgHead').querySelectorAll('th').forEach(t => {
    t.classList.toggle('sorted', t.dataset.col === col);
    const arrow = t.querySelector('.arrow');
    if (arrow) arrow.textContent = t.dataset.col === col ? (_epCfgSort.asc ? '▲' : '▼') : '';
  });
  showConfig();
});

// ── Time range selector ──

$('timeRange').addEventListener('click', e => {
  const btn = e.target.closest('button');
  if (!btn) return;
  liveRangeMs = +btn.dataset.r;
  viewMode = 'live';
  updateViewBounds();
  updateTimeRangeHighlight();
  updateAll();
});

// ── WebSocket ──

function setConnectionStatus(state) {
  const dot = $('dot');
  const text = $('connText');
  dot.className = 'dot' + (state === 'on' ? '' : state === 'try' ? ' try' : ' off');
  text.textContent = state === 'on' ? 'live' : state === 'try' ? 'connecting' : 'disconnected';
}

async function connect() {
  if (ws && ws.readyState === WebSocket.OPEN) return;
  if (ws && ws.readyState === WebSocket.CONNECTING) {
    ws.onclose = null;
    ws.onerror = null;
    ws.close();
  }

  setConnectionStatus('try');
  try {
    const resp = await fetch('/dashboard?_ws_check=' + Date.now(), { method: 'HEAD', cache: 'no-store' });
    if (!resp.ok) throw new Error('not ready');
  } catch (e) {
    setConnectionStatus('off');
    setTimeout(connect, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 1.2, 500);
    return;
  }

  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  ws = new WebSocket(protocol + '//' + location.host + '/dashboard/ws?t=' + Date.now());

  const connectTimer = setTimeout(() => {
    if (ws.readyState === WebSocket.CONNECTING) ws.close();
  }, 2000);

  ws.onopen = () => { clearTimeout(connectTimer); reconnectDelay = 50; setConnectionStatus('on'); };

  ws.onclose = () => {
    clearTimeout(connectTimer);
    setConnectionStatus('off');
    setTimeout(connect, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 1.2, 500);
  };

  ws.onerror = () => { clearTimeout(connectTimer); };

  ws.onmessage = e => {
    const msg = JSON.parse(e.data);
    if (msg.type === 'init') {
      snapshots = msg.history || [];
      startedAt = msg.startedAt;
      cacheConfig = msg.cache || {};
      requestLog = msg.recentRequests || [];
      hasEverReceivedData = snapshots.length > 0;
      showConfig();
      updateAll();
    } else if (msg.type === 'tick') {
      // Remove any synthetic snapshots before adding the real one
      while (snapshots.length > 0 && snapshots[snapshots.length - 1]._synthetic) snapshots.pop();
      snapshots.push(msg.snapshot);
      if (snapshots.length > 4000) snapshots = snapshots.slice(-3600);
      if (!_tabHidden) updateAll(); // skip rendering when tab is hidden
    } else if (msg.type === 'request') {
      requestLog.push(msg.record);
      if (requestLog.length > 200) requestLog = requestLog.slice(-100);
      if (!requestSort) updateRequestLog();

      // Synthetic snapshot: accumulate on existing synthetic instead of replacing it
      if (snapshots.length > 0) {
        const last = snapshots[snapshots.length - 1];
        let syn;
        if (last._synthetic) {
          // Mutate in place (already a synthetic from a prior request this tick)
          syn = last;
        } else {
          // Create new synthetic from the last real snapshot
          syn = structuredClone(last);
          syn._synthetic = true;
          snapshots.push(syn);
        }
        syn.ts = msg.record.ts;
        const rec = msg.record;
        if (rec.source === 'cache') { syn.hits++; syn.avoided++; }
        else { syn.misses++; syn.upstream++; }
        if (rec.status >= 400) {
          syn.responseCodes = syn.responseCodes || {};
          syn.responseCodes[rec.status] = (syn.responseCodes[rec.status] || 0) + 1;
        }
        if (rec.source !== 'cache' && rec.latencyUs > 0) {
          syn.latencyUsTotal += rec.latencyUs;
          syn.latencyCount++;
        }
        syn.hitRate = (syn.hits + syn.misses) > 0 ? syn.hits / (syn.hits + syn.misses) : 0;
        const ep = rec.endpoint;
        if (ep) {
          // Mutate in place - synthetic is a clone, no aliasing risk
          if (!syn.epHits) syn.epHits = {};
          if (!syn.epMisses) syn.epMisses = {};
          if (rec.source === 'cache') syn.epHits[ep] = (syn.epHits[ep] || 0) + 1;
          else syn.epMisses[ep] = (syn.epMisses[ep] || 0) + 1;
          if (rec.status >= 400) {
            if (!syn.epErrors) syn.epErrors = {};
            const errKey = ep + '\t' + rec.status;
            syn.epErrors[errKey] = (syn.epErrors[errKey] || 0) + 1;
          }
        }
        // In zoomed mode, only re-render if the request falls within the visible range
        if (viewMode === 'zoomed' && (rec.ts < viewMinSec * 1000 || rec.ts > viewMaxSec * 1000)) return;
        // Debounce: batch rapid request messages into a single rAF render pass
        if (!_reqUpdatePending) {
          _reqUpdatePending = true;
          requestAnimationFrame(() => { _reqUpdatePending = false; updateAll(); });
        }
      }
    }
  };
}

// ── Resize handler ──

let resizeTimer;
addEventListener('resize', () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => {
    for (const id of Object.keys(uplotInstances)) {
      uplotInstances[id].destroy();
      delete uplotInstances[id];
    }
    renderAllCharts();
  }, 200);
});

// ── Drag-to-pan (global handlers, rAF-throttled) ──

window.addEventListener('mousemove', e => {
  if (!panState) return;
  const dx = e.clientX - panState.sx;
  if (!panState.moved && Math.abs(dx) < 4) return;
  panState.moved = true;
  document.body.style.userSelect = 'none';

  const shift = -(dx / panState.pxWidth) * panState.range;
  viewMinSec = panState.minSec + shift;
  viewMaxSec = panState.maxSec + shift;
  clampView();
  viewMode = 'zoomed';

  updateTimeRangeHighlight();
  // Throttle renders to one per animation frame
  if (!_rafPending) {
    _rafPending = true;
    requestAnimationFrame(() => {
      _rafPending = false;
      updateAll();
    });
  }
});

window.addEventListener('mouseup', () => {
  if (panState) {
    panState = null;
    document.body.style.userSelect = '';
  }
});

// ── Keyboard shortcuts ──

document.addEventListener('keydown', e => {
  // Don't capture when typing in an input/select
  if (e.target.tagName === 'INPUT' || e.target.tagName === 'SELECT' || e.target.tagName === 'TEXTAREA') return;

  const panStep = (viewMaxSec - viewMinSec) * 0.1; // 10% of view range

  switch (e.key) {
    case 'Escape':
      viewMode = 'live';
      liveRangeMs = 3600000;
      updateViewBounds();
      updateTimeRangeHighlight();
      updateAll();
      break;
    case 'ArrowLeft':
      e.preventDefault();
      viewMinSec -= panStep;
      viewMaxSec -= panStep;
      clampView();
      viewMode = 'zoomed';
      updateTimeRangeHighlight();
      updateAll();
      break;
    case 'ArrowRight':
      e.preventDefault();
      viewMinSec += panStep;
      viewMaxSec += panStep;
      clampView();
      viewMode = 'zoomed';
      updateTimeRangeHighlight();
      updateAll();
      break;
    case '+': case '=': {
      const mid = (viewMinSec + viewMaxSec) / 2;
      const range = (viewMaxSec - viewMinSec) / 1.2;
      viewMinSec = mid - range / 2;
      viewMaxSec = mid + range / 2;
      clampView();
      viewMode = 'zoomed';
      updateTimeRangeHighlight();
      updateAll();
      break;
    }
    case '-': case '_': {
      const mid = (viewMinSec + viewMaxSec) / 2;
      const maxRange = snapshots.length >= 2
        ? (snapshots[snapshots.length - 1].ts - snapshots[0].ts) / 1000 : 3600;
      const range = Math.min(Math.max(maxRange, 60), (viewMaxSec - viewMinSec) * 1.2);
      viewMinSec = mid - range / 2;
      viewMaxSec = mid + range / 2;
      clampView();
      viewMode = 'zoomed';
      updateTimeRangeHighlight();
      updateAll();
      break;
    }
    case '1':
      liveRangeMs = 300000; viewMode = 'live';
      updateViewBounds(); updateTimeRangeHighlight(); updateAll();
      break;
    case '2':
      liveRangeMs = 900000; viewMode = 'live';
      updateViewBounds(); updateTimeRangeHighlight(); updateAll();
      break;
    case '3':
      liveRangeMs = 1800000; viewMode = 'live';
      updateViewBounds(); updateTimeRangeHighlight(); updateAll();
      break;
    case '4':
      liveRangeMs = 3600000; viewMode = 'live';
      updateViewBounds(); updateTimeRangeHighlight(); updateAll();
      break;
    case '?': {
      const help = $('kbdHelp');
      help.style.display = help.style.display === 'none' ? 'block' : 'none';
      break;
    }
  }
});

// ── Auto-pause on tab blur (feature 12) ──

let _tabHidden = false;

document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    _tabHidden = true;
    // Stop rendering while hidden to save CPU
  } else {
    _tabHidden = false;
    // Reconnect if needed
    if (!ws || ws.readyState > WebSocket.OPEN) {
      reconnectDelay = 200;
      connect();
    }
    // Refresh immediately on return
    updateAll();
  }
});

// ── Start ──

connect();
