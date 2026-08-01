# Typst Transcription Mirror Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every transcription bundle ships a Typst mirror (`typ/{id}.typ`, `_all.typ`) beside the primary LaTeX, with a best-effort Typst compile verdict in the manifest and copy-only UI changes.

**Architecture:** One validator, two emitters — `EmitTypstBody` consumes the same `closingDollar`/`ValidateMath*` verdicts as `EmitBody` and embeds accepted LaTeX math verbatim via mitex (`#mi`/`#mitex`); all student text lands in escaped Typst string literals (typst-report invariant). Export appends typ entries after tex; the httpapi gate adds a non-blocking bundle-level Typst compile whose verdict becomes a manifest header line.

**Tech Stack:** Go stdlib; typst binary via existing `config.TypstBinPath`; mitex (version pinned by `internal/report.mitexVersion`).

## Global Constraints

- Spec: docs/superpowers/specs/2026-07-30-typst-transcription-mirror-design.md — LaTeX primary, Typst secondary; Typst failure never blocks a bundle.
- Security: student bytes appear ONLY inside escaped Typst string literals or mitex string arguments that passed the math allow-list; never in markup position.
- Flag parity: `EmitTypstBody` returns the identical flags slice as `EmitBody` (pinned by test).
- Determinism: byte-identical re-emission; no timestamps, no map iteration; `tex/` entries stay ahead of `typ/` in ZIP order.
- Tests: TDD; never run `TestLive_*` / `*_live_test.go` (cost money); run `go test ./internal/<pkg>/ -run 'Test[^L]' -count=1`.
- Never log/commit student PII; compiler stderr stays suppressed.
- gofmt + go vet before each commit; do not push.

---

### Task 1: Typst emitter (`internal/transcribe`)

**Files:**
- Create: `internal/transcribe/typst.go`
- Test: `internal/transcribe/typst_test.go`

**Interfaces:**
- Consumes: `Doc`, `Block*` kinds, `closingDollar`, `backslashEscaped`, `mathLikeAfter`, `ValidateMath`, `ValidateMathInline`, `normalizeEOL`, `expandTabs` (all existing, same package).
- Produces: `TypstPreamble() string`, `EmitTypst(d Doc) (string, []string)`, `EmitTypstBody(d Doc) (string, []string)`, `escapeTypstString(s string) string` (package-private duplicate of internal/report's, cross-referenced comment).

- [ ] Step 1: write failing tests: `TestEmitTypstBody_FlagParityWithLaTeX` (corpus: safe prose+inline math, demoted inline `\def`, display math ok + demoted, currency dollars, empty math skip, list with lead-in + `[illegible]` item, code with tabs + `\end{verbatim}`, unknown kind, unpaired math-like `$ \alpha $` — assert `reflect.DeepEqual(latexFlags, typstFlags)` per doc); `TestEmitTypstBody_StudentTextNeverInMarkupPosition` (hostile corpus: backticks, `#import "x"`, `#eval`, `*bold*`, `"quote"`, `\` — assert every occurrence in output sits inside a `#"…"`/`#raw(…)`/`#mi(…)` string literal, i.e. output outside string literals matches `^[#a-z(),: \[\]0-9._\n-]*$`-style structural alphabet); `TestEmitTypst_IsDeterministic`; `TestEmitTypstBody_MathReusesValidatedLaTeXViaMitex` (accepted `O(n \log n)` inline → `#mi("O(n \\log n)")`, display frac → `#mitex(`); `TestTypstPreamble_ImportsMitexAndCJKFont`.
- [ ] Step 2: run, confirm all fail (functions undefined).
- [ ] Step 3: implement. Core shape:

```go
func EmitTypstBody(d Doc) (string, []string) {
    // mirrors EmitBody branch-for-branch; SAME conditions -> SAME flag strings
    // prose: renderInlineTypst; math: ValidateMath(text) -> #mitex("…") | demote to #"…" + flag
    // code: #raw("…", block: true) on normalizeEOL+expandTabs (no defuse flag: raw has no escape class — parity: LaTeX defuse flag only fires on \end{verbatim}, so Typst must emit the same flag on that input to keep parity)
    // list: #list([…], …) items via renderInlineTypst; lead-in text first; empty list skipped
}
func renderInlineTypst(s string) (string, []string) {
    // same scan as renderInline: closingDollar decides; ok -> #mi("<body>"); demote -> #"$<body>$" + same flag; literal $ -> #"$" (+ same mathLikeAfter flag)
}
```

  `TypstPreamble()`: `#import "@preview/mitex:<ver>": mi, mitex` + `#set text(font: ("Libertinus Serif","Noto Sans TC"), lang:"zh", region:"TW")` + page/par settings mirroring report's; version from a local `const transcribeMitexVersion` kept equal to report's (cross-check comment).
- [ ] Step 4: run `go test ./internal/transcribe/ -run 'Test[^L]' -count=1` → all pass.
- [ ] Step 5: gofmt, vet, commit `feat: Typst transcription emitter (mitex mirror of the LaTeX path)`.

### Task 2: bundle wiring (`internal/export`)

**Files:**
- Modify: `internal/export/export.go` (`problemEntries`, `renderManifest` caller, `Input`)
- Test: `internal/export/typ_test.go`

**Interfaces:**
- Consumes: `transcribe.TypstPreamble`, `transcribe.EmitTypstBody`.
- Produces: `Input.TypstVerdict string` ("", "verified", "failed"; empty renders "unverified"), `AllTyp(in Input) (string, error)`, ZIP entries `<root>/typ/{id}.typ` + `<root>/_all.typ`, manifest header line `# typst: <verdict>`.

- [ ] Step 1: failing tests: `TestBuildZIP_ContainsTypMirror` (typ/{id}.typ per answer + _all.typ; tex entries precede typ entries in zr.File order); `TestAllTyp_MatchesTheBundledEntry`; `TestManifest_RecordsTypstVerdict` (default → `# typst: unverified`; TypstVerdict "verified"/"failed" → verbatim); determinism test extension.
- [ ] Step 2: run → fail. Step 3: implement — typstSection helper mirroring emitSection (same scrubDoc + title + status comment as a Typst line comment `// status: …`); `_all.typ` = header comments + TypstPreamble + sections. Step 4: green + existing export suite green. Step 5: commit `feat: Typst mirror entries and verdict line in transcription bundles`.

### Task 3: generic Typst source compile (`internal/report`)

**Files:**
- Modify: `internal/report/typst.go`
- Test: extend `internal/report/typst_test.go`

**Interfaces:**
- Produces: `CompileTypstSource(ctx context.Context, bin, fontDir, src string) ([]byte, error)` — extracted from BuildTypst's tail (temp dir, --root sandbox, --creation-timestamp 0, 20s kill, stderr suppressed); BuildTypst refactored to call it. Sentinel `ErrTypstCompileFailed` for the gate to branch on.

- [ ] Step 1: failing test with a fake typst script (marker-fail pattern from httpapi's fakeTectonic) pinning arg shape + sentinel. Step 2: fail. Step 3: extract. Step 4: report suite green (non-live). Step 5: commit `refactor: expose generic Typst source compile for the transcription gate`.

### Task 4: secondary gate + status flag (`internal/httpapi`)

**Files:**
- Modify: `internal/httpapi/transcription.go` (both ZIP handlers + status response)
- Test: extend `internal/httpapi/compile_gate_test.go`, status test file

**Interfaces:**
- Consumes: `export.AllTyp`, `report.CompileTypstSource`, `s.cfg.TypstBinPath`, report font dir already used by the report handler (same config source).
- Produces: `typstVerdict(ctx, in) string` returning "", "verified", "failed" (empty when no binary configured); handlers set `in.TypstVerdict` BEFORE BuildZIP; status JSON gains `typst bool` (binary configured).

- [ ] Step 1: failing tests: `TestTypstVerdict_NeverBlocksTheBundle` (fake typst fails → verdict "failed", handler-level: BuildZIP still succeeds and manifest carries `# typst: failed`); `TestTypstVerdict_UnconfiguredIsUnverified`; status response includes `typst`. LaTeX-gate tests unchanged and green.
- [ ] Step 2: fail. Step 3: implement (verdict computed after the tectonic gate passes; any CompileTypstSource error → "failed", context errors → "" with content-free log). Step 4: green. Step 5: commit `feat: best-effort Typst compile verdict on transcription bundles`.

### Task 5: UI copy (`frontend`)

**Files:**
- Modify: `frontend/src/pages/TranscriptionExportCard.tsx`, `frontend/src/lib/types.ts`

- [ ] Step 1: types.ts `TranscriptionStatusResponse` gains `typst: boolean`. Card: title `Transcriptions (LaTeX + Typst)`; description sentence: "Each answer ships as LaTeX source (primary, tex/) with a Typst mirror (typ/) beside it."; verification line appends `data.typst ? " · Typst mirror included (best-effort)" : ""`. No structural changes.
- [ ] Step 2: `cd frontend && npm run typecheck` → clean. Step 3: commit `feat: transcription card states the LaTeX-primary / Typst-mirror hierarchy`.

### Task 6: verification and review

- [ ] `go test ./internal/transcribe/ ./internal/export/ ./internal/report/ -run 'Test[^L]' -count=1`; `go test ./internal/httpapi/ -run 'TestVerifyProblemTeX|TestGateFailure|TestTypstVerdict|TestTranscriptionStatus' -count=1`; gofmt -l; go vet; `make test` (background).
- [ ] One Opus 5 review subagent (per ~/.claude/guides/OPUS-5-SUBAGENT-GUIDE.md: single subagent, effort high, compact return contract) over the full diff: security invariant, flag parity, determinism, spec conformance; fix confirmed findings test-first.
- [ ] Add gated `TestLive_AllTypCompilesWithMitex` in `internal/export/live_test.go` (skips without ADAMARKER_TYPST_BIN; NOT run in this session) covering the spec's mitex-coverage check for the allow-listed command set.
- [ ] Final commit if review produced fixes.
