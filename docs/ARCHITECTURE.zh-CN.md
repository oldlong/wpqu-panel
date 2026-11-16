# 架构说明

[English](ARCHITECTURE.md) ｜ **简体中文**

面板是怎么搭起来的，以及要加东西时该动哪里。使用说明见 [GUIDE.zh-CN.md](GUIDE.zh-CN.md)，
接口见 [API.zh-CN.md](API.zh-CN.md)。

- [1. 整体结构](#1-整体结构)
- [2. 仓库地图](#2-仓库地图)
- [3. 一次写操作的完整过程](#3-一次写操作的完整过程)
- [4. 脚本契约](#4-脚本契约)
- [5. Web 服务器适配层](#5-web-服务器适配层)
- [6. 服务器上的落盘结构](#6-服务器上的落盘结构)
- [7. 本地数据与加密](#7-本地数据与加密)
- [8. 前端](#8-前端)
- [9. 界面语言](#9-界面语言)
- [10. 加一个功能](#10-加一个功能)
- [11. 编译与发布](#11-编译与发布)
- [12. 插件与授权码](#12-插件与授权码)
- [13. 自升级与签名](#13-自升级与签名)
- [14. 测试方式](#14-测试方式)
- [15. 取舍](#15-取舍)

## 1. 整体结构

面板是一个**带浏览器界面的桌面工具**。它不跑在服务器上、不联网回传、除了你要它写的那些配置之外，
在服务器上不留任何状态。

```
          你的电脑                                  服务器
┌────────────────────────────────┐          ┌──────────────────────────────┐
│  wpqu-panel（单文件）          │          │                              │
│                                │          │  /tmp/.wpqu-panel-*.sh       │
│  public/   HTML+JS+CSS  ───────┼── 嵌入   │      │                       │
│  scripts/  bash         ───────┤          │      └─► env VAR=… bash …    │
│                                │          │             │                │
│  httpapi ──► ops.Runner ───────┼── SSH ──►│             ├─► nginx/apache │
│     ▲           │              │  exec    │             ├─► php-fpm      │
│     │           └─► sshc 连接池┼── SFTP ──►│             ├─► mariadb      │
│  浏览器（127.0.0.1:7788）      │          │             └─► certbot      │
└────────────────────────────────┘          └──────────────────────────────┘
```

由此有四个值得先知道的结论：

1. **面板自身不往服务器装东西。** 没有 agent、没有常驻进程、不开新端口。插件会装东西，但那是你点了
   安装之后跑的一串脚本，跟建站没有区别。
2. **脚本才是实现。** Go 代码主要做校验、上传脚本、解析回显。想知道某个动作到底干了什么，去读脚本。
3. **面板对站点无状态。** 站点列表每次都从 `sites-enabled` / `sites-available` 读回来，不存在会漂移的
   本地清单。
4. **唯一的出网请求是你主动触发的。** `panel.wpqu.com` 只在点「刷新市场」和「检查更新」时被访问；
   没有启动自检、没有心跳、没有遥测。

## 2. 仓库地图

```
main.go                  参数、端口顺延、单实例复用、打开浏览器
launcher.go              数据目录解析与旧目录迁移、日志落盘
ui_windows.go            rundll32 打开浏览器、无控制台时弹窗
ui_other.go              macOS/Linux 的对应实现

internal/store/          服务器档案、AES-256-GCM 凭据、ndjson 操作日志
internal/sshc/           SSH 连接池、带流式回调的 exec、SFTP 辅助
internal/sites/          概览、站点列表、站点详情（脚本文本 + 输出解析）
internal/ops/            上传执行器；部署、建站/转换/换域名/删站、证书、备份、体检、FTP、维护
internal/dbx/            数据库浏览、SQL 检查、导出导入优化
internal/httpapi/        路由、校验、ndjson 流、静态资源、文件管理
internal/version/        semver 比较（预发布版小于正式版）
internal/signing/        Ed25519 验签，两个公钥以常量编进二进制
internal/plugins/        插件清单、下载解压、授权码、批量状态检测
internal/updater/        更新检查、下载暂存、替换与重启（按平台分文件）

cmd/wpqusign/            签发密钥与授权码、验签（没有 openssl 依赖）
cmd/wpqupack/            打包插件与更新清单，产出 site/ 目录

public/index.html        外壳（侧栏、顶栏、抽屉、toast）
public/app.js            全部界面逻辑，无框架
public/style.css         设计变量 + 布局，明暗两套
public/i18n/             core.js（翻译器）+ zh-CN.js + en.js + dicts.js

scripts/*.sh             远端脚本，编译期嵌入
scripts/_ws-lib.sh       nginx / Apache 适配库（唯一真源）
scripts/sync-ws-lib.sh   把 _ws-lib.sh 注入需要它的五个脚本

plugins-src/<id>/        插件源：plugin.json + scripts/ + ui/，打包后进 site/
site/                    分发站（panel.wpqu.com 的根目录）：首页、清单、插件包、二进制
tools/release.sh         一次发版：校验版本号 → 交叉编译 → 打包插件与清单 → 检查签名
tools/sync-github.sh     把 panel-go 同步进开源目录 github/

build.sh、build.bat      本机与交叉编译
```

`public/` 与 `scripts/` 通过 `//go:embed` 打进二进制，所以改脚本或改 JS 都要重新编译。

## 3. 一次写操作的完整过程

以「新建站点」为例：

1. **浏览器**把 JSON POST 到 `/api/servers/:id/sites`，带 `x-panel: 1` 头
2. **httpapi** 取服务器、解密凭据、逐项校验（域名语法、站点类型、PHP 版本…）。校验放在开流**之前**，
   参数不合法时返回普通 JSON 错误，而不是开一个半截的流
3. **ops.Runner** 从嵌入文件系统读出脚本文本、把 CRLF 统一成 LF、用 SFTP 传到
   `/tmp/.wpqu-panel-<毫秒>-add-site.sh`，再比对远端字节数。SFTP 静默截断这种事排查起来极其痛苦，
   所以这里必须校验
4. **exec** 执行 `env DOMAIN=… bash /tmp/.wpqu-panel-… 2>&1`（非 root 用户前面加 `sudo -n`），
   stdout 与 stderr 合并后逐行返回
5. **httpapi** 把每段输出写成一行 JSON（`Content-Type: application/x-ndjson`）：

   ```json
   {"type":"start","message":"新增站点 example.com（wordpress）"}
   {"type":"out","text":"[+] 目录已创建：/var/www/example.com"}
   {"type":"out","text":"[+] 证书签发成功"}
   {"type":"end","code":0,"ok":true,"durationMs":18432,"message":"执行完成"}
   ```

   失败时最后一个事件是 `{"type":"error","message":"…"}`，同时记一条失败日志
6. 脚本从 `/tmp` 删除（放在 goroutine 里，成败都删），操作日志追加一行

浏览器把 `out` 事件原样塞进抽屉，所以进度显示看起来像终端——它**就是**脚本的输出。

## 4. 脚本契约

`scripts/` 里每个脚本都遵守同一套规矩，因为面板是把它当独立文件上传执行的：

| 规则 | 原因 |
|---|---|
| **自包含**：不 `source`，不依赖旁边的文件 | 上传的只有这一个文件 |
| **LF 换行** | CRLF 会让 bash 报 `$'\r': command not found`；Go 侧也会统一一次 |
| **入参走环境变量**，如 `DOMAIN`、`SITE_TYPE`、`SSL` | 没有引号问题、不用解析 argv、密码不会出现在 `ps` 里 |
| **不交互** | 没有 TTY，任何等待输入的语句都会挂到超时 |
| **给解析器的机器可读行**：`KEY=value` 与 `###SECTION` 标记 | `internal/sites`、`internal/dbx` 靠这些解析 |
| **退出码即结论**：0 成功，非 0 失败 | 面板把它映射成 `end` 事件里的 `ok` |
| **面向人的输出是中文 + 颜色**（`[+]`、`[x]`、`── 段落 ──`） | 原样显示在抽屉里，ANSI 码在服务端剥掉 |
| **尽量幂等** | 用户会重跑，初始化脚本也会用在半配好的机器上 |
| **覆盖前先备份，配置校验失败就回滚** | `nginx -t` / `apache2ctl configtest` 失败不能留下坏服务器 |

### 适配库为什么要注入

`scripts/_ws-lib.sh` 装的是 nginx 与 Apache 的全部差异：配置路径、域名指令、根目录探测、socket 探测、
校验与重载、证书插件、日志目录、站点类型标记。有五个脚本要用它（`add-site`、`change-domain`、
`delete-site`、`site-convert`、`php-manage`），但它们都不能 `source`——远端只有脚本自己。

所以是**注入**：`scripts/sync-ws-lib.sh` 把那五个文件里 `# >>> ws-lib BEGIN` 与
`# <<< ws-lib END` 之间的内容替换成当前 `_ws-lib.sh` 的全文。

```bash
./scripts/sync-ws-lib.sh          # 重写那五个脚本
./scripts/sync-ws-lib.sh --check  # 校验是否同步（不同步退出 1）
```

改 `_ws-lib.sh` → 跑一次同步 → 两个改动一起提交。**不要手改注入块**，下次同步会覆盖掉。

## 5. Web 服务器适配层

每个脚本开头 `ws_init` 决定这次面对的是 nginx 还是 Apache（跟正在跑的那个走；两个都在跑时优先 Apache，
因为那通常意味着 nginx 是被人为停掉的），之后脚本只调适配函数：

| 关注点 | nginx | Apache |
|---|---|---|
| 配置文件 | `/etc/nginx/sites-available/<域名>` | `/etc/apache2/sites-available/<域名>.conf` |
| 启用 | `sites-enabled` 里的软链 | `a2ensite` / `a2dissite` |
| 域名 | `server_name a b;` | `ServerName` + `ServerAlias` |
| 根目录 | `root /var/www/x;` | `DocumentRoot` |
| PHP | `fastcgi_pass unix:/run/php/x.sock` | `SetHandler "proxy:unix:/run/php/x.sock\|fcgi://localhost"` |
| 校验 / 重载 | `nginx -t`、`systemctl reload nginx` | `apache2ctl configtest`、`systemctl reload apache2` |
| 证书 | `certbot --nginx` | `certbot --apache` |
| 日志 | `/var/log/nginx/` | `/var/log/apache2/` |
| 伪静态 | 由 vhost 引入的片段 | 写进根目录的 `.htaccess` |

Apache 的 vhost 在能力变化时整份重写（PHP handler、静态站的 `Require all denied`、WordPress 段）。
已有证书时同时写 `:80`（含 `/.well-known` 例外）与 `:443` 两块，所以换类型不会丢 HTTPS。

站点类型标记挪到了 `/etc/wp-panel/site-types/<域名>`——不在任何一家 Web 服务器的目录里，两家互切时标记
跟着走。旧的 `/etc/nginx/site-types/` 仍会被读取并自动迁移。

## 6. 服务器上的落盘结构

```
/var/www/<域名>/                                  站点根目录
/etc/nginx/sites-available/<域名>                 vhost（nginx）
/etc/nginx/snippets/site-<域名>.conf              能力片段（PHP / 静态 / WP 规则）
/etc/wp-panel/site-types/<域名>                   wordpress | custom | static
/etc/php/<版本>/fpm/pool.d/<域名>.conf            PHP-FPM pool（只有勾了独立 pool 才有）
/etc/letsencrypt/live/<域名>/                     证书
/var/log/nginx/<域名>.{access,error}.log          日志

Apache：/etc/apache2/sites-available/<域名>.conf、sites-enabled/<域名>.conf、
        /var/log/apache2/<域名>.{access,error}.log、根目录的 .htaccess
```

面板「认识」一个站点，只因为这些文件存在。你把它们删掉，面板也会认账——没有一个需要同步的数据库。

## 7. 本地数据与加密

```
data/
├── servers.json    {"servers":[{"id","name","host","port","username","authType","credentials","…"}]}
├── secret.key      32 字节随机数，base64
├── panel.log       启动日志
├── logs/<日期>.ndjson
└── backups/<服务器 id>/
```

凭据（密码、私钥、passphrase）用 `secret.key` 里的每安装一密钥做 AES-256-GCM 加密，文件权限 `0600`。
密钥不出本机，首次启动用 CSPRNG 生成。`secret.key` 丢了就只能重新填凭据——这是设计上的取舍。

两个实例可以除了二进制什么都不共享：`PANEL_DATA` 指到别处，换一个端口即可。

## 8. 前端

没有框架、没有构建步骤：`app.js` 就是一个模板字符串渲染 + `state` 对象的单文件。渲染函数从 state
生成 HTML，事件处理器改 state 再重渲染。好处是改起来快、`public/` 改完刷新就见效；代价是文件巨大，
靠段落注释导航。

几个不那么显然的地方：

- **`api()`** 包住 `fetch`，自动加 `x-panel: 1` 头、解析 JSON、用服务端的错误文案抛异常
- **流式**用 `getReader()` 读 ndjson，把 `start` / `out` / `error` / `end` 分派到抽屉
- **主题**在首帧前就写到 `documentElement`（`index.html` 里的内联脚本），避免深色模式下先闪一下白底

## 9. 界面语言

翻译发生在**渲染之后**，不在模板里。这个决定让 i18n 层非常小：`app.js` 里一个 `t()` 调用都没有。

```
public/i18n/core.js     LANGS、lookup()、DOM 遍历、MutationObserver、数字模式
public/i18n/zh-CN.js    空对象——源语言就是键
public/i18n/en.js       { '中文原文': 'English text', … }（约 700 条）
public/i18n/dicts.js    登记表：{ 'zh-CN': zhCN, en }
```

流程：

1. 启动时 `initI18n()` 决定语言（`?lang=` → `localStorage` → `navigator.language`）
2. 遍历 `#app` 下的文本节点与 `placeholder` / `title` / `aria-label`，逐条查表替换
3. `MutationObserver` 负责后来才渲染出来的 DOM（切页签、弹窗、toast）；`WeakSet` 记住自己写过的节点，
   避免自己触发自己
4. `<pre>`、`<code>`、`<textarea>` 一律跳过：远端脚本输出、日志正文、用户输入不能动
5. 带数字的句子走正则（`站点 3 个` → `Sites: 3`、`证书剩 88 天` → `88 days left`），不用为每种数量
   单独登记条目
6. 查不到就返回 `null`，保留中文原文——只翻了一半的语言包也能用

**加一门语言**（改三个地方）：

```js
// 1. public/i18n/de.js
export default { '概览': 'Übersicht', /* … */ };

// 2. public/i18n/dicts.js
import de from './de.js';
export default { 'zh-CN': zhCN, en, de };

// 3. public/i18n/core.js
export const LANGS = [
  { code: 'zh-CN', label: '简体中文', short: '中' },
  { code: 'en', label: 'English', short: 'EN' },
  { code: 'de', label: 'Deutsch', short: 'DE' },
];
```

切换按钮会自动跟着 `LANGS` 循环，`?lang=de` 立刻可用。

**不翻译**的内容：远端脚本产生的文案（本来就是中文）、服务器名、域名、库名表名、日志正文、命令输出，
以及 Go 侧返回的接口错误信息。

## 10. 加一个功能

一个功能通常要动四层。以「重启某站点的 php-fpm」为例：

1. **脚本**：在对应的 `scripts/*.sh`（这里是 `php-manage.sh`）加一个由环境变量驱动的动作
   （`PHP_ACTION=restart`）。保持幂等、用 `[+]`/`[x]` 输出、失败非零退出；碰 Web 服务器配置就走
   `_ws-lib.sh`，改完跑 `sync-ws-lib.sh`
2. **Ops**：在 `ops.Runner` 上加一个方法，填好环境变量表调 `RunUploadedScript`
   （个别维护动作是把脚本里的默认块用 `strings.NewReplacer` 换掉）
3. **HTTP**：在 `internal/httpapi/http.go` 注册路由。要回显输出用 `streamRoute`，否则用普通
   `route("POST", …)`。校验放在开流之前，优先复用 `AssertDomain` / `assertPHPVersion` 这些现成的，
   别再手写正则
4. **界面**：`app.js` 里加按钮，调 `streamTo(url, body)` 或 `api(url, {method:'POST'})`。按钮文案要
   加进 `public/i18n/en.js`（中文原文就是键）；如果新句子带数字，再去 `core.js` 加一条模式
5. `./build.sh`——脚本是嵌入的，不重编不生效

## 11. 编译与发布

```bash
./build.sh                # 当前平台
./build.sh all            # windows amd64（GUI + 控制台）、linux amd64/arm64、darwin amd64/arm64
```

全部 `CGO_ENABLED=0`、`-trimpath`、`-ldflags "-s -w"`，GUI 版加 `-H=windowsgui` 才能双击不弹黑窗。
版本号在 `internal/httpapi/http.go` 的 `const Version`，`build.sh` 会读它打横幅。

发版走 `tools/release.sh <版本>`：校验 `const Version` 与参数是否一致 → `build.sh all` →
`wpqupack updates`（复制二进制到 `site/dl/<版本>/`、签 `updates.json`，只保留最近 20 个版本）→
`wpqupack plugins`（打包 `plugins-src/`、签索引与每个包）→ 检查每个 `.json` / `.zip` 都有 `.sig`。
产物是 `site/`，整个目录就是要传到 `panel.wpqu.com` 的东西。

发布包目前是本地编译后手工传到 GitHub release，还没有 CI。要加的话，`./build.sh all` 加
`dist/SHA256SUMS` 就够了；`site/` 也可以用同一套脚本在 CI 里生成，但私钥不能进 CI。

## 12. 插件与授权码

插件是「面板没做、但用户确实需要」的那部分能力的外挂：装运行时、装 Docker、装 OpenClaw，或者给面板
自己加一个页签。设计上刻意让**插件不进入主流程**——面板不认识任何具体插件，只认识一份清单。

### 一份清单，两种形态

`plugins-src/<id>/plugin.json` 同时描述两件事：

```json
{
  "id": "golang", "version": "1.0.0", "category": "runtime", "tier": "free",
  "install": {
    "check": "test -x /usr/local/go/bin/go",
    "steps": [{ "title": {"zh-CN": "安装 Go"}, "file": "scripts/install.sh" }],
    "uninstall": [{ "file": "scripts/uninstall.sh" }],
    "params": [{ "key": "version", "default": "" }]
  },
  "requires": { "os": ["linux"], "minPanel": "1.9.0", "license": false }
}
```

- 有 `install.steps` → 会往服务器装东西，跑的就是这些脚本，跟主流程的建站脚本走同一个执行器
- 有 `ui.entry` → 给面板加页签，`entry` 指向包里的一个 JS 模块，导出 `mount(el, ctx)`
- 两者可以同时存在（先装服务，再给个面板看状态）

**面板代码里没有 `switch pluginID`。** 加一个新插件只需要加一个目录和一条清单项，不动 Go。

### 安装链路

```
POST /api/servers/:id/plugins/:pid/install
  ├─ 面板版本 ≥ requires.minPanel          （太老的面板不认识新字段，装上去只会报错）
  ├─ requires.license → Manager.License()  （本地验签 + 查到期）
  ├─ 远端 uname -s ∈ requires.os           （配方全是 Linux 包管理命令）
  ├─ params 的键转 WPQU_* 环境变量          （非法字符直接丢，不塞进 shell）
  ├─ Manager.Ensure()                      （下载 → 校验 → 解压，见下）
  └─ 逐个 step 跑 RunScriptText()          （ndjson：start / step / out / end）
```

`Manager.Ensure()` 的顺序不能换：

1. 已装同版本且目录在 → 直接返回，不重复下载
2. 下载 `package.url`（相对路径拼在 `Base` 上）
3. 比对 `package.sha256`
4. 拉 `url + ".sig"` 并用**发布公钥**验包签名
5. 解压到 `.staging-<id>`（拒 `..`，限 800 文件 / 64MB）
6. 校验**包内** `plugin.json` 的 `id` 与索引一致——防止拿 A 的包冒充 B
7. `RemoveAll(dest)` + `Rename(staging, dest)`，原子换上去

任何一步失败，旧目录原样保留；staging 目录会被删掉，不会留半个插件。

### 状态检测为什么只有一次 SSH

装没装、什么版本，本来是每插件一条命令。N 个插件就是 N 次往返，在丢包链路上很难看。所以
`StatusScript()` 把 N 段 `if/then` 拼成**一个**脚本，每段前面打一个 `###<id>` 标记，一次执行完。

```
echo "###golang"; if test -x /usr/local/go/bin/go; then echo "1 $(… --version)"; else echo "0"; fi
echo "###docker"; if command -v docker >/dev/null; then echo "1 $(docker -v)"; else echo "0"; fi
```

`ParseStatus()` 逆向解析时有两个坑：脚本输出没有换行（`0###docker` 粘一起）和命令往 stderr 吐杂音。
前者按 `###` 切再补换行，后者只认紧凑格式的行，认不出就当作未安装。

### 授权码

```
WPQU1.<base64url(payload)>.<base64url(ed25519_sig)>
payload = {"p":"openclaw","e":"2027-12-31","s":5,"n":"张三","i":"lic_7f3a","t":1730000000}
```

选**离线签名**而不是「账号 + 联网校验」的原因很直接：面板本身不联网、不上传、不占端口。要它联网验
授权，就得引入一个服务器地址、一个账号体系和一个失效策略，把「单机工具」变成「客户端」。而收益只是
防住那些本来就不会付钱的人。

代价是**没有吊销**。过期能管，退款管不了——要管就得联网。这个取舍写在 README 的「收费插件」里。

落盘是 `data/licenses.json`（`{keys:{<id>:<code>}}`），`/api/plugins` 只回显解出来的摘要
（客户名、到期日、席位数），不把原文吐回去。

### UI 插件的资源

`ui.entry` 装好后由 `/api/plugin-assets/<id>/<path>` 提供。只读、只服务已解压插件目录内的文件，
路径先 `Clean` 再拼，出界就 404。插件 JS 在面板页面里跑，所以它跟面板同权——这也是为什么 UI 插件
要签名。

## 13. 自升级与签名

「面板自己更新自己」是个别扭的问题：正在运行的二进制不能覆盖自己。这里拆成三段，每段都可以失败而
不影响现有安装。

### 三段式

| 阶段 | 函数 | 关键点 |
|---|---|---|
| 检查 | `Client.Check()` | 拉 `updates.json`、验签、按 `GOOS/GOARCH` 选包、判 `minSupported` |
| 暂存 | `Client.Stage()` | 边下边算 SHA256，写 `data/tmp/*.part`，校验通过才 rename 成正式名 |
| 替换 | `Restart(staged)` | 平台相关，见下；失败回滚 |

`Check()` 不缓存结果，因为 `/api/update/apply` 会**重新**调它——客户端传来的 URL 和哈希一律不采信，
否则这个接口就成了「下载任意 URL 并以 root 跑」。

### Windows：重命名而不是删除

Windows 上运行中的 exe 不能删、不能覆盖，**但可以改名**。所以：

```
copy staged → <exe>.new
Rename(<exe>, <exe>.old)        // 运行中的文件，改名是允许的
Rename(<exe>.new, <exe>)        // 失败就把 .old 改回来
StartProcess(<exe>, 原参数)      // Env 里加 WPQU_RESTARTED=1
```

`.old` 留到下次启动由 `CleanupOld()` 删——如果新版本起不来，用户把 `.old` 改回正式名就退回去了。

`WPQU_RESTARTED=1` 不是装饰：`main.go` 平时会探测端口段上有没有同版本的实例，有就复用并退出。新进程
继承这个变量后跳过复用逻辑，直接接管端口。

第一版试过「写一个 `.cmd`，等父进程退出后再替换」，在无控制台的 GUI 进程里 `.cmd` 立刻就退出了，
`.new` 已就位但 `.old` 没换回来、新进程也没起——所以改成同步改名，每一步都能判断成败。

### Linux / macOS：rename + exec

同目录 `os.Rename` 是原子的，随后 `syscall.Exec` 原地替换进程镜像（不 fork、不返回）。父进程的
PID、监听 fd、控制台都保持不变，所以端口不需要重新绑定，`-data` 参数也不用重新解析。

### 谁能升级自己

`CanSelfUpdate()` 看两件事：

- 不是 `go run` 起的（临时目录里的二进制，换完就丢了）
- exe 所在目录可写（macOS 的 `/Applications`、Linux 的 `/usr/local/bin` 都可能不可写）

不满足就把原因显示在「关于」里，按钮置灰。宁可明说，也不要装完之后起不来。

### 两个密钥，两条链

| 密钥 | 签什么 | 公钥常量 |
|---|---|---|
| `release.key` | `updates.json`、插件 `index.json`、插件 `.zip` | `signing.ReleasePublicKey` |
| `license.key` | 用户授权码的 payload | `signing.LicensePublicKey` |

分开是为了让「发版能力」和「卖授权的能力」可以交给不同的人，也为了让泄露一个不至于两个都完蛋。

签名对象是**文件原始字节**，不是解析后的 JSON。这是个刻意的选择：JSON 序列化在键顺序、空格、数字
格式上都没有保证，只要有一端重新序列化就验不过。`wpqupack` 因此先拼好要落盘的 `body`，再对同一个
`body` 签名并写盘——曾经因为「签名后多写了一个换行」导致面板报签名失败，就是踩在这个点上。

私钥只在本机 `site/.keys/`，`.gitignore` 排除，丢了就没法发新版。公钥以常量编进二进制，所以换密钥
必须重新编译——这是有意的，跟「可注入 `PublicKey` 字段」并存：字段供单元测试和内网自建源用。

## 14. 测试方式

单元测试覆盖纯逻辑，真机验证覆盖「bash 和 systemd 到底怎么想」。

```bash
go test ./...            # version / plugins / updater 三个包
gofmt -l .               # 应当无输出
go vet ./...
```

| 包 | 测什么 |
|---|---|
| `internal/version` | 版本比较、预发布版小于正式版、两段版本号不越界 |
| `internal/plugins` | 授权码往返/过期/篡改/空白容错、状态解析（含 tab 粘连与杂音）、`Ensure` 的哈希不符与包内 id 不符、zip 越界、`StatusScript` 保留多行 |
| `internal/updater` | 发现更新、已是最新、拒绝被篡改的清单、无本平台包、`Stage` 落地、拒绝错误哈希 |

验签相关的用例都用 `PublicKey` 字段注入测试密钥，不依赖真实私钥。

真机循环：

1. 用源码直接跑，指向一台可以随便折腾的服务器：
   `PANEL_DATA=./devdata go run . -no-browser -port 7789`
2. 把改过的功能在**真机**上走一遍。有意思的地方几乎都是 bash 在跟 systemd、包管理器、配置文件打交道
3. 走反面路径：域名不合法、80 端口被封、证书已存在、类型不对、配置校验失败、插件装到一半断网
4. 动了适配库就跑一次 `./scripts/sync-ws-lib.sh --check`
5. 动了升级链路，就在副本目录里跑一次 `WPQU_UPDATE_BASE=<本地镜像>`，确认替换后哈希变了、`.old`
   还在、新进程的 `version` 是新的

`data/logs/<日期>.ndjson` 是回溯「到底跑了什么、花了多久」最快的方式；升级过程另有 `data/update.log`。

## 15. 取舍

| 决定 | 代价 | 收益 |
|---|---|---|
| 脚本编译期嵌入 | 改脚本要重新编译 | 单文件分发；二进制与脚本不可能对不上 |
| 不校验主机密钥 | 首次连接被中间人劫持发现不了 | 新装 VPS 开箱即用；将来可加 TOFU 指纹固定 |
| 没有登录 | 只能监听本机 | 没有用户库、没有会话、没有能被写错的 Web 认证 |
| 站点列表从配置读 | 概览稍慢 | 面板与现实不会脱节 |
| i18n 做在 DOM 上 | 动态句子要用正则兜 | 模板保持可读，新写的界面自动被覆盖 |
| 后端文案以中文为主 | 英文用户会看到中文报错 | 改动面小得多，界面语言已覆盖面板本身 |
| 授权码离线验签 | 无法吊销，退款管不了 | 面板保持单机：不联网、不登录、不占端口 |
| 签名对象是文件字节 | 任何重新序列化都会验签失败 | 两端不可能在 JSON 格式上扯皮 |
| 插件脚本以 root 执行 | 恶意插件等于拿到服务器 | 运行时安装本来就需要 root；以此换掉「面板自带一堆安装逻辑」 |
| 升级不信任客户端参数 | 每次 apply 都要重新拉一次清单 | 这个接口不会变成「下载任意 URL 并以 root 替换自己」 |
