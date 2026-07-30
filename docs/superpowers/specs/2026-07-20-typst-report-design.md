# Typst result-PDF renderer (LaTeX math, at last) — design

*2026-07-20. Driver: the grading template mandates "LaTeX for math" in transcriptions and
comments (guide §1/§5, `internal/grading/prompt.go:144`), but nothing in the system renders
it — TAs and students see raw `\frac{...}` source. v1 solved this with XeLaTeX+ctex; v2
dropped the `.tex` pipeline and its fpdf report has no math engine. This round adds a
Typst-based result-PDF renderer — Typst is already the repo's doc toolchain, is CJK-capable,
and its `mitex` package renders LaTeX math directly (probed locally on typst 0.14.2).*

## Scope

**In:** an optional Typst renderer for the per-student result PDF (the publish attachment),
gated on `ADAMARKER_TYPST_BIN`; when unset or when a compile fails, the existing fpdf
renderer runs exactly as today. The Typst report renders the same disclosure the email
already makes — per-criterion (name, score/max) plus the **problem-level comment** — with
LaTeX math typeset via mitex in criterion names and comments. `ProblemReport` gains the
missing problem-level `Comment` field (the email discloses it already; the PDF omitting it
was a wiring gap, not a decision).

**Out (deliberately):** per-criterion AI rationales in student output (the email blanks
them today — an existing disclosure decision this round must not silently reverse);
transcriptions in student output (PLAN_GAPS B-C10 forbids); math rendering in the TA
browser UI (KaTeX is the right tool there — separate round); email-body math (email
clients can't).

## Mechanism

- `internal/report/typstmarkup.go` — pure converter from a comment/name string to Typst
  markup. **Injection-safety invariant** (comments are model/TA text derived from student
  answers — hostile input must be assumed): the generated markup consists ONLY of
  `#"<escaped string>"`, `#mi("<escaped latex>")`, `#mitex("<escaped latex>")`, and
  `#parbreak()` tokens. User text can never appear bare in markup position, so Typst
  directives (`#import`, `#read`, backticks) in a comment render as literal text.
  Math spans recognized: `$$…$$` and `\[…\]` (display), `\(…\)` and `$…$` (inline);
  unbalanced delimiters fall back to literal text.
- `internal/report/typst.go` — `BuildTypst(bin, fontDir string, in ReportInput)`: writes
  a temp dir (doc.typ + quality-processed page JPEGs, reusing the fpdf path's
  `downscaleForReport`), runs
  `bin compile --root <tmp> --creation-timestamp 0 [--font-path fontDir] doc.typ out.pdf`,
  returns the bytes. `--root` confines Typst's file access to the temp dir (defense in
  depth alongside escaping); `--creation-timestamp 0` keeps builds byte-deterministic
  (same input → same bytes, the fpdf invariant). On failure the error carries the exit
  status only — never stderr, which can quote source lines containing comments (PII rule).
- `internal/publish/sender.go` — when the sender has a Typst binary configured, PDF
  attachments try `BuildTypst` first and fall back to fpdf on error (a Typst hiccup must
  not fail a send); ZIP fallback unchanged.
- Config: `ADAMARKER_TYPST_BIN` (absolute path; empty = disabled). Operational note: mitex
  is a Typst package fetched once into the local package cache — first compile needs
  network or a pre-seeded cache (`--package-path`/`TYPST_PACKAGE_CACHE_PATH`), documented
  in `.env.adamarker.example`.

## Testing

Converter: exhaustive pure tests (injection attempts, unbalanced math, CJK, escaping).
BuildTypst: skipped when no `typst` binary is on PATH (mirrors live-test gating);
determinism (two builds byte-equal), %PDF magic, hostile-comment compile succeeds.
Config: env parse test. Live: republish the demo exam with a Typst-built attachment and
inspect the rendered PDF.
