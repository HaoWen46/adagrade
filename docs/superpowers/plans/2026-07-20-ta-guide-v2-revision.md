# TA Batch-Grading Guide v2 → v2.1 Revision Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Revise `docs/ta_batch_grading_guide_v2.typ` (the TA batch-grading SOP) into a publishable v2.1 that is AdaGrade-branded and accurate against the codebase as of 2026-07-20, then recompile the PDF.

**Architecture:** Doc-only revision of the Typst source plus an archive move of the v1 PDF; one optional, separately-gated task rebrands the frontend's display strings so the UI matches the guide. No behavior changes to the grading system.

**Tech Stack:** Typst 0.14.2 (`/opt/homebrew/bin/typst`), git, ripgrep. Fonts: New Computer Modern + Songti TC (already used by the doc; both resolve on this machine — the current PDF compiled here).

## Scope & Strategy (read first)

### Interpretation

The request was "read ta_batch_grading_guide_v2.pdf and study this project, and focus on creating the revision plan and strategy first." This plan interprets **the revision object as the guide document itself**: the guide is brand-new (dated 2026-07-20), was written for "ADA-Marker", and the project has just been renamed AdaGrade (commit 375dd1b); the working tree also deletes the root v1 PDF that the guide claims is "kept at the project root". If the intent was instead a *system* revision plan (building features the guide hedges about), see Track C below — those items are deliberately out of scope here and each needs its own design round.

### Verification result (why this plan is small)

Every load-bearing claim in the guide was checked against the code. The guide is **accurate**; only three real defects exist (naming, the v1-location claim, one wording issue) plus two unverifiable claims to resolve at execution time.

| Guide claim | Status | Evidence |
|---|---|---|
| Spot-check sample `min(max(5, 5%), 20)`, stratified, deterministic | ✅ verified | `internal/grading/spotcheck.go:28-29` |
| Tabs: Submissions / Identify / Masking / Overview / Review / Consensus / Analysis / Publish / Regrade rounds / Runs / Providers / Methods / Students / Guide | ✅ verified | `frontend/src/pages/*.tsx` (all exist by these names) |
| Identify queue keyboard `j / k / Enter`, roster-search-only picker | ✅ verified | `frontend/src/components/identify/OrphanQueue.tsx:298-330,72-230` |
| Regrade replies use `<p1>…</p1>` tags; malformed replies don't consume a turn | ✅ verified | `internal/regrade/parse_test.go`; `internal/httpapi/regrade.go:64` (`unparsed` kind, no token consumption) |
| Monthly budget + per-run cost cap | ✅ verified | `internal/config/config.go:229` (`ADAMARKER_MONTHLY_BUDGET_USD`), DECISIONS D35–D36 |
| Missing reference solution hard-blocks run launch | ✅ verified | `internal/httpapi/runs.go:255-285` (RunBlocker, "guaranteed to fail"; Runner.Plan hard-fails) |
| Local OCR via `make ocr-models`; cloud OCR opt-in | ✅ verified | `Makefile:63`; D21 ladder |
| Nightly backup order blobs → key → DB dump | ✅ verified | `deploy/backup.sh:2-13` |
| Render text-loss (non-embedded CJK font) warning | ✅ verified | `internal/render/textloss.go` |
| One page per (student × problem) cell; second sheet parks as duplicate | ✅ verified (and still true) | `internal/scan/identify.go:319-338`, PLAN_GAPS W1 |
| "一鍵抽樣 N 份" and "同分並排檢視" are *planned, not built* | ✅ verified absent | no calibration-sampling or side-by-side feature in `internal/` or `frontend/src` |
| Sources cite DECISIONS "D1–D68" | ✅ verified | `docs/DECISIONS.md` max is D68 |
| Analysis method cards: mean diff / exact / within-1 vs hand grades | ✅ verified | `frontend/src/pages/analysis/MethodCards.tsx:144` |
| System name "ADA-Marker" | ❌ **stale** | project renamed AdaGrade (README, `go.mod` module); binary + env vars still `adamarker`/`ADAMARKER_*` (`cmd/adamarker`, `.env.adamarker.example`) |
| "v1 原始文件保留於專案根目錄 `ta_batch_grading_guide.pdf`" | ❌ **stale** | working tree deletes the root v1 PDF (uncommitted `D`) |
| Result-PDF attachment "三種品質可選" | ⚠️ imprecise | the three options are **none / compressed / original** (`internal/publish/publish.go:414-423`) — one of the three is "no attachment", not a quality level |
| Publish dialog "會顯示寄件地址" | ❓ unverified | no sender-address string found in `frontend/src/pages/PublishTab.tsx`; resolve in Task 3 |
| ID-region edit "會使已審核頁面失效" | ❓ unverified | no invalidation path found in `internal/scan/`; W7 says seeding is append-only. Resolve in Task 3 |

### Strategy options

- **A — Minimal accuracy patch:** fix only the three defects, keep "ADA-Marker" branding. Cheapest, but ships a brand-new TA-facing doc under a retired name the day after the rename.
- **B — Rebrand + accuracy + v1 archive (RECOMMENDED, Tasks 1–5):** AdaGrade branding in prose with a "formerly ADA-Marker" note; operational identifiers (`adamarker` binary, `ADAMARKER_*` env) stay as-is and are footnoted — renaming them is a breaking ops change and out of scope (a codebase survey found ~286 `ADAMARKER_` references spanning config, systemd units `deploy/adamarker.service`, and all nine design specs; only `go.mod` and README were renamed in commit 375dd1b). Restore the v1 PDF into `docs/` as an archive so the guide's cross-reference stays true. Fix wording. Version bumps to v2.1 with a changelog note; the red-text convention keeps meaning "changed vs v1.0".
- **C — Product alignment (DEFERRED backlog, not in this plan):** build what the guide hedges about — calibration "sample N" one-click runs, same-score side-by-side review, multi-sheet answers ("append as extra page", PLAN_GAPS W1), problem-scoped overlay runs (deferred-features D1, audit 2026-07-16). Each is its own brainstorm → spec → plan cycle.

### Decision points for the user (defaults chosen so execution can proceed)

1. **v1 archive location** — default: restore to `docs/ta_batch_grading_guide_v1.pdf` (root deletion stands). Alternative: drop the file entirely and cite git history.
2. **Track B UI rebrand (Task 6)** — default: **skip unless approved**. 13 display strings across 7 frontend `.tsx` files still say "ADA-Marker" — including the **SPA sidebar**, the Login page, and the in-app Guide page the SOP cites as its primary source; doing them keeps UI and SOP consistent, but it's a code change. (`frontend/package.json` is also still named `ada-marker-frontend`; not user-visible, left alone by default.)
3. **Version label** — default: v2.1 (delta is small). Alternative: v3.
4. **Filename** — default: keep `ta_batch_grading_guide_v2.typ/.pdf` (version lives inside the doc; avoids link churn).

## Global Constraints

- Document language: Traditional Chinese (zh-TW), matching the existing doc; keep the `#chg[...]` red-text convention meaning "new/changed vs v1.0".
- Never introduce real student PII into examples (CLAUDE.md); the existing fictional roster rows (`b11902001,王小明,…`) are fine and stay.
- Do not push to GitHub (CLAUDE.md). Local commits only.
- Compile command: `typst compile docs/ta_batch_grading_guide_v2.typ docs/ta_batch_grading_guide_v2.pdf` — must exit 0.
- Prose says **AdaGrade**; literal operational identifiers (`adamarker`, `ADAMARKER_*`, `make ocr-models`, `docs/OPERATIONS.md` content) are **not** renamed anywhere.
- Go/frontend behavior is unchanged by Tasks 1–5; only Task 6 (if approved) touches code, and it changes display strings only.

---

### Task 1: Archive v1 PDF into docs/

**Files:**
- Create: `docs/ta_batch_grading_guide_v1.pdf` (restored from git HEAD)
- Delete (already deleted in working tree, make it official): `ta_batch_grading_guide.pdf`

**Interfaces:**
- Produces: the path `docs/ta_batch_grading_guide_v1.pdf`, referenced by Task 2's edited text.

- [ ] **Step 1: Restore v1 from HEAD into docs/**

```bash
git show HEAD:ta_batch_grading_guide.pdf > docs/ta_batch_grading_guide_v1.pdf
```

- [ ] **Step 2: Verify it is a valid PDF and the root copy stays deleted**

```bash
file docs/ta_batch_grading_guide_v1.pdf   # expect: PDF document
git status --short                         # expect: 'D ta_batch_grading_guide.pdf' and '?? docs/ta_batch_grading_guide_v1.pdf'
```

- [ ] **Step 3: Commit**

```bash
git add ta_batch_grading_guide.pdf docs/ta_batch_grading_guide_v1.pdf
git commit -m "docs: move TA grading guide v1 PDF from repo root to docs/ archive"
```

### Task 2: Rebrand prose to AdaGrade in the Typst source

**Files:**
- Modify: `docs/ta_batch_grading_guide_v2.typ`

**Interfaces:**
- Consumes: `docs/ta_batch_grading_guide_v1.pdf` (Task 1 path).
- Produces: the v2.1 source text Tasks 3–5 build on. All old-name occurrences after this task are exactly the sanctioned ones listed in Step 3.

- [ ] **Step 1: Apply the naming edits** (exact old → new; line numbers as of today):

1. Line 1 comment: `// ta_batch_grading_guide_v2.typ — 助教批改流程 v2.0（ADA-Marker 版）` → `// ta_batch_grading_guide_v2.typ — 助教批改流程 v2.1（AdaGrade 版，前身 ADA-Marker）`
2. Line 6: `title: "180 份考卷批次批改與 PDF 產出流程說明（v2.0，ADA-Marker 版）"` → `…（v2.1，AdaGrade 版）"`
3. Line 37: `#chg[（ADA-Marker 版）]` → `#chg[（AdaGrade 版）]`
4. Line 40: `掃描考卷匯入課程自架的 ADA-Marker 系統` → `掃描考卷匯入課程自架的 AdaGrade 系統`
5. Line 48 version row: `[#chg[v2.0（取代 v1.0 的 ChatGPT 網頁流程）]]` → `[#chg[v2.1（取代 v1.0 的 ChatGPT 網頁流程）]]`
6. After the 適用情境 row (line 50), add one grid row (literal Typst, backticks included — they render as raw text):

   ```typst
   [*系統名稱：*], [#chg[AdaGrade（前身 ADA-Marker；伺服器指令與環境變數仍沿用 `adamarker`／`ADAMARKER_*` 舊名）]],
   ```
7. Lines 55–57 閱讀說明 callout: change `v1.0 原始文件保留於專案根目錄 …pdf… 供對照` so the path reads `docs/ta_batch_grading_guide_v1.pdf` (keep the surrounding backticks as they are in the source)
8. Line 64 running header: `助教參考文件#chg[（v2.0）]` → `助教參考文件#chg[（v2.1）]`
9. Line 95: `ADA-Marker 是課程自架的服務` → `AdaGrade 是課程自架的服務`
10. Line 430: `本文件依 ADA-Marker 專案文件與系統內建說明整理：` → `本文件依 AdaGrade 專案文件與系統內建說明整理（部分沿用舊名 ADA-Marker）：`
11. Line 450 appendix header cell: `[*v2（ADA-Marker）*]` → `[*v2（AdaGrade）*]`

- [ ] **Step 2: Add a v2.1 changelog note.** Under the appendix heading `附錄：v1 → v2 變更總覽` (line 444), insert before the table:

```typst
#chg[_v2.1（2026-07-20）：系統更名 AdaGrade（原 ADA-Marker）；v1 文件移至
`docs/ta_batch_grading_guide_v1.pdf`；修正結果 PDF 附件選項的描述。
紅字仍表示相對 v1.0 的變更。_]
```

- [ ] **Step 3: Verify only sanctioned old-name occurrences remain**

```bash
grep -n "ADA-Marker\|adamarker\|ADAMARKER" docs/ta_batch_grading_guide_v2.typ
```
Expected remaining occurrences: the line-1 comment, the 系統名稱 row (item 6), the §12 sources line (item 10), and the two `ta_batch_grading_guide_v1.pdf`-adjacent mentions if any. No other hits.

- [ ] **Step 4: Commit**

```bash
git add docs/ta_batch_grading_guide_v2.typ
git commit -m "docs: rebrand TA grading guide v2.1 to AdaGrade, point v1 reference at docs/ archive"
```

### Task 3: Accuracy fixes + resolve the two unverified claims

**Files:**
- Modify: `docs/ta_batch_grading_guide_v2.typ`

**Interfaces:**
- Consumes: Task 2's text.
- Produces: final v2.1 wording for Task 5's compile.

- [ ] **Step 1: Fix the attachment-options wording (§10, line ~405).** Replace `每位學生的結果 PDF（發布信附件，三種品質可選；附件過大時自動退回逐題 JPEG 的 zip）` with:

```
每位學生的結果 PDF（發布信附件，選項三種：不附檔、壓縮影像或原始品質；
附件過大時自動退回逐題 JPEG 的 zip）
```

- [ ] **Step 2: Resolve the publish-dialog sender-address claim (§9 發布前 row).** Verify:

```bash
grep -rn -i "from\|寄件\|sender" frontend/src/pages/PublishTab.tsx | grep -vi "form\b" | head
grep -rn "FromEmail\|from_email" --include="*.go" internal/httpapi/ internal/publish/ | grep -v _test | head
```
Decision rule: if the publish confirm dialog demonstrably shows the sending address, keep the sentence. Otherwise change `發布對話框會顯示寄件地址，確認無誤後送出` to describe what the dialog actually shows (at minimum: recipient count and coverage/blocker summary — read the dialog JSX in `PublishTab.tsx` and describe that). Do not leave an unverifiable claim in a TA-facing SOP.

- [ ] **Step 3: Resolve the ID-region-edit claim (§9 開始前 row).** Verify:

```bash
grep -rn -i "region" --include="*.go" internal/httpapi/*.go | grep -i "update\|put\|handle" | head
grep -rn -i "review" --include="*.go" internal/scan/finalize.go | head
```
Decision rule: the current claim is `之後修改會使已審核頁面失效`. Per `internal/scan/finalize.go:88-96` (`seedMaskRegions`), region seeding is append-only and editing regions after finalize leaves stale mask rects (PLAN_GAPS W7) — it does not invalidate reviewed pages. Unless Step 3's greps show an invalidation path, replace the claim with the true, more useful warning:

```
（上傳前畫好；finalize 後再修改欄位區域不會重跑辨識，且舊遮罩框會殘留
——避免事後修改）
```

- [ ] **Step 4: Re-confirm the two "planned feature" hedges are still accurate** (they must stay if the features are still absent):

```bash
grep -rni "sample\|抽樣" frontend/src/pages/AnalysisTab.tsx frontend/src/pages/OverviewTab.tsx | head -5
grep -rni "side.by.side\|並排" frontend/src --include="*.tsx" | head -5
```
Expected: no calibration-sampling launcher, no same-score side-by-side view → keep both `規劃中功能` sentences unchanged. If either landed since 2026-07-20, rewrite that sentence to describe the shipped feature instead.

- [ ] **Step 5: Commit**

```bash
git add docs/ta_batch_grading_guide_v2.typ
git commit -m "docs: fix attachment-options wording and verify dialog/region claims in TA guide"
```

### Task 4: Compile and visually verify the PDF

**Files:**
- Modify (regenerate): `docs/ta_batch_grading_guide_v2.pdf`

**Interfaces:**
- Consumes: final .typ from Task 3.

- [ ] **Step 1: Compile**

```bash
typst compile docs/ta_batch_grading_guide_v2.typ docs/ta_batch_grading_guide_v2.pdf
```
Expected: exit 0, no missing-font warnings.

- [ ] **Step 2: Spot-check the output** — open the PDF and confirm: cover says v2.1／AdaGrade 版; the 系統名稱 row renders; the 閱讀說明 callout points at `docs/ta_batch_grading_guide_v1.pdf`; the appendix carries the v2.1 changelog note; red/black distinction is intact.

- [ ] **Step 3: Commit**

```bash
git add docs/ta_batch_grading_guide_v2.pdf
git commit -m "docs: recompile TA grading guide v2.1 PDF"
```

### Task 5: Cross-reference sweep

**Files:**
- Possibly modify: `README.md`, `docs/TA_DEPLOYMENT.md`

**Interfaces:**
- Consumes: nothing new; a pure check that other docs pointing at the guide still resolve.

- [ ] **Step 1: Find references to either guide file**

```bash
grep -rn "ta_batch_grading_guide" README.md docs/*.md docs/superpowers -l
```

- [ ] **Step 2: For each hit, confirm the path still resolves** (root v1 path is now `docs/ta_batch_grading_guide_v1.pdf`). Fix any stale path with the new location; commit as `docs: fix stale guide paths` only if changes were needed.

### Task 6 (OPTIONAL — requires explicit user approval, see Decision point 2): Frontend display-string rebrand

**Files:**
- Modify: `frontend/src/pages/Login.tsx`, `frontend/src/App.tsx`, `frontend/src/pages/GuidePage.tsx`, `frontend/src/pages/IdentifyTab.tsx`, `frontend/src/pages/EmailCallback.tsx`, `frontend/src/pages/PublishTab.tsx`, `frontend/src/lib/helpContent.tsx`

**Interfaces:**
- Produces: UI strings saying "AdaGrade", matching the revised SOP and README.

- [ ] **Step 1: Enumerate every occurrence**

```bash
grep -rn -i "ada-marker\|adamarker" frontend/src frontend/package.json
```
Expected: 13 hits across the 7 `.tsx` files above (counts as of 2026-07-20: helpContent 5, PublishTab 2, GuidePage 2, Login/IdentifyTab/EmailCallback/App 1 each) plus the `package.json` name `ada-marker-frontend` (leave the package name unless the user asks — it is not user-visible).

- [ ] **Step 2: Replace display text "ADA-Marker" → "AdaGrade" in each hit.** Rules: touch *user-visible strings only*; if a hit is an identifier, URL, or references the literal binary/env name (`adamarker`, `ADAMARKER_*`), leave it. In `GuidePage.tsx`, change `What ADA-Marker is` → `What AdaGrade is` and adjust the sentence at line 54 accordingly.

- [ ] **Step 3: Build and test**

```bash
make frontend && make test
```
Expected: both succeed. (Display-string edits must not break any Go test; if a Go test asserts on a UI string, update it in the same commit.)

- [ ] **Step 4: Commit**

```bash
git add frontend/src
git commit -m "frontend: rebrand user-visible ADA-Marker strings to AdaGrade"
```

---

## Self-review notes

- Spec coverage: all three defects and both unverifiable claims from the verification matrix map to Tasks 1–3; compile/cross-refs are Tasks 4–5; the naming-consistency concern maps to optional Task 6. Track C items are deliberately excluded (each is flagged in PLAN_GAPS with its own future-fix sketch).
- The doc's red-text semantics (`red = changed vs v1.0`) are preserved; all new v2.1 text is wrapped in `#chg[...]` in the steps above.
- Line numbers in Task 2 are as of 2026-07-20; the executor should match on the quoted text, not the line number, if the file has drifted.
