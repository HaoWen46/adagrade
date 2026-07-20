# AGENTS.md — working conventions for ADA-Marker

AI-assisted grading system. Start here: product plan [`ADA-Marker_Plan.md`](ADA-Marker_Plan.md),
architecture [`docs/superpowers/specs/2026-07-01-ada-marker-architecture-design.md`](docs/superpowers/specs/2026-07-01-ada-marker-architecture-design.md),
open plan gaps [`docs/PLAN_GAPS.md`](docs/PLAN_GAPS.md).

## Tooling

- **Python: always use `uv`.** Never invoke bare `python3` / `pip`. Run scripts with
  `uv run python script.py` (or `uv run - <<'PY' … PY` for inline), one-off tools with
  `uvx <tool>`, and manage deps with `uv add` / `uv pip`. This keeps Python hermetic and
  reproducible even though the app itself is Go + TypeScript.
- **Go 1.26+** (`go.mod` floor; River itself needs 1.25+). Build/test via the Makefile: `make test`, `make build`, `make run`.
- Prefer stdlib-first; third-party libraries only at the spec's defined seams
  (Renderer, BlobStore, VisionProvider, EmailProvider, Queue).

## Workflow

- **Don't push to GitHub without being asked.** Remote is `git@github.com:HaoWen46/adagrade.git`.
- **Never log, commit, or paste student PII** (names, IDs, emails, answer content, transcriptions).
  See the privacy gaps in `docs/PLAN_GAPS.md`.
- New logic is written **test-first** (see `internal/auth`, `internal/config`).
