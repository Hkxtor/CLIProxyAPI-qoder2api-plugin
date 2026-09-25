package main

import "strings"

// consolePageHTML 返回控制台页面的静态 HTML。
//
// 安全约定：页面本身不含任何账号、额度或凭证数据；页面通过 CPA 的管理接口
// （/v0/management/plugins/qoder2api/...）按需拉取，需要操作者提供管理密钥。
// 渲染动态数据时统一走 textContent，避免把上游/账号名当成 HTML 注入。
//
// 为什么不做自动轮询中的"无限重试"：CPA 按客户端 IP 统计管理鉴权失败次数，
// 连续失败会封禁该 IP（连正确密钥也会被拒）。因此页面在遇到 401/403 时立刻停止
// 自动刷新，改为提示用户检查密钥。
func consolePageHTML(basePath string) string {
	replacer := strings.NewReplacer("__BASE__", basePath)
	return replacer.Replace(consolePageTemplate)
}

const consolePageTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Qoder 2API 控制台</title>
<style>
  :root { color-scheme: light dark; --bd:#8884; --bg2:#8881; }
  * { box-sizing: border-box; }
  body { margin:0; padding:20px; font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif; }
  h1 { font-size:18px; margin:0 0 4px; }
  h2 { font-size:15px; margin:24px 0 8px; }
  .muted { opacity:.7; font-size:12px; }
  .row { display:flex; gap:8px; flex-wrap:wrap; align-items:center; }
  .card { border:1px solid var(--bd); border-radius:10px; padding:14px; margin-bottom:14px; }
  .grid { display:grid; gap:10px; grid-template-columns:repeat(auto-fit,minmax(190px,1fr)); }
  .kv { border:1px solid var(--bd); border-radius:8px; padding:10px; }
  .kv b { display:block; font-size:16px; margin-top:2px; }
  button { font:inherit; padding:6px 12px; border-radius:8px; border:1px solid var(--bd); background:var(--bg2); cursor:pointer; }
  button:disabled { opacity:.5; cursor:not-allowed; }
  input[type=text], input[type=password] { font:inherit; padding:6px 8px; border-radius:8px; border:1px solid var(--bd); background:transparent; color:inherit; }
  table { width:100%; border-collapse:collapse; font-size:13px; }
  th, td { text-align:left; padding:6px 8px; border-bottom:1px solid var(--bd); vertical-align:top; }
  th { font-weight:600; opacity:.8; white-space:nowrap; }
  code { font-size:12px; opacity:.9; }
  .pill { display:inline-block; padding:1px 8px; border-radius:999px; border:1px solid var(--bd); font-size:12px; }
  .ok { color:#1a7f37; } .warn { color:#9a6700; } .bad { color:#cf222e; }
  pre { max-height:240px; overflow:auto; background:var(--bg2); border-radius:8px; padding:10px; font-size:12px; }
  .sep { height:1px; background:var(--bd); margin:12px 0; }
</style>
</head>
<body>
<h1>Qoder 2API 控制台</h1>
<div class="muted">管理 Qoder 账号、额度与每日签到。数据来自 CPA 的 auth 文件；本页面不保存任何凭证。</div>

<div class="card row" style="margin-top:14px">
  <label class="muted" for="key">管理密钥</label>
  <input id="key" type="password" placeholder="CPA 管理密钥（management key）" size="34" autocomplete="off">
  <button id="save-key">保存密钥</button>
  <button id="reload">刷新</button>
  <label class="muted"><input id="auto" type="checkbox"> 每 30 秒自动刷新</label>
  <span id="key-state" class="muted"></span>
</div>

<div id="error" class="card bad" style="display:none"></div>

<div class="card">
  <h2 style="margin-top:0">概览</h2>
  <div class="grid" id="summary"></div>
  <div class="sep"></div>
  <div class="row">
    <button id="checkin-all">全部账号签到</button>
    <button id="quota-all">刷新全部额度</button>
    <button id="refresh-models">从上游刷新模型清单</button>
  </div>
  <div class="muted" id="action-state" style="margin-top:8px"></div>
</div>

<div class="card">
  <h2 style="margin-top:0">账号</h2>
  <div class="muted" id="account-hint">模型 ID 需要带 <code>qoder-</code> 前缀（例如 <code>qoder-claude-sonnet</code>）。</div>
  <table>
    <thead><tr><th>账号</th><th>区域</th><th>状态</th><th>签到</th><th>额度</th><th>操作</th></tr></thead>
    <tbody id="accounts"></tbody>
  </table>
</div>

<div class="card">
  <h2 style="margin-top:0">设置</h2>
  <div class="row" style="margin-bottom:8px">
    <label class="muted"><input id="auto-checkin" type="checkbox"> 每日自动签到</label>
    <label class="muted" for="checkin-at">时间</label>
    <input id="checkin-at" type="text" size="8" placeholder="10:00">
    <button id="save-settings">保存设置</button>
  </div>
  <div class="muted">自动签到依赖 CPA 常驻运行；上游活动窗口为 10:00 → 次日 10:00。</div>
</div>

<div class="card">
  <h2 style="margin-top:0">上游模型清单（实时缓存）</h2>
  <div class="muted" id="models-meta"></div>
  <table>
    <thead><tr><th>注册 ID（模型名）</th><th>上游 SKU</th><th>上下文</th><th>最大输出</th></tr></thead>
    <tbody id="models"></tbody>
  </table>
</div>

<div class="card">
  <h2 style="margin-top:0">插件日志</h2>
  <div class="row"><button id="logs-refresh">刷新日志</button><span class="muted" id="logs-meta"></span></div>
  <pre id="logs"></pre>
</div>

<script>
(function () {
  const BASE = '__BASE__';
  const API = '/v0/management' + BASE;
  const KEY_STORAGE = 'qoder2api.managementKey';
  const KEY_SOURCES = ['cli-proxy-auth', 'managementKey'];
  let autoTimer = null;
  let blocked = false;

  const $ = (id) => document.getElementById(id);
  const setText = (el, text) => { if (el) el.textContent = text == null ? '' : String(text); };

  function looksLikeKey(value) {
    const key = String(value == null ? '' : value).trim();
    if (!key || key.length > 512) return false;
    if (/\s/.test(key)) return false;
    const head = key.charAt(0);
    return head !== '{' && head !== '[' && head !== '"';
  }

  function findKey() {
    const stored = localStorage.getItem(KEY_STORAGE);
    if (looksLikeKey(stored)) return stored.trim();
    for (const name of KEY_SOURCES) {
      const raw = localStorage.getItem(name);
      if (!raw) continue;
      let value = raw;
      for (let depth = 0; depth < 3; depth++) {
        try { value = JSON.parse(value); } catch (_) { break; }
        if (typeof value === 'string') continue;
        if (value && typeof value === 'object') {
          const token = value.key || value.managementKey || value.token || value.state && value.state.key;
          if (looksLikeKey(token)) return String(token).trim();
          value = '';
          break;
        }
      }
      if (looksLikeKey(value)) return String(value).trim();
    }
    return '';
  }

  function currentKey() {
    const typed = $('key').value;
    if (looksLikeKey(typed)) return typed.trim();
    return findKey();
  }

  function showError(message, kind) {
    const box = $('error');
    if (!message) { box.style.display = 'none'; return; }
    box.style.display = '';
    box.className = 'card ' + (kind === 'bad' ? 'bad' : 'warn');
    box.textContent = message;
  }

  function stopAuto(reason) {
    blocked = true;
    $('auto').checked = false;
    if (autoTimer) { clearInterval(autoTimer); autoTimer = null; }
    setText($('action-state'), reason || '已停止自动刷新');
  }

  async function api(path, options) {
    const key = currentKey();
    if (!key) throw new Error('请先填写 CPA 管理密钥');
    const headers = Object.assign({ 'Content-Type': 'application/json' }, (options && options.headers) || {});
    headers['Authorization'] = 'Bearer ' + key;
    const response = await fetch(API + path, Object.assign({ headers }, options || {}));
    const text = await response.text();
    let payload = {};
    try { payload = text ? JSON.parse(text) : {}; } catch (_) { payload = { raw: text }; }
    if (response.status === 401 || response.status === 403) {
      stopAuto('管理密钥被拒绝，已停止自动刷新（避免触发 CPA 的 IP 封禁）');
      throw new Error('管理鉴权失败：请检查管理密钥');
    }
    if (!response.ok) throw new Error(payload.error || ('HTTP ' + response.status));
    return payload;
  }

  function row(cells) {
    const tr = document.createElement('tr');
    for (const cell of cells) {
      const td = document.createElement('td');
      if (typeof cell === 'string' || typeof cell === 'number') td.textContent = String(cell);
      else td.appendChild(cell);
      tr.appendChild(td);
    }
    return tr;
  }

  function button(label, onClick, className) {
    const b = document.createElement('button');
    b.textContent = label;
    if (className) b.className = className;
    b.addEventListener('click', onClick);
    return b;
  }

  function renderSummary(status) {
    const items = [
      ['账号数', status.accounts ? status.accounts.length : 0],
      ['区域', status.region],
      ['模型前缀', status.model_prefix],
      ['已注册模型', status.registered_models || 0],
      ['已缓存上游模型', status.model_count || 0],
      ['自动签到', status.auto_checkin ? ('开 · ' + status.auto_checkin_at) : '关'],
      ['最近自动签到', status.last_auto_checkin_on || '—'],
      ['插件版本', status.version],
    ];
    const container = $('summary');
    container.innerHTML = '';
    for (const [label, value] of items) {
      const cell = document.createElement('div');
      cell.className = 'kv';
      const name = document.createElement('span');
      name.className = 'muted';
      name.textContent = label;
      const strong = document.createElement('b');
      strong.textContent = String(value == null ? '—' : value);
      cell.appendChild(name);
      cell.appendChild(strong);
      container.appendChild(cell);
    }
    $('auto-checkin').checked = !!status.auto_checkin;
    $('checkin-at').value = status.auto_checkin_at || '10:00';
    if (status.warning) showError(status.warning, 'warn');
  }

  function renderAccounts(status) {
    const tbody = $('accounts');
    tbody.innerHTML = '';
    for (const account of status.accounts || []) {
      const name = document.createElement('div');
      name.textContent = account.label || account.name || account.id;
      const meta = document.createElement('div');
      meta.className = 'muted';
      meta.textContent = 'auth: ' + account.id + (account.path ? '' : '（无文件）');
      name.appendChild(meta);

      const stateCell = document.createElement('span');
      stateCell.className = 'pill' + (account.disabled || account.unavailable ? ' bad' : ' ok');
      stateCell.textContent = account.disabled ? '已禁用' : (account.unavailable ? '不可用' : '可用');

      const checkin = document.createElement('div');
      if (account.checkin_status) {
        checkin.className = 'pill' + (account.checkin_status === 'claimed' ? ' ok' : '');
        checkin.textContent = account.checkin_status;
        const detail = document.createElement('div');
        detail.className = 'muted';
        detail.textContent = [account.checkin_message, account.checkin_at].filter(Boolean).join(' · ');
        checkin.appendChild(detail);
      } else {
        checkin.className = 'muted';
        checkin.textContent = '—';
      }

      const quotaCell = document.createElement('div');
      quotaCell.className = 'muted';
      quotaCell.dataset.account = account.id;
      quotaCell.textContent = '点右侧按钮查询';

      const actions = document.createElement('div');
      actions.className = 'row';
      actions.appendChild(button('签到', async () => {
        actions.querySelectorAll('button').forEach((b) => { b.disabled = true; });
        setText($('action-state'), '正在签到：' + account.id + ' ...');
        try {
          const result = await api('/checkin', { method: 'POST', body: JSON.stringify({ account_ids: [account.id] }) });
          const first = (result.results || [])[0] || {};
          setText($('action-state'), account.id + ' → ' + (first.status || 'unknown') + ' ' + (first.message || ''));
          await load();
        } catch (err) { showError(err.message, 'bad'); }
        actions.querySelectorAll('button').forEach((b) => { b.disabled = false; });
      }));
      actions.appendChild(button('额度', async () => {
        actions.querySelectorAll('button').forEach((b) => { b.disabled = true; });
        try {
          const result = await api('/quotas', { method: 'POST', body: JSON.stringify({ account_ids: [account.id] }) });
          const entry = (result.accounts || [])[0] || {};
          quotaCell.innerHTML = '';
          if (entry.error) { quotaCell.textContent = entry.error; }
          else { renderQuota(quotaCell, entry.quota); }
        } catch (err) { showError(err.message, 'bad'); }
        actions.querySelectorAll('button').forEach((b) => { b.disabled = false; });
      }));
      actions.appendChild(button('模型', async () => {
        actions.querySelectorAll('button').forEach((b) => { b.disabled = true; });
        try {
          const result = await api('/models/refresh', { method: 'POST', body: '{}' });
          setText($('action-state'), '已从上游刷新 ' + result.models + ' 个模型（重启/重载后 CPA 会重新注册模型）');
          await load();
        } catch (err) { showError(err.message, 'bad'); }
        actions.querySelectorAll('button').forEach((b) => { b.disabled = false; });
      }));

      tbody.appendChild(row([name, account.region || status.region, stateCell, checkin, quotaCell, actions]));
    }
    if (!(status.accounts || []).length) {
      tbody.appendChild(row(['暂无账号', '', '', '', '', '']));
    }
  }

  function renderQuota(container, quota) {
    if (!quota) { container.textContent = '—'; return; }
    if (quota.subscription && quota.subscription.plan) {
      const plan = document.createElement('div');
      plan.textContent = '套餐：' + quota.subscription.plan;
      container.appendChild(plan);
    }
    for (const group of quota.groups || []) {
      for (const bucket of group.buckets || []) {
        const line = document.createElement('div');
        const percent = Math.round((bucket.remainingFraction || 0) * 100);
        line.textContent = (group.displayName || '额度') + '：剩余 ' + percent + '%' + (bucket.description ? '（' + bucket.description + '）' : '');
        container.appendChild(line);
      }
    }
    for (const metric of quota.summary || []) {
      const line = document.createElement('div');
      line.className = 'muted';
      line.textContent = metric.label + '：' + metric.value + (metric.unit ? ' ' + metric.unit : '');
      container.appendChild(line);
    }
  }

  function renderModels(status) {
    const tbody = $('models');
    tbody.innerHTML = '';
    setText($('models-meta'), status.models_fetched_at
      ? ('最近刷新：' + status.models_fetched_at + '（区域 ' + (status.models_region || '—') + '，共 ' + (status.model_count || 0) + ' 个）')
      : '尚未拉取实时清单：当前使用内置兜底 SKU 列表。点击上方「从上游刷新模型清单」。');
    for (const model of status.model_preview || []) {
      tbody.appendChild(row([
        model.registered_id || '',
        model.key,
        model.context_window || '',
        model.max_output || '',
      ]));
    }
  }

  async function load() {
    if (blocked) return;
    try {
      const status = await api('/status');
      showError(status.warning || '', 'warn');
      renderSummary(status);
      renderAccounts(status);
      renderModels(status);
      setText($('key-state'), '已认证');
    } catch (err) {
      showError(err.message, 'bad');
    }
  }

  async function loadLogs() {
    try {
      const payload = await api('/logs?limit=200');
      const lines = (payload.entries || []).map((entry) => '[' + entry.time + '][' + entry.level + '] ' + entry.message);
      setText($('logs'), lines.join('\n'));
      setText($('logs-meta'), 'last_seq=' + payload.last_seq);
    } catch (err) { showError(err.message, 'bad'); }
  }

  function syncAuto() {
    if (autoTimer) { clearInterval(autoTimer); autoTimer = null; }
    if ($('auto').checked && !blocked) autoTimer = setInterval(load, 30000);
  }

  $('save-key').addEventListener('click', () => {
    const typed = $('key').value.trim();
    if (!typed) { showError('请先填写管理密钥', 'bad'); return; }
    localStorage.setItem(KEY_STORAGE, typed);
    blocked = false;
    setText($('key-state'), '已保存到浏览器 localStorage');
    load();
  });
  $('reload').addEventListener('click', load);
  $('auto').addEventListener('change', syncAuto);
  $('logs-refresh').addEventListener('click', loadLogs);
  $('refresh-models').addEventListener('click', async () => {
    try {
      const result = await api('/models/refresh', { method: 'POST', body: '{}' });
      setText($('action-state'), '已刷新 ' + result.models + ' 个模型');
      await load();
    } catch (err) { showError(err.message, 'bad'); }
  });
  $('checkin-all').addEventListener('click', async () => {
    setText($('action-state'), '正在为全部账号签到 ...');
    try {
      const result = await api('/checkin', { method: 'POST', body: '{}' });
      setText($('action-state'), '签到完成：' + result.claimed + ' / ' + result.total + ' 个账号成功领取');
      await load();
    } catch (err) { showError(err.message, 'bad'); }
  });
  $('quota-all').addEventListener('click', async () => {
    setText($('action-state'), '正在查询额度 ...');
    try {
      const result = await api('/quotas', { method: 'POST', body: JSON.stringify({ concurrent: 2 }) });
      for (const entry of result.accounts || []) {
        const cell = document.querySelector('[data-account="' + entry.account_id + '"]');
        if (!cell) continue;
        cell.innerHTML = '';
        if (entry.error) cell.textContent = entry.error;
        else renderQuota(cell, entry.quota);
      }
      setText($('action-state'), '额度已更新');
    } catch (err) { showError(err.message, 'bad'); }
  });
  $('save-settings').addEventListener('click', async () => {
    try {
      const body = JSON.stringify({ auto_checkin: $('auto-checkin').checked, auto_checkin_at: $('checkin-at').value.trim() || '10:00' });
      const result = await api('/settings', { method: 'POST', body });
      setText($('action-state'), '设置已保存：自动签到 ' + (result.auto_checkin ? '开（' + result.auto_checkin_at + '）' : '关'));
      await load();
    } catch (err) { showError(err.message, 'bad'); }
  });

  const saved = findKey();
  if (saved) setText($('key-state'), '已从浏览器读取到管理密钥');
  load();
})();
</script>
</body>
</html>
`
