# Mire

A simple, multi-tenant feed reader

## Running and deploying

`go run .` keeps the development defaults: `:5544`, `./mire.db`, and `./files`.
Build production binaries with `go build -tags release`. Runtime overrides:

| Variable | Default | Meaning |
| --- | --- | --- |
| `MIRE_LISTEN_ADDR` | `:5544` | HTTP bind address; use `127.0.0.1:5544` behind the VPS proxy. |
| `MIRE_DATABASE` | `mire.db` | SQLite file, resolved relative to the working directory. |
| `MIRE_FILES_DIR` | `files` | Matching release templates and static assets. |
| `MIRE_REQUIRE_DATABASE` | `false` | Refuse a missing database instead of creating an empty installation. |
| `MIRE_BACKGROUND` | `true` | Automatic feed polling and statistics. Set false only for isolated rehearsals; explicit subscription requests can still fetch feeds. |

SIGTERM/SIGINT drains HTTP requests, cancels remaining feed requests, flushes
accepted post batches, and joins background workers before closing SQLite.
Shutdown has a 90-second application deadline and a 120-second systemd deadline.
`GET /healthz` checks the database connection and reports the serving revision in
`X-Mire-Revision`.

### Managed VPS

The repository-owned updater requires Linux, Go, Python 3.11+, SSH and SCP:

```sh
# Run from a clean, committed checkout. Uses normal SSH configuration and keys.
python3 scripts/deploy_vps.py meadow-ubuntu-8gb-hel1-1 --yes

# On the VPS:
systemctl status mire.service
journalctl -u mire.service --no-pager
```

Managed layout:

- Immutable binary, matching assets and unit under `/opt/mire/releases/`, selected
  by `/opt/mire/current`.
- Live database at `/var/lib/mire/mire.db`, owned by the dedicated `mire` account.
- Private runtime configuration at `/etc/mire/mire.env`.
- Enabled, hardened `mire.service`, bound to loopback and supervised by systemd.
- Private snapshots, source, logs and durable receipts in
  `/root/mire-deploy-backups/update-<id>/`.

The updater tests an archived committed source tree, builds a matching release,
checks upload hashes, and rehearses startup against a private SQLite snapshot
under the service account with external networking blocked and polling disabled.
It refuses a rehearsal that changes database contents or schema: review schema
migrations separately, rather than silently accepting them during an update.
An identical healthy release is a no-op.

Cutover briefly stops only Mire, takes a fresh WAL-safe snapshot, switches the
release, and checks the actual process and public HTTPS application. Unchanged
data permits a binary-only rollback; if data or schema changed after activation,
the service remains stopped and disabled with a recovery receipt. **The updater never restores
an old database over accepted writes.** An interrupted deployment marker must be
reviewed before retrying. Retain private receipts and snapshots until recovery is
no longer needed; they are not automatically pruned.

Initial migration from this VPS's `/root/mire` tmux wrapper is explicitly gated:

```sh
python3 scripts/deploy_vps.py meadow-ubuntu-8gb-hel1-1 --yes --migrate-tmux \
  --legacy-pid VERIFIED_APP_PID --wrapper-pid VERIFIED_WRAPPER_PID \
  --legacy-sha256 VERIFIED_RUNNING_BINARY_SHA256
```

First inspect the running executable, both process identities and local changes.
The migration stops only the pinned wrapper/app, copies and logically verifies
the stopped database, and retains the old files. It changes only Mire's source
path and database-parent permission in the existing managed Backuper configuration,
under Backuper's job lock. Other sources, destinations, retention, secrets, cron
jobs, Caddy routes and tmux applications remain untouched. A safe initial failure
can run the old binary under a temporary `mire-legacy-rollback.service`; inspect
the receipt and leftover managed files before any retry. Do not restart the old
auto-pulling wrapper alongside the managed service.

Tests:

```sh
go test ./...
go test -race -tags release ./...
python3 -B -m unittest discover -s scripts -p 'test_deploy*.py'
```

- features:
  - rss and atom support
  - minimal, simple, reliable, fast
  - refresh your feeds automatically
  - display a chronological list of feed items
  - track read status for each post
  - open source & free of charge forever

- anti-features:
  - no tags
  - no options
  - minimal javascript
  - no comments, upvotes, or ranks

Feeds are checked every ten minutes and normally fetched no more often than
every three hours. When a feed responds with `Cache-Control: max-age`, Mire
waits until that freshness period has elapsed if it is longer than three
hours. If there is no usable `max-age`, Mire uses `Expires` instead. These
per-feed deadlines survive restarts; new feeds can still be fetched on first
subscription. Actual requests may occur later than the deadline because
the scheduler checks every ten minutes.
