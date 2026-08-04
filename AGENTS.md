# AGENTS.md — working conventions for AdaGrade (ADA-Marker)

CLAUDE.md and AGENTS.md are mirrors — edit one, copy to the other (only the title line differs).

AI-assisted grading system, one Go binary + Postgres. Product plan: [`ADA-Marker_Plan.md`](ADA-Marker_Plan.md); architecture: [`docs/superpowers/specs/2026-07-01-ada-marker-architecture-design.md`](docs/superpowers/specs/2026-07-01-ada-marker-architecture-design.md); open gaps: [`docs/PLAN_GAPS.md`](docs/PLAN_GAPS.md).

## Start the server

- First command, always: `make doctor` — checks every prerequisite (Go 1.26+, C compiler, Docker or an external `ADAMARKER_DATABASE_URL`, built SPA, bootstrap admin email) and prints the exact fix for anything missing; re-run it after fixing until it says ready.
- Brand-new machine with nothing installed: follow [`docs/BOOTSTRAP.md`](docs/BOOTSTRAP.md) — per-OS copy-paste commands from zero, with the few password/GUI steps an agent must hand to the human tagged HUMAN, and a no-Docker local-Postgres path agents can run end-to-end.
- Start it: `./scripts/dev-e2e.sh` → http://localhost:8899, or `make dev` → http://localhost:8080; both start the compose Postgres themselves, rebuild, and enable dev login; dev-e2e.sh additionally defaults the bootstrap admin email and prints the exact login call at startup.
- Claude Code only: the `adamarker-e2e` config in [`.claude/launch.json`](.claude/launch.json) wraps `scripts/dev-e2e.sh`; other agents run the script directly.
- No Docker available? Set `ADAMARKER_DATABASE_URL` (in `.env`) to any reachable Postgres 16+ database and use `./scripts/dev-e2e.sh` (its compose step degrades to a warning; `make dev` still hard-requires Docker).
- If `docker` commands report "Docker Desktop is manually paused", resume it first (`docker desktop start`, or `docker desktop restart` if the pause persists) — a paused daemon makes the app hang waiting for Postgres and then exit.
- Migrations run automatically at startup; never run goose or psql schema commands by hand.
- CSRF: every non-GET API request MUST send the header `X-ADA-CSRF: 1` (any value works) or it fails 403 `missing CSRF header`; this includes `/auth/dev-login`.
- Log in: `POST /auth/dev-login` with header `X-ADA-CSRF: 1` and JSON `{"email":"<allowlisted email>"}` → 204 sets the session cookie (curl: `-c cookies.txt`, then `-b cookies.txt`; confirm with `GET /api/me`); the SPA login page exposes the same form; 403 `not authorized` means the email is not an active user in the DB.
- Very first login must use the bootstrap admin email (seeded as admin on boot): `scripts/dev-e2e.sh` defaults `ADAMARKER_BOOTSTRAP_ADMIN_EMAIL`; `make dev` reads it from `.env`.
- If the app serves a placeholder page instead of the real UI, run `make frontend` once (Vite build → embedded via go:embed), then restart the server.
- Config is env-driven from `.env` (gitignored); every variable is documented in [`.env.adamarker.example`](.env.adamarker.example); all are optional in development.
- Optional features stay off while their env vars are unset (local OCR, report PDF attachments, Typst renderer); enable only when the task needs them via `make ocr-models` / `make report-fonts` plus the env vars those targets print.

## Test / build

- `make test` — unit tests, no Postgres needed; run `make vet` too before calling work done.
- `make test-integration` — starts the compose test DB (:5434) and runs all tests with `-count=1`.
- NEVER run live tests (`-tags live`, `TestLive_*`, `*_live_test.go`) unless explicitly asked — they call paid LLM APIs.
- `make build` → `bin/adamarker`; `make frontend` → SPA bundle embedded into the next Go build.
- Demo fixtures: `make demo-data` regenerates the committed `data/demo/`; `make demo-walkthrough` seeds a completed demo exam into a RUNNING :8899 server (idempotent).

## Tooling

- **Python: always use `uv`.** Never invoke bare `python3` / `pip`; run scripts with `uv run python script.py`, one-off tools with `uvx <tool>`, deps with `uv add` / `uv pip`.
- **Go 1.26+** (`go.mod` floor; River itself needs 1.25+); build/test via the Makefile, not ad-hoc `go` invocations, so env wiring stays consistent.
- Prefer stdlib-first; third-party libraries only at the spec's defined seams (Renderer, BlobStore, VisionProvider, EmailProvider, Queue).

## Workflow

- **Don't push to GitHub without being asked.** Remote is `git@github.com:HaoWen46/adagrade.git`.
- **Never log, commit, or paste student PII** (names, IDs, emails, answer content, transcriptions); see the privacy gaps in `docs/PLAN_GAPS.md`.
- New logic is written **test-first** (see `internal/auth`, `internal/config`).
