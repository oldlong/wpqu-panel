# Contributing

**English** ｜ [简体中文](CONTRIBUTING.zh-CN.md)

Thanks for taking a look. This is a small project with a deliberately boring architecture; the rules
below exist so it stays that way.

## Good first contributions

| Kind | Notes |
|---|---|
| **A new UI language** | The most useful thing you can add. Copy `public/i18n/en.js`, translate, register — three edits, see [docs/ARCHITECTURE.md § 9](docs/ARCHITECTURE.md#9-interface-language) |
| **Missing translations** | Compare `public/i18n/en.js` with the strings in `public/app.js`; missing keys fall back to Chinese |
| **Docs** | Especially the English guide: screenshots, clearer wording, corrections |
| **A bug report with a reproduction** | Half the value of a fix |
| **Support for another distribution** | `scripts/install-wordpress.sh` currently hard-codes Debian/Ubuntu idioms |

Big features: open an issue first and describe what you want to change. The panel intentionally does
not have a plugin system, a database of its own, or user accounts — please check that your idea fits
before writing a lot of code.

## Setup

```bash
git clone https://github.com/oldlong/wpqu-panel.git
cd wpqu-panel
go build -o wpqu-panel .        # or ./build.sh
PANEL_DATA=./devdata go run . -no-browser -port 7789
```

- Go **1.24+**, no CGO. Dependencies are `golang.org/x/crypto` (SSH) and `github.com/pkg/sftp` — adding
  more needs a good reason.
- `devdata/` keeps development profiles away from your real `data/`. It is git-ignored.
- Anything that talks to a server needs a **real** server. There is no mock layer; that is on purpose.

## Checks before you push

```bash
gofmt -l .                       # must print nothing
go vet ./...                     # must be silent
go test ./...                    # version, plugins and updater packages
bash -n scripts/*.sh             # syntax check the remote scripts
node --check public/app.js       # the UI is one file; this catches typos
./scripts/sync-ws-lib.sh --check # if you touched _ws-lib.sh
```

There is no CI yet, so this list is the gate. Also do the thing the checks cannot: run the panel and
click through what you changed.

## Remote scripts (`scripts/`)

These are uploaded as standalone files and executed as root, which drives most of the rules:

- **Self-contained.** No `source`, no sibling files, no assumptions about the working directory.
- **LF line endings.** Add `*.sh text eol=lf` awareness; `.gitattributes` already enforces it.
- **Input via environment variables**, never positional arguments, with defaults at the top:
  `DOMAIN="${DOMAIN:-}"`.
- **Never interactive.** No `read`, no `apt` upgrades that ask questions; use `-y` and
  `DEBIAN_FRONTEND=noninteractive`.
- **Idempotent.** Re-running must not break a configured machine.
- **Back up, then act, then verify.** Copy the old config to `/root/`, run `nginx -t` /
  `apache2ctl configtest`, roll back if it fails.
- **Machine-readable output** where the Go side parses: `KEY=value` lines, `###SECTION` markers.
- **Human output in Chinese**, `[+]` for progress, `[x]` for failure, non-zero exit on failure.
- **Web-server specific work goes through `scripts/_ws-lib.sh`.** Never branch on nginx/Apache inline
  in a script; add a function to the library instead.

After editing `_ws-lib.sh`:

```bash
./scripts/sync-ws-lib.sh        # rewrites the five dependent scripts
./scripts/sync-ws-lib.sh --check
```

Commit the generated scripts together with the library change. Do not hand-edit the injected block.

## Go code

- Standard library first. `internal/` packages are small on purpose: `store` (profiles), `sshc`
  (transport), `sites` (read-only inventory), `ops` (actions), `dbx` (database), `httpapi` (HTTP).
- Validation belongs in `httpapi`, before any stream is opened; reuse `AssertDomain`,
  `assertPHPVersion`, `assertLogPath` rather than inventing new regexes.
- Errors are returned, not logged and swallowed. The HTTP layer turns a `dbx.BadRequest` into `400`,
  anything else into `500`.
- User-facing error messages are Chinese for now; keep a change consistent with its neighbours rather
  than mixing languages in the same dialog.
- Comments: Chinese or English, whichever explains the *why* better. Comments explaining a non-obvious
  workaround are worth more than comments restating the code.

## Frontend

- `public/app.js` is one file of template literals, no framework, no build step. Keep it that way.
- Find the section banners (`/* ---- 概览 ---- */`) and put things where they belong.
- New visible text: write Chinese in the template, then add the key to `public/i18n/en.js`. If the
  sentence contains a number, add a regex pattern in `public/i18n/core.js`.
- Never rely on translating `<pre>`, `<code>` or `<textarea>` content — those are skipped on purpose.
- After changing the UI, reload and check both languages; the English layout is usually the one that
  breaks (longer strings).

## Pull requests

- One logical change per PR. Formatting-only commits mixed with behaviour changes are painful to review.
- Say what you tested and on which distribution/version. "Works on my machine" is not enough for
  anything that touches `scripts/`.
- Include the negative path: what happens when the config test fails, the port is blocked, the domain
  is invalid.
- Update `CHANGELOG.md` for user-visible changes (both language versions if you can; otherwise English
  is fine and a maintainer will mirror it).
- Update the docs when behaviour changes — `docs/GUIDE*.md`, `docs/API*.md`, `README*.md`.

## Commit messages

Short imperative subject, then a body only when the reason is not obvious:

```
fix(add-site): keep the PHP pool when the socket is shared

The pool reference count grepped only sites-available, so a pool shared with
a snippet looked unused and was deleted on the next site removal.
```

## Licensing of contributions

The project is GPL-3.0. By opening a pull request you agree that your contribution is licensed under
the same terms (inbound = outbound). There is no CLA and no copyright assignment.

## Conduct

Be straightforward, assume good faith, and keep criticism about the code. Reports of a security issue
go through [SECURITY.md](SECURITY.md), not the issue tracker.
