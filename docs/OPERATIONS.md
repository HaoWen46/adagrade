# ADA-Marker Operations Runbook

ADA-Marker runs as a single Go binary on one Ubuntu-ish VM, backed by a local
Postgres instance and a local-disk blob store (`ADAMARKER_BLOB_DIR`, default
`./data/blobs`). This document covers install, TLS, environment configuration,
backup, and restore. It intentionally uses invented hostnames/emails
(`grader.example.edu`, `you@example.edu`) — never real student or staff data.

Deploy assets referenced below live in [`deploy/`](../deploy/):
`adamarker.service`, `adamarker-backup.service` + `.timer`, `backup.sh`,
`Caddyfile.example`.

## 1. Install

Assumes a dedicated VM, a system user `adamarker`, and Postgres 16 already
reachable (either the same host or another host on the private network).

Build/runtime prerequisites on the VM (Ubuntu package names):

- **Go 1.26+** — `go.mod` enforces `go 1.26.0`; an older toolchain fails the
  build immediately.
- **gcc / build-essential** — the binary links the onnxruntime Go binding via
  cgo even when local OCR stays disabled, so `make build` needs a C compiler
  (`sudo apt-get install -y build-essential`).
- **Node.js 20+ with npm** — `make frontend` runs `npm install && npm run
  build`; without it the binary embeds only the placeholder page.
- **postgresql-client** — `backup.sh` and the §5 restore procedure call
  `pg_dump`/`psql`/`createdb`, which are not in Ubuntu's base install
  (`sudo apt-get install -y postgresql-client`).
- Standard utilities (`tar`, `find`, `rsync`, `bash`, `curl`) — present on any
  non-minimal Ubuntu; `rsync` only needed if you set `BACKUP_RSYNC_TARGET`.

```bash
# 1. Create the service user and directory layout, and provision the Postgres role.
sudo useradd --system --home /opt/adamarker --shell /usr/sbin/nologin adamarker
sudo mkdir -p /opt/adamarker/bin /opt/adamarker/data /opt/adamarker/backups /etc/adamarker
sudo -u postgres createuser --createdb adamarker 2>/dev/null || true  # idempotent
sudo -u postgres createdb -O adamarker adamarker 2>/dev/null || true  # idempotent

# 2. Build the frontend first, then the binary (on a build host or the VM itself — Go 1.26+ required),
#    then copy the binary and deploy/ over. The Go binary embeds internal/web/assets/dist at
#    compile time (//go:embed), so make frontend MUST run before make build.
# Optional: fetch the report font now if you want result-PDF/ZIP email attachments
# (D42/D43) — downloads Noto Sans TC into ./data/fonts/, never committed. Skipping this
# leaves the attachment feature off (ADAMARKER_REPORT_FONT unset); publish still works.
make report-fonts
# Optional but recommended: fetch the local-OCR model files so scan identification
# reads ID crops on this host (see the ADAMARKER_OCR_* row in section 3 — you must
# also install libonnxruntime >= 1.27 yourself; make ocr-models does not).
make ocr-models
make frontend
make build
sudo cp bin/adamarker /opt/adamarker/bin/adamarker
sudo cp -r deploy /opt/adamarker/deploy
sudo chown -R adamarker:adamarker /opt/adamarker

# 3. Write the environment file (see the checklist in section 3 below).
sudo cp .env.adamarker.example /etc/adamarker/adamarker.env
sudo "$EDITOR" /etc/adamarker/adamarker.env
sudo chown root:adamarker /etc/adamarker/adamarker.env
sudo chmod 640 /etc/adamarker/adamarker.env

# 4. Install and enable the systemd units.
sudo cp /opt/adamarker/deploy/adamarker.service /etc/systemd/system/adamarker.service
sudo cp /opt/adamarker/deploy/adamarker-backup.service /etc/systemd/system/adamarker-backup.service
sudo cp /opt/adamarker/deploy/adamarker-backup.timer /etc/systemd/system/adamarker-backup.timer
sudo systemctl daemon-reload
sudo systemctl enable --now adamarker.service
sudo systemctl enable --now adamarker-backup.timer
```

`adamarker.service` has `EnvironmentFile=/etc/adamarker/adamarker.env` —
uncomment and fill in the lines you need there (values commented out fall back
to the defaults baked into `internal/config/config.go`).

The units in `deploy/` assume the app and its data both live under
`/opt/adamarker` (`WorkingDirectory=/opt/adamarker`, `ReadWritePaths=/opt/adamarker/data
/opt/adamarker/backups`). If you install elsewhere, edit
`/etc/systemd/system/adamarker.service` (and the backup unit) to match before
enabling, then `sudo systemctl daemon-reload`.

Check it came up:

```bash
sudo systemctl status adamarker.service
curl -s http://127.0.0.1:8080/ -o /dev/null -w '%{http_code}\n'
```

## 2. TLS (Caddy reverse proxy)

The app only speaks plain HTTP on `:8080` (`ADAMARKER_HTTP_ADDR`, default
`:8080`) — TLS termination is the reverse proxy's job, not the app's.
`deploy/Caddyfile.example` is a starting point:

```bash
sudo apt-get install -y caddy   # or the official Caddy apt repo for a newer version
sudo cp /opt/adamarker/deploy/Caddyfile.example /etc/caddy/Caddyfile
sudo "$EDITOR" /etc/caddy/Caddyfile   # replace grader.example.edu with the real hostname
sudo systemctl reload caddy
```

Caddy handles ACME/Let's Encrypt automatically as long as DNS for the
hostname resolves to this VM and ports 80/443 are reachable. No manual
certificate handling is required in steady state.

## 3. Environment checklist

Full reference: [`.env.adamarker.example`](../.env.adamarker.example). Copy it
to `/etc/adamarker/adamarker.env` and set at minimum, for production:

| Variable | Required in production? | Notes |
|---|---|---|
| `ADAMARKER_ENV` | yes | set to `production` — enables the validation below |
| `ADAMARKER_DATABASE_URL` | yes | `postgres://user:pass@host:5432/adamarker?sslmode=...` |
| `ADAMARKER_GOOGLE_CLIENT_ID` / `_SECRET` | optional | Google OAuth; if either is configured in production, both are required and the allowlist still gates access |
| `ADAMARKER_OAUTH_REDIRECT_URL` | yes if Google OAuth is used | must be the real `https://<host>/auth/callback`, not the `localhost` default |
| `ADAMARKER_HOSTED_DOMAIN` | recommended | Workspace `hd` claim guard, default `ntu.edu.tw` |
| `ADAMARKER_BOOTSTRAP_ADMIN_EMAIL` | recommended on first boot | seeds one admin so a fresh deploy isn't locked out; safe to remove after |
| `ADAMARKER_DEV_LOGIN` | must be **unset** | setting it in production is a config error the app refuses to start with |
| `ADAMARKER_BLOB_DIR` | recommended | default `./data/blobs`, resolved relative to `WorkingDirectory` |
| `ADAMARKER_SECRET_KEY_FILE` | recommended | default `./data/secret.key`; auto-generated on first boot, back it up (section 4) |
| `ADAMARKER_SHUTDOWN_DRAIN` | optional | default `5m30s`; if you raise it, also raise `TimeoutStopSec` in `adamarker.service` to stay above it |
| `OPENROUTER_API_KEY` / `QWEN_API_KEY` / `DEEPSEEK_API_KEY` | optional | one-time import seeds for the Providers page; safe to remove after first boot |
| `ADAMARKER_ALLOW_MULTIPLE_WORKERS` | must stay **unset** on a normal deploy | escape hatch for deliberate multi-instance experiments only (D26) |
| `ADAMARKER_EMAIL_PROVIDER` | yes for email login | `file`\|`smtp`\|`postmark`\|`none`. Default `none` in production (records but sends nothing, with a loud startup warning), `file` in development. Set `smtp` or `postmark` for production email-login links and student result emails. |
| `ADAMARKER_EMAIL_FROM` | yes if provider is `smtp`/`postmark` | sender address; the app refuses to boot in production with `smtp`/`postmark` and no FROM |
| `ADAMARKER_EMAIL_REPLY_DOMAIN` | recommended for regrades | inbound domain for `regrade+<token>@…` Reply-To. Unset ⇒ Reply-To is omitted and emails say replies aren't monitored. **Only `postmark` can parse inbound replies** — setting this with `smtp` promises a regrade channel that can never receive (see the boot warning). |
| `ADAMARKER_SMTP_HOST` / `_PORT` / `_USER` / `_PASS` | yes if provider is `smtp` | STARTTLS on 587 (default) or implicit TLS on 465; HOST required, PORT defaults `587` |
| `ADAMARKER_POSTMARK_TOKEN` | yes if provider is `postmark` | Postmark server API token |
| `ADAMARKER_EMAIL_RATE` | optional | outbound sends/sec, default `1.0`; must be positive |
| `ADAMARKER_REGRADE_WINDOW` | optional | signed regrade-token validity, default `336h` (14 days); must be positive |
| `ADAMARKER_REGRADE_MAX` | optional | regrade v2 turn budget, default `3`; must be positive. Turns `1..MAX` are system-adjudicated (per-problem `<pN>` requests with TA-clicked, gated result emails); turn `MAX+1` hands the thread off person-to-person to each contested problem's assigned TA (D57). Common values: 2, 3, 5. Read at receipt time — in-flight tokens carry their own turn, so changing this mid-term is safe. |
| `ADAMARKER_INBOUND_WEBHOOK_SECRET` | yes to accept inbound replies | path secret for `POST /webhooks/email/inbound/{secret}`; unset ⇒ the route 404s unconditionally (no inbound processing) |
| `ADAMARKER_APP_BASE_URL` | yes for normal email login | absolute origin (e.g. `https://ada.csie.ntu.edu.tw`, trailing slash trimmed) used for production sign-in links and the "open in app" deep link in the regrade v2 final-turn TA-notify email. |
| `ADAMARKER_EMAIL_LOGIN_TRUST_REQUEST_HOST` | optional testing escape hatch | when set, production email-login links may be built from the incoming request `Host` / `X-Forwarded-*` headers instead of `ADAMARKER_APP_BASE_URL`; useful only for temporary HTTPS tunnels with rotating hostnames. |
| `ADAMARKER_MONTHLY_BUDGET_USD` | optional | global monthly spend cap; unset ⇒ no cap. Run creation 409s when month-to-date + the new run's estimate would exceed it (D36); also gates **both** the AI re-grade batch button and the single-request AI re-grade (D52; single-request gate added in the N3 fix wave). Decimal string. |
| `ADAMARKER_REPORT_FONT` | optional | path to a UTF-8 TTF (Noto Sans TC) embedding CJK glyphs in the per-student result PDF (D42/D43). Fetch it with `make report-fonts` into `data/fonts/` (downloaded like OCR models, never committed). **Unset ⇒ the report/attachment feature is off entirely** — the publish dialog disables the attachment options with a hint; publish without attachments still works either way. |
| `ADAMARKER_OCR_MODEL` / `ADAMARKER_OCR_KEYS` / `ADAMARKER_ONNXRUNTIME` | optional, recommended | local OCR identification rung (D24) — all three or none (a partial set logs a warning and stays disabled). `make ocr-models` downloads the PP-OCRv5 server rec model (~85MB) + `ppocrv5_dict.txt` into `./data/ocr/`; libonnxruntime **>= 1.27** must be installed separately (the Go binding needs C API v26). Unset ⇒ a loud startup WARN and an Identify-tab banner: scan identification then depends on the per-upload **opt-in** cloud step (ID crops leave the host) or is fully manual in the orphan queue. |

`ADAMARKER_ENV=production` requires a database URL and at least one production
auth path: either Google OAuth or email magic-link login. If Google OAuth is
configured, `ADAMARKER_OAUTH_REDIRECT_URL` must be non-`localhost`. If email
login is the auth path, `ADAMARKER_EMAIL_PROVIDER` must not be `none` and
`ADAMARKER_APP_BASE_URL` must be set so one-time links point at the public HTTPS
origin. For temporary tunnel testing only,
`ADAMARKER_EMAIL_LOGIN_TRUST_REQUEST_HOST=1` can be used instead so the current
request hostname is used for sign-in links. Production always rejects
`ADAMARKER_DEV_LOGIN` — the app fails fast at startup rather than booting insecurely
(`internal/config/config.go`, `validate`).

### Shutdown drain vs systemd timeout

On `SIGTERM` the app stops fetching new River jobs but lets in-flight ones
finish for up to `ADAMARKER_SHUTDOWN_DRAIN` (default `5m30s` — just above the
5-minute longest job timeout) before it exits on its own
(`internal/config/config.go:81-88`). `deploy/adamarker.service` sets
`TimeoutStopSec=6m`, which must stay **greater than** whatever
`ADAMARKER_SHUTDOWN_DRAIN` resolves to, or systemd sends `SIGKILL` mid-drain
and a mid-flight grading/identify call is lost instead of finishing cleanly or
being snoozed for the next run. If you override `ADAMARKER_SHUTDOWN_DRAIN`,
raise `TimeoutStopSec` to match.

## 4. Backup design

`deploy/backup.sh` runs nightly via `adamarker-backup.timer`
(`OnCalendar=*-*-* 02:15:00`, ±5 min jitter). Each run, in order:

1. **Blobs first** — tars `ADAMARKER_BLOB_DIR` (default `./data/blobs`) to
   `backups/blobs-<timestamp>.tgz`.
2. **Secret key** — tars `ADAMARKER_SECRET_KEY_FILE` (default
   `./data/secret.key`) to `backups/secret-<timestamp>.tgz`, if present (D16 —
   losing it loses stored provider API keys, not data).
3. **Then the DB dump** — `pg_dump` to `backups/db-<timestamp>.sql`.
4. **Optional off-host copy** — if `BACKUP_RSYNC_TARGET` is set, `rsync`s the
   three files above there.
5. **Retention prune** — deletes backup sets older than the newest
   `BACKUP_KEEP` (default 14 nightly runs ≈ 2 weeks); set `BACKUP_KEEP=0` to
   disable pruning.
6. **Success stamp** — writes the current UTC timestamp to
   `backups/last-backup-ok` only after every prior step succeeds.

This order (**blobs, then DB**) is deliberate — see
[`docs/DECISIONS.md` D15](DECISIONS.md) and
[`docs/PLAN_GAPS.md` B-C1](PLAN_GAPS.md): a DB dump taken after the blob
tarball can only reference blobs that are already safely archived, never the
other way around. The failure mode this avoids is a restored DB row pointing
at a blob file the backup never captured.

Task U3 adds a `backups/last-backup-ok`-aware `GET /api/ops/status` endpoint
and an `adamarker -verify-blobs` CLI check; once landed, wire your monitoring
to poll `/api/ops/status` (or cron a check of `last-backup-ok`'s mtime) so a
silently-failing nightly backup is caught within a day, not discovered at
restore time.

To run a backup by hand:

```bash
sudo -u adamarker /opt/adamarker/deploy/backup.sh
```

Environment variables `backup.sh` reads (all optional, see the header comment
in the script for the full list): `ADAMARKER_BLOB_DIR`,
`ADAMARKER_SECRET_KEY_FILE`, `BACKUP_DIR` (default `./backups`),
`BACKUP_KEEP` (default `14`), `BACKUP_RSYNC_TARGET`, and standard `PG*`
variables for `pg_dump` (`PGHOST`, `PGPORT`, `PGUSER`, `PGDATABASE`,
`PGPASSWORD`/`PGPASSFILE`). Set overrides for the timer's run in
`/etc/adamarker/backup.env` (referenced by `adamarker-backup.service` as an
optional `EnvironmentFile`).

### RPO statement

Backups run **nightly**. Recovery Point Objective is therefore **up to 24
hours of data loss** in the worst case (a failure immediately before the next
scheduled backup). If that RPO is too coarse for a particular grading period
(e.g. the day of a major exam upload), run `deploy/backup.sh` manually before
and after the risky window to shrink the exposure.

## 5. Restore procedure

Restoring replaces the running app's blob store and database with the
contents of one backup set (`blobs-<ts>.tgz` + `db-<ts>.sql`, and optionally
`secret-<ts>.tgz`). Run this on the target VM as a user with `sudo` and access
to the `adamarker` system user. **Prerequisite:** the `adamarker` Postgres role
must exist with CREATEDB privilege (see step 1 of the install section above).

```bash
# 0. Pick the backup set to restore. List what's available:
ls -1 /opt/adamarker/backups/db-*.sql
TS=20260115-021500   # <-- set this to the timestamp suffix of the chosen set

# 1. Stop the app so nothing writes to blobs or the DB mid-restore.
sudo systemctl stop adamarker.service

# 2. Restore the blobs tarball. This REPLACES the current blob dir contents —
#    move the old one aside first rather than deleting it, in case the
#    restore needs to be aborted.
sudo -u adamarker mv /opt/adamarker/data/blobs /opt/adamarker/data/blobs.pre-restore
sudo -u adamarker mkdir -p /opt/adamarker/data
sudo -u adamarker tar -xzf /opt/adamarker/backups/blobs-"$TS".tgz -C /opt/adamarker/data

# 2b. If restoring the secret key too (only needed if data/secret.key was
#     also lost — restoring it when the current key is still intact and
#     already encrypting live provider credentials will make those
#     credentials unreadable, so skip this step unless the current key file
#     is gone or known-bad):
sudo -u adamarker tar -xzf /opt/adamarker/backups/secret-"$TS".tgz -C /opt/adamarker/data

# 3. Restore the database. This example drops and recreates the target
#    database first so the restore starts from empty. Read the connection
#    details out of adamarker.env's ADAMARKER_DATABASE_URL rather than
#    guessing — production Postgres is not necessarily on the dev-compose
#    port (5433) or even this host.
#    Example, if ADAMARKER_DATABASE_URL is
#    postgres://adamarker:adamarker@db.internal:5432/adamarker?sslmode=require :
export PGHOST=db.internal PGPORT=5432 PGUSER=adamarker PGDATABASE=adamarker
sudo -u adamarker dropdb -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" "$PGDATABASE"
sudo -u adamarker createdb -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" "$PGDATABASE"
sudo -u adamarker psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" \
  < /opt/adamarker/backups/db-"$TS".sql

# 4. Start the app back up. It re-applies goose + River migrations in-process
#    on boot (idempotent against an already-migrated schema from the dump).
sudo systemctl start adamarker.service
sudo systemctl status adamarker.service

# 5. Verify blob/DB referential integrity (Task U3's ref-integrity check —
#    confirms every image_ref/source_pdf_ref/masked_image_ref row in the
#    restored DB has a matching file in the restored blob dir, and flags
#    orphaned blob files with no owning row).
sudo -u adamarker /opt/adamarker/bin/adamarker -verify-blobs

# 6. Once verified, remove the pre-restore blob dir backup from step 2.
sudo -u adamarker rm -rf /opt/adamarker/data/blobs.pre-restore
```

If step 5 reports missing blobs, the DB dump was taken after the blob tarball
in a way that outpaced it (should not happen given the blobs-then-DB backup
order, but always re-verify after a restore rather than assuming). Fall back
to an older backup set, or restore blobs from a *later* tarball than the DB
dump if one exists — never the reverse.

### Post-restore checks

- `sudo systemctl status adamarker.service` — active, no crash loop.
- `curl -s http://127.0.0.1:8080/ -o /dev/null -w '%{http_code}\n'` — `200`.
- `adamarker -verify-blobs` (step 5 above) — no orphaned refs.
- Once Task U3 lands, `curl -s http://127.0.0.1:8080/api/ops/status` should
  also report the restored backup's freshness and a clean verify-blobs run.
- Spot-check the app UI: log in, open a recently-graded assessment, confirm a
  page image renders (exercises the blob store) and a grading record shows
  (exercises the DB).

## 6. Logs and troubleshooting

```bash
# App logs (structured, stdout -> journald)
sudo journalctl -u adamarker.service -f

# Nightly backup logs
sudo journalctl -u adamarker-backup.service -n 200

# Confirm the last successful backup's age
date -u -d "$(cat /opt/adamarker/backups/last-backup-ok)" +%s
```

Per [`CLAUDE.md`](../CLAUDE.md), app logs must never contain student PII
(names, IDs, emails, answer content, transcriptions) — if you see any while
debugging, that's a bug to file, not something to paste into a ticket or this
document.

## 7. Email and regrade operations

- **Enqueue failure at publish time (I3):** publishing commits the batch, then
  enqueues one send job per pending item. If the enqueue call fails (e.g. the
  River/queue backend is briefly unavailable), the affected items are marked
  `email_status=failed` with the error text instead of being left `pending`. Use
  the **Resend failed** action on the batch (Publish tab) to re-enqueue them once
  the queue is healthy — resend-failed only recovers items in the `failed` state.
- **`smtp` + `ADAMARKER_EMAIL_REPLY_DOMAIN` (I4):** only the `postmark` provider
  parses inbound replies. If you run `smtp` with a reply domain set, outbound
  emails advertise a `regrade+<token>@…` Reply-To that no webhook can ever
  receive, so students' regrade replies are silently lost. The app logs a loud
  startup warning in this configuration but does not refuse to boot — either
  switch to `postmark` for the regrade channel, or unset the reply domain so
  emails correctly say replies are not monitored.
- **Result-PDF/ZIP email attachments (D42–D45):** the publish dialog offers three
  attachment options — `none` (default, today's text-only email), `compressed`
  (page images downscaled to long edge 1600px, JPEG q75 — recommended), and
  `original` (page images at ingest render resolution) — plus a ZIP checkbox
  that swaps the merged per-student PDF for a ZIP of per-problem JPEGs (useful
  when a mail gateway or PDF viewer chokes on the merged file). These options
  are disabled in the UI whenever `ADAMARKER_REPORT_FONT` is unset. Attachments
  are built per item at send time from blobs (not stored), so resends rebuild
  them deterministically from the batch's recorded `attachment`/`zip` choice.
  Any single item whose built attachment exceeds 15 MB gets a non-terminal
  per-item warning in the batch view — the send still proceeds, and an SMTP
  server that rejects an oversized message will show its own failed status.
