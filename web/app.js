'use strict';

const $ = (id) => document.getElementById(id);

const KEYS = {
  enter: '\r', esc: '\x1b', tab: '\t', stab: '\x1b[Z', bs: '\x7f', ctrlc: '\x03',
  up: '\x1b[A', down: '\x1b[B', right: '\x1b[C', left: '\x1b[D',
  '1': '1', '2': '2', '3': '3', y: 'y', n: 'n',
};
const STATUS_LABEL = { active: 'работает', blocked: 'ждёт тебя', completed: 'готово', idle: '' };
// Claude Code prefixes its terminal title with a spinner glyph; it is noise in a list.
const GLYPH_PREFIX = /^[⠀-⣿✢-✽◐-◓·*•\s]+/u;
const TEXT_POLL_MS = 1500;
const TREE_POLL_MS = 15000;
const LIMITS_POLL_MS = 60000;

const state = {
  workspaces: [],
  cur: null,          // { window, session, pane } — replaced, never mutated, so in-flight reads can tell it changed
  gen: 0,             // bumped on every view change; reads from an older generation are dropped
  treeSeq: 0,
  treeApplied: 0,
  sending: false,
  textBusy: false,
  lastText: null,
  limits: null,
  limitsBusy: false,
  limitsOkAt: 0,      // Date.now() of the last successful /api/limits
  limitsErr: '',      // message of the last failed one, '' after a success
  font: load('font', 11),
  wrap: load('wrap', true),
  history: load('history', false),
};

function load(k, def) {
  try { const v = localStorage.getItem('aw.' + k); return v === null ? def : JSON.parse(v); } catch { return def; }
}
function save(k, v) {
  try { localStorage.setItem('aw.' + k, JSON.stringify(v)); } catch { /* private mode */ }
}

async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: { 'X-Agterm-Web': '1', ...(opts.body ? { 'Content-Type': 'application/json' } : {}) },
    credentials: 'same-origin',
    cache: 'no-store',
  });
  let data = null;
  try { data = await res.json(); } catch { /* empty */ }
  if (!res.ok) {
    if (res.status === 401) location.reload();
    throw new Error((data && data.error) || `HTTP ${res.status}`);
  }
  return data;
}

function cleanName(s) {
  const n = (s.name || '').replace(GLYPH_PREFIX, '').trim();
  return n || s.program || 'сессия';
}

function shortCwd(p) {
  if (!p) return '';
  return p.replace(/^\/Users\/[^/]+/, '~');
}

function ago(ts) {
  if (!ts) return '';
  const s = Math.max(0, Date.now() / 1000 - ts);
  if (s < 60) return 'только что';
  if (s < 3600) return `${Math.floor(s / 60)} мин назад`;
  if (s < 86400) return `${Math.floor(s / 3600)} ч назад`;
  return `${Math.floor(s / 86400)} д назад`;
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function allSessions() {
  const out = [];
  for (const ws of state.workspaces) for (const s of ws.sessions) out.push({ ...s, wsName: ws.name });
  return out;
}

function findSession(win, id) {
  return allSessions().find((s) => s.window === win && s.id === id) || null;
}

/* ---------- list ---------- */

function card(s, showWs) {
  const b = el('button', 'card');
  b.type = 'button';
  b.append(el('span', `dot ${s.status}`));
  const body = el('span', 'card-body');
  body.append(el('div', 'card-name', cleanName(s)));
  const sub = [showWs ? s.wsName : null, s.program, shortCwd(s.cwd), s.remoteHost ? `@${s.remoteHost}` : null]
    .filter(Boolean).join(' · ');
  body.append(el('div', 'card-sub', sub));
  b.append(body);
  if (s.status !== 'idle') {
    b.append(el('span', `badge ${s.status}`, STATUS_LABEL[s.status]));
  } else if (s.unseen) {
    b.append(el('span', 'badge', String(s.unseen)));
  }
  b.addEventListener('click', () => openSession(s.window, s.id));
  return b;
}

function group(title, sessions, showWs) {
  const g = el('div', 'group');
  g.append(el('h2', null, title));
  const c = el('div', 'cards');
  for (const s of sessions) c.append(card(s, showWs));
  g.append(c);
  return g;
}

function renderList() {
  const list = $('list');
  list.replaceChildren();
  const all = allSessions();
  if (!all.length) {
    list.append(el('div', 'empty', 'Сессий нет'));
    return;
  }
  const rank = { blocked: 0, completed: 1 };
  const attention = all
    .filter((s) => s.status === 'blocked' || s.status === 'completed')
    .sort((a, b) => rank[a.status] - rank[b.status] || (b.statusChangedAt || 0) - (a.statusChangedAt || 0));
  if (attention.length) list.append(group('Нужно внимание', attention, true));

  const multiWindow = new Set(state.workspaces.map((w) => w.window)).size > 1;
  for (const ws of state.workspaces) {
    if (!ws.sessions.length) continue;
    const title = multiWindow ? `${ws.name} · ${ws.windowName}` : ws.name;
    list.append(group(title, ws.sessions.map((s) => ({ ...s, wsName: ws.name })), false));
  }
}

async function loadTree() {
  const seq = ++state.treeSeq;
  try {
    const data = await api('/api/tree');
    if (seq < state.treeApplied) return; // a newer response already landed
    state.treeApplied = seq;
    state.workspaces = data.workspaces || [];
    $('error').hidden = true;
    renderList();
    if (state.cur) renderSessionHeader();
    updateTitle();
  } catch (e) {
    if (seq < state.treeApplied) return;
    $('error').textContent = `Не удалось получить сессии: ${e.message}`;
    $('error').hidden = false;
  }
}

function updateTitle() {
  const n = allSessions().filter((s) => s.status === 'blocked').length;
  document.title = n ? `(${n}) agterm` : 'agterm';
}

let treeTimer = null;
function scheduleTree(delay = 250) {
  clearTimeout(treeTimer);
  treeTimer = setTimeout(loadTree, delay);
}

/* ---------- limits ---------- */

const WEEKDAYS = ['вс', 'пн', 'вт', 'ср', 'чт', 'пт', 'сб'];
const MONTHS = ['янв', 'фев', 'мар', 'апр', 'мая', 'июн', 'июл', 'авг', 'сен', 'окт', 'ноя', 'дек'];
const CODEX_OLD_MS = 10 * 60 * 1000;

const pad2 = (n) => String(n).padStart(2, '0');
const hm = (d) => `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
const dayStart = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate());

// Same wording as the menu bar (limitsfmt.go): local wall time of the reset.
function resetText(iso, now = new Date()) {
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return '';
  if (t <= now) return 'сброшен';
  t.setTime(Math.round(t.getTime() / 60000) * 60000); // shown to the nearest minute; expiry stays exact
  const days = Math.round((dayStart(t) - dayStart(now)) / 86400000);
  if (days === 0) return `сброс сегодня ${hm(t)}`;
  if (days === 1) return `сброс завтра ${hm(t)}`;
  if (days <= 6) return `сброс ${WEEKDAYS[t.getDay()]} ${hm(t)}`;
  return `сброс ${t.getDate()} ${MONTHS[t.getMonth()]}`;
}

// "15:12" today, "8 ноя 15:12" otherwise.
function clockText(iso, now = new Date()) {
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return '';
  return dayStart(t).getTime() === dayStart(now).getTime() ? hm(t) : `${t.getDate()} ${MONTHS[t.getMonth()]} ${hm(t)}`;
}

function level(pct) {
  if (pct >= 90) return 'danger';
  if (pct >= 70) return 'warn';
  return 'ok';
}

// One window: "5 часов · 8% · сброс сегодня 18:40" over a bar. Stale numbers get "~".
function limitRow(w, stale) {
  const expired = w.expired || (w.resetsAt && Date.parse(w.resetsAt) <= Date.now());
  const pct = expired ? 0 : Math.round(w.pct || 0);
  const row = el('div', stale ? 'lim-win stale' : 'lim-win');
  const line = el('div', 'lim-line');
  line.append(el('span', 'lim-k', w.label || '—'), ' · ');
  line.append(el('span', `lim-pct ${level(pct)}`, `${stale ? '~' : ''}${pct}%`));
  const r = w.resetsAt ? resetText(w.resetsAt) : '';
  if (r) line.append(' · ', el('span', 'lim-reset', r));
  row.append(line);
  const meter = el('div', 'meter');
  const fill = el('span', `fill ${level(pct)}`);
  fill.style.width = `${Math.min(100, Math.max(0, pct))}%`;
  meter.append(fill);
  row.append(meter);
  return row;
}

function limitBlock(name, tags, note) {
  const b = el('div', 'lim-acct');
  const head = el('div', 'lim-head');
  head.append(el('span', 'lim-name', name));
  for (const [cls, text] of tags) head.append(el('span', `badge ${cls}`, text));
  if (note) head.append(el('span', 'lim-note', note));
  b.append(head);
  return b;
}

function renderLimits() {
  const box = $('limits');
  const data = state.limits;
  box.replaceChildren();
  if (!data && !state.limitsErr) { box.hidden = true; return; }
  box.append(el('h2', null, 'Лимиты (использовано)'));
  const c = el('div', 'cards');
  if (data) {
    if (data.claudeError) {
      const b = limitBlock('Claude', [], '');
      b.append(el('div', 'lim-err', data.claudeError));
      c.append(b);
    }
    for (const a of data.claude || []) {
      const tags = [];
      if (a.active) tags.push(['active', 'активный']);
      if (a.disabled) tags.push(['', 'выключен']);
      const b = limitBlock(a.label, tags, a.stale && a.asOf ? `данные от ${clockText(a.asOf)}` : '');
      const wins = [a.fiveHour, a.sevenDay, ...(a.scoped || []).filter((w) => w.pct >= 1)].filter(Boolean);
      for (const w of wins) b.append(limitRow(w, a.stale));
      if (a.error) b.append(el('div', 'lim-err', a.error));
      else if (!wins.length) b.append(el('div', 'lim-note', 'нет данных'));
      c.append(b);
    }
    const cx = data.codex;
    const old = cx && Date.now() - Date.parse(cx.asOf) > CODEX_OLD_MS;
    const b = limitBlock('Codex', [], old ? `данные от ${clockText(cx.asOf)}` : '');
    for (const w of (cx && cx.windows) || []) b.append(limitRow(w, false));
    if (!cx || !(cx.windows || []).length) b.append(el('div', 'lim-note', 'нет данных'));
    c.append(b);
  }
  // A failed refresh must not pass the old numbers off as current.
  const foot = el('div', 'lim-foot');
  if (state.limitsErr) {
    foot.classList.add('bad');
    foot.textContent = state.limitsOkAt
      ? `Не обновлялось ${Math.max(1, Math.round((Date.now() - state.limitsOkAt) / 60000))} мин: ${state.limitsErr}`
      : `Не удалось получить лимиты: ${state.limitsErr}`;
  } else if (data) {
    foot.textContent = `Обновлено в ${clockText(data.fetchedAt)}`;
  }
  c.append(foot);
  if (data && data.bot) {
    c.append(el('div', data.bot.bad ? 'lim-foot bot bad' : 'lim-foot bot', data.bot.text));
  }
  box.append(c);
  box.hidden = false;
}

async function loadLimits() {
  if (state.limitsBusy || document.hidden || state.cur) return;
  state.limitsBusy = true;
  try {
    state.limits = await api('/api/limits');
    state.limitsOkAt = Date.now();
    state.limitsErr = '';
  } catch (e) {
    state.limitsErr = e.message || 'ошибка';
  } finally {
    state.limitsBusy = false;
    renderLimits();
  }
}

/* ---------- session ---------- */

function setCur(cur) {
  state.cur = cur;
  state.gen++;
  state.lastText = null;
  state.textBusy = false; // the in-flight read belongs to the old generation and will be dropped
}

function openSession(win, id, push = true) {
  setCur({ window: win, session: id, pane: 'left' });
  $('screen').textContent = '';
  $('s-error').hidden = true;
  $('menu').hidden = true;
  $('list-view').hidden = true;
  $('session-view').hidden = false;
  if (push) history.pushState({ fromList: true }, '', `#s/${win}/${id}`);
  renderSessionHeader();
  pollText(true);
}

function goBack() {
  // Opened from the list: pop our own history entry. Opened by a direct link: there is
  // no list entry behind us, so switch to the list in place.
  if (history.state && history.state.fromList) history.back();
  else {
    history.replaceState({}, '', location.pathname);
    closeSession(false);
  }
}

function closeSession(push = true) {
  setCur(null);
  $('session-view').hidden = true;
  $('list-view').hidden = false;
  if (push) history.pushState({}, '', location.pathname);
  scheduleTree(0);
  loadLimits();
}

function renderSessionHeader() {
  const s = findSession(state.cur.window, state.cur.session);
  if (!s) {
    $('s-name').textContent = 'Сессия закрыта';
    $('s-meta').textContent = '';
    $('panes').hidden = true;
    return;
  }
  $('s-name').textContent = cleanName(s);
  const meta = [STATUS_LABEL[s.status], s.program, shortCwd(s.cwd)].filter(Boolean);
  if (s.status !== 'idle' && s.statusChangedAt) meta.splice(1, 0, ago(s.statusChangedAt));
  $('s-meta').textContent = meta.join(' · ');

  // Panes are always named explicitly: without --pane, `session text` reads the on-screen
  // pane while `session type` writes to the primary one, and they can differ.
  const panes = [];
  if (s.hasSplit) {
    const vertical = s.splitAxis !== 'horizontal';
    panes.push(['left', vertical ? 'Левая' : 'Верхняя'], ['right', vertical ? 'Правая' : 'Нижняя']);
  }
  if (s.hasScratch) {
    if (!panes.length) panes.push(['left', 'Основная']);
    panes.push(['scratch', 'Scratch']);
  }
  if (state.cur.pane !== 'left' && !panes.some(([v]) => v === state.cur.pane)) {
    setCur({ ...state.cur, pane: 'left' }); // the split or scratch went away
    $('screen').textContent = '';
    $('s-error').hidden = true;
    pollText(true);
  }
  const box = $('panes');
  box.replaceChildren();
  for (const [val, label] of panes) {
    const b = el('button', state.cur.pane === val ? 'on' : '', label);
    b.type = 'button';
    b.addEventListener('click', () => {
      if (state.cur.pane === val) return;
      setCur({ ...state.cur, pane: val });
      $('screen').textContent = '';
      $('s-error').hidden = true;
      renderSessionHeader();
      pollText(true);
    });
    box.append(b);
  }
  box.hidden = panes.length === 0;
}

function trimText(t) {
  const lines = t.split('\n').map((l) => l.replace(/\s+$/, ''));
  while (lines.length && !lines[lines.length - 1]) lines.pop();
  return lines.join('\n');
}

async function pollText(force = false) {
  if (!state.cur || state.textBusy) return;
  if (!force && document.hidden) return;
  const cur = state.cur;
  const gen = state.gen;
  state.textBusy = true;
  try {
    const q = new URLSearchParams({ window: cur.window, session: cur.session, pane: cur.pane });
    if (state.history) q.set('lines', '300');
    const data = await api(`/api/text?${q}`);
    if (state.gen !== gen) return;
    $('s-error').hidden = true;
    const text = trimText(data.text || '');
    if (text === state.lastText) return;
    const scr = $('screen');
    const atBottom = state.lastText === null || scr.scrollHeight - scr.scrollTop - scr.clientHeight < 40;
    scr.textContent = text;
    state.lastText = text;
    if (atBottom) scr.scrollTop = scr.scrollHeight;
  } catch (e) {
    if (state.gen === gen) {
      $('s-error').textContent = e.message;
      $('s-error').hidden = false;
    }
  } finally {
    if (state.gen === gen) state.textBusy = false;
  }
}

async function type(text, mode) {
  if (!state.cur) return false;
  const { window, session, pane } = state.cur;
  try {
    await api('/api/type', { method: 'POST', body: JSON.stringify({ window, session, pane, text, mode }) });
    setTimeout(() => pollText(true), 300);
    setTimeout(() => pollText(true), 1200);
    return true;
  } catch (e) {
    toast(`Не отправилось: ${e.message}`);
    return false;
  }
}

let toastTimer = null;
function toast(msg) {
  const t = $('toast');
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 3500);
}

/* ---------- events ---------- */

let es = null;
function connectEvents() {
  if (es) es.close();
  es = new EventSource('/api/events');
  es.onopen = () => { $('conn').className = 'conn ok'; };
  es.onerror = () => { $('conn').className = 'conn bad'; };
  es.onmessage = (m) => {
    let ev;
    try { ev = JSON.parse(m.data); } catch { return; }
    scheduleTree();
    if (ev.kind === 'status' && ev.payload && ev.payload.status === 'blocked' && ev.payload.previous !== 'blocked') {
      if (navigator.vibrate) navigator.vibrate([80, 60, 80]);
      const s = findSession(ev.window, ev.session);
      if (!state.cur || state.cur.session !== ev.session) toast(`Ждёт тебя: ${s ? cleanName(s) : ev.payload.name || 'сессия'}`);
    }
    if (state.cur && ev.session === state.cur.session) pollText(true);
  };
}

/* ---------- wiring ---------- */

function applyView() {
  const scr = $('screen');
  scr.style.fontSize = `${state.font}px`;
  scr.classList.toggle('wrap', state.wrap);
  $('wrap').checked = state.wrap;
  $('history').checked = state.history;
}

function autosize() {
  const t = $('input');
  t.style.height = 'auto';
  t.style.height = `${Math.min(t.scrollHeight, window.innerHeight * 0.4)}px`;
}

function routeFromHash(push) {
  const m = location.hash.match(/^#s\/([0-9A-Fa-f-]+)\/([0-9A-Fa-f-]+)$/);
  // agterm reports upper-case UUIDs; a hand-typed or lower-cased link must still match.
  if (m) openSession(m[1].toUpperCase(), m[2].toUpperCase(), push);
  else if (state.cur) closeSession(push);
}

function init() {
  applyView();
  $('back').addEventListener('click', goBack);
  $('menu-btn').addEventListener('click', () => { $('menu').hidden = !$('menu').hidden; });
  for (const b of document.querySelectorAll('[data-font]')) {
    b.addEventListener('click', () => {
      state.font = Math.min(18, Math.max(7, state.font + Number(b.dataset.font)));
      save('font', state.font);
      applyView();
    });
  }
  $('wrap').addEventListener('change', (e) => { state.wrap = e.target.checked; save('wrap', state.wrap); applyView(); });
  $('history').addEventListener('change', (e) => {
    state.history = e.target.checked; save('history', state.history); state.lastText = null; pollText(true);
  });
  for (const b of document.querySelectorAll('[data-key]')) {
    // pointerdown keeps the soft keyboard open when a key is tapped while typing
    b.addEventListener('pointerdown', (e) => e.preventDefault());
    b.addEventListener('click', () => type(KEYS[b.dataset.key], 'keys'));
  }
  const input = $('input');
  // Bumped on every edit and every view change, so an acknowledgement can tell whether
  // the textarea still holds the draft it sent — not a new one with the same text.
  let draftRev = 0;
  input.addEventListener('input', () => { draftRev++; autosize(); });
  // One send at a time: a double tap must not post the same prompt twice.
  async function sendDraft(mode) {
    if (state.sending) return;
    const text = input.value;
    if (mode === 'insert' && !text) return;
    // A blank Submit just presses Enter; Insert keeps whitespace as typed.
    const payload = mode === 'submit' && !text.trim() ? '' : text;
    const rev = draftRev;
    const gen = state.gen;
    state.sending = true;
    $('send').disabled = $('insert').disabled = true;
    try {
      const ok = await type(payload, mode);
      if (ok && draftRev === rev && state.gen === gen) { input.value = ''; autosize(); }
    } finally {
      state.sending = false;
      $('send').disabled = $('insert').disabled = false;
    }
  }
  $('composer').addEventListener('submit', (e) => { e.preventDefault(); sendDraft('submit'); });
  $('insert').addEventListener('pointerdown', (e) => e.preventDefault());
  $('insert').addEventListener('click', () => sendDraft('insert'));

  window.addEventListener('popstate', () => routeFromHash(false));
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) { loadTree(); pollText(true); loadLimits(); }
  });

  setInterval(() => pollText(), TEXT_POLL_MS);
  setInterval(() => { if (!document.hidden) loadTree(); }, TREE_POLL_MS);
  setInterval(loadLimits, LIMITS_POLL_MS);

  loadTree().then(() => { routeFromHash(false); loadLimits(); });
  connectEvents();
}

init();
