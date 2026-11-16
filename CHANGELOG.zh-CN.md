# 变更记录

[English](CHANGELOG.md) ｜ **简体中文**

重要变更，新的在前。版本号即 `/api/ping` 返回的 `version`。

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
