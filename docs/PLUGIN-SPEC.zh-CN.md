# WPQU panel 插件开发规范

> 本文与开发工作区的 `docs/插件开发规范.md` 内容一致（这里是开源仓库里的副本），改一份记得同步另一份。
>
> 适用：面板 v1.9.0 及以上。本文按 2026-11 的代码实现整理（`internal/plugins`、`internal/pack`、`public/app.js`）。
> 目标是：**照着这份文档写出来的插件，不改面板代码就能装上、能升级、能卸载、能上线。**
> 上架流程、定价、发授权码看 [插件市场运营手册](插件市场运营手册.md)，本文只管"插件本身怎么写"。

一个插件就是一个目录：

```
plugins-src/<id>/
├── plugin.json          必填，声明插件是什么、怎么装
├── scripts/
│   ├── install.sh       服务器侧插件必填
│   └── uninstall.sh     强烈建议提供
├── ui/
│   └── panel.js         面板侧插件必填
└── assets/              可选，脚本用到的静态文件
```

打完包是一个带签名的 zip，面板从市场拉索引 → 校验签名 → 解压 → 执行脚本。

---

## 0. 30 秒生成骨架

不想从空白开始，用仓库里的脚手架：

```bash
tools/new-plugin.sh hello "演示服务" "Hello demo" ops      # 服务器侧插件
tools/new-plugin.sh hello-ui "演示面板" "Hello panel" panel # 面板侧插件
```

它会建好 `plugins-src/<id>/`、写好 `plugin.json`、`scripts/install.sh`、`scripts/uninstall.sh`
（panel 类是 `ui/panel.js`），并自动跑一次 `sync-mirrors.sh` 注入网络自适应库。
生成后照着第 4 节把 TODO 换成真正的逻辑即可。

---

## 1. 三条硬约束

先记住这三条，其它都是细节：

1. **目录名 = `id`**。面板按 id 找目录、拼下载地址（`/plugins/<id>/<version>.zip`），不一致打包直接报错。
2. **改了任何脚本或界面，必须改 `version`**。面板靠版本号判断要不要更新，同版本号覆盖发不出去。
3. **脚本在服务器上以 root 跑**（非 root 登录时用 `sudo -n`），没有交互终端，超时 **40 分钟/步**。所有命令必须非交互、可失败退出。

---

## 2. plugin.json

### 2.1 最小可用（服务器侧插件）

```json
{
  "id": "hello",
  "name": { "zh-CN": "Hello 服务", "en": "Hello service" },
  "summary": {
    "zh-CN": "装一个演示用的小服务，开机自启",
    "en": "Installs a demo service, enabled on boot"
  },
  "version": "1.0.0",
  "category": "ops",
  "tier": "free",
  "author": "你的名字",
  "icon": "👋",
  "accent": "#2563eb",
  "highlights": {
    "zh-CN": ["systemd 托管，开机自启", "只装一个二进制，不碰系统包"],
    "en": ["Managed by systemd", "One binary, no system packages"]
  },
  "requires": { "os": ["linux"], "minPanel": "1.9.0" },
  "install": {
    "check": "test -f /usr/local/bin/hello",
    "versionCmd": "/usr/local/bin/hello --version",
    "steps": [
      { "title": { "zh-CN": "安装 Hello", "en": "Install Hello" }, "file": "scripts/install.sh" }
    ],
    "uninstall": [
      { "title": { "zh-CN": "移除 Hello", "en": "Remove Hello" }, "file": "scripts/uninstall.sh" }
    ],
    "params": []
  }
}
```

### 2.2 字段表

| 字段 | 必填 | 说明 |
|---|---|---|
| `id` | ✅ | 小写字母 / 数字 / 连字符，2–64 位，正则 `^[a-z0-9][a-z0-9-]{1,63}$`；**必须等于目录名** |
| `name` | ✅ | 卡片标题，双语 |
| `summary` | ✅ | 一句话说明，卡片副标题，建议 ≤ 40 字 |
| `description` | 建议 | 详情页正文，讲清楚装了什么、有什么副作用 |
| `version` | ✅ | `1.0.0` 形式；脚本或界面改动后必须 +1 |
| `category` | ✅ | `runtime` / `ai` / `dev` / `ops` / `panel`，其它值会显示原文且筛不到 |
| `tier` | ✅ | `free` 或 `paid`；`paid` 还要 `priceCny` 和 `requires.license` |
| `priceCny` | 收费必填 | 整数，单位元 |
| `author` | ✅ | 显示在详情页 |
| `homepage` | 可选 | 上游官网，详情页给个链接 |
| `icon` | ✅ | emoji，或 1–2 个字母（如 `GO`、`PM`）；市场卡片会用它当图标 |
| `accent` | ✅ | 卡片主色，`#rrggbb` |
| `highlights` | ✅ | 3–4 条卖点，每条 12–18 字，双语数组 |
| `requires.os` | 建议 | `["linux"]`；安装时会真的比对服务器 `uname -s`（小写） |
| `requires.minPanel` | 建议 | 低于此版本的面板拒绝安装，并提示升级 |
| `requires.arch` / `requires.distro` | 可选 | 目前**只在市场展示**，安装时不会拦（要拦就在脚本里自己判断） |
| `requires.license` | 收费必填 | `true` 时安装前要求本机有有效授权码 |
| `requires.product` | 可选 | 授权绑定产品 id，默认同 `id` |
| `install` | 服务器侧必填 | 见第 3 节 |
| `ui` | 面板侧必填 | 见第 6 节 |
| `package` | ❌ 不要写 | 打包时自动写入 `url` / `sha256` / `size` |

### 2.3 双语规则

`name`、`summary`、`description`、`highlights`、`steps[].title`、`params[].label`、`params[].hint`、`ui.tabLabel` 全部是 map：

```json
{ "zh-CN": "中文", "en": "English" }
```

- `zh-CN` 必填，界面默认语言就是它。
- `en` 缺失时英文界面回退显示中文，不会报错，但会被用户看见。
- 不要在值里塞 HTML；界面统一当纯文本转义。

### 2.4 面板侧插件（`category: "panel"`）

不需要 `install`，改成 `ui`：

```json
{
  "id": "notes",
  "name": { "zh-CN": "服务器便签", "en": "Server notes" },
  "summary": { "zh-CN": "每台服务器一块便签", "en": "A scratchpad per server" },
  "version": "1.0.0",
  "category": "panel",
  "tier": "free",
  "author": "WPQU",
  "icon": "📝",
  "accent": "#7c6bd6",
  "highlights": { "zh-CN": ["按服务器分开"], "en": ["One pad per server"] },
  "requires": { "minPanel": "1.9.0" },
  "ui": {
    "entry": "ui/panel.js",
    "tabLabel": { "zh-CN": "便签", "en": "Notes" }
  }
}
```

安装后侧栏自动多一个页签，点开就是 `mount()` 渲染的内容。

---

## 3. install：让面板知道"装没装、装哪个版本"

```json
"install": {
  "check": "test -x /usr/local/go/bin/go",
  "versionCmd": "/usr/local/go/bin/go version | awk '{print $3}'",
  "steps": [ ... ],
  "uninstall": [ ... ],
  "params": [ ... ]
}
```

### 3.1 `check`

面板把它塞进 `if ( ... ) >/dev/null 2>&1; then` 里，**退出码 0 = 已安装**。

- 只判断"在不在"，不要在这里升级或修复。
- 每次打开某台服务器的插件页，会对这台机器跑一次，所以**要快**：`test -f`、`command -v`、`ls`。
- 不要在里面联网、不要 `systemctl status`（慢且可能挂住）。
- 可以多行，直接写成正常的多行 shell 即可；不要为了"一行"用 `&&` 硬拼（含 `if`/`for` 时会被拼坏）。

### 3.2 `versionCmd`

已安装时执行，**只取第一行**作为版本号显示。

- 输出要干净，别夹带警告；`2>/dev/null | head -1` 由面板负责。
- 实在取不到就 `echo -`，不要空着，也不要报错。

```json
"versionCmd": "docker --version"
"versionCmd": "awk -F\"'\" '/const VERSION/{print $2; exit}' /var/www/typecho*/var/Typecho/Common.php 2>/dev/null || echo -"
"versionCmd": ". /etc/wpqu/phpmyadmin.conf 2>/dev/null; echo \"${PMA_VERSION:--}\""
```

### 3.3 `steps`

按顺序执行，每步一个文件，**任意一步退出码非 0 就整体失败**并记进操作记录。

```json
"steps": [
  { "title": { "zh-CN": "安装依赖", "en": "Install dependencies" }, "file": "scripts/install.sh" }
]
```

- `file` 是包内相对路径，大小写敏感。
- 一步能讲清就别拆；拆多步的场景是"耗时长、想看进度"或"某步可能失败但没关系"（后者要靠脚本自己 `|| true`）。
- 步骤标题会实时显示在面板输出里，写成动宾短语（"下载并解压"而不是"步骤 1"）。

### 3.4 `params`

用户在安装弹窗里填的表单，最终变成**服务器上的环境变量**：

| plugin.json | 服务器脚本里 |
|---|---|
| `{"key": "port", "default": "8080"}` | `$WPQU_PORT` |
| `{"key": "db_pass"}` | `$WPQU_DB_PASS` |
| `{"key": "webroot"}` | `$WPQU_WEBROOT` |

规则（照抄这几条，能避开绝大多数坑）：

1. `key` 只用小写字母、数字、下划线；后端会转大写并加 `WPQU_` 前缀，含其它字符会被**静默丢弃**。
2. **空值不会传**。用户把输入框清空，脚本里就没有这个变量。
3. 所以脚本必须自己兜底：`PORT="${WPQU_PORT:-8080}"`，绝不能写 `$WPQU_PORT` 直接用（`set -u` 会当场退出）。
4. `default` 会在弹窗里预填，但它只是"建议值"，不是"保证值"——脚本兜底要用同一个默认值。
5. 敏感信息（密码）不要给 `default`，留空让脚本生成并打印。

```json
{
  "key": "port",
  "label": { "zh-CN": "端口", "en": "Port" },
  "default": "8080",
  "hint": { "zh-CN": "被占用时换成 8081 之类", "en": "Use 8081 if 8080 is taken" }
}
```

### 3.5 `uninstall`

同样是步骤数组，脚本从**本地已下载的插件目录**读取执行（不重新下载），所以卸载在断网时也能用。

没有 `uninstall` 时，面板提示"没有卸载脚本，请手工处理"——功能上可接受，体验上不推荐。

---

## 4. 安装脚本规范

### 4.1 位置

**必须叫 `scripts/install.sh`**。网络自适应库的注入脚本只扫这个路径：

```
plugins-src/*/scripts/install.sh
```

放到别处不会自动注入，国内服务器上装包会很慢甚至失败。

### 4.2 骨架

```bash
#!/usr/bin/env bash
# 一句话说明这个脚本做什么、由谁调用。
# 由 WPQU panel 插件系统以 root 执行；参数通过环境变量传入（WPQU_ 前缀）。
set -euo pipefail

# >>> mirror-lib BEGIN
# （这一段由 panel-go/scripts/sync-mirrors.sh 自动注入，不要手改）
# <<< mirror-lib END

ID=hello
log()  { printf '[%s] %s\n' "$ID" "$*"; }
warn() { printf '[%s] 注意：%s\n' "$ID" "$*" >&2; }
die()  { printf '[%s] 错误：%s\n' "$ID" "$*" >&2; exit 1; }

PORT="${WPQU_PORT:-8080}"

[ "$(id -u)" -eq 0 ] || die "需要 root 权限"
command -v apt-get >/dev/null 2>&1 || die "目前只支持 Debian / Ubuntu"
case "$PORT" in '' | *[!0-9]*) die "端口必须是数字：$PORT" ;; esac
[ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || die "端口超出范围：$PORT"

mirror_init
log "网络环境：$MIRROR_SUMMARY"

if [ -x /usr/local/bin/hello ]; then
  log "已经装过了，跳过安装"
else
  # ... 真正干活的部分 ...
  :
fi

log "完成。服务端口 $PORT"
cat <<EOF

记得放行 TCP $PORT：
  ufw allow $PORT/tcp
云厂商安全组也要放行。
EOF
```

### 4.3 幂等：重复执行不能变糟

面板可能在"用户手滑点了两次"、"上次中途失败重试"时重复执行：

- 装包前先查：`dpkg -s nginx >/dev/null 2>&1 || apt-get install -y nginx`
- 写配置前先备份：`[ -f "$conf" ] && cp -a "$conf" "$conf.wpqu.bak"`
- 建目录用 `mkdir -p`，覆盖文件用 `cat >`（不要 `>>` 追加重装）
- 已经在跑的端口/目录，明确拒绝并给出提示，而不是硬覆盖

### 4.4 必须做的输入校验

用户填的东西都会进 shell 和配置文件，至少校验这几类：

| 类型 | 写法 |
|---|---|
| 端口 | `case "$PORT" in '' \| *[!0-9]*) die ... ;; esac` 再判 1–65535 |
| 绝对路径 | `case "$WEBROOT" in /*) : ;; *) die "要用绝对路径" ;; esac` |
| 域名 | 只留 `A-Za-z0-9.-`，拒绝 `..` 和以 `-` 开头 |
| 数据库账号 | `A-Za-z0-9_` |
| 布尔/枚举 | `case "$MODE" in auto\|nginx\|apache) : ;; *) die ... ;; esac` |

不要把这些值拼进 `sed` / `eval`；要替换配置就用 `printf` 生成整个文件。

### 4.5 非交互与网络

- apt：`export DEBIAN_FRONTEND=noninteractive`，`apt-get install -y`，必要时 `-o Dpkg::Options::=--force-confdef`。
- 不要 `apt-get upgrade`，只装/升自己需要的包。
- 下载前先 `mirror_init`，然后用库给的变量（`$MIRROR_GH`、`$MIRROR_PYPI`、`$MIRROR_GO_DL`、`$MIRROR_NODE_DIST`、`$MIRROR_WP`），不要硬编码 `https://github.com`。
- 下载必须校验：官方 SHA256 或 GPG；拿不到校验值就在注释里写明原因。
- 不要 `curl ... | bash`。要执行远程安装器就先下载、再检查、再执行（`bash installer.sh`）。
- 加超时与重试：`curl --max-time 60 --retry 2`。

### 4.6 输出

- 每行前缀 `[<id>]`，长输出（apt、编译）可以原样透出。
- **进度用"第几步在做什么"**，不要打 ASCII 进度条。
- 结尾给人看的信息用 `cat <<EOF`：端口、账号、密码、要放行的防火墙端口、后续怎么改。
- 中间产生的密码只打印一次，同时写进 `/etc/wpqu/<id>/...`（权限 600），方便重装/排查。

### 4.7 失败

- 关键步骤失败就 `die`（退出码非 0），面板会显示第几步失败并记进操作记录。
- `|| true` 只能用在"失败也无所谓"的地方（`systemctl daemon-reload` 这类），不要用来掩盖安装失败。
- 中途失败时尽量回滚自己改过的配置（至少留备份并提示用户）。

### 4.8 状态文件

需要记住自己装过什么（路径、端口、生成的密码）时，写到这里：

```
/etc/wpqu/<id>/<实例标识>.env
```

例如 typecho 插件写 `/etc/wpqu/typecho/<slug>.env`，卸载时只读自己这一条，**不会误删别人的站点**。文件名里的实例标识要做字符过滤，防路径穿越。

---

## 5. 卸载脚本规范

```bash
#!/usr/bin/env bash
# 只删本插件安装时创建的东西；用户数据默认保留。
set -euo pipefail

ID=hello
log() { printf '[%s] %s\n' "$ID" "$*"; }
die() { printf '[%s] 错误：%s\n' "$ID" "$*" >&2; exit 1; }
```

规则：

1. **只删自己的**。有状态文件就按状态文件删；没有就按精确路径删，禁止 `rm -rf /var/www/*`、禁止 `typecho-*.conf` 这种通配。
2. **可重复执行**：删之前判存在，第二次跑不报错。
3. **用户数据不动**：数据库、上传文件、站点目录默认保留；确实要删时在标题和输出里写明"会删除数据"，并给 `--purge` 之类的显式开关（参数同样走 `WPQU_`）。
4. **不卸载系统基础包**（nginx、php-fpm、数据库），除非插件自己装的就是它，并且确认没有别的站点在用。
5. 停服务用 `systemctl disable --now <unit>`，然后 `rm -f /etc/systemd/system/<unit>`，最后 `systemctl daemon-reload`。

---

## 6. 面板侧插件（ui）

### 6.1 入口

`ui.entry` 指向包内文件，面板以 ESM 动态 `import()` 它，然后调用 `mount`：

```js
// ui/panel.js
export async function mount(el, ctx) {
  el.innerHTML = `<div class="card"><h3 class="card-title">Hello</h3></div>`;
}
```

- 必须 `export function mount(el, ctx)`；没有 `mount` 时界面显示"插件没有导出 mount 函数"。
- 每次切换页签都会重新 `mount`，不要依赖模块级状态做持久化（用 `localStorage` 或面板接口）。
- 可以 `export` 其它函数，但面板只调 `mount`。

### 6.2 ctx 提供什么

| 字段 | 说明 |
|---|---|
| `ctx.api(path, opts)` | 调面板自己的接口，自动带 `x-panel` 头、自动处理 423 解锁与错误 |
| `ctx.toast(msg, type)` | 轻提示，`type` 可为 `'err'` |
| `ctx.esc(str)` | HTML 转义，拼模板必须用 |
| `ctx.server` | 当前服务器对象（`id` / `name` / `host`），没选时可能是 `null` |
| `ctx.lang` | `'zh-CN'` 或 `'en'` |
| `ctx.pickLang(map)` | 从 `{ "zh-CN": ..., en: ... }` 里取当前语言 |

调用接口的写法（`body` 传 JSON 字符串）：

```js
const st = await ctx.api(`/api/servers/${ctx.server.id}/plugins/${id}/status`);
const r = await ctx.api('/api/servers/' + ctx.server.id + '/overview');
await ctx.api('/api/...', { method: 'POST', body: JSON.stringify({ a: 1 }) });
```

### 6.3 界面规范

- 只用面板已有的样式类与 CSS 变量：`card`、`card-head`、`card-title`、`btn`、`btn-primary`、`btn-sm`、`input`、`row`、`muted`、`hint`、`mono`、`--danger` / `--ok` / `--mono` 等。不要引入外部 CSS/字体/图片。
- 文字要中英双语：静态文案用 `ctx.pickLang()`，或直接写中文（英文界面会按词典回退，但**插件文案不在词典里**，所以自己判 `ctx.lang`）。
- 所有拼进 HTML 的变量都过 `ctx.esc`，需要富文本时自己构造 DOM 节点。
- 面板侧插件与面板**同源、同权限**，能调所有接口。文档里要提醒用户"只装可信来源的插件"。

### 6.4 存储

- 小配置用 `localStorage`（键名带 `wpqu-<id>:` 前缀，避免撞车）。
- 需要跨设备同步才用面板接口；目前没有"插件私有存储"接口，别去写面板的数据目录。

---

## 7. 打包与发布

### 7.1 命令

```bash
cd panel-go
go run ./cmd/wpqupack plugins -src ../plugins-src -site ../site -key ../site/.keys/release.key
```

产物：

```
site/plugins/<id>/<version>.zip
site/plugins/<id>/<version>.zip.sig
site/api/plugins/index.json（+ .sig）
```

`-dry-run` 只算不落盘，适合提交前看一眼会不会误发。

### 7.2 打包做了什么

| 行为 | 说明 |
|---|---|
| 校验 | 目录必须有 `plugin.json`；`id`/`version` 不能空；**目录名必须等于 id**；清单必须在包根目录 |
| 排除 | `.git`、`node_modules`、所有点开头的文件和目录、`Thumbs.db` |
| 换行 | `.sh` `.js` `.mjs` `.json` `.md` `.conf` `.service` 里的 CRLF 一律转 LF（Windows 上编辑过的脚本带 `\r`，远端会报 `$'\r'`） |
| 签名 | 用发布私钥对 zip 字节签名；面板下载后先验签再解压 |
| 索引 | 把 `plugin.json` 原样塞进 `index.json`，并补上 `package.url/sha256/size` |

### 7.3 只上线一部分插件

`-src` 会扫整个目录。还没准备好上线的插件（例如内部用的 `api-relay`），打包时把要发的目录拷到临时目录再打：

```bash
mkdir -p /tmp/plugsrc && cd /tmp/plugsrc
for p in docker golang nodejs notes openclaw phpmyadmin python typecho hello; do
  cp -a /path/to/plugins-src/$p .
done
cd /path/to/panel-go
go run ./cmd/wpqupack plugins -src /tmp/plugsrc -site ../site -key ../site/.keys/release.key
```

### 7.4 版本与旧包

- 改了脚本/界面就 bump `version`，否则已装用户收不到更新。
- 新版本打出来后，旧 zip 建议从 `site/plugins/<id>/` 删掉（索引里只留最新一条，旧文件就是死链和混淆源）。
- 版本号要能比大小（`1.0.10 > 1.0.9`），别用日期或 `beta`。

### 7.5 上线检查

1. 市场页 `site/plugins.html` 能看到卡片（分类、图标、价格、亮点都对）。
2. 面板「插件」页能刷到，详情页字段完整。
3. 全新服务器能装上；装完 `check` 变"已安装"且版本号正确。
4. 再点一次安装不报错（幂等）。
5. 卸载干净，再装一次仍然是好的。
6. 老版本机器升级到新版本（同 id 更高 version）能正常更新。

---

## 8. 本地测一个还没上传的包

面板默认从 `panel.wpqu.com` 拉索引，本地开发时用环境变量指到本地目录：

```bash
# 1. 起一个静态服务当分发站
python -m http.server 8799 --directory site

# 2. 面板启动前指过去（索引里的地址是相对路径，会被这个 base 拼起来）
export WPQU_UPDATE_BASE=http://127.0.0.1:8799
./dist/wpqu-panel-windows-amd64.exe --port 7790 --data /tmp/panel-dev   # 换成你当前平台的产物
```

这样测的是**刚打出来的 zip**，不用真上传。测完记得清掉环境变量。

### 自测清单（每次发插件前过一遍）

- [ ] 全新机器安装成功，结尾提示里端口/账号/密码都能用
- [ ] 装完刷新插件页，状态是"已安装"，版本号正确
- [ ] 重复点安装：不报错、不重复堆叠
- [ ] 端口被占用时给出可读的报错，而不是一堆 apt 输出
- [ ] 卸载后再装，功能正常
- [ ] 只读用户（非 root 登录）也能装（`sudo -n` 可用）
- [ ] 断网/慢网时有明确报错，不会挂 40 分钟
- [ ] 改动后 bump 了 version，旧 zip 已删
- [ ] `bash panel-go/scripts/sync-mirrors.sh --check` 通过
- [ ] 中英文文案都填了，英文界面没有残留中文（除专有名词）

---

## 9. 安全红线

插件在服务器上是 root，在面板里是同源脚本，所以：

1. **不执行未校验的远程内容**。必须 `curl -o file && verify && bash file`，不要管道直连 shell。
2. **不外传服务器数据**。除了下载安装包，不应有任何出站请求携带本机信息（IP、密钥、站点内容）。
3. **不改用户已有的站点**。要改 nginx/apache 配置，只新增自己的 vhost，并在卸载时删掉自己那条。
4. **不静默改系统源**。`mirror_init` 已有换源与回滚逻辑，不要自己再写一套。
5. **端口/路径先探测**。`ss -ltnp` 看端口占用，目录存在就停手并提示。
6. **不写死别人的账号密码**。生成的密码随机、只打印一次、落盘 600。
7. **不依赖面板内部文件**。插件脚本只应假设"标准 Linux 环境 + 网络"，不要读 `/etc/wp-panel/*`（那是面板自己的状态）。

---

## 10. 常见问题

| 现象 | 原因 | 处理 |
|---|---|---|
| 打包报「目录名与 id 不一致」 | 目录叫 `my-plugin`，id 写成 `myplugin` | 改一致，面板按 id 找目录 |
| 打包报「缺少 id 或 version」 | `plugin.json` 少字段 | 补上 |
| 市场里没有新插件 | 没重新打包 / 索引没上传 | 重跑 `wpqupack plugins`，传 `api/plugins/index.json` 与 `.sig` |
| 面板装完仍显示未安装 | `check` 命令在服务器上返回非 0 | 手动跑一遍 `check`，多半是路径写错或权限 |
| 已装的用户收不到更新 | 没 bump `version` | 改版本号重新打包 |
| 脚本报 `$'\r': command not found` | 文件是 CRLF | 打包会自动转；手工拷到服务器时要 `dos2unix` |
| 脚本报 `WPQU_XXX: unbound variable` | 参数为空没传，脚本直接引用了 | 全部改成 `${WPQU_XXX:-默认值}` |
| 参数没生效 | `key` 用了大写或中文 | `key` 只能小写字母数字下划线 |
| 安装卡住不动 | 有交互提示（apt 问 Y/n、needrestart） | 加 `DEBIAN_FRONTEND=noninteractive`，`apt-get -y` |
| 卸载删掉了别人的配置 | 用了通配符 | 改成按 `/etc/wpqu/<id>/` 状态文件精确删 |
| UI 页签点开空白 | `ui/entry` 路径错，或没 `export function mount` | 检查包内路径与导出名 |
| 界面样式错乱 | 用了外部 CSS/字体 | 只用面板的类与 CSS 变量 |

---

## 11. 完整最小模板

### `plugin.json`

见 2.1。

### `scripts/install.sh`

```bash
#!/usr/bin/env bash
# 安装 hello：写一个 systemd 服务，跑一个静态页。
# 由 WPQU panel 插件系统以 root 执行；参数走 WPQU_ 环境变量。
set -euo pipefail

# >>> mirror-lib BEGIN
# <<< mirror-lib END

ID=hello
log()  { printf '[%s] %s\n' "$ID" "$*"; }
warn() { printf '[%s] 注意：%s\n' "$ID" "$*" >&2; }
die()  { printf '[%s] 错误：%s\n' "$ID" "$*" >&2; exit 1; }

PORT="${WPQU_PORT:-8080}"
ROOT="${WPQU_ROOT:-/opt/hello}"

[ "$(id -u)" -eq 0 ] || die "需要 root 权限"
case "$PORT" in '' | *[!0-9]*) die "端口必须是数字：$PORT" ;; esac
[ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || die "端口超出范围：$PORT"
case "$ROOT" in /*) : ;; *) die "安装目录要用绝对路径" ;; esac

mirror_init
log "网络环境：$MIRROR_SUMMARY"

if [ -f "$ROOT/index.html" ]; then
  log "已经装过（$ROOT），跳过"
else
  mkdir -p "$ROOT"
  printf '<!doctype html><title>hello</title><h1>hello from %s</h1>\n' "$(hostname)" > "$ROOT/index.html"
  log "写入 $ROOT/index.html"
fi

cat > /etc/systemd/system/hello.service <<EOF
[Unit]
Description=Hello demo
After=network.target

[Service]
Type=simple
WorkingDirectory=$ROOT
ExecStart=/usr/bin/python3 -m http.server $PORT --bind 0.0.0.0
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now hello.service >/dev/null 2>&1
log "服务已启动"

cat <<EOF

访问：http://<服务器IP>:$PORT
改端口：面板里卸载后重装，或改 /etc/systemd/system/hello.service
放行端口：ufw allow $PORT/tcp
EOF
```

### `scripts/uninstall.sh`

```bash
#!/usr/bin/env bash
# 卸载 hello：只删本插件创建的服务与目录，用户自己放进 /opt/hello 的文件不删。
set -euo pipefail

ID=hello
log() { printf '[%s] %s\n' "$ID" "$*"; }

systemctl disable --now hello.service >/dev/null 2>&1 || true
rm -f /etc/systemd/system/hello.service
systemctl daemon-reload || true
rm -f /opt/hello/index.html
rmdir /opt/hello 2>/dev/null || true

log "已移除 hello（/opt/hello 里其它文件保留）"
```

---

## 12. 提交前最后一眼

- [ ] 目录名 = `id`，`version` 已 +1
- [ ] `plugin.json` 是合法 JSON（`python -m json.tool` 过一遍）
- [ ] 中英文都填了，`highlights` 3–4 条
- [ ] 脚本有 `set -euo pipefail`，参数全部 `${WPQU_X:-默认}`
- [ ] `check` 够快，`versionCmd` 只输出一行
- [ ] 卸载只删自己的
- [ ] `sync-mirrors.sh --check` 通过
- [ ] 本地 8799 源实测过安装 / 重复安装 / 卸载 / 重装
