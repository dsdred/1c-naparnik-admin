'use strict';

const state = {
  initialized: false,
  authenticated: false,
  keys: [],
  tokens: [],
  overview: null,
  settings: null,
  listeners: [],
  connect: null,
  connectData: null,
  connectSel: { client: 'cline', tokenId: null, addr: null },
  connectSecrets: {},
  view: 'overview',
  keyID: null,
  usageRange: '24h',
  logOffset: 0,
  logTotal: 0,
  // logFilter — активный отбор журнала: токен и период. Пустые значения означают
  // «без ограничения», поэтому храним их же, а не null.
  logFilter: { tokenId: '', from: '', to: '' },
};

const app = document.getElementById('app');

/* ---------- HTTP ---------- */

async function api(path, opts = {}) {
  const headers = Object.assign({ 'X-Requested-With': 'naparnik-admin' }, opts.headers || {});
  if (opts.body && typeof opts.body !== 'string') {
    headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(opts.body);
  }
  const res = await fetch(path, Object.assign({}, opts, { headers }));
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (_) { data = { error: text }; }
  if (!res.ok) {
    const err = new Error((data && data.error) || ('HTTP ' + res.status));
    err.status = res.status;
    throw err;
  }
  return data;
}

/* ---------- утилиты ---------- */

const esc = (s) => String(s == null ? '' : s)
  .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  .replace(/"/g, '&quot;');

function fmtNum(n) {
  if (n == null) return '—';
  return new Intl.NumberFormat('ru-RU').format(n);
}

function fmtPct(v) {
  if (v == null) return '—';
  return (Math.round(v * 10) / 10).toString().replace('.', ',') + '%';
}

// plural выбирает русскую форму слова по числу: 1 точка, 2 точки, 5 точек.
function plural(n, one, few, many) {
  const a = Math.abs(n) % 100;
  const b = a % 10;
  if (a > 10 && a < 20) return many;
  if (b > 1 && b < 5) return few;
  if (b === 1) return one;
  return many;
}

function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d)) return '—';
  // Собираем вручную: toLocaleString('ru-RU') ставит запятую между датой и временем.
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getDate())}.${p(d.getMonth() + 1)} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function until(iso) {
  if (!iso) return '—';
  const ms = new Date(iso).getTime() - Date.now();
  if (isNaN(ms)) return '—';
  if (ms <= 0) return 'сброшено';
  const h = Math.floor(ms / 3600000);
  const m = Math.floor((ms % 3600000) / 60000);
  return h > 0 ? h + ' ч ' + m + ' мин' : m + ' мин';
}

function levelClass(v) {
  if (v == null) return '';
  if (v >= 95) return 'crit';
  if (v >= 80) return 'warn';
  return 'ok';
}

function bar(pct, cls) {
  const v = Math.min(100, Math.max(0, pct || 0));
  return '<div class="bar ' + (cls || levelClass(v)) + '"><span style="width:' + v + '%"></span></div>';
}

// Коды состояния ключа приходят из API Напарника — переводим их на русский.
const KEY_STATUS = {
  active: 'активен',
  unknown: 'не проверялся',
  error: 'ошибка',
  token_not_found: 'ключ не найден',
  invalid_token: 'ключ недействителен',
  no_token: 'ключ не принят',
  forbidden: 'нет доступа',
  unauthorized: 'не авторизован',
  connection_error: 'нет связи',
  disabled: 'отключён',
};

function statusLabel(code) {
  if (!code) return '—';
  return KEY_STATUS[code] || code;
}

// keyStatusText — статус ключа одной строкой: «активен», «отключён», «лимит исчерпан — …».
function keyStatusText(k) {
  if (k.disabled) return 'отключён';
  const label = k.status === 'active' ? 'активен' : statusLabel(k.status);
  return k.status_text ? label + ' — ' + k.status_text : label;
}

// Провайдеры = протоколы шлюза. Пусто — токен принимает любой.
const PROVIDERS = [
  { value: '', label: 'любой (по протоколу запроса)' },
  { value: 'openai', label: 'OpenAI Chat Completions' },
  { value: 'responses', label: 'OpenAI Responses' },
  { value: 'anthropic', label: 'Anthropic Messages' },
  { value: 'gemini', label: 'Gemini' },
];

function providerLabel(v) {
  const p = PROVIDERS.find((x) => x.value === v);
  return p ? p.label : (v || '—');
}

// Тарифы Напарника — фиксированный список.
const TARIFFS = ['Базовый', 'Проф', 'Корп'];

// tariffOpts отдаёт варианты для выпадающего списка, сохраняя нестандартное старое значение.
function tariffOpts(current) {
  const cur = (current || '').trim();
  const list = [{ value: '', label: '— не указан —' }].concat(TARIFFS.map((t) => ({ value: t, label: t })));
  if (cur && !TARIFFS.includes(cur)) list.push({ value: cur, label: cur });
  return list;
}

// ---------- выпадающий список ----------
// Нативный select в Windows рисует системный попап: он не стилизуется
// и может выехать за пределы окна. Поэтому список свой.

function ddHTML(id, opts, value, style, attrs) {
  const cur = opts.find((o) => o.value === value) || opts[0];
  return `<div class="dd" id="${id}" data-value="${esc(cur.value)}" style="${style || ''}" ${attrs || ''}>
    <button type="button" class="dd-btn" onclick="ddToggle('${id}', event)">
      <span class="dd-label">${esc(cur.label)}</span>
      <svg viewBox="0 0 24 24" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg>
    </button>
    <div class="dd-list" hidden>
      ${opts.map((o) => `<button type="button" class="dd-item${o.value === cur.value ? ' active' : ''}" data-value="${esc(o.value)}" onclick="ddPick('${id}', this)">${esc(o.label)}</button>`).join('')}
    </div>
  </div>`;
}

function ddToggle(id, ev) {
  ev.stopPropagation();
  const el = document.getElementById(id);
  if (!el) return;
  const list = el.querySelector('.dd-list');
  const willOpen = list.hidden;
  ddCloseAll();
  if (!willOpen) return;
  list.hidden = false;
  el.classList.add('open');
  const btn = el.querySelector('.dd-btn').getBoundingClientRect();
  const height = Math.min(240, list.scrollHeight);
  list.classList.toggle('up', btn.bottom + height + 10 > window.innerHeight && btn.top > height + 10);
}

function ddPick(id, item) {
  const el = document.getElementById(id);
  if (!el) return;
  el.dataset.value = item.dataset.value;
  el.querySelector('.dd-label').textContent = item.textContent;
  el.querySelectorAll('.dd-item').forEach((b) => b.classList.toggle('active', b === item));
  ddCloseAll();
  // Общий хук: страница может узнать о выборе через data-onchange на .dd.
  const hook = el.dataset.onchange;
  if (hook && typeof window[hook] === 'function') window[hook]();
}

function ddCloseAll() {
  document.querySelectorAll('.dd.open').forEach((el) => {
    el.classList.remove('open');
    const list = el.querySelector('.dd-list');
    if (list) list.hidden = true;
  });
}

function ddValue(id) {
  const el = document.getElementById(id);
  return el ? el.dataset.value : '';
}

document.addEventListener('click', ddCloseAll);
document.addEventListener('keydown', (ev) => { if (ev.key === 'Escape') ddCloseAll(); });

function toast(msg, isErr) {
  const el = document.createElement('div');
  el.className = 'card small';
  el.style.position = 'fixed';
  el.style.right = '20px';
  el.style.bottom = '20px';
  el.style.zIndex = 100;
  el.style.borderColor = isErr ? 'var(--crit)' : 'var(--ok)';
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 3200);
}

async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('Скопировано');
  } catch (_) {
    toast('Не удалось скопировать', true);
  }
}

// Тема: data-theme на <html>, выбор запоминается в localStorage.
const THEME_ICONS = {
  // Показываем иконку текущей темы: солнце — светлая, луна — тёмная.
  light: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4.2"/><path d="M12 2.5v2.2M12 19.3v2.2M4.2 4.2l1.6 1.6M18.2 18.2l1.6 1.6M2.5 12h2.2M19.3 12h2.2M4.2 19.8l1.6-1.6M18.2 5.8l1.6-1.6"/></svg>',
  dark: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z"/></svg>',
};

function isLightTheme() {
  return document.documentElement.getAttribute('data-theme') === 'light';
}

// syncThemeButtons обновляет иконку и подсказку у всех переключателей темы.
function syncThemeButtons() {
  const light = isLightTheme();
  const title = light ? 'Включить тёмную тему' : 'Включить светлую тему';
  document.querySelectorAll('[data-theme-toggle]').forEach((btn) => {
    btn.innerHTML = light ? THEME_ICONS.light : THEME_ICONS.dark;
    btn.title = title;
    btn.setAttribute('aria-label', title);
  });
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', light ? '#f2f5f9' : '#141B25');
}

function toggleTheme() {
  const next = isLightTheme() ? 'dark' : 'light';
  document.documentElement.setAttribute('data-theme', next);
  try { localStorage.setItem('na-theme', next); } catch (_) { /* приватный режим */ }
  syncThemeButtons();
}

// Бургер-меню: на узких экранах сайдбар уезжает за край и открывается поверх контента.
function toggleNav() {
  const open = document.body.classList.toggle('nav-open');
  const btn = document.querySelector('.nav-toggle');
  if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
}

function closeNav() {
  document.body.classList.remove('nav-open');
  const btn = document.querySelector('.nav-toggle');
  if (btn) btn.setAttribute('aria-expanded', 'false');
}

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') { closeNav(); closeInfo(); }
});

// Клик мимо значка «i» закрывает пояснение.
document.addEventListener('click', (e) => {
  if (!e.target.closest('.info-wrap')) closeInfo();
});

// infoBtn — значок «i» с всплывающим пояснением: подсказки не занимают место на странице.
function infoBtn(html, label) {
  const title = esc(label || 'Пояснение');
  return `<span class="info-wrap">
    <button type="button" class="info-btn" aria-label="${title}" aria-expanded="false" onclick="toggleInfo(event)">i</button>
    <span class="info-pop" role="tooltip" hidden>${html}</span>
  </span>`;
}

// closeInfo закрывает все подсказки, кроме указанной.
function closeInfo(except) {
  document.querySelectorAll('.info-pop').forEach((pop) => {
    if (pop === except) return;
    pop.hidden = true;
    const btn = pop.parentElement.querySelector('.info-btn');
    if (btn) btn.setAttribute('aria-expanded', 'false');
  });
}

function toggleInfo(e) {
  e.preventDefault();
  e.stopPropagation();
  const btn = e.currentTarget;
  const pop = btn.closest('.info-wrap').querySelector('.info-pop');
  const willOpen = pop.hidden;
  closeInfo(pop);
  pop.hidden = !willOpen;
  btn.setAttribute('aria-expanded', willOpen ? 'true' : 'false');
}

// confirmDialog — подтверждение в стиле админки вместо нативного confirm().
// Возвращает Promise<boolean>: true — «ОК», false — отмена/Esc/клик по фону.
function confirmDialog({ title, text, ok, cancel, danger }) {
  return new Promise((resolve) => {
    const wrap = document.createElement('div');
    wrap.className = 'modal-backdrop';
    wrap.innerHTML = `
      <div class="modal" style="max-width:430px">
        <h2 style="margin-top:0">${esc(title || 'Подтвердите действие')}</h2>
        <p class="muted" style="margin:0 0 18px">${esc(text || '')}</p>
        <div class="row" style="justify-content:flex-end">
          <button class="btn ghost" data-act="cancel">${esc(cancel || 'Отмена')}</button>
          <button class="btn ${danger ? 'danger' : ''}" data-act="ok">${esc(ok || 'Да')}</button>
        </div>
      </div>`;
    const close = (val) => {
      wrap.remove();
      document.removeEventListener('keydown', onKey);
      resolve(val);
    };
    const onKey = (e) => {
      if (e.key === 'Escape') { e.preventDefault(); close(false); }
      if (e.key === 'Enter') { e.preventDefault(); close(true); }
    };
    wrap.addEventListener('click', (e) => {
      if (e.target === wrap) return close(false);
      const act = e.target.closest('[data-act]');
      if (act) close(act.dataset.act === 'ok');
    });
    document.addEventListener('keydown', onKey);
    document.body.appendChild(wrap);
    const okBtn = wrap.querySelector('[data-act="ok"]');
    if (okBtn) okBtn.focus();
  });
}

/* ---------- запуск ---------- */

async function boot() {
  try {
    const s = await api('/api/state');
    state.initialized = s.initialized;
    state.authenticated = s.authenticated;
  } catch (_) {
    state.initialized = true;
    state.authenticated = false;
  }
  if (!state.initialized) return renderSetup();
  if (!state.authenticated) return renderLogin();
  await renderShell();
}

function renderSetup() {
  app.innerHTML = `
    <div class="login card">
      <div class="login-head">
        <div class="login-logo">${LOGO}</div>
        <div class="login-title">Напарник</div>
        <div class="login-sub">Админка шлюза</div>
      </div>
      <h1>Первый запуск</h1>
      <p class="muted small">Задайте пароль администратора. Он хранится в виде хеша argon2id.</p>
      <div class="grid">
        <input id="pw" type="password" placeholder="Пароль (не короче 6 символов)">
        <input id="pw2" type="password" placeholder="Повторите пароль">
        <button class="btn" onclick="setupPassword()">Сохранить и войти</button>
        <div class="error" id="err"></div>
      </div>
    </div>`;
}

async function setupPassword() {
  const pw = document.getElementById('pw').value;
  const pw2 = document.getElementById('pw2').value;
  const err = document.getElementById('err');
  if (pw !== pw2) { err.textContent = 'Пароли не совпадают'; return; }
  try {
    await api('/api/password', { method: 'POST', body: { password: pw } });
    await boot();
  } catch (e) { err.textContent = e.message; }
}

function renderLogin() {
  app.innerHTML = `
    <div class="login card">
      <div class="login-head">
        <div class="login-logo">${LOGO}</div>
        <div class="login-title">Напарник</div>
        <div class="login-sub">Админка шлюза</div>
      </div>
      <h1>Вход в админку</h1>
      <div class="grid">
        <input id="pw" type="password" placeholder="Пароль" onkeydown="if(event.key==='Enter')login()">
        <button class="btn" onclick="login()">Войти</button>
        <div class="error" id="err"></div>
      </div>
    </div>`;
  document.getElementById('pw').focus();
}

async function login() {
  const pw = document.getElementById('pw').value;
  try {
    await api('/api/login', { method: 'POST', body: { password: pw } });
    await boot();
  } catch (e) {
    document.getElementById('err').textContent = e.message;
  }
}

async function logout() {
  await api('/api/logout', { method: 'POST' });
  location.reload();
}

/* ---------- каркас ---------- */

const ICONS = {
  overview: '<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/>',  keys: '<circle cx="8" cy="8" r="4"/><path d="M11 11l9 9M17 17l2-2M20 20l2-2"/>',
  tokens: '<path d="M4 8a2 2 0 0 1 2-2h7l6 6-7 7-8-8z"/><circle cx="8.5" cy="9.5" r="1.2"/>',
  sessions: '<path d="M4 6h16M4 12h16M4 18h10"/>',
  logs: '<path d="M6 3h9l4 4v14H6z"/><path d="M9 12h6M9 16h6"/>',
  listeners: '<circle cx="12" cy="12" r="2.2"/><path d="M8.4 8.4a5.1 5.1 0 0 0 0 7.2M5.6 5.6a9.1 9.1 0 0 0 0 12.8"/><path d="M8.4 8.4a5.1 5.1 0 0 0 0 7.2M5.6 5.6a9.1 9.1 0 0 0 0 12.8" transform="translate(24 0) scale(-1 1)"/>',
  settings: '<path d="M4 7h9M17 7h3M4 17h3M11 17h9"/><circle cx="15" cy="7" r="2"/><circle cx="9" cy="17" r="2"/>',
  connect: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M7 9l3 3-3 3M12 15h5"/>',
};

// Логотип из Naparnik Admin Brand Kit v1.0: горизонтальный в сайдбаре и на входе,
// знак NA — в свёрнутом (узком) сайдбаре. Версия под тему выбирается в CSS.
const LOGO = `<span class="brand-logo" aria-hidden="true">
  <img class="bl bl-full bl-dark" src="/brand/logo-dark.svg" alt="Naparnik Admin">
  <img class="bl bl-full bl-light" src="/brand/logo-light.svg" alt="">
  <img class="bl bl-sym bl-dark" src="/brand/symbol-dark.svg" alt="">
  <img class="bl bl-sym bl-light" src="/brand/symbol-light.svg" alt="">
</span>`;

const VIEWS = [
  { group: 'Мониторинг', items: [['overview', 'Обзор'], ['keys', 'Ключи'], ['tokens', 'Токены']] },
  { group: 'Активность', items: [['sessions', 'Сессии'], ['logs', 'Журнал']] },
  { group: 'Система', items: [['listeners', 'Слушатели'], ['settings', 'Настройки'], ['connect', 'Подключение']] },
];

function navButton(id, title) {
  return `<button data-view="${id}" onclick="go('${id}')">
    <svg viewBox="0 0 24 24" fill="none" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">${ICONS[id] || ''}</svg>
    <span>${title}</span>
  </button>`;
}

async function renderShell() {
  app.innerHTML = `
    <div class="layout">
      <aside class="sidebar" id="sidebar">
        <div class="brand">${LOGO}</div>
        <div class="nav">
          ${VIEWS.map((v) => `<div class="nav-group">${v.group}</div>${v.items.map(([id, t]) => navButton(id, t)).join('')}`).join('')}
        </div>
        <div class="sidebar-footer">
          <button class="btn ghost small" style="width:100%" onclick="logout()">Выйти</button>
        </div>
      </aside>
      <div class="nav-backdrop" id="navBackdrop" onclick="closeNav()"></div>
      <div class="content">
        <header class="topbar">
          <button class="nav-toggle" onclick="toggleNav()" aria-label="Открыть меню" aria-controls="sidebar" aria-expanded="false">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M4 7h16M4 12h16M4 17h16"/></svg>
          </button>
          <div class="topbar-logo">${LOGO}</div>
          <button class="theme-toggle" data-theme-toggle onclick="toggleTheme()"></button>
        </header>
        <main class="main" id="main"></main>
      </div>
    </div>`;
  syncThemeButtons();
  await go(state.view);
}

async function go(view) {
  state.view = view;
  closeNav();
  document.querySelectorAll('.nav button').forEach((b) => {
    b.classList.toggle('active', b.dataset.view === view);
  });
  const main = document.getElementById('main');
  main.innerHTML = '<p class="muted">Загрузка…</p>';
  try {
    if (view === 'overview') await renderOverview(main);
    else if (view === 'keys') await renderKeys(main);
    else if (view === 'tokens') await renderTokens(main);
    else if (view === 'sessions') await renderSessions(main);
    else if (view === 'logs') await renderLogs(main);
    else if (view === 'listeners') await renderListeners(main);
    else if (view === 'settings') await renderSettings(main);
    else if (view === 'connect') await renderConnect(main);
    else if (view === 'key') await renderKeyUsage(main);
  } catch (e) {
    main.innerHTML = '<div class="card error">' + esc(e.message) + '</div>';
  }
}

/* ---------- обзор ---------- */

async function renderOverview(main) {
  const [ov, keys] = await Promise.all([api('/api/overview'), api('/api/keys')]);
  state.overview = ov;
  state.keys = keys;

  if (!keys.length) {
    main.innerHTML = `<h1>Обзор</h1>
      <div class="card">
        <p>Ключей Напарника пока нет.</p>
        <button class="btn" onclick="go('keys')">Добавить ключ</button>
      </div>`;
    return;
  }

  main.innerHTML = `
    <h1>Обзор</h1>
    <div class="grid cols3">
      <div class="card"><h3>Запросов за сутки</h3><div class="metric">${fmtNum(ov.requests_today)}</div></div>
      <div class="card"><h3>Токенов за сутки</h3><div class="metric">${fmtNum(ov.total_tokens_today)}</div></div>
      <div class="card"><h3>Ошибок за сутки</h3><div class="metric">${fmtNum(ov.errors_today)}</div></div>
      <div class="card"><h3>Ключей</h3><div class="metric">${fmtNum(keys.length)}<small> ${keys.filter((k) => !k.disabled).length} активных</small></div></div>
    </div>
    <div class="grid cols2">
      ${ov.keys.map(keyCard).join('')}
    </div>`;
}

function keyCard(k) {
  const c = k.current;
  const badge = k.key.disabled
    ? '<span class="badge crit">отключён</span>'
    : k.key.status === 'active'
      ? '<span class="badge ok">активен</span>'
      : '<span class="badge">' + esc(statusLabel(k.key.status)) + '</span>';
  const warn = k.level === 'crit'
    ? '<span class="badge crit">лимит почти исчерпан</span>'
    : k.level === 'warn'
      ? '<span class="badge warn">высокий расход</span>' : '';

  return `
    <div class="card">
      <div class="row">
        <strong>${esc(k.key.name)}</strong> ${badge} ${warn}
        <span class="spacer"></span>
        <button class="btn ghost small" onclick="openKey(${k.key.id})">Подробно</button>
      </div>
      ${c ? `
      <div style="margin-top:14px">
        <div class="row small muted"><span>5 часов</span><span class="spacer"></span>
          <span>${fmtPct(c.used_5h)} · осталось ${fmtPct(c.remaining_5h)} · сброс через ${until(c.resets_5h)}</span>
        </div>
        ${bar(c.used_5h)}
        <div class="row small muted" style="margin-top:12px"><span>Неделя</span><span class="spacer"></span>
          <span>${fmtPct(c.used_week)} · осталось ${fmtPct(c.remaining_week)} · сброс через ${until(c.resets_week)}</span>
        </div>
        ${bar(c.used_week)}
      </div>` : '<p class="muted small" style="margin-top:12px">Бюджет ещё не снят — нажмите «Обновить» в разделе «Ключи».</p>'}
      <div class="row small muted" style="margin-top:14px">
        <span>Запросов: ${fmtNum(k.requests_today)}</span>
        <span>Токенов: ${fmtNum(k.tokens_today)}</span>
        <span class="spacer"></span>
        <span>${esc(k.key.key_hint)}</span>
      </div>
    </div>`;
}

/* ---------- ключи ---------- */

async function renderKeys(main) {
  const [keys, tokens] = await Promise.all([api('/api/keys'), api('/api/tokens')]);
  state.keys = keys;
  state.tokens = tokens;

  main.innerHTML = `
    <h1>Ключи Напарника</h1>
    <div class="card">
      <h3>Добавить ключ</h3>
      <div class="row">
        <input id="kname" placeholder="Название (например, «Отдел разработки»)" style="flex:2;min-width:220px">
        <input id="kkey" placeholder="Ключ из code.1c.ai" style="flex:2;min-width:240px">
        ${ddHTML('ktariff', tariffOpts(''), '', 'flex:1;min-width:150px')}
        <button class="btn" onclick="addKey()">Добавить</button>
      </div>
    </div>
    <div class="card">
      <table>
        <thead><tr><th style="width:20px"></th><th>Название</th><th>Тариф</th><th>Токенов</th><th></th></tr></thead>
        <tbody>
          ${keys.map((k) => {
            const dot = k.disabled
              ? ['crit', 'отключён']
              : (k.status === 'active' ? ['ok', 'активен'] : ['warn', statusLabel(k.status)]);
            const tip = dot[1] + (k.status_text ? ': ' + k.status_text : '');
            return `
            <tr>
              <td><span class="dot ${dot[0]}" title="${esc(tip)}"></span></td>
              <td>${esc(k.name)}</td>
              <td class="small muted">${esc(k.tariff_label || '—')}</td>
              <td>${tokens.filter((t) => t.key_id === k.id).length}</td>
              <td class="actions">
                ${iconBtn('refresh', 'Обновить бюджет', `refreshKey(${k.id})`)}
                ${iconBtn('chart', 'Статистика', `openKey(${k.id})`)}
                ${iconBtn('edit', 'Изменить название, тариф или ключ', `openKeyEdit(${k.id})`)}
                ${iconBtn(k.disabled ? 'powerOn' : 'powerOff', k.disabled ? 'Включить' : 'Отключить', `toggleKey(${k.id}, ${!k.disabled})`)}
                ${iconBtn('trash', 'Удалить', `deleteKey(${k.id})`, 'danger')}
              </td>
            </tr>`;
          }).join('') || '<tr><td colspan="5" class="muted">Пока нет ключей</td></tr>'}
        </tbody>
      </table>
    </div>`;
}

const ACTION_ICONS = {
  refresh: '<path d="M20 12a8 8 0 1 1-2.3-5.6"/><path d="M20 4v4.5h-4.5"/>',
  chart: '<path d="M4 19V5"/><path d="M4 19h16"/><path d="M8 16v-4M12 16V8M16 16v-6"/>',
  edit: '<path d="M4 20h4l10-10-4-4L4 16z"/><path d="M14 6l4 4"/>',
  powerOff: '<path d="M12 4v8"/><path d="M7.5 7a7 7 0 1 0 9 0"/>',
  powerOn: '<path d="M12 4v8"/><path d="M7.5 7a7 7 0 1 0 9 0"/><path d="M9 13l2 2 4-4"/>',
  trash: '<path d="M4 7h16"/><path d="M9 7V5h6v2"/><path d="M6 7l1 13h10l1-13"/>',
  revoke: '<circle cx="12" cy="12" r="8"/><path d="M6.4 17.6L17.6 6.4"/>',
  copy: '<rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5h10"/>',
  download: '<path d="M12 4v10"/><path d="M8 11l4 4 4-4"/><path d="M5 19h14"/>',
  prev: '<path d="M14.5 5l-7 7 7 7"/>',
  next: '<path d="M9.5 5l7 7-7 7"/>',
  eye: '<path d="M2 12s3.6-6 10-6 10 6 10 6-3.6 6-10 6-10-6-10-6z"/><circle cx="12" cy="12" r="3"/>',
  rotate: '<path d="M20 12a8 8 0 1 1-2.34-5.66"/><path d="M20 3.5V9h-5.5"/>',
  lock: '<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>',
  deltas: '<path d="M4 12h2.5M10.75 12h2.5M17.5 12H20"/>',
  final: '<rect x="4" y="7" width="16" height="10" rx="2" fill="currentColor" stroke="none" opacity=".45"/><rect x="4" y="7" width="16" height="10" rx="2"/>',
  auto: '<path d="M7 8h9"/><path d="M13 5l3 3-3 3"/><path d="M17 16H8"/><path d="M11 13l-3 3 3 3"/>',
};

function iconBtn(name, title, onclick, cls, disabled) {
  const act = disabled ? 'disabled' : `onclick="${onclick}"`;
  return `<button class="btn icon ghost ${cls || ''}" title="${esc(title)}" aria-label="${esc(title)}" ${act}>
    <svg viewBox="0 0 24 24" fill="none" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">${ACTION_ICONS[name] || ''}</svg>
  </button>`;
}

// iconSvg — та же иконка, но без кнопки: для ячеек таблиц.
function iconSvg(name) {
  return `<svg class="ico" viewBox="0 0 24 24" fill="none" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">${ACTION_ICONS[name] || ''}</svg>`;
}

// streamIcon — режим потока картинкой: пунктир (по кускам), блок (целиком), стрелки (по умолчанию).
function streamIcon(mode) {
  if (mode === 'deltas') {
    return `<span title="deltas — каждое подтверждённое сообщение сразу, по мере готовности">${iconSvg('deltas')}</span>`;
  }
  if (mode === 'final') {
    return `<span title="final — ответ приходит целиком в конце">${iconSvg('final')}</span>`;
  }
  return `<span title="как в настройках шлюза">${iconSvg('auto')}</span>`;
}

async function addKey() {
  const name = document.getElementById('kname').value.trim();
  const key = document.getElementById('kkey').value.trim();
  const tariff = ddValue('ktariff');
  if (!key) return toast('Вставьте ключ Напарника', true);
  try {
    await api('/api/keys', { method: 'POST', body: { name, key, tariff_label: tariff } });
    toast('Ключ добавлен');
    go('keys');
  } catch (e) { toast(e.message, true); }
}

async function refreshKey(id) {
  try {
    await api('/api/keys/' + id + '/refresh', { method: 'POST' });
    toast('Бюджет обновлён');
    go(state.view === 'key' ? 'key' : 'keys');
  } catch (e) { toast(e.message, true); }
}

async function toggleKey(id, disabled) {
  try {
    await api('/api/keys/' + id, { method: 'PATCH', body: { disabled } });
    go('keys');
  } catch (e) { toast(e.message, true); }
}

async function deleteKey(id) {
  const ok = await confirmDialog({
    title: 'Удалить ключ?',
    text: 'Ключ удалится вместе с его токенами, сессиями и историей квоты. Отключить без удаления можно через диалог ключа.',
    ok: 'Удалить',
    danger: true,
  });
  if (!ok) return;
  try {
    await api('/api/keys/' + id, { method: 'DELETE' });
    toast('Ключ удалён');
    go('keys');
  } catch (e) { toast(e.message, true); }
}

// ---------- редактирование ключа ----------

async function openKeyEdit(id) {
  const k = (state.keys || []).find((x) => x.id === id);
  if (!k) return;
  // Ключ показываем сразу целиком: поле и есть ключ, менять — прямо в нём.
  let secret = '';
  try {
    secret = (await api('/api/keys/' + id + '/reveal', { method: 'POST' })).key || '';
  } catch (e) {
    toast('Не удалось расшифровать ключ: ' + e.message, true);
  }
  const wrap = document.createElement('div');
  wrap.className = 'modal-backdrop';
  wrap.id = 'key-modal';
  wrap.dataset.secret = secret;
  const tokenCount = (state.tokens || []).filter((t) => t.key_id === id).length;
  wrap.innerHTML = `
    <div class="modal">
      <div class="row" style="align-items:center;margin-bottom:14px">
        <h2 style="margin:0">Изменить ключ ${infoBtn('Измените значение ключа и сохраните — ключ заменится. Маску ключа в списке увидите, если не удалось его расшифровать. При доступе к админке по сети нужен HTTPS.', 'Про изменение ключа')}</h2>
        <span class="spacer"></span>
        <label class="switch" title="Включить или отключить ключ — шлюз перестанет отдавать его токенам">
          <input type="checkbox" id="e_enabled" ${k.disabled ? '' : 'checked'}>
          <span class="track"><span class="knob"></span></span>
          <span class="switch-label">включён</span>
        </label>
      </div>
      <div class="grid">
        <label>Название<input id="e_name" value="${esc(k.name)}"></label>
        <label>Тариф${ddHTML('e_tariff', tariffOpts(k.tariff_label), k.tariff_label || '', '')}</label>
        <label>Ключ Напарника
          <div class="row" style="gap:6px;flex-wrap:nowrap">
            <input id="e_key" class="mono" value="${esc(secret || k.key_hint)}" style="flex:1" ${secret ? '' : 'readonly'}>
            ${iconBtn('copy', 'Копировать ключ', 'copyKeyField()')}
          </div>
        </label>
      </div>
      <p class="muted small" style="margin:14px 0 0">Статус: ${esc(keyStatusText(k))} · токенов ${tokenCount}
        · создан ${fmtTime(k.created_at)}</p>
      <div class="row" style="justify-content:flex-end;margin-top:18px">
        <button class="btn ghost" onclick="closeKeyEdit()">Отмена</button>
        <button class="btn" onclick="saveKeyEdit(${k.id})">Сохранить</button>
      </div>
    </div>`;
  wrap.addEventListener('click', (ev) => { if (ev.target === wrap) closeKeyEdit(); });
  document.body.appendChild(wrap);
  document.getElementById('e_name').focus();
}

function closeKeyEdit() {
  const el = document.getElementById('key-modal');
  if (el) el.remove();
}

function copyKeyField() {
  const input = document.getElementById('e_key');
  if (!input) return;
  copy(input.value);
}

async function saveKeyEdit(id) {
  const name = document.getElementById('e_name').value.trim();
  const tariff = ddValue('e_tariff');
  const keyVal = document.getElementById('e_key').value.trim();
  const original = document.getElementById('key-modal').dataset.secret || '';
  const body = { name, tariff_label: tariff };
  if (keyVal && keyVal !== original) body.key = keyVal;
  const chk = document.getElementById('e_enabled');
  if (chk) body.disabled = !chk.checked;
  try {
    await api('/api/keys/' + id, { method: 'PATCH', body });
    closeKeyEdit();
    toast(body.key ? 'Ключ заменён' : 'Сохранено');
    go('keys');
  } catch (e) { toast(e.message, true); }
}

/* ---------- статистика ключа ---------- */

async function openKey(id) {
  state.keyID = id;
  await go('key');
}

async function renderKeyUsage(main) {
  const u = await api('/api/keys/' + state.keyID + '/usage?range=' + state.usageRange);
  const c = u.current;
  const e = u.estimate || {};

  main.innerHTML = `
    <div class="row"><button class="btn ghost small" onclick="go('keys')">← К списку</button></div>
    <h1>${esc(u.key.name)}</h1>

    <div class="tabs">
      ${['24h', '7d', '30d'].map((r) =>
        `<button class="${state.usageRange === r ? 'active' : ''}" onclick="setRange('${r}')">${r === '24h' ? 'Сутки' : r === '7d' ? 'Неделя' : 'Месяц'}</button>`).join('')}
    </div>

    ${c ? `
    <div class="grid cols2">
      <div class="card">
        <h3>Окно 5 часов</h3>
        <div class="metric">${fmtPct(c.used_5h)} <small>израсходовано</small></div>
        ${bar(c.used_5h)}
        <div class="row small muted" style="margin-top:8px">
          <span>осталось ${fmtPct(c.remaining_5h)}</span><span class="spacer"></span>
          <span>сброс ${fmtTime(c.resets_5h)} (через ${until(c.resets_5h)})</span>
        </div>
      </div>
      <div class="card">
        <h3>Недельное окно</h3>
        <div class="metric">${fmtPct(c.used_week)} <small>израсходовано</small></div>
        ${bar(c.used_week)}
        <div class="row small muted" style="margin-top:8px">
          <span>осталось ${fmtPct(c.remaining_week)}</span><span class="spacer"></span>
          <span>сброс ${fmtTime(c.resets_week)} (через ${until(c.resets_week)})</span>
        </div>
      </div>
    </div>` : '<div class="card muted">Замеров бюджета пока нет.</div>'}

    <div class="grid cols3">
      <div class="card"><h3>Запросов</h3><div class="metric">${fmtNum(u.usage.requests)}</div>
        <div class="small muted">успешных ${fmtPct(u.usage.success_pct)}</div></div>
      <div class="card"><h3>Токенов</h3><div class="metric">${fmtNum(u.usage.prompt_tokens + u.usage.completion_tokens)}</div>
        <div class="small muted">prompt ${fmtNum(u.usage.prompt_tokens)} · completion ${fmtNum(u.usage.completion_tokens)}</div></div>
      <div class="card"><h3>Средний ответ</h3><div class="metric">${fmtNum(u.usage.avg_latency_ms)} <small>мс</small></div></div>
    </div>

    <div class="grid cols2">
    <div class="card">
      <h3>Расход по времени</h3>
      ${chart(u.series)}
    </div>

    <div class="card">
      <h3>Оценка остатка</h3>
      ${e.percent_per_request
        ? `<div class="row"><span class="metric">${e.percent_per_request.toFixed(3).replace('.', ',')}%</span>
             <span class="muted">на запрос · примерно ${fmtNum(e.requests_left)} запросов до 100% недельного окна</span></div>
           ${e.exhaust_at ? '<p class="small muted">При текущем темпе окно закончится около ' + fmtTime(e.exhaust_at) + '</p>' : ''}`
        : '<p class="muted small">Недостаточно данных для оценки — нужно минимум два замера бюджета и хотя бы один запрос.</p>'}
      <p class="small muted" style="margin-bottom:0">${esc(e.note || '')}</p>
    </div>
    </div>

    <div class="card">
      <h3>По токенам</h3>
      <table>
        <thead><tr><th>Токен</th><th>Провайдер</th><th>Запросов</th><th>Токенов</th><th>Последний</th></tr></thead>
        <tbody>
          ${(u.by_token || []).map((t) => `<tr>
            <td>${esc(t.token_name)}</td>
            <td class="muted small">${esc(providerLabel(t.provider))}</td>
            <td>${fmtNum(t.requests)}</td>
            <td>${fmtNum(t.prompt_tokens + t.completion_tokens)}</td>
            <td class="small muted">${fmtTime(t.last_used_at)}</td>
          </tr>`).join('') || '<tr><td colspan="5" class="muted">Нет данных</td></tr>'}
        </tbody>
      </table>
    </div>`;
}

function setRange(r) {
  state.usageRange = r;
  go('key');
}

function chart(series) {
  if (!series || !series.length) return '<p class="muted small">Нет данных за период.</p>';
  const w = 800, h = 120, pad = 6;
  const maxTokens = Math.max(1, ...series.map((p) => p.tokens || 0));
  const step = series.length > 1 ? (w - pad * 2) / (series.length - 1) : 0;
  const points = series.map((p, i) => {
    const x = pad + i * step;
    const y = h - pad - ((p.tokens || 0) / maxTokens) * (h - pad * 2);
    return x.toFixed(1) + ',' + y.toFixed(1);
  }).join(' ');
  const area = pad + ',' + (h - pad) + ' ' + points + ' ' + (pad + (series.length - 1) * step).toFixed(1) + ',' + (h - pad);
  return `<svg class="chart" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">
    <defs>
      <linearGradient id="chartFill" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stop-color="#4c8dff" stop-opacity="0.28"></stop>
        <stop offset="100%" stop-color="#4c8dff" stop-opacity="0"></stop>
      </linearGradient>
    </defs>
    <line class="axis" x1="0" y1="${h - pad}" x2="${w}" y2="${h - pad}"></line>
    <polygon class="area" points="${area}"></polygon>
    <polyline points="${points}"></polyline>
  </svg>
  <div class="row small muted"><span>макс. ${fmtNum(maxTokens)} токенов/час</span><span class="spacer"></span>
  <span>${series.length} ${plural(series.length, 'точка', 'точки', 'точек')}</span></div>`;
}

/* ---------- токены ---------- */

async function renderTokens(main) {
  const [tokens, keys] = await Promise.all([api('/api/tokens'), api('/api/keys')]);
  state.tokens = tokens;
  state.keys = keys;
  const keyName = (id) => (keys.find((k) => k.id === id) || {}).name || '—';
  const keyHint = (id) => (keys.find((k) => k.id === id) || {}).key_hint || '';

  main.innerHTML = `
    <h1>Токены для IDE</h1>
    <div class="card">
      <h3>Выпустить токен ${infoBtn('Один ключ Напарника может иметь сколько угодно токенов — по одному на IDE или разработчика.', 'Про токены')}</h3>
      <div class="row">
        ${ddHTML('tkey', keys.map((k) => ({ value: String(k.id), label: k.name + ' · ' + k.key_hint })), keys[0] ? String(keys[0].id) : '', 'flex:1.4;min-width:220px')}
        <input id="tname" placeholder="Название (например, «Cline, ноутбук»)" style="flex:2;min-width:200px">
        ${ddHTML('tprovider', PROVIDERS, '', 'flex:1;min-width:210px')}
        ${ddHTML('tmode', [
          { value: '', label: 'поток: как в настройках' },
          { value: 'final', label: 'final — цельным куском' },
          { value: 'deltas', label: 'deltas — по мере готовности' },
        ], '', 'flex:1;min-width:200px')}
        <button class="btn" onclick="addToken()">Выпустить</button>
      </div>
    </div>
    <div class="card">
      <table>
        <thead><tr><th style="width:20px"></th><th>Название</th><th>Ключ</th><th>Провайдер</th><th>Поток</th><th>Последний</th><th></th></tr></thead>
        <tbody>
          ${tokens.map((t) => {
            const dot = t.revoked_at
              ? ['crit', 'отозван']
              : (t.enabled ? ['ok', 'активен'] : ['warn', 'выключен']);
            return `<tr>
            <td><span class="dot ${dot[0]}" title="${dot[1]}"></span></td>
            <td>${esc(t.name)}</td>
            <td class="small muted">${esc(keyName(t.key_id))}<br>
              <span class="mono" style="font-size:11.5px;color:var(--muted-2)">${esc(keyHint(t.key_id))}</span></td>
            <td class="small muted">${esc(providerLabel(t.provider))}</td>
            <td>${streamIcon(t.stream_mode)}</td>
            <td class="small muted">${fmtTime(t.last_used_at)}</td>
            <td class="actions">
              ${t.secret_available ? iconBtn('copy', 'Скопировать токен — можно передать сотруднику', `copyToken(${t.id})`) : ''}
              ${iconBtn('edit', 'Изменить токен: название, ключ, провайдер, поток', `openTokenEdit(${t.id})`)}
              ${iconBtn(t.enabled ? 'powerOff' : 'powerOn', t.enabled ? 'Выключить' : 'Включить', `toggleToken(${t.id}, ${!t.enabled})`)}
              ${iconBtn('revoke', 'Отозвать — IDE перестанет подключаться', `revokeToken(${t.id})`, 'danger')}
            </td>
          </tr>`;
          }).join('') || '<tr><td colspan="7" class="muted">Пока нет токенов</td></tr>'}
        </tbody>
      </table>
    </div>`;
}

async function addToken() {
  const keyID = Number(ddValue('tkey'));
  const name = document.getElementById('tname').value.trim();
  const provider = ddValue('tprovider');
  const stream_mode = ddValue('tmode');
  if (!keyID) return toast('Сначала добавьте ключ Напарника', true);
  try {
    const res = await api('/api/tokens', { method: 'POST', body: { key_id: keyID, name, provider, stream_mode } });
    showTokenModal(res);
  } catch (e) { toast(e.message, true); }
}

function showTokenModal(res) {
  const backdrop = document.createElement('div');
  backdrop.className = 'modal-backdrop';
  backdrop.innerHTML = `
    <div class="modal">
      <h2 style="margin-top:0">Токен создан</h2>
      <p class="muted small">Значение показывается один раз — сохраните его в настройках IDE.</p>
      <pre id="tokval">${esc(res.token)}</pre>
      <div class="row">
        <button class="btn" onclick="copy(document.getElementById('tokval').textContent)">Скопировать</button>
        <span class="spacer"></span>
        <button class="btn ghost" onclick="this.closest('.modal-backdrop').remove(); go('tokens')">Готово</button>
      </div>
    </div>`;
  document.body.appendChild(backdrop);
}

async function toggleToken(id, enabled) {
  try {
    await api('/api/tokens/' + id, { method: 'PATCH', body: { enabled } });
    go('tokens');
  } catch (e) { toast(e.message, true); }
}

async function revokeToken(id) {
  const ok = await confirmDialog({
    title: 'Отозвать токен?',
    text: 'IDE перестанет подключаться с этим токеном. Вернуть его нельзя — только выпустить новый или «Сгенерировать новый» в диалоге.',
    ok: 'Отозвать',
    danger: true,
  });
  if (!ok) return;
  try {
    await api('/api/tokens/' + id + '/revoke', { method: 'POST' });
    go('tokens');
  } catch (e) { toast(e.message, true); }
}

async function copyToken(id) {
  try {
    const res = await api('/api/tokens/' + id + '/reveal', { method: 'POST' });
    await copy(res.token);
  } catch (e) { toast(e.message, true); }
}

// ---------- редактирование токена ----------

function openTokenEdit(id) {
  const t = (state.tokens || []).find((x) => x.id === id);
  if (!t) return;
  const keys = state.keys || [];
  const status = t.revoked_at
    ? 'отозван ' + fmtTime(t.revoked_at)
    : (t.enabled ? 'включён' : 'выключен');
  const wrap = document.createElement('div');
  wrap.className = 'modal-backdrop';
  wrap.id = 'token-modal';
  wrap.innerHTML = `
    <div class="modal">
      <div class="row" style="align-items:center;margin-bottom:14px">
        <h2 style="margin:0">Изменить токен</h2>
        <span class="spacer"></span>
        <label class="switch" title="${t.revoked_at ? 'Токен отозван — включить нельзя, только перевыпустить' : 'Включить или выключить токен'}">
          <input type="checkbox" id="te_enabled" ${t.enabled && !t.revoked_at ? 'checked' : ''} ${t.revoked_at ? 'disabled' : ''}>
          <span class="track"><span class="knob"></span></span>
          <span class="switch-label">включён</span>
        </label>
      </div>
      <div class="grid">
        <label>Название<input id="te_name" value="${esc(t.name)}"></label>
        <label>Ключ Напарника${ddHTML('te_key',
          keys.map((k) => ({ value: String(k.id), label: k.name + ' · ' + k.key_hint })),
          String(t.key_id), '')}</label>
        <label>Провайдер${ddHTML('te_provider', PROVIDERS, t.provider || '', '')}</label>
        <label>Поток${ddHTML('te_stream', [
          { value: '', label: 'как в настройках' },
          { value: 'final', label: 'final — цельным куском' },
          { value: 'deltas', label: 'deltas — по мере готовности' },
        ], t.stream_mode || '', '')}</label>
      </div>
      <label style="display:block;margin-top:14px">Токен
        <div class="row" style="gap:6px;flex-wrap:nowrap">
          <input id="te_token" readonly value="" placeholder="${t.secret_available ? 'нажмите «Показать», чтобы получить значение' : 'значение не сохранено — выпущен до этой версии'}" style="flex:1;min-width:200px">
          ${iconBtn('eye', 'Показать значение', `revealTokenField(${t.id})`)}
          ${iconBtn('copy', 'Копировать значение', 'copyTokenField()')}
          ${iconBtn('rotate', 'Сгенерировать новый токен — старое значение перестанет работать', `rotateTokenField(${t.id})`)}
        </div>
      </label>
      <p class="muted small">Префикс <span class="mono" id="te_prefix">${esc(t.prefix)}…</span> · создан ${fmtTime(t.created_at)}
        · последний запрос ${fmtTime(t.last_used_at)} · ${esc(status)}</p>
      <div class="row" style="justify-content:flex-end;margin-top:18px">
        <button class="btn ghost" onclick="closeTokenEdit()">Отмена</button>
        <button class="btn" onclick="saveTokenEdit(${t.id})">Сохранить</button>
      </div>
    </div>`;
  wrap.addEventListener('click', (ev) => { if (ev.target === wrap) closeTokenEdit(); });
  document.body.appendChild(wrap);
  document.getElementById('te_name').focus();
}

async function revealTokenField(id) {
  try {
    const res = await api('/api/tokens/' + id + '/reveal', { method: 'POST' });
    const input = document.getElementById('te_token');
    if (input) input.value = res.token;
  } catch (e) { toast(e.message, true); }
}

function copyTokenField() {
  const input = document.getElementById('te_token');
  if (!input || !input.value) return toast('Сначала нажмите «Показать»', true);
  copy(input.value);
}

async function rotateTokenField(id) {
  const ok = await confirmDialog({
    title: 'Сгенерировать новый токен?',
    text: 'Старое значение сразу перестанет работать, токен снова станет активным. Ключ, провайдер и поток сохранятся.',
    ok: 'Сгенерировать',
  });
  if (!ok) return;
  try {
    const res = await api('/api/tokens/' + id + '/rotate', { method: 'POST' });
    const input = document.getElementById('te_token');
    if (input) input.value = res.token;
    const pref = document.getElementById('te_prefix');
    if (pref) pref.textContent = res.prefix + '…';
    toast('Новый токен сгенерирован — скопируйте его');
  } catch (e) { toast(e.message, true); }
}

function closeTokenEdit() {
  const el = document.getElementById('token-modal');
  if (el) el.remove();
}

async function saveTokenEdit(id) {
  const name = document.getElementById('te_name').value.trim();
  if (!name) return toast('Название не может быть пустым', true);
  const body = {
    key_id: Number(ddValue('te_key')),
    name,
    provider: ddValue('te_provider'),
    stream_mode: ddValue('te_stream'),
  };
  // Отозванный токен переключателем не оживить — флаг не отправляем.
  const chk = document.getElementById('te_enabled');
  if (chk && !chk.disabled) body.enabled = chk.checked;
  try {
    await api('/api/tokens/' + id, { method: 'PATCH', body });
    closeTokenEdit();
    toast('Сохранено');
    go('tokens');
  } catch (e) { toast(e.message, true); }
}

/* ---------- сессии ---------- */

async function renderSessions(main) {
  const [sessions, tokens] = await Promise.all([api('/api/sessions'), api('/api/tokens')]);
  const tokenName = (id) => (tokens.find((t) => t.id === id) || {}).name || ('#' + id);

  main.innerHTML = `
    <h1>Сессии ${infoBtn('Привязка клиентской сессии к диалогу Напарника. Сброс создаёт новый диалог при следующем запросе.', 'Про сессии')}</h1>
    <div class="card">
      <table>
        <thead><tr><th>Токен</th><th>Протокол</th><th>Сессия</th><th>Диалог</th><th>Обновлена</th><th></th></tr></thead>
        <tbody>
          ${sessions.map((s) => `<tr>
            <td>${esc(tokenName(s.token_id))}</td>
            <td class="small muted">${esc(s.protocol)}</td>
            <td class="mono small">${esc(String(s.session_id).slice(0, 24))}</td>
            <td class="mono small muted">${esc(String(s.conversation).slice(0, 8))}…</td>
            <td class="small muted">${fmtTime(s.updated_at)}</td>
            <td style="text-align:right">
              <button class="btn ghost small" type="button" data-reset-session data-token-id="${esc(s.token_id)}" data-protocol="${esc(s.protocol)}" data-session-id="${esc(s.session_id)}">Сбросить</button>
            </td>
          </tr>`).join('') || '<tr><td colspan="6" class="muted">Активных сессий нет</td></tr>'}
        </tbody>
      </table>
    </div>`;
  main.querySelectorAll('[data-reset-session]').forEach((button) => {
    button.addEventListener('click', () => {
      const { tokenId, protocol, sessionId } = button.dataset;
      resetSession(tokenId, protocol, sessionId);
    });
  });
}

async function resetSession(tokenID, protocol, sessionID) {
  try {
    await api('/api/sessions?token_id=' + tokenID + '&protocol=' + encodeURIComponent(protocol) +
      '&session_id=' + encodeURIComponent(sessionID), { method: 'DELETE' });
    toast('Сессия сброшена');
    go('sessions');
  } catch (e) { toast(e.message, true); }
}

/* ---------- журнал ---------- */

const LOG_PAGE_SIZE = 50;

// logQuery собирает параметры запроса журнала: активный отбор плюс то, что
// передали явно. Один построитель на список и выгрузку — иначе фильтры в них
// разъезжаются и CSV приходит не за тот период, что видно на экране.
function logQuery(extra) {
  const p = new URLSearchParams();
  const f = state.logFilter || {};
  if (f.tokenId) p.set('token_id', f.tokenId);
  if (f.from) p.set('from', f.from);
  if (f.to) p.set('to', f.to);
  if (extra) Object.entries(extra).forEach(([k, v]) => p.set(k, String(v)));
  return p.toString();
}

// applyLogFilter переносит выбранный отбор в состояние и перечитывает журнал
// с первой страницы: после сужения выборки текущая страница «уезжает» за конец.
function applyLogFilter() {
  const fromEl = document.getElementById('log-from');
  const toEl = document.getElementById('log-to');
  const from = fromEl ? fromEl.value : '';
  const to = toEl ? toEl.value : '';
  const tokenId = ddValue('log-token') || '';
  if (from && to && new Date(to) < new Date(from)) {
    toast('Дата «по» раньше даты «с»', true);
    return;
  }
  state.logFilter = { tokenId, from, to };
  state.logOffset = 0;
  go('logs');
}

function resetLogFilter() {
  state.logFilter = { tokenId: '', from: '', to: '' };
  state.logOffset = 0;
  go('logs');
}

// logFilterActive сообщает, задано ли хоть одно условие отбора — по нему
// показываем пояснение под панелью фильтров.
function logFilterActive() {
  const f = state.logFilter || {};
  return !!(f.tokenId || f.from || f.to);
}

async function renderLogs(main) {
  const [keys, tokens] = await Promise.all([api('/api/keys'), api('/api/tokens')]);
  const size = LOG_PAGE_SIZE;
  if (state.logOffset < 0) state.logOffset = 0;
  const res = await api('/api/logs?' + logQuery({ limit: size, offset: state.logOffset }));
  state.logTotal = res.total;
  // После очистки текущая страница могла оказаться за пределами журнала.
  if (res.total > 0 && state.logOffset >= res.total) {
    state.logOffset = Math.floor((res.total - 1) / size) * size;
    return renderLogs(main);
  }
  if (res.total === 0) state.logOffset = 0;

  // Записи и справочники держим в состоянии: карточка записи открывается по клику без запроса к серверу.
  state.logEntries = res.entries;
  state.logTokens = tokens;
  state.logKeys = keys;

  const tokenName = (id) => (tokens.find((t) => t.id === id) || {}).name || ('#' + id);
  const cut = (s, n) => {
    const t = String(s || '');
    return t.length > n ? t.slice(0, n - 1) + '…' : t;
  };
  const from = res.total ? state.logOffset + 1 : 0;
  const to = state.logOffset + res.entries.length;
  const prevOff = iconBtn('prev', 'Предыдущая страница', 'logsPage(-1)', '', state.logOffset <= 0);
  const nextOff = iconBtn('next', 'Следующая страница', 'logsPage(1)', '', to >= res.total);

  main.innerHTML = `
    <h1>Журнал запросов ${infoBtn('Что именно спрашивали пользователи: последняя реплика каждого запроса, до 2000 символов.', 'Про журнал')}</h1>

    <section class="card settings-section card-compact">
      <div class="card-head">
        <h2>Хранение и выгрузка ${infoBtn('Записи старше указанного числа дней удаляются безвозвратно; 0 — удалить весь журнал. Выгрузка — CSV, открывается в Excel.', 'Про хранение и выгрузку')}</h2>
        <span class="spacer"></span>
        <label class="inline-label">Удалить записи старше
          <input id="log-days" type="number" min="0" max="3650" step="1" value="30" style="width:88px;margin-top:0">
          дней
        </label>
        ${iconBtn('download', 'Выгрузить журнал в CSV', 'exportLogs()')}
        ${iconBtn('trash', 'Удалить записи', 'purgeLogs()', 'danger')}
      </div>
    </section>

    <section class="card settings-section">
      <div class="card-head">
        <h2>Отбор ${infoBtn('Фильтры действуют и на список, и на выгрузку в CSV. Период «по» включает указанную минуту целиком.', 'Про отбор')}</h2>
        <span class="spacer"></span>
        <button class="btn ghost small" type="button" onclick="resetLogFilter()">Сбросить</button>
        <button class="btn small" type="button" onclick="applyLogFilter()">Применить</button>
      </div>
      <div class="row log-filter">
        <label class="inline-label">Токен
          ${ddHTML('log-token', [{ value: '', label: 'Все токены' }].concat(
            tokens.map((t) => ({ value: String(t.id), label: t.name || ('#' + t.id) }))
          ), state.logFilter.tokenId, 'min-width:220px')}
        </label>
        <label class="inline-label">с
          <input id="log-from" type="datetime-local" value="${esc(state.logFilter.from)}" style="margin-top:0">
        </label>
        <label class="inline-label">по
          <input id="log-to" type="datetime-local" value="${esc(state.logFilter.to)}" style="margin-top:0">
        </label>
      </div>
      ${logFilterActive() ? '<p class="muted small" style="margin:10px 0 0">Показаны записи по отбору. «Всего записей» ниже — с учётом отбора.</p>' : ''}
    </section>

    <section class="card settings-section">
      <div class="card-head">
        <h2>Записи ${infoBtn('Страница показывает 50 записей. Нажмите на строку, чтобы посмотреть подробности. Выгрузка в CSV отдаёт весь журнал с учётом отбора.', 'Про список записей')}</h2>
        <span class="muted small">Всего записей: ${fmtNum(res.total)}</span>
        <span class="spacer"></span>
        ${res.total ? `<span class="muted small">${fmtNum(from)}–${fmtNum(to)} из ${fmtNum(res.total)}</span>` : ''}
        ${iconBtn('refresh', 'Обновить список', "go('logs')")}
        ${prevOff}
        ${nextOff}
      </div>
      <table>
        <thead><tr><th>Время</th><th>Токен</th><th>Запрос</th><th>Протокол</th><th>Статус</th><th>Токены</th><th>мс</th><th></th></tr></thead>
        <tbody>
          ${res.entries.map((e) => `<tr class="row-click" tabindex="0"
              onclick="openLogEntry(${e.id})"
              onkeydown="if(event.key==='Enter'||event.key===' '){event.preventDefault();openLogEntry(${e.id})}">
            <td class="small">${fmtTime(e.ts)}</td>
            <td class="small">${esc(tokenName(e.token_id))}</td>
            <td class="small log-prompt" title="${esc(e.prompt || '')}">${e.prompt
              ? esc(cut(e.prompt, 90))
              : '<span class="muted">—</span>'}</td>
            <td class="small muted">${esc(e.protocol)}${e.stream ? ' · SSE' : ''}</td>
            <td class="small">${e.status >= 400
              ? '<span class="badge crit">' + e.status + ' ' + esc(e.error_type || '') + '</span>'
              : '<span class="badge ok">' + e.status + '</span>'}</td>
            <td class="small">${fmtNum(e.prompt_tokens + e.completion_tokens)}</td>
            <td class="small muted">${e.latency_ms}</td>
            <td class="small muted">${iconBtn('eye', 'Показать подробности записи', `openLogEntry(${e.id})`)}</td>
          </tr>`).join('') || '<tr><td colspan="8" class="muted">Записей нет</td></tr>'}
        </tbody>
      </table>
    </section>`;
}

// logsPage переключает страницу журнала: -1 — назад, 1 — вперёд.
function logsPage(delta) {
  const size = LOG_PAGE_SIZE;
  let off = state.logOffset + delta * size;
  if (off < 0) off = 0;
  if (state.logTotal > 0 && off >= state.logTotal) {
    off = Math.floor((state.logTotal - 1) / size) * size;
  }
  state.logOffset = off;
  go('logs');
}

// exportLogs скачивает журнал файлом CSV: сервер отдаёт его вложением.
// Отбор передаётся тот же, что действует на экране.
function exportLogs() {
  const q = logQuery();
  window.location.href = '/api/logs/export' + (q ? '?' + q : '');
}

// purgeLogs удаляет записи журнала старше введённого числа дней (0 — все).
async function purgeLogs() {
  const field = document.getElementById('log-days');
  const raw = field.value.trim();
  const days = Number(raw);
  if (raw === '' || !Number.isFinite(days) || days < 0 || !Number.isInteger(days)) {
    toast('Укажите целое число дней (0 — удалить все записи)', true);
    field.focus();
    return;
  }
  const ok = await confirmDialog({
    title: days === 0 ? 'Удалить весь журнал?' : 'Удалить старые записи?',
    text: days === 0
      ? 'Будут удалены все записи журнала. Отменить это действие нельзя.'
      : `Будут удалены записи старше ${days} дн. Отменить это действие нельзя.`,
    ok: 'Удалить',
    danger: true,
  });
  if (!ok) return;
  try {
    const r = await api('/api/logs/purge', { method: 'POST', body: { days } });
    toast('Удалено записей: ' + fmtNum(r.deleted));
    state.logOffset = 0;
    go('logs');
  } catch (e) { toast(e.message, true); }
}

/* ---------- карточка записи журнала ---------- */

// fmtDateTime — полная дата и время: toLocaleString('ru-RU') ставит запятую между датой и временем.
function fmtDateTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d)) return '—';
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getDate())}.${p(d.getMonth() + 1)}.${d.getFullYear()} `
    + `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

// openLogEntry показывает подробности записи: полный вопрос и все метрики запроса.
// Данные берём из уже загруженной страницы журнала, сервер не дёргаем.
function openLogEntry(id) {
  // Строка и кнопка «глаз» ведут в одно окно: второй вызов из всплытия клика игнорируем.
  if (document.getElementById('log-modal')) return;
  const e = (state.logEntries || []).find((x) => x.id === id);
  if (!e) return;
  const token = (state.logTokens || []).find((t) => t.id === e.token_id) || {};
  const key = (state.logKeys || []).find((k) => k.id === e.key_id) || {};
  const fact = (name, val) => `<div><span class="log-fact-k">${name}</span><span class="log-fact-v">${val}</span></div>`;
  const tokenLabel = esc(token.name || '#' + e.token_id)
    + (token.prefix ? ` <span class="muted">${esc(token.prefix)}</span>` : '');
  const proto = esc(e.protocol || '—') + (e.stream ? ' · SSE' : '');
  const truncated = (e.prompt || '').length >= 2000;

  const wrap = document.createElement('div');
  wrap.className = 'modal-backdrop';
  wrap.id = 'log-modal';
  wrap.innerHTML = `
    <div class="modal log-detail">
      <div class="row" style="align-items:center;margin-bottom:14px">
        <h2 style="margin:0">Запрос #${e.id}</h2>
        <span class="spacer"></span>
        <span class="badge ${e.status >= 400 ? 'crit' : 'ok'}">${e.status}${e.error_type ? ' ' + esc(e.error_type) : ''}</span>
      </div>
      <div class="log-facts">
        ${fact('Время', esc(fmtDateTime(e.ts)))}
        ${fact('Токен', tokenLabel)}
        ${fact('Ключ', esc(key.name || '#' + e.key_id))}
        ${fact('Протокол', proto)}
        ${fact('Модель', esc(e.model || '—'))}
        ${fact('Токены запроса', fmtNum(e.prompt_tokens))}
        ${fact('Токены ответа', fmtNum(e.completion_tokens))}
        ${fact('Из кэша', fmtNum(e.cached_tokens))}
        ${fact('Задержка', fmtNum(e.latency_ms) + ' мс')}
        ${e.error_type ? fact('Ошибка', `<span style="color:var(--crit)">${esc(e.error_type)}</span>`) : ''}
      </div>
      <div class="row" style="align-items:center;margin:0 0 6px">
        <h3 style="margin:0">Запрос</h3>
        <span class="spacer"></span>
        ${e.prompt ? iconBtn('copy', 'Копировать текст запроса', `copyLogPrompt(${e.id})`) : ''}
      </div>
      ${e.prompt
        ? `<pre class="log-full">${esc(e.prompt)}</pre>`
        : '<p class="muted small" style="margin:0">Текст запроса не сохранён — запрос отклонён до разбора тела.</p>'}
      <p class="muted small" style="margin:8px 0 0">Последняя реплика пользователя${truncated
        ? ', обрезана до 2000 символов' : ''}. Ответ модели в журнале не хранится.</p>
      <div class="row" style="justify-content:flex-end;margin-top:18px">
        <button class="btn" onclick="closeLogEntry()">Закрыть</button>
      </div>
    </div>`;
  const onKey = (ev) => { if (ev.key === 'Escape') { ev.preventDefault(); closeLogEntry(); } };
  wrap._onKey = onKey;
  wrap.addEventListener('click', (ev) => { if (ev.target === wrap) closeLogEntry(); });
  document.addEventListener('keydown', onKey);
  document.body.appendChild(wrap);
}

function closeLogEntry() {
  const el = document.getElementById('log-modal');
  if (!el) return;
  if (el._onKey) document.removeEventListener('keydown', el._onKey);
  el.remove();
}

function copyLogPrompt(id) {
  const e = (state.logEntries || []).find((x) => x.id === id);
  if (e && e.prompt) copy(e.prompt);
}

/* ---------- слушатели ---------- */

async function renderListeners(main) {
  state.listeners = await api('/api/listeners');
  paintListeners(main);
}

// duplicateAddrs возвращает адреса, которые заняты несколькими включёнными слушателями.
// Выключенные записи порт не занимают, поэтому с ними дубль допустим.
function duplicateAddrs(list) {
  const seen = new Set();
  const dup = new Set();
  list.forEach((l) => {
    if (l.disabled) return;
    const addr = String(l.addr || '').trim();
    if (!addr) return;
    if (seen.has(addr)) dup.add(addr);
    else seen.add(addr);
  });
  return dup;
}

function paintListeners(main) {
  const dup = duplicateAddrs(state.listeners);
  main.innerHTML = `
    <h1>Слушатели ${infoBtn('Можно слушать несколько портов одновременно. Для доступа из локальной сети укажите 0.0.0.0:порт. Админку держите на 127.0.0.1, если не нужен удалённый доступ.', 'Про слушателей')}</h1>
    <div class="card">
      <table>
        <thead><tr><th></th><th>Адрес</th><th>Тип</th><th></th></tr></thead>
        <tbody id="listeners-body">
          ${state.listeners.map((l, i) => listenerRow(l, i, dup)).join('') || '<tr><td colspan="4" class="muted">Слушателей нет — добавьте хотя бы один.</td></tr>'}
        </tbody>
      </table>
      ${dup.size ? `<p class="field-error">Адрес ${esc([...dup][0])} занят двумя включёнными слушателями — выключите один или измените порт.</p>` : ''}
      <div class="row" style="margin-top:12px">
        <button class="btn ghost" onclick="addListenerRow()">Добавить</button>
        <span class="spacer"></span>
        <button class="btn" onclick="saveListeners()">Применить</button>
      </div>
    </div>`;
}

function listenerRow(l, i, dup) {
  const kinds = [
    { value: 'gateway', label: 'gateway — шлюз для IDE' },
    { value: 'admin', label: 'admin — админка' },
  ];
  const addr = String(l.addr || '').trim();
  const clash = dup && dup.has(addr);
  const tls = l.cert_file && l.key_file
    ? ' <span class="badge ok" title="Слушатель работает по HTTPS">https</span>'
    : '';
  const tlsTitle = tls ? 'Изменить сертификат' : 'Включить HTTPS (сертификат)';
  return `<tr>
    <td>
      <label class="switch" title="${l.disabled ? 'Включить слушатель' : 'Выключить слушатель'}">
        <input type="checkbox" ${l.disabled ? '' : 'checked'} onchange="setListenerEnabled(${i}, this.checked)">
        <span class="track"><span class="knob"></span></span>
      </label>
    </td>
    <td><input value="${esc(l.addr)}" data-addr="${i}" class="${clash ? 'invalid' : ''}" style="width:100%"${clash ? ' title="Этот адрес уже занят другим включённым слушателем"' : ''}></td>
    <td>${ddHTML('lkind-' + i, kinds, l.kind, '', 'data-kind="' + i + '"')}${tls}</td>
    <td class="actions">${iconBtn('lock', tlsTitle, 'listenerTLS(' + i + ')', tls ? 'ok' : '')}${iconBtn('trash', 'Удалить слушатель', 'removeListenerRow(' + i + ')', 'danger')}</td>
  </tr>`;
}

// listenerTLS открывает диалог с путями к сертификату и ключу. Пустая пара
// означает HTTP; заполненная — HTTPS. Один путь без другого сервер не примет.
function listenerTLS(i) {
  syncListenerInputs();
  const l = state.listeners[i];
  if (!l) return;
  const wrap = document.createElement('div');
  wrap.className = 'modal-backdrop';
  wrap.id = 'listener-tls-modal';
  wrap.innerHTML = `
    <div class="modal" style="max-width:560px">
      <h2 style="margin-top:0">Сертификат для ${esc(l.addr || 'слушателя')}</h2>
      <p class="muted" style="margin:0 0 14px">Пути к файлам на машине, где запущен сервис. Оба поля пустые — слушатель работает по HTTP. Заполнены оба — по HTTPS (TLS 1.2+).</p>
      <label>Сертификат (PEM, цепочка)</label>
      <input id="ltls-cert" value="${esc(l.cert_file || '')}" placeholder="C:\\certs\\server.crt" style="width:100%">
      <label>Ключ (PEM, без пароля)</label>
      <input id="ltls-key" value="${esc(l.key_file || '')}" placeholder="C:\\certs\\server.key" style="width:100%">
      <p class="field-error" id="ltls-err" hidden></p>
      <div class="row" style="justify-content:flex-end;margin-top:16px">
        <button class="btn ghost" data-act="cancel">Отмена</button>
        <button class="btn ghost" data-act="clear">Снять HTTPS</button>
        <button class="btn" data-act="ok">Сохранить</button>
      </div>
    </div>`;
  const close = () => wrap.remove();
  wrap.addEventListener('click', (e) => {
    if (e.target === wrap) return close();
    const act = e.target.closest('[data-act]');
    if (!act) return;
    if (act.dataset.act === 'cancel') return close();
    if (act.dataset.act === 'clear') {
      delete l.cert_file;
      delete l.key_file;
      close();
      paintListeners(document.getElementById('main'));
      return toast('HTTPS снят — нажмите «Применить», чтобы сохранить');
    }
    const cert = wrap.querySelector('#ltls-cert').value.trim();
    const key = wrap.querySelector('#ltls-key').value.trim();
    const err = wrap.querySelector('#ltls-err');
    if (!!cert !== !!key) {
      err.textContent = 'Нужны оба файла — и сертификат, и ключ';
      err.hidden = false;
      return;
    }
    if (cert) { l.cert_file = cert; l.key_file = key; }
    else { delete l.cert_file; delete l.key_file; }
    close();
    paintListeners(document.getElementById('main'));
    toast(cert ? 'HTTPS настроен — нажмите «Применить»' : 'HTTPS снят — нажмите «Применить»');
  });
  document.body.appendChild(wrap);
  const first = wrap.querySelector('#ltls-cert');
  if (first) first.focus();
}

// syncListenerInputs переносит правки из полей ввода в state,
// чтобы они не терялись при перерисовке таблицы.
function syncListenerInputs() {
  document.querySelectorAll('[data-addr]').forEach((el, i) => {
    if (state.listeners[i]) state.listeners[i].addr = el.value;
  });
  document.querySelectorAll('.dd[data-kind]').forEach((el, i) => {
    if (state.listeners[i]) state.listeners[i].kind = ddValue(el.id);
  });
}

function addListenerRow() {
  syncListenerInputs();
  // Новый слушатель добавляется выключенным: включаешь осознанно, когда адрес и тип проверены.
  state.listeners.push({ addr: '127.0.0.1:47833', kind: 'gateway', disabled: true });
  paintListeners(document.getElementById('main'));
}

// listenerPayload собирает тело запроса из state, сохраняя выключенные слушатели
// и пути к сертификату: иначе «Применить» стирал бы cert_file/key_file из конфига.
function listenerPayload() {
  return state.listeners
    .map((l) => {
      const out = { addr: String(l.addr || '').trim(), kind: l.kind, disabled: !!l.disabled };
      if (l.cert_file) out.cert_file = l.cert_file;
      if (l.key_file) out.key_file = l.key_file;
      return out;
    })
    .filter((l) => l.addr !== '');
}

// setListenerEnabled включает и выключает слушатель сразу, без кнопки «Применить».
// Если адрес уже занят другим включённым слушателем, строки остаются на месте,
// а поля с адресом подсвечиваются как ошибка.
async function setListenerEnabled(i, on) {
  syncListenerInputs();
  const l = state.listeners[i];
  if (!l) return;
  l.disabled = !on;
  const dup = duplicateAddrs(state.listeners);
  if (dup.size) {
    paintListeners(document.getElementById('main'));
    toast('Адрес ' + [...dup][0] + ' уже занят другим включённым слушателем', true);
    return;
  }
  try {
    await api('/api/listeners', { method: 'PUT', body: listenerPayload() });
    toast(on ? 'Слушатель включён' : 'Слушатель выключен');
    go('listeners');
  } catch (e) {
    toast(e.message, true);
    // Не перечитываем список с сервера: иначе несохранённые правки (например,
    // только что добавленная строка) пропадут из таблицы.
    paintListeners(document.getElementById('main'));
  }
}

function removeListenerRow(i) {
  syncListenerInputs();
  state.listeners.splice(i, 1);
  paintListeners(document.getElementById('main'));
}

async function saveListeners() {
  syncListenerInputs();
  const list = listenerPayload();
  if (!list.length) return toast('Нужен хотя бы один слушатель', true);
  const dup = duplicateAddrs(list);
  if (dup.size) {
    paintListeners(document.getElementById('main'));
    return toast('Адрес ' + [...dup][0] + ' уже занят другим включённым слушателем', true);
  }
  try {
    await api('/api/listeners', { method: 'PUT', body: list });
    toast('Слушатели применены');
    go('listeners');
  } catch (e) { toast(e.message, true); }
}

/* ---------- настройки ---------- */

async function renderSettings(main) {
  const s = await api('/api/settings');
  state.settings = s;
  main.innerHTML = `
    <h1>Настройки</h1>

    <section class="card settings-section">
      <h2>Подключение ${infoBtn('Имя модели, которое видят IDE в сниппетах и в <code>/v1/models</code>. В API Напарника не уходит — можно задать любое: латиница, цифры, <code>- _ . : /</code>.', 'Про ID модели')}</h2>
      <div class="grid cols2">
        <label class="narrow">ID модели для подключения
          <input id="s_model" value="${esc(s.model_id || 'naparnik')}" placeholder="naparnik">
        </label>
      </div>
    </section>

    <section class="card settings-section">
      <h2>Ответы и сессии ${infoBtn('Как шлюз отдаёт текст клиенту и держит диалог с Напарником. Длительности вводятся числом с единицей: 30s, 5m, 2h. Таймаут запроса применяется после перезапуска сервиса.', 'Про ответы и сессии')}</h2>
      <div class="grid cols2">
        <label>Режим потока по умолчанию
          ${ddHTML('s_stream', [
            { value: 'final', label: 'final — отдавать финальный текст' },
            { value: 'deltas', label: 'deltas — по мере готовности' },
          ], s.stream_mode)}
        </label>
        <label>Порог авто-компактизации
          <input id="s_compact" type="number" step="0.05" min="0.1" max="1" value="${s.auto_compact_at}">
        </label>
        <label>TTL сессии
          <input id="s_ttl" value="${esc(s.session_ttl)}" placeholder="например, 2h"
            title="Число с единицей: 30s, 5m, 2h" oninput="this.classList.remove('invalid')">
        </label>
        <label>Таймаут запроса
          <input id="s_timeout" value="${esc(s.request_timeout)}" placeholder="например, 10m"
            title="Число с единицей: 30s, 5m, 2h" oninput="this.classList.remove('invalid')">
        </label>
      </div>
    </section>

    <section class="card settings-section">
      <h2>Мониторинг и алерты ${infoBtn('Опрос бюджетов ключей и уведомления о расходе лимита. Период вводится числом с единицей: 30s, 5m, 1h (минимум 30s). Применяется после перезапуска сервиса.', 'Про мониторинг')}</h2>
      <div class="grid cols2">
        <label>Период опроса бюджета
          <input id="s_poll" value="${esc(s.budget_poll_interval)}" placeholder="например, 5m"
            title="Число с единицей: 30s, 5m, 1h" oninput="this.classList.remove('invalid')">
        </label>
        <label>Порог предупреждения, %
          <input id="s_warn" type="number" min="1" max="99" value="${s.warn_percent}">
        </label>
        <label>Порог критический, %
          <input id="s_crit" type="number" min="1" max="100" value="${s.crit_percent}">
        </label>
        <label>Webhook алертов
          <input id="s_hook" value="${esc(s.alert_webhook || '')}" placeholder="https://...">
        </label>
      </div>
    </section>

    <div class="row"><button class="btn" onclick="saveSettings()">Сохранить</button></div>`;
}

async function saveSettings() {
  // Длительности вводятся числом с единицей: 30s, 5m, 2h.
  const durationRe = /^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$/;
  for (const [id, name] of [
    ['s_ttl', 'TTL сессии'],
    ['s_timeout', 'Таймаут запроса'],
    ['s_poll', 'Период опроса бюджета'],
  ]) {
    const el = document.getElementById(id);
    const valid = durationRe.test(el.value.trim());
    el.classList.toggle('invalid', !valid);
    if (!valid) {
      el.focus();
      return toast(name + ': укажите число с единицей, например 5m', true);
    }
  }

  const body = {
    model_id: document.getElementById('s_model').value.trim(),
    stream_mode: ddValue('s_stream'),
    auto_compact_at: Number(document.getElementById('s_compact').value),
    session_ttl: document.getElementById('s_ttl').value.trim(),
    request_timeout: document.getElementById('s_timeout').value.trim(),
    budget_poll_interval: document.getElementById('s_poll').value.trim(),
    warn_percent: Number(document.getElementById('s_warn').value),
    crit_percent: Number(document.getElementById('s_crit').value),
    alert_webhook: document.getElementById('s_hook').value.trim(),
    upstream_base: state.settings.upstream_base,
  };
  try {
    await api('/api/settings', { method: 'PUT', body });
    toast('Сохранено');
  } catch (e) { toast(e.message, true); }
}

/* ---------- подключение ---------- */

// Клиенты для страницы «Подключение». Флаг verified ставим вручную после проверки
// в самом клиенте: true — «Проверено», false — «Не проверено».
const CONNECT_CLIENTS = [
  {
    id: 'cline',
    name: 'Cline',
    note: 'Провайдер «OpenAI Compatible»',
    verified: true,
    text: (c) => `Provider: OpenAI Compatible
Base URL: ${c.openai_url}
API Key: ${c.token}
Model: ${c.model}`,
  },
  {
    id: 'roo',
    name: 'Roo Code / Kilo Code',
    note: 'Провайдер «OpenAI Compatible»',
    verified: false,
    text: (c) => `Provider: OpenAI Compatible
Base URL: ${c.openai_url}
API Key: ${c.token}
Model: ${c.model}`,
  },
  {
    id: 'continue',
    name: 'Continue (VS Code, JetBrains)',
    note: '~/.continue/config.yaml',
    verified: false,
    text: (c) => `models:
  - name: Напарник
    provider: openai
    model: ${c.model}
    apiBase: ${c.openai_url}
    apiKey: ${c.token}`,
  },
  {
    id: 'zed',
    name: 'Zed',
    note: 'settings.json',
    verified: true,
    text: (c) => `"language_models": {
  "openai": {
    "api_url": "${c.openai_url}",
    "available_models": [
      { "name": "${c.model}", "max_tokens": 32000 }
    ]
  }
}`,
  },
  {
    id: 'jetbrains',
    name: 'JetBrains AI Assistant',
    note: 'свой OpenAI-совместимый провайдер',
    verified: false,
    text: (c) => `Base URL: ${c.openai_url}
API Key: ${c.token}
Model: ${c.model}`,
  },
  {
    id: 'qoder',
    name: 'Qoder',
    note: 'свой OpenAI-совместимый провайдер',
    verified: true,
    text: (c) => `Base URL: ${c.openai_url}
API Key: ${c.token}
Model: ${c.model}`,
  },
  {
    id: 'claude',
    name: 'Claude Code',
    note: 'переменные окружения',
    verified: false,
    proto: 'anthropic',
    text: (c) => `export ANTHROPIC_BASE_URL=${c.anthropic_url}
export ANTHROPIC_API_KEY=${c.token}
export ANTHROPIC_MODEL=${c.model}
claude`,
  },
  {
    // DSH (DeepSeek Harness) работает через Anthropic Messages, как Claude Code,
    // поэтому переменные те же. Живым подключением пока не проверялся.
    id: 'dsh',
    name: 'DSH (DeepSeek Harness)',
    note: 'переменные окружения, Anthropic Messages',
    verified: false,
    proto: 'anthropic',
    text: (c) => `export ANTHROPIC_BASE_URL=${c.anthropic_url}
export ANTHROPIC_API_KEY=${c.token}
export ANTHROPIC_MODEL=${c.model}`,
  },
  {
    id: 'codex',
    proto: 'responses',
    name: 'Codex CLI',
    note: '~/.codex/config.toml',
    verified: true,
    // Codex CLI для своих провайдеров ходит в Responses API: wire_api = "responses".
    // Токен нужен без ограничения протокола или с OpenAI Responses — токен,
    // ограниченный Chat Completions, вернёт 403 protocol_not_allowed.
    text: (c) => `# ~/.codex/config.toml
model = "${c.model}"
model_provider = "naparnik"

[model_providers.naparnik]
name = "Naparnik"
base_url = "${c.openai_url}"
env_key = "NAPARNIK_API_KEY"
wire_api = "responses"

# PowerShell: токен живёт только в этом окне
$token = Read-Host 'Токен Напарника' -AsSecureString
$env:NAPARNIK_API_KEY = [System.Net.NetworkCredential]::new('', $token).Password
codex -m ${c.model} -c 'model_provider="naparnik"'`,
  },
  {
    id: 'gemini',
    name: 'Gemini CLI',
    note: 'переменные окружения',
    verified: false,
    proto: 'gemini',
    text: (c) => `export GOOGLE_GEMINI_BASE_URL=${c.gemini_url}
export GEMINI_API_KEY=${c.token}
gemini --model ${c.model}`,
  },
  {
    id: 'opencode',
    name: 'OpenCode',
    note: 'opencode.jsonc',
    verified: true,
    text: (c) => `{
  "provider": {
    "naparnik": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "${c.openai_url}",
        "apiKey": "${c.token}"
      },
      "models": { "${c.model}": { "name": "Напарник" } }
    }
  }
}`,
  },
  {
    id: 'zcode',
    name: 'ZCode',
    note: 'провайдер «OpenAI Compatible»',
    verified: true,
    text: (c) => `Provider: OpenAI Compatible
Base URL: ${c.openai_url}
API Key: ${c.token}
Model: ${c.model}`,
  },
  {
    id: 'curl',
    name: 'Проверка через curl',
    note: 'быстрый тест из консоли',
    verified: true,
    text: (c) => `curl ${c.openai_url}/chat/completions \\
  -H "Authorization: Bearer ${c.token}" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"${c.model}","messages":[{"role":"user","content":"Привет"}]}'`,
  },
];

// Клиенты без явного протокола работают через OpenAI-совместимый эндпоинт.
CONNECT_CLIENTS.forEach((cl) => { if (!cl.proto) cl.proto = 'openai'; });

const CONNECT_PROTO_NAMES = { openai: 'OpenAI Chat Completions', responses: 'OpenAI Responses', anthropic: 'Anthropic', gemini: 'Gemini' };

function connectBadge(ok) {
  return ok
    ? '<span class="badge ok">Проверено</span>'
    : '<span class="badge">Не проверено</span>';
}

// connectTokens — токены, которые можно выбрать: включённые и не отозванные.
function connectTokens() {
  const all = (state.connectData && state.connectData.tokens) || [];
  return all.filter((t) => t.enabled && !t.revoked_at);
}

// splitAddr разбирает адрес слушателя на хост и порт, включая вид [::1]:47831.
function splitAddr(addr) {
  const v6 = /^\[(.*)\]:(\d+)$/.exec(addr);
  if (v6) return { host: v6[1], port: v6[2] };
  const i = addr.lastIndexOf(':');
  if (i < 0) return null;
  return { host: addr.slice(0, i), port: addr.slice(i + 1) };
}

// connectAddrs — адреса включённых gateway-слушателей. 0.0.0.0 и :: заменяем на
// 127.0.0.1: клиент обычно на этой же машине, а из сети подойдёт адрес сервера.
function connectAddrs() {
  const list = ((state.connectData && state.connectData.listeners) || [])
    .filter((l) => !l.disabled && l.kind === 'gateway');
  const out = [];
  list.forEach((l) => {
    const addr = String(l.addr || '').trim();
    const parts = addr && splitAddr(addr);
    if (!parts) return;
    const anyHost = parts.host === '0.0.0.0' || parts.host === '::' || parts.host === '';
    // Слушатель на всех интерфейсах: из сети клиент идёт по имени машины, а не по 127.0.0.1.
    const lanHost = (state.connect && state.connect.host) || '127.0.0.1';
    // Схему берём у слушателя: на HTTPS-порту сниппет с http:// не заработает.
    const scheme = l.cert_file && l.key_file ? 'https' : 'http';
    const value = scheme + '://' + (anyHost ? lanHost : parts.host) + ':' + parts.port;
    if (out.some((a) => a.value === value)) return;
    out.push({ value, label: (anyHost ? addr + ' — из локальной сети' : addr) + (scheme === 'https' ? ' (https)' : '') });
  });
  if (!out.length && state.connect && state.connect.base_url) {
    out.push({ value: state.connect.base_url, label: state.connect.base_url });
  }
  return out;
}

// connectTokenValue возвращает подставляемое значение токена: расшифрованное,
// префикс (если значение не сохранено) или плейсхолдер.
function connectTokenValue() {
  const t = connectTokens().find((x) => String(x.id) === String(state.connectSel.tokenId));
  if (!t) return '<токен nr_...>';
  if (state.connectSecrets[t.id]) return state.connectSecrets[t.id];
  return t.secret_available ? '<токен nr_...>' : '<токен ' + t.prefix + '…>';
}

function connectCtx() {
  const addrs = connectAddrs();
  const addr = addrs.find((a) => a.value === state.connectSel.addr) || addrs[0];
  const base = (addr && addr.value) || (state.connect && state.connect.base_url) || 'http://127.0.0.1:47831';
  return {
    base_url: base,
    openai_url: base + '/v1',
    anthropic_url: base,
    gemini_url: base,
    model: (state.connect && state.connect.model) || 'naparnik',
    token: connectTokenValue(),
  };
}

// revealConnectToken расшифровывает значение токена один раз и запоминает его.
async function revealConnectToken(id) {
  if (!id || Object.prototype.hasOwnProperty.call(state.connectSecrets, id)) return;
  state.connectSecrets[id] = '';
  try {
    const res = await api('/api/tokens/' + id + '/reveal', { method: 'POST' });
    state.connectSecrets[id] = res.token || '';
  } catch (_) { /* значение недоступно — останется плейсхолдер */ }
}

function pickConnectClient(id) {
  state.connectSel.client = id;
  const main = document.getElementById('main');
  if (main) paintConnect(main);
}

// connectChanged вызывается селекторами мастера через data-onchange.
async function connectChanged() {
  const tEl = document.getElementById('cc_token');
  const aEl = document.getElementById('cc_addr');
  if (tEl) state.connectSel.tokenId = tEl.dataset.value;
  if (aEl) state.connectSel.addr = aEl.dataset.value;
  await revealConnectToken(state.connectSel.tokenId);
  const main = document.getElementById('main');
  if (main) paintConnect(main);
}

// copyConnect копирует инструкцию выбранного клиента с токеном и адресом.
function copyConnect() {
  const cl = CONNECT_CLIENTS.find((x) => x.id === state.connectSel.client);
  if (cl) copy(cl.text(connectCtx()));
}

async function renderConnect(main) {
  const [connect, tokens, listeners] = await Promise.all([
    api('/api/connect'), api('/api/tokens'), api('/api/listeners'),
  ]);
  state.connect = connect;
  state.connectData = { tokens, listeners };

  const sel = state.connectSel;
  if (!CONNECT_CLIENTS.some((cl) => cl.id === sel.client)) sel.client = CONNECT_CLIENTS[0].id;
  const list = connectTokens();
  if (!list.some((t) => String(t.id) === String(sel.tokenId))) {
    sel.tokenId = list.length ? String(list[0].id) : null;
  }
  await revealConnectToken(sel.tokenId);
  const addrs = connectAddrs();
  if (!addrs.some((a) => a.value === sel.addr)) sel.addr = addrs.length ? addrs[0].value : null;
  paintConnect(main);
}

function paintConnect(main) {
  const sel = state.connectSel;
  const cl = CONNECT_CLIENTS.find((x) => x.id === sel.client) || CONNECT_CLIENTS[0];
  const tokens = connectTokens();
  const addrs = connectAddrs();
  const done = CONNECT_CLIENTS.filter((x) => x.verified).length;

  const tokenOpts = tokens.length
    ? tokens.map((t) => ({
      value: String(t.id),
      label: t.name + ' — ' + t.prefix + (t.provider ? ' · ' + (CONNECT_PROTO_NAMES[t.provider] || t.provider) : ''),
    }))
    : [{ value: '', label: 'токенов нет' }];
  const addrOpts = addrs.length ? addrs : [{ value: '', label: 'слушателей нет' }];

  const token = tokens.find((t) => String(t.id) === String(sel.tokenId));
  const warnings = [];
  if (!tokens.length) {
    warnings.push(`<p class="muted small">Токенов пока нет — <button type="button" class="btn ghost small" onclick="go('tokens')">выпустить токен</button>. В инструкции останется плейсхолдер.</p>`);
  } else if (token && !token.secret_available) {
    warnings.push(`<p class="muted small">У токена «${esc(token.name)}» значение не сохранено — вставьте его из диалога выпуска.</p>`);
  }
  const proto = token && token.provider;
  if (proto && proto !== cl.proto) {
    warnings.push(`<p class="small" style="color:var(--warn)">Токен ограничен протоколом «${esc(CONNECT_PROTO_NAMES[proto] || proto)}», а ${esc(cl.name)} работает через ${esc(CONNECT_PROTO_NAMES[cl.proto])}. Возьмите токен без ограничения или с нужным протоколом.</p>`);
  }

  main.innerHTML = `
    <h1>Подключение ${infoBtn('Выберите IDE, токен и адрес шлюза — инструкция соберётся сама. Значок «Проверено» ставится после реального подключения клиента.', 'Про подключение')}</h1>
    <p class="muted small">Проверено: ${done} из ${CONNECT_CLIENTS.length}. Остальные инструкции рабочие, но в этих клиентах ещё не подтверждены.</p>
    <div class="tiles">
      ${CONNECT_CLIENTS.map((x) => `<button type="button" class="tile${x.id === cl.id ? ' active' : ''}" onclick="pickConnectClient('${x.id}')">
        <span class="tile-head"><span class="tile-name">${esc(x.name)}</span>${connectBadge(x.verified)}</span>
        <span class="tile-note">${esc(x.note)}</span>
      </button>`).join('')}
    </div>
    <section class="card settings-section">
      <div class="card-head">
        <h2>${esc(cl.name)} ${connectBadge(cl.verified)}</h2>
        <span class="spacer"></span>
        ${iconBtn('copy', 'Копировать инструкцию', 'copyConnect()')}
      </div>
      <div class="grid cols2">
        <label>Токен${ddHTML('cc_token', tokenOpts, sel.tokenId == null ? '' : String(sel.tokenId), '', 'data-onchange="connectChanged"')}</label>
        <label>Подключение${ddHTML('cc_addr', addrOpts, sel.addr || '', '', 'data-onchange="connectChanged"')}</label>
      </div>
      ${warnings.join('')}
      <pre>${esc(cl.text(connectCtx()))}</pre>
    </section>`;
}

/* ---------- старт ---------- */

boot();
setInterval(() => {
  if (state.authenticated && state.view === 'overview') {
    const main = document.getElementById('main');
    if (main && !document.querySelector('.modal-backdrop')) {
      renderOverview(main).catch(() => {});
    }
  }
}, 30000);
