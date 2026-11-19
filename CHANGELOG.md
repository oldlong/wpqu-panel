# Changelog

**English** ｜ [简体中文](CHANGELOG.zh-CN.md)

Notable changes, newest first. Versions are the value returned by `/api/ping`.

## 1.10.0

**Emergency fixes: patches that do not need a panel release**

- New "Emergency fixes" tab under Maintenance. Opening it pulls a **signed** feed
  (`panel.wpqu.com/api/security.json`) carrying advisories plus detection rules and fix steps. Whoever
  runs the distribution site edits one JSON and re-signs; every installed panel sees it on next open,
  with no panel release
- Up to three advisories at the top (level / date / source link) — also usable as a notice bar
- Two detection kinds: `apt` (are upgrades available for these packages — upgrades only, never installs)
  and `version` (below the feed's threshold). A hit shows "current → fixed"
- "Show commands" reveals the script first, then "Fix now"; output streams and a re-check follows.
  Every run lands in Activity as `security.fix`
- Safety: commands are signed with the feed (same release key as update packages) and rejected on a bad
  signature; the API accepts only a fix id, so the browser cannot inject a command; entries whose
  `minPanel` is newer than the running panel are skipped
- New `/security.html` on the distribution site, linked from the site navigation

## 1.9.3

**Three high-severity problems in one-step install and plugins**

- The database name and user for one-step install are now derived from the domain
  (`wp_<fragment>` / `u_<fragment>`). They used to be hardcoded to `wordpress` / `wpuser`, so a second
  site on the same host reused the old database and reset the account's password, leaving the first
  site unable to connect
- Two guards before creating a database: stop if it already holds tables, stop if the user already
  exists (no more `ALTER USER` on someone else's account)
- The PHP page's site list is fixed: `ws_init` was called before the function was defined, so the list
  was always empty and removing a PHP version in use was not blocked (a purge would 502 every site on it)
- The Typecho plugin (1.0.1) no longer removes every config when uninstalling by globbing
  `typecho-*.conf`: the installer records SLUG/WEBROOT in `/etc/wpqu/typecho/<slug>.env` and uninstall
  removes only that one; older installs without the state file are resolved from the web root, and if
  that fails the configs are listed for you to handle by hand

**Other fixes**

- Overview now fills in the database version and flavour (`mariadb` / `dbFlavor`), which were always empty
- The local password is now **at least 6 characters** (length only, no character rules) — a single digit
  used to be accepted, which barely guards the root credentials
- A forgotten local password can be **reset**: the password and the stored SSH credentials are cleared
  together, so you just fill them in once instead of waiting out the lock or re-adding the server
- Deleting a site with "drop database" but without a database name now reads the name from
  `wp-config.php`, and drops the site's dedicated database user afterwards (kept when it has access to
  other databases, so the shared legacy `wpuser` is safe)
- Certificate failures now diagnose from **this run's** certbot output (`/var/log/wp-ssl-*.log`) instead
  of whatever an earlier operation left in `wp-maint.log`
- Database exports and backups use local time in filenames (UTC put them 8 hours behind); backup steps no
  longer hardcode `[1/4]…[4/4]`, so database-only backups do not skip numbers
- `cron/run` works with just a line number (the command is read from crontab); cron entries now always
  carry a string `id`, with non-panel entries named `line-<n>`
- English errors from the file API (`file does not exist`, `permission denied`, …) are translated
- Log tail occasionally returned nothing on the first try: it now retries once and falls back to the raw
  content when the separator is missing

## Admin console 1.0 (separate tool, versioned independently)

New `wpquadmin` — a localhost-only web console for whoever runs the distribution site:

- **Site content**: edit any text file under `site/` and `plugins-src/` in the browser; every save
  snapshots the previous version so you can roll back
- **Plugin marketplace**: edit `plugin.json` (price, category, copy), bump versions, pack and
  publish; the list shows at a glance whether the published catalog matches the local sources
- **License keys**: issue per plugin (single or bulk), export CSV, verify, void in the ledger. The
  UI states plainly that the ledger is not revocation
- **Release publishing**: push new binaries from `dist/` into `site/dl/`, rewrite and sign
  `updates.json`, keep the last 20 versions
- **Self-check**: `-check` verifies the site directory, that the private key's public half matches
  the one baked into the panel, and that an admin password is set — non-zero exit otherwise

**Double-click and go**: on Windows, just double-click the exe. On start it walks up from the exe
location to find `site/`, `plugins-src/` and `dist/`, keeps `admin.json` next to the exe (no
dependence on the working directory), generates a password if none exists and shows it in both the
console and a dialog, then opens the browser. If the port is taken it walks forward; if an instance
is already running it just points the browser at it instead of starting a second one that would
fight over the same ledger files.

Packing moved to `internal/pack` and key issuing to `internal/licensing`, shared by `wpqupack`,
`wpqusign` and the console — so both paths produce byte-identical artifacts and can be mixed
freely. Not part of the panel's update feed; `./build.sh admin` writes to `dist/admin/`.

## 1.9.2

**Per-server password (optional)**

- New "Set password" in the top-right: applies to the current server only; skip it and the panel behaves as before
- Once set, the SSH credentials are re-encrypted with AES-256-GCM under a key derived by `scrypt`
  (N=32768, r=8, p=1); the previous machine-key `secret` is deleted, so `data/secret.key` cannot open
  it and copying all of `data/` still reveals no plaintext
- **No strength rules** — a single digit is accepted. The password itself is never stored (only a salt
  and a verifier), so forgetting it means removing and re-adding the server
- **Five wrong attempts lock it for an hour**: the counter and `LockedUntil` live in `servers.json`,
  so restarting the panel does not clear them, and even the right password is refused while locked
- Unlocking lasts 30 minutes and is renewed on use (a process restart clears it); you can lock now,
  change or remove the password at any time
- While locked, every server endpoint returns `423` with `code: vault_locked`; the UI prompts once and
  replays the original request. `vault_lockout` / `vault_bad_password` carry the remaining seconds or attempts
- Changing credentials on an encrypted server also requires unlocking, and they stay encrypted under
  the derived key; `MarkServer` never drops the vault field (unit-tested)

**Plugins**

- The Node.js plugin now defaults to **24 LTS** (switchable to 20 / 22), and uninstall also removes
  `nodesource.sources` plus the `/usr/local` binaries
- docker / openclaw move to 1.0.2, carrying the install-path fixes verified on real machines

## 1.9.1

**Network-aware installs**

- The panel now detects whether a server sits in China or overseas before installing anything: four endpoints are probed in parallel (Aliyun + Tencent Cloud mirrors vs. Google + raw.githubusercontent), results are cached for 12 hours in `/var/lib/wpqu/net.env`
- Pin it manually with `echo 'REGION=cn' > /etc/wpqu/net.conf` (`cn` / `global` / `off`)
- One shared shell library (`scripts/_mirrors.sh`, injected by `sync-mirrors.sh` like `_ws-lib.sh`) so every script and plugin bundle carries the same logic and still runs standalone
- cloud intranet mirrors win when present (`mirrors.tencentyun.com` answers in 4 ms where the public Aliyun mirror needs 3 s)
- apt sources are rewritten host-only (suites, components and `signed-by` are preserved, both `sources.list` and Debian 13 deb822 `*.sources`), verified with `apt-get update` and **rolled back automatically** if the mirror fails; originals kept as `*.wpqu.bak`
- `apt-get update` runs with `Acquire::Languages=none`, halving index traffic
- Download sources follow the region: PyPI, npm, GOPROXY, Go and Node binaries, wp-cli, WordPress core, GitHub release assets
- Plugin installers no longer probe mirrors one by one — they try the regional source first, so installs start immediately
- Health check gains a "network" item; plugin install logs print the detected region on the first line

**Fixes found on a real machine**

- `rand_pw` (Typecho) was killed by SIGPIPE under `pipefail` when generating a random password — the whole installer exited 141 before touching the database
- Go plugin's SHA256 lookup was cut short by `head` closing the pipe, silently skipping verification; the `-A3` window also missed the `sha256` field
- The Docker plugin used to shell out to `get.docker.com`, which is reset in China (the same host flips between `000` and `200` between requests) even though the URL it really needs is the `docker-ce` repo. In China it now installs `docker-ce` straight from the mirror's docker-ce repo, signing key included — done in about a minute
- OpenClaw's official installer is a bash script (uses `[[ ]]`); running it through Debian's `sh` (dash) died with a syntax error. It now runs as `bash ... --no-onboard`
- OpenClaw requires Node 24 while the Node.js plugin installs 22 into `/usr/local`, which shadows `/usr/bin` in `PATH` — the freshly installed 24 was silently ignored. The script now picks the newer binary explicitly
- `$("cmd")` (quoting the whole command inside a substitution) turns the command name into a filename with a space; this is why OpenClaw's version line came out empty

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
