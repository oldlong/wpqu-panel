/* WPQU panel — 国际化核心
 *
 * 设计要点
 * ---------
 * 1. 源语言是中文：字典的键就是界面上的中文原文（msgid 风格）。缺译文时直接回退中文，
 *    所以新增文案不必先补翻译，也不会出现空键名或 undefined。
 * 2. 翻译作用在「渲染后的 DOM」上（文本节点 + placeholder/title/aria-label），
 *    因此 app.js 里那些拼接出来的 HTML 模板不用逐条改成 t('…')，
 *    以后新写的界面也能自动吃到译文。
 * 3. <pre>（命令输出、日志）、<code>、<textarea>（用户输入）一律跳过：
 *    远端脚本输出本来就是中文，不该被替换。
 * 4. 加一门语言：新建 i18n/xx.js 导出 { '中文原文': '译文' }，
 *    再到 i18n/dicts.js 里登记一行，并在下面的 LANGS 里补一条元信息即可。
 */
import dicts from './dicts.js';

export const LANGS = [
  { code: 'zh-CN', label: '简体中文', short: 'CH' },
  { code: 'en', label: 'English', short: 'EN' },
];
// 侧栏语言是 index.html 里写死的 CH / EN 两个按钮（按 data-lang 匹配）。
// 真加第三门语言时，除了这里补一条，还要在 index.html 的 .seg 里补一个按钮。

const STORE_KEY = 'wpqu-lang';
const CJK = /[\u4e00-\u9fa5]/;
const SKIP_TAGS = new Set(['SCRIPT', 'STYLE', 'PRE', 'CODE', 'TEXTAREA', 'KBD', 'SAMP']);
const ATTRS = ['placeholder', 'title', 'aria-label'];

/* 带数字的句子用模式匹配，免得为每种数量写一条译文。
   zh-CN 不需要（原文即译文），所以只登记非源语言。 */
const PATTERNS = {
  en: [
    [/^站点 (\d+) 个$/, (m) => `Sites: ${m[1]}`],
    [/^(\d+) 核 · 负载 (.+)$/, (m) => `${m[1]} cores · load ${m[2]}`],
    [/^个站点 · (\d+) 个 WordPress$/, (m) => `sites · ${m[1]} WordPress`],
    [/^(\d+) 个 WordPress$/, (m) => `${m[1]} WordPress`],
    [/^(\d+) 个域名$/, (m) => `${m[1]} domains`],
    [/^(\d+) 个扩展$/, (m) => `${m[1]} extensions`],
    [/^(\d+) 张表$/, (m) => `${m[1]} tables`],
    [/^表（(\d+)）$/, (m) => `Tables (${m[1]})`],
    [/^(\d+) 张表 · (.+) · (.+)$/, (m) => `${m[1]} tables · ${m[2]} · ${m[3]}`],
    [/^(.+)（(\d+) 张表 · (.+)）$/, (m) => `${m[1]} (${m[2]} tables · ${m[3]})`],
    [/^共 (\d+) 个文件，保存在本机 (.+)$/, (m) => `${m[1]} files, saved locally in ${m[2]}`],
    [/^证书剩 (\d+) 天$/, (m) => `Cert expires in ${m[1]} days`],
    [/^(.+)（(.+)） · 命令行默认 (.+)$/, (m) => `${m[1]} (${m[2]}) · CLI default ${m[3]}`],
    [/^(\d+) 行$/, (m) => `${m[1]} rows`],
    [/^(\d+) 项$/, (m) => `${m[1]} items`],
    [/^(\d+) 天$/, (m) => `${m[1]} days`],
    [/^剩余 (\d+) 天$/, (m) => `${m[1]} days left`],
    [/^(\d+) 个$/, (m) => `${m[1]}`],
    /* 操作记录里的句子由后端按模板拼好，历史记录里已经是中文，只能按形状反推 */
    [/^执行：(.+)$/, (m) => `Run: ${m[1]}`],
    [/^新增站点 (.+)（(.+)）$/, (m) => `Add site ${m[1]} (${m[2]})`],
    [/^删除整站 (.+)$/, (m) => `Delete site ${m[1]}`],
    [/^变更域名 (.+) → (.+)$/, (m) => `Change domain ${m[1]} → ${m[2]}`],
    [/^切换站点能力 (.+) → (.+)$/, (m) => `Switch capability ${m[1]} → ${m[2]}`],
    [/^初始化环境并部署 (.+)（(.+)）$/, (m) => `Set up and deploy ${m[1]} (${m[2]})`],
    [/^续期证书 (.+)$/, (m) => `Renew certificate ${m[1]}`],
    [/^申请证书 (.+)$/, (m) => `Issue certificate ${m[1]}`],
    [/^备份 (.+)$/, (m) => `Back up ${m[1]}`],
    /* 插件市场 / 更新：数字与名称是变量，只能按形状匹配 */
    [/^已安装 (.+)$/, (m) => `Installed ${m[1]}`],
    [/^已下载 v(.+)$/, (m) => `Downloaded v${m[1]}`],
    [/^收费 · ¥(\d+)$/, (m) => `Paid · ¥${m[1]}`],
    [/^连不上插件市场：(.+)$/, (m) => `Can't reach the plugin market: ${m[1]}`],
    [/^市场地址 (.+) · 更新于 (.+)$/, (m) => `Market: ${m[1]} · updated ${m[2]}`],
    [/^市场地址 (.+)$/, (m) => `Market: ${m[1]}`],
    [/^首次进入需要连接 (.+)$/, (m) => `First visit connects to ${m[1]}`],
    [/^已授权 · (.+) · 有效期至 (.+)$/, (m) => `Licensed · ${m[1]} · valid until ${m[2]}`],
    [/^v(\d\S*) · (.+)$/, (m) => `v${m[1]} · ${m[2]}`],
    [/^发现新版本 v(.+)$/, (m) => `New version v${m[1]}`],
    [/^立即升级到 v(.+)$/, (m) => `Update to v${m[1]}`],
    [/^已是最新版本（v(.+)）。$/, (m) => `You are on the latest version (v${m[1]}).`],
    [/^检查失败：(.+)$/, (m) => `Check failed: ${m[1]}`],
    [/^安装 (.+)$/, (m) => `Install ${m[1]}`],
    [/^卸载 (.+)$/, (m) => `Uninstall ${m[1]}`],
    [/^升级到 v(.+)$/, (m) => `Updating to v${m[1]}`],
    [/^插件界面加载失败：(.+)$/, (m) => `Failed to load the plugin UI: ${m[1]}`],
    [/^(\d+) 字$/, (m) => `${m[1]} chars`],
    [/^(.+) · 只保存在这台电脑的浏览器里$/, (m) => `${m[1]} · stored only in this browser`],
  ],
};

function detectLang() {
  let saved = '';
  try {
    saved = localStorage.getItem(STORE_KEY) || '';
  } catch {
    /* 隐私模式读不到 localStorage，按浏览器语言走 */
  }
  // ?lang=en 优先级最高：方便把某个语言的界面链接直接发给别人，也方便截图核对
  let qp = '';
  try {
    qp = new URLSearchParams(location.search).get('lang') || '';
  } catch {
    /* 老浏览器忽略 */
  }
  if (qp && LANGS.some((l) => l.code === qp)) {
    try {
      localStorage.setItem(STORE_KEY, qp);
    } catch {
      /* 存不下也不影响本次 */
    }
    return qp;
  }
  if (saved && LANGS.some((l) => l.code === saved)) return saved;
  const nav = (navigator.language || 'zh').toLowerCase();
  // 中文环境默认中文；其它语言先落到英文，等该语种的包补齐再切过去
  return nav.startsWith('zh') ? 'zh-CN' : 'en';
}

let lang = detectLang();
const dict = dicts[lang] || {};
const patterns = PATTERNS[lang] || [];
let observer = null;
// 已经翻过的节点记在这里：MutationObserver 会看到自己写回的内容，靠它防重入
const done = new WeakSet();

export function currentLang() {
  return lang;
}

export function langLabel(code = lang) {
  const meta = LANGS.find((l) => l.code === code);
  return meta ? meta.label : code;
}

/** 查表：命中返回译文，未命中返回 null（调用方决定回退） */
function lookup(text) {
  if (!text) return null;
  // 模板里的文案常带换行与缩进，归一化后再查，避免同一条文案要写多个版本的键
  const key = text.replace(/\s+/g, ' ').trim();
  const hit = dict[key];
  if (typeof hit === 'string') return hit;
  for (const [re, make] of patterns) {
    const m = key.match(re);
    if (m) return make(m);
  }
  return null;
}

/** 供 JS 侧主动翻译单条文案（DOM 之外，比如 document.title） */
export function t(text) {
  if (lang === 'zh-CN') return text;
  return lookup(text) ?? text;
}

export function setLang(code) {
  if (!LANGS.some((l) => l.code === code)) return;
  try {
    localStorage.setItem(STORE_KEY, code);
  } catch {
    /* 存不下就只在本次生效 */
  }
  // ?lang= 的优先级高于 localStorage（方便分享指定语言的链接），
  // 所以手动切换后必须把它从地址栏摘掉，否则 reload 会又按参数切回去
  try {
    const u = new URL(location.href);
    if (u.searchParams.has('lang')) {
      u.searchParams.delete('lang');
      history.replaceState(null, '', u.toString());
    }
  } catch {
    /* 老浏览器忽略 */
  }
  location.reload();
}

function translateTextNode(node) {
  const raw = node.nodeValue;
  if (!raw || !CJK.test(raw)) return;
  const key = raw.trim();
  if (!key) return;
  const tr = lookup(key);
  if (tr == null || tr === key) return;
  // 保留原有缩进与换行，只替换实体文本
  const start = raw.indexOf(key);
  node.nodeValue = raw.slice(0, start) + tr + raw.slice(start + key.length);
  done.add(node);
}

export function translate(root) {
  if (lang === 'zh-CN' || !root) return;
  const walk = (node) => {
    if (node.nodeType === 3) {
      if (!done.has(node)) translateTextNode(node);
      return;
    }
    if (node.nodeType !== 1) return;
    const tag = node.tagName;
    if (SKIP_TAGS.has(tag) || node.dataset.noI18n !== undefined) return;
    for (const attr of ATTRS) {
      const val = node.getAttribute(attr);
      if (!val || !CJK.test(val)) continue;
      const tr = lookup(val.trim());
      if (tr != null && tr !== val) node.setAttribute(attr, tr);
    }
    for (const child of node.childNodes) walk(child);
  };
  walk(root);
}

/** 启动：设 <html lang>、翻译首屏、挂观察器接管后续渲染 */
export function initI18n() {
  document.documentElement.lang = lang;
  if (lang === 'zh-CN') return lang;

  // 侧栏语言按钮是固定的 CH / EN 两个，选中态由 app.js 按 currentLang() 标；
  // 按钮上的拉丁字母本来就不进字典，这里不用做额外处理。
  translate(document.body);

  // 面板的界面是反复 innerHTML 重建的，观察器让新节点自动过一遍字典
  observer = new MutationObserver((records) => {
    for (const r of records) {
      if (r.type === 'characterData') {
        if (!done.has(r.target)) translateTextNode(r.target);
      } else if (r.type === 'attributes') {
        translate(r.target);
      } else {
        for (const n of r.addedNodes) translate(n);
      }
    }
  });
  observer.observe(document.body, {
    childList: true,
    subtree: true,
    characterData: true,
    attributes: true,
    attributeFilter: ATTRS,
  });
  return lang;
}
