/* WPQU panel — 前端逻辑（无框架，原生 DOM） */

import { initI18n, cycleLang, t, currentLang } from './i18n/core.js';

// 首屏就要生效：设 <html lang>、翻一遍现有节点、挂上观察器接管后续渲染
initI18n();
document.title = `WPQU panel · ${t('本地运维面板')}`;

const state = {
  servers: [],
  currentId: null,
  snapshot: null,
  tab: 'overview',
  filePath: '/',
  files: null,
  ftp: null, // { for: serverId, data: 状态 }
  maint: newMaintState(), // 维护栏目：php / logs / cron / exec
  sel: new Set(), // 文件管理里勾选的绝对路径
  selPath: '', // 选择集对应的目录，切目录时清空
  backups: [],
  loadingSite: false,
  db: newDbState(),
  plugins: newPluginState(),
  version: '',
};

function newMaintState() {
  return {
    tab: 'php', // php | logs | cron | exec
    php: null,
    logs: null,
    logFilter: 'all',
    cron: null,
    exec: { out: '', running: false, cmd: '', cwd: '', seconds: 120 },
  };
}

function newPluginState() {
  return {
    loaded: false, // 市场清单是否拉过
    loading: false,
    catalog: [],
    installed: {}, // 本地已解压：id -> {version, installedAt}
    licenses: {}, // 已保存的授权摘要：id -> {name, expire, seats}
    uiTabs: [], // 已装的界面插件：变成页签
    status: {}, // serverId -> {pluginId: {installed, version}}
    statusFor: '', // status 对应的服务器，切服务器时作废
    q: '',
    cat: 'all',
    tier: 'all',
    error: '',
    source: '',
    updated: '',
  };
}

function newDbState() {  return {
    loaded: false, // 是否已拉过库列表
    loadedFor: '', // 表列表对应的库名
    name: '',
    databases: [],
    tables: [],
    site: null,
    table: '',
    view: 'data', // data | columns | indexes
    offset: 0,
    limit: 50,
    detail: null,
    credText: '',
  };
}

const $ = (sel) => document.querySelector(sel);
const view = () => $('#view');

const esc = (s) =>
  String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');

function fmtBytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v.toFixed(v >= 10 || i === 0 ? 0 : 1)} ${u[i]}`;
}

function fmtTime(ts) {
  if (!ts) return '—';
  const d = new Date(ts);
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function toast(msg, kind = '') {
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.textContent = msg;
  $('#toast').appendChild(el);
  setTimeout(() => {
    el.style.opacity = '0';
    el.style.transition = 'opacity .2s';
    setTimeout(() => el.remove(), 250);
  }, kind === 'err' ? 5000 : 3000);
}

async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: { 'content-type': 'application/json', 'x-panel': '1', ...(opts.headers || {}) },
  });
  const text = await res.text();
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = null;
  }
  if (!res.ok) {
    throw new Error((data && data.error) || `请求失败（${res.status}）`);
  }
  return data;
}

/* ---------------------------------------------------------------- 抽屉 */

function openDrawer(title) {
  $('#drawer-title').textContent = title;
  $('#drawer-body').textContent = '';
  $('#drawer-foot').textContent = '执行中…';
  $('#drawer').classList.remove('hidden');
}

function drawerAppend(text) {
  const body = $('#drawer-body');
  const atBottom = body.scrollHeight - body.scrollTop - body.clientHeight < 40;
  body.textContent += text;
  if (atBottom) body.scrollTop = body.scrollHeight;
}

function drawerDone(ok, message) {
  $('#drawer-foot').innerHTML = ok
    ? `<span style="color:#7ee2b8">${esc(message)}</span>`
    : `<span style="color:#f09595">${esc(message)}</span>`;
}

/**
 * 统一处理服务端的 ndjson 流。
 * hooks: { start(msg), out(text), end(evt), error(msg) }
 */
async function streamPost(path, body, hooks = {}) {
  let res;
  try {
    res = await fetch(path, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'x-panel': '1' },
      body: JSON.stringify(body),
    });
  } catch (err) {
    if (hooks.error) hooks.error(err.message);
    return { ok: false, message: err.message };
  }

  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    const msg = data.error || `请求失败（${res.status}）`;
    if (hooks.error) hooks.error(msg);
    return { ok: false, message: msg };
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  let result = { ok: false, message: '连接意外中断' };

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let idx;
    while ((idx = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, idx).trim();
      buf = buf.slice(idx + 1);
      if (!line) continue;
      let evt;
      try {
        evt = JSON.parse(line);
      } catch {
        continue;
      }
      if (evt.type === 'start') {
        if (hooks.start) hooks.start(evt.message);
      } else if (evt.type === 'out') {
        if (hooks.out) hooks.out(evt.text);
      } else if (evt.type === 'step') {
        if (hooks.step) hooks.step(evt);
      } else if (evt.type === 'progress') {
        if (hooks.progress) hooks.progress(evt);
      } else if (evt.type === 'end') {
        result = evt;
        if (hooks.end) hooks.end(evt);
      } else if (evt.type === 'error') {
        result = { ok: false, message: evt.message };
        if (hooks.error) hooks.error(evt.message);
      }
    }
  }
  return result;
}

async function runStream(path, body, title, onFinish) {
  openDrawer(title);
  let finished = false;
  await streamPost(path, body, {
    start: (msg) => drawerAppend(`▸ ${msg}\n\n`),
    out: (text) => drawerAppend(text),
    end: (evt) => {
      finished = true;
      drawerDone(evt.ok, evt.ok ? `${evt.message}（${(evt.durationMs / 1000).toFixed(1)}s）` : evt.message);
      if (onFinish) onFinish(evt);
    },
    error: (msg) => {
      finished = true;
      drawerAppend(`\n错误：${msg}\n`);
      drawerDone(false, msg);
    },
  });
  if (!finished) drawerDone(false, '连接意外中断');
}

/* ---------------------------------------------------------------- 弹窗 */

function closeModal() {
  $('#modal').classList.add('hidden');
  $('#modal').innerHTML = '';
}

function openModal({ title, subtitle = '', body, footer, wide = false }) {
  const root = $('#modal');
  root.innerHTML = `
    <div class="modal ${wide ? 'wide' : ''}" role="dialog">
      <div class="modal-head">
        <h2>${esc(title)}</h2>
        ${subtitle ? `<p>${esc(subtitle)}</p>` : ''}
      </div>
      <div class="modal-body">${body}</div>
      <div class="modal-foot">${footer}</div>
    </div>`;
  root.classList.remove('hidden');
  const first = root.querySelector('input, select, textarea');
  if (first) setTimeout(() => first.focus(), 30);
  return root.querySelector('.modal');
}

$('#modal').addEventListener('click', (e) => {
  if (e.target.id === 'modal') closeModal();
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && !$('#modal').classList.contains('hidden')) closeModal();
});

/* ---------------------------------------------------------------- 侧栏 */

function renderSidebar() {
  const list = $('#server-list');
  if (!state.servers.length) {
    list.innerHTML = `<div class="empty" style="padding:28px 12px">
      <strong>还没有服务器</strong>添加一台 ECS 开始管理
    </div>`;
    return;
  }
  list.innerHTML = state.servers
    .map((s) => {
      const active = s.id === state.currentId ? 'is-active' : '';
      const dot = s.lastStatus === 'ok' ? 'ok' : s.lastStatus === 'fail' ? 'fail' : '';
      return `<div class="server-item ${active}" data-server="${s.id}">
        <span class="dot ${dot}"></span>
        <div class="server-item-body">
          <div class="server-item-name">${esc(s.name)}</div>
          <div class="server-item-host">${esc(s.username)}@${esc(s.host)}:${s.port}</div>
        </div>
      </div>`;
    })
    .join('');
}

function currentServer() {
  return state.servers.find((s) => s.id === state.currentId) || null;
}

function renderTopbar() {
  const s = currentServer();
  if (!s) {
    $('#server-name').textContent = '未选择服务器';
    $('#server-meta').textContent = '';
    $('#server-actions').innerHTML = '';
    return;
  }
  $('#server-name').textContent = s.name;
  const ov = state.snapshot && state.snapshot.overview;
  $('#server-meta').textContent = ov
    ? `${s.username}@${s.host}:${s.port} · ${ov.uptime || '在线'} · PHP ${ov.php || '?'}`
    : `${s.username}@${s.host}:${s.port}${s.lastConnectedAt ? ` · 上次连接 ${fmtTime(s.lastConnectedAt)}` : ''}`;

  $('#server-actions').innerHTML = `
    <button class="btn btn-sm" data-act="refresh">${state.loadingSite ? '<span class="spinner"></span>' : '刷新'}</button>
    <button class="btn btn-sm" data-act="edit-server">编辑</button>
    <button class="btn btn-sm btn-danger" data-act="del-server">移除</button>`;
}

$('#server-actions').addEventListener('click', async (e) => {
  const btn = e.target.closest('[data-act]');
  if (!btn) return;
  const act = btn.dataset.act;
  if (act === 'refresh') return loadSnapshot();
  if (act === 'edit-server') return editServerModal();
  if (act === 'del-server') return removeServerFlow();
});

$('#server-list').addEventListener('click', (e) => {
  const item = e.target.closest('[data-server]');
  if (!item) return;
  selectServer(item.dataset.server);
});

$('#btn-add-server').addEventListener('click', () => serverModal());

$('#tabs').addEventListener('click', (e) => {
  const tab = e.target.closest('.tab');
  if (!tab) return;
  state.tab = tab.dataset.tab;
  document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('is-active', t === tab));
  renderView();
});

/* ---------------------------------------------------------------- 服务器操作 */

async function loadServers() {
  const data = await api('/api/servers');
  state.servers = data.servers || [];
  renderSidebar();
  if (!state.currentId && state.servers.length) {
    await selectServer(state.servers[0].id);
  }
}

async function selectServer(id) {
  state.currentId = id;
  state.snapshot = null;
  state.tab = 'overview';
  state.filePath = '/';
  state.ftp = null;
  state.maint = newMaintState();
  state.db = newDbState();
  document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('is-active', t.dataset.tab === 'overview'));
  renderSidebar();
  renderTopbar();
  await loadSnapshot();
}

async function loadSnapshot() {
  const s = currentServer();
  if (!s) return;
  state.loadingSite = true;
  renderTopbar();
  view().innerHTML = `<div class="empty"><span class="spinner"></span> 正在读取服务器状态…</div>`;
  try {
    state.snapshot = await api(`/api/servers/${s.id}/overview`);
    state.servers = state.servers.map((x) => (x.id === s.id ? { ...x, lastStatus: 'ok', lastConnectedAt: new Date().toISOString() } : x));
    renderSidebar();
  } catch (err) {
    state.servers = state.servers.map((x) => (x.id === s.id ? { ...x, lastStatus: 'fail' } : x));
    renderSidebar();
    view().innerHTML = `<div class="card">
      <h3 class="card-title" style="color:var(--danger)">无法连接服务器</h3>
      <p class="muted" style="margin:8px 0 0">${esc(err.message)}</p>
      <div style="margin-top:14px">
        <button class="btn btn-sm" data-act="edit-server">检查连接配置</button>
        <button class="btn btn-sm" data-act="retry">重试</button>
      </div>
    </div>`;
  } finally {
    state.loadingSite = false;
    renderTopbar();
    if (state.snapshot) renderView();
  }
}

function serverModal(existing = null) {
  const isEdit = Boolean(existing);
  const body = `
    <div class="field">
      <label>名称</label>
      <input type="text" id="f-name" placeholder="生产-主站" value="${esc(existing?.name || '')}">
    </div>
    <div class="field-row">
      <div class="field">
        <label>IP 或域名</label>
        <input type="text" id="f-host" placeholder="47.100.1.1" value="${esc(existing?.host || '')}">
      </div>
      <div class="field">
        <label>SSH 端口</label>
        <input type="number" id="f-port" value="${existing?.port || 22}">
      </div>
    </div>
    <div class="field">
      <label>登录用户</label>
      <input type="text" id="f-user" value="${esc(existing?.username || 'root')}">
      <div class="note">非 root 用户需要服务器已配置免密 sudo</div>
    </div>
    <div class="field">
      <label>认证方式</label>
      <select id="f-auth">
        <option value="password" ${existing?.authType !== 'key' ? 'selected' : ''}>密码</option>
        <option value="key" ${existing?.authType === 'key' ? 'selected' : ''}>私钥</option>
      </select>
    </div>
    <div class="field" id="wrap-password">
      <label>密码</label>
      <input type="password" id="f-password" placeholder="${isEdit ? '留空表示不修改' : ''}">
    </div>
    <div class="field hidden" id="wrap-key">
      <label>私钥内容</label>
      <textarea id="f-key" rows="7" placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea>
      <div class="note">${isEdit ? '留空表示不修改' : '粘贴完整私钥，包含首尾 BEGIN/END 行'}</div>
    </div>
    <div class="field hidden" id="wrap-passphrase">
      <label>私钥口令（可选）</label>
      <input type="password" id="f-passphrase">
    </div>`;

  openModal({
    title: isEdit ? '编辑服务器' : '添加服务器',
    subtitle: '凭据使用 AES-256-GCM 加密后保存在本机',
    body,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="btn-save-server">${isEdit ? '保存' : '添加并测试'}</button>`,
  });

  const syncAuth = () => {
    const isKey = $('#f-auth').value === 'key';
    $('#wrap-password').classList.toggle('hidden', isKey);
    $('#wrap-key').classList.toggle('hidden', !isKey);
    $('#wrap-passphrase').classList.toggle('hidden', !isKey);
  };
  $('#f-auth').value = existing?.authType || 'password';
  syncAuth();
  $('#f-auth').addEventListener('change', syncAuth);

  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);

  $('#btn-save-server').addEventListener('click', async () => {
    const payload = {
      name: $('#f-name').value.trim() || $('#f-host').value.trim(),
      host: $('#f-host').value.trim(),
      port: Number($('#f-port').value) || 22,
      username: $('#f-user').value.trim() || 'root',
      authType: $('#f-auth').value,
      password: $('#f-password').value,
      privateKey: $('#f-key').value,
      passphrase: $('#f-passphrase').value,
    };
    if (!payload.host) return toast('请填写服务器地址', 'err');
    const btn = $('#btn-save-server');
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner"></span> 保存中';
    try {
      let server;
      if (isEdit) {
        const r = await api(`/api/servers/${existing.id}`, { method: 'PATCH', body: JSON.stringify(payload) });
        server = r.server;
      } else {
        const r = await api('/api/servers', { method: 'POST', body: JSON.stringify(payload) });
        server = r.server;
      }
      closeModal();
      await loadServers();
      state.currentId = server.id;
      renderSidebar();
      toast('服务器已保存，正在测试连接…', 'ok');
      try {
        const t = await api(`/api/servers/${server.id}/test`, { method: 'POST', body: '{}' });
        toast(`连接成功：${t.info.os || t.info.hostname}`, 'ok');
      } catch (err) {
        toast(`连接失败：${err.message}`, 'err');
      }
      await selectServer(server.id);
    } catch (err) {
      toast(err.message, 'err');
      btn.disabled = false;
      btn.textContent = isEdit ? '保存' : '添加并测试';
    }
  });
}

function editServerModal() {
  const s = currentServer();
  if (s) serverModal(s);
}

function removeServerFlow() {
  const s = currentServer();
  if (!s) return;
  openModal({
    title: '移除服务器',
    subtitle: '只删除本机保存的连接信息，不会改动服务器上的任何内容',
    body: `<p style="margin:0">确定移除 <strong>${esc(s.name)}</strong>（${esc(s.host)}）吗？</p>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-danger" id="btn-del">确认移除</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#btn-del').addEventListener('click', async () => {
    try {
      await api(`/api/servers/${s.id}`, { method: 'DELETE' });
      closeModal();
      state.currentId = null;
      state.snapshot = null;
      await loadServers();
      if (!state.servers.length) {
        renderTopbar();
        view().innerHTML = `<div class="empty"><strong>还没有服务器</strong>点击左侧「添加」接入第一台</div>`;
      }
    } catch (err) {
      toast(err.message, 'err');
    }
  });
}

/* ---------------------------------------------------------------- 视图分发 */

function renderView() {
  const s = currentServer();
  if (!s) {
    view().innerHTML = `<div class="empty">
      <strong>还没有选择服务器</strong>
      在左侧添加一台云服务器，填写 IP、root 密码或私钥即可开始管理
    </div>`;
    return;
  }
  if (state.tab === 'overview') return renderOverview();
  if (state.tab === 'sites') return renderSites();
  if (state.tab === 'files') return renderFiles();
  if (state.tab === 'db') return renderDatabase();
  if (state.tab === 'ftp') return renderFTP();
  if (state.tab === 'plugins') return renderPlugins();
  if (state.tab.startsWith('plugin:')) return renderPluginMount(state.tab.slice(7));
  if (state.tab === 'maint') return renderMaint();
  if (state.tab === 'backups') return renderBackups();
  if (state.tab === 'logs') return renderLogs();
}

/* ------------------------------------------------------------ 双栈判断 */

// 面板支持 Nginx 与 Apache 两套站点实现，按钮文案、配置路径、快捷命令都要跟着切换
function webServer() {
  return state.snapshot?.overview?.webServer === 'apache' ? 'apache' : 'nginx';
}

function isApache() {
  return webServer() === 'apache';
}

function wsName() {
  return isApache() ? 'Apache' : 'Nginx';
}

// 站点配置在服务端的落点（用于弹窗里告诉用户到底删什么）
function siteConfPaths(domain) {
  return isApache()
    ? [`/etc/apache2/sites-available/${domain}.conf`, `/etc/apache2/sites-enabled/${domain}.conf`]
    : [`/etc/nginx/sites-available/${domain}`, `/etc/nginx/sites-enabled/${domain}`, `/etc/nginx/snippets/site-${domain}.conf`];
}

/* ---------------------------------------------------------------- 概览 */

function renderOverview() {
  const snap = state.snapshot;
  if (!snap) return;
  const ov = snap.overview;
  const memPct = ov.memTotal ? Math.round((ov.memUsed / ov.memTotal) * 100) : 0;
  const diskPct = Number(String(ov.diskPercent).replace('%', '')) || 0;
  const memClass = memPct > 85 ? 'is-danger' : memPct > 70 ? 'is-warn' : '';
  const diskClass = diskPct > 85 ? 'is-danger' : diskPct > 70 ? 'is-warn' : '';

  const expiring = snap.sites.filter((x) => x.certDaysLeft != null && x.certDaysLeft <= 21);

  view().innerHTML = `
    <div class="card">
      <div class="card-head">
        <h3 class="card-title">资源</h3>
        <span class="tag">${ov.cpu} 核 · 负载 ${esc(ov.load)}</span>
      </div>
      <div class="grid-stats">
        <div class="stat">
          <div class="stat-label">内存</div>
          <div class="stat-value">${ov.memUsed} <small>/ ${ov.memTotal} MB</small></div>
          <div class="bar"><i class="${memClass}" style="width:${memPct}%"></i></div>
        </div>
        <div class="stat">
          <div class="stat-label">磁盘</div>
          <div class="stat-value">${esc(ov.diskUsed)} <small>/ ${esc(ov.diskTotal)}</small></div>
          <div class="bar"><i class="${diskClass}" style="width:${diskPct}%"></i></div>
        </div>
        <div class="stat">
          <div class="stat-label">站点</div>
          <div class="stat-value">${ov.siteCount} <small>个站点 · ${ov.wpCount} 个 WordPress</small></div>
        </div>
        <div class="stat">
          <div class="stat-label">运行时长</div>
          <div class="stat-value" style="font-size:13px;padding-top:3px">${esc(ov.uptime || '—')}</div>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-head"><h3 class="card-title">运行环境</h3></div>
      <div class="grid-stats">
        <div class="stat"><div class="stat-label">${wsName()}</div><div class="stat-value" style="font-size:13px">${esc(ov.webVersion || ov.nginx || '—')}</div></div>
        <div class="stat"><div class="stat-label">PHP</div><div class="stat-value" style="font-size:13px">${esc(ov.php || '—')}</div></div>
        <div class="stat"><div class="stat-label">${esc(ov.dbFlavor || '数据库')}</div><div class="stat-value" style="font-size:13px">${esc(ov.mariadb || '—')}</div></div>
        <div class="stat"><div class="stat-label">certbot</div><div class="stat-value" style="font-size:13px">${ov.hasCertbot ? '已安装' : '未安装'}</div></div>
      </div>
    </div>

    ${
      expiring.length
        ? `<div class="card" style="border-color:rgba(154,100,0,.3)">
            <div class="card-head"><h3 class="card-title" style="color:var(--warn)">证书即将到期</h3></div>
            ${expiring
              .map(
                (x) =>
                  `<div style="display:flex;justify-content:space-between;padding:5px 0">
                    <span class="mono">${esc(x.primaryDomain)}</span>
                    <span class="tag warn">剩余 ${x.certDaysLeft} 天</span>
                  </div>`,
              )
              .join('')}
            <div style="margin-top:10px"><button class="btn btn-sm" data-act="renew-all">立即续期全部</button></div>
          </div>`
        : ''
    }

    <div class="card">
      <div class="card-head">
        <h3 class="card-title">站点</h3>
        <button class="btn btn-sm" data-act="goto-sites">管理站点</button>
      </div>
      ${
        snap.sites.length
          ? snap.sites
              .map(
                (x) => `<div style="display:flex;justify-content:space-between;gap:12px;padding:7px 0;border-bottom:0.5px solid var(--border)">
                  <div style="min-width:0">
                    <div style="font-weight:500">${esc(x.primaryDomain)}</div>
                    <div class="site-path">${esc(x.root)}</div>
                  </div>
                  <div style="display:flex;gap:5px;flex:none;align-items:flex-start">
                    ${x.isWordPress ? '<span class="tag info">WordPress</span>' : '<span class="tag">静态/其他</span>'}
                    ${x.ssl ? '<span class="tag ok">HTTPS</span>' : '<span class="tag warn">HTTP</span>'}
                  </div>
                </div>`,
              )
              .join('')
          : `<div class="muted">服务器上还没有 ${wsName()} 站点</div>`
      }
    </div>`;
}

/* ---------------------------------------------------------------- 站点 */

function renderSites() {
  const snap = state.snapshot;
  if (!snap) return;
  const sites = snap.sites;

  view().innerHTML = `
    <div class="card-head" style="margin-bottom:14px">
      <div>
        <div class="section-title" style="margin:0">站点 ${sites.length} 个</div>
      </div>
      <button class="btn btn-sm" data-act="doctor">环境体检</button>
      <button class="btn btn-primary btn-sm" data-act="add-site">+ 新增站点</button>
    </div>
    <div class="site-list">
      ${
        sites.length
          ? sites.map(siteCard).join('')
          : `<div class="empty">
               <strong>还没有站点</strong>
               如果这台服务器刚开通、还没装 Web 环境，可以先一键初始化
               <div style="margin-top:14px;display:flex;gap:8px;justify-content:center">
                 <button class="btn btn-primary btn-sm" data-act="bootstrap">初始化环境并装首个站点</button>
                 <button class="btn btn-sm" data-act="add-site">服务器已装好环境，直接加站点</button>
               </div>
             </div>`
      }
    </div>`;
}

const SITE_TYPES = [
  {
    value: 'wordpress',
    label: 'WordPress 站',
    short: 'WordPress',
    desc: '建库并安装 WordPress，开箱即用',
    hint: '博客、企业站首选；装好即可登录后台',
  },
  {
    value: 'custom',
    label: '自助网站',
    short: '自助站点',
    desc: '只建目录 + 数据库并启用 PHP',
    hint: '框架、论坛、自研 PHP 程序自行上传安装',
  },
  {
    value: 'static',
    label: '纯静态站',
    short: '静态站点',
    desc: '只有目录，不解析 PHP、不建数据库',
    hint: 'HTML、文档站、落地页，几乎不占内存',
  },
];

function siteTypeMeta(value) {
  return (
    SITE_TYPES.find((t) => t.value === value) || {
      value: value || '',
      label: value || '未知类型',
      short: value || '未知',
      desc: '',
      hint: '',
    }
  );
}

function typeTag(s) {
  const t = s.type || (s.isWordPress ? 'wordpress' : 'custom');
  if (t === 'wordpress') return '<span class="tag info">WordPress</span>';
  if (t === 'static') return '<span class="tag">纯静态</span>';
  return '<span class="tag">自助网站</span>';
}

function typePickerHtml(current) {
  return `<div class="type-picker" id="type-picker">
    ${SITE_TYPES.map(
      (t) => `<label class="type-opt" data-type-opt="${t.value}">
        <input type="radio" name="site-type" value="${t.value}" ${t.value === current ? 'checked' : ''}>
        <span class="type-opt-body">
          <b>${t.label}</b>
          <span>${t.desc}</span>
          <em>${t.hint}</em>
        </span>
      </label>`,
    ).join('')}
  </div>`;
}

/** 切换选择器时显示/隐藏对应字段，返回当前选中的类型 */
function applySiteType(scope) {
  const sel = scope.querySelector('input[name="site-type"]:checked');
  const type = sel ? sel.value : 'wordpress';
  const hasWp = scope.dataset.hasWp === '1';

  scope.querySelectorAll('[data-for]').forEach((el) => {
    let show = el.dataset.for.split(/\s+/).includes(type);
    if (show && el.dataset.when === 'no-wp' && hasWp) show = false;
    el.classList.toggle('is-hidden', !show);
  });

  const warn = scope.querySelector('[data-type-warning]');
  if (warn) {
    const current = scope.dataset.currentType || '';
    const map = {
      wordpress:
        '启用 WordPress 专用规则（伪静态、wp-admin、禁止直接读取 wp-config.php）。目录里还没有 WordPress 时会自动下载安装。',
      custom:
        '启用通用 PHP 规则，不做框架专属伪静态。目录内容与数据库都保留，需要 rewrite 时自行在能力片段里追加。',
      static:
        '关闭 PHP 解析（.php 直接返回 404），数据库与文件都保留、不会删除。从 WordPress 转过来时记得上传自己的静态首页。',
    };
    warn.textContent =
      type === current
        ? `当前已是该类型。保存后会按该类型重写 ${wsName()} 配置，可用于修复配置错乱。`
        : map[type] || '';
  }

  const save = scope.querySelector('#btn-save-type');
  if (save) {
    const same = type === (scope.dataset.currentType || '');
    save.textContent = same ? '重写配置' : '保存并转换';
  }

  const hint = scope.querySelector('[data-type-hint]');
  if (hint) {
    hint.textContent =
      type === 'static'
        ? '纯静态站不需要 PHP 与数据库，创建最快、占用最低。'
        : type === 'custom'
          ? '会创建独立数据库并启用 PHP，目录留空由你上传程序。'
          : '会创建数据库并安装 WordPress，完成后直接给后台地址与账号。';
  }
  return type;
}

function siteCard(s) {
  const days = s.certDaysLeft;
  const certTag =
    days == null
      ? ''
      : days < 0
        ? '<span class="tag danger">证书已过期</span>'
        : days <= 14
          ? `<span class="tag warn">证书剩 ${days} 天</span>`
          : `<span class="tag ok">证书剩 ${days} 天</span>`;

  return `<div class="site-card">
    <div class="site-top">
      <div style="min-width:0">
        <h4 class="site-domain">${esc(s.primaryDomain)}</h4>
        <div class="site-path">${esc(s.root)}</div>
        <div class="site-tags">
          ${typeTag(s)}
          ${s.ssl ? '<span class="tag ok">HTTPS</span>' : '<span class="tag warn">HTTP</span>'}
          ${certTag}
          ${s.domains.length > 1 ? `<span class="tag">${s.domains.length} 个域名</span>` : ''}
        </div>
      </div>
    </div>
    <div class="site-actions">
      <button class="btn btn-sm" data-site="${esc(s.primaryDomain)}" data-act="browse">浏览文件</button>
      <button class="btn btn-sm" data-site="${esc(s.primaryDomain)}" data-act="backup">备份</button>
      ${
        s.dbName
          ? `<button class="btn btn-sm" data-site="${esc(s.primaryDomain)}" data-db="${esc(s.dbName)}" data-act="site-db">数据库</button>`
          : ''
      }
      <button class="btn btn-sm" data-site="${esc(s.primaryDomain)}" data-act="domain">变更域名</button>
      ${
        s.ssl
          ? `<button class="btn btn-sm" data-site="${esc(s.primaryDomain)}" data-act="renew-one">续期证书</button>`
          : `<button class="btn btn-sm" data-site="${esc(s.primaryDomain)}" data-act="issue">申请 HTTPS</button>`
      }
      <button class="btn btn-sm" data-site="${esc(s.primaryDomain)}" data-act="edit-site">编辑站点</button>
      <button class="btn btn-sm btn-danger" data-site="${esc(s.primaryDomain)}" data-act="delete-site">删除站点</button>
    </div>
  </div>`;
}

/* ---------------------------------------------------------------- 站点操作 */

/* 视图内所有点击统一走这一个委托处理器，避免重复绑定 */
view().addEventListener('click', async (e) => {
  const crumb = e.target.closest('.crumb');
  if (crumb && crumb.dataset.path) {
    state.filePath = crumb.dataset.path;
    return renderFiles();
  }

  const fname = e.target.closest('.fname');
  if (fname && fname.dataset.path) {
    if (fname.dataset.type === 'dir') {
      state.filePath = fname.dataset.path;
      return renderFiles();
    }
    return openEditor(fname.dataset.path);
  }

  const el = e.target.closest('[data-act]');
  if (!el) return;
  const act = el.dataset.act;
  const p = el.dataset.path;
  const site = el.dataset.site;

  switch (act) {
    case 'retry':
      return loadSnapshot();
    case 'edit-server':
      return editServerModal();
    case 'bootstrap':
      return bootstrapModal();
    case 'goto-sites':
      state.tab = 'sites';
      document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('is-active', t.dataset.tab === 'sites'));
      return renderView();
    case 'add-site':
      return addSiteModal();
    case 'doctor':
      return doctorModal();
    case 'renew-all':
      return renewAll();
    case 'browse': {
      const target = (state.snapshot?.sites || []).find((x) => x.primaryDomain === site);
      state.filePath = target?.root || `/var/www/${site}`;
      state.tab = 'files';
      document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('is-active', t.dataset.tab === 'files'));
      return renderFiles();
    }
    case 'backup':
      return backupModal(site);
    case 'site-db': {
      state.tab = 'db';
      document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('is-active', t.dataset.tab === 'db'));
      state.db = newDbState();
      state.db.name = el.dataset.db || '';
      return renderDatabase();
    }
    case 'domain':
      return domainModal(site);
    case 'issue':
      return issueCertModal(site);
    case 'renew-one':
      return renewOne(site);
    case 'edit-site':
      return editSiteModal(site);
    case 'delete-site':
      return deleteSiteFlow(site);
    case 'reload-files':
      return renderFiles();
    case 'reload-backups':
      return renderBackups();
    case 'up':
    case 'enter':
      state.filePath = p;
      return renderFiles();
    case 'edit':
      return openEditor(p);
    case 'download':
      window.open(`/api/file/download?server=${state.currentId}&path=${encodeURIComponent(p)}`, '_blank');
      return undefined;
    case 'mkdir':
      return mkdirFlow();
    case 'upload':
      return uploadFlow();
    case 'rename':
      return renameFlow(p);
    case 'delete':
      return deleteFlow(p);
    case 'perm':
      return permModal([p], Number(el.dataset.mode), el.dataset.type === 'dir');
    case 'sel-perm':
      return selPermModal();
    case 'sel-copy':
      return transferFlow('copy');
    case 'sel-move':
      return transferFlow('move');
    case 'sel-delete':
      return selDeleteFlow();
    case 'sel-clear':
      state.sel.clear();
      return syncSelection();

    case 'ftp-install':
      return installFTPFlow();
    case 'ftp-uninstall':
      return uninstallFTPFlow();
    case 'ftp-reload':
      return renderFTP(true);
    case 'ftp-start':
    case 'ftp-stop':
    case 'ftp-restart':
      return ftpCall('/ftp/control', { action: act.replace('ftp-', '') }, '服务已' + (act === 'ftp-stop' ? '停止' : act === 'ftp-start' ? '启动' : '重启')).catch((err) =>
        toast(err.message, 'err'),
      );
    case 'ftp-save-config':
      return ftpSaveConfig();
    case 'ftp-add-user':
      return ftpAddUserModal();
    case 'ftp-user-dir':
      return ftpUserDirModal(el.dataset.name, el.dataset.dir);
    case 'ftp-user-pass':
      return ftpUserPassModal(el.dataset.name);
    case 'ftp-user-mode': {
      const next = el.dataset.writable !== '1';
      return ftpCall('/ftp/users/mode', { name: el.dataset.name, writable: next }, next ? '已改为可读写' : '已改为只读').catch(
        (err) => toast(err.message, 'err'),
      );
    }
    case 'ftp-user-del':
      return ftpUserDeleteModal(el.dataset.name, el.dataset.dir);

    case 'maint-tab': {
      const t = el.dataset.mtab;
      if (!t || t === state.maint.tab) return undefined;
      state.maint.tab = t;
      document.querySelectorAll('.subtab').forEach((x) => x.classList.toggle('is-active', x.dataset.mtab === t));
      const box = $('#maint-body');
      if (box) box.innerHTML = `<div class="muted"><span class="spinner"></span> 读取中…</div>`;
      return renderMaintPane();
    }
    case 'php-reload':
      return renderPHPPane(true);
    case 'php-install':
      return phpInstallFlow(el.dataset.version);
    case 'php-repo':
      return phpRepoFlow();
    case 'php-default':
      return phpDefaultFlow(el.dataset.version);
    case 'php-remove':
      return phpRemoveFlow(el.dataset.version, Number(el.dataset.sites || 0));
    case 'php-switch':
      return phpSwitchFlow(el.dataset.domain, el.dataset.version);

    case 'logs-reload':
      return renderLogsPane(true);
    case 'log-view':
      return openLogModal(el.dataset.path);
    case 'log-download':
      window.open(
        `/api/file/download?server=${state.currentId}&path=${encodeURIComponent(el.dataset.path)}`,
        '_blank',
      );
      return undefined;
    case 'log-rotate':
      return logActionFlow(el.dataset.path, 'rotate');
    case 'log-clear':
      return logActionFlow(el.dataset.path, 'clear');
    case 'log-delete':
      return logActionFlow(el.dataset.path, 'delete');

    case 'cron-reload':
      return renderCronPane(true);
    case 'cron-add':
      return cronAddModal();
    case 'cron-install':
      return cronInstallFlow();
    case 'cron-run':
      return cronRunFlow(el.dataset.command);
    case 'cron-toggle':
      return cronToggleFlow(Number(el.dataset.line), el.dataset.enabled === '1');
    case 'cron-delete':
      return cronDeleteFlow(Number(el.dataset.line), el.dataset.id, el.dataset.command);

    case 'exec-run':
      return execRun();
    case 'exec-quick': {
      const input = $('#exec-cmd');
      if (input) {
        input.value = el.dataset.cmd;
        input.focus();
      }
      return undefined;
    }
    case 'exec-clear-out': {
      state.maint.exec.out = '';
      const out = $('#exec-out');
      if (out) out.textContent = '（还没有输出）';
      return undefined;
    }

    case 'db-reload':
      state.db.loaded = false;
      state.db.loadedFor = '';
      return renderDatabase();
    case 'db-table':
      state.db.table = el.dataset.table;
      state.db.view = 'data';
      state.db.offset = 0;
      return renderTableDetail();
    case 'db-structure':
      state.db.table = el.dataset.table;
      state.db.view = 'columns';
      state.db.offset = 0;
      return renderTableDetail();
    case 'db-back':
      state.db.table = '';
      state.db.offset = 0;
      return renderDbTables();
    case 'db-tab':
      state.db.view = el.dataset.kind;
      state.db.offset = 0;
      return renderTableDetail();
    case 'db-prev':
      state.db.offset = Math.max(0, state.db.offset - state.db.limit);
      return renderTableDetail();
    case 'db-next':
      state.db.offset += state.db.limit;
      return renderTableDetail();
    case 'db-sql':
      return sqlConsole();
    case 'db-export':
      return exportDatabase();
    case 'db-import':
      return importDbModal();
    case 'db-optimize':
      return optimizeModal('');
    case 'db-optimize-table':
      return optimizeModal(state.db.table);
    case 'db-copy':
      return copyCreds();
    case 'db-files': {
      if (!state.db.site) return undefined;
      state.filePath = state.db.site.root;
      state.tab = 'files';
      document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('is-active', t.dataset.tab === 'files'));
      return renderFiles();
    }
    default:
      return undefined;
  }
});

/* 数据库下拉切换：换库后要重拉表列表 */
view().addEventListener('change', async (e) => {
  if (e.target.id === 'sel-all') {
    const boxes = [...view().querySelectorAll('.fsel')];
    if (e.target.checked) boxes.forEach((cb) => state.sel.add(cb.dataset.path));
    else state.sel.clear();
    return syncSelection();
  }
  if (e.target.classList.contains('fsel')) {
    if (e.target.checked) state.sel.add(e.target.dataset.path);
    else state.sel.delete(e.target.dataset.path);
    return syncSelection();
  }
  if (e.target.id !== 'db-pick') return;
  const d = state.db;
  d.name = e.target.value;
  d.loadedFor = '';
  d.tables = [];
  d.table = '';
  d.offset = 0;
  await renderDatabase();
});

async function copyCreds() {
  const text = state.db.credText;
  if (!text) return toast('没有可复制的连接信息', 'err');
  try {
    await navigator.clipboard.writeText(text);
    toast('连接信息已复制', 'ok');
  } catch {
    toast('复制失败，请手动选中文本', 'err');
  }
}

function bootstrapModal() {
  const body = `
    <div class="note" style="padding:9px 11px;background:var(--warn-soft);border-radius:6px;color:var(--warn);margin-bottom:14px">
      仅用于全新服务器。会安装 Web 服务器、PHP-FPM、你选择的数据库，并部署首个 WordPress 站点。默认组合约 5–10 分钟，自定义选装（换 PHP 版本）会多花几分钟。
    </div>
    <div class="field">
      <label>域名</label>
      <input type="text" id="b-domain" placeholder="example.com">
      <div class="note">需提前把域名解析到这台服务器</div>
    </div>
    <div class="field">
      <label>安装方式</label>
      <div class="type-picker" id="b-mode-picker">
        <label class="type-opt">
          <input type="radio" name="b-mode" value="mariadb" checked>
          <span class="type-opt-body">
            <b>MariaDB 11</b>
            <span>默认组合</span>
            <em>Nginx + 系统自带 PHP 8.4</em>
          </span>
        </label>
        <label class="type-opt">
          <input type="radio" name="b-mode" value="mysql84">
          <span class="type-opt-body">
            <b>MySQL 8.4 LTS</b>
            <span>默认组合</span>
            <em>Nginx + 系统自带 PHP 8.4</em>
          </span>
        </label>
        <label class="type-opt">
          <input type="radio" name="b-mode" value="custom">
          <span class="type-opt-body">
            <b>自定义选装</b>
            <span>自选 Web 服务器 / PHP / 数据库</span>
            <em>适合要对齐既有环境规范</em>
          </span>
        </label>
      </div>
      <div class="note" id="b-mode-hint"></div>
    </div>
    <div class="field is-hidden" id="b-custom">
      <div class="field-row">
        <div class="field">
          <label>Web 服务器</label>
          <select id="b-web">
            <option value="nginx">Nginx（推荐）</option>
            <option value="apache">Apache 2.4</option>
          </select>
        </div>
        <div class="field">
          <label>PHP 版本</label>
          <select id="b-php">
            <option value="">系统默认（8.4）</option>
            <option value="8.4">PHP 8.4</option>
            <option value="8.3">PHP 8.3</option>
            <option value="8.2">PHP 8.2</option>
            <option value="8.1">PHP 8.1</option>
            <option value="8.0">PHP 8.0</option>
          </select>
        </div>
      </div>
      <div class="field-row">
        <div class="field">
          <label>数据库</label>
          <select id="b-db2">
            <option value="mariadb">MariaDB 11</option>
            <option value="mysql84">MySQL 8.4 LTS</option>
            <option value="mysql57">MySQL 5.7（已停止维护）</option>
          </select>
        </div>
        <div class="field">
          <label>可选组件</label>
          <label class="check" style="margin-top:8px"><input type="checkbox" id="b-redis"> Redis 对象缓存</label>
        </div>
      </div>
      <div class="note" id="b-custom-hint"></div>
    </div>
    <div class="field">
      <label>站点标题</label>
      <input type="text" id="b-title" placeholder="我的网站">
    </div>
    <div class="field-row">
      <div class="field">
        <label>管理员账号</label>
        <input type="text" id="b-user" value="admin">
      </div>
      <div class="field">
        <label>管理员密码</label>
        <input type="text" id="b-pass" placeholder="留空自动生成">
      </div>
    </div>
    <div class="field">
      <label>管理员邮箱</label>
      <input type="text" id="b-email" placeholder="admin@example.com">
    </div>
    <label class="check"><input type="checkbox" id="b-ssl" checked> 自动申请 HTTPS 证书</label>
    <label class="check"><input type="checkbox" id="b-ufw" checked> 启用服务器防火墙（ufw）</label>`;

  openModal({
    title: '初始化服务器环境',
    subtitle: '运行 install-wordpress.sh，完成后会在服务器上写入部署信息文件',
    body,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="btn-do-bootstrap">开始初始化</button>`,
    wide: true,
  });

  const modeHint = {
    mariadb: 'MariaDB 与 Debian 源同步更新，无需额外配置；适合 2G 小机器，内存占用略低。',
    mysql84: '会添加 MySQL 官方 APT 源并卸载 MariaDB。若服务器上已有业务数据库，脚本会拒绝执行而不是删库。',
    custom: '按下面的组合安装。系统源里没有的 PHP 版本会自动添加第三方源（Debian 走 Sury，优先国内镜像），装得比默认组合慢一些。',
  };
  const customHint = () => {
    const web = $('#b-web').value;
    const db = $('#b-db2').value;
    const php = $('#b-php').value;
    const tips = [];
    if (php) tips.push(`PHP ${php} 由第三方源安装，站点与 WP-CLI 都用它。`);
    if (web === 'apache') tips.push('Apache 走 mpm_event + php-fpm，面板按 VirtualHost 维护站点，建站/换域名/删站/改类型都能用。');
    if (db === 'mysql57') tips.push('MySQL 5.7 已停止维护；低配机器其实 MariaDB 更省内存。');
    if (db === 'mysql84') tips.push('MySQL 8.4 需要能访问 repo.mysql.com。');
    $('#b-custom-hint').textContent = tips.join(' ');
  };
  const syncMode = () => {
    const v = document.querySelector('input[name="b-mode"]:checked')?.value || 'mariadb';
    $('#b-mode-hint').textContent = modeHint[v];
    $('#b-custom').classList.toggle('is-hidden', v !== 'custom');
    if (v === 'custom') customHint();
  };
  document.querySelectorAll('input[name="b-mode"]').forEach((el) => {
    el.addEventListener('change', syncMode);
  });
  ['#b-web', '#b-php', '#b-db2'].forEach((sel) => {
    $(sel).addEventListener('change', customHint);
  });
  syncMode();

  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#btn-do-bootstrap').addEventListener('click', async () => {
    const domain = $('#b-domain').value.trim();
    if (!domain) return toast('请填写域名', 'err');
    const mode = document.querySelector('input[name="b-mode"]:checked')?.value || 'mariadb';
    const custom = mode === 'custom';
    const payload = {
      domain,
      title: $('#b-title').value.trim() || domain,
      adminUser: $('#b-user').value.trim() || 'admin',
      adminPass: $('#b-pass').value.trim(),
      adminEmail: $('#b-email').value.trim(),
      ssl: $('#b-ssl').checked,
      ufw: $('#b-ufw').checked,
      webServer: custom ? $('#b-web').value : 'nginx',
      phpVersion: custom ? $('#b-php').value : '',
      dbEngine: custom ? $('#b-db2').value : mode,
      redis: custom ? $('#b-redis').checked : false,
    };
    closeModal();
    await runStream(
      `/api/servers/${state.currentId}/bootstrap`,
      payload,
      `初始化环境 ${domain}`,
      (evt) => {
        if (evt.ok) {
          toast('环境初始化完成', 'ok');
          setTimeout(loadSnapshot, 1500);
        }
      },
    );
  });
}

async function doctorModal() {
  openModal({
    title: '环境体检',
    subtitle: '检查新增 / 变更站点所需的前置条件，不会改动服务器',
    body: `<div class="note"><span class="spinner"></span> 正在检查服务器…</div>`,
    footer: `<button class="btn" data-close>关闭</button>`,
    wide: true,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);

  let data;
  try {
    data = await api(`/api/servers/${state.currentId}/doctor`);
  } catch (err) {
    $('#modal').querySelector('.modal-body').innerHTML = `<div class="note" style="color:var(--danger)">检查失败：${esc(err.message)}</div>`;
    return;
  }

  const rows = data.checks
    .map(
      (c) => `<div class="doc-row ${c.ok ? 'ok' : 'bad'}">
        <div class="doc-flag">${c.ok ? '✓' : '✗'}</div>
        <div class="doc-main">
          <div class="doc-label">${esc(c.label)}<span class="doc-value">${esc(c.value)}</span></div>
          ${c.hint ? `<div class="doc-hint">${esc(c.hint)}</div>` : ''}
        </div>
      </div>`,
    )
    .join('');

  const bad = data.checks.filter((c) => !c.ok);
  $('#modal').querySelector('.modal-body').innerHTML = `
    <div class="note" style="margin-bottom:12px">
      ${esc(data.host)} · ${esc(data.os)} · 登录账号 ${esc(data.user)}
      ${bad.length ? `· <span style="color:var(--danger)">${bad.length} 项需要处理</span>` : '· <span style="color:var(--ok)">全部通过</span>'}
    </div>
    <div class="doc-list">${rows}</div>
    ${data.logs?.length ? `<div class="note" style="margin-top:12px">最近的运维日志：${data.logs.map((l) => esc(l)).join('、')}</div>` : ''}`;
}

function addSiteModal() {
  const body = `
    <div class="field">
      <label>域名</label>
      <input type="text" id="s-domain" placeholder="blog.example.com">
      <div class="note">需要先把域名解析到这台服务器；未解析时会自动跳过 HTTPS</div>
    </div>
    <div class="field">
      <label>站点类型</label>
      ${typePickerHtml('wordpress')}
      <div class="note" data-type-hint></div>
    </div>
    <div class="field" data-for="wordpress">
      <label>站点标题</label>
      <input type="text" id="s-title" placeholder="我的博客">
    </div>
    <div class="field-row" data-for="wordpress">
      <div class="field">
        <label>管理员账号</label>
        <input type="text" id="s-user" value="admin">
      </div>
      <div class="field">
        <label>管理员密码</label>
        <input type="text" id="s-pass" placeholder="留空自动生成">
      </div>
    </div>
    <div class="field-row" data-for="wordpress">
      <div class="field">
        <label>管理员邮箱</label>
        <input type="text" id="s-email" placeholder="admin@example.com">
      </div>
      <div class="field">
        <label>站点语言</label>
        <select id="s-locale">
          <option value="zh_CN">简体中文</option>
          <option value="en_US">English</option>
        </select>
      </div>
    </div>
    <div class="field" data-for="wordpress custom" style="max-width:220px">
      <label>PHP worker 数</label>
      <input type="number" id="s-workers" value="4" min="1" max="12">
      <div class="note">ondemand 模式，空闲时不占内存</div>
    </div>
    <label class="check" data-for="wordpress custom"><input type="checkbox" id="s-pool"> 使用独立 PHP-FPM 池（多占约 25MB，隔离性更好）</label>
    <label class="check"><input type="checkbox" id="s-ssl" checked> 自动申请 HTTPS 证书</label>
    <div class="note" style="margin-top:10px">
      静态站不建数据库、不解析 PHP；自助网站会建库并启用 PHP，目录留空由你上传程序。
    </div>`;

  const modal = openModal({
    title: '新增站点',
    subtitle: `选择站点能力，面板会按类型建目录、配 ${wsName()}（WordPress 还会建库并安装）`,
    wide: true,
    body,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="btn-do-add">创建站点</button>`,
  });

  applySiteType(modal);
  modal.addEventListener('change', (e) => {
    if (e.target.name === 'site-type') applySiteType(modal);
  });
  modal.querySelector('[data-close]').addEventListener('click', closeModal);

  $('#btn-do-add').addEventListener('click', async () => {
    // 必须先取值再关弹窗：closeModal 会清空 #modal 的 innerHTML
    const type = applySiteType(modal);
    const domain = $('#s-domain').value.trim();
    if (!domain) return toast('请填写域名', 'err');
    const payload = {
      domain,
      type,
      ssl: $('#s-ssl').checked,
      title: $('#s-title').value.trim() || domain,
      adminUser: $('#s-user').value.trim() || 'admin',
      adminPass: $('#s-pass').value.trim(),
      adminEmail: $('#s-email').value.trim(),
      maxChildren: Number($('#s-workers').value) || 4,
      locale: $('#s-locale').value,
      separatePool: $('#s-pool').checked,
    };
    closeModal();
    await runStream(
      `/api/servers/${state.currentId}/sites`,
      payload,
      `新增站点 ${domain}（${siteTypeMeta(type).label}）`,
      (evt) => {
        if (evt.ok) {
          toast('站点创建完成', 'ok');
          setTimeout(loadSnapshot, 800);
        }
      },
    );
  });
}

function domainModal(site) {
  const body = `
    <div class="field">
      <label>原域名</label>
      <input type="text" id="d-old" value="${esc(site)}" readonly>
    </div>
    <div class="field">
      <label>新域名</label>
      <input type="text" id="d-new" placeholder="new.example.com">
      <div class="note">新域名需提前解析到本机，否则证书申请会跳过</div>
    </div>
    <label class="check"><input type="checkbox" id="d-redirect" checked> 旧域名 301 跳转到新域名</label>
    <label class="check"><input type="checkbox" id="d-move" checked> 网站目录一并改名（/var/www/旧 → /var/www/新）</label>
    <label class="check"><input type="checkbox" id="d-ssl" checked> 为新域名申请证书</label>
    <label class="check"><input type="checkbox" id="d-dry"> 先演练一遍（只检查不修改，用于排查问题）</label>
    <div class="note" style="margin-top:10px;padding:8px 10px;background:var(--warn-soft);border-radius:6px;color:var(--warn)">
      会执行数据库全量搜索替换，操作前自动导出备份。建议先确认站点无正在进行的导入任务。
    </div>`;

  openModal({
    title: '变更域名',
    subtitle: `当前站点：${site}`,
    body,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="btn-do-domain">开始变更</button>`,
  });

  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#btn-do-domain').addEventListener('click', async () => {
    const newDomain = $('#d-new').value.trim();
    if (!newDomain) return toast('请填写新域名', 'err');
    const payload = {
      oldDomain: site,
      newDomain,
      keepRedirect: $('#d-redirect').checked,
      moveDir: $('#d-move').checked,
      ssl: $('#d-ssl').checked,
      dryRun: $('#d-dry').checked,
    };
    closeModal();
    await runStream(
      `/api/servers/${state.currentId}/domain`,
      payload,
      `变更域名 ${site} → ${newDomain}${payload.dryRun ? '（演练）' : ''}`,
      (evt) => {
        if (evt.ok) {
          toast(payload.dryRun ? '演练完成，未改动服务器' : '域名变更完成', 'ok');
          if (!payload.dryRun) setTimeout(loadSnapshot, 800);
        }
      },
    );
  });
}

function backupModal(site) {
  const body = `
    <p style="margin:0 0 12px">将打包 <strong>${esc(site)}</strong> 的数据库与站点文件，并下载到本机。</p>
    <label class="check"><input type="checkbox" id="b-db" checked> 包含数据库</label>
    <label class="check"><input type="checkbox" id="b-files" checked> 包含站点文件（排除缓存目录）</label>
    <div class="note">保存位置：panel/data/backups/&lt;服务器ID&gt;/</div>`;

  openModal({
    title: '备份站点',
    body,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="btn-do-backup">开始备份</button>`,
  });

  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#btn-do-backup').addEventListener('click', async () => {
    const payload = {
      domain: site,
      includeDb: $('#b-db').checked,
      includeFiles: $('#b-files').checked,
    };
    closeModal();
    await runStream(
      `/api/servers/${state.currentId}/backup`,
      payload,
      `备份 ${site}`,
      (evt) => {
        if (evt.ok) {
          toast(`备份完成`, 'ok');
          if (state.tab === 'backups') renderBackups();
        }
      },
    );
  });
}

function issueCertModal(site) {
  const body = `
    <p style="margin:0 0 12px">为 <strong>${esc(site)}</strong> 申请 Let's Encrypt 证书并开启强制 HTTPS。</p>
    <div class="field">
      <label>通知邮箱</label>
      <input type="text" id="c-email" placeholder="admin@${esc(site)}">
    </div>
    <div class="note">证书 90 天有效，certbot 会自动续期。域名未解析或 80 端口不通时会失败。</div>`;

  openModal({
    title: '申请 HTTPS 证书',
    body,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="btn-do-cert">申请</button>`,
  });

  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#btn-do-cert').addEventListener('click', async () => {
    const email = $('#c-email').value.trim();
    closeModal();
    await runStream(
      `/api/servers/${state.currentId}/ssl/issue`,
      { domain: site, email },
      `申请证书 ${site}`,
      (evt) => {
        if (evt.ok) {
          toast('证书已签发', 'ok');
          setTimeout(loadSnapshot, 1200);
        }
      },
    );
  });
}

async function renewOne(site) {
  await runStream(`/api/servers/${state.currentId}/ssl/renew`, {}, `续期证书（${site}）`, (evt) => {
    if (evt.ok) setTimeout(loadSnapshot, 1200);
  });
}

async function renewAll() {
  await runStream(`/api/servers/${state.currentId}/ssl/renew`, {}, '续期全部证书', (evt) => {
    if (evt.ok) setTimeout(loadSnapshot, 1200);
  });
}

async function editSiteModal(site) {
  const snap = (state.snapshot?.sites || []).find((x) => x.primaryDomain === site) || {};
  const current = snap.type || (snap.isWordPress ? 'wordpress' : 'custom');
  const meta = siteTypeMeta(current);

  const infoRow = (label, value, id = '') =>
    `<div><span>${esc(label)}</span><b class="mono"${id ? ` id="${id}"` : ''}>${value}</b></div>`;

  const body = `
    <div class="db-kv">
      ${infoRow('类型', esc(meta.label), 'e-type')}
      ${infoRow('目录', esc(snap.root || `/var/www/${site}`))}
      ${infoRow('数据库', esc(snap.dbName || '—'), 'e-db')}
      ${infoRow('HTTPS', snap.ssl ? '已启用' : '未启用')}
      ${infoRow('体积', '<span class="muted">读取中…</span>', 'e-size')}
      ${infoRow('文件数', '<span class="muted">读取中…</span>', 'e-files')}
      ${infoRow('WordPress', snap.isWordPress ? '<span class="muted">读取版本…</span>' : '否', 'e-wp')}
    </div>
    <div class="section-title" style="margin-top:20px">站点能力</div>
    ${typePickerHtml(current)}
    <div class="note" data-type-warning style="margin-top:10px"></div>
    <div data-for="wordpress" data-when="no-wp" style="margin-top:12px">
      <div class="field-row">
        <div class="field"><label>后台账号</label><input id="e-user" value="admin"></div>
        <div class="field"><label>后台密码</label><input id="e-pass" placeholder="留空自动生成"></div>
      </div>
      <div class="field-row">
        <div class="field"><label>管理员邮箱</label><input id="e-email" placeholder="admin@example.com"></div>
        <div class="field"><label>站点标题</label><input id="e-title" placeholder="${esc(site)}"></div>
      </div>
      <div class="note">目录里还没有 WordPress，保存后会自动下载并安装</div>
    </div>
    <div data-for="wordpress custom" style="margin-top:12px">
      <div class="field" style="max-width:220px">
        <label>PHP worker 数</label>
        <input type="number" id="e-workers" value="4" min="1" max="12">
      </div>
      <label class="check"><input type="checkbox" id="e-pool"> 使用独立 PHP-FPM 池（多占约 25MB）</label>
    </div>
    <label class="check" style="margin-top:14px"><input type="checkbox" id="e-dry"> 先演练（只打印计划，不改动服务器）</label>
    <div class="note" style="margin-top:12px;padding:8px 10px;background:var(--warn-soft);border-radius:6px;color:var(--warn)">
      转换不会删除目录内容，也不会删数据库；${wsName()} 配置会先备份到 <span class="mono">/root/</span>，失败自动还原。
    </div>`;

  const modal = openModal({
    title: `编辑站点 · ${site}`,
    subtitle: `当前类型：${meta.label}`,
    wide: true,
    body,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="btn-save-type">保存并转换</button>`,
  });

  let hasWp = snap.isWordPress === true;
  modal.dataset.currentType = current;
  modal.dataset.hasWp = hasWp ? '1' : '0';
  applySiteType(modal);

  modal.addEventListener('change', (e) => {
    if (e.target.name === 'site-type') applySiteType(modal);
  });
  modal.querySelector('[data-close]').addEventListener('click', closeModal);

  // 详情异步回填：先让用户能操作，读取慢也不挡路
  api(`/api/servers/${state.currentId}/site-detail?domain=${encodeURIComponent(site)}`)
    .then((d) => {
      if (!document.body.contains(modal)) return; // 弹窗已关闭
      const $id = (x) => modal.querySelector(`#${x}`);
      if (d.TYPE && ['wordpress', 'custom', 'static'].includes(d.TYPE) && d.TYPE !== modal.dataset.currentType) {
        modal.dataset.currentType = d.TYPE;
        const radio = modal.querySelector(`input[name="site-type"][value="${d.TYPE}"]`);
        if (radio) radio.checked = true;
        modal.querySelector('.modal-head p').textContent = `当前类型：${siteTypeMeta(d.TYPE).label}`;
        if ($id('e-type')) $id('e-type').textContent = siteTypeMeta(d.TYPE).label;
      }
      hasWp = d.WP === '1';
      modal.dataset.hasWp = hasWp ? '1' : '0';
      applySiteType(modal);
      if ($id('e-size')) $id('e-size').textContent = d.SIZE || '—';
      if ($id('e-files')) $id('e-files').textContent = d.FILES || '—';
      if ($id('e-db')) $id('e-db').textContent = snap.dbName || d.DB || '—';
      if ($id('e-wp')) $id('e-wp').textContent = hasWp ? `是（${d.WPVERSION || '版本未知'}）` : '否';
    })
    .catch((err) => {
      const size = modal.querySelector('#e-size');
      if (size) size.innerHTML = `<span style="color:var(--danger)">${esc(err.message)}</span>`;
    });

  $('#btn-save-type').addEventListener('click', async () => {
    // 先取值再关弹窗
    const type = applySiteType(modal);
    const from = modal.dataset.currentType || current;
    const dryRun = $('#e-dry').checked;
    const payload = {
      domain: site,
      type,
      dryRun,
      force: type === from,
      maxChildren: Number($('#e-workers').value) || 4,
      separatePool: $('#e-pool').checked,
      adminUser: $('#e-user').value.trim() || 'admin',
      adminPass: $('#e-pass').value.trim(),
      adminEmail: $('#e-email').value.trim(),
      title: $('#e-title').value.trim() || site,
    };
    closeModal();
    await runStream(
      `/api/servers/${state.currentId}/site-type`,
      payload,
      `${dryRun ? '演练：' : ''}切换 ${site} → ${siteTypeMeta(type).label}`,
      (evt) => {
        if (evt.ok) {
          toast(dryRun ? '演练完成，未改动服务器' : `站点已切换为${siteTypeMeta(type).label}`, 'ok');
          if (!dryRun) setTimeout(loadSnapshot, 900);
        }
      },
    );
  });
}

/* ---------------------------------------------------------------- 文件管理 */

async function renderFiles() {
  const s = currentServer();
  if (!s) return;
  view().innerHTML = `<div class="muted"><span class="spinner"></span> 读取目录…</div>`;
  try {
    const data = await api(`/api/files?server=${s.id}&path=${encodeURIComponent(state.filePath)}`);
    state.files = data;
    const parts = data.path.split('/').filter(Boolean);
    let acc = '';
    const crumbs = parts
      .map((p) => {
        acc += `/${p}`;
        const isLast = acc === data.path;
        return isLast
          ? `<span class="crumb-current">${esc(p)}</span>`
          : `<span class="crumb" data-path="${esc(acc)}">${esc(p)}</span><span class="crumb-sep">/</span>`;
      })
      .join('');

    view().innerHTML = `
      <div class="crumbs">
        <span class="crumb" data-path="/">/</span><span class="crumb-sep"></span>${crumbs}
      </div>
      <div class="card-head" style="margin-bottom:12px">
        <div class="muted" style="font-size:11.5px">${data.items.length} 项</div>
        <div style="display:flex;gap:6px">
          ${data.parent ? `<button class="btn btn-sm" data-act="up" data-path="${esc(data.parent)}">上级</button>` : ''}
          <button class="btn btn-sm" data-act="mkdir">新建文件夹</button>
          <button class="btn btn-sm" data-act="upload">上传</button>
          <button class="btn btn-sm" data-act="reload-files">刷新</button>
        </div>
      </div>
      <div class="selbar" id="selbar" hidden>
        <div class="selbar-info">已选 <b id="sel-count">0</b> 项</div>
        <div class="selbar-acts">
          <button class="btn btn-sm" data-act="sel-perm">改权限</button>
          <button class="btn btn-sm" data-act="sel-copy">复制到…</button>
          <button class="btn btn-sm" data-act="sel-move">移动到…</button>
          <button class="btn btn-sm btn-danger" data-act="sel-delete">删除</button>
          <button class="btn btn-sm btn-ghost" data-act="sel-clear">取消选择</button>
        </div>
      </div>
      <table class="file-table">
        <thead><tr>
          <th class="fcheck"><input type="checkbox" id="sel-all" title="全选 / 取消全选"></th>
          <th>名称</th><th style="width:80px">大小</th><th style="width:114px">权限</th><th style="width:120px">修改时间</th><th style="width:200px"></th>
        </tr></thead>
        <tbody>
          ${data.items
            .map(
              (f) => `<tr data-row="${esc(fullPath(data.path, f.name))}">
                <td class="fcheck"><input type="checkbox" class="fsel" data-path="${esc(fullPath(data.path, f.name))}"></td>
                <td><span class="fname" data-path="${esc(data.path === '/' ? `/${f.name}` : `${data.path}/${f.name}`)}" data-type="${f.type}">
                  <i class="ficon ${f.type === 'dir' ? 'dir' : ''}"></i>${esc(f.name)}
                </span></td>
                <td class="fsize">${f.type === 'dir' ? '—' : fmtBytes(f.size)}</td>
                <td><span class="fperm" data-act="perm" data-path="${esc(fullPath(data.path, f.name))}" data-mode="${f.mode || 0}" data-type="${f.type}" title="点击修改权限">${permText(f.mode)}</span></td>
                <td class="fmtime">${fmtTime(f.mtime)}</td>
                <td><div class="fops">
                  ${
                    f.type === 'dir'
                      ? `<button class="btn btn-xs btn-ghost" data-act="enter" data-path="${esc(fullPath(data.path, f.name))}">打开</button>`
                      : `<button class="btn btn-xs btn-ghost" data-act="edit" data-path="${esc(fullPath(data.path, f.name))}">编辑</button>
                         <button class="btn btn-xs btn-ghost" data-act="download" data-path="${esc(fullPath(data.path, f.name))}">下载</button>`
                  }
                  <button class="btn btn-xs btn-ghost" data-act="rename" data-path="${esc(fullPath(data.path, f.name))}">重命名</button>
                  <button class="btn btn-xs btn-ghost" data-act="delete" data-path="${esc(fullPath(data.path, f.name))}">删除</button>
                </div></td>
              </tr>`,
            )
            .join('')}
        </tbody>
      </table>`;
    // 换目录时清空选择；同目录刷新（操作后）保留勾选但按现有行过滤
    if (state.selPath !== data.path) {
      state.selPath = data.path;
      state.sel.clear();
    } else {
      const alive = new Set(data.items.map((f) => fullPath(data.path, f.name)));
      [...state.sel].forEach((x) => {
        if (!alive.has(x)) state.sel.delete(x);
      });
    }
    syncSelection();
  } catch (err) {
    view().innerHTML = `<div class="card">
      <div style="color:var(--danger)">${esc(err.message)}</div>
      <div style="margin-top:12px"><button class="btn btn-sm" data-act="reload-files">重试</button></div>
    </div>`;
  }
}

function fullPath(dir, name) {
  return dir === '/' ? `/${name}` : `${dir}/${name}`;
}

/* ---------------------------------------------------------------- 文件选择 */

// 勾选状态存在 state.sel（绝对路径），渲染后统一回填到 DOM
function syncSelection() {
  const boxes = [...view().querySelectorAll('.fsel')];
  boxes.forEach((cb) => {
    const on = state.sel.has(cb.dataset.path);
    cb.checked = on;
    const tr = cb.closest('tr');
    if (tr) tr.classList.toggle('is-sel', on);
  });
  const n = state.sel.size;
  const bar = $('#selbar');
  if (bar) bar.hidden = n === 0;
  const cnt = $('#sel-count');
  if (cnt) cnt.textContent = String(n);
  const all = $('#sel-all');
  if (all) {
    all.disabled = boxes.length === 0;
    all.checked = boxes.length > 0 && n === boxes.length;
    all.indeterminate = n > 0 && n < boxes.length;
  }
}

function selectedPaths() {
  return [...state.sel];
}

// 选中项对应的文件条目（拿 type / mode）
function selectedItems() {
  const data = state.files;
  if (!data || !Array.isArray(data.items)) return [];
  return data.items.filter((f) => state.sel.has(fullPath(data.path, f.name)));
}

function selPermModal() {
  const paths = selectedPaths();
  if (!paths.length) return toast('先勾选文件或目录', 'err');
  const items = selectedItems();
  const isDir = items.some((f) => f.type === 'dir');
  const mode = items.length === 1 ? items[0].mode : 0o755;
  permModal(paths, mode, isDir);
}

function selDeleteFlow() {
  const paths = selectedPaths();
  if (!paths.length) return toast('先勾选文件或目录', 'err');
  const items = selectedItems();
  const dirs = items.filter((f) => f.type === 'dir').length;

  openModal({
    title: '删除确认',
    subtitle: `${paths.length} 项`,
    body: `
      <div style="padding:11px 13px;border-radius:8px;background:var(--danger-soft);color:var(--danger);font-size:12.5px;line-height:1.6;text-align:left">
        <strong>不可恢复。</strong>${dirs > 0 ? `${dirs} 个目录会连内部所有文件一起删除。` : '这些文件会被直接删除。'}
      </div>
      <div class="field" style="margin-top:16px">
        <label>将删除</label>
        <div class="mono t-list">${paths.slice(0, 12).map(esc).join('<br>')}${
          paths.length > 12 ? `<br>… 还有 ${paths.length - 12} 项` : ''
        }</div>
      </div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-danger" id="btn-del-sel">永久删除</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#btn-del-sel').addEventListener('click', async () => {
    const btn = $('#btn-del-sel');
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner"></span> 删除中';
    try {
      const r = await api('/api/file/batch', {
        method: 'POST',
        body: JSON.stringify({ server: state.currentId, action: 'delete', paths }),
      });
      closeModal();
      toast(`已删除 ${r.count} 项`, 'ok');
      state.sel.clear();
      renderFiles();
    } catch (err) {
      toast(err.message, 'err');
      btn.disabled = false;
      btn.textContent = '永久删除';
    }
  });
}

function transferFlow(kind) {
  const paths = selectedPaths();
  if (!paths.length) return toast('先勾选文件或目录', 'err');
  const isMove = kind === 'move';
  const dir = state.filePath || '/';

  openModal({
    title: isMove ? '移动到…' : '复制到…',
    subtitle: `${paths.length} 项`,
    body: `
      <div class="field">
        <label>目标目录</label>
        <input type="text" id="t-dest" value="${esc(dir)}" spellcheck="false" autocomplete="off">
        <div class="note">服务器上的绝对路径，目录必须已存在。${
          isMove
            ? '填当前目录等于原地不动，请改成别的目录（如 /var/www/backup）。'
            : '填当前目录会在名字后面加 -copy 生成副本，可以直接当备份。'
        }</div>
      </div>
      <div class="field">
        <label>将${isMove ? '移动' : '复制'}以下 ${paths.length} 项</label>
        <div class="mono t-list">${paths.slice(0, 10).map(esc).join('<br>')}${
          paths.length > 10 ? `<br>… 还有 ${paths.length - 10} 项` : ''
        }</div>
      </div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="t-go">${isMove ? '开始移动' : '开始复制'}</button>`,
  });

  const input = $('#t-dest');
  input.focus();
  input.select();
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#t-go').addEventListener('click', async () => {
    const dest = input.value.trim();
    if (!dest.startsWith('/')) return toast('目标目录必须是绝对路径', 'err');
    const btn = $('#t-go');
    btn.disabled = true;
    btn.innerHTML = `<span class="spinner"></span> ${isMove ? '移动中' : '复制中'}`;
    try {
      const r = await api('/api/file/batch', {
        method: 'POST',
        body: JSON.stringify({ server: state.currentId, action: kind, paths, dest }),
      });
      closeModal();
      toast(`已${isMove ? '移动' : '复制'} ${r.count} 项到 ${dest}`, 'ok');
      state.sel.clear();
      renderFiles();
    } catch (err) {
      toast(err.message, 'err');
      btn.disabled = false;
      btn.textContent = isMove ? '开始移动' : '开始复制';
    }
  });
}

async function openEditor(path) {
  openModal({
    title: '编辑文件',
    subtitle: path,
    wide: true,
    body: `<div class="muted"><span class="spinner"></span> 读取中…</div>`,
    footer: '<button class="btn" data-close>关闭</button>',
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  try {
    const data = await api(`/api/file?server=${state.currentId}&path=${encodeURIComponent(path)}`);
    $('#modal .modal-body').innerHTML = `<textarea class="code-editor" id="editor" spellcheck="false"></textarea>`;
    $('#editor').value = data.content;
    $('#modal .modal-foot').innerHTML = `
      <span class="muted" style="margin-right:auto;font-size:11.5px">${fmtBytes(data.size)} · ${fmtTime(data.mtime)}</span>
      <button class="btn" data-close>取消</button>
      <button class="btn btn-primary" id="btn-save-file">保存</button>`;
    $('#modal').querySelectorAll('[data-close]').forEach((b) => b.addEventListener('click', closeModal));
    $('#btn-save-file').addEventListener('click', async () => {
      const btn = $('#btn-save-file');
      btn.disabled = true;
      btn.innerHTML = '<span class="spinner"></span> 保存中';
      try {
        await api('/api/file', {
          method: 'PUT',
          body: JSON.stringify({ server: state.currentId, path, content: $('#editor').value }),
        });
        toast('已保存', 'ok');
        closeModal();
        renderFiles();
      } catch (err) {
        toast(err.message, 'err');
        btn.disabled = false;
        btn.textContent = '保存';
      }
    });
  } catch (err) {
    $('#modal .modal-body').innerHTML = `<div style="color:var(--danger)">${esc(err.message)}</div>`;
    $('#modal .modal-foot').innerHTML = '<button class="btn" data-close>关闭</button>';
    $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  }
}

function mkdirFlow() {
  openModal({
    title: '新建文件夹',
    body: `<div class="field"><label>文件夹名称</label><input type="text" id="m-name" placeholder="new-folder"></div>`,
    footer: `<button class="btn" data-close>取消</button><button class="btn btn-primary" id="btn-mkdir">创建</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#btn-mkdir').addEventListener('click', async () => {
    const name = $('#m-name').value.trim();
    if (!name) return toast('请填写名称', 'err');
    try {
      await api('/api/file/mkdir', {
        method: 'POST',
        body: JSON.stringify({ server: state.currentId, path: fullPath(state.filePath, name) }),
      });
      closeModal();
      renderFiles();
    } catch (err) {
      toast(err.message, 'err');
    }
  });
}

function uploadFlow() {
  const input = document.createElement('input');
  input.type = 'file';
  input.multiple = true;
  input.addEventListener('change', async () => {
    const files = [...input.files];
    if (!files.length) return;
    for (const file of files) {
      try {
        await new Promise((resolve, reject) => {
          const xhr = new XMLHttpRequest();
          xhr.open(
            'POST',
            `/api/file/upload?server=${state.currentId}&dir=${encodeURIComponent(state.filePath)}&name=${encodeURIComponent(file.name)}`,
          );
          xhr.setRequestHeader('x-panel', '1');
          xhr.onload = () =>
            xhr.status >= 200 && xhr.status < 300 ? resolve() : reject(new Error(`上传失败（${xhr.status}）`));
          xhr.onerror = () => reject(new Error('上传失败，网络异常'));
          xhr.send(file);
        });
        toast(`${file.name} 已上传`, 'ok');
      } catch (err) {
        toast(err.message, 'err');
      }
    }
    renderFiles();
  });
  input.click();
}

function renameFlow(path) {
  const name = path.split('/').pop();
  openModal({
    title: '重命名',
    body: `<div class="field"><label>新名称</label><input type="text" id="r-name" value="${esc(name)}"></div>`,
    footer: `<button class="btn" data-close>取消</button><button class="btn btn-primary" id="btn-rename">确定</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#btn-rename').addEventListener('click', async () => {
    const newName = $('#r-name').value.trim();
    if (!newName || newName.includes('/')) return toast('名称不合法', 'err');
    try {
      await api('/api/file/rename', {
        method: 'POST',
        body: JSON.stringify({ server: state.currentId, from: path, to: fullPath(path.split('/').slice(0, -1).join('/') || '/', newName) }),
      });
      closeModal();
      renderFiles();
    } catch (err) {
      toast(err.message, 'err');
    }
  });
}

function deleteFlow(path) {
  openModal({
    title: '删除确认',
    body: `<p style="margin:0 0 10px">即将删除：</p>
           <div class="mono" style="padding:8px 10px;background:var(--danger-soft);border-radius:6px;color:var(--danger);word-break:break-all">${esc(path)}</div>
           <p style="margin:12px 0 0;color:var(--danger);font-size:12px">不可恢复，目录会连同内部所有文件一起删除。</p>`,
    footer: `<button class="btn" data-close>取消</button><button class="btn btn-danger" id="btn-delete">确认删除</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#btn-delete').addEventListener('click', async () => {
    try {
      await api('/api/file/delete', {
        method: 'POST',
        body: JSON.stringify({ server: state.currentId, path }),
      });
      closeModal();
      toast('已删除', 'ok');
      renderFiles();
    } catch (err) {
      toast(err.message, 'err');
    }
  });
}

/* ---------------------------------------------------------------- 文件权限 */

function permText(mode) {
  if (mode == null) return '—';
  const m = Number(mode);
  const chars = [];
  for (let i = 8; i >= 0; i -= 1) chars.push((m >> i) & 1 ? ['r', 'w', 'x'][(8 - i) % 3] : '-');
  // 与 ls 一致：setuid/setgid/sticky 覆盖对应组的执行位
  if (m & 0o4000) chars[2] = (m >> 6) & 1 ? 's' : 'S';
  if (m & 0o2000) chars[5] = (m >> 3) & 1 ? 's' : 'S';
  if (m & 0o1000) chars[8] = (m >> 0) & 1 ? 't' : 'T';
  return chars.join('');
}

function permModal(targets, mode, isDir) {
  const list = Array.isArray(targets) ? targets : [targets];
  const multi = list.length > 1;
  const base = Number(mode || 0) & 0o7777;
  let special = base & 0o7000;
  const rows = [
    { label: '属主 u', shift: 6 },
    { label: '属组 g', shift: 3 },
    { label: '其他 o', shift: 0 },
  ];
  const oct = (n) => ((n & 0o7777) > 0o777 ? (n & 0o7777).toString(8).padStart(4, '0') : (n & 0o777).toString(8).padStart(3, '0'));

  openModal({
    title: multi ? '批量改权限' : '权限',
    subtitle: multi ? `${list.length} 项` : list[0],
    body: `
      ${
        multi
          ? `<div class="field">
        <label>将统一改为</label>
        <div class="mono t-list">${list.slice(0, 8).map(esc).join('<br>')}${
          list.length > 8 ? `<br>… 还有 ${list.length - 8} 项` : ''
        }</div>
      </div>`
          : ''
      }
      <div class="field">
        <label>八进制</label>
        <div style="display:flex;align-items:center;gap:12px">
          <input class="perm-oct" id="p-oct" value="${oct(base)}" maxlength="4" inputmode="numeric" style="width:92px">
          <span class="perm-preview" id="p-view">${permText(base)}</span>
        </div>
        <div class="note">文件常用 644，目录常用 755，需要组内可写用 775；四位可写 4755 / 2755 / 1777（setuid / setgid / sticky）</div>
      </div>
      <div class="perm-grid" style="margin-top:16px">
        <div class="perm-head"></div>
        <div class="perm-head">读 r</div>
        <div class="perm-head">写 w</div>
        <div class="perm-head">执行 x</div>
        ${rows
          .map(
            (r) => `<div class="perm-row-label">${r.label}</div>
              ${[2, 1, 0]
                .map(
                  (b) => `<label class="check" style="justify-content:flex-start">
                    <input type="checkbox" data-bit="${r.shift + b}" ${(base >> (r.shift + b)) & 1 ? 'checked' : ''}>
                  </label>`,
                )
                .join('')}`,
          )
          .join('')}
      </div>
      ${
        isDir
          ? `<label class="check" style="margin-top:16px"><input type="checkbox" id="p-rec"> 同时应用到该目录下的所有内容（chmod -R）</label>`
          : ''
      }
      <div class="note" style="margin-top:12px">${wsName()} 需要对其读取的目录至少有 o+rx；WordPress 目录一般 755、文件 644。</div>`,
    footer: `<button class="btn" data-close>取消</button><button class="btn btn-primary" id="btn-perm">应用</button>`,
  });

  const octInput = $('#p-oct');
  const boxes = [...$('#modal').querySelectorAll('input[data-bit]')];

  const fromBoxes = () => {
    let v = special;
    boxes.forEach((b) => {
      if (b.checked) v |= 1 << Number(b.dataset.bit);
    });
    return v;
  };

  octInput.addEventListener('input', () => {
    const v = parseInt(octInput.value.replace(/[^0-7]/g, ''), 8);
    if (Number.isNaN(v)) return;
    special = v & 0o7000;
    boxes.forEach((b) => {
      b.checked = Boolean((v >> Number(b.dataset.bit)) & 1);
    });
    $('#p-view').textContent = permText(v);
  });

  boxes.forEach((b) =>
    b.addEventListener('change', () => {
      const v = fromBoxes();
      octInput.value = oct(v);
      $('#p-view').textContent = permText(v);
    }),
  );

  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#btn-perm').addEventListener('click', async () => {
    const btn = $('#btn-perm');
    const value = octInput.value.replace(/[^0-7]/g, '');
    if (!value) return toast('权限只能是 0-7 组成的八进制数', 'err');
    const recursive = Boolean(isDir && $('#p-rec') && $('#p-rec').checked);
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner"></span> 应用中';
    try {
      await api('/api/file/batch', {
        method: 'POST',
        body: JSON.stringify({ server: state.currentId, action: 'chmod', paths: list, mode: value, recursive }),
      });
      closeModal();
      toast(`${multi ? `${list.length} 项权限` : '权限'}已改为 ${value}${recursive ? '（含子项）' : ''}`, 'ok');
      state.sel.clear();
      renderFiles();
    } catch (err) {
      toast(err.message, 'err');
      btn.disabled = false;
      btn.textContent = '应用';
    }
  });
}

/* ---------------------------------------------------------------- 删除整站 */

function deleteSiteFlow(domain) {
  const s = (state.snapshot?.sites || []).find((x) => x.primaryDomain === domain);
  const root = s?.root || `/var/www/${domain}`;
  const dbName = s?.dbName || '';

  openModal({
    title: '删除整站',
    subtitle: domain,
    body: `
      <div style="padding:11px 13px;border-radius:8px;background:var(--danger-soft);color:var(--danger);font-size:12.5px;line-height:1.6">
        <strong>不可撤销。</strong>${wsName()} 配置、站点目录、数据库会从服务器上移除，站点立即无法访问。
      </div>
      <div class="field" style="margin-top:16px">
        <label>将会删除</label>
        <div class="mono" style="font-size:11.5px;line-height:1.85;color:var(--text-2);word-break:break-all">
          ${siteConfPaths(domain).map((p) => `${esc(p)}<br>`).join('')}
          <span id="d-root-line">${esc(root)}</span><br>
          <span id="d-db-line">${dbName ? `数据库 ${esc(dbName)}` : '数据库（无）'}</span>
        </div>
      </div>
      <div class="field" style="margin-top:14px">
        <label>删除范围</label>
        <label class="check"><input type="checkbox" id="d-files" checked> 站点目录（含上传文件与插件）</label>
        ${dbName ? `<label class="check"><input type="checkbox" id="d-db" checked> 数据库 ${esc(dbName)}</label>` : ''}
        <label class="check"><input type="checkbox" id="d-cert" checked> Let's Encrypt 证书</label>
      </div>
      <div class="field" style="margin-top:14px">
        <label>输入域名确认</label>
        <input type="text" id="d-confirm" placeholder="${esc(domain)}" autocomplete="off" spellcheck="false">
        <div class="note">必须与域名完全一致，右侧按钮才会启用</div>
      </div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-danger" id="btn-del-site" disabled>永久删除</button>`,
  });

  const confirmInput = $('#d-confirm');
  const delBtn = $('#btn-del-site');
  confirmInput.addEventListener('input', () => {
    delBtn.disabled = confirmInput.value.trim() !== domain;
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);

  delBtn.addEventListener('click', () => {
    if (confirmInput.value.trim() !== domain) return toast('域名不一致，已取消', 'err');
    const filesBox = $('#d-files');
    const dbBox = $('#d-db');
    const certBox = $('#d-cert');
    const body = {
      domain,
      dbName,
      deleteFiles: filesBox ? filesBox.checked : true,
      deleteDb: dbName ? Boolean(dbBox && dbBox.checked) : false,
      deleteCert: certBox ? certBox.checked : true,
    };
    closeModal();
    runStream(`/api/servers/${state.currentId}/site-delete`, body, `删除整站 ${domain}`, (evt) => {
      if (evt.ok) loadSnapshot();
    });
  });
}

/* ---------------------------------------------------------------- 数据库 */

async function renderDatabase() {
  const s = currentServer();
  if (!s) return;
  const d = state.db;

  if (!d.loaded) {
    view().innerHTML = `<div class="empty"><span class="spinner"></span> 正在读取数据库列表…</div>`;
    try {
      const data = await api(`/api/servers/${s.id}/db/databases`);
      d.databases = data.databases || [];
      d.loaded = true;
      if (!d.name || !d.databases.some((x) => x.name === d.name)) d.name = d.databases[0]?.name || '';
      d.loadedFor = '';
      d.tables = [];
    } catch (err) {
      view().innerHTML = `<div class="card">
        <div style="color:var(--danger)">${esc(err.message)}</div>
        <div class="note" style="margin-top:8px">数据库管理需要服务器已安装 MariaDB，且 root 可免密连接（install-wordpress.sh 装好的环境默认满足）</div>
        <div style="margin-top:12px"><button class="btn btn-sm" data-act="db-reload">重试</button></div>
      </div>`;
      return;
    }
  }

  if (!d.databases.length) {
    view().innerHTML = `<div class="empty">
      <strong>没有业务数据库</strong>
      面板只显示非系统库。装一个 WordPress 站点后，它的库会出现在这里
      <div style="margin-top:14px"><button class="btn btn-sm" data-act="db-reload">刷新</button></div>
    </div>`;
    return;
  }

  if (d.loadedFor !== d.name) {
    const name = d.name;
    d.table = '';
    d.offset = 0;
    view().innerHTML = `<div class="empty"><span class="spinner"></span> 正在读取 ${esc(name)} 的表…</div>`;
    try {
      const data = await api(`/api/servers/${s.id}/db/tables?db=${encodeURIComponent(name)}`);
      d.tables = data.tables || [];
      d.site = data.site || null;
      d.loadedFor = name;
      syncCredText();
    } catch (err) {
      view().innerHTML = `<div class="card">
        <div style="color:var(--danger)">${esc(err.message)}</div>
        <div style="margin-top:12px"><button class="btn btn-sm" data-act="db-reload">重试</button></div>
      </div>`;
      return;
    }
  }

  if (d.table) return renderTableDetail();
  return renderDbTables();
}

function syncCredText() {
  const site = state.db.site;
  state.db.credText = site
    ? [
        `数据库：${state.db.name}`,
        `用户名：${site.user}`,
        `密码：${site.password}`,
        `主机：${site.host}`,
        `表前缀：${site.prefix}`,
        `站点目录：${site.root}`,
      ].join('\n')
    : '';
}

function renderDbTables() {
  const d = state.db;
  const info = d.databases.find((x) => x.name === d.name) || {};
  const options = d.databases
    .map((x) => `<option value="${esc(x.name)}" ${x.name === d.name ? 'selected' : ''}>${esc(x.name)}（${x.tables} 张表 · ${x.sizeText}）</option>`)
    .join('');

  const rows = d.tables.length
    ? d.tables
        .map(
          (t) => `<tr>
            <td><span class="fname" data-act="db-table" data-table="${esc(t.name)}"><i class="ficon dir"></i>${esc(t.name)}</span></td>
            <td class="fsize">${esc(t.engine || '—')}</td>
            <td class="fsize">${t.rows.toLocaleString()}</td>
            <td class="fsize">${t.sizeKb >= 1024 ? `${(t.sizeKb / 1024).toFixed(1)} MB` : `${t.sizeKb} KB`}</td>
            <td class="muted" style="font-size:11.5px;max-width:260px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(t.comment || '')}</td>
            <td><div class="fops">
              <button class="btn btn-xs btn-ghost" data-act="db-table" data-table="${esc(t.name)}">浏览</button>
              <button class="btn btn-xs btn-ghost" data-act="db-structure" data-table="${esc(t.name)}">结构</button>
            </div></td>
          </tr>`,
        )
        .join('')
    : '';

  view().innerHTML = `
    <div class="db-toolbar">
      <label class="muted" style="font-size:11.5px">数据库</label>
      <select id="db-pick">${options}</select>
      <span class="tag">${info.tables || 0} 张表 · ${esc(info.sizeText || '0 B')} · ${esc(info.charset || '')}</span>
      <div style="margin-left:auto;display:flex;gap:6px;flex-wrap:wrap">
        <button class="btn btn-sm btn-primary" data-act="db-sql">SQL 查询</button>
        <button class="btn btn-sm" data-act="db-export">导出</button>
        <button class="btn btn-sm" data-act="db-import">导入</button>
        <button class="btn btn-sm" data-act="db-optimize">优化表</button>
        <button class="btn btn-sm" data-act="db-reload">刷新</button>
      </div>
    </div>

    ${
      d.site
        ? `<div class="card">
            <div class="card-head">
              <h3 class="card-title">连接信息</h3>
              <div style="display:flex;gap:6px">
                <button class="btn btn-xs" data-act="db-copy">复制</button>
                <button class="btn btn-xs" data-act="db-files">打开站点文件</button>
              </div>
            </div>
            <div class="db-kv">
              <div><span>数据库</span><b class="mono">${esc(d.name)}</b></div>
              <div><span>用户名</span><b class="mono">${esc(d.site.user)}</b></div>
              <div><span>密码</span><b class="mono">${esc(d.site.password)}</b></div>
              <div><span>主机</span><b class="mono">${esc(d.site.host)}</b></div>
              <div><span>表前缀</span><b class="mono">${esc(d.site.prefix || '—')}</b></div>
              <div><span>站点目录</span><b class="mono">${esc(d.site.root)}</b></div>
            </div>
            <div class="note" style="margin-top:10px">来自站点 wp-config.php，供 Navicat / DataGrip 等外部工具使用（需 SSH 隧道，MariaDB 默认不对外监听）</div>
          </div>`
        : ''
    }

    <div class="card">
      <div class="card-head"><h3 class="card-title">表（${d.tables.length}）</h3></div>
      ${
        d.tables.length
          ? `<table class="file-table">
              <thead><tr><th>表名</th><th style="width:80px">引擎</th><th style="width:90px">行数</th><th style="width:90px">大小</th><th>备注</th><th style="width:130px"></th></tr></thead>
              <tbody>${rows}</tbody>
            </table>`
          : `<div class="muted">这个库里还没有表</div>`
      }
    </div>`;
}

async function renderTableDetail() {
  const s = currentServer();
  const d = state.db;
  if (!s || !d.table) return renderDbTables();

  view().innerHTML = `<div class="muted"><span class="spinner"></span> 读取 ${esc(d.table)}…</div>`;
  const q = new URLSearchParams({ db: d.name, table: d.table });
  let data;
  try {
    if (d.view === 'data') {
      q.set('limit', String(d.limit));
      q.set('offset', String(d.offset));
      data = await api(`/api/servers/${s.id}/db/rows?${q.toString()}`);
    } else {
      q.set('kind', d.view);
      data = await api(`/api/servers/${s.id}/db/structure?${q.toString()}`);
    }
  } catch (err) {
    view().innerHTML = `<div class="card">
      <div style="color:var(--danger)">${esc(err.message)}</div>
      <div style="margin-top:12px"><button class="btn btn-sm" data-act="db-back">返回表列表</button></div>
    </div>`;
    return;
  }
  d.detail = data;

  const est = (d.tables.find((t) => t.name === d.table) || {}).rows;
  const tabs = [
    ['data', '数据'],
    ['columns', '结构'],
    ['indexes', '索引'],
  ]
    .map(
      ([kind, label]) =>
        `<button class="btn btn-sm ${d.view === kind ? 'is-on' : ''}" data-act="db-tab" data-kind="${kind}">${label}</button>`,
    )
    .join('');

  let body;
  let pager = '';
  if (d.view === 'data') {
    body = data.rows.length
      ? dbGrid(data.columns, data.rows, '58vh')
      : '<div class="muted" style="padding:18px 4px">这张表还没有数据</div>';
    pager = `<div class="db-pager">
      <span class="muted">${data.rows.length ? `第 ${d.offset + 1}–${d.offset + data.rows.length} 行` : '0 行'}${est ? ` · 共约 ${est.toLocaleString()} 行` : ''}</span>
      <button class="btn btn-sm" data-act="db-prev" ${d.offset ? '' : 'disabled'}>上一页</button>
      <button class="btn btn-sm" data-act="db-next" ${data.rows.length < d.limit ? 'disabled' : ''}>下一页</button>
    </div>`;
  } else if (d.view === 'columns') {
    body = dbGrid(data.columns, data.rows, '58vh');
  } else {
    body = data.rows.length ? dbGrid(data.columns, data.rows, '58vh') : '<div class="muted">没有索引</div>';
  }

  view().innerHTML = `
    <div class="db-toolbar">
      <button class="btn btn-sm" data-act="db-back">← 表列表</button>
      <span class="mono" style="font-weight:600">${esc(d.name)}.${esc(d.table)}</span>
      <div style="display:flex;gap:4px">${tabs}</div>
      <div style="margin-left:auto;display:flex;gap:6px">
        <button class="btn btn-sm" data-act="db-optimize-table">优化此表</button>
        <button class="btn btn-sm" data-act="db-sql">SQL 查询</button>
      </div>
    </div>
    ${pager}
    <div class="db-detail-body">${body}</div>`;
}

function cellHtml(value) {
  if (value === null || value === undefined) return '<td><span class="db-null">NULL</span></td>';
  const text = String(value).replace(/\r\n/g, '\n');
  const shown = text.length > 300 ? `${text.slice(0, 300)}…` : text;
  // 换行/制表符换成可见符号，否则一条长文本会把整行撑得很高
  const flat = shown.replace(/\n/g, ' ↵ ').replace(/\t/g, ' ⇥ ');
  return `<td title="${esc(text.slice(0, 800))}">${esc(flat)}</td>`;
}

function dbGrid(columns, rows, maxHeight = '60vh') {
  if (!columns || !columns.length) return '<div class="muted">没有返回结果集</div>';
  return `<div class="db-grid-scroll" style="max-height:${maxHeight}">
    <table class="db-grid">
      <thead><tr>${columns.map((c) => `<th>${esc(c)}</th>`).join('')}</tr></thead>
      <tbody>${rows.map((r) => `<tr>${r.map(cellHtml).join('')}</tr>`).join('')}</tbody>
    </table>
  </div>`;
}

/* ---- SQL 控制台 ---- */

function sqlConsole() {
  const d = state.db;
  const sample = d.tables[0]?.name || 'your_table';
  openModal({
    title: 'SQL 控制台',
    subtitle: `当前库 ${d.name} · 一次一条语句 · 只读查询自动限制 500 行`,
    wide: true,
    body: `<div class="field">
        <textarea id="q-sql" class="code-editor" style="min-height:120px" spellcheck="false"
          placeholder="SELECT * FROM ${esc(sample)} LIMIT 10"></textarea>
        <div class="note">Ctrl / ⌘ + Enter 执行。写操作（UPDATE / DELETE / DROP 等）会先要求确认；多条语句请用「导入」</div>
      </div>
      <div id="q-result"></div>`,
    footer: `<button class="btn" data-close>关闭</button>
             <button class="btn btn-primary" id="q-run">执行</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);

  const run = (confirm = false) => runSqlPanel(confirm);
  $('#q-run').addEventListener('click', () => run(false));
  $('#q-sql').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      run(false);
    }
  });
}

async function runSqlPanel(confirm) {
  const sqlEl = $('#q-sql');
  const box = $('#q-result');
  if (!sqlEl || !box) return;
  const sql = sqlEl.value.trim();
  if (!sql) return toast('请输入 SQL', 'err');

  const btn = $('#q-run');
  btn.disabled = true;
  box.innerHTML = `<div class="muted"><span class="spinner"></span> 执行中…</div>`;
  try {
    const r = await api(`/api/servers/${state.currentId}/db/query`, {
      method: 'POST',
      body: JSON.stringify({ db: state.db.name, sql, confirm }),
    });

    if (r.needConfirm) {
      box.innerHTML = `<div class="sql-warn">
        <div><strong>这是写操作</strong>，会修改 <span class="mono">${esc(state.db.name)}</span> 的数据：</div>
        <pre class="sql-echo">${esc(r.statement)}</pre>
        <div class="note">确认无误后点下面的按钮，语句才会真正发给数据库。</div>
        <div style="margin-top:9px"><button class="btn btn-solid-danger btn-sm" id="q-confirm">我确认，执行这条语句</button></div>
      </div>`;
      $('#q-confirm').addEventListener('click', () => runSqlPanel(true));
      return;
    }

    if (r.kind === 'write') {
      state.db.loadedFor = ''; // 表行数可能变了，回列表时重新拉
      box.innerHTML = `<div class="sql-ok">✓ ${esc(r.message)}<span class="muted"> · ${r.durationMs} ms</span></div>`;
      toast('执行成功', 'ok');
      return;
    }

    box.innerHTML = `
      <div class="db-result-head">
        <span>${r.rowCount} 行</span>
        <span class="muted">· ${r.durationMs} ms${r.truncated ? ` · 只显示前 ${r.rows.length} 行` : ''}</span>
      </div>
      ${dbGrid(r.columns, r.rows, '46vh')}`;
  } catch (err) {
    box.innerHTML = `<div class="sql-error"><strong>执行失败</strong><div class="mono" style="margin-top:4px;white-space:pre-wrap">${esc(err.message)}</div></div>`;
  } finally {
    btn.disabled = false;
  }
}

/* ---- 导入 / 导出 / 优化 ---- */

function exportDatabase() {
  const d = state.db;
  toast(`正在导出 ${d.name}，文件较大时请稍等…`);
  window.open(
    `/api/servers/${state.currentId}/db/export?db=${encodeURIComponent(d.name)}`,
    '_blank',
  );
}

function importDbModal() {
  const d = state.db;
  openModal({
    title: `导入 SQL 到 ${d.name}`,
    subtitle: '支持 .sql 与 .sql.gz，最大 300 MB',
    body: `<div class="note" style="padding:9px 11px;background:var(--danger-soft);border-radius:6px;color:var(--danger);margin-bottom:14px">
        导入会直接在现有库上执行文件里的 SQL，同名表会被覆盖或修改。正式数据建议先勾选下面的备份。
      </div>
      <div class="field">
        <label>SQL 文件</label>
        <input type="file" id="imp-file" accept=".sql,.gz,.sql.gz,application/gzip">
        <div class="note" id="imp-name">未选择文件</div>
      </div>
      <label class="check"><input type="checkbox" id="imp-backup" checked> 导入前先把当前库导出到本机备份（推荐）</label>
      <div id="imp-progress"></div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-solid-danger" id="imp-start" disabled>开始导入</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);

  let file = null;
  $('#imp-file').addEventListener('change', (e) => {
    file = e.target.files[0] || null;
    $('#imp-name').textContent = file ? `${file.name} · ${fmtBytes(file.size)}` : '未选择文件';
    $('#imp-start').disabled = !file;
  });

  $('#imp-start').addEventListener('click', () => {
    if (!file) return;
    const name = file.name;
    const backup = $('#imp-backup').checked;
    const btn = $('#imp-start');
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner"></span> 导入中';
    const bar = $('#imp-progress');
    bar.innerHTML = `<div class="bar" style="margin-top:12px"><i style="width:0%"></i></div>
      <div class="note" id="imp-stage">正在上传文件…</div>`;

    const xhr = new XMLHttpRequest();
    xhr.open(
      'POST',
      `/api/servers/${state.currentId}/db/import?db=${encodeURIComponent(state.db.name)}&name=${encodeURIComponent(name)}&backup=${backup ? '1' : '0'}`,
    );
    xhr.setRequestHeader('x-panel', '1');
    xhr.setRequestHeader('content-type', 'application/octet-stream');
    xhr.upload.onprogress = (ev) => {
      if (!ev.lengthComputable) return;
      const pct = Math.round((ev.loaded / ev.total) * 100);
      const fill = bar.querySelector('.bar > i');
      if (fill) fill.style.width = `${pct}%`;
      const stage = $('#imp-stage');
      if (stage) stage.textContent = pct >= 100 ? '上传完成，服务器正在执行 SQL…' : `正在上传 ${pct}%`;
    };
    xhr.onload = () => {
      let data = {};
      try {
        data = JSON.parse(xhr.responseText || '{}');
      } catch {
        /* 忽略 */
      }
      if (xhr.status >= 200 && xhr.status < 300) {
        closeModal();
        toast(`导入完成${data.backup ? `，导入前的备份：${data.backup.name}` : ''}`, 'ok');
        state.db.loadedFor = '';
        renderDatabase();
      } else {
        bar.innerHTML = `<div class="sql-error" style="margin-top:12px">${esc(data.error || `导入失败（${xhr.status}）`)}</div>`;
        btn.disabled = false;
        btn.textContent = '重新导入';
      }
    };
    xhr.onerror = () => {
      bar.innerHTML = '<div class="sql-error" style="margin-top:12px">上传中断，检查网络或面板是否仍在运行</div>';
      btn.disabled = false;
      btn.textContent = '重试';
    };
    xhr.send(file);
  });
}

function optimizeModal(table) {
  const d = state.db;
  const scope = table ? `表 ${table}` : `整个库 ${d.name}（${d.tables.length} 张表）`;
  openModal({
    title: '优化 / 修复表',
    subtitle: scope,
    body: `<div class="field">
        <label>操作</label>
        <select id="opt-mode">
          <option value="optimize">优化表（OPTIMIZE TABLE，回收碎片）</option>
          <option value="repair">修复表（REPAIR TABLE，表损坏时用）</option>
        </select>
      </div>
      <div class="note">表较大时会执行较久（面板给 15 分钟超时），期间站点仍可访问；InnoDB 表会以重建方式完成优化。</div>
      <div id="opt-result" style="margin-top:12px"></div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="opt-run">开始</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#opt-run').addEventListener('click', async () => {
    const btn = $('#opt-run');
    const mode = $('#opt-mode').value;
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner"></span> 执行中';
    try {
      const r = await api(`/api/servers/${state.currentId}/db/optimize`, {
        method: 'POST',
        body: JSON.stringify({ db: d.name, table: table || '', mode }),
      });
      $('#opt-result').innerHTML = `<div class="db-result-head"><span>${r.count} 张表</span></div>${dbGrid(r.columns, r.rows, '40vh')}`;
      toast(mode === 'repair' ? '修复完成' : '优化完成', 'ok');
    } catch (err) {
      $('#opt-result').innerHTML = `<div class="sql-error">${esc(err.message)}</div>`;
    } finally {
      btn.disabled = false;
      btn.textContent = '完成';
    }
  });
}

/* ---------------------------------------------------------------- 备份 / 日志 */

async function renderBackups() {
  const s = currentServer();
  view().innerHTML = `<div class="muted"><span class="spinner"></span> 读取备份列表…</div>`;
  try {
    const data = await api(`/api/servers/${s.id}/backups`);
    const files = data.files || [];
    view().innerHTML = `
      <div class="card-head" style="margin-bottom:14px">
        <div class="muted" style="font-size:11.5px">共 ${files.length} 个文件，保存在本机 panel/data/backups/${esc(s.id)}/</div>
        <button class="btn btn-sm" data-act="reload-backups">刷新</button>
      </div>
      ${
        files.length
          ? `<table class="file-table">
              <thead><tr><th>文件名</th><th style="width:100px">大小</th><th style="width:140px">备份时间</th></tr></thead>
              <tbody>
                ${files
                  .map(
                    (f) => `<tr>
                      <td class="mono" style="font-size:11.5px">${esc(f.name)}</td>
                      <td class="fsize">${fmtBytes(f.size)}</td>
                      <td class="fmtime">${fmtTime(f.mtime)}</td>
                    </tr>`,
                  )
                  .join('')}
              </tbody>
            </table>`
          : `<div class="empty"><strong>还没有备份</strong>到「站点」里选择备份，文件会下载到本机</div>`
      }`;
  } catch (err) {
    view().innerHTML = `<div class="card" style="color:var(--danger)">${esc(err.message)}</div>`;
  }
}

async function renderLogs() {
  view().innerHTML = `<div class="muted"><span class="spinner"></span> 读取操作记录…</div>`;
  try {
    const data = await api('/api/logs?limit=120');
    const logs = data.logs || [];
    view().innerHTML = logs.length
      ? `<table class="file-table">
          <thead><tr><th style="width:140px">时间</th><th style="width:120px">类型</th><th>对象</th><th style="width:80px">结果</th></tr></thead>
          <tbody>
            ${logs
              .map(
                (l) => `<tr>
                  <td class="fmtime">${fmtTime(new Date(l.ts).getTime())}</td>
                  <td><span class="tag">${esc(l.type || '')}</span></td>
                  <td class="mono" style="font-size:11.5px">${esc(l.detail || l.target || '')}</td>
                  <td>${l.result === 'ok' ? '<span class="tag ok">成功</span>' : l.result === 'error' ? '<span class="tag danger">异常</span>' : `<span class="tag warn">${esc(l.result || '')}</span>`}</td>
                </tr>`,
              )
              .join('')}
          </tbody>
        </table>`
      : `<div class="empty"><strong>还没有操作记录</strong>执行添加站点、备份等操作后会记录在这里</div>`;
  } catch (err) {
    view().innerHTML = `<div class="card" style="color:var(--danger)">${esc(err.message)}</div>`;
  }
}

/* ---------------------------------------------------------------- FTP */

async function renderFTP(force) {
  const s = currentServer();
  if (!s) return;
  if (force || !state.ftp || state.ftp.for !== s.id) {
    view().innerHTML = `<div class="muted"><span class="spinner"></span> 读取 FTP 状态…</div>`;
    try {
      const r = await api(`/api/servers/${s.id}/ftp`);
      state.ftp = { for: s.id, data: r.status };
    } catch (err) {
      view().innerHTML = `<div class="card">
        <div style="color:var(--danger)">${esc(err.message)}</div>
        <div style="margin-top:12px"><button class="btn btn-sm" data-act="ftp-reload">重试</button></div>
      </div>`;
      return;
    }
  }
  const d = state.ftp.data;
  view().innerHTML = d.installed ? ftpInstalledView(d) : ftpInstallView(d);
}

function ftpApply(status) {
  if (status && state.ftp) state.ftp.data = status;
  if (state.ftp) view().innerHTML = ftpInstalledView(state.ftp.data);
}

function ftpHost(d) {
  return d.pasvAddr || d.publicIp || '服务器 IP';
}

function ftpInstallView(d) {
  return `
    <div class="card">
      <div class="card-head">
        <div>
          <div class="card-title">FTP 组件</div>
          <div class="muted" style="font-size:11.5px">可选装，初始部署不会安装</div>
        </div>
        <span class="tag">未安装</span>
      </div>
      <div style="max-width:660px;font-size:12.5px;line-height:1.75;color:var(--text-2)">
        需要给别人开一条「只能动某个目录」的上传通道时再装。装的是 <b>vsftpd</b>：
        禁止匿名登录、每个账号被 chroot 在自己的目录里、账号以该目录属主的身份读写
        （上传的文件 ${wsName()} / WordPress 立刻能用，不会出现「传上去了但后台改不了」）。
      </div>
      <div class="ftp-hint">
        <div><b>21</b> 控制端口</div>
        <div><b>${d.pasvMin || 40000}-${d.pasvMax || 40100}</b> 被动端口</div>
        <div><b>${d.publicIp ? esc(d.publicIp) : '待探测'}</b> 公网地址</div>
        <div><b>${d.firewall === 'ufw' ? 'ufw' : d.firewall === 'firewalld' ? 'firewalld' : '未启用'}</b> 本机防火墙</div>
      </div>
      <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:16px;max-width:560px">
        <div class="field" style="margin-bottom:0">
          <label>被动端口起</label>
          <input type="number" id="ftp-pmin" value="${d.pasvMin || 40000}" min="1024" max="65535">
        </div>
        <div class="field" style="margin-bottom:0">
          <label>被动端口止</label>
          <input type="number" id="ftp-pmax" value="${d.pasvMax || 40100}" min="1024" max="65535">
        </div>
      </div>
      <div class="field" style="margin-top:12px;max-width:560px">
        <label>公网地址（留空自动探测）</label>
        <input type="text" id="ftp-paddr" value="${esc(d.pasvAddr || '')}" placeholder="${esc(d.publicIp || '例如 203.0.113.10')}">
        <div class="note">云主机是 NAT 网络，被动模式必须告诉客户端公网 IP，否则能登录但列不出目录。</div>
      </div>
      <div style="display:flex;gap:8px;margin-top:16px">
        <button class="btn btn-primary" data-act="ftp-install">安装 FTP 组件</button>
        <button class="btn btn-ghost" data-act="ftp-reload">重新检测</button>
      </div>
      <div class="note" style="margin-top:12px">安装需要能访问软件源，大约 30-60 秒；装完还要在云控制台安全组放行 21 与被动端口段。</div>
    </div>`;
}

function ftpInstalledView(d) {
  const users = d.users || [];
  return `
    <div class="card">
      <div class="card-head">
        <div>
          <div class="card-title">FTP 组件</div>
          <div class="muted" style="font-size:11.5px">vsftpd ${esc(d.version || '')} · 端口 ${d.port}</div>
        </div>
        <div style="display:flex;align-items:center;gap:8px">
          ${d.running ? '<span class="tag ok">运行中</span>' : '<span class="tag danger">已停止</span>'}
          <button class="btn btn-sm" data-act="ftp-reload">刷新</button>
        </div>
      </div>
      <div class="grid-stats" style="grid-template-columns:repeat(4,minmax(0,1fr))">
        <div class="stat"><div class="stat-label">公网地址</div><div class="stat-value" style="font-size:13px">${esc(ftpHost(d))}</div></div>
        <div class="stat"><div class="stat-label">被动端口</div><div class="stat-value" style="font-size:13px">${d.pasvMin}-${d.pasvMax}</div></div>
        <div class="stat"><div class="stat-label">本机防火墙</div><div class="stat-value" style="font-size:13px">${esc(d.firewall === 'none' ? '未启用' : d.firewall)}</div></div>
        <div class="stat"><div class="stat-label">账号</div><div class="stat-value">${users.length} <small>个</small></div></div>
      </div>
      <div style="display:flex;gap:6px;margin-top:14px;flex-wrap:wrap">
        ${
          d.running
            ? `<button class="btn btn-sm" data-act="ftp-restart">重启</button>
               <button class="btn btn-sm" data-act="ftp-stop">停止</button>`
            : `<button class="btn btn-sm btn-primary" data-act="ftp-start">启动</button>`
        }
        <button class="btn btn-sm btn-danger" data-act="ftp-uninstall">卸载组件</button>
        <div class="muted" style="font-size:11.5px;align-self:center;margin-left:auto">
          停用后 21 端口关闭，账号与目录都不动
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-head"><div class="card-title">连接设置</div></div>
      <div style="display:grid;grid-template-columns:1fr 1fr 1.4fr;gap:12px;align-items:end">
        <div class="field" style="margin-bottom:0">
          <label>被动端口起</label>
          <input type="number" id="ftp-pmin" value="${d.pasvMin}" min="1024" max="65535">
        </div>
        <div class="field" style="margin-bottom:0">
          <label>被动端口止</label>
          <input type="number" id="ftp-pmax" value="${d.pasvMax}" min="1024" max="65535">
        </div>
        <div class="field" style="margin-bottom:0">
          <label>公网地址</label>
          <input type="text" id="ftp-paddr" value="${esc(d.pasvAddr || '')}" placeholder="${esc(d.publicIp || '')}">
        </div>
      </div>
      <div style="display:flex;gap:8px;margin-top:12px;align-items:center">
        <button class="btn btn-sm" data-act="ftp-save-config">保存并重启</button>
        <span class="note" style="margin:0">改端口后记得同步安全组</span>
      </div>
    </div>

    <div class="card">
      <div class="card-head">
        <div class="card-title">FTP 账号</div>
        <button class="btn btn-sm btn-primary" data-act="ftp-add-user">新建账号</button>
      </div>
      ${
        users.length === 0
          ? `<div class="empty" style="padding:26px 0">
               <strong>还没有账号</strong>
               新建时选一个站点目录，账号就被锁在那个目录里
             </div>`
          : `<table class="file-table">
              <thead><tr>
                <th>账号</th><th>根目录</th><th style="width:70px">权限</th><th style="width:70px">状态</th><th style="width:250px"></th>
              </tr></thead>
              <tbody>
                ${users
                  .map(
                    (u) => `<tr>
                      <td><span class="mono" style="font-size:12px">${esc(u.name)}</span></td>
                      <td><span class="mono" style="font-size:11.5px;color:var(--text-2)">${esc(u.dir)}</span></td>
                      <td>${u.writable ? '<span class="tag info">读写</span>' : '<span class="tag">只读</span>'}</td>
                      <td>${u.active ? '<span class="tag ok">正常</span>' : '<span class="tag warn">已锁定</span>'}</td>
                      <td><div class="fops">
                        <button class="btn btn-xs btn-ghost" data-act="ftp-user-dir" data-name="${esc(u.name)}" data-dir="${esc(u.dir)}">改目录</button>
                        <button class="btn btn-xs btn-ghost" data-act="ftp-user-pass" data-name="${esc(u.name)}">改密码</button>
                        <button class="btn btn-xs btn-ghost" data-act="ftp-user-mode" data-name="${esc(u.name)}" data-writable="${u.writable ? '1' : '0'}">${u.writable ? '改只读' : '改读写'}</button>
                        <button class="btn btn-xs btn-ghost" data-act="ftp-user-del" data-name="${esc(u.name)}" data-dir="${esc(u.dir)}">删除</button>
                      </div></td>
                    </tr>`,
                  )
                  .join('')}
              </tbody>
            </table>`
      }
      <div class="note" style="margin-top:12px">
        客户端用 SFTP/FTP 均可：主机 <b>${esc(ftpHost(d))}</b>、端口 <b>${d.port}</b>、传输模式建议「被动（PASV）」。
      </div>
    </div>`;
}

function ftpDirOptions() {
  const roots = (state.snapshot?.sites || []).map((x) => x.root);
  return roots.map((r) => `<option value="${esc(r)}"></option>`).join('');
}

function genPassword(n = 14) {
  const chars = 'abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789';
  const out = [];
  const buf = new Uint32Array(n);
  crypto.getRandomValues(buf);
  for (let i = 0; i < n; i += 1) out.push(chars[buf[i] % chars.length]);
  return out.join('');
}

function installFTPFlow() {
  const pasvMin = Number($('#ftp-pmin')?.value || 0);
  const pasvMax = Number($('#ftp-pmax')?.value || 0);
  const pasvAddr = ($('#ftp-paddr')?.value || '').trim();
  runStream(`/api/servers/${state.currentId}/ftp/install`, { pasvMin, pasvMax, pasvAddr }, '安装 FTP 组件', (evt) => {
    if (evt.ok) renderFTP(true);
  });
}

function uninstallFTPFlow() {
  const d = state.ftp?.data;
  const users = d?.users || [];
  openModal({
    title: '卸载 FTP 组件',
    body: `
      <div style="padding:11px 13px;border-radius:8px;background:var(--danger-soft);color:var(--danger);font-size:12.5px;line-height:1.6;text-align:left">
        会停止并卸载 vsftpd，删除 <code>${esc('/etc/vsftpd.conf')}</code> 与账号配置。
        <b>站点文件不会被删除</b>，只是这些账号登不上了。
      </div>
      ${
        users.length
          ? `<div class="field" style="margin-top:16px">
               <label>同时删除的面板账号（${users.length} 个）</label>
               <div class="mono t-list">${users.map((u) => `${esc(u.name)} → ${esc(u.dir)}`).join('<br>')}</div>
             </div>`
          : ''
      }
      <div class="note" style="margin-top:12px">以后想用可以随时再装回来，账号需要重新创建。</div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-danger" id="ftp-del">卸载</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#ftp-del').addEventListener('click', () => {
    closeModal();
    runStream(`/api/servers/${state.currentId}/ftp/uninstall`, {}, '卸载 FTP 组件', (evt) => {
      if (evt.ok) renderFTP(true);
    });
  });
}

function ftpCall(path, body, okMsg) {
  return api(`/api/servers/${state.currentId}${path}`, {
    method: 'POST',
    body: JSON.stringify(body || {}),
  }).then((r) => {
    if (okMsg) toast(okMsg, 'ok');
    if (r && r.status) ftpApply(r.status);
    return r;
  });
}

function ftpAddUserModal() {
  const dirs = (state.snapshot?.sites || []).map((x) => x.root);
  const defDir = dirs[0] || '/var/www';
  const pw = genPassword();

  openModal({
    title: '新建 FTP 账号',
    body: `
      <div class="field">
        <label>用户名</label>
        <input type="text" id="fu-name" placeholder="例如 editor" spellcheck="false" autocomplete="off">
        <div class="note">3-32 位小写字母、数字、下划线或连字符，以字母开头</div>
      </div>
      <div class="field">
        <label>密码</label>
        <div style="display:flex;gap:6px">
          <input type="text" id="fu-pass" class="mono" value="${esc(pw)}" spellcheck="false" autocomplete="off">
          <button class="btn btn-sm" id="fu-gen" style="white-space:nowrap">换一个</button>
        </div>
      </div>
      <div class="field">
        <label>FTP 根目录</label>
        <input type="text" id="fu-dir" value="${esc(defDir)}" list="ftp-dir-list" spellcheck="false" autocomplete="off">
        <datalist id="ftp-dir-list">${ftpDirOptions()}</datalist>
        <div class="note">账号被 chroot 在这里，看不到上级目录；选站点根目录即可</div>
      </div>
      <div class="field">
        <label>权限</label>
        <select id="fu-writable">
          <option value="1">读写（能上传、改名、删除）</option>
          <option value="0">只读（只能下载和浏览）</option>
        </select>
      </div>
      <div class="note">账号会以该目录属主的身份读写，上传的文件 ${wsName()} / WordPress 直接可用。</div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="fu-save">创建</button>`,
  });

  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#fu-gen').addEventListener('click', () => {
    $('#fu-pass').value = genPassword();
  });
  $('#fu-save').addEventListener('click', async () => {
    const name = $('#fu-name').value.trim();
    const password = $('#fu-pass').value;
    const dir = $('#fu-dir').value.trim();
    const writable = $('#fu-writable').value === '1';
    if (!/^[a-z][a-z0-9_-]{2,31}$/.test(name)) return toast('用户名格式不对', 'err');
    if (password.length < 6) return toast('密码至少 6 位', 'err');
    if (!dir.startsWith('/')) return toast('目录要填绝对路径', 'err');
    const btn = $('#fu-save');
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner"></span> 创建中';
    try {
      const r = await ftpCall('/ftp/users', { name, password, dir, writable });
      closeModal();
      ftpApply(r.status);
      ftpCredsModal(name, dir, password, r.log);
    } catch (err) {
      toast(err.message, 'err');
      btn.disabled = false;
      btn.textContent = '创建';
    }
  });
}

function ftpCredsModal(name, dir, password, log) {
  const d = state.ftp?.data || {};
  const host = ftpHost(d);
  const text = `主机：${host}\n端口：${d.port || 21}\n账号：${name}\n密码：${password}\n根目录：${dir}\n传输模式：被动（PASV）`;
  openModal({
    title: '账号已创建',
    subtitle: name,
    body: `
      <div class="field">
        <label>连接信息</label>
        <div class="mono t-list" style="max-height:none;white-space:pre-wrap">${esc(text)}</div>
      </div>
      <div class="note">密码只在这里显示一次，关掉后只能重置。客户端里记得选「被动 / PASV」模式。</div>
      ${
        log && log.includes('[warn]')
          ? `<div class="field" style="margin-top:14px">
               <label>服务端自检</label>
               <div class="mono t-list" style="max-height:120px;white-space:pre-wrap;color:var(--warn)">${esc(log)}</div>
             </div>`
          : ''
      }`,
    footer: `<button class="btn" data-close>关闭</button>
             <button class="btn btn-primary" id="fc-copy">复制</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#fc-copy').addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(text);
      toast('连接信息已复制', 'ok');
    } catch (e) {
      toast('复制失败，请手动选中', 'err');
    }
  });
}

function ftpUserDirModal(name, dir) {
  openModal({
    title: '变更 FTP 根目录',
    subtitle: name,
    body: `
      <div class="field">
        <label>新目录</label>
        <input type="text" id="fd-dir" value="${esc(dir)}" list="ftp-dir-list" spellcheck="false" autocomplete="off">
        <datalist id="ftp-dir-list">${ftpDirOptions()}</datalist>
        <div class="note">只改指路，不移动文件；目录不存在会报错（可以先用文件管理新建）。</div>
      </div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="fd-save">保存</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#fd-save').addEventListener('click', async () => {
    const next = $('#fd-dir').value.trim();
    if (!next.startsWith('/')) return toast('目录要填绝对路径', 'err');
    try {
      const r = await ftpCall('/ftp/users/dir', { name, dir: next }, '目录已更新');
      closeModal();
      ftpApply(r.status);
    } catch (err) {
      toast(err.message, 'err');
    }
  });
}

function ftpUserPassModal(name) {
  const pw = genPassword();
  openModal({
    title: '重置密码',
    subtitle: name,
    body: `
      <div class="field">
        <label>新密码</label>
        <div style="display:flex;gap:6px">
          <input type="text" id="fp-pass" class="mono" value="${esc(pw)}" spellcheck="false" autocomplete="off">
          <button class="btn btn-sm" id="fp-gen" style="white-space:nowrap">换一个</button>
        </div>
      </div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="fp-save">保存</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#fp-gen').addEventListener('click', () => {
    $('#fp-pass').value = genPassword();
  });
  $('#fp-save').addEventListener('click', async () => {
    const password = $('#fp-pass').value;
    if (password.length < 6) return toast('密码至少 6 位', 'err');
    try {
      await ftpCall('/ftp/users/password', { name, password }, '密码已更新');
      closeModal();
      toast(`新密码：${password}`, 'ok');
    } catch (err) {
      toast(err.message, 'err');
    }
  });
}

function ftpUserDeleteModal(name, dir) {
  openModal({
    title: '删除 FTP 账号',
    subtitle: name,
    body: `
      <div style="padding:11px 13px;border-radius:8px;background:var(--danger-soft);color:var(--danger);font-size:12.5px;line-height:1.6;text-align:left">
        只删账号，<b>不会删除目录与文件</b>。
      </div>
      <div class="field" style="margin-top:14px">
        <label>目录（保留）</label>
        <div class="mono t-list">${esc(dir)}</div>
      </div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-danger" id="fdu-del">删除账号</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#fdu-del').addEventListener('click', async () => {
    try {
      const r = await ftpCall('/ftp/users/delete', { name }, `账号 ${name} 已删除`);
      closeModal();
      ftpApply(r.status);
    } catch (err) {
      toast(err.message, 'err');
    }
  });
}

function ftpSaveConfig() {
  const pasvMin = Number($('#ftp-pmin').value || 0);
  const pasvMax = Number($('#ftp-pmax').value || 0);
  const pasvAddr = ($('#ftp-paddr').value || '').trim();
  if (!pasvMin || !pasvMax || pasvMin > pasvMax) return toast('端口范围填反了', 'err');
  ftpCall('/ftp/config', { pasvMin, pasvMax, pasvAddr }, '设置已保存，服务已重启').catch((err) =>
    toast(err.message, 'err'),
  );
}

/* ---------------------------------------------------------------- 维护 */

const MAINT_TABS = [
  { key: 'php', label: 'PHP 版本' },
  { key: 'logs', label: '网站日志' },
  { key: 'cron', label: '定时任务' },
  { key: 'exec', label: '执行命令' },
];

function renderMaint() {
  const t = state.maint.tab || 'php';
  view().innerHTML = `
    <div class="subtabs">
      ${MAINT_TABS.map(
        (x) =>
          `<button class="subtab ${x.key === t ? 'is-active' : ''}" data-act="maint-tab" data-mtab="${x.key}">${x.label}</button>`,
      ).join('')}
    </div>
    <div id="maint-body"><div class="muted"><span class="spinner"></span> 读取中…</div></div>`;
  renderMaintPane();
}

function renderMaintPane(force) {
  const box = $('#maint-body');
  if (!box) return;
  const t = state.maint.tab;
  if (t === 'php') return renderPHPPane(force);
  if (t === 'logs') return renderLogsPane(force);
  if (t === 'cron') return renderCronPane(force);
  return renderExecPane();
}

function maintError(msg, act) {
  return `<div class="card">
    <div style="color:var(--danger)">${esc(msg)}</div>
    <div style="margin-top:12px"><button class="btn btn-sm" data-act="${act}">重试</button></div>
  </div>`;
}

/* ---------------------------------------------------------------- 维护 / PHP 版本 */

async function renderPHPPane(force) {
  const s = currentServer();
  const box = $('#maint-body');
  if (!box || !s) return;
  if (force || !state.maint.php) {
    box.innerHTML = `<div class="muted"><span class="spinner"></span> 读取 PHP 版本…</div>`;
    try {
      const r = await api(`/api/servers/${s.id}/php`);
      state.maint.php = r.status;
    } catch (err) {
      box.innerHTML = maintError(err.message, 'php-reload');
      return;
    }
  }
  if (state.maint.tab !== 'php' || !$('#maint-body')) return;
  box.innerHTML = phpView(state.maint.php);
}

function phpView(d) {
  const versions = d.versions || [];
  const sites = d.sites || [];
  const avail = d.available || [];
  const running = versions.filter((v) => v.running).length;
  const needRepo = avail.some((a) => a.state === 'missing');

  const verRows = versions
    .map(
      (v) => `<tr>
        <td><span class="mono" style="font-size:12.5px">PHP ${esc(v.version)}</span></td>
        <td>${v.running ? '<span class="tag ok">运行中</span>' : '<span class="tag danger">未运行</span>'}</td>
        <td>${v.sites} <span class="muted" style="font-size:11.5px">个站点</span></td>
        <td class="fsize">${v.extensions} 个扩展</td>
        <td>${v.defaultCli ? '<span class="tag info">命令行默认</span>' : '<span class="muted">—</span>'}</td>
        <td><div class="fops">
          ${
            v.defaultCli
              ? ''
              : `<button class="btn btn-xs btn-ghost" data-act="php-default" data-version="${esc(v.version)}">设为默认</button>`
          }
          <button class="btn btn-xs btn-ghost" data-act="php-remove" data-version="${esc(v.version)}" data-sites="${v.sites}">卸载</button>
        </div></td>
      </tr>`,
    )
    .join('');

  const siteRows = sites
    .map((s) => {
      const isStatic = s.type === 'static' || !s.version;
      const opts = versions
        .map((v) => `<option value="${esc(v.version)}" ${v.version === s.version ? 'selected' : ''}>PHP ${esc(v.version)}</option>`)
        .join('');
      const typeTag =
        s.type === 'wordpress'
          ? '<span class="tag info">WordPress</span>'
          : s.type === 'custom'
            ? '<span class="tag">自助站</span>'
            : '<span class="tag">纯静态</span>';
      return `<tr>
        <td><span class="mono" style="font-size:12px">${esc(s.domain)}</span></td>
        <td>${typeTag}</td>
        <td>${
          isStatic
            ? '<span class="muted" style="font-size:11.5px">不跑 PHP</span>'
            : `<select class="php-pick" data-domain="${esc(s.domain)}">${opts}</select>`
        }</td>
        <td><div class="fops">
          ${
            isStatic
              ? '<span class="muted" style="font-size:11.5px">纯静态站点没有 PHP 依赖</span>'
              : `<button class="btn btn-xs" data-act="php-switch" data-domain="${esc(s.domain)}" data-version="${esc(s.version)}">切换</button>`
          }
        </div></td>
      </tr>`;
    })
    .join('');

  const chips = avail
    .map((a) => {
      if (a.state === 'installed') return `<span class="php-chip is-on">PHP ${esc(a.version)}<small>已装</small></span>`;
      if (a.state === 'ready')
        return `<button class="php-chip" data-act="php-install" data-version="${esc(a.version)}">安装 PHP ${esc(a.version)}</button>`;
      return `<span class="php-chip is-off" title="当前软件源里没有这个版本">PHP ${esc(a.version)}<small>需加源</small></span>`;
    })
    .join('');

  return `
    <div class="card">
      <div class="card-head">
        <div>
          <div class="card-title">PHP 版本</div>
          <div class="muted" style="font-size:11.5px">${esc(d.os || '系统未知')} ${esc(d.osVersion || '')}${
            d.codename ? `（${esc(d.codename)}）` : ''
          } · 命令行默认 ${esc(d.cli || '未知')}</div>
        </div>
        <button class="btn btn-sm" data-act="php-reload">刷新</button>
      </div>
      <div class="grid-stats" style="grid-template-columns:repeat(4,minmax(0,1fr))">
        <div class="stat"><div class="stat-label">已装版本</div><div class="stat-value">${versions.length} <small>个</small></div></div>
        <div class="stat"><div class="stat-label">FPM 运行中</div><div class="stat-value">${running} <small>个</small></div></div>
        <div class="stat"><div class="stat-label">站点</div><div class="stat-value">${sites.length} <small>个</small></div></div>
        <div class="stat"><div class="stat-label">PHP 软件源</div><div class="stat-value" style="font-size:13px">${
          d.extraRepo ? '已添加' : '系统默认'
        }</div></div>
      </div>
    </div>

    <div class="card">
      <div class="card-head"><div class="card-title">已安装版本</div></div>
      ${
        versions.length
          ? `<table class="file-table">
              <thead><tr><th>版本</th><th style="width:90px">服务</th><th style="width:110px">使用情况</th><th style="width:110px">扩展</th><th style="width:110px">命令行</th><th style="width:190px"></th></tr></thead>
              <tbody>${verRows}</tbody>
            </table>`
          : `<div class="empty" style="padding:26px 0"><strong>这台机器还没装 PHP</strong>从下面「可安装版本」里挑一个装上</div>`
      }
    </div>

    <div class="card">
      <div class="card-head">
        <div>
          <div class="card-title">站点使用的版本</div>
          <div class="muted" style="font-size:11.5px">切换会迁移该站点的 PHP-FPM pool 并重载 ${wsName()}</div>
        </div>
      </div>
      ${
        sites.length
          ? `<table class="file-table">
              <thead><tr><th>站点</th><th style="width:120px">类型</th><th style="width:170px">当前版本</th><th style="width:90px"></th></tr></thead>
              <tbody>${siteRows}</tbody>
            </table>`
          : `<div class="empty" style="padding:26px 0"><strong>没有站点</strong>先去「站点」里建一个</div>`
      }
    </div>

    <div class="card">
      <div class="card-head">
        <div>
          <div class="card-title">可安装版本</div>
          <div class="muted" style="font-size:11.5px">安装后不会自动绑定站点，需要到上面手动切换</div>
        </div>
        ${d.extraRepo ? '' : `<button class="btn btn-sm" data-act="php-repo">添加 PHP 软件源</button>`}
      </div>
      <div class="php-chips">${chips}</div>
      ${
        needRepo && !d.extraRepo
          ? `<div class="note" style="margin-top:12px">
               系统自带源里只有一两个版本。要装 7.4 / 8.0 / 8.1 / 8.3 这些，需要先加第三方 PHP 源
               （Debian 用 packages.sury.org，Ubuntu 用 ondrej/php），点右上角按钮即可。
             </div>`
          : `<div class="note" style="margin-top:12px">装一个版本大约 40-90 秒，会带上 fpm / cli 与常用扩展（mysql、gd、mbstring、xml、zip、intl、bcmath、opcache）。</div>`
      }
    </div>`;
}

function phpPicked(domain) {
  const list = view().querySelectorAll('.php-pick');
  for (const el of list) {
    if (el.dataset.domain === domain) return el.value;
  }
  return '';
}

function phpSwitchFlow(domain, from) {
  const to = phpPicked(domain);
  if (!to) return toast('找不到版本选择框', 'err');
  if (to === from) return toast('版本没有变化', 'err');

  openModal({
    title: '切换站点 PHP 版本',
    subtitle: domain,
    body: `
      <div class="kv-flow"><span>当前版本</span><b>PHP ${esc(from)}</b></div>
      <div class="kv-flow"><span>切换到</span><b>PHP ${esc(to)}</b></div>
      <div class="note" style="margin-top:14px">
        会把这个站点的 PHP-FPM pool 复制到 ${esc(to)} 目录、改写 ${wsName()} 指向的 socket，再重载 php-fpm 与 ${wsName()}。
        切换瞬间（约 1 秒）站点可能返回 502 或 504。
      </div>
      <div class="note">如果站点用了 imagick、redis、swoole 这类扩展，切换后要确认新版本上也装了。</div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="php-do">切换</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  const btn = $('#php-do');
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner"></span> 切换中';
    try {
      const r = await api(`/api/servers/${state.currentId}/php/site`, {
        method: 'POST',
        body: JSON.stringify({ domain, version: to }),
      });
      state.maint.php = r.status;
      closeModal();
      renderMaintPane();
      toast(`${domain} 已切到 PHP ${to}`, 'ok');
      if (r.log && /[!]|\[warn\]/.test(r.log)) phpLogModal(r.log, '切换日志');
    } catch (err) {
      btn.disabled = false;
      btn.textContent = '切换';
      toast(err.message, 'err');
    }
  });
}

function phpLogModal(text, title) {
  openModal({
    title: title || '脚本输出',
    wide: true,
    body: `<pre class="log-view" style="max-height:46vh">${esc(text)}</pre>`,
    footer: `<button class="btn" data-close>知道了</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
}

function phpDefaultFlow(version) {
  openModal({
    title: '设为默认命令行版本',
    body: `
      <div class="kv-flow"><span>php 命令</span><b>PHP ${esc(version)}</b></div>
      <div class="note" style="margin-top:12px">
        只影响在服务器上直接敲 <code>php</code> 时用哪个版本（composer、wp-cli 会跟着它走），
        <b>不会</b>改任何站点。站点版本请在下面的表格里单独切换。
      </div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="php-def">设默认</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#php-def').addEventListener('click', async () => {
    try {
      const r = await api(`/api/servers/${state.currentId}/php/default`, {
        method: 'POST',
        body: JSON.stringify({ version }),
      });
      state.maint.php = r.status;
      closeModal();
      renderMaintPane();
      toast(`命令行默认版本已改为 PHP ${version}`, 'ok');
    } catch (err) {
      toast(err.message, 'err');
    }
  });
}

function phpRemoveFlow(version, sites) {
  if (sites > 0) {
    openModal({
      title: '暂时不能卸载',
      body: `
        <div class="warn-box">还有 <b>${sites}</b> 个站点在用 PHP ${esc(version)}。</div>
        <div class="note" style="margin-top:12px">
          直接卸载会让这些站点立刻 502。请先在上面的「站点使用的版本」里把它们切到别的版本，再回来卸载。
        </div>`,
      footer: `<button class="btn" data-close>知道了</button>`,
    });
    $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
    return;
  }

  openModal({
    title: '卸载 PHP',
    body: `
      <div class="danger-box">
        会 purge 掉所有 <code>php${esc(version)}-*</code> 包，并删除 <code>/etc/php/${esc(version)}</code>。
        这个版本的配置、扩展设置都会消失，站点文件与数据库不受影响。
      </div>
      <div class="field" style="margin-top:14px">
        <label>输入「${esc(version)}」确认</label>
        <input type="text" id="php-rm-ok" placeholder="${esc(version)}" autocomplete="off" spellcheck="false">
      </div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-danger" id="php-rm" disabled>卸载</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  const input = $('#php-rm-ok');
  const go = $('#php-rm');
  input.addEventListener('input', () => {
    go.disabled = input.value.trim() !== version;
  });
  go.addEventListener('click', () => {
    closeModal();
    runStream(`/api/servers/${state.currentId}/php/remove`, { version }, `卸载 PHP ${version}`, (evt) => {
      if (evt.ok) {
        state.maint.php = null;
        renderPHPPane(true);
      }
    });
  });
}

function phpInstallFlow(version) {
  runStream(`/api/servers/${state.currentId}/php/install`, { version }, `安装 PHP ${version}`, (evt) => {
    if (evt.ok) {
      state.maint.php = null;
      renderPHPPane(true);
    }
  });
}

function phpRepoFlow() {
  const d = state.maint.php || {};
  const deb = (d.os || 'debian') === 'debian';
  openModal({
    title: '添加 PHP 软件源',
    body: `
      <div class="note" style="font-size:12.5px;margin-bottom:12px;line-height:1.7">
        系统自带源里的 PHP 版本很有限。这一步会加入第三方 PHP 源并刷新索引，之后就能装 7.4 到 8.5 的任意版本。
      </div>
      <div class="kv-flow"><span>系统</span><b>${esc(d.os || '')} ${esc(d.osVersion || '')} ${
        d.codename ? `（${esc(d.codename)}）` : ''
      }</b></div>
      <div class="kv-flow"><span>软件源</span><b>${deb ? 'packages.sury.org/php' : 'ppa:ondrej/php'}</b></div>
      <div class="note" style="margin-top:12px">只加源，不改任何已装版本。以后想撤掉，删掉对应的 sources 文件即可。</div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="php-repo-do">添加</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#php-repo-do').addEventListener('click', () => {
    closeModal();
    runStream(`/api/servers/${state.currentId}/php/repo`, {}, '添加 PHP 软件源', (evt) => {
      if (evt.ok) {
        state.maint.php = null;
        renderPHPPane(true);
      }
    });
  });
}

/* ---------------------------------------------------------------- 维护 / 网站日志 */

const LOG_KINDS = {
  access: { label: '访问', cls: '' },
  error: { label: '错误', cls: 'danger' },
  php: { label: 'PHP', cls: 'warn' },
  debug: { label: '调试', cls: '' },
  other: { label: '其它', cls: '' },
  archive: { label: '归档', cls: 'info' },
};

async function renderLogsPane(force) {
  const s = currentServer();
  const box = $('#maint-body');
  if (!box || !s) return;
  if (force || !state.maint.logs) {
    box.innerHTML = `<div class="muted"><span class="spinner"></span> 扫描日志文件…</div>`;
    try {
      const r = await api(`/api/servers/${s.id}/logs/list`);
      state.maint.logs = r.files || [];
    } catch (err) {
      box.innerHTML = maintError(err.message, 'logs-reload');
      return;
    }
  }
  if (state.maint.tab !== 'logs' || !$('#maint-body')) return;
  box.innerHTML = logsView(state.maint.logs, state.maint.logFilter);
}

function logsView(files, filter) {
  // 站点自己的日志排前面，归档垫后，同类按体积从大到小——空文件自然沉底
  const sorted = files.slice().sort((a, b) => {
    const da = a.domain && a.domain !== '-' ? 0 : 1;
    const db = b.domain && b.domain !== '-' ? 0 : 1;
    if (da !== db) return da - db;
    if (a.archive !== b.archive) return a.archive ? 1 : -1;
    return (b.size || 0) - (a.size || 0);
  });
  const list = sorted.filter((f) => {
    if (!filter || filter === 'all') return true;
    if (filter === 'archive') return f.archive;
    return !f.archive && f.kind === filter;
  });
  const total = files.reduce((n, f) => n + (f.size || 0), 0);

  const rows = list
    .map((f) => {
      const kind = LOG_KINDS[f.kind] || { label: f.kind, cls: '' };
      const domain = f.domain ? `<span class="mono" style="font-size:11.5px">${esc(f.domain)}</span>` : '<span class="muted">系统</span>';
      return `<tr>
        <td>${domain}</td>
        <td><span class="tag ${kind.cls}">${kind.label}</span></td>
        <td class="mono" style="font-size:11px;color:var(--text-2);word-break:break-all">${esc(f.path)}</td>
        <td class="fsize">${fmtBytes(f.size)}</td>
        <td class="fmtime">${fmtTime(f.mtime * 1000)}</td>
        <td><div class="fops">
          <button class="btn btn-xs btn-ghost" data-act="log-view" data-path="${esc(f.path)}">查看</button>
          <button class="btn btn-xs btn-ghost" data-act="log-download" data-path="${esc(f.path)}">下载</button>
          ${
            f.archive
              ? `<button class="btn btn-xs btn-ghost" data-act="log-delete" data-path="${esc(f.path)}">删除</button>`
              : `<button class="btn btn-xs btn-ghost" data-act="log-rotate" data-path="${esc(f.path)}">轮转</button>
                 <button class="btn btn-xs btn-ghost" data-act="log-clear" data-path="${esc(f.path)}">清空</button>`
          }
        </div></td>
      </tr>`;
    })
    .join('');

  return `
    <div class="card">
      <div class="card-head">
        <div>
          <div class="card-title">网站日志</div>
          <div class="muted" style="font-size:11.5px">共 ${files.length} 个文件，合计 ${fmtBytes(total)}</div>
        </div>
        <div style="display:flex;gap:8px;align-items:center">
          <select id="log-filter" class="mini-select">
            <option value="all" ${filter === 'all' ? 'selected' : ''}>全部</option>
            <option value="access" ${filter === 'access' ? 'selected' : ''}>访问日志</option>
            <option value="error" ${filter === 'error' ? 'selected' : ''}>错误日志</option>
            <option value="php" ${filter === 'php' ? 'selected' : ''}>PHP 日志</option>
            <option value="debug" ${filter === 'debug' ? 'selected' : ''}>调试日志</option>
            <option value="other" ${filter === 'other' ? 'selected' : ''}>其它</option>
            <option value="archive" ${filter === 'archive' ? 'selected' : ''}>归档</option>
          </select>
          <button class="btn btn-sm" data-act="logs-reload">重新扫描</button>
        </div>
      </div>
      ${
        list.length
          ? `<table class="file-table">
              <thead><tr><th style="width:150px">来源</th><th style="width:70px">类型</th><th>路径</th><th style="width:80px">大小</th><th style="width:130px">更新时间</th><th style="width:210px"></th></tr></thead>
              <tbody>${rows}</tbody>
            </table>`
          : `<div class="empty" style="padding:30px 0"><strong>没有匹配的日志</strong>站点跑起来之后才会有访问与错误日志</div>`
      }
      <div class="note" style="margin-top:12px">
        「清空」是把文件截断成 0 字节，不动 inode，${wsName()} / php-fpm 不用重启；
        「轮转」会把当前内容压缩成 <code>xxx.log.日期.gz</code> 再清空，归档可以在这里删掉。
      </div>
    </div>`;
}

async function openLogModal(path) {
  openModal({
    title: '查看日志',
    subtitle: path,
    wide: true,
    body: `<div id="log-box"><div class="muted"><span class="spinner"></span> 读取中…</div></div>`,
    footer: `<select id="log-lines" class="mini-select">
               <option value="100">最后 100 行</option>
               <option value="200" selected>最后 200 行</option>
               <option value="500">最后 500 行</option>
               <option value="1000">最后 1000 行</option>
             </select>
             <button class="btn" data-close>关闭</button>
             <button class="btn btn-primary" id="log-refresh">刷新</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);

  const load = async () => {
    const box = $('#log-box');
    if (!box) return;
    const lines = Number($('#log-lines')?.value || 200);
    box.innerHTML = `<div class="muted"><span class="spinner"></span> 读取中…</div>`;
    try {
      const r = await api(`/api/servers/${state.currentId}/logs/tail`, {
        method: 'POST',
        body: JSON.stringify({ path, lines }),
      });
      if (!$('#log-box')) return;
      box.innerHTML = r.text
        ? `<pre class="log-view">${esc(r.text)}</pre>`
        : `<div class="empty" style="padding:24px 0"><strong>文件是空的</strong>这个日志还没有内容</div>`;
    } catch (err) {
      if ($('#log-box')) box.innerHTML = `<div style="color:var(--danger)">${esc(err.message)}</div>`;
    }
  };
  $('#log-refresh').addEventListener('click', load);
  $('#log-lines').addEventListener('change', load);
  load();
}

function logActionFlow(path, action) {
  const meta = {
    rotate: { title: '轮转日志', label: '轮转', cls: 'btn-primary', msg: '会把当前内容压缩归档，然后把文件清空。' },
    clear: { title: '清空日志', label: '清空', cls: 'btn-danger', msg: '会立刻丢掉这个文件里的全部历史记录，无法恢复。' },
    delete: { title: '删除归档', label: '删除', cls: 'btn-danger', msg: '会永久删除这个归档文件。' },
  }[action];
  if (!meta) return;

  openModal({
    title: meta.title,
    body: `
      <div class="${action === 'rotate' ? 'warn-box' : 'danger-box'}">${meta.msg}</div>
      <div class="field" style="margin-top:14px">
        <label>文件</label>
        <div class="mono t-list">${esc(path)}</div>
      </div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn ${meta.cls}" id="log-do">${meta.label}</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#log-do').addEventListener('click', () => {
    closeModal();
    runStream(`/api/servers/${state.currentId}/logs/${action}`, { path }, meta.title, (evt) => {
      if (evt.ok) {
        state.maint.logs = null;
        renderLogsPane(true);
      }
    });
  });
}

/* ---------------------------------------------------------------- 维护 / 定时任务 */

const CRON_PRESETS = [
  { label: '每 5 分钟', value: '*/5 * * * *' },
  { label: '每 30 分钟', value: '*/30 * * * *' },
  { label: '每小时', value: '0 * * * *' },
  { label: '每天 3:30', value: '30 3 * * *' },
  { label: '每周一 4:00', value: '0 4 * * 1' },
  { label: '每月 1 日 5:00', value: '0 5 1 * *' },
  { label: '开机时', value: '@reboot' },
];

function cronHuman(schedule) {
  if (!schedule) return '';
  if (schedule.startsWith('@')) {
    const map = {
      '@reboot': '开机时执行一次',
      '@yearly': '每年 1 月 1 日 0:00',
      '@annually': '每年 1 月 1 日 0:00',
      '@monthly': '每月 1 日 0:00',
      '@weekly': '每周日 0:00',
      '@daily': '每天 0:00',
      '@midnight': '每天 0:00',
      '@hourly': '每小时整点',
    };
    return map[schedule] || schedule;
  }
  const p = schedule.split(/\s+/);
  if (p.length !== 5) return '自定义';
  const [m, h, dom, mon, dow] = p;
  if (m === '*' && h === '*' && dom === '*' && mon === '*' && dow === '*') return '每分钟';
  if (/^\*\/\d+$/.test(m) && h === '*' && dom === '*' && mon === '*' && dow === '*') return `每 ${m.slice(2)} 分钟`;
  if (m === '0' && /^\*\/\d+$/.test(h) && dom === '*' && mon === '*' && dow === '*') return `每 ${h.slice(2)} 小时`;
  const clock =
    /^\d+$/.test(m) && /^\d+$/.test(h) ? `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}` : null;
  if (clock && dom === '*' && mon === '*' && dow === '*') return `每天 ${clock}`;
  if (clock && dom === '*' && mon === '*' && /^\d$/.test(dow)) return `每周${'日一二三四五六'[Number(dow)]} ${clock}`;
  if (clock && /^\d+$/.test(dom) && mon === '*' && dow === '*') return `每月 ${dom} 日 ${clock}`;
  return '自定义周期';
}

async function renderCronPane(force) {
  const s = currentServer();
  const box = $('#maint-body');
  if (!box || !s) return;
  if (force || !state.maint.cron) {
    box.innerHTML = `<div class="muted"><span class="spinner"></span> 读取定时任务…</div>`;
    try {
      const r = await api(`/api/servers/${s.id}/cron`);
      state.maint.cron = r.status;
    } catch (err) {
      box.innerHTML = maintError(err.message, 'cron-reload');
      return;
    }
  }
  if (state.maint.tab !== 'cron' || !$('#maint-body')) return;
  box.innerHTML = cronView(state.maint.cron);
}

function cronView(d) {
  const entries = d.entries || [];
  const svc = d.service || 'none';
  const svcTag =
    svc === 'running'
      ? '<span class="tag ok">cron 运行中</span>'
      : svc === 'stopped'
        ? '<span class="tag warn">cron 已停止</span>'
        : '<span class="tag danger">未安装 cron</span>';

  const rows = entries
    .map(
      (e) => `<tr class="${e.enabled ? '' : 'is-off'}">
        <td>${e.enabled ? '<span class="tag ok">启用</span>' : '<span class="tag">停用</span>'}</td>
        <td>
          <div style="font-size:12.5px">${esc(cronHuman(e.schedule))}</div>
          <div class="mono" style="font-size:10.5px;color:var(--text-3)">${esc(e.schedule)}</div>
        </td>
        <td class="mono" style="font-size:11.5px;word-break:break-all">${esc(e.command)}</td>
        <td style="font-size:11.5px;color:var(--text-2)">${
          e.label ? esc(e.label) : '<span class="muted">—</span>'
        }</td>
        <td><div class="fops">
          <button class="btn btn-xs btn-ghost" data-act="cron-run" data-command="${esc(e.command)}">运行</button>
          <button class="btn btn-xs btn-ghost" data-act="cron-toggle" data-line="${e.line}" data-enabled="${
            e.enabled ? '1' : '0'
          }" data-command="${esc(e.command)}">${e.enabled ? '停用' : '启用'}</button>
          <button class="btn btn-xs btn-ghost" data-act="cron-delete" data-line="${e.line}" data-id="${esc(
            e.id,
          )}" data-command="${esc(e.command)}">删除</button>
        </div></td>
      </tr>`,
    )
    .join('');

  return `
    <div class="card">
      <div class="card-head">
        <div>
          <div class="card-title">定时任务</div>
          <div class="muted" style="font-size:11.5px">管理 root 的 crontab，共 ${entries.length} 条</div>
        </div>
        <div style="display:flex;gap:8px;align-items:center">
          ${svcTag}
          <button class="btn btn-sm" data-act="cron-reload">刷新</button>
          <button class="btn btn-sm btn-primary" data-act="cron-add">新建任务</button>
        </div>
      </div>
      ${
        !d.available
          ? `<div class="warn-box" style="margin-bottom:12px">
               系统里没有 crontab 命令，定时任务不会执行。
             </div>
             <button class="btn btn-sm" data-act="cron-install">安装 cron</button>`
          : entries.length
            ? `<table class="file-table">
                <thead><tr><th style="width:70px">状态</th><th style="width:180px">周期</th><th>命令</th><th style="width:140px">备注</th><th style="width:180px"></th></tr></thead>
                <tbody>${rows}</tbody>
              </table>`
            : `<div class="empty" style="padding:30px 0"><strong>还没有定时任务</strong>可以从「新建任务」里挑一个常用周期开始</div>`
      }
      <div class="note" style="margin-top:12px">
        cron 的环境变量比登录 shell 少得多：命令里的程序尽量写绝对路径（<code>/usr/bin/php</code>、<code>/usr/bin/mysqldump</code>），
        否则容易出现「手动能跑、定时不跑」。面板只碰自己创建的那几行，手写的任务原样保留。
      </div>
    </div>`;
}

function cronAddModal() {
  openModal({
    title: '新建定时任务',
    wide: true,
    body: `
      <div class="field">
        <label>执行周期</label>
        <input type="text" id="cron-sched" class="mono" value="30 3 * * *" spellcheck="false" autocomplete="off">
        <div class="cron-presets">
          ${CRON_PRESETS.map((p) => `<button type="button" class="cron-preset" data-sched="${esc(p.value)}">${p.label}</button>`).join('')}
        </div>
        <div class="note" id="cron-human">${esc(cronHuman('30 3 * * *'))}</div>
      </div>
      <div class="field">
        <label>命令</label>
        <textarea id="cron-cmd" rows="3" class="mono" spellcheck="false" placeholder="/usr/bin/php /var/www/example.com/cron.php >> /var/log/mycron.log 2>&1"></textarea>
        <div class="note">支持 <code>cd /path &amp;&amp; ...</code> 这类写法；输出不重定向的话会进入 root 的本地邮件。</div>
      </div>
      <div class="field">
        <label>备注（可选）</label>
        <input type="text" id="cron-label" placeholder="例如 每天备份数据库" autocomplete="off">
      </div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-primary" id="cron-save">创建</button>`,
  });

  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  const sched = $('#cron-sched');
  const human = $('#cron-human');
  const sync = () => {
    human.textContent = cronHuman(sched.value.trim());
  };
  sched.addEventListener('input', sync);
  sched.addEventListener('change', sync);
  $('#modal').querySelectorAll('.cron-preset').forEach((b) => {
    b.addEventListener('click', () => {
      sched.value = b.dataset.sched;
      sync();
    });
  });

  $('#cron-save').addEventListener('click', () => {
    const schedule = sched.value.trim();
    const command = $('#cron-cmd').value.trim();
    const label = $('#cron-label').value.trim();
    if (!schedule) return toast('执行周期不能为空', 'err');
    if (!command) return toast('命令不能为空', 'err');
    closeModal();
    runStream(`/api/servers/${state.currentId}/cron/add`, { schedule, command, label }, '新增定时任务', (evt) => {
      if (evt.ok) {
        state.maint.cron = null;
        renderCronPane(true);
      }
    });
  });
}

function cronDeleteFlow(line, id, command) {
  openModal({
    title: '删除定时任务',
    body: `
      <div class="danger-box">这一行会从 crontab 里移除，之后不再按周期执行。</div>
      <div class="field" style="margin-top:14px">
        <label>命令</label>
        <div class="mono t-list">${esc(command)}</div>
      </div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-danger" id="cron-del">删除</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  $('#cron-del').addEventListener('click', () => {
    closeModal();
    runStream(`/api/servers/${state.currentId}/cron/delete`, { line, id }, '删除定时任务', (evt) => {
      if (evt.ok) {
        state.maint.cron = null;
        renderCronPane(true);
      }
    });
  });
}

function cronToggleFlow(line, enabled) {
  runStream(
    `/api/servers/${state.currentId}/cron/toggle`,
    { line, enabled: !enabled },
    enabled ? '停用定时任务' : '启用定时任务',
    (evt) => {
      if (evt.ok) {
        state.maint.cron = null;
        renderCronPane(true);
      }
    },
  );
}

function cronRunFlow(command) {
  runStream(`/api/servers/${state.currentId}/cron/run`, { command }, '立即执行任务');
}

function cronInstallFlow() {
  runStream(`/api/servers/${state.currentId}/exec`, { command: 'apt-get update -qq && apt-get install -y cron', seconds: 300 }, '安装 cron', (evt) => {
    if (evt.ok) {
      state.maint.cron = null;
      renderCronPane(true);
    }
  });
}

/* ---------------------------------------------------------------- 维护 / 执行命令 */

// 快捷命令按当前 Web 服务器切换，Apache 机器上别给用户 nginx 的命令
function execQuick() {
  const apache = isApache();
  return [
    { label: '磁盘占用', cmd: 'df -h' },
    { label: '内存', cmd: 'free -m' },
    { label: '负载与进程', cmd: 'uptime && ps aux --sort=-%mem | head -12' },
    apache
      ? { label: 'apache 状态', cmd: 'systemctl status apache2 --no-pager -l | head -20' }
      : { label: 'nginx 状态', cmd: 'systemctl status nginx --no-pager -l | head -20' },
    apache
      ? { label: '重启 apache', cmd: 'apache2ctl configtest && systemctl reload apache2' }
      : { label: '重启 nginx', cmd: 'nginx -t && systemctl reload nginx' },
    { label: 'PHP-FPM 服务', cmd: 'systemctl list-units "php*-fpm" --no-pager' },
    { label: '可升级软件', cmd: 'apt list --upgradable 2>/dev/null | head -30' },
    { label: '最近登录', cmd: 'last -n 10' },
  ];
}

const EXEC_DANGER = [
  { re: /\brm\s+-[a-zA-Z]*[rR][a-zA-Z]*\s+\//, label: '递归删除目录（rm -r /…）' },
  { re: /\bmkfs(\.[a-z0-9]+)?\b/, label: '格式化文件系统' },
  { re: /\bdd\b[^\n]*\bof=\/dev\//, label: '直接写裸设备' },
  { re: /\b(shutdown|reboot|halt|poweroff)\b/, label: '关机或重启服务器' },
  { re: /\bcrontab\s+-r\b/, label: '清空所有定时任务' },
  { re: /\bDROP\s+(DATABASE|TABLE|SCHEMA)\b/i, label: '删除数据库对象' },
  { re: />>?\s*\/dev\/sd[a-z]/, label: '覆盖磁盘设备' },
  { re: /\b(userdel|groupdel)\b/, label: '删除系统账号' },
  { re: /\bchmod\s+-R\s+777\b/, label: '递归放开全部权限' },
];

function execDanger(cmd) {
  for (const rule of EXEC_DANGER) {
    if (rule.re.test(cmd)) return rule;
  }
  return null;
}

function renderExecPane() {
  const ex = state.maint.exec;
  const box = $('#maint-body');
  if (!box) return;
  box.innerHTML = `
    <div class="card">
      <div class="card-head">
        <div>
          <div class="card-title">执行命令</div>
          <div class="muted" style="font-size:11.5px">以 root 身份在远端执行，输出实时回显并记入操作记录</div>
        </div>
        <span class="tag danger">谨慎操作</span>
      </div>
      <div class="exec-quick">
        ${execQuick().map((q) => `<button class="exec-chip" data-act="exec-quick" data-cmd="${esc(q.cmd)}">${q.label}</button>`).join('')}
      </div>
      <textarea id="exec-cmd" class="exec-input mono" rows="4" spellcheck="false" placeholder="df -h">${esc(ex.cmd)}</textarea>
      <div class="exec-bar">
        <div class="field" style="margin:0;width:240px">
          <label>工作目录</label>
          <input type="text" id="exec-cwd" class="mono" value="${esc(ex.cwd)}" placeholder="留空则为 /root" spellcheck="false" style="font-size:12px">
        </div>
        <div class="field" style="margin:0;width:130px">
          <label>超时</label>
          <select id="exec-sec">
            <option value="30" ${ex.seconds === 30 ? 'selected' : ''}>30 秒</option>
            <option value="120" ${ex.seconds === 120 ? 'selected' : ''}>2 分钟</option>
            <option value="300" ${ex.seconds === 300 ? 'selected' : ''}>5 分钟</option>
            <option value="600" ${ex.seconds === 600 ? 'selected' : ''}>10 分钟</option>
          </select>
        </div>
        <div style="margin-left:auto;display:flex;gap:6px">
          <button class="btn btn-sm btn-ghost" data-act="exec-clear-out">清空输出</button>
          <button class="btn btn-sm btn-primary" data-act="exec-run" ${ex.running ? 'disabled' : ''}>运行</button>
        </div>
      </div>
      <pre class="exec-out" id="exec-out">${esc(ex.out) || '（还没有输出）'}</pre>
      <div class="note" style="margin-top:10px">
        支持管道、重定向与多条命令；命令和输出会写进「操作记录」。删库、格式化、关机这类命令会要求二次确认。
      </div>
    </div>`;
}

async function execRun() {
  const ex = state.maint.exec;
  const cmd = ($('#exec-cmd')?.value || '').trim();
  if (!cmd) return toast('先写点什么再运行', 'err');
  if (ex.running) return;
  const hit = execDanger(cmd);
  if (hit) return execDangerConfirm(hit, cmd);
  return execStart(cmd);
}

function execDangerConfirm(hit, cmd) {
  openModal({
    title: '这条命令会改动系统',
    body: `
      <div class="danger-box">识别到「${esc(hit.label)}」。执行后可能无法回滚，请确认这是你要做的。</div>
      <div class="field" style="margin-top:14px">
        <label>命令</label>
        <div class="mono t-list" style="max-height:none;white-space:pre-wrap">${esc(cmd)}</div>
      </div>
      <div class="field">
        <label>输入「确认执行」继续</label>
        <input type="text" id="exec-ok" placeholder="确认执行" autocomplete="off">
      </div>`,
    footer: `<button class="btn" data-close>取消</button>
             <button class="btn btn-danger" id="exec-go" disabled>执行</button>`,
  });
  $('#modal').querySelector('[data-close]').addEventListener('click', closeModal);
  const input = $('#exec-ok');
  const go = $('#exec-go');
  input.addEventListener('input', () => {
    go.disabled = input.value.trim() !== '确认执行';
  });
  go.addEventListener('click', () => {
    closeModal();
    execStart(cmd);
  });
}

async function execStart(cmd) {
  const ex = state.maint.exec;
  ex.cmd = cmd;
  ex.cwd = ($('#exec-cwd')?.value || '').trim();
  ex.seconds = Number($('#exec-sec')?.value || 120);
  ex.out = '';
  ex.running = true;
  renderExecPane();

  const out = $('#exec-out');
  const append = (text) => {
    ex.out += text;
    const el = $('#exec-out');
    if (el) {
      el.textContent = ex.out;
      el.scrollTop = el.scrollHeight;
    }
  };
  if (out) out.textContent = '';
  append(`$ ${cmd}\n\n`);

  await streamPost(
    `/api/servers/${state.currentId}/exec`,
    { command: cmd, cwd: ex.cwd, seconds: ex.seconds },
    {
      out: append,
      end: (evt) => {
        const secs = evt.durationMs ? `，${(evt.durationMs / 1000).toFixed(1)}s` : '';
        append(`\n[${evt.ok ? '完成' : '失败'}：${evt.message}${secs}]\n`);
      },
      error: (msg) => append(`\n[错误] ${msg}\n`),
    },
  );

  ex.running = false;
  const btn = view().querySelector('[data-act="exec-run"]');
  if (btn) btn.disabled = false;
}

view().addEventListener('change', (e) => {
  if (e.target.id === 'log-filter') {
    state.maint.logFilter = e.target.value;
    const box = $('#maint-body');
    if (box && state.maint.logs) box.innerHTML = logsView(state.maint.logs, state.maint.logFilter);
  }
});

/* ---------------------------------------------------------------- 主题 */

function syncThemeButton() {
  const dark = document.documentElement.dataset.theme === 'dark';
  const btn = $('#btn-theme');
  if (btn) {
    btn.textContent = dark ? '日间模式' : '夜间模式';
    btn.title = dark ? '切回浅色' : '切到深色底浅色字';
  }
}

function applyTheme(mode, persist = true) {
  const dark = mode === 'dark';
  document.documentElement.dataset.theme = dark ? 'dark' : 'light';
  syncThemeButton();
  if (persist) {
    try {
      localStorage.setItem('wpqu-theme', dark ? 'dark' : 'light');
    } catch (e) {}
  }
}

$('#btn-theme').addEventListener('click', () => {
  applyTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
});

// 语言按钮：按 LANGS 顺序循环，切完 reload 让整页按新语言重渲染
const btnLang = $('#btn-lang');
if (btnLang) {
  btnLang.addEventListener('click', cycleLang);
  // 侧栏按钮上只放语种简写，完整名字放 title
  if (currentLang() !== 'zh-CN') btnLang.title = 'Switch language / 切换语言';
}

// 没手动选过时跟随系统
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', (e) => {
  let saved = null;
  try {
    saved = localStorage.getItem('wpqu-theme');
  } catch (err) {}
  if (!saved) applyTheme(e.matches ? 'dark' : 'light', false);
});

syncThemeButton();

/* ---------------------------------------------------------------- 插件市场 */

const PLUGIN_CATS = [
  { key: 'all', label: '全部' },
  { key: 'runtime', label: '运行时' },
  { key: 'ai', label: 'AI 应用' },
  { key: 'dev', label: '开发工具' },
  { key: 'ops', label: '运维' },
  { key: 'panel', label: '面板增强' },
];

// 插件文案是多语言 map，界面切到哪种语言就取哪种，缺了退回中文
function pickLang(map) {
  if (!map) return '';
  return map[currentLang()] || map['zh-CN'] || map['zh'] || map['en'] || Object.values(map)[0] || '';
}

function pluginById(id) {
  return state.plugins.catalog.find((p) => p.id === id) || null;
}

function pluginStatus(id) {
  const s = state.plugins.status[state.currentId];
  return (s && s[id]) || { installed: false, version: '' };
}

function catLabel(key) {
  const c = PLUGIN_CATS.find((x) => x.key === key);
  return c ? c.label : key;
}

async function loadPlugins(force = false) {
  const st = state.plugins;
  if (st.loading) return;
  if (st.loaded && !force) return;
  st.loading = true;
  try {
    const d = await api('/api/plugins');
    st.catalog = d.catalog || [];
    st.installed = d.installed || {};
    st.licenses = d.licenses || {};
    st.uiTabs = d.uiTabs || [];
    st.error = d.error || '';
    st.source = d.source || '';
    st.updated = d.updated || '';
    st.loaded = true;
    syncPluginTabs();
  } catch (e) {
    st.error = e.message;
    st.loaded = true; // 失败也只提示一次，不要每次进页面都重试
  } finally {
    st.loading = false;
  }
}

function syncPluginTabs() {
  const tabs = $('#tabs');
  tabs.querySelectorAll('.tab.plugin-tab').forEach((el) => el.remove());
  state.plugins.uiTabs.forEach((t) => {
    const b = document.createElement('button');
    b.className = 'tab plugin-tab';
    b.dataset.tab = 'plugin:' + t.id;
    b.textContent = (t.icon ? t.icon + ' ' : '') + (pickLang(t.label) || t.id);
    tabs.appendChild(b);
  });
}

function filteredPlugins() {
  const st = state.plugins;
  const q = st.q.trim().toLowerCase();
  return st.catalog.filter((p) => {
    if (st.cat !== 'all' && p.category !== st.cat) return false;
    if (st.tier !== 'all' && p.tier !== st.tier) return false;
    if (!q) return true;
    const hl = p.highlights ? Object.values(p.highlights).flat() : [];
    const hay = [pickLang(p.name), pickLang(p.summary), p.id, ...hl].join(' ').toLowerCase();
    return hay.includes(q);
  });
}

function pluginCard(mf) {
  const inst = state.plugins.installed[mf.id];
  const sv = pluginStatus(mf.id);
  const paid = mf.tier === 'paid';
  const badge = paid
    ? `<span class="pbadge paid">收费 · ¥${mf.priceCny || 0}</span>`
    : `<span class="pbadge free">免费</span>`;
  let stateText = '';
  if (sv.installed) stateText = `已安装${sv.version ? ' ' + sv.version : ''}`;
  else if (inst) stateText = '已下载';
  return `<article class="pcard" data-plugin="${esc(mf.id)}">
    <div class="pcard-top">
      <div class="picon" style="--pin:${esc(mf.accent || '#2563eb')}">${esc(mf.icon || (mf.id[0] || '?').toUpperCase())}</div>
      <div class="pcard-main">
        <div class="pname">${esc(pickLang(mf.name))}<span class="pver">v${esc(mf.version)}</span></div>
        <div class="psum">${esc(pickLang(mf.summary))}</div>
      </div>
    </div>
    <div class="pcard-foot">
      ${badge}
      <span class="pstate">${esc(stateText)}</span>
      <button class="btn btn-xs" data-act="plugin-detail" data-id="${esc(mf.id)}">详情</button>
    </div>
  </article>`;
}

function renderPlugins() {
  const st = state.plugins;
  if (!st.loaded) {
    loadPlugins().then(() => {
      if (state.tab === 'plugins') renderPlugins();
    });
    view().innerHTML = `<div class="empty"><strong>正在加载插件市场…</strong>
      首次进入需要连接 ${esc(st.source || 'panel.wpqu.com')}</div>`;
    return;
  }

  const list = filteredPlugins();
  const cats = PLUGIN_CATS.filter((c) => c.key === 'all' || st.catalog.some((p) => p.category === c.key));
  const noServer = !state.currentId;

  view().innerHTML = `
    <div class="phead">
      <div class="phead-row">
        <div>
          <h2 class="ptitle">插件</h2>
          <p class="psub">给服务器一键安装运行时与工具。安装脚本会在目标服务器上以 root 执行。</p>
        </div>
        <div class="phead-tools">
          <input class="input" id="plugin-q" placeholder="搜索插件" value="${esc(st.q)}">
          <button class="btn btn-ghost btn-sm" id="plugin-refresh">刷新市场</button>
        </div>
      </div>
      <div class="chips">
        ${cats.map((c) => `<button class="chip ${st.cat === c.key ? 'is-on' : ''}" data-cat="${c.key}">${c.label}</button>`).join('')}
        <span class="chip-sep"></span>
        <button class="chip ${st.tier === 'all' ? 'is-on' : ''}" data-tier="all">不限</button>
        <button class="chip ${st.tier === 'free' ? 'is-on' : ''}" data-tier="free">免费</button>
        <button class="chip ${st.tier === 'paid' ? 'is-on' : ''}" data-tier="paid">收费</button>
      </div>
    </div>
    ${st.error ? `<div class="notice warn">连不上插件市场：${esc(st.error)}<br><span class="hint">已安装的插件仍可正常卸载；联网后点「刷新市场」重试。</span></div>` : ''}
    ${noServer ? `<div class="notice">还没选择服务器。可以先浏览插件，安装需要先在左侧添加并选中一台服务器。</div>` : ''}
    <div class="pgrid">
      ${list.length ? list.map(pluginCard).join('') : `<div class="empty" style="grid-column:1/-1"><strong>没有匹配的插件</strong>换个关键词或分类试试</div>`}
    </div>
    <div class="pfoot hint">市场地址 ${esc(st.source || '—')}${st.updated ? ' · 更新于 ' + esc(st.updated) : ''}</div>`;

  $('#plugin-q').addEventListener('input', (e) => {
    state.plugins.q = e.target.value;
    const pos = e.target.selectionStart;
    renderPlugins();
    const q = $('#plugin-q');
    if (q) {
      q.focus();
      q.setSelectionRange(pos, pos);
    }
  });
  $('#plugin-refresh').addEventListener('click', () => {
    state.plugins.loaded = false;
    state.plugins.uiTabs = [];
    renderPlugins();
  });
  view().querySelectorAll('[data-cat]').forEach((el) =>
    el.addEventListener('click', () => {
      state.plugins.cat = el.dataset.cat;
      renderPlugins();
    })
  );
  view().querySelectorAll('[data-tier]').forEach((el) =>
    el.addEventListener('click', () => {
      state.plugins.tier = el.dataset.tier;
      renderPlugins();
    })
  );
  view().querySelectorAll('[data-act="plugin-detail"]').forEach((el) =>
    el.addEventListener('click', (e) => {
      e.stopPropagation();
      pluginDetail(el.dataset.id);
    })
  );

  if (state.currentId && state.plugins.statusFor !== state.currentId) {
    refreshPluginStatus();
  }
}

async function refreshPluginStatus() {
  if (!state.currentId) return;
  const sid = state.currentId;
  try {
    const d = await api(`/api/servers/${sid}/plugins/status`);
    state.plugins.status[sid] = d.status || {};
    state.plugins.statusFor = sid;
    if (state.tab === 'plugins') renderPlugins();
  } catch (e) {
    // 状态检测失败不该打断浏览，静默处理
  }
}

function pluginDetail(id) {
  const mf = pluginById(id);
  if (!mf) return;
  const paid = mf.tier === 'paid';
  const lic = state.plugins.licenses[id];
  const sv = pluginStatus(id);
  const inst = state.plugins.installed[id];
  const osList = (mf.requires && mf.requires.os) || [];
  const params = (mf.install && mf.install.params) || [];
  const highlights = pickLangList(mf.highlights);

  let btn = '';
  if (mf.ui && !mf.install) {
    // 界面类插件：装在面板本地，与服务器无关
    btn = inst
      ? `<button class="btn btn-danger" id="p-remove-local">从面板移除</button>`
      : `<button class="btn btn-primary" id="p-add-panel">安装到面板</button>`;
  } else if (!state.currentId) {
    btn = `<button class="btn btn-primary" disabled>先选择服务器</button>`;
  } else if (paid && !lic) {
    btn = `<button class="btn btn-primary" disabled>需要授权码</button>`;
  } else if (sv.installed) {
    btn = `<button class="btn btn-danger" id="p-uninstall">卸载</button>
           <button class="btn" id="p-reinstall">重新安装</button>`;
  } else {
    btn = `<button class="btn btn-primary" id="p-install">安装到服务器</button>`;
  }

  const modal = openModal({
    title: pickLang(mf.name),
    subtitle: `v${mf.version} · ${pickLang(mf.summary)}`,
    wide: true,
    body: `<div class="pdetail">
      <p class="pdesc">${esc(pickLang(mf.description) || pickLang(mf.summary))}</p>
      ${highlights.length ? `<ul class="phl">${highlights.map((h) => `<li>${esc(h)}</li>`).join('')}</ul>` : ''}
      <div class="pkv">
        <div><span>分类</span><b>${esc(catLabel(mf.category))}</b></div>
        <div><span>价格</span><b>${paid ? '¥' + (mf.priceCny || 0) : '免费'}</b></div>
        <div><span>支持系统</span><b>${esc(osList.length ? osList.join(' / ') : '不限')}</b></div>
        <div><span>本地状态</span><b>${inst ? '已下载 v' + esc(inst.version) : '未下载'}</b></div>
      </div>
      ${params.length ? `<div class="pblock"><h4>安装参数</h4>${params
        .map(
          (p) => `<label class="pfield">
            <span>${esc(pickLang(p.label) || p.key)}</span>
            <input class="input" data-param="${esc(p.key)}" value="${esc(p.default || '')}" placeholder="${esc(pickLang(p.hint) || '')}">
          </label>`
        )
        .join('')}</div>` : ''}
      ${paid ? `<div class="pblock"><h4>授权码</h4>
        ${lic
          ? `<div class="plic ok">已授权${lic.name ? ' · ' + esc(lic.name) : ''} · 有效期至 ${esc(lic.expire)}</div>
             <button class="btn btn-xs btn-ghost" id="p-clear-lic">清除授权码</button>`
          : `<div class="plic">这是收费插件。购买后把授权码粘贴到下面，本机会离线校验，不上传任何信息。</div>
             <div class="row"><input class="input" id="p-lic-input" placeholder="WPQU1.xxxxx.yyyyy">
             <button class="btn btn-sm" id="p-lic-save">保存授权码</button></div>`}
      </div>` : ''}
      <div class="notice warn">安装脚本会在目标服务器上以 root 执行。只安装你信任的插件。</div>
    </div>`,
    footer: `<button class="btn" id="p-close">关闭</button>${btn}`,
  });

  modal.querySelector('#p-close').addEventListener('click', closeModal);
  const saveLic = modal.querySelector('#p-lic-save');
  if (saveLic) {
    saveLic.addEventListener('click', async () => {
      const key = modal.querySelector('#p-lic-input').value.trim();
      if (!key) return toast('请先粘贴授权码', 'err');
      try {
        const d = await api(`/api/plugins/${id}/license`, {
          method: 'POST',
          body: JSON.stringify({ key }),
        });
        state.plugins.licenses[id] = { name: d.name, expire: d.expire, seats: d.seats };
        toast('授权码已保存');
        closeModal();
        pluginDetail(id);
      } catch (e) {
        toast(e.message, 'err');
      }
    });
  }
  const clearLic = modal.querySelector('#p-clear-lic');
  if (clearLic) {
    clearLic.addEventListener('click', async () => {
      await api(`/api/plugins/${id}/license`, { method: 'DELETE' }).catch(() => {});
      delete state.plugins.licenses[id];
      closeModal();
      renderPlugins();
    });
  }

  const installBtn = modal.querySelector('#p-install') || modal.querySelector('#p-reinstall');
  if (installBtn) {
    installBtn.addEventListener('click', () => {
      const vals = {};
      modal.querySelectorAll('[data-param]').forEach((el) => {
        vals[el.dataset.param] = el.value;
      });
      closeModal();
      installPlugin(mf, vals);
    });
  }
  const unBtn = modal.querySelector('#p-uninstall');
  if (unBtn) {
    unBtn.addEventListener('click', () => {
      closeModal();
      uninstallPlugin(mf);
    });
  }

  const addLocal = modal.querySelector('#p-add-panel');
  if (addLocal) {
    addLocal.addEventListener('click', async () => {
      addLocal.disabled = true;
      try {
        await api(`/api/plugins/${id}/install`, { method: 'POST', body: '{}' });
        closeModal();
        await loadPlugins(true);
        toast('已安装到面板，页签栏已更新');
        renderPlugins();
      } catch (e) {
        addLocal.disabled = false;
        toast(e.message, 'err');
      }
    });
  }
  const rmLocal = modal.querySelector('#p-remove-local');
  if (rmLocal) {
    rmLocal.addEventListener('click', async () => {
      rmLocal.disabled = true;
      try {
        await api(`/api/plugins/${id}`, { method: 'DELETE' });
        if (state.tab === 'plugin:' + id) state.tab = 'plugins';
        closeModal();
        await loadPlugins(true);
        renderPlugins();
      } catch (e) {
        rmLocal.disabled = false;
        toast(e.message, 'err');
      }
    });
  }
}

function pickLangList(map) {
  if (!map) return [];
  const v = map[currentLang()] || map['zh-CN'] || map['en'];
  if (Array.isArray(v)) return v;
  const first = Object.values(map).find((x) => Array.isArray(x));
  return first || [];
}

function installPlugin(mf, params) {
  const name = pickLang(mf.name);
  const url = `/api/servers/${state.currentId}/plugins/${mf.id}/install`;
  let ok = false;
  openDrawer(`安装 ${name}`);
  streamPost(url, { params }, {
    start: (msg) => drawerAppend(`▸ ${msg}\n\n`),
    step: (e) => drawerAppend(`\n▸ [${e.index}/${e.total}] ${e.title}\n`),
    out: (text) => drawerAppend(text),
    end: (evt) => {
      ok = true;
      drawerDone(true, `${evt.message}（${((evt.durationMs || 0) / 1000).toFixed(1)}s）`);
    },
    error: (msg) => {
      ok = false;
      drawerAppend(`\n错误：${msg}\n`);
      drawerDone(false, msg);
    },
  }).then(() => {
    if (ok) {
      toast(`${name} 安装完成`);
      delete state.plugins.status[state.currentId];
      refreshPluginStatus();
    }
  });
}

function uninstallPlugin(mf) {
  const name = pickLang(mf.name);
  openModal({
    title: '卸载 ' + name,
    subtitle: '只移除服务器上的软件，面板里的插件包会保留',
    body: `<p>将在目标服务器上执行该插件自带的卸载脚本。数据库和站点文件不受影响。</p>`,
    footer: `<button class="btn" id="u-cancel">取消</button>
             <button class="btn btn-danger" id="u-ok">确认卸载</button>`,
  });
  $('#u-cancel').addEventListener('click', closeModal);
  $('#u-ok').addEventListener('click', () => {
    closeModal();
    let ok = false;
    openDrawer(`卸载 ${name}`);
    streamPost(`/api/servers/${state.currentId}/plugins/${mf.id}/uninstall`, {}, {
      start: (msg) => drawerAppend(`▸ ${msg}\n\n`),
      step: (e) => drawerAppend(`\n▸ [${e.index}/${e.total}] ${e.title}\n`),
      out: (text) => drawerAppend(text),
      end: (evt) => {
        ok = true;
        drawerDone(true, evt.message);
      },
      error: (msg) => drawerAppend(`\n错误：${msg}\n`),
    }).then(() => {
      if (ok) {
        toast(`${name} 已卸载`);
        delete state.plugins.status[state.currentId];
        refreshPluginStatus();
      }
    });
  });
}

// UI 插件：把插件自带的 panel.js 挂进当前视图
const pluginModules = new Map();

async function renderPluginMount(id) {
  const t = state.plugins.uiTabs.find((x) => x.id === id);
  if (!t) {
    view().innerHTML = `<div class="empty"><strong>插件未安装</strong>先到「插件」页装上它</div>`;
    return;
  }
  view().innerHTML = `<div id="plugin-root" class="plugin-root"></div>`;
  try {
    let mod = pluginModules.get(id);
    if (!mod) {
      mod = await import(`${t.entry}?v=${Date.now()}`);
      pluginModules.set(id, mod);
    }
    if (typeof mod.mount !== 'function') {
      view().innerHTML = `<div class="notice warn">插件没有导出 mount 函数</div>`;
      return;
    }
    await mod.mount($('#plugin-root'), {
      api,
      toast,
      esc,
      server: currentServer(),
      lang: currentLang(),
      pickLang,
    });
  } catch (e) {
    view().innerHTML = `<div class="card" style="color:var(--danger)">插件界面加载失败：${esc(e.message)}</div>`;
  }
}

/* ---------------------------------------------------------------- 版本与更新 */

async function initVersion() {
  try {
    const r = await fetch('/api/ping', { cache: 'no-store' });
    const d = await r.json();
    state.version = d.version || '';
    const btn = $('#btn-version');
    if (btn) btn.textContent = 'v' + state.version;
  } catch (e) {
    /* 拿不到版本不影响使用 */
  }
  loadPlugins();
}

function aboutModal() {
  const modal = openModal({
    title: '关于与更新',
    subtitle: `WPQU panel v${state.version}`,
    body: `<div class="pkv">
        <div><span>当前版本</span><b>v${esc(state.version)}</b></div>
        <div><span>更新源</span><b>${esc(state.plugins.source || 'panel.wpqu.com')}</b></div>
        <div><span>数据目录</span><b>本机 data/</b></div>
        <div><span>安装包校验</span><b>Ed25519 签名 + SHA256</b></div>
      </div>
      <div id="update-result" class="upd-box">
        <p class="hint">面板不会自动联网。点「检查更新」才会连接更新源，升级包在替换前会先校验签名与哈希。</p>
      </div>`,
    footer: `<button class="btn" id="upd-close">关闭</button>
             <button class="btn btn-primary" id="upd-check">检查更新</button>`,
  });
  modal.querySelector('#upd-close').addEventListener('click', closeModal);
  modal.querySelector('#upd-check').addEventListener('click', () => checkUpdate(modal));
}

async function checkUpdate(modal) {
  const box = modal.querySelector('#update-result');
  box.innerHTML = `<p class="hint">正在连接更新源…</p>`;
  let d;
  try {
    d = await api('/api/update/check');
  } catch (e) {
    box.innerHTML = `<div class="notice warn">检查失败：${esc(e.message)}</div>`;
    return;
  }
  if (d.error) {
    box.innerHTML = `<div class="notice warn">检查失败：${esc(d.error)}</div>`;
    return;
  }
  const r = d.result || {};
  if (!r.available) {
    box.innerHTML = `<div class="notice">已是最新版本（v${esc(d.version)}）。</div>`;
    return;
  }
  const notes = (r.notes && (r.notes[currentLang()] || r.notes['zh-CN'] || r.notes.en)) || [];
  const size = r.size ? (r.size / 1048576).toFixed(1) + ' MB' : '';
  const can = d.canSelfUpdate;
  box.innerHTML = `<div class="upd-new">
      <div class="upd-head">
        <b>发现新版本 v${esc(r.latest)}</b>
        <span class="hint">${esc(r.date || '')}${size ? ' · ' + size : ''}</span>
        ${r.critical ? '<span class="tag warn">重要更新</span>' : ''}
      </div>
      ${notes.length ? `<ul class="phl">${notes.map((n) => `<li>${esc(n)}</li>`).join('')}</ul>` : ''}
      ${r.skipped ? `<div class="notice warn">${esc(r.skipped)}</div>` : ''}
      ${can ? '' : `<div class="notice warn">${esc(d.reason || '当前环境不支持自动替换')}${r.downloadUrl ? `<br><a href="${esc(r.downloadUrl)}" target="_blank" rel="noreferrer">手动下载安装包</a>` : ''}</div>`}
    </div>`;
  if (can) {
    const btn = document.createElement('button');
    btn.className = 'btn btn-primary';
    btn.textContent = `立即升级到 v${r.latest}`;
    btn.addEventListener('click', () => applyUpdate(r.latest));
    box.appendChild(btn);
  }
}

async function applyUpdate(latest) {
  closeModal();
  openDrawer(`升级到 v${latest}`);
  let ok = false;
  await streamPost('/api/update/apply', {}, {
    start: (msg) => drawerAppend(`▸ ${msg}\n\n`),
    progress: (e) => {
      const pct = Math.round((e.pct || 0) * 100);
      $('#drawer-foot').innerHTML = `<span>${esc(e.message)} ${pct}%</span>`;
    },
    end: (evt) => {
      ok = true;
      drawerAppend(`\n▸ ${evt.message}\n`);
      drawerDone(true, '正在重启…');
    },
    error: (msg) => {
      drawerAppend(`\n错误：${msg}\n`);
      drawerDone(false, msg);
    },
  });
  if (ok) waitForRestart();
}

// 升级后进程会退出再拉起，轮询 ping 直到服务回来，然后刷新页面
async function waitForRestart() {
  for (let i = 0; i < 120; i++) {
    await new Promise((r) => setTimeout(r, 1000));
    try {
      const r = await fetch('/api/ping', { cache: 'no-store' });
      if (r.ok) {
        location.reload();
        return;
      }
    } catch (e) {
      /* 还没起来 */
    }
  }
  drawerDone(false, '面板没有在预期时间内重启，请手动启动程序');
}

$('#btn-version').addEventListener('click', aboutModal);
initVersion();

/* ---------------------------------------------------------------- 启动 */

$('#drawer-close').addEventListener('click', () => {
  $('#drawer').classList.add('hidden');
});

async function boot() {
  try {
    await loadServers();
    if (!state.servers.length) {
      renderTopbar();
      view().innerHTML = `<div class="empty">
        <strong>还没有服务器</strong>
        点击左侧「添加」，填写服务器 IP 与 root 密码或私钥<br>
        <span style="font-size:11.5px">凭据会加密保存在本机 panel/data</span>
      </div>`;
    }
  } catch (err) {
    view().innerHTML = `<div class="card" style="color:var(--danger)">面板接口异常：${esc(err.message)}</div>`;
  }
}

boot();
