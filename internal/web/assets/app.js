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
  view: 'overview',
  keyID: null,
  usageRange: '24h',
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
  return d.toLocaleString('ru-RU', { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' });
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
      <div class="login-brand">${LOGO}<span>Напарник<small>админка шлюза</small></span></div>
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
      <div class="login-brand">${LOGO}<span>Напарник<small>админка шлюза</small></span></div>
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
  listeners: '<path d="M12 3v8"/><path d="M7 6.5a7 7 0 0 0 10 0"/><path d="M9 21h6"/><path d="M12 15v6"/>',
  settings: '<path d="M4 7h9M17 7h3M4 17h3M11 17h9"/><circle cx="15" cy="7" r="2"/><circle cx="9" cy="17" r="2"/>',
  connect: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M7 9l3 3-3 3M12 15h5"/>',
};

// Знак «Напарника»: монограмма Н в золотой плитке.
const LOGO = `<svg class="brand-mark" viewBox="0 0 32 32" aria-hidden="true">
  <defs>
    <linearGradient id="logoGold" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="#ffe9a8"></stop>
      <stop offset="0.45" stop-color="#f0c14b"></stop>
      <stop offset="1" stop-color="#b8862a"></stop>
    </linearGradient>
  </defs>
  <rect x="1" y="1" width="30" height="30" rx="9" fill="url(#logoGold)"></rect>
  <rect x="1" y="1" width="30" height="30" rx="9" fill="none" stroke="rgba(255,255,255,.35)" stroke-width="1"></rect>
  <path d="M10.5 9.5v13M21.5 9.5v13M10.5 16h11" fill="none" stroke="#1a1a16" stroke-width="2.6" stroke-linecap="round"></path>
  <circle cx="24.2" cy="8.2" r="2.1" fill="#1a1a16"></circle>
</svg>`;

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
      <aside class="sidebar">
        <div class="brand">${LOGO}<span>Напарник<small>админка шлюза</small></span></div>
        <div class="nav">
          ${VIEWS.map((v) => `<div class="nav-group">${v.group}</div>${v.items.map(([id, t]) => navButton(id, t)).join('')}`).join('')}
        </div>
        <div class="sidebar-footer">
          <button class="btn ghost small" style="width:100%" onclick="logout()">Выйти</button>
        </div>
      </aside>
      <main class="main" id="main"></main>
    </div>`;
  await go(state.view);
}

async function go(view) {
  state.view = view;
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
        <thead><tr><th>Название</th><th>Статус</th><th>Тариф</th><th>Токенов</th><th></th></tr></thead>
        <tbody>
          ${keys.map((k) => `
            <tr>
              <td>${esc(k.name)}</td>
              <td>${k.disabled ? '<span class="badge crit">отключён</span>'
                : k.status === 'active' ? '<span class="badge ok">активен</span>'
                : '<span class="badge">' + esc(statusLabel(k.status)) + '</span>'}</td>
              <td class="small muted">${esc(k.tariff_label || '—')}</td>
              <td>${tokens.filter((t) => t.key_id === k.id).length}</td>
              <td class="actions">
                ${iconBtn('refresh', 'Обновить бюджет', `refreshKey(${k.id})`)}
                ${iconBtn('chart', 'Статистика', `openKey(${k.id})`)}
                ${iconBtn('edit', 'Изменить название, тариф или ключ', `openKeyEdit(${k.id})`)}
                ${iconBtn(k.disabled ? 'powerOn' : 'powerOff', k.disabled ? 'Включить' : 'Отключить', `toggleKey(${k.id}, ${!k.disabled})`)}
                ${iconBtn('trash', 'Удалить', `deleteKey(${k.id})`, 'danger')}
              </td>
            </tr>`).join('') || '<tr><td colspan="5" class="muted">Пока нет ключей</td></tr>'}
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
  eye: '<path d="M2 12s3.6-6 10-6 10 6 10 6-3.6 6-10 6-10-6-10-6z"/><circle cx="12" cy="12" r="3"/>',
  rotate: '<path d="M20 12a8 8 0 1 1-2.34-5.66"/><path d="M20 3.5V9h-5.5"/>',
};

function iconBtn(name, title, onclick, cls) {
  return `<button class="btn icon ghost ${cls || ''}" title="${esc(title)}" aria-label="${esc(title)}" onclick="${onclick}">
    <svg viewBox="0 0 24 24" fill="none" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">${ACTION_ICONS[name] || ''}</svg>
  </button>`;
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
  if (!confirm('Удалить ключ вместе с его токенами и сессиями?')) return;
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
  wrap.innerHTML = `
    <div class="modal">
      <h2 style="margin-top:0">Изменить ключ</h2>
      <div class="grid">
        <label>Название<input id="e_name" value="${esc(k.name)}"></label>
        <label>Тариф${ddHTML('e_tariff', tariffOpts(k.tariff_label), k.tariff_label || '', '')}</label>
        <label>Ключ Напарника
          <div class="row" style="gap:6px;flex-wrap:nowrap">
            <input id="e_key" class="mono" value="${esc(secret || k.key_hint)}" style="flex:1" ${secret ? '' : 'readonly'}>
            ${iconBtn('copy', 'Копировать ключ', 'copyKeyField()')}
          </div>
        </label>
        <p class="muted small" style="margin:0">Измените значение и сохраните — ключ заменится. Маску ключа в списке
          увидите, если не удалось его расшифровать. При доступе к админке по сети нужен HTTPS.</p>
      </div>
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
      <h3>Выпустить токен</h3>
      <div class="row">
        ${ddHTML('tkey', keys.map((k) => ({ value: String(k.id), label: k.name + ' · ' + k.key_hint })), keys[0] ? String(keys[0].id) : '', 'flex:1.4;min-width:220px')}
        <input id="tname" placeholder="Название (например, «Cline, ноутбук»)" style="flex:2;min-width:200px">
        ${ddHTML('tprovider', PROVIDERS, '', 'flex:1;min-width:210px')}
        ${ddHTML('tmode', [
          { value: '', label: 'поток: как в настройках' },
          { value: 'final', label: 'final — цельным куском' },
          { value: 'deltas', label: 'deltas — по мере генерации' },
        ], '', 'flex:1;min-width:200px')}
        <button class="btn" onclick="addToken()">Выпустить</button>
      </div>
      <p class="muted small" style="margin-bottom:0">Один ключ Напарника может иметь сколько угодно токенов — по одному на IDE или разработчика.</p>
    </div>
    <div class="card">
      <table>
        <thead><tr><th>Название</th><th>Ключ</th><th>Провайдер</th><th>Префикс</th><th>Поток</th><th>Последний</th><th></th></tr></thead>
        <tbody>
          ${tokens.map((t) => `<tr>
            <td>${esc(t.name)} ${t.revoked_at ? '<span class="badge crit">отозван</span>' : t.enabled ? '' : '<span class="badge warn">выключен</span>'}</td>
            <td class="small muted">${esc(keyName(t.key_id))}<br>
              <span class="mono" style="font-size:11.5px;color:var(--muted-2)">${esc(keyHint(t.key_id))}</span></td>
            <td class="small muted">${esc(providerLabel(t.provider))}</td>
            <td class="mono small" title="Первые символы токена — по ним токен находится в базе. Полное значение показывается один раз при выпуске.">${esc(t.prefix)}…</td>
            <td class="small muted">${esc(t.stream_mode || 'по умолчанию')}</td>
            <td class="small muted">${fmtTime(t.last_used_at)}</td>
            <td class="actions">
              ${t.secret_available ? iconBtn('copy', 'Скопировать токен — можно передать сотруднику', `copyToken(${t.id})`) : ''}
              ${iconBtn('edit', 'Изменить токен: название, ключ, провайдер, поток', `openTokenEdit(${t.id})`)}
              ${iconBtn(t.enabled ? 'powerOff' : 'powerOn', t.enabled ? 'Выключить' : 'Включить', `toggleToken(${t.id}, ${!t.enabled})`)}
              ${iconBtn('revoke', 'Отозвать — IDE перестанет подключаться', `revokeToken(${t.id})`, 'danger')}
            </td>
          </tr>`).join('') || '<tr><td colspan="7" class="muted">Пока нет токенов</td></tr>'}
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
  if (!confirm('Отозвать токен? IDE перестанет подключаться.')) return;
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
      <h2 style="margin-top:0">Изменить токен</h2>
      <div class="grid">
        <label>Название<input id="te_name" value="${esc(t.name)}"></label>
        <label>Ключ Напарника${ddHTML('te_key',
          keys.map((k) => ({ value: String(k.id), label: k.name + ' · ' + k.key_hint })),
          String(t.key_id), '')}</label>
        <label>Провайдер${ddHTML('te_provider', PROVIDERS, t.provider || '', '')}</label>
        <label>Поток${ddHTML('te_stream', [
          { value: '', label: 'как в настройках' },
          { value: 'final', label: 'final — цельным куском' },
          { value: 'deltas', label: 'deltas — по мере генерации' },
        ], t.stream_mode || '', '')}</label>
        <label>Состояние${ddHTML('te_enabled', [
          { value: '1', label: 'включён' },
          { value: '0', label: 'выключен' },
        ], t.enabled ? '1' : '0', '')}</label>
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
  if (!confirm('Сгенерировать новый токен? Старое значение сразу перестанет работать, а токен снова станет активным. Настройки (ключ, провайдер, поток) сохранятся.')) return;
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
  const key_id = Number(ddValue('te_key'));
  const provider = ddValue('te_provider');
  const stream_mode = ddValue('te_stream');
  const enabled = ddValue('te_enabled') === '1';
  try {
    await api('/api/tokens/' + id, { method: 'PATCH', body: { key_id, name, provider, stream_mode, enabled } });
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
    <h1>Сессии</h1>
    <p class="muted small">Привязка клиентской сессии к диалогу Напарника. Сброс создаёт новый диалог при следующем запросе.</p>
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
              <button class="btn ghost small" onclick="resetSession(${s.token_id}, '${esc(s.protocol)}', '${esc(s.session_id)}')">Сбросить</button>
            </td>
          </tr>`).join('') || '<tr><td colspan="6" class="muted">Активных сессий нет</td></tr>'}
        </tbody>
      </table>
    </div>`;
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

async function renderLogs(main) {
  const [keys, tokens] = await Promise.all([api('/api/keys'), api('/api/tokens')]);
  const res = await api('/api/logs?limit=100');
  const tokenName = (id) => (tokens.find((t) => t.id === id) || {}).name || ('#' + id);

  main.innerHTML = `
    <h1>Журнал запросов</h1>
    <div class="card">
      <div class="row small muted">Всего записей: ${fmtNum(res.total)}</div>
      <table>
        <thead><tr><th>Время</th><th>Токен</th><th>Протокол</th><th>Статус</th><th>Токены</th><th>мс</th></tr></thead>
        <tbody>
          ${res.entries.map((e) => `<tr>
            <td class="small">${fmtTime(e.ts)}</td>
            <td class="small">${esc(tokenName(e.token_id))}</td>
            <td class="small muted">${esc(e.protocol)}${e.stream ? ' · SSE' : ''}</td>
            <td class="small">${e.status >= 400
              ? '<span class="badge crit">' + e.status + ' ' + esc(e.error_type || '') + '</span>'
              : '<span class="badge ok">' + e.status + '</span>'}</td>
            <td class="small">${fmtNum(e.prompt_tokens + e.completion_tokens)}</td>
            <td class="small muted">${e.latency_ms}</td>
          </tr>`).join('') || '<tr><td colspan="6" class="muted">Записей нет</td></tr>'}
        </tbody>
      </table>
    </div>`;
}

/* ---------- слушатели ---------- */

async function renderListeners(main) {
  state.listeners = await api('/api/listeners');
  paintListeners(main);
}

function paintListeners(main) {
  main.innerHTML = `
    <h1>Слушатели</h1>
    <p class="muted small">Можно слушать несколько портов одновременно. Для доступа из локальной сети укажите 0.0.0.0:порт.
    Админку держите на 127.0.0.1, если не нужен удалённый доступ.</p>
    <div class="card">
      <table>
        <thead><tr><th>Адрес</th><th>Тип</th><th></th></tr></thead>
        <tbody id="listeners-body">
          ${state.listeners.map((l, i) => listenerRow(l, i)).join('') || '<tr><td colspan="3" class="muted">Слушателей нет — добавьте хотя бы один.</td></tr>'}
        </tbody>
      </table>
      <div class="row" style="margin-top:12px">
        <button class="btn ghost" onclick="addListenerRow()">Добавить</button>
        <span class="spacer"></span>
        <button class="btn" onclick="saveListeners()">Применить</button>
      </div>
    </div>`;
}

function listenerRow(l, i) {
  const kinds = [
    { value: 'gateway', label: 'gateway — шлюз для IDE' },
    { value: 'admin', label: 'admin — админка' },
  ];
  return `<tr>
    <td><input value="${esc(l.addr)}" data-addr="${i}" style="width:100%"></td>
    <td>${ddHTML('lkind-' + i, kinds, l.kind, '', 'data-kind="' + i + '"')}</td>
    <td class="actions">${iconBtn('trash', 'Удалить слушатель', 'removeListenerRow(' + i + ')', 'danger')}</td>
  </tr>`;
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
  state.listeners.push({ addr: '127.0.0.1:47833', kind: 'gateway' });
  paintListeners(document.getElementById('main'));
}

function removeListenerRow(i) {
  syncListenerInputs();
  state.listeners.splice(i, 1);
  paintListeners(document.getElementById('main'));
}

async function saveListeners() {
  syncListenerInputs();
  const list = state.listeners
    .map((l) => ({ addr: String(l.addr || '').trim(), kind: l.kind }))
    .filter((l) => l.addr !== '');
  if (!list.length) return toast('Нужен хотя бы один слушатель', true);
  const dup = list.find((l, i) => list.findIndex((x) => x.addr === l.addr) !== i);
  if (dup) return toast('Адрес указан дважды: ' + dup.addr, true);
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
    <div class="card grid cols2">
      <label>Режим потока по умолчанию
        ${ddHTML('s_stream', [
          { value: 'final', label: 'final — отдавать финальный текст' },
          { value: 'deltas', label: 'deltas — по мере генерации' },
        ], s.stream_mode)}
      </label>
      <label>Порог авто-компактизации
        <input id="s_compact" type="number" step="0.05" min="0.1" max="1" value="${s.auto_compact_at}">
      </label>
      <label>TTL сессии
        <input id="s_ttl" value="${esc(s.session_ttl)}">
      </label>
      <label>Таймаут запроса
        <input id="s_timeout" value="${esc(s.request_timeout)}">
      </label>
      <label>Период опроса бюджета
        <input id="s_poll" value="${esc(s.budget_poll_interval)}">
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
    <div class="row"><button class="btn" onclick="saveSettings()">Сохранить</button></div>
    <p class="muted small">Часть настроек (таймаут запроса, период опроса) применится после перезапуска сервиса.</p>`;
}

async function saveSettings() {
  const body = {
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

async function renderConnect(main) {
  const c = await api('/api/connect');
  state.connect = c;
  const snippets = [
    ['OpenAI-совместимые клиенты (Cline, Roo Code, Continue, Zed)',
      `Provider: OpenAI Compatible
Base URL: ${c.openai_url}
API Key: <ваш токен nr_...>
Model: ${c.model}`],
    ['Claude Code',
      `export ANTHROPIC_BASE_URL=${c.anthropic_url}
export ANTHROPIC_API_KEY=<ваш токен nr_...>
claude`],
    ['Gemini CLI',
      `export GOOGLE_GEMINI_BASE_URL=${c.gemini_url}
export GEMINI_API_KEY=<ваш токен nr_...>
gemini`],
    ['Проверка через curl',
      `curl ${c.openai_url}/chat/completions \\
  -H "Authorization: Bearer <ваш токен>" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"${c.model}","messages":[{"role":"user","content":"Привет"}]}'`],
  ];

  main.innerHTML = `
    <h1>Подключение</h1>
    <p class="muted small">Базовый адрес взят из первого gateway-слушателя. Токен выпускается в разделе «Токены».</p>
    ${snippets.map(([title, code]) => `
      <div class="card">
        <div class="row"><strong>${esc(title)}</strong><span class="spacer"></span>
          <button class="btn ghost small" onclick="copy(${JSON.stringify(code).replace(/"/g, '&quot;')})">Копировать</button>
        </div>
        <pre>${esc(code)}</pre>
      </div>`).join('')}`;
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
