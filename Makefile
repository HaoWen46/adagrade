.PHONY: build test vet run tidy frontend dev db-up db-test-up db-down test-integration sqlc db-dump ocr-models report-fonts

DEV_DB_URL  ?= postgres://adamarker:adamarker@localhost:5433/adamarker?sslmode=disable
TEST_DB_URL ?= postgres://adamarker:adamarker@localhost:5434/adamarker_test?sslmode=disable

# Keep Go's writable build and module caches in the repository. This makes all
# Make targets work in sandboxed environments where the usual user-level Go
# caches are read-only. Callers may still override either variable if needed.
GOCACHE    ?= $(CURDIR)/.cache/go-build
GOMODCACHE ?= $(CURDIR)/.cache/go-mod
# Keep Buildx state alongside the Go caches without hiding the user's Docker
# config, which may provide the Compose plugin and selected Docker context.
BUILDX_CONFIG ?= $(CURDIR)/.cache/buildx

export GOCACHE GOMODCACHE BUILDX_CONFIG

# Vite builds into internal/web/assets/dist (gitignored); go:embed picks it up when
# present, else the committed placeholder serves (docs/DECISIONS.md D9).
frontend:
	cd frontend && npm install --no-fund --no-audit && npm run build

build:
	go build -o bin/adamarker ./cmd/adamarker

# `run`/`dev` source .env (provider keys) and point at the compose Postgres unless
# ADAMARKER_DATABASE_URL is already set. `dev` additionally enables the dev login.
run: db-up
	@set -a; [ -f .env ] && . ./.env; set +a; \
	export ADAMARKER_DATABASE_URL="$${ADAMARKER_DATABASE_URL:-$(DEV_DB_URL)}"; \
	go run ./cmd/adamarker

dev: db-up
	@set -a; [ -f .env ] && . ./.env; set +a; \
	export ADAMARKER_DATABASE_URL="$${ADAMARKER_DATABASE_URL:-$(DEV_DB_URL)}"; \
	export ADAMARKER_DEV_LOGIN=1; \
	go run ./cmd/adamarker

db-up:
	docker compose up -d --wait db

db-test-up:
	docker compose up -d --wait db-test

db-down:
	docker compose down

# Unit tests never need Postgres; integration tests read ADAMARKER_TEST_DATABASE_URL
# and skip themselves when it is unset.
test:
	go test ./...

test-integration: db-test-up
	ADAMARKER_TEST_DATABASE_URL="$(TEST_DB_URL)" go test ./... -count=1

vet:
	go vet ./...

tidy:
	go mod tidy

sqlc:
	go tool sqlc generate

# Local OCR (docs/DECISIONS.md D24): fetches the two model assets the offline
# identification rung needs into ./data/ocr/ (gitignored — never committed).
# libonnxruntime itself is NOT downloaded here: install it separately
# (`brew install onnxruntime`, or a GitHub release) at >= 1.27 — the ORT Go
# binding requires C API v26, which only 1.27+ satisfies (verified empirically,
# see .superpowers/sdd/t1-localocr-report.md).
OCR_DIR       := data/ocr
OCR_MODEL_URL := https://huggingface.co/SWHL/RapidOCR/resolve/main/PP-OCRv4/ch_PP-OCRv4_rec_infer.onnx
OCR_KEYS_URL  := https://raw.githubusercontent.com/PaddlePaddle/PaddleOCR/main/ppocr/utils/ppocr_keys_v1.txt

ocr-models:
	@mkdir -p $(OCR_DIR)
	@echo "Downloading ch_PP-OCRv4 rec model..."
	@curl -fL --retry 3 -o $(OCR_DIR)/ch_PP-OCRv4_rec_infer.onnx "$(OCR_MODEL_URL)"
	@size=$$(wc -c < $(OCR_DIR)/ch_PP-OCRv4_rec_infer.onnx | tr -d ' '); \
	if [ "$$size" -lt 5000000 ]; then \
		echo "error: $(OCR_DIR)/ch_PP-OCRv4_rec_infer.onnx is only $$size bytes (want >5MB) — download likely failed"; \
		rm -f $(OCR_DIR)/ch_PP-OCRv4_rec_infer.onnx; \
		exit 1; \
	fi
	@echo "Downloading ppocr_keys dict..."
	@curl -fL --retry 3 -o $(OCR_DIR)/ppocr_keys_v1.txt "$(OCR_KEYS_URL)"
	@lines=$$(wc -l < $(OCR_DIR)/ppocr_keys_v1.txt | tr -d ' '); \
	if [ "$$lines" -lt 1000 ]; then \
		echo "error: $(OCR_DIR)/ppocr_keys_v1.txt only has $$lines lines — download likely failed"; \
		rm -f $(OCR_DIR)/ppocr_keys_v1.txt; \
		exit 1; \
	fi
	@echo ""
	@echo "OCR assets ready in ./$(OCR_DIR)/"
	@echo ""
	@echo "Next steps — set these three env vars (e.g. in .env) to enable local OCR:"
	@echo "  ADAMARKER_OCR_MODEL=./$(OCR_DIR)/ch_PP-OCRv4_rec_infer.onnx"
	@echo "  ADAMARKER_OCR_KEYS=./$(OCR_DIR)/ppocr_keys_v1.txt"
	@echo "  ADAMARKER_ONNXRUNTIME=/path/to/libonnxruntime.{dylib,so}"
	@echo ""
	@echo "NOTE: libonnxruntime is NOT downloaded by this target. Install it"
	@echo "separately at >= 1.27 (brew install onnxruntime, or a GitHub release"
	@echo "from https://github.com/microsoft/onnxruntime/releases) — the Go"
	@echo "binding requires C API v26, only available from onnxruntime 1.27+."
	@echo "All three vars are optional; if unset (or only some are set), local"
	@echo "OCR stays disabled and the app behaves exactly as before."

# Backup order per docs/DECISIONS.md D15: blobs first, then the DB dump, so a restored
# DB never references a blob the tarball predates.
db-dump:
	@mkdir -p backups
	@ts=$$(date +%Y%m%d-%H%M%S); \
	tar -czf backups/blobs-$$ts.tgz -C . data 2>/dev/null || true; \
	docker compose exec -T db pg_dump -U adamarker adamarker > backups/db-$$ts.sql; \
	echo "backups/blobs-$$ts.tgz + backups/db-$$ts.sql"

# Report PDF font (report-attachments spec §3, D42/D43): fetches Noto Sans TC
# into ./data/fonts/ (gitignored — never committed), mirroring ocr-models'
# style (size-validated download, printed next-steps env var).
#
# The upstream notofonts/noto-cjk repo only ships OpenType/CFF (.otf) and
# OpenType Collection (.ttc) assets — fpdf's AddUTF8Font/AddUTF8FontFromBytes
# explicitly rejects "OTTO" (PostScript-outline) fonts ("fonts based on
# PostScript outlines are not supported", ttfparser.go), so those builds
# don't work here. google/fonts mirrors Noto Sans TC as a real TrueType
# (glyf-outline) variable font at a stable raw.githubusercontent.com path,
# which fpdf parses and embeds correctly (verified empirically against the
# CJK-gated test in internal/report/live_test.go, see
# .superpowers/sdd/n3-A-report.md) — fpdf renders the font's default-weight
# instance; it has no variable-font axis support, but does not need it for a
# single Regular weight.
REPORT_FONT_DIR := data/fonts
REPORT_FONT_URL := https://raw.githubusercontent.com/google/fonts/main/ofl/notosanstc/NotoSansTC%5Bwght%5D.ttf

report-fonts:
	@mkdir -p $(REPORT_FONT_DIR)
	@echo "Downloading Noto Sans TC..."
	@curl -fL --retry 3 -o $(REPORT_FONT_DIR)/NotoSansTC-Regular.ttf "$(REPORT_FONT_URL)"
	@size=$$(wc -c < $(REPORT_FONT_DIR)/NotoSansTC-Regular.ttf | tr -d ' '); \
	if [ "$$size" -lt 3000000 ]; then \
		echo "error: $(REPORT_FONT_DIR)/NotoSansTC-Regular.ttf is only $$size bytes (want >3MB) — download likely failed"; \
		rm -f $(REPORT_FONT_DIR)/NotoSansTC-Regular.ttf; \
		exit 1; \
	fi
	@echo ""
	@echo "Report font ready in ./$(REPORT_FONT_DIR)/"
	@echo ""
	@echo "Set this env var (e.g. in .env) to enable PDF/ZIP result attachments:"
	@echo "  ADAMARKER_REPORT_FONT=./$(REPORT_FONT_DIR)/NotoSansTC-Regular.ttf"
	@echo ""
	@echo "NOTE: this is optional — if unset, the report/attachment feature stays"
	@echo "disabled and publishing without attachments works exactly as before."

# Regenerate the committed demo data (data/demo/): roster CSV, exam PDF with the
# answer-sheet template page, and the 40-page shuffled scan pile. Deterministic.
demo-data:
	uv run --with reportlab python scripts/make-demo-data.py

# Seed "Demo Exam — completed" into the RUNNING dev server on :8899 through the
# public HTTP API (demo-polish plan 2026-07-10): intake, masks, an AI run on the
# cheapest flash method, spot-check, publish, and two webhook-filed regrade
# threads. Idempotent (skips if the assessment exists); needs the dev server up
# (.claude/launch.json / scripts/dev-e2e.sh) and the demo roster imported.
demo-walkthrough:
	uv run --with reportlab python scripts/seed-demo-walkthrough.py
