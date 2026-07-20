# TA Deployment Guide

This guide is for future TAs who need to bring up ADA-Marker on a new host
without inheriting any current machine state, tunnel, cache directory, or
absolute path. The older [operations runbook](OPERATIONS.md) is still useful for
a dedicated root-managed VM. This file focuses on the portable path: shared
workstations, user-owned processes, and Cloudflare Tunnel.

ADA-Marker is not a static site. It is:

- one Go HTTP server with the React/Vite frontend embedded into the binary;
- Postgres 16 for all relational state and River jobs;
- a local blob directory for PDFs, rendered pages, masked images, and generated
  artifacts;
- outbound email for admin/TA magic-link login and student result email.

## 0. First safety checks

Do these before exposing the site to the Internet:

1. Bind ADA-Marker to localhost only: `ADAMARKER_HTTP_ADDR=127.0.0.1:8080`.
2. Use `ADAMARKER_ENV=production`; never expose `make dev` or
   `ADAMARKER_DEV_LOGIN=1`.
3. Put database files, Docker data, build caches, blobs, backups, and temporary
   files under a private state directory with enough quota. Do not depend on the
   repo checkout or a small home quota for large state.
4. Use real email delivery for a public instance. The `file` email provider is
   only for local smoke tests because it writes `.eml` files on disk instead of
   emailing users.
5. Set `ADAMARKER_APP_BASE_URL` to the final public HTTPS origin before sending
   login links. Magic-link login tokens are single-use and expire after 15
   minutes, but stale links are still confusing during setup.
6. Back up blobs, `secret.key`, and the database before changing hosts or
   upgrading code. Restore requires all three to match.

## 1. Choose paths

Pick paths per host. These examples use variables on purpose; replace them for
the actual machine.

```bash
# Run this from the cloned repo.
export ADA_REPO="$PWD"

# Choose a private directory with enough quota and persistence.
# On a normal VM, $HOME/.local/state/adamarker is fine.
# On a quota-limited workstation, choose the local large-disk area assigned
# by that site, for example /tmp2/$USER/adamarker if that is the local policy.
export ADA_STATE="${ADA_STATE:-$HOME/.local/state/adamarker}"

export ADA_CACHE="$ADA_STATE/cache"
export ADA_ENV="$ADA_STATE/adamarker.env"
export ADA_PORT="${ADA_PORT:-8080}"
export ADA_HTTP_ADDR="127.0.0.1:$ADA_PORT"

# Fill these in for the deployment.
export ADA_PUBLIC_ORIGIN="https://<public-hostname>"
export ADA_ADMIN_EMAIL="<admin>@ntu.edu.tw"
```

Create the directories:

```bash
install -d -m 700 "$ADA_STATE" "$ADA_STATE/blobs" "$ADA_STATE/backups" "$ADA_STATE/home"
install -d -m 700 "$ADA_CACHE" "$ADA_CACHE/go-build" "$ADA_CACHE/go-mod"
install -d -m 700 "$ADA_CACHE/go-tmp" "$ADA_CACHE/npm" "$ADA_CACHE/tmp" "$ADA_CACHE/xdg"
```

Keep `$ADA_ENV` private. It contains SMTP credentials and possibly provider
secrets.

```bash
install -m 600 /dev/null "$ADA_ENV"
```

## 2. Prerequisites

Install or make available:

- Go 1.26 or newer;
- Node.js 20 or newer with npm;
- a C compiler, because the binary links cgo packages;
- Postgres 16, either native or via Docker Compose;
- `cloudflared` if this host will be published through Cloudflare Tunnel.

Docker is optional if a real Postgres service is already available. If Docker is
used on a shared workstation, prefer rootless Docker and put Docker's data root
under `$ADA_STATE`. Docker's official rootless docs describe the non-root daemon
setup; Docker's daemon docs describe `data-root` and note that rootless Linux
uses `~/.config/docker/daemon.json`.

Example rootless Docker storage configuration:

```bash
install -d -m 700 "$ADA_STATE/docker/data" "$HOME/.config/docker"
printf '{\n  "data-root": "%s"\n}\n' "$ADA_STATE/docker/data" > "$HOME/.config/docker/daemon.json"
systemctl --user restart docker.service
# If the rootless installer told you to set DOCKER_HOST, export it in this shell.
# This is the common rootless socket location on Linux.
export DOCKER_HOST="unix:///run/user/$(id -u)/docker.sock"
docker info --format '{{.DockerRootDir}}'
```

If `docker info` does not point at the chosen state directory, fix Docker before
starting Postgres. The compose volume for the dev database lives under Docker's
data root.

## 3. Build with cache outside the repo

Build the frontend first so the Go binary embeds the real SPA instead of the
placeholder page.

```bash
cd "$ADA_REPO"

env \
  npm_config_cache="$ADA_CACHE/npm" \
  XDG_CACHE_HOME="$ADA_CACHE/xdg" \
  TMPDIR="$ADA_CACHE/tmp" \
  make frontend

env \
  GOCACHE="$ADA_CACHE/go-build" \
  GOMODCACHE="$ADA_CACHE/go-mod" \
  GOTMPDIR="$ADA_CACHE/go-tmp" \
  TMPDIR="$ADA_CACHE/tmp" \
  make build
```

Optional assets:

```bash
# Enables PDF/ZIP result attachments when ADAMARKER_REPORT_FONT is set.
env npm_config_cache="$ADA_CACHE/npm" TMPDIR="$ADA_CACHE/tmp" make report-fonts

# Local OCR (recommended): downloads the PP-OCRv4 model + keys dict into
# ./data/ocr/. It does NOT install libonnxruntime — install that separately
# (>= 1.27; Linux: a GitHub release tarball, macOS: brew install onnxruntime).
# Local OCR activates only when ADAMARKER_OCR_MODEL, ADAMARKER_OCR_KEYS, and
# ADAMARKER_ONNXRUNTIME are all set (section 5). Without it the server logs a
# startup WARN and the Identify tab shows a banner: scan identification then
# depends on the per-upload opt-in cloud step (ID crops leave this machine)
# or is fully manual.
env TMPDIR="$ADA_CACHE/tmp" make ocr-models
```

## 4. Start Postgres

Option A: existing Postgres

Create a database and user with a strong password, then set:

```bash
export ADA_DATABASE_URL='postgres://<user>:<password>@<host>:5432/<db>?sslmode=<mode>'
```

Option B: Docker Compose Postgres

Use the repo's `docker-compose.yml` database. It listens on localhost port
`5433`, and its named volume is stored wherever Docker stores volumes.

```bash
cd "$ADA_REPO"
docker compose up -d --wait db
export ADA_DATABASE_URL='postgres://adamarker:adamarker@127.0.0.1:5433/adamarker?sslmode=disable'
```

For a long-lived server, set a Docker restart policy after the first successful
start:

```bash
docker update --restart unless-stopped "$(docker compose ps -q db)"
```

## 5. Write production env

Fill `$ADA_ENV` with real values for this host. Do not commit this file.

```bash
cat > "$ADA_ENV" <<EOF
ADAMARKER_ENV=production
ADAMARKER_HTTP_ADDR=$ADA_HTTP_ADDR
ADAMARKER_DATABASE_URL=$ADA_DATABASE_URL
ADAMARKER_BLOB_DIR=$ADA_STATE/blobs
ADAMARKER_SECRET_KEY_FILE=$ADA_STATE/secret.key
ADAMARKER_BOOTSTRAP_ADMIN_EMAIL=$ADA_ADMIN_EMAIL
ADAMARKER_HOSTED_DOMAIN=ntu.edu.tw

# Public origin used inside login and regrade links. No trailing slash needed.
ADAMARKER_APP_BASE_URL=$ADA_PUBLIC_ORIGIN

# Email magic-link login and result email. Use either smtp or postmark.
ADAMARKER_EMAIL_PROVIDER=smtp
ADAMARKER_EMAIL_FROM=<sender>@ntu.edu.tw
ADAMARKER_SMTP_HOST=<smtp-host>
ADAMARKER_SMTP_PORT=465
ADAMARKER_SMTP_USER=<smtp-user>
ADAMARKER_SMTP_PASS=<smtp-password>

# Optional local OCR (recommended — keeps scan identification on this machine).
# Run make ocr-models first (section 3) and install libonnxruntime >= 1.27.
# ADAMARKER_OCR_MODEL=$ADA_REPO/data/ocr/ch_PP-OCRv4_rec_infer.onnx
# ADAMARKER_OCR_KEYS=$ADA_REPO/data/ocr/ppocr_keys_v1.txt
# ADAMARKER_ONNXRUNTIME=/opt/onnxruntime/lib/libonnxruntime.so

# Optional, but recommended before running real grading jobs.
# ADAMARKER_MONTHLY_BUDGET_USD=150.00

# Optional report attachments, if make report-fonts was run.
# ADAMARKER_REPORT_FONT=$ADA_REPO/data/fonts/NotoSansTC-Regular.ttf
EOF
chmod 600 "$ADA_ENV"
```

For NTU SMTP, the host that has worked for this project is
`smtps.ntu.edu.tw` on port `465`, with the mailbox username and password from
the sender mailbox. If a future course account uses another mail provider, use
that provider's SMTP settings instead.

For full inbound regrade replies, use `postmark` rather than `smtp`.
The SMTP provider can send mail, but it cannot parse inbound student replies.

## 6. Run the app locally

Foreground test run:

```bash
cd "$ADA_REPO"
set -a
. "$ADA_ENV"
set +a

env \
  HOME="$ADA_STATE/home" \
  XDG_CACHE_HOME="$ADA_CACHE/xdg" \
  TMPDIR="$ADA_CACHE/tmp" \
  "$ADA_REPO/bin/adamarker"
```

In another shell:

```bash
curl -fsS "http://$ADA_HTTP_ADDR/healthz"
curl -fsS "http://$ADA_HTTP_ADDR/api/auth/modes"
```

Expected production auth shape for email-only login:

```json
{"dev":false,"email":true,"google":false}
```

The first boot applies database migrations automatically and creates the
bootstrap admin if no active admin exists. After the admin can log in, add other
TAs from the Users page. Only allowlisted active users receive login links.

## 7. Keep the app running

On a root-managed VM, prefer the system service in `deploy/` and adapt
[OPERATIONS.md](OPERATIONS.md) to the host's actual paths.

On a shared workstation without root service access, use a user systemd unit if
available. This example expands the variables when creating the unit, so it does
not assume a specific machine path.

```bash
install -d -m 700 "$HOME/.config/systemd/user"
cat > "$HOME/.config/systemd/user/adamarker.service" <<EOF
[Unit]
Description=ADA-Marker user service
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$ADA_REPO
EnvironmentFile=$ADA_ENV
Environment=HOME=$ADA_STATE/home
Environment=XDG_CACHE_HOME=$ADA_CACHE/xdg
Environment=TMPDIR=$ADA_CACHE/tmp
ExecStart=$ADA_REPO/bin/adamarker
Restart=on-failure
RestartSec=5s
KillSignal=SIGTERM
TimeoutStopSec=6m

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now adamarker.service
systemctl --user status adamarker.service
```

If the host logs out user services, ask the machine owner how to enable linger
for the TA account. On many Linux hosts this is:

```bash
loginctl enable-linger "$USER"
```

## 8. Cloudflare Tunnel from scratch

Do not reuse another TA's temporary tunnel URL. Each deployment needs its own
public hostname and its own tunnel configuration.

Cloudflare has two relevant tunnel types:

- Quick Tunnel: no account required, random `trycloudflare.com` hostname,
  intended only for testing and development.
- Named Tunnel: created in a Cloudflare account, mapped to a real hostname,
  suitable for a course deployment.

### 8.1 Install cloudflared

Install using Cloudflare's package repository, the dashboard-provided command,
or a binary from Cloudflare's GitHub releases. For a user-owned Linux install,
place the binary under a private bin directory and add it to `PATH`:

```bash
export ADA_BIN="$ADA_STATE/bin"
install -d -m 700 "$ADA_BIN"

case "$(uname -m)" in
  x86_64|amd64) cf_arch=amd64 ;;
  aarch64|arm64) cf_arch=arm64 ;;
  *)
    echo "Unsupported architecture for this snippet; download manually from Cloudflare's downloads page."
    exit 1
    ;;
esac

curl -fL --retry 3 \
  -o "$ADA_BIN/cloudflared" \
  "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-$cf_arch"
chmod 700 "$ADA_BIN/cloudflared"
export PATH="$ADA_BIN:$PATH"
cloudflared --version
```

For macOS or Windows, use the OS-specific install method from Cloudflare's
downloads page.

### 8.2 Recommended: dashboard-managed named tunnel

Use this when the TA owns or has access to a Cloudflare zone for the course
hostname.

1. In the Cloudflare dashboard, go to `Networking > Tunnels`.
2. Create a tunnel with a name like `ada-marker-<term>`.
3. Choose the server OS and architecture, then run the install/run command shown
   by Cloudflare on the server.
4. Add a published application route:
   - Hostname: `<public-hostname>`.
   - Service URL: `http://localhost:<ADA_PORT>` or
     `http://127.0.0.1:<ADA_PORT>`.
5. Wait until the dashboard shows the connector as healthy.
6. Set `ADAMARKER_APP_BASE_URL=https://<public-hostname>` in `$ADA_ENV`.
7. Restart ADA-Marker so new login links use the final public origin.

Cloudflare's dashboard flow can also install `cloudflared` as a service with a
tunnel token. If using a root-managed service, keep the token out of the repo
and out of shared chat logs.

### 8.3 CLI-managed named tunnel

Use this when you prefer local `cloudflared` configuration files.

```bash
cloudflared tunnel login
cloudflared tunnel create <tunnel-name>
```

Create the config:

```bash
install -d -m 700 "$HOME/.cloudflared"
cat > "$HOME/.cloudflared/config.yml" <<EOF
tunnel: <tunnel-uuid>
credentials-file: $HOME/.cloudflared/<tunnel-uuid>.json

ingress:
  - hostname: <public-hostname>
    service: http://127.0.0.1:$ADA_PORT
  - service: http_status:404
EOF
chmod 600 "$HOME/.cloudflared/config.yml" "$HOME/.cloudflared/<tunnel-uuid>.json"
```

Create the DNS route and run the tunnel:

```bash
cloudflared tunnel route dns <tunnel-name-or-uuid> <public-hostname>
cloudflared tunnel --config "$HOME/.cloudflared/config.yml" run <tunnel-name-or-uuid>
```

For a root-managed Linux service, Cloudflare documents:

```bash
sudo cloudflared --config "$HOME/.cloudflared/config.yml" service install
sudo systemctl start cloudflared
sudo systemctl status cloudflared
```

For a user-owned tunnel service, create a user systemd unit:

```bash
cat > "$HOME/.config/systemd/user/cloudflared-adamarker.service" <<EOF
[Unit]
Description=Cloudflare Tunnel for ADA-Marker
After=network-online.target adamarker.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=$ADA_BIN/cloudflared tunnel --config $HOME/.cloudflared/config.yml run <tunnel-name-or-uuid>
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now cloudflared-adamarker.service
```

### 8.4 Quick tunnel for smoke testing only

Quick tunnels are useful before a real hostname exists:

```bash
cloudflared tunnel --url "http://127.0.0.1:$ADA_PORT"
```

Cloudflare prints a random public URL. If the URL rotates during testing, set
`ADAMARKER_EMAIL_LOGIN_TRUST_REQUEST_HOST=1` and leave
`ADAMARKER_APP_BASE_URL` unset so magic-link emails use the hostname from the
current request. Do not use a quick tunnel as the course URL; it is random,
testing-oriented, and has Cloudflare-imposed limits.

## 9. Public smoke test

After the app and tunnel are both running:

```bash
curl -fsS "$ADA_PUBLIC_ORIGIN/healthz"
curl -fsS "$ADA_PUBLIC_ORIGIN/api/auth/modes"
```

Then open `$ADA_PUBLIC_ORIGIN` in a browser and request a login link for the
bootstrap admin email. The direct API endpoint requires the SPA's CSRF header,
so the browser flow is the normal test.

Expected behavior:

- the admin receives an email with a link under `$ADA_PUBLIC_ORIGIN/login/email`;
- opening the link shows a "Complete sign-in" page without consuming the token
  (mail-scanner link prefetch cannot burn it);
- clicking "Complete sign-in" works once; the token stops working after 15
  minutes or after it has been consumed;
- `/api/me` shows the admin role after login.

## 10. Backups

For any real course data, run backups from the same env file:

```bash
cd "$ADA_REPO"
set -a
. "$ADA_ENV"
set +a

env \
  BACKUP_DIR="$ADA_STATE/backups" \
  PGHOST=<pg-host> \
  PGPORT=<pg-port> \
  PGUSER=<pg-user> \
  PGDATABASE=<pg-database> \
  PGPASSWORD=<pg-password> \
  "$ADA_REPO/deploy/backup.sh"
```

The backup script archives blobs first, then `secret.key`, then the database.
Keep at least one off-host copy. Losing `secret.key` does not erase grades, but
it does make stored provider API keys unreadable.

## 11. Upgrade checklist

Before pulling new code onto an active deployment:

1. Stop new grading/upload activity in the UI.
2. Run a backup.
3. Pull or deploy the new code.
4. Rebuild `make frontend` and `make build` with the cache env from section 3.
5. Restart ADA-Marker.
6. Check `/healthz`, `/api/auth/modes`, one admin login, and one representative
   assessment page image.
7. Keep the previous binary and latest backup until the smoke test passes.

## External references

Checked on 2026-07-05:

- Cloudflare Tunnel setup:
  https://developers.cloudflare.com/tunnel/setup/
- Cloudflare locally managed tunnel CLI:
  https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/local-management/create-local-tunnel/
- Cloudflare Linux service install:
  https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/local-management/as-a-service/linux/
- Cloudflare Quick Tunnels:
  https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/trycloudflare/
- Cloudflare `cloudflared` downloads:
  https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/downloads/
- Docker rootless mode:
  https://docs.docker.com/engine/security/rootless/
- Docker daemon `data-root`:
  https://docs.docker.com/engine/daemon/
