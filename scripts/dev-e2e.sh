#!/bin/sh
# Runs the built binary against the compose dev DB with dev login for local smoke
# testing (used by .claude/launch.json). Sources .env for provider keys.
set -e
cd "$(dirname "$0")/.."
set -a; [ -f .env ] && . ./.env; set +a
# Bring up the compose dev DB like `make dev` does, so a cold launch doesn't
# hang on a missing Postgres. Warning-only: an external ADAMARKER_DATABASE_URL
# (or no Docker) must not block the launch.
docker compose up -d --wait db || echo "dev-e2e: warning: could not start the compose db — assuming ADAMARKER_DATABASE_URL points at a reachable Postgres." >&2
export ADAMARKER_DATABASE_URL="${ADAMARKER_DATABASE_URL:-postgres://adamarker:adamarker@localhost:5433/adamarker?sslmode=disable}"
export ADAMARKER_DEV_LOGIN=1
export ADAMARKER_BOOTSTRAP_ADMIN_EMAIL="${ADAMARKER_BOOTSTRAP_ADMIN_EMAIL:-b11902156@ntu.edu.tw}"
export ADAMARKER_HTTP_ADDR=:8899
# Same blob dir as `make dev`/`make run` — a divergent dir here once made every
# stored image 404 for anyone running the app with defaults.
export ADAMARKER_BLOB_DIR=./data/blobs
# Regrade dev loop (demo-polish plan 2026-07-10, Task SEED): a webhook path
# secret so POST /webhooks/email/inbound/{secret} exists, and a reply domain so
# publish mints regrade+<token>@… Reply-To headers into the outbox .eml files
# (scripts/seed-demo-walkthrough.py extracts those tokens to file demo regrade
# threads). Dev-only defaults, applied only when unset; the file provider never
# sends anything off-disk.
export ADAMARKER_INBOUND_WEBHOOK_SECRET="${ADAMARKER_INBOUND_WEBHOOK_SECRET:-dev-webhook-secret}"
export ADAMARKER_EMAIL_REPLY_DOMAIN="${ADAMARKER_EMAIL_REPLY_DOMAIN:-regrades.dev.local}"
# Rebuild before exec so this never smoke-tests a stale binary (audit 2026-07-16
# B16 — the binary in bin/ once predated the commit under test). go build is
# incremental; the SPA bundle is NOT rebuilt here — run `make frontend` when
# frontend changes matter, else the embedded (possibly stale) dist is served.
if [ ! -d internal/web/assets/dist ]; then
  echo "dev-e2e: warning: internal/web/assets/dist missing — the embedded SPA placeholder will be served; run 'make frontend' first." >&2
fi
go build -o bin/adamarker ./cmd/adamarker
echo "dev-e2e: http://localhost:8899 — dev login email: $ADAMARKER_BOOTSTRAP_ADMIN_EMAIL (POST /auth/dev-login, header 'X-ADA-CSRF: 1', JSON {\"email\":...}; every non-GET API call needs that header)" >&2
exec ./bin/adamarker
