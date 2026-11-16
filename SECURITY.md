# Security

**English** ｜ [简体中文](SECURITY.zh-CN.md)

The panel is an administration tool that holds root credentials for your servers. This document states
what it protects, what it deliberately does not, and how to report a problem.

## Supported versions

| Version | Supported |
|---|---|
| 1.8.x | yes |
| < 1.8 | no — upgrade |

## Reporting a vulnerability

Please **do not** open a public issue for a security problem.

- Preferred: GitHub → *Security* → *Report a vulnerability* (private advisory) on this repository.
- Otherwise: mail the maintainer listed in the repository profile.

Include the version (`curl -s http://127.0.0.1:7788/api/ping`), platform, and the shortest sequence
that shows the problem. You will get an acknowledgement, and credit in the release notes unless you
prefer otherwise.

## Threat model

**Trusted**

- The machine the panel runs on, and the user account it runs as.
- The network path to the server (`ssh`), assuming the SSH host is the one you meant to reach.

**Not trusted**

- Any other process on the same machine that can reach `127.0.0.1:7788`.
- Websites you have open in the same browser (they must not be able to make the panel act).
- Anyone who can reach the port if you bind it to a non-loopback address.

**In scope**

| Asset | Why it matters |
|---|---|
| Server credentials (passwords, private keys) | Root on every managed server |
| The servers themselves | The panel can run arbitrary commands as root |
| `data/` on the local machine | Encrypted credentials, `secret.key`, backups, activity log |

**Out of scope**

- A compromised local machine: with code execution as your user, the panel's key is readable by design.
- A compromised server: the panel is a client; it cannot protect a host that is already owned.
- Physical access, or someone using your unlocked session.

## What is implemented

| Control | Where |
|---|---|
| Binds to `127.0.0.1` by default and warns loudly when it does not | `main.go` |
| No login at all — the local port *is* the credential | design choice, see below |
| Non-`GET` requests must carry `x-panel: 1`; otherwise `403` | `internal/httpapi` |
| Credentials encrypted with AES-256-GCM, one random key per install | `internal/store` |
| `secret.key`, `servers.json`, log and backup files created `0600`, data dir `0700` | `internal/store`, `launcher.go` |
| Credentials never appear in URLs, query strings or the activity log | all layers |
| Database access uses the server's local root auth (socket), no password on the command line | `internal/dbx` |
| Domain names, ids, log paths and FTP paths validated against allow-lists before use | `internal/httpapi` |
| Destructive SQL (`CREATE USER`, `GRANT`, `LOAD DATA`, `INTO OUTFILE`, `LOAD_FILE`, …) refused | `internal/dbx` |
| Destructive UI actions require typed confirmation (domain name, site type) | `public/app.js` |
| Shell arguments quoted by a single quoter (`ShellQuote`), never string-concatenated unquoted | `internal/sshc` |
| Remote scripts written to `/tmp/.wpqu-panel-*`, executed, then removed regardless of outcome | `internal/ops` |
| No port opened, no daemon installed, no persistent agent on the server | design |
| Oversized request bodies rejected (`readJSON` limit, upload streaming) | `internal/httpapi` |

## Known gaps

These are deliberate trade-offs or unimplemented work. Read them before exposing the panel to anything.

1. **No SSH host key verification.** Connections use `ssh.InsecureIgnoreHostKey()`. A machine-in-the-middle
   could impersonate a server on first contact (or any contact) and receive the credentials you type.
   Mitigation: connect from a trusted network, or pin `known_hosts` yourself (see roadmap). This is the
   most significant gap in the project.
2. **No authentication.** Anyone who can reach the port, or any process on your machine, is
   administrator. Do not set `-host 0.0.0.0` on a shared or internet-facing host.
3. **Backups are not encrypted.** `data/backups/` holds plain `.tar.gz` archives, including database
   dumps with password hashes and user data — the same material as a plain `mysqldump` on the server.
   Protect or move them accordingly.
4. **The activity log records commands.** Whatever you type in *Run command* or *cron* is stored in
   `data/logs/<date>.ndjson` — including secrets if you put them there. It is not redacted.
5. **Root by design.** Scripts run as root (or via `sudo -n`). A bug in a script is a root bug; the
   panel's validation is the only thing between the UI and the shell.
6. **CSRF protection is header-based only.** A cross-site `POST` cannot set `x-panel: 1` without a
   preflight, but script running on the panel's own origin would bypass it.
7. **No rate limiting, no lockout.** Local-only, so it has not been a priority.
8. **Unsigned binaries.** Verify the SHA256SUMS attached to a release, or build from source.

## Hardening checklist

- Keep the panel on `127.0.0.1`. If you need remote access:
  `ssh -L 7788:127.0.0.1:7788 you@your-desktop`
- Use a dedicated SSH key per server, with a passphrase, and `PermitRootLogin prohibit-password` on the
  server where possible (the panel supports `sudo -n` users).
- Store the binary in a directory only your account can write to; run it as your normal user, not as
  administrator/root.
- Keep `data/` on an encrypted disk (BitLocker, FileVault, LUKS), since `secret.key` sits next to
  `servers.json`.
- Restrict the server's cloud firewall: 80/443 public, 22 only from your IP.
- Enable the provider's snapshot/backup for anything you would hate to rebuild.

## Server-side footprint

To audit what the panel did on a server, look at:

- `/tmp/.wpqu-panel-*` — should be empty; a leftover means an interrupted upload
- `/root/panel-site-*.tar.gz` and `/tmp/wp-delsite.*` — configuration backups from type/domain changes
  and deletions
- `/var/log/nginx/…` or `/var/log/apache2/…` — what the panel wrote is ordinary config
- shell history is *not* touched: the panel executes through SSH, not an interactive shell

Nothing else is installed: uninstalling the panel means deleting the local `data/` directory.

## Not a vulnerability

- The panel is reachable from the network because you passed `-host 0.0.0.0`.
- An operation you confirmed destroyed data (that is what the confirmation is for).
- The remote scripts' output being Chinese, or the API returning Chinese error messages.
- Missing protection against an attacker who already has your user account on the local machine.
