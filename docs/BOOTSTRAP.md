# BOOTSTRAP.md — zero-to-running on a brand-new machine (agent playbook)

Goal: fresh laptop → `make doctor` says ready. Follow your OS section top to bottom; every step is a runnable command. Steps tagged **HUMAN** need an admin password or a GUI click — hand those to the human, run everything else yourself. After any fix, re-run `make doctor`; it tells you what is still missing.

## macOS

1. **HUMAN** — install Homebrew (asks for the account password; also installs the Xcode Command Line Tools = git + C compiler): `/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`
2. Put brew on PATH for this shell (Apple Silicon shown; Intel uses `/usr/local/bin/brew`): `eval "$(/opt/homebrew/bin/brew shellenv)"`
3. Install the toolchain: `brew install go node@20` (Go must end up 1.26+; `brew install go` tracks latest).
4. Database — pick ONE:
   - Local Postgres, fully agent-runnable (recommended for agents): `brew install postgresql@16 && brew services start postgresql@16`, then the "Create the database" block below with `PGBIN=/opt/homebrew/opt/postgresql@16/bin`.
   - Docker Desktop, **HUMAN** (installer needs password, first launch needs GUI): `brew install --cask docker`, open `/Applications/Docker.app` once, wait for the whale icon; then skip "Create the database" — compose handles it.
5. Clone and enter the repo: `git clone https://github.com/HaoWen46/adagrade.git && cd adagrade`
6. Verify and start: `make doctor`, fix anything it flags, then `make frontend` once and `./scripts/dev-e2e.sh` → http://localhost:8899 (it prints the login email + curl at startup).

## Ubuntu / Debian (incl. WSL2 and root containers/sandboxes)

Running as root with no `sudo` binary (most agent sandboxes and containers): drop every `sudo`/`sudo -E` prefix below, and use `su postgres -c '<command>'` wherever a step says `sudo -u postgres <command>`. This whole section is then agent-runnable with no HUMAN steps (verified end-to-end on a blank ubuntu:24.04 container, no Docker inside).

1. **HUMAN if you lack sudo** — base tools: `sudo apt-get update && sudo apt-get install -y build-essential git curl`
2. Go 1.26+ (apt's golang is too old — use the official tarball; pick the right arch from https://go.dev/dl/, e.g. `linux-arm64` on ARM): `mkdir -p "$HOME/.local" && curl -fL https://go.dev/dl/go1.26.4.linux-amd64.tar.gz | tar -C "$HOME/.local" -xz && export PATH="$HOME/.local/go/bin:$PATH"` (persist the PATH line into `~/.profile`).
3. Node 20+ for the SPA build: `curl -fsSL https://deb.nodesource.com/setup_20.x | sudo -E bash - && sudo apt-get install -y nodejs` (**HUMAN if you lack sudo**).
4. Database — pick ONE:
   - Local Postgres (no Docker; the right choice in any sandbox without a Docker daemon): `sudo apt-get install -y postgresql` (16+ on current releases), then START it — desktop installs auto-start via systemd, but containers and WSL do not: `sudo service postgresql start` (re-run after any container restart; `service postgresql status` must say online). Then do the "Create the database" block below with `SUDO_PG="sudo -u postgres"`.
   - Docker: `sudo apt-get install -y docker.io && sudo usermod -aG docker "$USER"` then re-login (**HUMAN**); compose then handles the DB.
5. Clone, verify, start — same as macOS steps 5–6.

## Windows

Use WSL2 with Ubuntu (`wsl --install`, **HUMAN**), then follow the Ubuntu section inside WSL.

## Create the database (only for the no-Docker path)

Mirrors the compose defaults except the port (5432 instead of 5433). `PGBIN`/`SUDO_PG` come from your OS step above; on Ubuntu prefix the two commands with `$SUDO_PG` instead of using `$PGBIN/`.

```bash
"$PGBIN/psql" -d postgres -c "CREATE ROLE adamarker LOGIN PASSWORD 'adamarker' CREATEDB;"
"$PGBIN/createdb" -O adamarker adamarker
```

Root container/sandbox variant (no sudo) of the same two commands:

```bash
su postgres -c "psql -d postgres -c \"CREATE ROLE adamarker LOGIN PASSWORD 'adamarker' CREATEDB;\""
su postgres -c "createdb -O adamarker adamarker"
```

Then point the app at it (from the repo root):

```bash
echo 'ADAMARKER_DATABASE_URL=postgres://adamarker:adamarker@localhost:5432/adamarker?sslmode=disable' >> .env
```

Migrations run automatically at server startup — never create tables by hand.

## After bootstrap

- `make doctor` must print `doctor: ready` — if not, apply its fix lines; each names the exact command.
- Start: `./scripts/dev-e2e.sh` → http://localhost:8899; log in with the email it prints (POST `/auth/dev-login`, header `X-ADA-CSRF: 1`).
- Day-to-day workflow, tests, and API conventions: [`../AGENTS.md`](../AGENTS.md).
