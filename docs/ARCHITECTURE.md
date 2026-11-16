# Architecture

**English** ｜ [简体中文](ARCHITECTURE.zh-CN.md)

How the panel is put together, and where to touch it when you add something. For usage see
[GUIDE.md](GUIDE.md), for endpoints see [API.md](API.md).

- [1. Big picture](#1-big-picture)
- [2. Repository map](#2-repository-map)
- [3. Lifecycle of a write operation](#3-lifecycle-of-a-write-operation)
- [4. The script contract](#4-the-script-contract)
- [5. Web server abstraction](#5-web-server-abstraction)
- [6. On-disk layout on the server](#6-on-disk-layout-on-the-server)
- [7. Local data and encryption](#7-local-data-and-encryption)
- [8. Frontend](#8-frontend)
- [9. Interface language](#9-interface-language)
- [10. Adding a feature](#10-adding-a-feature)
- [11. Build and release](#11-build-and-release)
- [12. Plugins and licenses](#12-plugins-and-licenses)
- [13. Self-update and signing](#13-self-update-and-signing)
- [14. Testing](#14-testing)
- [15. Trade-offs](#15-trade-offs)

## 1. Big picture

The panel is a **desktop tool with a browser UI**. It does not run on the server, it does not phone
home, and it holds no state on the server other than the configuration it was asked to write.

```
          your machine                                server
┌────────────────────────────────┐          ┌──────────────────────────────┐
│  wpqu-panel (single binary)    │          │                              │
│                                │          │  /tmp/.wpqu-panel-*.sh       │
│  public/   HTML+JS+CSS  ───────┼── embed  │      │                       │
│  scripts/  bash         ───────┤          │      └─► env VAR=… bash …    │
│                                │          │             │                │
│  httpapi ──► ops.Runner ───────┼── SSH ──►│             ├─► nginx/apache │
│     ▲           │              │  exec    │             ├─► php-fpm      │
│     │           └─► sshc pool ─┼── SFTP ──►│             ├─► mariadb      │
│  browser (127.0.0.1:7788)      │          │             └─► certbot      │
└────────────────────────────────┘          └──────────────────────────────┘
```

Four consequences worth knowing:

1. **Nothing is installed for the panel itself.** No agent, no daemon, no open port on the server.
   Plugins do install software, but that is a script you asked for by clicking Install — the same
   shape as creating a site.
2. **The scripts are the implementation.** The Go code mostly validates input, uploads a script,
   and parses what comes back. If you want to know what an action really does, read the script.
3. **The panel is stateless with respect to sites.** The site list is read back from
   `sites-enabled` / `sites-available` every time; there is no local inventory that could drift.
4. **Every outbound request is user-initiated.** `panel.wpqu.com` is contacted only when you click
   Refresh in the plugin market or Check for updates. No startup ping, no heartbeat, no telemetry.

## 2. Repository map

```
main.go                  flags, port walking, single-instance reuse, open browser
launcher.go              data directory resolution + legacy migration, log file
ui_windows.go            open browser via rundll32, message box when no console
ui_other.go              macOS/Linux equivalents

internal/store/          server profiles, AES-256-GCM credentials, ndjson activity log
internal/sshc/           SSH pool, exec with streaming callbacks, SFTP helpers
internal/sites/          overview, site list, site detail (script text + output parsing)
internal/ops/            runner for uploaded scripts; deploy, add/convert/rename/delete site,
                         certificates, backups, health check, FTP, maintenance
internal/dbx/            database browsing, SQL inspection, export/import/optimize
internal/httpapi/        routing, validation, ndjson streaming, static assets, file manager
internal/version/        semver comparison (prerelease sorts before release)
internal/signing/        Ed25519 verification; both public keys compiled in as constants
internal/plugins/        catalog, download/unpack, license keys, batched status probing
internal/updater/        update check, staged download, swap and restart (platform-split)

cmd/wpqusign/            key and license issuance, signature verification (no openssl needed)
cmd/wpqupack/            packs plugins and update manifests, emits the site/ tree

public/index.html        app shell (sidebar, topbar, drawer, toast)
public/app.js            all UI logic, no framework
public/style.css         design tokens + layout, light/dark
public/i18n/             core.js (translator) + zh-CN.js + en.js + dicts.js

scripts/*.sh             remote scripts, embedded at compile time
scripts/_ws-lib.sh       nginx/Apache adapter library (source of truth)
scripts/sync-ws-lib.sh   injects _ws-lib.sh into the five scripts that need it

plugins-src/<id>/        plugin sources: plugin.json + scripts/ + ui/, packed into site/
site/                    the distribution site (root of panel.wpqu.com): pages, manifests,
                         plugin zips, binaries
tools/release.sh         one release: version check, cross-build, pack plugins and manifests,
                         verify signatures
tools/sync-github.sh     syncs panel-go into the open-source tree github/

build.sh, build.bat      local and cross-compilation
```

The binary embeds `public/` and `scripts/` via `//go:embed`. Editing a script or a JS file therefore
requires a rebuild, and a rebuilt binary is the only thing you have to ship.

## 3. Lifecycle of a write operation

Taking "create a site" as the example:

1. **Browser** posts JSON to `/api/servers/:id/sites` with the `x-panel: 1` header.
2. **httpapi** resolves the server, decrypts its credentials, validates every field
   (domain syntax, site type, PHP version, …). Validation happens *before* the stream opens, so a
   bad request returns a normal JSON error instead of a half-open stream.
3. **ops.Runner** reads the script text out of the embedded FS, normalises CRLF → LF, uploads it over
   SFTP to `/tmp/.wpqu-panel-<millis>-add-site.sh`, and compares the remote size with the local size.
   A silent truncation at this point would be a debugging nightmare, so it is checked.
4. **exec** runs `env DOMAIN=… bash /tmp/.wpqu-panel-… 2>&1` (prefixed with `sudo -n` when the user is
   not root). Stdout and stderr are merged and delivered line by line.
5. **httpapi** writes each chunk to the response as a JSON object on its own line
   (`Content-Type: application/x-ndjson`):

   ```json
   {"type":"start","message":"新增站点 example.com（wordpress）"}
   {"type":"out","text":"[+] 目录已创建：/var/www/example.com"}
   {"type":"out","text":"[+] 证书签发成功"}
   {"type":"end","code":0,"ok":true,"durationMs":18432,"message":"执行完成"}
   ```

   On failure the last event is `{"type":"error","message":"…"}` and the operation is recorded as failed.
6. The script is removed from `/tmp` (in a goroutine, regardless of outcome) and the activity log gets
   one line.

The browser feeds `out` events into the drawer verbatim, which is why the progress display looks like
a terminal: it *is* the script's output.

## 4. The script contract

Every script in `scripts/` follows the same rules, because the panel uploads it as a standalone file:

| Rule | Why |
|---|---|
| **Self-contained.** No `source`, no helper files beside it. | Only this one file is uploaded. |
| **LF line endings.** | CRLF breaks bash with `$'\r': command not found`. The Go side normalises too. |
| **Input via environment variables**, e.g. `DOMAIN`, `SITE_TYPE`, `SSL`. | No quoting problems, no argv parsing, and secrets never appear in `ps`. |
| **No interactive prompts.** | There is no TTY. Anything that waits for input hangs until timeout. |
| **Machine-readable lines for the parser**: `KEY=value` and `###SECTION` markers. | `internal/sites` and `internal/dbx` parse these. |
| **Exit code is the verdict.** `0` = success, non-zero = failure. | The panel maps it to `ok` in the `end` event. |
| **Human-readable output is Chinese and colored** (`[+]`, `[x]`, `── section ──`). | It is shown as-is in the drawer; ANSI codes are stripped server-side. |
| **Idempotent where possible.** | Users re-run things, and the installer is used on machines that are half configured. |
| **Back up before overwriting, roll back on failed config test.** | `nginx -t` / `apache2ctl configtest` failures must not leave a broken server. |

### The adapter library and why it is injected

`scripts/_ws-lib.sh` holds everything that differs between nginx and Apache: config paths, domain
directives, document root detection, socket detection, `configtest`/reload, certificate plugin, log
directory, site-type markers. Five scripts need it (`add-site`, `change-domain`, `delete-site`,
`site-convert`, `php-manage`), and none of them can `source` it, because only the script itself is
uploaded.

So the library is **injected**: `scripts/sync-ws-lib.sh` replaces the text between
`# >>> ws-lib BEGIN` and `# <<< ws-lib END` in those five files with the current contents of
`_ws-lib.sh`.

```bash
./scripts/sync-ws-lib.sh          # rewrite the five scripts
./scripts/sync-ws-lib.sh --check  # verify they are in sync (exit 1 if not)
```

Edit `_ws-lib.sh`, run the sync, and commit both. Never edit the injected block by hand — the next
sync will overwrite it.

## 5. Web server abstraction

`ws_init` decides once per script whether it is talking to nginx or Apache (whoever is active; if both,
Apache wins because a running Apache usually means nginx was stopped on purpose), then the rest of the
script only uses the adapter:

| Concern | nginx | Apache |
|---|---|---|
| Config file | `/etc/nginx/sites-available/<domain>` | `/etc/apache2/sites-available/<domain>.conf` |
| Enable | symlink in `sites-enabled` | `a2ensite` / `a2dissite` |
| Domain | `server_name a b;` | `ServerName` + `ServerAlias` |
| Root | `root /var/www/x;` | `DocumentRoot` |
| PHP | `fastcgi_pass unix:/run/php/x.sock` | `SetHandler "proxy:unix:/run/php/x.sock\|fcgi://localhost"` |
| Test / reload | `nginx -t`, `systemctl reload nginx` | `apache2ctl configtest`, `systemctl reload apache2` |
| Certificate | `certbot --nginx` | `certbot --apache` |
| Logs | `/var/log/nginx/` | `/var/log/apache2/` |
| Rewrite rules | snippet included by the vhost | `.htaccess` written into the root |

Apache vhosts are rewritten as a whole whenever the capability changes (the PHP handler, the static
`Require all denied`, the WordPress block). When a certificate exists, both a `:80` block with a
`/.well-known` exception and a `:443` block are written, so converting a site does not lose HTTPS.

Site type markers moved to `/etc/wp-panel/site-types/<domain>` — outside both web servers' trees — so
the same marker survives a switch between them. The old `/etc/nginx/site-types/` location is still read
and migrated.

## 6. On-disk layout on the server

```
/var/www/<domain>/                                 document root
/etc/nginx/sites-available/<domain>                vhost (nginx)
/etc/nginx/snippets/site-<domain>.conf             capability snippet (PHP / static / WP rules)
/etc/wp-panel/site-types/<domain>                  wordpress | custom | static
/etc/php/<ver>/fpm/pool.d/<domain>.conf            PHP-FPM pool (per site only when opted in)
/etc/letsencrypt/live/<domain>/                    certificate
/var/log/nginx/<domain>.{access,error}.log         logs

Apache: /etc/apache2/sites-available/<domain>.conf, sites-enabled/<domain>.conf,
        /var/log/apache2/<domain>.{access,error}.log, .htaccess in the root
```

A site is "known" to the panel only because these files exist. Delete them by hand and the panel will
agree the site is gone — nothing to clean up in a database.

## 7. Local data and encryption

```
data/
├── servers.json    {"servers":[{"id","name","host","port","username","authType","credentials","…"}]}
├── secret.key      32 random bytes, base64
├── panel.log       startup log
├── logs/<date>.ndjson
└── backups/<serverId>/
```

Credentials (password, private key, passphrase) are encrypted with AES-256-GCM using a per-install key
from `secret.key`, stored with `0600`. The key never leaves the machine; the file is created on first
run with a CSPRNG. Losing `secret.key` means re-entering credentials — there is no recovery path by
design.

Two instances can share nothing but the binary: point `PANEL_DATA` somewhere else and use a different
port.

## 8. Frontend

No framework, no build step: `app.js` is a single file of template-literal rendering plus a `state`
object. A render function builds HTML from state, and event handlers mutate state and re-render. It
keeps the panel quick to iterate and means `public/` can be edited and reloaded without tooling.
The cost is that everything is in one file — search for the section banner comments.

The only non-obvious pieces:

- **`api()`** wraps `fetch`, adds the `x-panel: 1` header, parses JSON, throws with the server's error
  message.
- **Streaming** reads the ndjson response with `getReader()` and dispatches `start` / `out` / `error` /
  `end` into the drawer.
- **Theme** is applied on `documentElement` before first paint (inline script in `index.html`) to avoid
  a white flash in dark mode.

## 9. Interface language

The UI is translated **after** rendering, not inside the templates. That decision is what makes the
i18n layer small: `app.js` has no `t()` calls at all.

```
public/i18n/core.js     LANGS, lookup(), DOM walker, MutationObserver, number patterns
public/i18n/zh-CN.js    empty object — the source language is the key
public/i18n/en.js       { '中文原文': 'English text', … }  (~700 entries)
public/i18n/dicts.js    registry: { 'zh-CN': zhCN, en }
```

How it works:

1. On load, `initI18n()` picks a language (`?lang=` → `localStorage` → `navigator.language`).
2. It walks text nodes and the `placeholder` / `title` / `aria-label` attributes of `#app`, looks each
   string up in the dictionary, and replaces it.
3. `MutationObserver` repeats the walk for DOM that is rendered later (tab switches, dialogs, toasts).
   A `WeakSet` prevents re-translating what it wrote itself.
4. `<pre>`, `<code>` and `<textarea>` are skipped: remote script output, log bodies and user input must
   stay as they are.
5. Sentences with numbers go through regex patterns (`站点 3 个` → `Sites: 3`, `证书剩 88 天` →
   `88 days left`), so they do not need one dictionary entry per count.
6. A missing key returns `null` and the Chinese original stays — a half-finished language pack degrades
   gracefully.

**Adding a language** (three edits):

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

The switcher cycles through `LANGS` automatically, and `?lang=de` works immediately.

What is *not* translated: strings produced by the remote scripts (they are Chinese), server names,
domains, database and table names, log content, command output, and Go-side API error messages.

## 10. Adding a feature

A feature usually touches four layers. Using "restart php-fpm for a site" as an example:

1. **Script** — add an action to the relevant `scripts/*.sh` (here `php-manage.sh`), driven by an env
   var (`PHP_ACTION=restart`). Keep it idempotent, print `[+]`/`[x]`, exit non-zero on failure.
   If it touches web server config, go through `_ws-lib.sh` and run `sync-ws-lib.sh`.
2. **Ops** — add a method on `ops.Runner` that fills in the env map and calls `RunUploadedScript`
   (or `strings.NewReplacer` the script's own default block, as some maintenance actions do).
3. **HTTP** — register a route in `internal/httpapi/http.go`. Use `streamRoute` if there is output to
   stream, a plain `route("POST", …)` otherwise. Validate before opening a stream; use the existing
   `AssertDomain` / `assertPHPVersion` helpers rather than writing new regexes.
4. **UI** — a button in `app.js`, then either `streamTo(url, body)` or `api(url, {method:'POST'})`.
   Add the button label to `public/i18n/en.js` (the Chinese string is the key) and, if it produces
   new numbers-in-sentences, a pattern in `core.js`.
5. `./build.sh` — scripts are embedded, so nothing works until the binary is rebuilt.

## 11. Build and release

```bash
./build.sh                # current platform
./build.sh all            # windows amd64 (GUI + console), linux amd64/arm64, darwin amd64/arm64
```

`CGO_ENABLED=0` everywhere, `-trimpath`, `-ldflags "-s -w"`, and `-H=windowsgui` for the GUI build so
double-clicking does not open a console. The version string lives in `internal/httpapi/http.go`
(`const Version`); `build.sh` reads it for the banner.

A release goes through `tools/release.sh <version>`: it checks that `const Version` matches the
argument, runs `build.sh all`, then `wpqupack updates` (copies binaries into `site/dl/<version>/`,
signs `updates.json`, keeps the last 20 versions) and `wpqupack plugins` (packs `plugins-src/`, signs
the index and every package), and finally asserts that every `.json` / `.zip` has a `.sig`. The output
is `site/` — that directory is what you upload to `panel.wpqu.com`.

Releases are still built locally and attached to a GitHub release by hand — there is no CI yet. If you
add one, `./build.sh all` plus `dist/SHA256SUMS` is all it needs; `site/` can be generated in CI by the
same script, but the private keys must not go there.

## 12. Plugins and licenses

Plugins carry the capabilities the panel does not build in: install a runtime, install Docker, install
OpenClaw, or add a tab to the panel. The design keeps **plugins out of the main code path** — the panel
knows about a manifest, never about a specific plugin.

### One manifest, two shapes

`plugins-src/<id>/plugin.json` describes both at once:

```json
{
  "id": "golang", "version": "1.0.0", "category": "runtime", "tier": "free",
  "install": {
    "check": "test -x /usr/local/go/bin/go",
    "steps": [{ "title": {"en": "Install Go"}, "file": "scripts/install.sh" }],
    "uninstall": [{ "file": "scripts/uninstall.sh" }],
    "params": [{ "key": "version", "default": "" }]
  },
  "requires": { "os": ["linux"], "minPanel": "1.9.0", "license": false }
}
```

- With `install.steps` it puts software on a server, running those scripts through the same runner the
  site operations use
- With `ui.entry` it adds a panel tab; `entry` points at a JS module in the package exporting
  `mount(el, ctx)`
- Both can coexist (install a service, then give it a panel view)

**There is no `switch pluginID` anywhere in the Go code.** A new plugin is a directory plus a manifest
entry, and nothing else.

### Install path

```
POST /api/servers/:id/plugins/:pid/install
  ├─ panel version >= requires.minPanel    (an old panel would not understand new fields anyway)
  ├─ requires.license -> Manager.License() (local verify + expiry)
  ├─ remote uname -s in requires.os        (every recipe is Linux package management)
  ├─ params keys -> WPQU_* env vars        (illegal characters dropped, never spliced into shell)
  ├─ Manager.Ensure()                      (download / verify / unpack, below)
  └─ one RunScriptText() per step          (ndjson: start / step / out / end)
```

The order inside `Manager.Ensure()` matters:

1. Same version already unpacked and present → return it, no download
2. Download `package.url` (relative paths resolve against `Base`)
3. Compare `package.sha256`
4. Fetch `url + ".sig"` and verify the package against the **release public key**
5. Unpack into `.staging-<id>` (rejects `..`, caps at 800 files / 64 MB)
6. Verify the **inner** `plugin.json` id matches the catalogue — stops package A impersonating B
7. `RemoveAll(dest)` + `Rename(staging, dest)`: the swap is atomic

Any failure leaves the old directory untouched; the staging directory is removed, so there is never a
half-installed plugin.

### Why status probing is one SSH call

Installed-or-not plus version would naturally be one command per plugin, so N plugins means N round
trips — painful on a lossy link. `StatusScript()` concatenates N `if/then` blocks into **one** script,
each prefixed with a `###<id>` marker:

```
echo "###golang"; if test -x /usr/local/go/bin/go; then echo "1 $(… --version)"; else echo "0"; fi
echo "###docker"; if command -v docker >/dev/null; then echo "1 $(docker -v)"; else echo "0"; fi
```

`ParseStatus()` has two traps: output can lose newlines (`0###docker`) and commands can write noise to
stderr. It splits on `###` and re-inserts newlines, then only accepts compact-format lines — anything
unrecognisable counts as not installed.

### License keys

```
WPQU1.<base64url(payload)>.<base64url(ed25519_sig)>
payload = {"p":"openclaw","e":"2027-12-31","s":5,"n":"Jane Doe","i":"lic_7f3a","t":1730000000}
```

Offline signatures rather than "account + online check" for a simple reason: the panel does not go
online, does not upload anything and does not open a port. Verifying a license online would mean
introducing a server address, an account system and a revocation policy — turning a desktop tool into a
client. The only thing it buys is stopping people who were never going to pay.

The cost is **no revocation**. Expiry works; refunds do not, and handling them would need networking.
That trade-off is stated in the README's paid-plugin section.

Keys live in `data/licenses.json` as `{keys:{<id>:<code>}}`, and `/api/plugins` only echoes the decoded
summary (customer, expiry, seats) — never the original string.

### UI plugin assets

After install, `ui.entry` is served from `/api/plugin-assets/<id>/<path>`: read-only, confined to the
unpacked plugin directory, path `Clean`ed before joining and 404 outside the root. The plugin JS runs
inside the panel page, so it has the panel's privileges — which is exactly why UI plugins are signed.

## 13. Self-update and signing

A panel that updates itself is awkward: a running binary cannot overwrite itself. The work is split
into three stages, each of which can fail without touching the working install.

### Three stages

| Stage | Function | Key point |
|---|---|---|
| Check | `Client.Check()` | fetch `updates.json`, verify, pick by `GOOS/GOARCH`, honour `minSupported` |
| Stage | `Client.Stage()` | hash while downloading, write `data/tmp/*.part`, rename only after the hash matches |
| Swap | `Restart(staged)` | platform-specific, see below; rolls back on failure |

`Check()` caches nothing, because `/api/update/apply` calls it **again** — a URL or hash supplied by the
client is never trusted, otherwise the endpoint would be "download an arbitrary URL and run it as root".

### Windows: rename, don't delete

A running exe cannot be deleted or overwritten on Windows, **but it can be renamed**:

```
copy staged -> <exe>.new
Rename(<exe>, <exe>.old)        // allowed while running
Rename(<exe>.new, <exe>)        // on failure, rename .old back
StartProcess(<exe>, same args)  // with WPQU_RESTARTED=1 in Env
```

`.old` survives until the next startup, where `CleanupOld()` removes it — and if the new build refuses
to start, renaming `.old` back is the rollback path.

`WPQU_RESTARTED=1` is not decoration: `main.go` normally probes the port range for an instance of the
same version and exits if it finds one. The new process inherits the variable, skips reuse, and takes
the port over.

The first attempt wrote a `.cmd` that waited for the parent to exit and then swapped files. In a
windowless GUI process that `.cmd` exits immediately: `.new` was in place but `.old` never became the
real name and no new process started. Hence synchronous renames, where every step can be checked.

### Linux / macOS: rename + exec

`os.Rename` within a directory is atomic; `syscall.Exec` then replaces the process image in place (no
fork, no return). PID, listening fds and the console stay the same, so the port does not need rebinding
and the `-data` argument does not need re-parsing.

### Who may self-update

`CanSelfUpdate()` checks two things:

- the binary was not started by `go run` (a temp-directory binary that disappears on swap)
- the exe's directory is writable (macOS `/Applications`, `/usr/local/bin` on Linux often are not)

When either fails, the About dialog shows the reason and the button is disabled. Better to say no than
to swap in a build that cannot start.

### Two keys, two chains

| Key | Signs | Public key constant |
|---|---|---|
| `release.key` | `updates.json`, the plugin `index.json`, each plugin `.zip` | `signing.ReleasePublicKey` |
| `license.key` | the payload of a customer license | `signing.LicensePublicKey` |

Separating them lets "ability to ship releases" and "ability to sell licenses" belong to different
people, and keeps one leak from taking out both.

The signature covers the **raw bytes of the file**, not parsed JSON. This is deliberate: JSON
serialisation guarantees nothing about key order, whitespace or number formatting, so any re-encode on
either end breaks verification. `wpqupack` therefore builds the final `body`, signs that same `body`,
and writes it — an earlier bug where one extra newline was appended after signing produced a panel-side
"signature verification failed" and is exactly this trap.

Private keys live only in `site/.keys/` (git-ignored); losing them means you cannot ship an update.
Public keys are compiled in, so rotating them requires a rebuild — intentional, and compatible with the
injectable `PublicKey` field used by tests and by self-hosted mirrors.

## 14. Testing

Unit tests cover the pure logic; real-machine runs cover "what bash and systemd actually think".

```bash
go test ./...            # version, plugins, updater
gofmt -l .               # should print nothing
go vet ./...
```

| Package | Covered |
|---|---|
| `internal/version` | ordering, prerelease below release, two-component versions do not panic |
| `internal/plugins` | license round-trip/expiry/tamper/whitespace, status parsing (tab gluing, noise), `Ensure` hash and inner-id mismatches, zip traversal, multi-line `StatusScript` |
| `internal/updater` | update found, already current, tampered manifest rejected, no package for this platform, `Stage` lands the file, wrong hash rejected |

Signature cases inject a test key through the `PublicKey` field, so no real private key is needed.

The real-machine loop:

1. Run from source against a throwaway server:
   `PANEL_DATA=./devdata go run . -no-browser -port 7789`.
2. Walk the affected feature on a **real** server, because almost everything interesting is a bash
   script talking to systemd, package managers and config files.
3. Check the negative path: invalid domain, port 80 blocked, certificate already present, wrong site
   type, config test failure, a plugin install that loses the network halfway.
4. `./scripts/sync-ws-lib.sh --check` if you touched the adapter.
5. If you touched the update path, run once from a copied directory with
   `WPQU_UPDATE_BASE=<local mirror>` and confirm the hash changed, `.old` is still there, and the new
   process reports the new `version`.

`data/logs/<date>.ndjson` is the fastest way to see what actually ran and how long it took; the update
path additionally writes `data/update.log`.

## 15. Trade-offs

| Decision | Cost | Benefit |
|---|---|---|
| Scripts embedded at compile time | Editing a script needs a rebuild | One-file distribution; no way for the binary and its scripts to disagree |
| No host key verification | A MITM on first connect is not detected | Works out of the box with fresh VPS images; TOFU pinning is a future option |
| No login | Must stay on loopback | No user database, no session handling, no web-facing auth to get wrong |
| Site list read from config files | Slightly slower overview | No drift between the panel and reality |
| i18n in the DOM, not the templates | Regex patterns for dynamic sentences | Templates stay readable; new UI is covered for free |
| Chinese-first backend messages | English users see Chinese errors from the API/scripts | Much smaller diff; UI language already covers the interface |
| Offline license verification | No revocation; refunds cannot be enforced | The panel stays a local tool: no networking, no login, no port |
| Signatures cover file bytes | Any re-serialisation breaks verification | The two ends can never disagree about JSON formatting |
| Plugin scripts run as root | A malicious plugin owns the server | Installing a runtime needs root anyway; replaces a pile of built-in install logic |
| Update apply trusts no client input | Every apply re-fetches the manifest | The endpoint cannot become "download any URL and swap yourself for it, as root" |
