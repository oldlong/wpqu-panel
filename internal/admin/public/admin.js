'use strict';

/* ---------------------------------------------------------------- 基础 */

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

const S = {
  me: null,
  route: 'overview',
  param: '',
  plugins: [],
  plugin: null,
  root: 'site',
  tree: [],
  file: null,
  licenses: null,
  filters: { q: '', product: '', status: '' },
  updates: null,
  lastCodes: [],
};

const TITLES = {
  overview: '概览', plugins: '插件市场', site: '官网内容',
  licenses: '授权码', updates: '升级发布', audit: '审计日志', password: '修改口令',
};

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

function fmtBytes(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1048576).toFixed(1) + ' MB';
}

function fmtDate(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d)) return iso;
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

let toastTimer = 0;
function toast(msg, kind) {
  const el = $('#toast');
  el.textContent = msg;
  el.className = 'toast show' + (kind ? ' ' + kind : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.className = 'toast'; }, 3200);
}

async function api(path, body, method) {
  const opt = { credentials: 'same-origin', headers: {} };
  if (body !== undefined) {
    opt.method = method || 'POST';
    opt.headers['Content-Type'] = 'application/json';
    opt.headers['X-Admin'] = '1';
    opt.body = JSON.stringify(body);
  } else if (method && method !== 'GET') {
    opt.method = method;
    opt.headers['X-Admin'] = '1';
  }
  const r = await fetch(path, opt);
  const ct = r.headers.get('content-type') || '';
  const data = ct.includes('json') ? await r.json() : await r.text();
  if (r.status === 401) {
    showLogin('登录已过期，请重新登录');
    throw new Error('未登录');
  }
  if (!r.ok) throw new Error((data && data.error) || ('请求失败（' + r.status + '）'));
  return data;
}

function copy(text) {
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(text).then(() => toast('已复制', 'ok'))
      .catch(() => fallbackCopy(text));
  }
  return Promise.resolve(fallbackCopy(text));
}
function fallbackCopy(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand('copy'); toast('已复制', 'ok'); }
  catch (e) { toast('复制失败，请手动选中', 'err'); }
  document.body.removeChild(ta);
}

function badge(text, cls) {
  return `<span class="badge ${cls || ''}">${esc(text)}</span>`;
}

// 插件清单里的名称是多语言对象
function localName(name) {
  if (!name) return '';
  if (typeof name === 'string') return name;
  return name['zh-CN'] || name.zh || name.en || Object.values(name)[0] || '';
}

function driftBadge(drift) {
  switch (drift) {
    case 'unpublished': return badge('未发布', 'warn');
    case 'version': return badge('有新版本', 'accent');
    case 'content': return badge('内容有变', 'warn');
    case 'broken': return badge('清单损坏', 'err');
    default: return badge('已同步', 'ok');
  }
}

function licBadge(status) {
  const map = { '有效': 'ok', '永久': 'accent', '即将到期': 'warn', '已过期': 'err', '已作废': '' };
  return badge(status, map[status] || '');
}

/* ---------------------------------------------------------------- 登录 */

function showLogin(msg) {
  $('#app').classList.add('hidden');
  $('#login').classList.remove('hidden');
  $('#login-err').textContent = msg || '';
  const pw = $('#login-pw');
  pw.value = '';
  setTimeout(() => pw.focus(), 60);
}

function showApp() {
  $('#login').classList.add('hidden');
  $('#app').classList.remove('hidden');
}

async function doLogin(e) {
  e.preventDefault();
  const btn = $('#login-btn');
  const pw = $('#login-pw').value;
  if (!pw) return;
  btn.disabled = true;
  btn.textContent = '登录中…';
  try {
    await api('/api/login', { password: pw }, 'POST');
    showApp();
    await boot();
  } catch (err) {
    $('#login-err').textContent = err.message;
  } finally {
    btn.disabled = false;
    btn.textContent = '登录';
  }
}

/* ---------------------------------------------------------------- 路由 */

function parseHash() {
  const h = location.hash.replace(/^#\/?/, '');
  const [route, ...rest] = h.split('/');
  S.route = TITLES[route] ? route : 'overview';
  S.param = decodeURIComponent(rest.join('/') || '');
  if (S.route !== 'plugins') S.param = S.param || '';
}

function syncNav() {
  $$('#nav a').forEach(a => a.classList.toggle('active', a.dataset.route === S.route));
  $('#title').textContent = TITLES[S.route] || '概览';
  $('#hint').textContent = S.route === 'plugins' && S.param ? '/ ' + S.param : '';
}

async function render() {
  syncNav();
  const view = $('#view');
  if (!S.me) return;
  view.innerHTML = '<div class="loading">加载中…</div>';
  try {
    switch (S.route) {
      case 'overview': return await renderOverview(view);
      case 'plugins': return S.param ? await renderPluginDetail(view, S.param) : await renderPlugins(view);
      case 'site': return await renderSite(view);
      case 'licenses': return await renderLicenses(view);
      case 'updates': return await renderUpdates(view);
      case 'audit': return await renderAudit(view);
      case 'password': return renderPassword(view);
    }
  } catch (err) {
    view.innerHTML = `<div class="notice err">${esc(err.message)}</div>`;
  }
}

function go(hash) {
  if (location.hash === hash) render();
  else location.hash = hash;
}

/* ---------------------------------------------------------------- 概览 */

async function renderOverview(view) {
  const d = await api('/api/overview');
  const p = d.plugins, l = d.licenses;
  const warn = (d.warnings || []).map(w => `<div class="notice warn">${esc(w)}</div>`).join('');

  view.innerHTML = `
    <div class="grid c4" style="margin-bottom:16px">
      <div class="stat"><div class="k">插件</div><div class="v">${p.total}<small>个</small></div>
        <div class="tiny dim">已发布 ${p.published} · 待发布 ${p.drift}</div></div>
      <div class="stat"><div class="k">授权码</div><div class="v">${l.total}<small>张</small></div>
        <div class="tiny dim">活跃 ${l.active} · 30 天内到期 ${l.expiring}</div></div>
      <div class="stat"><div class="k">面板版本</div><div class="v" style="font-family:var(--mono)">${esc(d.updates.latest || '—')}</div>
        <div class="tiny dim">清单里 ${d.updates.releases} 个版本</div></div>
      <div class="stat"><div class="k">后台运行</div><div class="v">${esc(d.admin.uptime)}</div>
        <div class="tiny dim">v${esc(d.admin.version)}</div></div>
    </div>

    ${warn ? `<div class="card"><h2>需要处理</h2>${warn}</div>` : ''}

    <div class="card">
      <h2>密钥自检<span class="spacer"></span>
        <span class="tiny dim">私钥的公钥部分必须与面板内置常量一致</span></h2>
      <div class="table-wrap"><table>
        <thead><tr><th>用途</th><th>文件</th><th>公钥</th><th>状态</th></tr></thead>
        <tbody>${d.keys.map(k => `
          <tr>
            <td>${esc(k.name)}</td>
            <td class="mono tiny">${esc(k.path)}</td>
            <td class="mono tiny" style="max-width:340px;word-break:break-all">${esc(k.public || '—')}</td>
            <td>${k.match ? badge('与面板一致', 'ok') : badge('不匹配', 'err')}</td>
          </tr>`).join('')}
        </tbody>
      </table></div>
    </div>

    <div class="card">
      <h2>路径</h2>
      <dl class="kv">
        ${[['site', '站点'], ['src', '插件源'], ['dist', '构建产物'], ['keys', '私钥'], ['data', '数据']]
          .filter(([k]) => d.paths[k])
          .map(([k, label]) => `<dt>${esc(label)}</dt><dd class="mono tiny">${esc(d.paths[k])}</dd>`).join('')}
      </dl>
    </div>`;
}

/* ---------------------------------------------------------------- 插件 */

async function renderPlugins(view) {
  const d = await api('/api/plugins');
  S.plugins = d.plugins || [];

  const rows = S.plugins.map(p => {
    const mf = p.manifest || {};
    const price = mf.tier === 'paid' ? `¥${Number(mf.priceCny) || 0}` : '免费';
    const pub = p.published && p.published.exists ? 'v' + p.published.version : '—';
    return `<tr>
      <td><a href="#/plugins/${encodeURIComponent(p.id)}" class="mono">${esc(p.id)}</a></td>
      <td>${esc(localName(mf.name))}</td>
      <td class="mono">v${esc(mf.version || '')}</td>
      <td class="mono dim">${esc(pub)}</td>
      <td class="dim">${esc(mf.category || '')}</td>
      <td>${mf.tier === 'paid' ? badge(price, 'accent') : badge(price)}</td>
      <td>${driftBadge(p.drift)}</td>
      <td class="row" style="gap:6px">
        <button class="btn btn-sm" data-edit="${esc(p.id)}">编辑</button>
      </td>
    </tr>`;
  }).join('');

  view.innerHTML = `
    <div class="card">
      <h2>插件清单<span class="spacer"></span>
        <span class="tiny dim">改完清单点「发布」才会写到线上</span>
        <button class="btn btn-sm" id="dry">演练</button>
        <button class="btn btn-primary btn-sm" id="publish">发布市场</button>
      </h2>
      <div class="table-wrap"><table>
        <thead><tr><th>ID</th><th>名称</th><th>源码版本</th><th>线上</th><th>分类</th><th>价格</th><th>状态</th><th></th></tr></thead>
        <tbody>${rows || '<tr><td colspan="8" class="dim">还没有插件</td></tr>'}</tbody>
      </table></div>
      <div id="build-result" style="margin-top:14px"></div>
    </div>`;

  $$('[data-edit]').forEach(b => b.addEventListener('click', () => go('#/plugins/' + encodeURIComponent(b.dataset.edit))));
  const runBuild = async (dryRun) => {
    const box = $('#build-result');
    box.innerHTML = `<div class="notice">${dryRun ? '演练中' : '打包中'}…</div>`;
    try {
      const res = await api('/api/market/build', { dryRun });
      const statusText = { new: '首次发布', changed: '已更新', same: '无变化' };
      box.innerHTML = `
        <div class="notice ok">${dryRun ? '演练完成，没有写入任何文件。' : '已发布到站点目录。'}
          索引 ${res.indexStatus === 'same' ? '未变化' : '已更新'}。</div>
        <div class="table-wrap"><table>
          <thead><tr><th>插件</th><th>版本</th><th class="num">大小</th><th class="num">文件</th><th>结果</th></tr></thead>
          <tbody>${res.plugins.map(p => `<tr>
            <td class="mono">${esc(p.id)}</td><td class="mono">v${esc(p.version)}</td>
            <td class="num">${fmtBytes(p.size)}</td><td class="num">${p.files}</td>
            <td>${badge(statusText[p.status] || p.status, p.status === 'same' ? '' : 'ok')}</td>
          </tr>`).join('')}</tbody>
        </table></div>`;
      toast(dryRun ? '演练完成' : '已发布', 'ok');
      if (!dryRun) render();
    } catch (err) {
      box.innerHTML = `<div class="notice err">${esc(err.message)}</div>`;
    }
  };
  $('#dry').addEventListener('click', () => runBuild(true));
  $('#publish').addEventListener('click', () => {
    if (!confirm('把当前源码打包并发布到 panel.wpqu.com？\n客户刷新市场后就能看到新版本。')) return;
    runBuild(false);
  });
}

async function renderPluginDetail(view, id) {
  const d = await api('/api/plugins/' + encodeURIComponent(id));
  S.plugin = d;
  const mf = d.manifest || {};
  const pub = d.published && d.published.exists
    ? `<dl class="kv">
        <dt>版本</dt><dd class="mono">v${esc(d.published.version)}</dd>
        <dt>大小</dt><dd>${fmtBytes(d.published.size)}</dd>
        <dt>SHA256</dt><dd class="mono tiny">${esc(d.published.sha256)}</dd>
        <dt>地址</dt><dd class="mono tiny">${esc(d.published.url)}</dd>
      </dl>`
    : '<div class="notice">线上还没有这个插件。</div>';

  view.innerHTML = `
    <div class="row" style="margin-bottom:14px">
      <button class="btn btn-ghost btn-sm" onclick="history.back()">← 返回列表</button>
      <span class="mono">${esc(d.id)}</span>${driftBadge(d.drift)}
    </div>
    <div class="split">
      <div>
        <div class="card">
          <h2>概要</h2>
          <dl class="kv">
            <dt>名称</dt><dd>${esc(localName(mf.name))}</dd>
            <dt>版本</dt><dd class="mono">v${esc(mf.version || '')}</dd>
            <dt>分类</dt><dd>${esc(mf.category || '—')}</dd>
            <dt>收费</dt><dd>${mf.tier === 'paid' ? '¥' + (Number(mf.priceCny) || 0) : '免费'}</dd>
            <dt>最低面板</dt><dd class="mono">${esc((mf.requires && mf.requires.minPanel) || '—')}</dd>
          </dl>
        </div>
        <div class="card">
          <h2>线上包</h2>
          ${pub}
        </div>
        <div class="card">
          <h2>包内文件</h2>
          <div class="list">${(d.files || []).map(f =>
            `<div class="row" style="justify-content:space-between"><span class="mono tiny">${esc(f.path)}</span>
             <span class="tiny dim">${fmtBytes(f.size)}</span></div>`).join('') || '<span class="dim">—</span>'}</div>
        </div>
      </div>
      <div>
        <div class="card">
          <h2>plugin.json<span class="spacer"></span>
            <button class="btn btn-sm" data-bump="patch">补丁 +1</button>
            <button class="btn btn-sm" data-bump="minor">次版本 +1</button>
            <button class="btn btn-sm" data-bump="major">主版本 +1</button>
          </h2>
          <textarea class="code" id="manifest" spellcheck="false">${esc(d.raw)}</textarea>
          <div class="row end" style="margin-top:12px">
            <button class="btn btn-ghost" id="reset">还原</button>
            <button class="btn btn-primary" id="save">保存清单</button>
          </div>
          <div id="m-result" style="margin-top:12px"></div>
        </div>
      </div>
    </div>`;

  const ta = $('#manifest');
  const box = $('#m-result');

  $('#reset').addEventListener('click', () => { ta.value = d.raw; box.innerHTML = ''; });

  $('#save').addEventListener('click', async () => {
    box.innerHTML = '';
    try {
      const res = await api(`/api/plugins/${encodeURIComponent(id)}/manifest`, { json: ta.value }, 'PUT');
      ta.value = res.json;
      toast('已保存', 'ok');
      box.innerHTML = '<div class="notice ok">已保存。要让它生效，回到列表点「发布市场」。</div>';
      syncNav();
    } catch (err) {
      box.innerHTML = `<div class="notice err">${esc(err.message)}</div>`;
    }
  });

  $$('[data-bump]').forEach(b => b.addEventListener('click', async () => {
    try {
      const res = await api(`/api/plugins/${encodeURIComponent(id)}/version`, { mode: b.dataset.bump }, 'POST');
      ta.value = res.json;
      toast('版本已改为 v' + res.version, 'ok');
      box.innerHTML = `<div class="notice ok">版本已改为 <b>v${esc(res.version)}</b>。记得发布。</div>`;
    } catch (err) {
      box.innerHTML = `<div class="notice err">${esc(err.message)}</div>`;
    }
  }));
}

/* ---------------------------------------------------------------- 官网 */

async function renderSite(view) {
  const tree = await api('/api/site/tree?root=' + encodeURIComponent(S.root));
  S.tree = tree.files || [];

  const groups = {};
  for (const f of S.tree) {
    const dir = f.path.includes('/') ? f.path.split('/')[0] : '(根目录)';
    (groups[dir] = groups[dir] || []).push(f);
  }
  const listHtml = Object.entries(groups).map(([dir, files]) => `
    <div class="g">${esc(dir)}</div>
    ${files.map(f => `<button data-path="${esc(f.path)}" class="${S.file && S.file.path === f.path ? 'active' : ''}">
      ${esc(f.path.split('/').pop())}<span class="spacer"></span>
      <span class="tiny dim">${fmtBytes(f.size)}</span></button>`).join('')}`).join('');

  view.innerHTML = `
    <div class="tabs">
      <button data-root="site" class="${S.root === 'site' ? 'active' : ''}">站点 site/</button>
      <button data-root="plugins-src" class="${S.root === 'plugins-src' ? 'active' : ''}">插件源 plugins-src/</button>
    </div>
    <div class="split">
      <div class="card" style="max-height:calc(100vh - 200px);overflow:auto">
        <div class="list">${listHtml || '<span class="dim">没有可编辑的文本文件</span>'}</div>
      </div>
      <div class="card" id="editor-card">
        ${S.file ? editorHtml(S.file) : '<div class="dim">从左边选一个文件。所有修改都会先备份，可以还原。</div>'}
      </div>
    </div>`;

  $$('[data-root]').forEach(b => b.addEventListener('click', () => {
    S.root = b.dataset.root;
    S.file = null;
    render();
  }));
  $$('[data-path]').forEach(b => b.addEventListener('click', () => loadFile(b.dataset.path)));

  bindEditor();
}

function editorHtml(f) {
  const backups = (f.backups || []).map(b =>
    `<button class="btn btn-sm" data-restore="${esc(b.name)}">${esc(b.at)}</button>`).join('');
  return `
    <h2 style="font-size:13px;margin-bottom:10px" class="mono">${esc(f.root)}/${esc(f.path)}</h2>
    <textarea class="code" id="editor" spellcheck="false">${esc(f.content)}</textarea>
    <div class="row" style="margin-top:12px">
      <span class="tiny dim">${fmtBytes(f.size)} · 修改于 ${esc(f.mtime)}</span>
      <span class="spacer"></span>
      <button class="btn btn-primary" id="save-file">保存</button>
    </div>
    ${backups ? `<div style="margin-top:14px"><div class="tiny dim" style="margin-bottom:6px">历史版本（点一下还原）</div>
      <div class="row">${backups}</div></div>` : ''}
    <div id="f-result" style="margin-top:12px"></div>`;
}

async function loadFile(p) {
  try {
    const f = await api(`/api/site/file?root=${encodeURIComponent(S.root)}&path=${encodeURIComponent(p)}`);
    f.root = S.root;
    S.file = f;
    await renderSite($('#view'));
  } catch (err) {
    toast(err.message, 'err');
  }
}

function bindEditor() {
  const save = $('#save-file');
  if (save) {
    save.addEventListener('click', async () => {
      const content = $('#editor').value;
      const box = $('#f-result');
      save.disabled = true;
      try {
        const res = await api('/api/site/file', { root: S.root, path: S.file.path, content }, 'PUT');
        if (res.unchanged) {
          box.innerHTML = '<div class="notice">内容没有变化。</div>';
        } else {
          S.file.content = content;
          S.file.backups = res.backups || [];
          box.innerHTML = '<div class="notice ok">已保存。' +
            (res.backup ? '旧版本已备份：<span class="mono tiny">' + esc(res.backup) + '</span>' : '') + '</div>';
          toast('已保存', 'ok');
        }
      } catch (err) {
        box.innerHTML = `<div class="notice err">${esc(err.message)}</div>`;
      } finally {
        save.disabled = false;
      }
    });
  }
  $$('[data-restore]').forEach(b => b.addEventListener('click', async () => {
    if (!confirm('用这一版覆盖当前文件？当前内容会先被备份。')) return;
    try {
      const res = await api('/api/site/restore',
        { root: S.root, path: S.file.path, backup: b.dataset.restore }, 'POST');
      S.file.content = res.content;
      toast('已还原', 'ok');
      await renderSite($('#view'));
    } catch (err) {
      toast(err.message, 'err');
    }
  }));
}

/* ---------------------------------------------------------------- 授权码 */

async function renderLicenses(view) {
  const qs = new URLSearchParams();
  if (S.filters.q) qs.set('q', S.filters.q);
  if (S.filters.product) qs.set('product', S.filters.product);
  if (S.filters.status) qs.set('status', S.filters.status);
  const d = await api('/api/licenses?' + qs.toString());
  S.licenses = d;

  const products = d.products || [];
  const opts = products.map(p =>
    `<option value="${esc(p.id)}">${esc(p.id)} · ${esc(localName(p.name))}${p.tier === 'paid' ? '（¥' + (p.priceCny || 0) + '）' : '（免费）'}</option>`
  ).join('');

  const rows = (d.licenses || []).map(e => `
    <tr>
      <td class="mono tiny">${esc(e.id)}</td>
      <td class="mono">${esc(e.product)}</td>
      <td>${esc(e.name)}</td>
      <td class="mono tiny">${esc(e.expire || '永久')}</td>
      <td class="num">${e.seats ? e.seats : '不限'}</td>
      <td>${licBadge(e.status)}</td>
      <td class="tiny dim">${esc(fmtDate(e.issuedAt))}</td>
      <td class="row" style="gap:6px">
        <button class="btn btn-sm" data-copy="${esc(e.code)}">复制码</button>
        ${e.void ? '' : `<button class="btn btn-sm btn-danger" data-void="${esc(e.id)}">作废</button>`}
      </td>
    </tr>`).join('');

  view.innerHTML = `
    <div class="grid c2">
      <div class="card">
        <h2>签发授权码</h2>
        <form id="issue-form">
          <label class="field"><span>插件</span><select id="i-product">${opts || '<option value="">没有插件</option>'}</select></label>
          <div class="row" style="gap:12px">
            <label class="field" style="flex:1"><span>到期日 <b>（留空为永久）</b></span>
              <input type="date" id="i-expire"></label>
            <label class="field" style="width:110px"><span>席位</span>
              <input type="number" id="i-seats" value="1" min="0"></label>
            <label class="field" style="width:100px"><span>数量</span>
              <input type="number" id="i-count" value="1" min="1" max="200"></label>
          </div>
          <label class="field"><span>客户名称 <b>（写进码里，必填）</b></span>
            <input type="text" id="i-name" placeholder="某某科技有限公司"></label>
          <label class="field"><span>备注</span>
            <input type="text" id="i-note" placeholder="微信转账 199，2026-11-17"></label>
          <button class="btn btn-primary btn-block" type="submit">签发</button>
        </form>
        <div id="issue-result" style="margin-top:14px"></div>
      </div>

      <div class="card">
        <h2>验一个码</h2>
        <p class="tiny dim" style="margin-top:-4px">客户说「码用不了」时，贴进来看看问题在哪。校验用的是面板里同一个公钥。</p>
        <textarea id="v-code" style="min-height:96px" placeholder="WPQU1.…"></textarea>
        <div class="row end" style="margin-top:10px">
          <button class="btn" id="v-run">校验</button>
        </div>
        <div id="v-result" style="margin-top:12px"></div>
      </div>
    </div>

    <div class="card">
      <h2>台账<span class="spacer"></span>
        <span class="tiny dim">共 ${d.summary.total} 张 · 活跃 ${d.summary.active} · 30 天内到期 ${d.summary.expiring} · 已过期 ${d.summary.expired}</span>
        <a class="btn btn-sm" href="/api/licenses/export">导出 CSV</a>
      </h2>
      <div class="row" style="margin-bottom:12px">
        <input type="text" id="f-q" placeholder="搜客户 / 备注 / id" value="${esc(S.filters.q)}" style="width:220px">
        <select id="f-product" style="width:200px">
          <option value="">全部插件</option>
          ${products.map(p => `<option value="${esc(p.id)}" ${S.filters.product === p.id ? 'selected' : ''}>${esc(p.id)}</option>`).join('')}
        </select>
        <select id="f-status" style="width:140px">
          ${['', '有效', '永久', '即将到期', '已过期', '已作废'].map(s =>
            `<option value="${esc(s)}" ${S.filters.status === s ? 'selected' : ''}>${esc(s || '全部状态')}</option>`).join('')}
        </select>
        <button class="btn btn-sm" id="f-reset">清空</button>
      </div>
      <div class="table-wrap"><table>
        <thead><tr><th>ID</th><th>插件</th><th>客户</th><th>到期</th><th class="num">席位</th><th>状态</th><th>签发</th><th></th></tr></thead>
        <tbody>${rows || '<tr><td colspan="8" class="dim">还没有发过码</td></tr>'}</tbody>
      </table></div>
      <p class="tiny dim" style="margin:10px 0 0">
        「作废」只改台账，不会让码失效 —— 离线验签没有吊销机制。客户那边仍然能用，直到到期。</p>
    </div>`;

  $('#issue-form').addEventListener('submit', doIssue);
  $('#v-run').addEventListener('click', doVerify);
  $$('[data-copy]').forEach(b => b.addEventListener('click', () => copy(b.dataset.copy)));
  $$('[data-void]').forEach(b => b.addEventListener('click', async () => {
    const reason = prompt('作废原因（记进台账）', '客户退款');
    if (reason === null) return;
    try {
      const res = await api(`/api/licenses/${encodeURIComponent(b.dataset.void)}/void`, { reason }, 'POST');
      toast(res.notice || '已作废', 'ok');
      render();
    } catch (err) { toast(err.message, 'err'); }
  }));

  let timer = 0;
  const applyFilter = () => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      S.filters.q = $('#f-q').value.trim();
      S.filters.product = $('#f-product').value;
      S.filters.status = $('#f-status').value;
      renderLicenses($('#view'));
    }, 260);
  };
  $('#f-q').addEventListener('input', applyFilter);
  $('#f-product').addEventListener('change', applyFilter);
  $('#f-status').addEventListener('change', applyFilter);
  $('#f-reset').addEventListener('click', () => {
    S.filters = { q: '', product: '', status: '' };
    renderLicenses($('#view'));
  });
}

async function doIssue(e) {
  e.preventDefault();
  const box = $('#issue-result');
  const body = {
    product: $('#i-product').value,
    expire: $('#i-expire').value,
    seats: Number($('#i-seats').value) || 0,
    count: Number($('#i-count').value) || 1,
    name: $('#i-name').value.trim(),
    note: $('#i-note').value.trim(),
  };
  if (!body.name) { toast('请填客户名称', 'err'); $('#i-name').focus(); return; }
  box.innerHTML = '<div class="notice">签发中…</div>';
  try {
    const res = await api('/api/licenses', body, 'POST');
    S.lastCodes = res.codes;
    box.innerHTML = `
      <div class="notice ok">已签发 ${res.codes.length} 张，同时记入台账。</div>
      <div class="row" style="margin:8px 0">
        <button class="btn btn-sm" id="copy-all">复制全部</button>
        <button class="btn btn-sm" id="copy-one">只复制第一张</button>
      </div>
      <div class="code-box">${esc(res.codes.join('\n'))}</div>`;
    $('#copy-all').addEventListener('click', () => copy(res.codes.join('\n')));
    $('#copy-one').addEventListener('click', () => copy(res.codes[0]));
    $('#i-note').value = '';
    toast('已签发', 'ok');
    await renderLicenses($('#view'));
  } catch (err) {
    box.innerHTML = `<div class="notice err">${esc(err.message)}</div>`;
  }
}

async function doVerify() {
  const code = $('#v-code').value.trim();
  const box = $('#v-result');
  if (!code) { toast('先贴一个码', 'err'); return; }
  box.innerHTML = '<div class="notice">校验中…</div>';
  try {
    const r = await api('/api/licenses/verify', { code }, 'POST');
    if (!r.ok) {
      const dec = r.decoded
        ? `<dl class="kv" style="margin-top:10px">
             <dt>产品</dt><dd class="mono">${esc(r.decoded.p)}</dd>
             <dt>客户</dt><dd>${esc(r.decoded.n || '—')}</dd>
             <dt>到期</dt><dd class="mono">${esc(r.decoded.e || '永久')}</dd>
           </dl><p class="tiny dim">载荷能读出来，但签名不对，面板会拒绝这个码。</p>`
        : '';
      box.innerHTML = `<div class="notice err">${esc(r.error)}</div>${dec}`;
      return;
    }
    const lic = r.license;
    box.innerHTML = `
      <div class="notice ${r.panelKeyMatch === false ? 'err' : 'ok'}">
        ${r.panelKeyMatch === false
          ? esc(r.warning || '本机密钥与面板内置公钥不一致，面板会拒绝这个码')
          : '签名有效，面板会接受这个码。'}</div>
      <dl class="kv" style="margin-top:10px">
        <dt>产品</dt><dd class="mono">${esc(lic.p)} ${r.product ? '· ' + esc(localName(r.product.name)) : '· <span style="color:var(--err)">插件已不存在</span>'}</dd>
        <dt>客户</dt><dd>${esc(lic.n || '—')}</dd>
        <dt>到期</dt><dd class="mono">${esc(lic.e || '永久')} <span class="dim">（${esc(r.expireIn)}）</span></dd>
        <dt>状态</dt><dd>${licBadge(r.status)}</dd>
        <dt>席位</dt><dd>${esc(lic.s || 0)}</dd>
        <dt>码 id</dt><dd class="mono">${esc(lic.i)} ${r.inLedger ? badge('台账里有', 'ok') : badge('台账里没有', 'warn')}</dd>
        <dt>签发</dt><dd class="mono tiny">${esc(lic.t)}</dd>
      </dl>`;
  } catch (err) {
    box.innerHTML = `<div class="notice err">${esc(err.message)}</div>`;
  }
}

/* ---------------------------------------------------------------- 更新 */

async function renderUpdates(view) {
  const d = await api('/api/updates');
  S.updates = d;
  const m = d.manifest || {};
  const bins = d.distFiles || [];

  view.innerHTML = `
    <div class="grid c2">
      <div class="card">
        <h2>当前清单</h2>
        <dl class="kv">
          <dt>最新版本</dt><dd class="mono">${esc(m.latest || '—')}</dd>
          <dt>最低支持</dt><dd class="mono">${esc(m.minSupported || '—')}</dd>
          <dt>渠道</dt><dd>${esc(m.channel || '—')}</dd>
          <dt>安装源</dt><dd class="mono tiny">${esc(d.updateBase)}</dd>
        </dl>
        <div style="margin-top:14px" class="table-wrap"><table>
          <thead><tr><th>版本</th><th>日期</th><th class="num">平台</th><th>说明</th></tr></thead>
          <tbody>${(d.releases || []).map(r => `<tr>
            <td class="mono">v${esc(r.version)} ${r.critical ? badge('重要', 'warn') : ''}</td>
            <td class="tiny dim nowrap">${esc(r.date)}</td>
            <td class="num">${r.platforms}</td>
            <td class="tiny">${esc((r.notes || []).slice(0, 2).join('；'))}</td>
          </tr>`).join('') || '<tr><td colspan="4" class="dim">还没有发布过</td></tr>'}</tbody>
        </table></div>
      </div>

      <div class="card">
        <h2>发布新版本</h2>
        ${bins.length ? '' : `<div class="notice warn">${esc(d.distDir)} 里没有可发布的二进制。
          先在 panel-go 下跑 <code>./build.sh all</code>。</div>`}
        <form id="up-form" style="margin-top:${bins.length ? '0' : '12px'}">
          <div class="row" style="gap:12px">
            <label class="field" style="flex:1"><span>版本号</span>
              <input type="text" id="u-version" placeholder="1.9.1" value="${esc(nextVersion(m.latest))}"></label>
            <label class="field" style="width:130px"><span>最低支持</span>
              <input type="text" id="u-min" placeholder="${esc(m.minSupported || '1.9.0')}"></label>
          </div>
          <label class="check" style="margin-bottom:14px">
            <input type="checkbox" id="u-critical"> 重大更新（面板会用红色标出，建议慎重）
          </label>
          <label class="field"><span>更新说明（中文，一行一条）</span>
            <textarea id="u-zh" style="min-height:96px" placeholder="修复了插件安装中途断网后无法重试的问题&#10;新增 Docker 插件"></textarea></label>
          <label class="field"><span>更新说明（英文，可留空）</span>
            <textarea id="u-en" style="min-height:70px" placeholder="Fixed …"></textarea></label>
          <button class="btn btn-primary btn-block" type="submit" ${bins.length ? '' : 'disabled'}>发布</button>
        </form>
        <div id="u-result" style="margin-top:12px"></div>
      </div>
    </div>

    <div class="card">
      <h2>构建产物<span class="spacer"></span><span class="tiny dim mono">${esc(d.distDir)}</span></h2>
      <div class="table-wrap"><table>
        <thead><tr><th>文件</th><th>平台</th><th class="num">大小</th><th>修改时间</th></tr></thead>
        <tbody>${bins.map(b => `<tr>
          <td class="mono tiny">${esc(b.name)}</td>
          <td class="dim">${esc(b.os)}/${esc(b.arch)}</td>
          <td class="num">${fmtBytes(b.size)}</td>
          <td class="tiny dim">${esc(b.mtime)}</td>
        </tr>`).join('') || '<tr><td colspan="4" class="dim">没有</td></tr>'}</tbody>
      </table></div>
      <p class="tiny dim" style="margin:10px 0 0">
        只有形如 <code>wpqu-panel-&lt;os&gt;-&lt;arch&gt;</code> 的文件会进清单；console 版与 SHA256SUMS 自动排除。</p>
    </div>`;

  $('#up-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const box = $('#u-result');
    const body = {
      version: $('#u-version').value.trim(),
      minSupported: $('#u-min').value.trim(),
      critical: $('#u-critical').checked,
      notesZh: $('#u-zh').value,
      notesEn: $('#u-en').value,
    };
    box.innerHTML = '<div class="notice">发布中…</div>';
    try {
      const res = await api('/api/updates/publish', body, 'POST');
      box.innerHTML = `<div class="notice ok">已发布 v${esc(res.version)}：${res.platforms} 个平台，
        清单里共 ${res.versions} 个版本。</div>`;
      toast('已发布', 'ok');
      await renderUpdates($('#view'));
    } catch (err) {
      box.innerHTML = `<div class="notice err">${esc(err.message)}</div>`;
    }
  });
}

function nextVersion(latest) {
  if (!latest) return '';
  const p = String(latest).split('.').map(n => parseInt(n, 10) || 0);
  if (p.length < 3) return '';
  return [p[0], p[1], p[2] + 1].join('.');
}

/* ---------------------------------------------------------------- 审计 */

async function renderAudit(view) {
  const d = await api('/api/audit?limit=300');
  const rows = (d.entries || []).map(e => `<tr>
    <td class="tiny dim mono">${esc(fmtDate(e.at))}</td>
    <td class="mono tiny">${esc(e.action)}</td>
    <td class="mono tiny">${esc(e.target)}</td>
    <td class="tiny">${esc(e.detail || '')}</td>
    <td class="tiny dim mono">${esc(e.ip)}</td>
  </tr>`).join('');

  view.innerHTML = `
    <div class="card">
      <h2>最近 300 条<span class="spacer"></span>
        <span class="tiny dim">每次保存、发布、发码、作废都会留痕</span></h2>
      <div class="table-wrap"><table>
        <thead><tr><th>时间</th><th>动作</th><th>对象</th><th>详情</th><th>来源</th></tr></thead>
        <tbody>${rows || '<tr><td colspan="5" class="dim">还没有记录</td></tr>'}</tbody>
      </table></div>
    </div>`;
}

/* ---------------------------------------------------------------- 口令 */

function renderPassword(view) {
  view.innerHTML = `
    <div class="card" style="max-width:420px">
      <h2>修改口令</h2>
      <form id="pw-form">
        <label class="field"><span>当前口令</span><input type="password" id="pw-old" autocomplete="current-password"></label>
        <label class="field"><span>新口令（至少 8 位）</span><input type="password" id="pw-new" autocomplete="new-password"></label>
        <label class="field"><span>再输一次</span><input type="password" id="pw-new2" autocomplete="new-password"></label>
        <button class="btn btn-primary" type="submit">保存</button>
      </form>
      <div id="pw-result" style="margin-top:12px"></div>
    </div>`;
  $('#pw-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const box = $('#pw-result');
    const oldPw = $('#pw-old').value;
    const a = $('#pw-new').value;
    const b = $('#pw-new2').value;
    if (a !== b) { box.innerHTML = '<div class="notice err">两次输入的新口令不一致</div>'; return; }
    try {
      await api('/api/password', { old: oldPw, new: a }, 'POST');
      box.innerHTML = '<div class="notice ok">已修改。下次登录用新口令。</div>';
      $('#pw-form').reset();
    } catch (err) {
      box.innerHTML = `<div class="notice err">${esc(err.message)}</div>`;
    }
  });
}

/* ---------------------------------------------------------------- 启动 */

async function boot() {
  try {
    S.me = await api('/api/me');
    $('#ver').textContent = 'v' + S.me.version;
  } catch (e) {
    showLogin();
    return;
  }
  // 刷新页面时也要把主界面显示出来：doLogin 里有这一步，但带会话直接打开时
  // 走的是 boot，漏了就会出现「登录后刷新一片空白」。
  showApp();
  parseHash();
  await render();
}

$('#login-form').addEventListener('submit', doLogin);
$('#logout').addEventListener('click', async () => {
  try { await api('/api/logout', {}, 'POST'); } catch (e) { /* 已经失效也无所谓 */ }
  S.me = null;
  showLogin('已退出');
});
window.addEventListener('hashchange', async () => {
  if (!S.me) return;
  parseHash();
  await render();
});

boot();
