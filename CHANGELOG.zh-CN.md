# 变更记录

[English](CHANGELOG.md) ｜ **简体中文**

重要变更，新的在前。版本号即 `/api/ping` 返回的 `version`。

## 管理后台 1.0（独立工具，版本与面板解耦）

新增 `wpquadmin`，一个只监听 `127.0.0.1` 的网页后台，给自己人管分发站用：

- **官网内容**：`site/` 与 `plugins-src/` 下的文本文件在线编辑，保存前自动留历史版本，可一键回滚
- **插件市场**：改 `plugin.json`（价格、分类、文案）、版本进位、打包发布；列表里直接显示线上与本地是否一致
- **授权码**：按插件签发（单个或批量）、CSV 导出、验码、台账作废。台账与吊销无关，界面里写明
- **升级发布**：把 `dist/` 里的新二进制推进 `site/dl/`，重写并签名 `updates.json`，历史版本保留 20 个
- **自检**：`-check` 校验站点目录、私钥公钥与面板内置公钥一致、管理员口令已设置，任一不过非零退出

**双击即用**：Windows 上直接双击 exe 就行。启动时按 exe 位置往上找 `site/`、`plugins-src/`、`dist/`，配置写在 exe 旁边（不依赖当前目录）；口令不存在就生成一个并同时打印在窗口与弹窗里；起来后自动开浏览器；端口被占自动顺延；已经在跑则只把浏览器指过去，不起第二份去抢同一批台账文件。

工程上把打包逻辑抽到 `internal/pack`、发码逻辑抽到 `internal/licensing`，`wpqupack` / `wpqusign` 与新后台共用同一份代码——所以后台打的包与命令行逐字节相同，两边可以混着用。不参与面板的升级清单，`./build.sh admin` 单独产出到 `dist/admin/`。

## 1.9.2

**按服务器设密码（可选）**

- 右上角新增「设置密码」：只对当前这台服务器生效，不设也能照常用面板
- 设了之后，SSH 凭据改用 `scrypt`（N=32768, r=8, p=1）派生的密钥重新做 AES-256-GCM 加密，原来那份机器密钥加密的 `secret` 会被删掉——`data/secret.key` 解不开它，整个 `data/` 拷走也看不到明文
- **不校验密码强度**，一位数字也能设；密码本身不落盘（只存 salt 与验证器），忘了只能移除服务器重新添加
- **连错 5 次锁 1 小时**：次数与 `LockedUntil` 都写进 `servers.json`，重启面板不会清零；锁定期间正确的密码也不放行
- 解锁后 30 分钟内免密（每次使用续期，进程重启失效）；可以随时「立即锁定」、改密码或关闭加密
- 未解锁时所有服务器接口返回 `423` + `code: vault_locked`，前端弹一次解锁框，解锁后原请求自动重试；`vault_lockout` / `vault_bad_password` 分别带剩余秒数与剩余次数
- 设了密码的服务器，改密码/私钥也要先解锁，改完仍用密码派生的密钥保存；`MarkServer` 不碰 vault 字段，有单测盯着

**插件**

- Node.js 插件默认改成 **24 LTS**（参数里可切回 20 / 22），卸载补上 `nodesource.sources` 与 `/usr/local` 下的二进制残留
- docker / openclaw 插件升到 1.0.2，带上上一版在真机上修好的安装路径

## 1.9.1

**按网络环境选源**

- 装任何东西之前先判断服务器在国内还是海外：并发探四组地址（阿里云 + 腾讯云镜像 vs. Google + raw.githubusercontent），结果缓存 12 小时到 `/var/lib/wpqu/net.env`
- 想固定下来：`echo 'REGION=cn' > /etc/wpqu/net.conf`（`cn` / `global` / `off`）
- 逻辑收在一份 `scripts/_mirrors.sh`，由 `sync-mirrors.sh` 注入（和 `_ws-lib.sh` 同一套做法），每个脚本、每个插件包各带一份，仍可单文件独立运行
- 云厂商内网镜像优先：腾讯云机器上 `mirrors.tencentyun.com` 4 毫秒响应，公网阿里云要 3 秒
- apt 换源只改主机段（套件、组件、`signed-by` 原样保留，`sources.list` 与 Debian 13 的 deb822 `*.sources` 都支持），换完先验证，**不通自动撤回**；原文件备份为 `*.wpqu.bak`
- `apt-get update` 统一带 `Acquire::Languages=none`，索引流量少一半
- 下载源跟着区域走：PyPI、npm、GOPROXY、Go 与 Node 二进制、wp-cli、WordPress 主程序、GitHub 发布包
- 插件安装不再逐个测速挑源，直接用区域默认源，安装立刻开始
- 体检新增「网络环境」；插件安装日志第一行打印判定结果

**真机上才暴露的几个坑**

- Typecho 的 `rand_pw` 在 `pipefail` 下被 SIGPIPE 打断：留空密码时整个安装脚本 141 退出，还没走到建库
- Go 插件的 SHA256 查询被 `head` 提前关管道打断，校验被静默跳过；`-A3` 的窗口也不够长，取不到 `sha256` 字段
- docker 插件原来跑 `get.docker.com`，这个地址在国内会被重置（同一台机器连续请求会在 `000` 和 `200` 之间跳），而它真正要拉的是 `docker-ce` 仓库。国内现在直接从镜像的 docker-ce 仓库装（连签名密钥一起下），一分钟装完
- openclaw 的官方安装器是 bash 脚本（用了 `[[ ]]`），用 Debian 的 `sh`（dash）跑会直接语法报错；改成 `bash ... --no-onboard`
- openclaw 要求 Node 24，而 nodejs 插件装在 `/usr/local` 的是 22，`/usr/local` 在 `PATH` 里排在 `/usr/bin` 前面，刚装好的 24 被盖住了；现在脚本会显式挑新的那个
- `$("cmd")` 这种把命令整体加引号的写法，会让命令名变成一个带空格的文件名（openclaw 的版本号因此是空的）

## 1.9.0

**插件市场**

- 新增「插件」页签：从 `panel.wpqu.com` 拉签名过的插件索引，卡片式浏览、搜索、按分类与免费/收费筛选
- 统一插件模型：`install`（在服务器上执行的部署配方）与 `ui`（装进面板的界面模块）共用一份 `plugin.json`
- 官方插件六个：OpenClaw（小龙虾）、Go 工具链、Python 3 + uv、Node.js 22 LTS、Docker Engine、服务器便签
- 安装走 ndjson 流：分步标题 + 实时脚本输出；装完自动刷新状态
- 服务器端状态检测合并为一次 SSH 执行，用 `###<id>` 分隔结果
- 界面插件通过 `/api/plugin-assets/` 动态加载，导出 `mount(el, ctx)` 即可挂进页签

**收费插件**

- 离线授权码：`WPQU1.<payload>.<signature>`，面板内置授权公钥本地验签，不联网、无账号体系
- 授权码绑定产品、到期日与可装机器数；跨产品使用会被拒绝
- 签发工具 `wpqusign license`

**面板自升级**

- 侧栏底部显示版本，点开「关于与更新」可检查更新、看更新说明、一键升级
- 升级包校验 Ed25519 签名 + SHA256，任一不过即拒绝落地
- Windows：重命名运行中的 exe 后替换，失败回滚；Linux / macOS：同目录 rename 后 `syscall.Exec` 原地重启
- 面板不会自动联网，只有点「检查更新」才会连接更新源

**分发与工具**

- 新增 `panel.wpqu.com` 静态站：官网首页、插件市场页、`updates.json`、`plugins/index.json`，中英双语
- `cmd/wpqusign`：生成密钥对、签名文件、签发授权码、校验签名
- `cmd/wpqupack`：打包插件、生成并签名索引与升级清单
- `tools/release.sh`：一条命令完成编译 → 清单 → 打包 → 签名
- 构建产物统一更名为 `wpqu-panel-<os>-<arch>`
- 数据目录改为 `wpqu-panel`，旧 `wp-panel` 目录首次启动时整份复制迁移（含插件与授权码）

**其它**

- 新增 `internal/version`（含单测）与 `internal/signing`（Ed25519 验签）
- 修复 `version.Valid` 在版本号只有两段时越界 panic

## 1.8.0

首次公开发布。以下条目都早于开源，保留下来是因为它们解释了代码为什么长这样。

**面板**

- 全界面、文档、脚本头注释更名为 **WPQU panel**（侧栏标识仍是 `WQ`）
- **中英双语界面**：侧栏可切换，`?lang=` 可强制指定，缺译自动回退中文
- i18n 放在 `public/i18n/`（`core.js` 翻译器 + `zh-CN.js` + `en.js` + `dicts.js` 登记处），翻译发生在
  渲染后的 DOM 上，`app.js` 里没有 `t()` 调用；加一门语言只改三个地方
- 数据目录改为 `wpqu-panel`（Windows 在 `%APPDATA%`，其它在 `~/.config`）；已有 `wp-panel` 目录会在
  首次启动时迁移，源目录保留
- 二进制更名为 `wpqu-panel` / `wpqu-panel-console`；Go 模块改为 `wpqu-panel`
- 界面里的示例 IP 换成 RFC 5737 文档地址段；云厂商相关文案改为通用说法

## 1.7.0

**站点管理双栈——nginx 与 Apache 都能管**

- 四套站点脚本（`add-site`、`change-domain`、`delete-site`、`site-convert`）加 `php-manage.sh` 通过新的
  适配库 `scripts/_ws-lib.sh` 同时支持两种 Web 服务器；库由 `scripts/sync-ws-lib.sh` 注入进各脚本
  （远端脚本无法 `source` 同目录文件）
- Apache 路径整份写 `VirtualHost`：PHP 用 `SetHandler "proxy:unix:…|fcgi://localhost"`，静态站
  `Require all denied`，配 `a2ensite` / `a2dissite`、`apache2ctl configtest`、`certbot --apache`；
  有证书时同时写 `:80`（含 `/.well-known` 例外）与 `:443`，换类型不丢 HTTPS
- WordPress 伪静态显式写 `.htaccess`（CLI 下 `wp rewrite flush --hard` 不落盘）；切走 WordPress 时把
  文件改名成 `.htaccess.wp-bak-<时间戳>` 存起来
- 站点类型标记迁到 `/etc/wp-panel/site-types/`，切换 Web 服务器时标记跟着走；旧的
  `/etc/nginx/site-types/` 仍会读取并迁移
- 概览、站点列表、站点详情、日志路径、PHP 池切换、界面文案全部跟随当前 Web 服务器；纯跳转 vhost 不再
  当成站点列出
- 修 `ws_socket_of` 在没有 PHP 的 vhost 上误报（`set -e` + `pipefail` 下 grep 返回 1）

## 1.6.0

**初始化支持自定义选装**

- 可自选 Web 服务器（nginx / Apache）、PHP 版本（8.0–8.4，缺的自动加第三方源）、数据库
  （MariaDB 11 / MySQL 8.4 / MySQL 5.7）
- 安装脚本按版本装 PHP 扩展、PHP 源优先国内镜像、Apache 写 vhost + `.htaccess`、MySQL 5.7 兼容
  Debian 12/13
- 修三个只在真机上暴露的问题：php-fpm pool 里的 `#` 注释导致起不来、数据库调优写在 `mysql.conf.d`
  被忽略、装过 Sury 源的机器上 `php-opcache` 元包解析失败

## 1.5.0

**维护栏目**

- PHP 版本管理（安装 / 卸载 / 切站点 / 设默认 CLI）、网站日志（查看 / 轮转 / 清空 / 删归档）、
  root crontab 编辑、任意命令执行
- 修 `set -u` 下未初始化变量、`pipefail` 下 `grep -q` 误判
- 启动器改为比对版本号，自动替换旧进程

## 1.4.0

**可选 FTP 栏目**

- vsftpd 安装 / 卸载、账号增删、改目录与权限；账号复用站点目录属主 UID，上传的文件 PHP 立刻可写

## 1.3.x

- 文件权限查看与修改（八进制、可递归）、多选批量操作
- 整站删除，需手工输入域名确认
- 夜间模式
- 面板定名 WPQU panel

## 1.2.0

- 数据库模块适配 MySQL 8.4（`sql_mode` 兼容）

## 1.1.0

- 启动器重写：固定数据目录 + 自动迁移、无控制台 GUI、自动打开浏览器、单实例复用、`-stop`

## 1.0.0

- Go 版首发，接口与早期 Node 实现对齐
