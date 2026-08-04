#!/bin/sh
# make doctor — preflight for running AdaGrade locally. Checks every prerequisite
# `make dev` / scripts/dev-e2e.sh needs and prints the exact fix for anything
# missing. Exit 0 = the server can start; warnings are non-fatal (degraded UI or
# first-login caveats). Safe to run repeatedly; changes nothing.
cd "$(dirname "$0")/.."
set -a; [ -f .env ] && . ./.env; set +a

failed=0
ok()   { printf 'ok    %s\n' "$1"; }
warn() { printf 'warn  %s\n' "$1"; }
bad()  { printf 'FAIL  %s\n      fix: %s\n' "$1" "$2"; failed=1; }

# Go >= 1.26
if ! command -v go >/dev/null 2>&1; then
  bad "go not found" "macOS: brew install go · Linux: official tarball (apt golang is too old) — full from-zero commands in docs/BOOTSTRAP.md"
else
  gover=$(go version | sed -n 's/.*go\([0-9]*\)\.\([0-9]*\).*/\1 \2/p')
  gomaj=${gover%% *}; gomin=${gover##* }
  if [ "${gomaj:-0}" -gt 1 ] || { [ "${gomaj:-0}" -eq 1 ] && [ "${gomin:-0}" -ge 26 ]; }; then
    ok "go $(go version | awk '{print $3}')"
  else
    bad "go too old ($(go version | awk '{print $3}'); need 1.26+)" "install Go 1.26+ from https://go.dev/dl/"
  fi
fi

# C compiler (cgo)
if command -v cc >/dev/null 2>&1 || command -v gcc >/dev/null 2>&1 || command -v clang >/dev/null 2>&1; then
  ok "C compiler present (cgo)"
else
  bad "no C compiler (cc/gcc/clang)" "macOS: installing Homebrew brings it (docs/BOOTSTRAP.md step 1) · Debian/Ubuntu: sudo apt-get install -y build-essential"
fi

# Postgres: external ADAMARKER_DATABASE_URL, or Docker for the compose DB
if [ -n "${ADAMARKER_DATABASE_URL:-}" ]; then
  ok "ADAMARKER_DATABASE_URL set — using that Postgres; Docker not required"
elif ! command -v docker >/dev/null 2>&1; then
  bad "docker not found and no ADAMARKER_DATABASE_URL" "no Docker needed: install local Postgres and set ADAMARKER_DATABASE_URL in .env — exact commands in docs/BOOTSTRAP.md ('Create the database')"
else
  dockererr=$(docker info --format ok 2>&1)
  case "$dockererr" in
    ok*) ok "docker daemon running (compose Postgres available)" ;;
    *paused*) bad "Docker Desktop is paused" "run: docker desktop start   (or: docker desktop restart)" ;;
    *) bad "docker daemon not reachable" "start Docker (open Docker Desktop, or systemctl start docker), OR set ADAMARKER_DATABASE_URL to an external Postgres" ;;
  esac
fi

# Built SPA (optional — placeholder page without it)
if [ -d internal/web/assets/dist ]; then
  ok "frontend built (internal/web/assets/dist present)"
elif command -v npm >/dev/null 2>&1; then
  warn "frontend not built — the server will serve a placeholder page; run: make frontend"
else
  warn "frontend not built and npm missing — placeholder page only; install Node 20+ then run: make frontend (the API still works without it)"
fi

# First-login email (only matters on an empty database)
if [ -n "${ADAMARKER_BOOTSTRAP_ADMIN_EMAIL:-}" ]; then
  ok "bootstrap admin email set (${ADAMARKER_BOOTSTRAP_ADMIN_EMAIL})"
else
  warn "ADAMARKER_BOOTSTRAP_ADMIN_EMAIL not set — scripts/dev-e2e.sh defaults one (printed at startup); for 'make dev' on an empty DB, put ADAMARKER_BOOTSTRAP_ADMIN_EMAIL=you@example.com in .env or the first login will 403"
fi

echo
if [ "$failed" -eq 0 ]; then
  echo "doctor: ready. Start the server with ./scripts/dev-e2e.sh (http://localhost:8899) or make dev (http://localhost:8080)."
else
  echo "doctor: NOT ready — apply the fix lines above, then re-run: make doctor"
fi
exit "$failed"
