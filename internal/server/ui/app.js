// Checklist order, matching the real backend pipeline.
const STAGES = ['scan', 'identify', 'score', 'analyze', 'rip', 'deliver'];

// The backend emits progressive ("-ing") stage names; map them to checklist keys.
const STAGE_ALIASES = {
  scanning:    'scan',
  identifying: 'identify',
  scoring:     'score',
  analyzing:   'analyze',
  ripping:     'rip',
  delivering:  'deliver',
};
function fmtETA(sec) {
  if (!sec || sec <= 0) return '';
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${String(s).padStart(2, '0')}s`;
  return `${s}s`;
}
function canonStage(stage) { return STAGE_ALIASES[stage] || stage; }

// Stages with no measurable progress — show an indeterminate sweep, not a bar.
const INDETERMINATE_STAGES = new Set(['scan', 'identify', 'score']);

// device → ProgressEvent (stage, percent, message, title, device)
const driveStates = {};
const knownDevices = new Set();

// device → { query, results, message } for the inline re-identify search on a card
const cardResolveState = {};

let jobs = [];
let jobsHasMore = false;
let jobsLoadingMore = false;
let selectedJobId = null;
let detailPanel = null;
let jobsRefreshTimer = null;
let infoRefreshTimer = null;
const layout = document.querySelector('.layout');
const validTabs = new Set(['drives', 'rips', 'info']);

function setActiveTab(tab, save = true) {
  if (!validTabs.has(tab) || !layout) return;
  layout.dataset.tab = tab;
  document.querySelectorAll('.tabbar [data-go]').forEach(button => {
    if (button.dataset.go === tab) button.setAttribute('aria-current', 'page');
    else button.removeAttribute('aria-current');
  });
  updateTabBadges();
  if (save) {
    try { localStorage.setItem('tab', tab); } catch (_) { /* storage unavailable (private mode); tab just isn't remembered */ }
  }
}

function initTabs() {
  let storedTab = '';
  try { storedTab = localStorage.getItem('tab') || ''; } catch (_) { /* storage unavailable; fall back to default tab */ }
  const tab = validTabs.has(storedTab) ? storedTab : 'drives';
  setActiveTab(tab);
  document.querySelectorAll('.tabbar [data-go]').forEach(button => {
    button.addEventListener('click', () => setActiveTab(button.dataset.go));
  });
}

function updateTabBadges() {
  const activeTab = layout && layout.dataset.tab;
  const drivesBadge = document.getElementById('drives-tab-badge');
  const hasActiveDrive = Object.values(driveStates).some(isDriveActive);
  if (drivesBadge) drivesBadge.hidden = !hasActiveDrive || activeTab === 'drives';

  const ripsBadge = document.getElementById('rips-tab-badge');
  if (ripsBadge) {
    const recentErrors = jobs
      .filter(job => isTerminal(job.Status))
      .slice(0, 20)
      .filter(job => String(job.Status).toLowerCase() === 'error').length;
    ripsBadge.textContent = recentErrors > 9 ? '9+' : String(recentErrors);
    ripsBadge.hidden = recentErrors === 0 || activeTab === 'rips';
  }
}

// ── Rendering helpers ────────────────────────────────────────────────────

function esc(s) {
  return String(s ?? '')
    .replaceAll('&', '&amp;').replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;').replaceAll('"', '&quot;');
}

function escAttr(s) {
  return String(s ?? '').replaceAll('&', '&amp;').replaceAll('"', '&quot;').replaceAll("'", '&#39;');
}

// Drives with auto eject on. The server saves this per drive; the UI only mirrors it.
const autoEjectDevices = new Set();

function isManualSearchWait(stage, message) {
  const canon = canonStage(stage || '');
  const msg = String(message || '').toLowerCase();
  return canon === 'identify' && (
    msg.includes('waiting for manual movie search') ||
    msg.includes('waiting for manual media search') ||
    msg.includes('no main title detected') ||
    msg.includes('tv disc detected')
  );
}

function badgeHTML(status, message = '') {
  if (isManualSearchWait(status, message)) {
    return '<span class="badge badge-waiting">⚠ awaiting title</span>';
  }
  const s = (status || 'idle').toLowerCase();
  return `<span class="badge badge-${esc(s)}">${esc(s)}</span>`;
}

function fmtTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

function fmtDate(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' }) +
    ' ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function fmtJSON(raw) {
  try { return JSON.stringify(typeof raw === 'string' ? JSON.parse(raw) : raw, null, 2); }
  catch (_) { return String(raw ?? ''); }
}

function fmtDiscType(t) {
  switch (t) {
    case 'bluray': return 'Blu-ray';
    case 'dvd':    return 'DVD';
    case 'cd':     return 'CD';
    default:       return t;
  }
}

// Recent Rips only shows finished jobs; in-progress state lives on the drive card.
const TERMINAL_STATUSES = new Set(['done', 'error', 'cancelled']);
function isTerminal(status) {
  return TERMINAL_STATUSES.has(String(status || '').toLowerCase());
}

// Finds the job currently in progress on a device, e.g. to apply a re-identify to it.
function findActiveJobForDevice(device) {
  return jobs.find(j => j.Device === device && !isTerminal(j.Status));
}

// mediaTypePill labels a search result as a movie or a TV series.
function mediaTypePill(type) {
  return type === 'tv'
    ? '<span class="type-pill type-pill-tv">TV series</span>'
    : '<span class="type-pill type-pill-movie">Movie</span>';
}

function searchResultMeta(r) {
  return `${r.year || 'Year unknown'}${r.runtime ? ` · ${r.runtime} min` : ''}`;
}

// defaultSearchFilter starts the search on the type of disc in the drive.
function defaultSearchFilter(device) {
  const st = driveStates[device] || {};
  if (st.needs_input) return 'tv';
  const job = findActiveJobForDevice(device);
  const pattern = job && String(job.Pattern || '').toLowerCase();
  if (pattern === 'tv') return 'tv';
  if (pattern === 'movie') return 'movie';
  return 'all';
}

function renderResolveResults(device, results, filter = 'all') {
  if (!results.length) return '<div class="msg-info">No results found.</div>';
  const counts = { all: results.length, movie: 0, tv: 0 };
  results.forEach(r => { counts[(r.media_type || 'movie') === 'tv' ? 'tv' : 'movie']++; });
  const chip = (key, label) => `<button type="button" class="filter-chip${filter === key ? ' active' : ''}"
    data-device="${escAttr(device)}" data-filter="${key}" aria-pressed="${filter === key}">${label} <span class="filter-count">${counts[key]}</span></button>`;
  const shown = results.filter(r => filter === 'all' || (r.media_type || 'movie') === filter);
  const rows = shown.length ? shown.map(r => `
    <div class="sr-row">
      <div>
        <div class="sr-title">${esc(r.title)}</div>
        <div class="sr-meta">${mediaTypePill(r.media_type || 'movie')} ${esc(searchResultMeta(r))}</div>
      </div>
      <button class="sr-apply card-resolve-apply" data-device="${escAttr(device)}"
        data-id="${r.id}" data-title="${escAttr(r.title)}" data-year="${r.year || 0}"
        data-type="${escAttr(r.media_type || 'movie')}">Select</button>
    </div>`).join('')
    : `<div class="msg-info">No ${filter === 'tv' ? 'TV series' : 'movies'} found. Try All.</div>`;
  return `<div class="filter-chips" role="group" aria-label="Filter results">
      ${chip('all', 'All')}${chip('movie', 'Movies')}${chip('tv', 'TV')}
    </div>${rows}`;
}

function selectionPayload(button) {
  const payload = {
    tmdb_id: Number.parseInt(button.dataset.id, 10),
    title: button.dataset.title,
    year: Number.parseInt(button.dataset.year, 10),
    media_type: button.dataset.type || 'movie',
  };
  if (payload.media_type !== 'tv') return payload;

  const seasonText = prompt(`What season is this disc of "${payload.title}"?`);
  if (seasonText === null) return null;
  const season = Number(seasonText.trim());
  if (!Number.isInteger(season) || season < 1) {
    alert('Enter a season number greater than zero.');
    return null;
  }
  const episodeText = prompt('Starting episode number on this disc (optional). Leave blank to continue after the highest episode already saved for this season.');
  if (episodeText === null) return null;
  const episodeTrimmed = episodeText.trim();
  // Blank sends 0: the server continues after the highest saved episode.
  const episodeStart = episodeTrimmed === '' ? 0 : Number(episodeTrimmed);
  if (episodeTrimmed !== '' && (!Number.isInteger(episodeStart) || episodeStart < 1)) {
    alert('Enter a starting episode number greater than zero, or leave it blank.');
    return null;
  }
  payload.season = season;
  payload.episode_start = episodeStart;
  return payload;
}

// ── Drive cards ──────────────────────────────────────────────────────────

function ensureDevice(device) {
  if (!device || knownDevices.has(device)) return;
  knownDevices.add(device);
  if (!driveStates[device]) {
    driveStates[device] = { device, stage: 'idle', percent: 0, message: 'no disc — waiting', title: '' };
  }
  renderDrives();
}

function applyProgressEvent(ev) {
  const dev = ev.device;
  const prev = dev ? driveStates[dev] : undefined;
  if (dev) {
    ensureDevice(dev);
    // The server repeats needs_input on every event while it waits, so an event
    // without it means the request was answered or is over.
    driveStates[dev] = { ...driveStates[dev], ...ev, needs_input: ev.needs_input || null, device: dev, progressAt: Date.now() };
    if (!ev.needs_input && cardResolveState[dev] && !cardResolveState[dev].open && !cardResolveState[dev].picked
        && !cardResolveState[dev].applying && !isManualSearchWait(ev.stage, ev.message)) {
      delete cardResolveState[dev];
    }
  }
  renderDrives();
  const stageChanged = !prev || prev.stage !== ev.stage;
  const finished = ev.stage === 'done' || ev.stage === 'error' || ev.stage === 'cancelled';
  if (finished && stageChanged) {
    scheduleJobsRefresh();
  }
  // System info (rip counts, staging space, drives with discs) only changes
  // when a rip finishes or a drive's status changes, so refresh on those events.
  if ((finished && stageChanged) || (prev && ev.drive_status && prev.drive_status !== ev.drive_status)) {
    scheduleInfoRefresh();
  }
}

function scheduleInfoRefresh() {
  if (infoRefreshTimer) return;
  infoRefreshTimer = setTimeout(() => {
    infoRefreshTimer = null;
    loadSystemInfo();
  }, 700);
}

function scheduleJobsRefresh() {
  if (jobsRefreshTimer) return;
  jobsRefreshTimer = setTimeout(async () => {
    jobsRefreshTimer = null;
    await loadJobs();
  }, 700);
}

// force re-renders even while a card's search box has focus; use it when the
// user's own action (search, select) must show up immediately. Focus returns
// to that search box if it is still on the card.
function renderDrives(force = false) {
  updateTabBadges();
  const grid = document.getElementById('drives-grid');
  // Re-rendering replaces the DOM, which would drop focus and typed text on
  // every progress tick. Skip while the user is typing in a card's search box.
  const focused = document.activeElement;
  const typingIn = focused && focused.classList && focused.classList.contains('card-resolve-input')
    ? focused.dataset.device : null;
  if (typingIn && !force) return;
  renderDriveGrid(grid);
  if (typingIn) {
    const field = focused.dataset.field || 'query';
    const input = [...grid.querySelectorAll('.card-resolve-input')]
      .find(el => el.dataset.device === typingIn && (el.dataset.field || 'query') === field);
    if (input) {
      input.focus();
      if (input.type === 'text') input.setSelectionRange(input.value.length, input.value.length);
    }
  }
}

function renderDriveGrid(grid) {
  const devices = knownDevices.size > 0 ? [...knownDevices] : [];
  if (!devices.length) {
    grid.innerHTML = '<div class="jobs-empty">Loading drives…</div>';
    return;
  }
  grid.innerHTML = devices.map(d => {
    const st = driveStates[d] || { device: d, stage: 'idle', percent: 0, message: 'no disc — waiting', title: '' };
    return driveCardHTML(st);
  }).join('');
  grid.querySelectorAll('.eject-btn').forEach(btn => {
    btn.addEventListener('click', async () => {
      const d = btn.dataset.device;
      const response = await fetch('/api/eject', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ device: d }),
      }).catch(() => null);
      if (response && !response.ok) {
        const data = await response.json().catch(() => ({}));
        alert(data.error || 'Tray toggle failed.');
      }
    });
  });
  grid.querySelectorAll('.cancel-rip-btn').forEach(btn => btn.addEventListener('click', async () => {
    const d = btn.dataset.device;
    if (!confirm(`Cancel the active rip on ${d}? Any work in progress may be lost.`)) return;
    const response = await fetch('/api/cancel', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ device: d }) }).catch(() => null);
    if (!response || !response.ok) {
      const data = response ? await response.json().catch(() => ({})) : {};
      alert(data.error || 'Could not cancel the rip.');
    }
  }));
  grid.querySelectorAll('.auto-eject-toggle').forEach(input => input.addEventListener('change', () => {
    setAutoEject(input.dataset.device, input.checked);
  }));
  grid.querySelectorAll('.drive-info-btn').forEach(btn => btn.addEventListener('click', () => {
    openDriveDialog(btn.dataset.device);
  }));
  grid.querySelectorAll('.card-edit-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      const d = btn.dataset.device;
      const cur = driveStates[d] || {};
      cardResolveState[d] = { query: cur.title || '', results: null, message: '', ...(cardResolveState[d] || {}), open: true };
      renderDrives();
    });
  });
  grid.querySelectorAll('.card-resolve-cancel').forEach(btn => {
    btn.addEventListener('click', () => { delete cardResolveState[btn.dataset.device]; renderDrives(true); });
  });
  grid.querySelectorAll('.card-resolve-search').forEach(btn => {
    btn.addEventListener('click', () => doCardSearch(btn.dataset.device));
  });
  grid.querySelectorAll('.card-resolve-input').forEach(input => {
    const d = input.dataset.device;
    const field = input.dataset.field || 'query';
    input.addEventListener('input', () => {
      const rs = cardResolveState[d] || {};
      cardResolveState[d] = field === 'query'
        ? { ...rs, query: input.value }
        : { ...rs, form: { ...(rs.form || {}), [field]: input.value } };
    });
    input.addEventListener('keydown', e => {
      if (e.key !== 'Enter') return;
      if (field === 'query') doCardSearch(d); else confirmTVForm(d);
    });
  });
  grid.querySelectorAll('.card-resolve-apply').forEach(btn => {
    btn.addEventListener('click', () => {
      const d = btn.dataset.device;
      if ((btn.dataset.type || 'movie') === 'tv') {
        // Ask for season and episode inline instead of browser dialogs.
        const show = {
          id: Number.parseInt(btn.dataset.id, 10), title: btn.dataset.title,
          year: Number.parseInt(btn.dataset.year, 10) || 0,
        };
        cardResolveState[d] = { ...(cardResolveState[d] || {}), picked: show, open: false, form: {}, message: '' };
        renderDrives(true);
        return;
      }
      const payload = selectionPayload(btn);
      if (payload) applyCardReidentify(d, payload);
    });
  });
  grid.querySelectorAll('.filter-chip').forEach(btn => {
    btn.addEventListener('click', () => {
      const d = btn.dataset.device;
      cardResolveState[d] = { ...(cardResolveState[d] || {}), filter: btn.dataset.filter };
      renderDrives(true);
    });
  });
  grid.querySelectorAll('.tv-confirm').forEach(btn => {
    btn.addEventListener('click', () => confirmTVForm(btn.dataset.device));
  });
  grid.querySelectorAll('.tv-other-show').forEach(btn => {
    btn.addEventListener('click', () => {
      const d = btn.dataset.device;
      const rs = cardResolveState[d] || {};
      cardResolveState[d] = { ...rs, picked: null, open: true, form: {}, message: '' };
      renderDrives(true);
    });
  });
}

function isDriveActive(st) {
  return !!st.stage && !['idle', 'done', 'error', 'cancelled'].includes(st.stage);
}

function driveIdleState(message) {
  const m = message || '';
  if (/not responding/i.test(m)) return { icon: '⚠️', cls: 'drive-idle-warn', text: m };
  if (/disc detected/i.test(m))  return { icon: '💿', cls: 'drive-idle-ok', text: m };
  if (/loading|detecting/i.test(m)) return { icon: '⏳', cls: '', text: m };
  if (/tray open/i.test(m))      return { icon: '⏏️', cls: '', text: m };
  return { icon: '', cls: '', text: m || 'no disc — waiting' };
}

function driveMediaStatusHTML(status) {
  switch (status) {
    case 'disc_present': return '<div class="drive-media-status ok"><span>●</span> Tray closed · Disc present</div>';
    case 'no_disc': return '<div class="drive-media-status"><span>○</span> Tray closed · Empty</div>';
    case 'tray_open': return '<div class="drive-media-status warn"><span>⏏</span> Tray open · No disc</div>';
    case 'loading': return '<div class="drive-media-status warn"><span>◌</span> Drive loading</div>';
    case 'detecting': return '<div class="drive-media-status"><span>◌</span> Checking drive…</div>';
    case 'unresponsive': return '<div class="drive-media-status error"><span>⚠</span> Drive not responding</div>';
    default: return '<div class="drive-media-status"><span>◌</span> Checking drive…</div>';
  }
}

function driveCardHTML(st) {
  const { stage, percent, message, title } = st;
  const device = st.device;
  const isIdle = !stage || stage === 'idle';
  const discTypeChip = st.disc_type && st.disc_type !== 'unknown'
    ? `<span class="badge badge-disc-type">${esc(fmtDiscType(st.disc_type))}</span>` : '';

  const header = `
    <div class="drive-card-header">
      <div style="display:flex;align-items:center;gap:6px">
        <span class="drive-device">${esc(device)}</span>
        ${discTypeChip}
      </div>
      <div class="drive-card-tools">
        ${st.needs_input && !isIdle ? '<span class="badge badge-waiting">⚠ needs input</span>' : badgeHTML(stage, message)}
        <button type="button" class="drive-info-btn" data-device="${escAttr(device)}" aria-label="Drive details for ${escAttr(device)}" title="Drive details">ⓘ</button>
      </div>
    </div>`;

  if (isIdle) {
    return `<div class="drive-card">${header}${driveMediaStatusHTML(st.drive_status)}${(() => { const d = driveIdleState(message); return `<div class="drive-idle ${d.cls}">${d.icon ? `<span class="drive-idle-icon">${d.icon}</span> ` : ""}${esc(d.text)}</div>`; })()}${driveActionsHTML(device, false, st.drive_status)}</div>`;
  }

  const titleLine  = title   ? `<div class="drive-title">${esc(title)}</div>` : '';
  const msgLine    = message ? `<div class="drive-subtitle">${esc(message)}</div>` : '';

  // "done" means every stage completed; otherwise locate the active stage.
  const finished = stage === 'done';
  const canon = canonStage(stage);
  const curIdx = finished ? STAGES.length : STAGES.indexOf(canon);
  const stagesHTML = STAGES.map((s, idx) => {
    const isDone   = idx < curIdx;
    const isCurrent = s === canon;
    const waitingIdentify = isCurrent && isManualSearchWait(stage, message) && s === 'identify';
    const indeterminate = isCurrent && INDETERMINATE_STAGES.has(s);

    const labelClass = stageStateClass(isDone, isCurrent);
    const circleClass = waitingIdentify && !isDone ? 'waiting' : labelClass;
    const icon = stageIcon(isDone, waitingIdentify, indeterminate);
    const connectorClass = labelClass;

    return `
      <div class="stage-node">
        <div class="stage-circle ${circleClass}">${icon}</div>
        <div class="stage-label ${labelClass}">${esc(s)}</div>
      </div>
      ${idx < STAGES.length - 1 ? `<div class="stage-connector ${connectorClass}"></div>` : ''}`;
  }).join('');

  // only rip/deliver get a progress bar under the stepper — matches the
  // existing pct/indeterminate values, just moved out of the per-row layout
  const activeStageHasBar = curIdx >= 0 && curIdx < STAGES.length && !INDETERMINATE_STAGES.has(canon) && canon !== 'idle';
  const pct = activeStageHasBar ? displayPercent(st) : 0;
  const progressHTML = activeStageHasBar
    ? `<div class="stage-progress prog-wrap"><div class="prog-fill" data-progress-device="${escAttr(device)}" style="width:${pct}%"></div></div>`
    : '';
  const etaText = canon === 'rip' ? fmtETA(st.eta_seconds) : '';
  const etaHTML = etaText
    ? `<div class="drive-subtitle">${Math.round(pct)}% · ~${esc(etaText)} remaining</div>`
    : '';

  const canEdit = ['scanning', 'identifying', 'analyzing', 'ripping'].includes(stage);
  const need = canEdit ? st.needs_input : null;
  const rsExisting = cardResolveState[device];
  const editOpen = canEdit && !!(rsExisting && rsExisting.open);
  const picked = canEdit && rsExisting ? rsExisting.picked : null;
  // The show the TV form confirms: one picked from search, else the matched or suggested show.
  const formShow = picked || (need && need.tmdb_id ? { id: need.tmdb_id, title: need.title, year: need.year } : null);
  const showForm = !!formShow && (!!need || !!picked) && !editOpen;
  const showSearch = editOpen || (!!need && !formShow) || (!need && isManualSearchWait(stage, message));
  const showResolve = showForm || showSearch || !!need;
  let resolvePanelHTML = '';
  if (showResolve) {
    const rs = rsExisting || { query: (need && need.title) || title || '', results: null, message: '' };
    cardResolveState[device] = rs;
    if (rs.applying) {
      resolvePanelHTML = `<div class="card-resolve"><div class="msg-info">Applying ${esc(rs.applying)}…</div></div>`;
    } else {
      const needHTML = need ? `
        <div class="tv-need-head">Needs your input</div>
        <div class="tv-need-summary">${esc(tvNeedPrompt(need))}</div>` : '';
      const formHTML = showForm ? tvConfirmFormHTML(device, formShow, need, rs, !!picked) : '';
      const searchHTML = showSearch ? `
        <div class="search-row">
          <input class="search-input card-resolve-input" data-device="${escAttr(device)}" data-field="query"
            type="text" aria-label="Search TMDB for the correct title" placeholder="Search TMDB…" value="${escAttr(rs.query)}" />
          <button class="btn btn-primary card-resolve-search" data-device="${escAttr(device)}">Search</button>
          ${editOpen ? `<button class="btn card-resolve-cancel" data-device="${escAttr(device)}">Cancel</button>` : ''}
        </div>
        <div class="search-results">${rs.results ? renderResolveResults(device, rs.results, rs.filter || defaultSearchFilter(device)) : ''}</div>` : '';
      resolvePanelHTML = `
        <div class="card-resolve${need ? ' tv-need' : ''}" data-device="${escAttr(device)}">
          ${needHTML}
          ${formHTML}
          ${searchHTML}
          ${rs.message ? `<div class="msg-error">${esc(rs.message)}</div>` : ''}
        </div>`;
    }
  }

  return `
    <div class="drive-card">
      ${header}
      ${driveMediaStatusHTML(st.drive_status)}
      ${titleLine}
      ${msgLine}
      <div class="stages">${stagesHTML}</div>
      ${progressHTML}
      ${etaHTML}
      ${canEdit && !showResolve && !picked ? `<button class="btn card-edit-btn" data-device="${escAttr(device)}">✎ Edit title</button>` : ''}
      ${resolvePanelHTML}
      ${driveActionsHTML(device, isDriveActive(st), st.drive_status)}
    </div>`;
}

// tvNeedPrompt is the drive card's one-line ask for a TV disc. (need.summary
// is worded for notifications, which point the user to SimpleRip.)
function tvNeedPrompt(need) {
  switch (need.kind) {
    case 'season': return 'Which season is this disc? The rip continues meanwhile.';
    case 'episode': return `Season ${need.season} matched, but the episode order could not be confirmed. Check the first episode number.`;
    case 'show': return need.tmdb_id
      ? 'Is this the right show? Confirm it and choose the season.'
      : 'The show was not identified. Search for it below.';
    default: return need.summary || '';
  }
}

// tvConfirmFormHTML is the inline season/episode form for a TV disc. show is
// the series being confirmed; need is the server's request (may be null when
// the user picked a show from search without being asked).
function tvConfirmFormHTML(device, show, need, rs, fromSearch) {
  const form = rs.form || {};
  const fromNeed = need && need.tmdb_id === show.id;
  const season = form.season ?? (fromNeed && need.season ? String(need.season) : '');
  const episode = form.episode ?? '';
  const label = `${show.title}${show.year ? ` (${show.year})` : ''}`;
  let tag = '';
  if (!fromSearch && need) tag = need.show_matched ? 'matched' : 'suggested';
  const seasonHint = fromNeed && need.season_suggested && form.season === undefined
    ? '<div class="tv-hint">Season pre-filled from an earlier disc of this set.</div>' : '';
  const d = escAttr(device);
  return `
    <div class="tv-form">
      <div class="tv-show">Show: <strong>${esc(label)}</strong>${tag ? ` <span class="tv-tag tv-tag-${tag}">${tag}</span>` : ''}</div>
      <div class="tv-fields">
        <label>Season
          <input class="search-input card-resolve-input tv-input" data-device="${d}" data-field="season"
            type="number" min="1" inputmode="numeric" value="${escAttr(season)}" />
        </label>
        <label>First episode on disc
          <input class="search-input card-resolve-input tv-input" data-device="${d}" data-field="episode"
            type="number" min="1" inputmode="numeric" placeholder="Next after saved" value="${escAttr(episode)}" />
        </label>
      </div>
      ${seasonHint}
      <div class="tv-hint">Leave the first episode blank to continue after the episodes already saved for this season.</div>
      <div class="tv-actions">
        <button class="btn btn-primary tv-confirm" data-device="${d}">Confirm</button>
        <button class="btn tv-other-show" data-device="${d}">Different show…</button>
        ${fromSearch ? `<button class="btn card-resolve-cancel" data-device="${d}">Cancel</button>` : ''}
      </div>
    </div>`;
}

// confirmTVForm validates the inline TV form and applies it.
function confirmTVForm(device) {
  const rs = cardResolveState[device] || {};
  const st = driveStates[device] || {};
  const need = st.needs_input;
  const show = rs.picked || (need && need.tmdb_id ? { id: need.tmdb_id, title: need.title, year: need.year } : null);
  if (!show) return;
  const form = rs.form || {};
  const seasonText = String(form.season ?? (need && need.tmdb_id === show.id && need.season ? need.season : '')).trim();
  const season = Number(seasonText);
  const episodeText = String(form.episode ?? '').trim();
  const episodeStart = episodeText === '' ? 0 : Number(episodeText);
  let error = '';
  if (!Number.isInteger(season) || season < 1) error = 'Enter a season number of 1 or more.';
  else if (episodeText !== '' && (!Number.isInteger(episodeStart) || episodeStart < 1)) error = 'Enter a first episode of 1 or more, or leave it blank.';
  if (error) {
    cardResolveState[device] = { ...rs, message: error };
    renderDrives(true);
    return;
  }
  applyCardReidentify(device, {
    tmdb_id: show.id, title: show.title, year: show.year || 0,
    media_type: 'tv', season, episode_start: episodeStart,
  });
}

// Estimate only a short distance beyond the latest server value. The ETA
// controls the rate; every WebSocket event resets the estimate to the backend
// value, so corrections are smoothly reconciled by the bar's CSS transition.
function displayPercent(st) {
  const base = Number(st.percent) || 0;
  if (canonStage(st.stage) !== 'rip' || !st.progressAt || !st.eta_seconds || base >= 99) return base;
  const elapsedSec = Math.max(0, (Date.now() - st.progressAt) / 1000);
  const remainingPct = Math.max(1, 100 - base);
  const secondsPerPoint = Math.max(1, st.eta_seconds / remainingPct);
  return Math.min(99, base + Math.min(1.5, elapsedSec / secondsPerPoint));
}

// Keep the display moving between backend ticks without re-rendering cards or
// disturbing an open input. This is display-only; backend progress remains the
// source of truth for stage changes and completion.
setInterval(() => {
  document.querySelectorAll('.prog-fill[data-progress-device]').forEach(fill => {
    const st = driveStates[fill.dataset.progressDevice];
    if (st) fill.style.width = `${displayPercent(st)}%`;
  });
}, 250);

// autoEjectSwitchHTML is a phone-style on/off switch for a drive's auto eject.
function autoEjectSwitchHTML(device, checked) {
  return `<label class="switch-row"><span>Auto eject when complete</span><input class="switch auto-eject-toggle" type="checkbox" role="switch" data-device="${escAttr(device)}" ${checked}></label>`;
}

function driveActionsHTML(device, active, driveStatus) {
  const checked = autoEjectDevices.has(device) ? 'checked' : '';
  const trayAction = {
    disc_present: '⏏ Eject',
    no_disc: 'Open',
    tray_open: 'Close',
  }[driveStatus];
  const action = trayAction || 'Checking…';
  return `<div class="drive-actions">${active ? `<button class="btn cancel-rip-btn" data-device="${escAttr(device)}">Cancel rip</button>` : ''}${autoEjectSwitchHTML(device, checked)}<button type="button" class="eject-btn" data-device="${escAttr(device)}" ${trayAction ? '' : 'disabled'}>${action}</button></div>`;
}

// setAutoEject saves a drive's auto eject setting and keeps every switch for
// that drive (card and drive panel) in step.
async function setAutoEject(device, enabled) {
  const show = on => {
    if (on) autoEjectDevices.add(device); else autoEjectDevices.delete(device);
    document.querySelectorAll('.auto-eject-toggle').forEach(el => {
      if (el.dataset.device === device) el.checked = on;
    });
  };
  show(enabled);
  const response = await fetch('/api/auto-eject', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ device, enabled }) }).catch(() => null);
  if (!response || !response.ok) {
    show(!enabled);
    alert('Could not save the auto-eject setting.');
  }
}

// loadAutoEject reads each drive's saved auto-eject setting from the server.
async function loadAutoEject() {
  try {
    const r = await fetch('/api/auto-eject');
    if (!r.ok) return;
    const settings = await r.json() || {};
    autoEjectDevices.clear();
    Object.entries(settings).forEach(([device, on]) => { if (on) autoEjectDevices.add(device); });
    renderDrives();
  } catch (_) { /* switches show off until the next load */ }
}

// ── Drive details panel ──────────────────────────────────────────────────

let driveDialogDevice = null;

const DRIVE_STATUS_TEXT = {
  disc_present: 'Tray closed · disc present', no_disc: 'Tray closed · empty', tray_open: 'Tray open',
  loading: 'Loading disc', detecting: 'Checking…', unresponsive: 'Not responding',
};

function fmtMinutes(min) {
  if (!min) return '—';
  const h = Math.floor(min / 60), m = Math.round(min % 60);
  return h ? `${h}h ${m}m` : `${m}m`;
}

function libreDriveText(msg) {
  if (!msg) return 'Not reported';
  const version = /\((v[\d.]+)/.exec(msg);
  return /Using LibreDrive mode/i.test(msg) ? `Enabled${version ? ` (${version[1]})` : ''}` : msg;
}

// driveSerial takes the serial from makemkvcon's drive name, which ends
// "<model> <firmware> <serial>".
function driveSerial(name, firmware) {
  if (!name || !firmware) return '';
  const at = name.lastIndexOf(` ${firmware} `);
  return at >= 0 ? name.slice(at + firmware.length + 2).trim() : '';
}

function driveModelName(data) {
  const hw = data.hardware || {};
  return [hw.vendor, hw.model].filter(Boolean).join(' ') || (data.makemkv && data.makemkv.name) || '';
}

function infoRows(rows) {
  return rows.filter(([, value]) => value !== '' && value != null)
    .map(([label, value]) => `<div class="dd-row"><span>${esc(label)}</span><span>${esc(String(value))}</span></div>`).join('');
}

function driveDialogShell(device, body, subtitle = '') {
  return `
    <div class="dd-head">
      <div>
        <div id="drive-dialog-title" class="dd-title">${esc(device)}</div>
        ${subtitle ? `<div class="dd-subtitle">${esc(subtitle)}</div>` : ''}
      </div>
      <button type="button" class="dd-close" aria-label="Close">✕</button>
    </div>
    <div class="dd-body">${body}</div>`;
}

function driveDialogBody(data) {
  const device = data.device;
  const st = { ...(data.state || {}), ...(driveStates[device] || {}) };
  const hw = data.hardware || {};
  const mk = data.makemkv || {};
  const stats = data.stats;
  const active = isDriveActive(st);
  const activity = active ? `${st.stage}${st.title ? ` · ${st.title}` : ''}` : 'Idle';

  const settings = `
    <section class="dd-section">
      <h3>Settings</h3>
      ${autoEjectSwitchHTML(device, data.auto_eject ? 'checked' : '')}
      <div class="tv-hint">Opens the tray after a rip is delivered.</div>
    </section>`;
  const now = `
    <section class="dd-section">
      <h3>Now</h3>
      ${infoRows([
        ['Tray', DRIVE_STATUS_TEXT[st.drive_status] || 'Unknown'],
        ['Activity', activity],
        ['Disc type', st.disc_type && st.disc_type !== 'unknown' ? fmtDiscType(st.disc_type) : ''],
      ])}
    </section>`;
  const drive = `
    <section class="dd-section">
      <h3>Drive</h3>
      ${infoRows([
        ['Vendor', hw.vendor || ''], ['Model', hw.model || ''], ['Firmware', hw.firmware || ''],
        ['Serial', driveSerial(mk.name, hw.firmware)],
        ['LibreDrive', libreDriveText(mk.libredrive)],
        ['MakeMKV name', mk.name || 'Not scanned yet'],
      ])}
      ${mk.scanned_at ? `<div class="tv-hint">From the scan at ${esc(fmtDate(mk.scanned_at))}.</div>` : (mk.name ? '<div class="tv-hint">From the last recorded scan.</div>' : '')}
    </section>`;
  let history = '';
  if (stats) {
    const recent = (data.recent || []).map(j => `
      <button type="button" class="dd-job" data-job="${escAttr(j.ID)}">
        <span class="dd-job-title">${esc(j.Title ? `${j.Title}${j.Year && !j.Title.includes(`(${j.Year})`) ? ` (${j.Year})` : ''}` : (j.DiscLabel || 'Unknown disc'))}</span>
        ${badgeHTML(j.Status)}
        <span class="dd-job-date">${esc(fmtDate(j.FinishedAt || j.CreatedAt))}</span>
      </button>`).join('');
    history = `
      <section class="dd-section">
        <h3>History</h3>
        <div class="dd-tiles">
          <div class="dd-tile"><b>${stats.done}</b><span>done</span></div>
          <div class="dd-tile"><b>${stats.error}</b><span>errors</span></div>
          <div class="dd-tile"><b>${stats.cancelled}</b><span>cancelled</span></div>
        </div>
        ${infoRows([
          ['Delivered', `${(stats.delivered_gb || 0).toFixed(1)} GB`],
          ['Average rip', fmtMinutes(stats.avg_minutes)],
          ['Last finished', stats.last_rip_at ? fmtDate(stats.last_rip_at) : '—'],
        ])}
        ${recent ? `<div class="dd-recent-head">Recent rips</div>${recent}` : '<div class="tv-hint">No rips on this drive yet.</div>'}
      </section>`;
  }
  return settings + now + drive + history;
}

function bindDriveDialog(dialog) {
  const close = dialog.querySelector('.dd-close');
  if (close) close.addEventListener('click', () => dialog.close());
  dialog.querySelectorAll('.auto-eject-toggle').forEach(input => input.addEventListener('change', () => {
    setAutoEject(input.dataset.device, input.checked);
    renderDrives();
  }));
  dialog.querySelectorAll('.dd-job').forEach(btn => btn.addEventListener('click', () => {
    dialog.close();
    openJob(btn.dataset.job);
  }));
}

async function openDriveDialog(device) {
  const dialog = document.getElementById('drive-dialog');
  driveDialogDevice = device;
  dialog.innerHTML = driveDialogShell(device, '<div class="msg-info">Loading drive details…</div>');
  bindDriveDialog(dialog);
  if (!dialog.open) dialog.showModal();
  let body, subtitle = '';
  try {
    const r = await fetch(`/api/drive?device=${encodeURIComponent(device)}`);
    const data = await r.json();
    if (!r.ok) throw new Error(data.error || 'request failed');
    // The server's saved setting is authoritative.
    if (data.auto_eject) autoEjectDevices.add(device); else autoEjectDevices.delete(device);
    body = driveDialogBody(data);
    subtitle = driveModelName(data);
  } catch (e) {
    body = `<div class="msg-error">Could not load drive details: ${esc(e.message)}</div>`;
  }
  if (driveDialogDevice !== device || !dialog.open) return;
  dialog.innerHTML = driveDialogShell(device, body, subtitle);
  bindDriveDialog(dialog);
}

async function doCardSearch(device) {
  const rs = cardResolveState[device] || { query: '' };
  const q = (rs.query || '').trim();
  if (!q) return;
  cardResolveState[device] = { ...rs, message: '' };
  try {
    const r = await fetch(`/api/search?q=${encodeURIComponent(q)}`);
    const data = await r.json();
    if (!r.ok) {
      cardResolveState[device] = { ...rs, results: null, message: data.error || 'Search failed' };
    } else {
      cardResolveState[device] = { ...rs, results: data, message: '' };
    }
  } catch (_) {
    cardResolveState[device] = { ...rs, results: null, message: 'Search request failed.' };
  }
  renderDrives(true);
}

function selectionLabel(payload) {
  let label = payload.title + (payload.year ? ` (${payload.year})` : '');
  if (payload.media_type === 'tv') {
    label += `, season ${payload.season}`;
    if (payload.episode_start) label += ` from episode ${payload.episode_start}`;
  }
  return label;
}

async function applyCardReidentify(device, payload) {
  const label = selectionLabel(payload);
  // Show the choice was taken right away; the results stay hidden until it fails.
  if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
  cardResolveState[device] = { ...cardResolveState[device], applying: label, message: '' };
  renderDrives(true);
  const fail = message => {
    cardResolveState[device] = { ...cardResolveState[device], applying: '', message };
    renderDrives(true);
  };
  let job = findActiveJobForDevice(device);
  if (!job) { await loadJobs(); job = findActiveJobForDevice(device); }
  if (!job) {
    fail('No active job found for this drive.');
    return;
  }
  try {
    const r = await fetch(`/api/jobs/${encodeURIComponent(job.ID)}/reidentify`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const data = await r.json();
    if (!r.ok) {
      fail(data.error || 'Failed');
      return;
    }
    delete cardResolveState[device];
    // The server re-emits the drive's state with the new title; don't let the
    // old "search for the show" message reopen the panel before that arrives.
    const st = driveStates[device];
    if (st && isManualSearchWait(st.stage, st.message)) {
      const title = payload.title + (payload.year ? ` (${payload.year})` : '');
      driveStates[device] = { ...st, title, message: `Identified as ${title}` };
    }
    if (driveStates[device]) driveStates[device] = { ...driveStates[device], needs_input: null };
    renderDrives(true);
    showToast(`Saved: ${label}.${data.message ? ' ' + data.message : ''}`);
    await loadJobs();
  } catch (_) {
    fail('Request failed.');
  }
}

function showToast(text) {
  const el = document.createElement('div');
  el.textContent = text;
  el.style.cssText = 'position:fixed;bottom:20px;left:50%;transform:translateX(-50%);max-width:80vw;padding:10px 16px;background:#222;color:#fff;border-radius:6px;box-shadow:0 2px 10px rgba(0,0,0,.4);z-index:9999;font-size:14px';
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 8000);
}

function fmtUptime(totalSeconds) {
  if (!Number.isFinite(totalSeconds)) return 'Unavailable';
  const secs = Math.max(0, Math.floor(totalSeconds));
  const days = Math.floor(secs / 86400);
  const hours = Math.floor((secs % 86400) / 3600);
  const mins = Math.floor((secs % 3600) / 60);
  const remainder = secs % 60;
  if (days > 0) return `${days}d ${hours}h ${mins}m`;
  if (hours > 0) return `${hours}h ${mins}m`;
  if (mins > 0) return `${mins}m ${remainder}s`;
  return `${remainder}s`;
}

function stageStateClass(isDone, isCurrent) {
  if (isDone) return 'done';
  if (isCurrent) return 'active';
  return '';
}

function stageIcon(isDone, waitingIdentify, indeterminate) {
  if (isDone) return '✓';
  if (waitingIdentify) return '⚠';
  if (indeterminate) return '<span class="spinner"></span>';
  return '';
}

function statusDotHTML(ok, warn) {
  let cls = 'bad';
  if (ok) cls = 'ok';
  else if (warn) cls = 'warn';
  return `<span class="status-dot ${cls}"></span>`;
}

function renderInfoCard(info) {
  const el = document.getElementById('info-card');
  if (!el) return;
  if (!info) {
    el.innerHTML = '<h2>SimpleRip</h2><div class="info-card-body"><div class="info-empty">System info unavailable.</div></div>';
    return;
  }

  const version = info.version || 'dev';
  const built = new Date(info.build_date || '');
  const buildDate = isNaN(built) ? 'unknown date' : built.toLocaleDateString([], { year: 'numeric', month: 'short', day: 'numeric' });
  const buildMeta = info.commit && info.commit !== 'unknown' ? `${info.commit} · ${buildDate}` : buildDate;
  const uptime = info.started_at ? fmtUptime((Date.now() - new Date(info.started_at).getTime()) / 1000) : 'Unavailable';
  const host = info.hostname || 'unknown';
  const makemkv = info.tools && info.tools.makemkvcon && info.tools.makemkvcon.version ? info.tools.makemkvcon.version : 'unavailable';
  const ffprobe = info.tools && info.tools.ffprobe && info.tools.ffprobe.version ? info.tools.ffprobe.version : 'unavailable';
  const deliveryTarget = info.delivery && info.delivery.destination ? info.delivery.destination : 'not configured';
  const deliveryReachable = !!(info.delivery && info.delivery.reachable);
  const staging = info.staging || {};
  const freeBytes = Number(staging.free_bytes || 0);
  const totalBytes = Number(staging.total_bytes || 0);
  const stagingPct = totalBytes > 0 ? (freeBytes / totalBytes) * 100 : 0;
  const stagingWarn = stagingPct < 10 || freeBytes < 50 * 1024 * 1024 * 1024;
  const tmdb = !!(info.integrations && info.integrations.tmdb_configured);
  const discord = !!(info.integrations && info.integrations.discord_configured);
  const done = Number(info.stats && info.stats.done ? info.stats.done : 0);
  const errors = Number(info.stats && info.stats.error ? info.stats.error : 0);

  el.innerHTML = `
    <h2>SimpleRip</h2>
    <div class="info-card-body">
      <div class="info-row"><span>Version</span><div class="info-value"><strong>${esc(version)}</strong><div class="muted">${esc(buildMeta)}</div></div></div>
      <div class="info-row"><span>Uptime</span><div class="info-value"><strong>${esc(uptime)}</strong></div></div>
      <div class="info-row"><span>Host</span><div class="info-value"><strong>${esc(host)}</strong></div></div>
      <div class="info-row"><span>makemkvcon</span><div class="info-value"><strong>${esc(String(makemkv))}</strong></div></div>
      <div class="info-row"><span>ffprobe</span><div class="info-value"><strong>${esc(String(ffprobe))}</strong></div></div>
      <div class="info-row"><span>Delivery</span><div class="info-value"><span class="info-status">${statusDotHTML(deliveryReachable, !deliveryReachable)}<strong>${esc(deliveryTarget)}</strong></span></div></div>
      <div class="info-row"><span>Staging</span><div class="info-value"><span class="info-status ${stagingWarn ? 'info-warning' : ''}">${staging.free_bytes ? `${esc((freeBytes / (1024 * 1024 * 1024)).toFixed(1))} GB free` : 'Unavailable'}</span></div></div>
      <div class="info-row"><span>TMDB</span><div class="info-value"><span class="info-status">${statusDotHTML(tmdb, !tmdb)}<strong>${tmdb ? 'configured' : 'missing'}</strong></span></div></div>
      <div class="info-row"><span>Discord</span><div class="info-value"><span class="info-status">${statusDotHTML(discord, !discord)}<strong>${discord ? 'configured' : 'missing'}</strong></span></div></div>
      <div class="info-row"><span>Rips</span><div class="info-value"><strong>${done} done · ${errors} error</strong></div></div>
    </div>
  `;
}

function updateInfoUptime() {
  if (!document.getElementById('info-card')) return;
  const card = document.getElementById('info-card');
  const start = card.dataset.startedAt;
  if (!start) return;
  const started = new Date(start);
  if (Number.isNaN(started.getTime())) return;
  const uptime = fmtUptime((Date.now() - started.getTime()) / 1000);
  const uptimeNode = card.querySelector('.info-row:nth-child(2) .info-value strong');
  if (uptimeNode) uptimeNode.textContent = uptime;
}

async function loadSystemInfo() {
  try {
    const response = await fetch('/api/info');
    if (!response.ok) throw new Error('system info request failed');
    const info = await response.json();
    const card = document.getElementById('info-card');
    if (card) {
      card.dataset.startedAt = info.started_at || '';
    }
    renderInfoCard(info);
    updateInfoUptime();
  } catch (_) {
    renderInfoCard(null);
  }
}

// ── Jobs list ────────────────────────────────────────────────────────────

async function loadJobs() {
  await fetchJobsPage(true);
}

async function fetchJobsPage(reset) {
  if (jobsLoadingMore) return;
  jobsLoadingMore = true;
  try {
    const offset = reset ? 0 : jobs.length;
    const r = await fetch(`/api/jobs?limit=100&offset=${offset}`);
    if (!r.ok) return;
    const page = await r.json() || [];
    jobs = reset ? page : jobs.concat(page);
    jobsHasMore = page.length === 100;
    renderJobs();
    jobs.forEach(j => { if (j.Device) ensureDevice(j.Device); });
  } catch (_) {
    // Network error: keep the current list; the next refresh retries.
  }
  finally {
    jobsLoadingMore = false;
    updateLoadMoreButton();
  }
}

function updateLoadMoreButton() {
  const button = document.getElementById('load-more-jobs');
  button.style.display = jobsHasMore ? 'block' : 'none';
  button.disabled = jobsLoadingMore;
}

async function loadDevices() {
  try {
    const r = await fetch('/api/devices');
    if (!r.ok) return;
    const devices = await r.json() || [];
    devices.forEach(ensureDevice);
    await loadAutoEject();
  } catch (_) {
    // Network error: drive cards appear once status arrives over the WebSocket.
  }
}

function renderJobs() {
  updateTabBadges();
  const el = document.getElementById('jobs-table');
  const historical = jobs.filter(j => isTerminal(j.Status));
  document.getElementById('clear-jobs').style.display = historical.length ? '' : 'none';
  updateLoadMoreButton();
  const focused = detailPanel && detailPanel.contains(document.activeElement)
    ? document.activeElement
    : null;
  const selectionStart = focused && typeof focused.selectionStart === 'number' ? focused.selectionStart : null;
  const selectionEnd = focused && typeof focused.selectionEnd === 'number' ? focused.selectionEnd : null;
  if (detailPanel) detailPanel.remove();
  if (!historical.length) {
    el.innerHTML = '<div class="jobs-empty">No completed rips yet.</div>';
    return;
  }
  el.innerHTML = historical.map(j => {
    const title = j.Title || j.DiscLabel || '—';
    const year  = j.Year && !title.includes(`(${j.Year})`) ? ` (${j.Year})` : '';
    const isOpen = j.ID === selectedJobId;

    return `
      <div class="job-entry" data-entry-id="${escAttr(j.ID)}">
      <button type="button" class="job-row${isOpen ? ' selected' : ''}" id="job-row-${escAttr(j.ID)}"
        data-id="${escAttr(j.ID)}" aria-expanded="${isOpen}" aria-controls="detail-panel">
        <div class="job-title-col">
          <div class="job-title-text">${esc(title)}${esc(year)}</div>
        </div>
        <span class="job-meta">${esc(j.Device || '')}</span>
        <span class="job-meta">${j.DiscType && j.DiscType !== 'unknown' ? esc(fmtDiscType(j.DiscType)) : ''}</span>
        <span class="job-meta">${fmtDate(j.FinishedAt || j.CreatedAt)}</span>
        ${j.Status === 'error' && (j.ErrorSummary || j.ErrorHint)
          ? `<span class="badge badge-error has-tip" data-tip="${escAttr([j.ErrorSummary, j.ErrorHint && 'Try this: ' + j.ErrorHint].filter(Boolean).join('\n\n'))}">error ⓘ</span>`
          : badgeHTML(j.Status)}
        <span class="job-chevron" aria-hidden="true">›</span>
      </button>
      <button type="button" class="btn job-delete" data-id="${escAttr(j.ID)}"
        title="Delete from history" aria-label="Delete rip from history">🗑</button>
      </div>`;
  }).join('');
  el.querySelectorAll('.job-row').forEach(row => {
    row.addEventListener('click', () => openJob(row.dataset.id));
  });
  el.querySelectorAll('.job-delete').forEach(btn => {
    btn.addEventListener('click', e => { e.stopPropagation(); deleteJob(btn.dataset.id); });
  });
  if (detailPanel && selectedJobId) {
    const entry = Array.from(el.querySelectorAll('.job-entry'))
      .find(item => item.dataset.entryId === selectedJobId);
    if (entry) entry.appendChild(detailPanel);
  }
  if (focused && focused.isConnected) {
    focused.focus({ preventScroll: true });
    if (selectionStart !== null && selectionEnd !== null) focused.setSelectionRange(selectionStart, selectionEnd);
  }
}

document.getElementById('load-more-jobs').addEventListener('click', () => fetchJobsPage(false));

async function deleteJob(id) {
  if (!confirm('Delete this rip from history? Delivered files are not touched.')) return;
  const r = await fetch(`/api/jobs/${encodeURIComponent(id)}`, { method: 'DELETE' });
  if (!r.ok && r.status !== 404) { alert('Delete failed'); return; }
  if (selectedJobId === id) { selectedJobId = null; if (detailPanel) detailPanel.remove(); }
  await loadJobs();
}

document.getElementById('clear-jobs').addEventListener('click', async () => {
  if (!confirm('Clear all finished rips from history? Delivered files are not touched.')) return;
  const r = await fetch('/api/jobs', { method: 'DELETE' });
  if (!r.ok) { alert('Clear failed'); return; }
  selectedJobId = null;
  if (detailPanel) detailPanel.remove();
  await loadJobs();
});

// ── Detail panel ─────────────────────────────────────────────────────────

async function openJob(id) {
  if (selectedJobId === id) {
    const row = document.getElementById(`job-row-${CSS.escape(id)}`);
    selectedJobId = null;
    if (row) {
      row.setAttribute('aria-expanded', 'false');
      row.classList.remove('selected');
    }
    if (detailPanel) {
      const panel = detailPanel;
      panel.inert = true;
      panel.classList.remove('open');
      const finishCollapse = () => {
        if (!selectedJobId && !panel.classList.contains('open')) panel.remove();
      };
      const onTransitionEnd = event => {
        if (event.target === panel && event.propertyName === 'opacity') {
          panel.removeEventListener('transitionend', onTransitionEnd);
          finishCollapse();
        }
      };
      panel.addEventListener('transitionend', onTransitionEnd);
      setTimeout(() => {
        panel.removeEventListener('transitionend', onTransitionEnd);
        finishCollapse();
      }, 250);
    }
    return;
  }
  selectedJobId = id;
  if (!detailPanel) {
    detailPanel = document.createElement('div');
    detailPanel.id = 'detail-panel';
    detailPanel.className = 'job-detail';
    detailPanel.setAttribute('role', 'region');
    detailPanel.setAttribute('aria-label', 'Rip details');
  }
  detailPanel.inert = false;
  detailPanel.classList.remove('open');
  detailPanel.innerHTML = '<div class="job-detail-inner"><div class="detail-panel" style="color:var(--text-muted)">Loading…</div></div>';
  renderJobs();
  const row = document.getElementById(`job-row-${CSS.escape(id)}`);
  if (row) row.scrollIntoView({ block: 'nearest' });
  requestAnimationFrame(() => {
    if (selectedJobId === id && detailPanel) detailPanel.classList.add('open');
  });
  try {
    const r = await fetch(`/api/jobs/${encodeURIComponent(id)}`);
    if (!r.ok) throw new Error('not found');
    const { job, events } = await r.json();
    if (selectedJobId !== id) return;
    renderDetail(job, events || []);
  } catch (_) {
    if (selectedJobId === id) {
      detailPanel.innerHTML = `<div class="job-detail-inner"><div class="detail-panel"><div style="color:var(--red);padding:4px">Failed to load job.</div></div></div>`;
    }
  }
}

function alternatesHTML(job, events) {
  const ev = [...events].reverse().find(e => e.Stage === 'alternates');
  if (!ev) return '';
  let data = ev.Data;
  try { if (typeof data === 'string') data = JSON.parse(data); } catch (_) { return ''; }
  const cands = (data && data.candidates) || [];
  if (!cands.length) return '';
  const rows = cands.map(c => `
    <div class="tl-event">
      <span class="tl-msg">Title ${esc(String(c.index))} — ${esc(String(c.minutes))} min — ${esc(c.label || '')}</span>
      <button class="btn alt-rip" data-job="${esc(job.ID)}" data-idx="${esc(String(c.index))}">Rip this cut</button>
    </div>`).join('');
  return `<div class="reid-section"><div class="reid-heading">Alternate cuts</div>${rows}<div id="alt-msg"></div></div>`;
}

function tvIdentificationSummary(rawData) {
  let data = rawData;
  try { if (typeof data === 'string') data = JSON.parse(data); } catch (_) { return ''; }
  if (!data || data.action !== 'tv_identification') return '';
  const best = Array.isArray(data.candidates) ? data.candidates[0] : null;
  const candidate = best
    ? `${esc(best.title)}${best.year ? ` (${esc(String(best.year))})` : ''} · title similarity ${Number(best.title_similarity || 0).toFixed(2)} from “${esc(best.matched_query || data.query || '')}”${best.runtime_fit ? ` · runtimes ${esc(best.runtime_fit)}` : ''}`
    : 'no TMDB series candidate';
  const reason = data.show_reason ? ` (${esc(data.show_reason)})` : '';
  const season = data.suggested_season
    ? `suggested ${esc(String(data.suggested_season))} from earlier disc ${esc(data.earlier_disc_label || '')}`
    : esc(data.season_confidence || 'unresolved');
  return `<div class="tl-tv-summary">TV evidence: show ${esc(data.show_confidence || 'unresolved')}${reason} — ${candidate}; season ${season}; episodes ${esc(data.episode_confidence || 'unresolved')}${data.lookup_error ? `; lookup issue: ${esc(data.lookup_error)}` : ''}. Similarity is not a probability.</div>`;
}

async function ripAlternate(jobId, idx) {
  try {
    const r = await fetch(`/api/jobs/${encodeURIComponent(jobId)}/alternates/${encodeURIComponent(idx)}/rip`, { method: 'POST' });
    const d = await r.json();
    showToast(r.ok ? 'Ripping the alternate cut. Progress shows on the drive card.' : (d.error || 'Failed'));
  } catch (_) { showToast('Request failed.'); }
}

document.addEventListener('click', e => {
  const b = e.target.closest && e.target.closest('.alt-rip');
  if (b) ripAlternate(b.dataset.job, b.dataset.idx);
});

function renderDetail(job, events) {
  const panel = detailPanel;
  const title = job.Title || job.DiscLabel || '—';
  const year  = job.Year ? ` (${job.Year})` : '';

  const timelineHTML = events.length === 0
    ? '<div style="color:var(--text-muted);font-size:12px;padding:4px 0">No events recorded.</div>'
    : events.map((e, i) => {
        const rawData = e.Data;
        const hasData = rawData && rawData !== 'null' && rawData !== '{}' && rawData !== 'undefined';
        return `
          <div class="tl-event" data-i="${i}">
            <span class="tl-time">${esc(fmtTime(e.CreatedAt))}</span>
            ${badgeHTML(e.Stage)}
            <span class="tl-msg">${esc(e.Message)}</span>
            ${tvIdentificationSummary(rawData)}
            ${hasData ? `<div class="tl-data" id="tdata-${i}">${esc(fmtJSON(rawData))}</div>` : ''}
          </div>`;
      }).join('');

  panel.innerHTML = `
    <div class="job-detail-inner"><div class="detail-panel">
      <div class="detail-header">
        <div>
          <div class="detail-title">${esc(title)}${esc(year)}</div>
          <div class="detail-meta">
            ${badgeHTML(job.Status)}
            <span>${esc(job.Device || '')}</span>
            ${job.DiscType && job.DiscType !== 'unknown' ? `<span class="badge badge-disc-type">${esc(fmtDiscType(job.DiscType))}</span>` : ''}
            <span>${fmtDate(job.CreatedAt)}</span>
          </div>
        </div>
      </div>
      <div class="timeline">${timelineHTML}</div>
      ${alternatesHTML(job, events)}
      ${['done', 'error', 'cancelled'].includes(String(job.Status || '').toLowerCase()) ? `<div class="reid-section">
        <div class="reid-heading">Re-identify</div>
        <div class="search-row">
          <input class="search-input" id="reid-q" type="text"
            aria-label="Search TMDB for the correct title"
            placeholder="Search TMDB…"
            value="${escAttr(job.Title || job.DiscLabel || '')}" />
          <button class="btn btn-primary" id="reid-btn">Search</button>
        </div>
        <div id="reid-results" class="search-results"></div>
        <div id="reid-msg"></div>
      </div>` : ''}
    </div></div></div>`;

  panel.querySelectorAll('.tl-event').forEach(row => {
    row.addEventListener('click', () => {
      const dd = document.getElementById(`tdata-${row.dataset.i}`);
      if (dd) dd.classList.toggle('open');
    });
  });

  const qInput  = document.getElementById('reid-q');
  if (!qInput) return;
  const btn     = document.getElementById('reid-btn');
  const results = document.getElementById('reid-results');
  const msg     = document.getElementById('reid-msg');

  async function doSearch() {
    const q = qInput.value.trim();
    if (!q) return;
    btn.disabled = true;
    results.innerHTML = '<div class="msg-info">Searching…</div>';
    msg.innerHTML = '';
    try {
      const r = await fetch(`/api/search?q=${encodeURIComponent(q)}`);
      const data = await r.json();
      if (!r.ok) {
        results.innerHTML = '';
        msg.innerHTML = `<div class="msg-error">${esc(data.error || 'Search failed')}</div>`;
        return;
      }
      if (!data.length) {
        results.innerHTML = '<div class="msg-info">No results found.</div>';
        return;
      }
      results.innerHTML = data.map(r => `
        <div class="sr-row">
          <div>
            <div class="sr-title">${esc(r.title)}</div>
            <div class="sr-meta">${mediaTypePill(r.media_type || 'movie')} ${esc(searchResultMeta(r))}</div>
          </div>
          <button class="sr-apply"
            data-id="${r.id}"
            data-title="${escAttr(r.title)}"
            data-year="${r.year || 0}"
            data-type="${escAttr(r.media_type || 'movie')}">Select</button>
        </div>`).join('');
      results.querySelectorAll('.sr-apply').forEach(applyBtn => {
        applyBtn.addEventListener('click', () => {
          const payload = selectionPayload(applyBtn);
          if (payload) applyReidentify(job.ID, payload);
        });
      });
    } catch (_) {
      results.innerHTML = '';
      msg.innerHTML = '<div class="msg-error">Search request failed.</div>';
    } finally {
      btn.disabled = false;
    }
  }

  btn.addEventListener('click', doSearch);
  qInput.addEventListener('keydown', e => { if (e.key === 'Enter') doSearch(); });
}

async function applyReidentify(jobId, payload) {
  const msg = document.getElementById('reid-msg');
  if (!msg) return;
  const results = document.getElementById('reid-results');
  msg.innerHTML = `<div class="msg-info">Applying ${esc(selectionLabel(payload))}…</div>`;
  try {
    const r = await fetch(`/api/jobs/${encodeURIComponent(jobId)}/reidentify`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const data = await r.json();
    if (!r.ok) {
      msg.innerHTML = `<div class="msg-error">${esc(data.error || 'Failed')}</div>`;
      return;
    }
    if (results) results.innerHTML = '';
    msg.innerHTML = `<div class="msg-success">Saved: ${esc(selectionLabel(payload))}. ${esc(data.message || '')}</div>`;
    const title = detailPanel && detailPanel.querySelector('.detail-title');
    if (title) title.textContent = `${data.Title || payload.title}${data.Year ? ` (${data.Year})` : ''}`;
    await loadJobs();
  } catch (_) {
    msg.innerHTML = '<div class="msg-error">Request failed.</div>';
  }
}

// ── WebSocket ────────────────────────────────────────────────────────────

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${proto}//${location.host}/ws/progress`);
  // On (re)connect the server replays every drive's state; refresh info too in
  // case the server restarted or events were missed while disconnected.
  ws.onopen = () => { loadSystemInfo(); loadAutoEject(); };
  ws.onmessage = e => {
    try { applyProgressEvent(JSON.parse(e.data)); } catch (_) { /* ignore malformed frames */ }
  };
  ws.onclose = () => setTimeout(connectWS, 3000);
}

// ── Boot ─────────────────────────────────────────────────────────────────

initTabs();
loadDevices();
loadJobs();
// Clicking the dimmed area around the drive panel closes it.
document.getElementById('drive-dialog').addEventListener('click', e => {
  if (e.target === e.currentTarget) e.currentTarget.close();
});
// Uptime ticks locally from started_at; no server round-trip needed.
setInterval(updateInfoUptime, 1000);
// Delivery reachability can change without any rip event; recheck when the
// user comes back to the tab rather than polling.
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') scheduleInfoRefresh();
});
connectWS();
