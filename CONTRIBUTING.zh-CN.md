# 参与贡献

[English](CONTRIBUTING.md) ｜ **简体中文**

欢迎来看看。这是个小项目，架构刻意保持无聊；下面这些规矩就是为了让它继续无聊下去。

## 适合上手的贡献

| 类型 | 说明 |
|---|---|
| **加一门界面语言** | 最有价值的贡献。复制 `public/i18n/en.js`、翻译、登记——只改三个地方，见 [docs/ARCHITECTURE.zh-CN.md § 9](docs/ARCHITECTURE.zh-CN.md#9-界面语言) |
| **补漏译** | 拿 `public/i18n/en.js` 和 `public/app.js` 里的字符串对一遍；缺的键会自动回退中文 |
| **文档** | 尤其是英文指南：截图、措辞、纠错 |
| **带复现步骤的 bug 报告** | 价值等于半个修复 |
| **支持其它发行版** | `scripts/install-wordpress.sh` 目前写死了 Debian/Ubuntu 的写法 |

大功能请先开 issue 说清楚要改什么。面板刻意不做插件系统、不做自己的数据库、不做用户体系——动手前
先确认想法合不合这个前提。

## 开发环境

```bash
git clone https://github.com/oldlong/wpqu-panel.git
cd wpqu-panel
go build -o wpqu-panel .        # 或 ./build.sh
PANEL_DATA=./devdata go run . -no-browser -port 7789
```

- Go **1.24+**，不用 CGO。依赖只有 `golang.org/x/crypto`（SSH）和 `github.com/pkg/sftp`，再加需要理由
- `devdata/` 把开发档案和真实的 `data/` 隔开，已在 .gitignore 里
- 碰服务器的功能需要**真机**验证。项目没有 mock 层，这是故意的

## 提交前自检

```bash
gofmt -l .                       # 必须没有输出
go vet ./...                     # 必须安静
go test ./...                    # version / plugins / updater 三个包
bash -n scripts/*.sh             # 远端脚本语法
node --check public/app.js       # 界面就一个文件，这一步能抓到拼写错误
./scripts/sync-ws-lib.sh --check # 如果你动过 _ws-lib.sh
```

目前没有 CI，这份清单就是关卡。另外做检查做不到的事：把面板跑起来，把改过的功能点一遍。

## 远端脚本（`scripts/`）

这些脚本会被单独上传并以 root 执行，所以规矩比较多：

- **自包含**：不 `source`、不依赖同目录文件、不假设当前目录
- **LF 换行**：`.gitattributes` 已经强制了
- **入参走环境变量**，不用位置参数，顶部给默认值：`DOMAIN="${DOMAIN:-}"`
- **不交互**：不写 `read`，apt 一律 `-y` 加 `DEBIAN_FRONTEND=noninteractive`
- **幂等**：重跑不能弄坏已经配好的机器
- **先备份、再动手、后校验**：旧配置复制到 `/root/`，跑 `nginx -t` / `apache2ctl configtest`，
  不过就回滚
- **给解析器的输出**用 `KEY=value` 行与 `###SECTION` 标记
- **面向人的输出用中文**，`[+]` 表示进展，`[x]` 表示失败，失败要非零退出
- **Web 服务器相关的逻辑必须走 `scripts/_ws-lib.sh`**，不要在脚本里内联 nginx / Apache 分支

改过 `_ws-lib.sh` 之后：

```bash
./scripts/sync-ws-lib.sh        # 重写依赖它的五个脚本
./scripts/sync-ws-lib.sh --check
```

生成出来的脚本要和库的改动一起提交。不要手改注入块。

## Go 代码

- 标准库优先。`internal/` 下的包都刻意很小：`store`（档案）、`sshc`（传输）、`sites`（只读清单）、
  `ops`（动作）、`dbx`（数据库）、`httpapi`（HTTP）
- 校验放在 `httpapi`、开流之前；复用 `AssertDomain`、`assertPHPVersion`、`assertLogPath`，别自己造正则
- 错误一律返回，不要打了日志又吞掉。HTTP 层把 `dbx.BadRequest` 变成 `400`，其余 `500`
- 面向用户的错误文案目前是中文，改动时跟同一处保持一致，别在一个弹窗里混语言
- 注释中文英文都行，能讲清「为什么」的那种更有价值，别复述代码

## 前端

- `public/app.js` 是一个模板字符串文件，没框架、没构建步骤。保持这样
- 按段落注释（`/* ---- 概览 ---- */`）找位置，把东西放到该放的地方
- 新增可见文案：模板里写中文，然后往 `public/i18n/en.js` 加一条键。句子带数字的话，去
  `public/i18n/core.js` 加一条正则模式
- 不要指望翻译 `<pre>`、`<code>`、`<textarea>` 里的内容——那些是刻意跳过的
- 改完界面刷新看两种语言；通常英文版会先撑破布局（字符串更长）

## Pull request

- 一个 PR 只做一件逻辑上的事。纯格式化改动混在行为改动里会让 review 很痛
- 说清楚在什么发行版/版本上验证过。碰 `scripts/` 的改动，「我这儿能跑」不够
- 把反面路径也写清楚：配置校验失败、端口被封、域名不合法时会怎样
- 用户可见的改动要更新 `CHANGELOG.md`（能两版都写最好，只写英文也行，维护者会补中文）
- 行为变了就同步文档：`docs/GUIDE*.md`、`docs/API*.md`、`README*.md`

## 提交信息

祈使句短标题，理由不明显时再加正文：

```
fix(add-site): 共享 socket 时保留 PHP pool

统计引用时只扫了 sites-available，与片段共享的 pool 看起来没人用，
下次删站就被连带删掉了。
```

## 贡献的许可

项目是 GPL-3.0。提交 PR 即表示你的贡献以同样条款授权（inbound = outbound）。没有 CLA，也不用签版权转让。

## 行为准则

直来直去、假定善意、批评只针对代码。安全问题走 [SECURITY.zh-CN.md](SECURITY.zh-CN.md)，不要开在 issue 里。
