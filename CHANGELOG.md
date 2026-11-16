# Changelog

**English** ｜ [简体中文](CHANGELOG.zh-CN.md)

Notable changes, newest first. Versions are the value returned by `/api/ping`.

## 1.9.0

**Plugin marketplace**

- New Plugins tab: a signed index served from `panel.wpqu.com`, browsable as cards with search and
  category / free-paid filters
- One plugin model for both kinds: `install` (a deploy recipe that runs on the server) and `ui`
  (a panel module), sharing a single `plugin.json`
- Six official plugins: OpenClaw, Go toolchain, Python 3 + uv, Node.js 22 LTS, Docker Engine, Notes
- Installs stream over ndjson with per-step titles and live script output; status refreshes on finish
- Server-side status detection is batched into one SSH call delimited by `###<id>`
- UI plugins load through `/api/plugin-assets/` and just export `mount(el, ctx)`

**Paid plugins**

- Offline license keys: `WPQU1.<payload>.<signature>`, verified locally against a public key baked
  into the binary. No account, no network call
- Keys bind to a product, an expiry date and a seat count; cross-product use is rejected
- Issued with `wpqusign license`

**Self-update**

- Version shown at the bottom of the sidebar; "About and updates" checks, shows release notes and
  updates in place
- Packages are verified by Ed25519 signature and SHA256; either failing means nothing is written
- Windows renames the running exe and rolls back on failure; Linux / macOS rename in place and
  `syscall.Exec` to restart
- The panel never phones home by itself — only "Check for updates" contacts the source

**Distribution and tooling**

- New static site at `panel.wpqu.com`: landing page, plugin market page, `updates.json`,
  `plugins/index.json`, bilingual
- `cmd/wpqusign`: keygen, sign files, issue license keys, verify signatures
- `cmd/wpqupack`: package plugins, generate and sign the index and update manifest
- `tools/release.sh`: one command for build → manifest → package → sign
- Build artifacts renamed to `wpqu-panel-<os>-<arch>`
- Data directory renamed to `wpqu-panel`; an existing `wp-panel` directory is copied over on first
  launch (plugins and license keys included)

**Other**

- New `internal/version` (with tests) and `internal/signing` (Ed25519 verification)
- Fixed an out-of-range panic in `version.Valid` for two-segment versions

## 1.8.0

First public release. Everything below this line predates the open-sourcing; the entries are kept
because they explain why the code looks the way it does.

**Panel**

- Renamed to **WPQU panel** throughout the UI, docs and script headers (the sidebar mark is still `WQ`)
- **Bilingual interface**: English and Simplified Chinese, switchable from the sidebar; `?lang=` to
  force one; missing translations fall back to Chinese
- i18n lives in `public/i18n/` (`core.js` translator + `zh-CN.js` + `en.js` + `dicts.js` registry) and
  translates the rendered DOM, so `app.js` needs no `t()` calls; adding a language is three edits
- Data directory renamed to `wpqu-panel` (Windows `%APPDATA%`, elsewhere `~/.config`); an existing
  `wp-panel` directory is migrated on first start, source kept
- Binaries renamed to `wpqu-panel` / `wpqu-panel-console`; Go module is now `wpqu-panel`
- Example IPs in the UI changed to the RFC 5737 documentation range; cloud-provider-specific wording
  made generic

## 1.7.0

**Dual-stack site management — nginx *and* Apache**

- The four site scripts (`add-site`, `change-domain`, `delete-site`, `site-convert`) plus
  `php-manage.sh` now work against either web server through a new adapter library
  `scripts/_ws-lib.sh`, injected into each script by `scripts/sync-ws-lib.sh` (remote scripts cannot
  `source` a sibling file)
- Apache path writes a full `VirtualHost`: `SetHandler "proxy:unix:…|fcgi://localhost"` for PHP,
  `Require all denied` for static sites, `a2ensite` / `a2dissite`, `apache2ctl configtest`,
  `certbot --apache`; when a certificate exists both `:80` (with `/.well-known` exception) and `:443`
  blocks are written, so converting a site keeps HTTPS
- WordPress rewrite rules are written to `.htaccess` explicitly (`wp rewrite flush --hard` is a no-op
  in CLI); switching away from WordPress parks the file as `.htaccess.wp-bak-<timestamp>`
- Site type markers moved to `/etc/wp-panel/site-types/` so they survive a switch of web server; the
  old `/etc/nginx/site-types/` is still read and migrated
- Overview, site list, site detail, log paths, PHP pool switching and UI wording all follow the
  running web server; pure-redirect vhosts no longer appear as sites
- Fixed `ws_socket_of` raising a spurious error on vhosts without PHP (grep exit 1 under `set -e` +
  `pipefail`)

## 1.6.0

**Custom stack selection during setup**

- Choose web server (nginx / Apache), PHP version (8.0–8.4, third-party repository added when needed)
  and database (MariaDB 11 / MySQL 8.4 / MySQL 5.7) instead of the two fixed combinations
- Installer installs PHP extensions per version, prefers a domestic mirror for PHP packages, writes
  an Apache vhost with `.htaccess`, and handles MySQL 5.7 on Debian 12/13
- Three real-machine bugs fixed: `#` comments in the php-fpm pool prevented startup, database tuning
  written to `mysql.conf.d` was ignored, and the `php-opcache` metapackage failed to resolve on
  machines with the Sury repository enabled

## 1.5.0

**Maintenance section**

- PHP version management (install / remove / switch a site / default CLI), site log viewer with
  rotate-clear-delete, root crontab editor, arbitrary command runner
- Fixed uninitialised variables under `set -u` and `grep -q` being misjudged under `pipefail`
- Launcher now compares versions and replaces a stale instance automatically

## 1.4.0

**Optional FTP section**

- Install/uninstall vsftpd, add/remove accounts, change directory and mode; accounts reuse the site
  directory's owner UID so uploads are immediately writable by PHP

## 1.3.x

- File permissions view/edit (octal, recursive), multi-select batch operations
- Delete whole site with typed confirmation
- Dark mode
- Panel named WPQU panel

## 1.2.0

- Database module adapted to MySQL 8.4 (`sql_mode` compatibility)

## 1.1.0

- Launcher rewritten: fixed data directory with automatic migration, GUI build without a console,
  automatic browser launch, single-instance reuse, `-stop`

## 1.0.0

- First Go release, API-compatible with the earlier Node implementation
