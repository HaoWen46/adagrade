.PHONY: build test vet run tidy frontend dev db-up db-test-up db-down test-integration sqlc db-dump ocr-models report-fonts doctor

# Preflight: checks Go/cgo/Docker-or-DATABASE_URL/frontend/bootstrap-admin and
# prints the exact fix for anything missing. Run this first when setup fails.
doctor:
	@sh scripts/doctor.sh

DEV_DB_URL  ?= postgres://adamarker:adamarker@localhost:5433/adamarker?sslmode=disable
TEST_DB_URL ?= postgres://adamarker:adamarker@localhost:5434/adamarker_test?sslmode=disable

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
OCR_DIR   := data/ocr
OCR_MODEL := $(OCR_DIR)/PP-OCRv5_server_rec_infer.onnx
OCR_KEYS  := $(OCR_DIR)/ppocrv5_dict.txt
# PP-OCRv5 server rec (D24): supersedes the ch_PP-OCRv4 mobile rec whose charset
# was missing many Traditional Chinese characters. Primary source is the official
# PaddlePaddle org export (Apache-2.0); the ModelScope mirror is the fallback for
# networks that cannot reach huggingface.co. The two copies differ only by a few
# bytes of ONNX metadata — either satisfies the gates below.
OCR_MODEL_URL := https://huggingface.co/PaddlePaddle/PP-OCRv5_server_rec_onnx/resolve/main/inference.onnx
OCR_MODEL_ALT := https://modelscope.cn/models/RapidAI/RapidOCR/resolve/master/onnx/PP-OCRv5/rec/ch_PP-OCRv5_rec_server.onnx
OCR_KEYS_URL  := https://raw.githubusercontent.com/PaddlePaddle/PaddleOCR/main/ppocr/utils/dict/ppocrv5_dict.txt
# Sanity gates: the v5 server rec export is ~84.5MB and ppocrv5_dict.txt carries
# 18,383 entries. A file materially under these landed an HTML error page or was
# truncated mid-transfer, so the target deletes it and fails loudly rather than
# leaving a corrupt asset the engine would reject later with a stranger error.
OCR_MODEL_MIN_BYTES := 80000000
OCR_KEYS_MIN_LINES  := 18000

# Both assets download to a .part file and are moved into place only after their
# size/line gate passes: the gates above are also the SKIP conditions, so a
# Ctrl-C'd 80MB partial written straight to $(OCR_MODEL) would be large enough to
# be mistaken for a complete model on the next run and would fail later, at load
# time, as an unreadable ONNX graph. mv within one directory is atomic.
ocr-models:
	@mkdir -p $(OCR_DIR)
	@if [ -f $(OCR_MODEL) ] && [ "$$(wc -c < $(OCR_MODEL) | tr -d ' ')" -ge $(OCR_MODEL_MIN_BYTES) ]; then \
		echo "PP-OCRv5 server rec model already present — skipping download."; \
	else \
		echo "Downloading PP-OCRv5 server rec model (~85MB)..."; \
		curl -fL --retry 3 -o $(OCR_MODEL).part "$(OCR_MODEL_URL)" || \
		curl -fL --retry 3 -o $(OCR_MODEL).part "$(OCR_MODEL_ALT)" || \
		{ echo "error: both mirrors failed for $(OCR_MODEL)"; rm -f $(OCR_MODEL).part; exit 1; }; \
		size=$$(wc -c < $(OCR_MODEL).part | tr -d ' '); \
		if [ "$$size" -lt $(OCR_MODEL_MIN_BYTES) ]; then \
			echo "error: the download is only $$size bytes (want >80MB) — discarding the partial file"; \
			rm -f $(OCR_MODEL).part; \
			exit 1; \
		fi; \
		mv $(OCR_MODEL).part $(OCR_MODEL); \
		echo "model ok ($$size bytes)"; \
	fi
	@if [ -f $(OCR_KEYS) ] && [ "$$(wc -l < $(OCR_KEYS) | tr -d ' ')" -ge $(OCR_KEYS_MIN_LINES) ]; then \
		echo "ppocrv5_dict.txt already present — skipping download."; \
	else \
		echo "Downloading ppocrv5_dict charset..."; \
		curl -fL --retry 3 -o $(OCR_KEYS).part "$(OCR_KEYS_URL)" || \
		{ echo "error: download failed for $(OCR_KEYS)"; rm -f $(OCR_KEYS).part; exit 1; }; \
		lines=$$(wc -l < $(OCR_KEYS).part | tr -d ' '); \
		if [ "$$lines" -lt $(OCR_KEYS_MIN_LINES) ]; then \
			echo "error: the download only has $$lines lines (want >=18000) — discarding the partial file"; \
			rm -f $(OCR_KEYS).part; \
			exit 1; \
		fi; \
		mv $(OCR_KEYS).part $(OCR_KEYS); \
		echo "keys ok ($$lines lines)"; \
	fi
	@echo ""
	@echo "OCR assets ready in ./$(OCR_DIR)/"
	@echo ""
	@echo "Next steps — set these three env vars (e.g. in .env) to enable local OCR:"
	@echo "  ADAMARKER_OCR_MODEL=./$(OCR_MODEL)"
	@echo "  ADAMARKER_OCR_KEYS=./$(OCR_KEYS)"
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
