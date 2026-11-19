# HTTP 接口

[English](API.md) ｜ **简体中文**

浏览器能做的事，`curl` 都能做。这份文档列出全部接口；请求字段与界面实际发送的 JSON 一致。

## 约定

- **地址** `http://127.0.0.1:7788`（或面板实际选中的端口）
- **写操作必须带头**：除 `GET` 之外都要带 `x-panel: 1`，否则返回 `403`。这是 CSRF 防护——随机网页发起的
  跨域 `POST` 无法在不触发预检的情况下加自定义头
- **错误**是 `{"error": "…"}`，参数问题 `400`，其余 `500`。错误文案目前是中文
- **成功**是 `{"ok": true, …}` 加各接口自己的字段
- **id** 形如 `srv_1a2b3c4d5e6f7a8b`，重启后不变
- **大体积上传**（文件、SQL 导入）用 `multipart/form-data`，其余都是 JSON

```bash
curl -s http://127.0.0.1:7788/api/ping
# {"impl":"go","ok":true,"time":"2026-11-16T22:41:07+08:00","version":"1.10.1"}

curl -s http://127.0.0.1:7788/api/servers -H 'x-panel: 1'
```

## 流式接口

要跑远端脚本的操作返回 `Content-Type: application/x-ndjson`，一行一个 JSON 对象：

```json
{"type":"start","message":"新增站点 example.com（wordpress）"}
{"type":"out","text":"[+] 目录已创建：/var/www/example.com"}
{"type":"out","text":"[+] 证书签发成功"}
{"type":"end","code":0,"ok":true,"durationMs":18432,"message":"执行完成"}
```

| type | 含义 |
|---|---|
| `start` | 参数已接受，`message` 是这次操作的描述 |
| `step` | 多步操作的分步标记：`index`、`total`、`title`（目前插件安装/卸载会发） |
| `out` | 一段脚本输出（ANSI 颜色已剥掉） |
| `progress` | 进度：`pct`（0–1）、`message`（目前只有面板自升级会发） |
| `error` | 终止性失败，`message` 是原因；后面不会再有 `end` |
| `end` | 终止，`code` 是脚本退出码，`ok` 等于 `code == 0` |

参数校验在开流之前，所以请求不合法时返回的是普通 JSON `400`，而不是半截的流。超时也以 `end` 结束，
但 `code` 为 `-1`、`message` 带「（超时）」。

## 基础

| 方法 | 路径 | 参数 | 返回 |
|---|---|---|---|
| GET | `/api/ping` | — | `{ok, impl, version, time}`，面板用它在端口段里辨认自己的其它实例 |
| POST | `/api/shutdown` | — | `{ok}`，请求进程退出 |
| GET | `/api/logs` | `limit`（1–300，默认 100） | `{logs:[…]}` 操作记录，新的在前 |

## 服务器

| 方法 | 路径 | 参数 | 返回 |
|---|---|---|---|
| GET | `/api/servers` | — | `{servers:[…]}`（不含凭据） |
| POST | `/api/servers` | `name, host, port, username, authType ("password"\|"key"), password, privateKey, passphrase, note` | 新建的服务器 |
| PATCH | `/api/servers/:id` | 上述任意字段 | 更新后的服务器 |
| DELETE | `/api/servers/:id` | — | `{ok}`（只删本地档案，不动服务器） |
| POST | `/api/servers/:id/test` | — | `{ok, output}` SSH 连通性探测 |
| GET | `/api/servers/:id/doctor` | — | `{ok, doctor:{checks:[…]}}` 只读体检 |
| GET | `/api/servers/:id/overview` | — | 资源、版本、站点摘要 |
| GET | `/api/servers/:id/site-detail` | `domain` | 单个站点：根目录、类型、PHP socket、SSL、占用、文件数 |

### 服务器保险库（可选的按服务器本地加密）

启用后，该服务器的凭据会用口令派生密钥（scrypt → AES-256-GCM）重加密。保险库未解锁时，
需要凭据的接口返回 **HTTP 423**，响应带 `error` + `code`：`vault_locked`（需要解锁）、
`vault_lockout`（错次过多锁定，带 `lockSeconds`）、`vault_bad_password`（口令错误，带
`remaining`；连错 5 次锁 1 小时）。解锁成功后有 30 分钟会话。

| 方法 | 路径 | 参数 | 返回 |
|---|---|---|---|
| GET | `/api/servers/:id/vault` | — | `{enabled, setAt}`（未启用时 `enabled:false`） |
| POST | `/api/servers/:id/vault` | `password`；改密时另带 `oldPassword` | `{ok}` |
| POST | `/api/servers/:id/vault/unlock` | `password` | `{ok}`（开启 30 分钟会话） |
| POST | `/api/servers/:id/vault/lock` | — | `{ok}`（清掉内存密钥） |
| POST | `/api/servers/:id/vault/reset` | `confirm: true` | `{vault:{enabled:false}, needsCreds:true}`：忘密码时用，口令与凭据一起清掉，之后必须重新填 SSH 凭据；锁定期也能调 |
| DELETE | `/api/servers/:id/vault` | `password` | `{ok}`（凭据改回机器密钥加密） |

口令只校验长度（≥ 6 位），不限制字符种类。

注意：已加密服务器上 `PATCH /api/servers/:id` 同样要求先解锁（否则 423 `vault_need_password`）。
`POST /api/servers/:id/test` 不涉及保险库状态。

## 站点

全部是流式接口。

| 路径 | 参数 | 作用 |
|---|---|---|
| `POST /api/servers/:id/bootstrap` | `webServer ("nginx"\|"apache"), phpVersion ("8.0"…"8.4"，空=发行版默认), dbEngine ("mariadb"\|"mysql84"\|"mysql57"), domain, title, adminUser, adminPass, ssl, redis` | 装环境并配第一个站点 |
| `POST /api/servers/:id/sites` | `domain, type ("wordpress"\|"custom"\|"static"), title, adminUser, adminPass, locale ("zh_CN"\|"en_US"), separatePool, maxChildren (1–12), ssl` | 新建站点 |
| `POST /api/servers/:id/site-type` | `domain, type, title, adminUser, adminPass, locale, separatePool, maxChildren, force, dryRun` | 三种类型互转 |
| `POST /api/servers/:id/domain` | `oldDomain, newDomain, ssl, keepRedirect, moveDir, dryRun` | 换域名；`keepRedirect` 决定是否写旧域名的 301 vhost |
| `POST /api/servers/:id/site-delete` | `domain, dbName, deleteFiles, deleteDb, deleteCert` | 删站，可一并删文件 / 数据库 / 证书 |
| `POST /api/servers/:id/ssl/issue` | `domain, email` | 签发或扩项证书 |
| `POST /api/servers/:id/ssl/renew` | — | 续期所有证书 |

## 文件

路径是服务器上的绝对路径。读不设限（SSH 用户能读就能读），写走 SFTP。

| 方法 | 路径 | 参数 | 说明 |
|---|---|---|---|
| GET | `/api/files` | `server, path` | 目录列表，含权限、大小、修改时间 |
| GET | `/api/file` | `server, path` | 文件内容（文本，有大小上限） |
| PUT | `/api/file` | `server, path, content` | 保存编辑器内容 |
| POST | `/api/file/upload` | multipart，query `server, dir, name` | 上传单个文件 |
| GET | `/api/file/download` | `server, path` | 下载，流式 |
| POST | `/api/file/mkdir` | `server, path` | 建目录 |
| POST | `/api/file/rename` | `server, from, to` | 重命名或移动 |
| POST | `/api/file/delete` | `server, path` | 删文件或目录 |
| POST | `/api/file/chmod` | `server, path, mode ("755"), recursive` | 只改 mode 位 |
| POST | `/api/file/batch` | `server, action ("chmod"\|"delete"\|"copy"\|"move"), paths[], mode, recursive, dest` | 批量操作 |

## 数据库

| 方法 | 路径 | 参数 | 说明 |
|---|---|---|---|
| GET | `/api/servers/:id/db/databases` | — | 库列表，含大小与表数 |
| GET | `/api/servers/:id/db/tables` | `db` | 表列表，含行数、大小、引擎 |
| GET | `/api/servers/:id/db/rows` | `db, table, limit, offset` | 数据行 + 总行数 |
| GET | `/api/servers/:id/db/structure` | `db, table, kind` | `kind` 取 `columns`（默认）或 `indexes` |
| POST | `/api/servers/:id/db/query` | `db, sql` | 执行一条语句；写语句先过检查 |
| GET | `/api/servers/:id/db/export` | `db`（空=整台） | gzip 压缩的 dump，流式下载 |
| POST | `/api/servers/:id/db/import` | multipart，query `db, name, backup` | 导入 `.sql` / `.sql.gz` |
| POST | `/api/servers/:id/db/optimize` | `db, table, mode` | `OPTIMIZE TABLE` |

会被拒绝的语句：`CREATE/DROP/ALTER USER|ROLE`、`GRANT`、`REVOKE`、`SET PASSWORD`、`LOAD DATA`、
`INTO OUTFILE|DUMPFILE`、`LOAD_FILE()`、`INSTALL/UNINSTALL PLUGIN|SONAME`。

## FTP

| 方法 | 路径 | 参数 |
|---|---|---|
| GET | `/api/servers/:id/ftp` | —（状态、账号、配置） |
| POST | `/api/servers/:id/ftp/control` | `action ("start"\|"stop"\|"restart")` |
| POST | `/api/servers/:id/ftp/config` | 被动端口段、被动地址、TLS 选项 |
| POST | `/api/servers/:id/ftp/users` | `name, password, dir, writable` |
| POST | `/api/servers/:id/ftp/users/delete` | `name` |
| POST | `/api/servers/:id/ftp/users/dir` | `name, dir` |
| POST | `/api/servers/:id/ftp/users/password` | `name, password` |
| POST | `/api/servers/:id/ftp/users/mode` | `name, writable` |

`POST /api/servers/:id/ftp/install` 与 `/ftp/uninstall` 是流式的（要调 apt）。

## 维护

| 方法 | 路径 | 参数 | 说明 |
|---|---|---|---|
| GET | `/api/servers/:id/php` | — | 已装版本、站点绑定、默认 CLI |
| POST | `/api/servers/:id/php/install` | `version` | 流式；需要时自动加 Sury/ondrej 源 |
| POST | `/api/servers/:id/php/remove` | `version, force` | 流式 |
| POST | `/api/servers/:id/php/repo` | — | 流式；只加第三方软件源 |
| POST | `/api/servers/:id/php/site` | `domain, version, force` | 把某个站点切到另一版本 |
| POST | `/api/servers/:id/php/default` | `version` | 设默认 CLI 版本 |
| GET | `/api/servers/:id/logs/list` | — | 白名单内的日志文件 |
| POST | `/api/servers/:id/logs/tail` | `path, lines` | 取尾部，`.gz` 自动解压 |
| POST | `/api/servers/:id/logs/clear` | `path` | 原地截断（流式） |
| POST | `/api/servers/:id/logs/rotate` | `path` | 压缩 + 截断（流式） |
| POST | `/api/servers/:id/logs/delete` | `path` | 只对归档（流式） |
| GET | `/api/servers/:id/cron` | — | root crontab，带 `managed` 标记 |
| POST | `/api/servers/:id/cron/add` | `schedule, command, label` | 流式 |
| POST | `/api/servers/:id/cron/delete` | `id, line` | 流式 |
| POST | `/api/servers/:id/cron/toggle` | `line, enabled` | 流式 |
| POST | `/api/servers/:id/cron/run` | `command` | 立即执行一次，流式 |
| POST | `/api/servers/:id/exec` | `command, cwd, seconds` | root shell，流式；危险命令界面会拦一道 |

## 紧急修复

清单由分发站下发（`/api/security.json` + `.sig`），命令全部来自清单，接口只接受修复编号。

| 方法 | 路径 | 参数 | 说明 |
|---|---|---|---|
| GET | `/api/security/feed` | `refresh=1` 可选 | 公告 + 当前面板能执行的修复项（已按 `minPanel` 过滤） |
| POST | `/api/servers/:id/security/scan` | `refresh` 可选 | 在服务器上跑一遍检测，返回每项 `affected / ok / skip` |
| POST | `/api/servers/:id/security/fix` | `id` | 流式；执行该修复项的步骤（apt 升级 + 清单里的校验命令） |

## 备份

| 方法 | 路径 | 参数 | 说明 |
|---|---|---|---|
| POST | `/api/servers/:id/backup` | `domain, includeDb, includeFiles` | 流式；归档落在 `data/backups/<服务器 id>/` |
| GET | `/api/servers/:id/backups` | — | `{backups:[…]}`，含文件名、大小、时间 |

## 插件

插件分两种形态：带 `install` 的会往服务器装东西，带 `ui` 的只给面板加一个页签（有些两者都有）。
清单来自 `panel.wpqu.com` 并在本地验签，验不过 `/api/plugins` 返回 `catalog: []` 加 `error`。

| 方法 | 路径 | 参数 | 说明 |
|---|---|---|---|
| GET | `/api/plugins` | — | `{catalog, installed, licenses, uiTabs, source, updated, panel, platform}`；`error` 非空表示清单拉取或验签失败 |
| GET | `/api/servers/:id/plugins/status` | — | `{status:{"<插件 id>":{installed, version}}}`，一次 SSH 批量检测 |
| POST | `/api/plugins/:id/license` | `key` | 本地验签后落盘，返回 `{ok, product, name, expire, seats}` |
| DELETE | `/api/plugins/:id/license` | — | `{ok}`，删除本机的授权码 |
| DELETE | `/api/plugins/:id` | — | `{ok}`，移除面板侧插件（含已解压的包），不动服务器 |
| POST | `/api/plugins/:id/install` | — | 装 `ui` 插件到面板本地 |
| POST | `/api/servers/:id/plugins/:pid/install` | `params`（对象，键见插件 `install.params`） | 流式；在服务器上跑安装脚本 |
| POST | `/api/servers/:id/plugins/:pid/uninstall` | — | 流式；跑卸载脚本 |

安装前的检查按顺序做，任一条不过就 `400`：面板版本满足 `requires.minPanel`、收费插件有有效授权码、
服务器 `uname -s` 在 `requires.os` 里。`params` 的键会大写并加 `WPQU_` 前缀当环境变量传给脚本
（`{"major":"22"}` → `WPQU_MAJOR=22`）；键里出现非 `A-Z0-9_` 的字符（比如 `major.minor`）会被静默丢弃，
所以别用带点的键。

```bash
# 看市场
curl -s http://127.0.0.1:7788/api/plugins -H 'x-panel: 1' | jq '.catalog[].id'

# 贴授权码
curl -s -X POST http://127.0.0.1:7788/api/plugins/openclaw/license \
  -H 'x-panel: 1' -H 'content-type: application/json' \
  -d '{"key":"WPQU1.eyJwIjoib3BlbmNsYXci...","x":0}' | jq

# 装到服务器（流式）
curl -sN -X POST http://127.0.0.1:7788/api/servers/srv_xxx/plugins/golang/install \
  -H 'x-panel: 1' -H 'content-type: application/json' -d '{"params":{}}'
```

授权码本身是 `WPQU1.<payload>.<sig>`，payload 里带产品 id、到期日、席位数、客户名。面板只做本地
Ed25519 验签，不联网、不占端口。

`/api/plugin-assets/<插件 id>/<entry>` 提供 `ui` 插件的前端文件，只读、限定在已解压的插件目录内。

## 更新

只更新面板自己。

| 方法 | 路径 | 参数 | 说明 |
|---|---|---|---|
| GET | `/api/update/check` | — | `{version, canSelfUpdate, reason, source, result}` |
| POST | `/api/update/apply` | — | 流式；重新检查后下载、替换、重启 |

`result` 是清单比对结果：`{available, latest, notes, critical, size, sha256, skipped, tooOld}`。
版本已最新时 `available:false`，`skipped` 说明原因（如「没有当前平台的包」）。
检查失败**不返回 5xx**——`error` 字段带原因、HTTP 仍是 `200`，因为断网是常态，界面要能把原因显示出来。

```bash
curl -s http://127.0.0.1:7788/api/update/check -H 'x-panel: 1' | jq '.result, .canSelfUpdate'
```

`POST /api/update/apply` 时服务端会**重新**检查一遍，不采信客户端传来的 URL 或 SHA256。流里依次是
`start` → `progress`（下载字节数）→ 校验 → 替换 → `end`；成功后约 700ms 进程退出并自我重启，
所以客户端要能容忍连接断开。`canSelfUpdate:false`（`go run` 起的、或目录不可写）时这个接口直接 `400`。

## 静态资源

| 路径 | 内容 |
|---|---|
| `/` | `public/index.html` |
| `/app.js`、`/style.css` | 界面代码 |
| `/i18n/*.js` | 语言包 |
| `/api/plugin-assets/<id>/<file>` | `ui` 插件的前端文件 |

## 脚本化调用注意

- 面板是单用户、无认证的：`127.0.0.1` 上任何程序都能调它，别暴露到别处
- 同一台服务器上的操作**不排队**：两个 `POST /sites` 同时发会争抢包管理器和配置文件，请串行
- 长操作（初始化、装 PHP）可能跑几分钟，请读流，不要设很短的客户端超时
