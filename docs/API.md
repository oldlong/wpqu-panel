# HTTP API

**English** ｜ [简体中文](API.zh-CN.md)

Everything the browser does, you can do with `curl`. This document lists the surface; the request
fields match the JSON the UI sends.

## Conventions

- **Base URL** `http://127.0.0.1:7788` (or whatever port the panel settled on).
- **Write requests need a header.** Anything other than `GET` must carry `x-panel: 1`, otherwise the
  panel answers `403`. This is a CSRF guard: a cross-origin `POST` from a random page cannot set a
  custom header without a preflight.
- **Errors** are `{"error": "…"}` with status `400` for bad input and `500` for everything else.
  Messages are currently Chinese.
- **Success** is `{"ok": true, …}` plus endpoint-specific fields.
- **IDs** look like `srv_1a2b3c4d5e6f7a8b`; they stay stable across restarts.
- **Large uploads** (file upload, SQL import) go as `multipart/form-data`; everything else is JSON.

Example:

```bash
curl -s http://127.0.0.1:7788/api/ping
# {"impl":"go","ok":true,"time":"2026-11-16T22:41:07+08:00","version":"1.9.2"}

curl -s http://127.0.0.1:7788/api/servers -H 'x-panel: 1'
```

## Streaming endpoints

Operations that run a remote script answer with `Content-Type: application/x-ndjson` and one JSON
object per line:

```json
{"type":"start","message":"新增站点 example.com（wordpress）"}
{"type":"out","text":"[+] 目录已创建：/var/www/example.com"}
{"type":"out","text":"[+] 证书签发成功"}
{"type":"end","code":0,"ok":true,"durationMs":18432,"message":"执行完成"}
```

| Type | Meaning |
|---|---|
| `start` | operation accepted, `message` describes it |
| `step` | step marker for multi-step operations: `index`, `total`, `title` (used by plugin install/uninstall) |
| `out` | one chunk of script output (ANSI stripped) |
| `progress` | progress: `pct` (0–1) and `message` (currently only the self-updater) |
| `error` | terminal failure, `message` is the reason; no `end` follows |
| `end` | terminal, `code` is the script's exit code, `ok` is `code == 0` |

Validation happens before the stream opens, so a malformed request returns a plain JSON `400` instead
of a half-open stream. Timeouts still end with `end`, but with `code: -1` and `（超时）` appended to the message.

## Meta

| Method | Path | Body / query | Returns |
|---|---|---|---|
| GET | `/api/ping` | — | `{ok, impl, version, time}` — used by the panel to detect other instances |
| POST | `/api/shutdown` | — | `{ok}`; asks the process to exit |
| GET | `/api/logs` | `limit` (1–300, default 100) | `{logs:[…]}` activity log, newest first |

## Servers

| Method | Path | Body / query | Returns |
|---|---|---|---|
| GET | `/api/servers` | — | `{servers:[…]}` (no credentials) |
| POST | `/api/servers` | `name, host, port, username, authType ("password"\|"key"), password, privateKey, passphrase, note` | created server |
| PATCH | `/api/servers/:id` | any of `name, port, username, authType, password, privateKey, passphrase, note` | updated server |
| DELETE | `/api/servers/:id` | — | `{ok}` (profiles only; the server is untouched) |
| POST | `/api/servers/:id/test` | — | `{ok, output}` SSH connectivity probe |
| GET | `/api/servers/:id/doctor` | — | `{ok, doctor:{checks:[…]}}` health check, read-only |
| GET | `/api/servers/:id/overview` | — | resources, versions, site list summary |
| GET | `/api/servers/:id/site-detail` | `domain` | one site: root, type, PHP socket, SSL, size, file count |

### Server vault (optional per-server local encryption)

When a server's vault is enabled, its stored credentials are re-encrypted with a key derived from a
passphrase (scrypt → AES-256-GCM). Operations that need those credentials return **HTTP 423** while
the vault is locked, with `error` + `code`: `vault_locked` (needs unlocking), `vault_lockout`
(too many wrong attempts; body carries `lockSeconds`), or `vault_bad_password` (body carries
`remaining`; 5 wrong attempts lock it for one hour). Successful unlock starts a 30-minute session.

| Method | Path | Body | Returns |
|---|---|---|---|
| GET | `/api/servers/:id/vault` | — | `{enabled, setAt}` (or `enabled:false`) |
| POST | `/api/servers/:id/vault` | `password`; plus `oldPassword` when changing | `{ok}` |
| POST | `/api/servers/:id/vault/unlock` | `password` | `{ok}` (30-minute session) |
| POST | `/api/servers/:id/vault/lock` | — | `{ok}` (clears the in-memory key) |
| DELETE | `/api/servers/:id/vault` | `password` | `{ok}` (credentials go back to machine-key encryption) |

Note: `PATCH /api/servers/:id` on an encrypted server also requires an unlocked vault (423
`vault_need_password` otherwise). `POST /api/servers/:id/test` does not touch vault state.

## Sites

All of these are streaming.

| Path | Body | What it does |
|---|---|---|
| `POST /api/servers/:id/bootstrap` | `webServer ("nginx"\|"apache"), phpVersion ("8.0"…"8.4", empty = distro), dbEngine ("mariadb"\|"mysql84"\|"mysql57"), domain, title, adminUser, adminPass, ssl, redis` | install the stack and create the first site |
| `POST /api/servers/:id/sites` | `domain, type ("wordpress"\|"custom"\|"static"), title, adminUser, adminPass, locale ("zh_CN"\|"en_US"), separatePool, maxChildren (1–12), ssl` | create a site |
| `POST /api/servers/:id/site-type` | `domain, type, title, adminUser, adminPass, locale, separatePool, maxChildren, force, dryRun` | convert between the three types |
| `POST /api/servers/:id/domain` | `oldDomain, newDomain, ssl, keepRedirect, moveDir, dryRun` | rename the domain; `keepRedirect` writes the 301 vhost |
| `POST /api/servers/:id/site-delete` | `domain, dbName, deleteFiles, deleteDb, deleteCert` | remove a site and optionally its files/database/certificate |
| `POST /api/servers/:id/ssl/issue` | `domain, email` | issue or expand a certificate |
| `POST /api/servers/:id/ssl/renew` | — | renew all certificates |

## Files

Paths are absolute on the server. Reads are unrestricted (whatever the SSH user can read); writes are
sent over SFTP.

| Method | Path | Body / query | Notes |
|---|---|---|---|
| GET | `/api/files` | `server, path` | directory listing with mode, size, mtime |
| GET | `/api/file` | `server, path` | file content (text, size-capped) |
| PUT | `/api/file` | `server, path, content` | save the editor buffer |
| POST | `/api/file/upload` | multipart, query `server, dir, name` | upload one file |
| GET | `/api/file/download` | `server, path` | download, streamed |
| POST | `/api/file/mkdir` | `server, path` | create a directory |
| POST | `/api/file/rename` | `server, from, to` | rename or move |
| POST | `/api/file/delete` | `server, path` | delete file or directory |
| POST | `/api/file/chmod` | `server, path, mode ("755"), recursive` | mode bits only |
| POST | `/api/file/batch` | `server, action ("chmod"\|"delete"\|"copy"\|"move"), paths[], mode, recursive, dest` | batch operations |

## Database

| Method | Path | Body / query | Notes |
|---|---|---|---|
| GET | `/api/servers/:id/db/databases` | — | databases with size and table count |
| GET | `/api/servers/:id/db/tables` | `db` | tables with rows, size, engine |
| GET | `/api/servers/:id/db/rows` | `db, table, limit, offset` | rows plus total count |
| GET | `/api/servers/:id/db/structure` | `db, table, kind` | `kind` = `columns` (default) or `indexes` |
| POST | `/api/servers/:id/db/query` | `db, sql` | run one statement; write statements are inspected first |
| GET | `/api/servers/:id/db/export` | `db` (empty = all) | gzipped dump, streamed |
| POST | `/api/servers/:id/db/import` | multipart, query `db, name, backup` | restore a `.sql` / `.sql.gz` |
| POST | `/api/servers/:id/db/optimize` | `db, table, mode` | `OPTIMIZE TABLE` |

Refused statements: `CREATE/DROP/ALTER USER|ROLE`, `GRANT`, `REVOKE`, `SET PASSWORD`, `LOAD DATA`,
`INTO OUTFILE|DUMPFILE`, `LOAD_FILE()`, `INSTALL/UNINSTALL PLUGIN|SONAME`.

## FTP

| Method | Path | Body |
|---|---|---|
| GET | `/api/servers/:id/ftp` | — (status, users, config) |
| POST | `/api/servers/:id/ftp/control` | `action ("start"\|"stop"\|"restart")` |
| POST | `/api/servers/:id/ftp/config` | passive port range, passive address, TLS options |
| POST | `/api/servers/:id/ftp/users` | `name, password, dir, writable` |
| POST | `/api/servers/:id/ftp/users/delete` | `name` |
| POST | `/api/servers/:id/ftp/users/dir` | `name, dir` |
| POST | `/api/servers/:id/ftp/users/password` | `name, password` |
| POST | `/api/servers/:id/ftp/users/mode` | `name, writable` |

`POST /api/servers/:id/ftp/install` and `/ftp/uninstall` are streaming (they call apt).

## Maintenance

| Method | Path | Body | Notes |
|---|---|---|---|
| GET | `/api/servers/:id/php` | — | installed versions, per-site binding, default CLI |
| POST | `/api/servers/:id/php/install` | `version` | streaming; adds Sury/ondrej if needed |
| POST | `/api/servers/:id/php/remove` | `version, force` | streaming |
| POST | `/api/servers/:id/php/repo` | — | streaming; add the third-party repository only |
| POST | `/api/servers/:id/php/site` | `domain, version, force` | move one site to another version |
| POST | `/api/servers/:id/php/default` | `version` | set the default CLI version |
| GET | `/api/servers/:id/logs/list` | — | whitelisted log files |
| POST | `/api/servers/:id/logs/tail` | `path, lines` | tail with optional decompression |
| POST | `/api/servers/:id/logs/clear` | `path` | truncate in place (streaming) |
| POST | `/api/servers/:id/logs/rotate` | `path` | compress + truncate (streaming) |
| POST | `/api/servers/:id/logs/delete` | `path` | archives only (streaming) |
| GET | `/api/servers/:id/cron` | — | root crontab, flagged `managed` |
| POST | `/api/servers/:id/cron/add` | `schedule, command, label` | streaming |
| POST | `/api/servers/:id/cron/delete` | `id, line` | streaming |
| POST | `/api/servers/:id/cron/toggle` | `line, enabled` | streaming |
| POST | `/api/servers/:id/cron/run` | `command` | run once now, streamed |
| POST | `/api/servers/:id/exec` | `command, cwd, seconds` | root shell, streamed; flagged commands need confirmation in the UI |

## Backups

| Method | Path | Body | Notes |
|---|---|---|---|
| POST | `/api/servers/:id/backup` | `domain, includeDb, includeFiles` | streaming; archive lands in `data/backups/<serverId>/` |
| GET | `/api/servers/:id/backups` | — | `{backups:[…]}` with name, size, mtime |

## Plugins

Plugins come in two shapes: one with `install` puts software on a server, one with `ui` adds a tab to
the panel (some have both). The catalogue is served from `panel.wpqu.com` and verified locally; when
verification fails `/api/plugins` returns `catalog: []` plus a non-empty `error`.

| Method | Path | Body / query | Notes |
|---|---|---|---|
| GET | `/api/plugins` | — | `{catalog, installed, licenses, uiTabs, source, updated, panel, platform}`; non-empty `error` means the catalogue could not be fetched or verified |
| GET | `/api/servers/:id/plugins/status` | — | `{status:{"<pluginId>":{installed, version}}}` — one SSH call for all plugins |
| POST | `/api/plugins/:id/license` | `key` | verifies locally, then stores; returns `{ok, product, name, expire, seats}` |
| DELETE | `/api/plugins/:id/license` | — | `{ok}`; forget the stored key |
| DELETE | `/api/plugins/:id` | — | `{ok}`; remove a panel-side plugin and its unpacked files (server untouched) |
| POST | `/api/plugins/:id/install` | — | install a `ui` plugin into the panel |
| POST | `/api/servers/:id/plugins/:pid/install` | `params` (object, keys as declared in the plugin's `install.params`) | streaming; runs the install scripts on the server |
| POST | `/api/servers/:id/plugins/:pid/uninstall` | — | streaming; runs the uninstall script |

Pre-flight checks run in order and each one answers `400` on failure: the panel version satisfies
`requires.minPanel`, paid plugins have a valid license, and the server's `uname -s` is listed in
`requires.os`. Keys in `params` are upper-cased and prefixed with `WPQU_` before being passed to the
script as environment variables (`{"major":"22"}` → `WPQU_MAJOR=22`); a key containing anything outside
`A-Z0-9_` (say `major.minor`) is silently dropped, so don't use dots.

```bash
# browse the catalogue
curl -s http://127.0.0.1:7788/api/plugins -H 'x-panel: 1' | jq '.catalog[].id'

# store a license key
curl -s -X POST http://127.0.0.1:7788/api/plugins/openclaw/license \
  -H 'x-panel: 1' -H 'content-type: application/json' \
  -d '{"key":"WPQU1.eyJwIjoib3BlbmNsYXci..."}' | jq

# install on a server (streaming)
curl -sN -X POST http://127.0.0.1:7788/api/servers/srv_xxx/plugins/golang/install \
  -H 'x-panel: 1' -H 'content-type: application/json' -d '{"params":{}}'
```

A license key is `WPQU1.<payload>.<sig>`; the payload carries the product id, expiry, seat count and
customer name. The panel only does a local Ed25519 verification — no network, no open port.

`/api/plugin-assets/<pluginId>/<entry>` serves the front-end files of a `ui` plugin, read-only and
confined to the unpacked plugin directory.

## Updates

Panel only.

| Method | Path | Body | Notes |
|---|---|---|---|
| GET | `/api/update/check` | — | `{version, canSelfUpdate, reason, source, result}` |
| POST | `/api/update/apply` | — | streaming; re-checks, downloads, swaps and restarts |

`result` is the manifest comparison: `{available, latest, notes, critical, size, sha256, skipped, tooOld}`.
When you are already current, `available:false` and `skipped` says why (for example "no package for this
platform"). A failed check **never returns 5xx**: the reason goes into `error` with HTTP `200`, because
being offline is normal and the UI has to show why.

```bash
curl -s http://127.0.0.1:7788/api/update/check -H 'x-panel: 1' | jq '.result, .canSelfUpdate'
```

`POST /api/update/apply` **re-checks** server-side and never trusts a URL or SHA256 sent by the client.
The stream is `start` → `progress` (bytes) → verification → swap → `end`; roughly 700ms after `end` the
process exits and restarts itself, so clients must tolerate a dropped connection. When
`canSelfUpdate:false` (started via `go run`, or the directory is not writable) this endpoint answers `400`.

## Static assets

| Path | Contents |
|---|---|
| `/` | `public/index.html` |
| `/app.js`, `/style.css` | UI code |
| `/i18n/*.js` | language packs |
| `/api/plugin-assets/<id>/<file>` | front-end files of a `ui` plugin |

## Notes for scripted use

- The panel is single-user and unauthenticated: anything on `127.0.0.1` can call it. Do not expose it.
- Concurrent operations on the same server are not queued; two `POST /sites` at once will race on
  the package manager and the config files. Script it sequentially.
- Long operations (bootstrap, PHP install) can take minutes; read the stream instead of using a short
  client timeout.
