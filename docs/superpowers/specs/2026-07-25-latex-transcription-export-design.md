# LaTeX transcription export — design (2026-07-25)

Optional, professor-facing export of scanned handwritten answers as `.tex` source, plus
the original masked page images, packaged per (assessment, problem). Explicitly **not**
part of the grading workflow.

## 1. Why

The professor wants a text rendering of student solutions "just in case" he has to grade
everything himself — he intends to upload the `.tex` to a chat LLM. He is attached to
LaTeX specifically; he does not care how it is produced, but he will spot-check compiled
PDFs against the original handwriting to judge quality.

Two constraints follow from the ask, and they drive the whole design:

1. **No per-click spend on inconsistent output.** Re-exporting must be free and
   byte-identical. Only genuinely new transcription may cost money.
2. **Quality must be checkable.** He will sample compiled output against source crops, so
   that comparison is a product surface, not a manual chore.

## 2. The core decision: the model never writes `.tex`

A vision model asked to emit a LaTeX *document* produces output that is unverifiable
(does it compile?), unsafe (TeX is Turing-complete), and irreproducible. So the model
emits a **constrained intermediate** and deterministic Go code writes the LaTeX.

```
masked page images
   │
   ├─(1) TRANSCRIBE   model → []Block          PAID, cached, non-deterministic
   │
   ├─(2) VALIDATE     allow-list over math      free, pure, deterministic
   │
   ├─(3) EMIT         []Block → .tex           free, pure, deterministic
   │
   └─(4) VERIFY       tectonic compile → PDF   free, cached, sandboxed
```

Stages 2–4 are pure functions of stage 1's output. Consequences:

- Re-export is free and byte-identical.
- Improving the emitter re-exports the whole cohort at zero cost.
- A given crop is paid for **once, ever** — the cache key is
  `(masked image SHA-256 list, model id, prompt version, params hash)`.

### 2.1 Why an allow-list, not escaping

`internal/report/typstmarkup.go` keeps hostile text safe because Typst has a literal-string
content token (`#"…"`) — user text can be made structurally inert. **LaTeX has no such
primitive.** Math mode must let commands through; that is what math mode is for. And TeX is
Turing-complete: `\def\x{\x}\x` is a two-token hang, the same failure class D70 had to kill
with a subprocess timeout.

So the math path is an **allow-list of command names**, not an escape function. `\frac`,
`\sum`, `\leq` pass; `\def`, `\newcommand`, `\csname`, `\expandafter`, `\input`, `\write`,
`\loop`, `\read`, `\catcode` never do. Anything unrecognised is demoted to literal text and
flagged in the manifest — never silently dropped, never passed through.

This makes the injection defense and the runaway-compile defense the same mechanism: you
cannot build a macro bomb out of an alphabet with no macro-definition primitive.

Prose text takes the other path — full TeX escaping of `# $ % & _ { } ~ ^ \`.

### 2.2 Why compiling is not enough

The most dangerous documented failure mode is not broken LaTeX, it is **fluent, compilable,
fabricated** LaTeX (a 2026 benchmark records a model emitting well-formed LaTeX with 1–4%
similarity to the source page). A compile gate cannot detect it. Only visual comparison can.
Hence stage 4 stores the rendered PDF and the review UI shows it beside the source crop.

## 3. Output contract

```
{assessment-slug}-p{problem-number}/
├── _all.tex           every student, pseudonymous, one preamble
├── MANIFEST.csv       student_id, pseudonym, pages, status, source, confidence
├── tex/{student_id}.tex
└── images/{student_id}.jpg          (or -p1/-p2 when the answer spans pages)
```

- **Identity lives in filenames, never in file bytes.** Mechanically testable, and tested.
- **`_all.tex` uses pseudonyms** (`Student 001`, ordered by sorted student ID);
  `MANIFEST.csv` is the local decoder ring and is documented as not-for-upload. This gives a
  genuinely zero-PII bulk-upload path, which is the case the professor actually cares about.
- **Images are the masked variants** (`answer_pages.masked_image_ref`). The filename already
  identifies the student, so masking costs nothing and avoids re-exporting the identity
  region the pipeline exists to strip.
- **`status` distinguishes `ok` / `illegible` / `failed` / `absent`.** An empty `.tex` is
  otherwise ambiguous between "wrote nothing", "transcription failed", and "never scanned" —
  three very different facts when grading.
- **`source`** records `grading-cache` vs `dedicated`, so mixed-provenance exports are legible.

Caveat recorded honestly: filenames travel with uploads, so per-student files still disclose
the ID to whatever service they are uploaded to. Only the `_all.tex` path is fully clean.

## 4. Privacy — and closing B-C10

`PLAN_GAPS.md` B-C10 (Critical, open) already records that `grading_records.transcription`
can contain unmasked identity permanently, because masking is region-based and students
write their name in margins the mask does not cover. Its prescribed fix is a scrub pass
**before persistence**.

This feature is what turns that latent gap into an outbound leak, so the fix belongs here —
and it belongs at **write time**, not export time. Scrubbing only on the way out leaves the
PII permanently in the immutable store and does the work twice.

`internal/regrade.Redact(text, Identity)` already exists (D51) and returns `RedactionCounts`.
Reuse it on `transcription` before insert.

The export also holds grading's D10 line on masks: a page whose mask is not
human-**accepted** (pending or flagged) is treated exactly as an unmasked page — that
student's row degrades to `failed` with a `mask not accepted` flag, and neither the
provider nor the bundle sees the image. Grading refuses to run under the same condition;
an export that shipped what a run may not send would be a policy hole. Verified live:
flipping one page to `pending` drops that student's images and marks the row; restoring
it returns the bundle byte-identical with no re-spend.

Two things fall out:

- The filename↔content invariant is testable: no exported byte stream contains any roster
  name or ID.
- `RedactionCounts` becomes a **mask-quality metric** — routine redactions mean identity text
  is surviving the image mask, which is a leak signal for the core grading path, not just
  for this export.

## 5. Model selection

Deferred to the operator; the transcription model is a method-style config value, not a
constant. Findings that constrain the choice:

- **No dedicated OCR model is callable on OpenRouter** (verified against the live catalog:
  345 models, 183 vision-capable, zero OCR specialists; `baidu/qianfan-ocr-fast` exists as a
  catalog record but serves `"endpoints": []`). The 2026 OCR-specialist field is self-host or
  direct-vendor only.
- **Mathpix is disqualified** — its own docs limit handwritten recognition to Hindi and
  Latin-alphabet languages; Traditional Chinese is printed-only.
- **The handwriting × zh-Hant intersection is unevaluated** in public benchmarks.
  OmniDocBench has no zh-Hant category and no handwriting leaderboard; where handwriting is
  isolated, scores drop from ~0.96 to ~0.70. Printed-OCR scores do not transfer.
- **Reproducibility favours first-party routing.** `google/gemini-3.1-flash-lite`
  ($0.25/$1.50 per Mtok, image $0.25/Mtok, 1.05M ctx) supports `seed`, `temperature`,
  `structured_outputs`, and serves from six endpoints that are all first-party Google — no
  third-party quantisation drift. `anthropic/claude-sonnet-5` exposes no `seed` at all.

The export records the model id and prompt version per cached transcription, so a later
model change is a new cache entry rather than a silent overwrite.

### 5.1 Measured, 2026-07-25 — `google/gemini-3.1-flash-lite`

Live runs against OpenRouter (`internal/transcribe/openrouter_live_test.go`, gated behind
`TRANSCRIBE_LIVE=1` so a plain `go test` never spends money):

| Fixture | In / out tokens | Result |
|---|---|---|
| Demo page (typed English prose) | 1573 / 63 | verbatim; `confidence=high`; no invented content |
| zh-Hant + inline math + pseudocode | 1561 / 260 | 5 blocks; `confidence=high`; **zero demotion flags**; compiled |

The zh-Hant round trip preserved `\min_{j<i}` subscripts, `O(n^2)` superscripts,
`O(n \log n)`, pseudocode indentation, and every Traditional character — with no
Simplified-character leakage, which is the documented zh-Hant failure mode and is now
asserted in the test.

**Cost: ≈$0.0008 per answer.** A 200-student × 8-problem exam is ~1,600 answers ≈ **$1.30**,
paid once because transcriptions are cached content-addressed. Re-exports are free.

Caveat that still stands: both fixtures are **printed**, not handwritten. This isolates the
language and notation problem — which is now measured and passing — from the handwriting
problem, which remains unmeasured until real scans exist.

## 6. Components

| Unit | Responsibility | Depends on |
|---|---|---|
| `internal/transcribe` (`Block`, `Doc`) | intermediate representation | — |
| `internal/transcribe` validator | LaTeX allow-list; demote-and-flag | — |
| `internal/transcribe` emitter | `Doc` → `.tex`, fixed preamble | validator |
| `internal/transcribe` prompt/schema | constrained output contract | `internal/llm` |
| `internal/transcribe` compile gate | `tectonic` subprocess, sandboxed | config |
| store + migration `0038` | cache table, content-addressed | — |
| River job `transcribe_answer` | one answer per leaf | queue, llm, store |
| `internal/export` ZIP builder | folder contract above | transcribe, blobstore |
| httpapi + frontend | trigger, progress, download, review | all |

`tectonic` is an optional binary behind `ADAMARKER_TECTONIC_BIN`, mirroring D70's
`ADAMARKER_TYPST_BIN`: sandboxed to a temp root, hard subprocess timeout, no shell-escape,
stderr suppressed (compiler diagnostics quote source lines, which embed student text). When
unset, `.tex` still exports; it is marked `unverified` in the manifest rather than silently
presented as checked.

CJK requires XeLaTeX (`xeCJK`) — tectonic runs XeTeX. The bundled
`data/fonts/NotoSansTC-Regular.ttf` is the only CJK face available, so bold/italic synthesise.

## 6.1 Placement revision (2026-07-29) — the ladder card

The export surface first shipped as a card under Results → Totals. That placement was
wrong twice over: the lecturer's intent is object-scoped ("the midterm's LaTeX"), not
workflow-stage-scoped, so burying it in the fifth tab made it find-by-accident; and an
always-visible header button was rejected too (dead chrome through most of an
assessment's life), as was conditional visibility (a control that materializes cannot
be planned around).

The resolution: **one always-present, stage-aware card on the Overview tab**, directly
after the grading-workflow card. It never appears or disappears; its *content* advances
through a ladder that narrates the path to the artifact:

1. no problems → "Waiting on problems" + set-up link
2. no student work → "Waiting on scans" + upload link
3. mask review incomplete → "Mask review a/b pages accepted" + review link
4. ready → the downloads table: an **entire-exam bundle** (`GET
   /api/assessments/{id}/transcription.zip`, one `{slug}/` dir nesting each problem's
   per-problem tree) plus the per-problem rows, costs shown before any click.

The status endpoint carries the gate counts (`gates{problems, students_total,
students_with_work, pages_total, pages_mask_accepted}`, top-level and per-problem
`ready`). Both ZIP endpoints refuse with 409 when an included problem is not
mask-ready — the UI contract is "no bundle with mask-failed rows", enforced
server-side, with the per-answer degrade kept as defense in depth.

## 7. Testing

- Validator: allow-list table tests; a macro-bomb corpus (`\def\x{\x}\x`, `\csname`,
  `\input`) must all demote to literal text.
- Emitter: golden-file `.tex`; byte-identical output for identical input.
- Compile gate: real `tectonic` run behind a build tag (skipped when the binary is absent),
  including a timeout regression test — D70's macro-bomb precedent.
- Privacy: property test asserting no exported byte contains a roster name/ID.
- Cache: same input → zero provider calls on the second run.

## 8. Deliberately out of scope

- A `.typ` emitter. The block format is engine-agnostic, so this stays a ~200-line addition
  with no model changes. Preserved as an option, not built.
- Feeding the dedicated transcription back into grading. Additive later; touching D5's
  reproducibility contract for an optional export is the wrong trade.
- Quality calibration against real handwriting — the dev corpus is synthetic typed English
  (all 88 stored transcriptions are one identical mock string), so this requires a real key
  against real scans and is sequenced after the build.
