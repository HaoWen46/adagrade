# Typst transcription mirror — design (2026-07-30)

Status: approved (user, 2026-07-30). LaTeX stays the primary transcription export; Typst ships as a secondary mirror inside the same bundle.

## Decision summary

- One ZIP, both formats: every transcription bundle gains `typ/{studentID}.typ` per answer and a bundle-level `_all.typ`, beside the existing `tex/` and `_all.tex`. No new endpoints, no new buttons.
- The emitter reuses the LaTeX math validator's verdicts verbatim via mitex; there is no LaTeX→Typst math translation anywhere.
- Gate asymmetry: the tectonic compile gate keeps blocking a bundle that fails; a Typst compile failure never blocks — it flags in the manifest and the bundle ships.
- UI is copy-only: the card keeps its ladder and single button per row; the title and description state the LaTeX-primary / Typst-mirror hierarchy.

## Emitter (`internal/transcribe/typst.go`)

- `EmitTypstBody(d Doc) (string, []string)` mirrors `EmitBody`, consuming the SAME validator decisions: the shared `$`-pairing (`closingDollar`) and `ValidateMathInline` / `ValidateMath` verdicts drive both outputs.
- Accepted math embeds the validated LaTeX fragment verbatim: `#mi("<latex>")` inline, `#mitex("<latex>")` display — the D70 mitex path from internal/report.
- Prose and demoted fragments become `#"<escaped>"` string literals via the escapeTypstString convention; code blocks become `#raw("<escaped>", block: true)` (no verbatim-escape class exists); lists become `#list(...)` items; the app-controlled title becomes `#heading(...)`.
- SECURITY INVARIANT (inherits typst-report spec 2026-07-20): student-derived bytes appear ONLY inside escaped Typst string literals or inside mitex string arguments that passed the math allow-list; they never reach Typst markup position, so `#import`, backticks, and markup characters are inert.
- Flag parity by construction: because both emitters branch on identical validator verdicts, `EmitTypstBody` returns the same flags list as `EmitBody`; a test pins equality so the manifest's one flags column stays truthful for both formats.
- `EmitTypst`/`TypstPreamble` mirror the LaTeX composition seams: one preamble (mitex import + CJK font setup compatible with BuildTypst's font-dir handling) reused by the standalone and `_all.typ` paths.

## Bundle layout (`internal/export`)

- `problemEntries` appends, per answer, `typ/{id}.typ` (deflate) and one `_all.typ`, AFTER the tex entries so `tex/` stays first in the fixed entry order; byte-determinism is preserved (no timestamps, no map iteration).
- `_all.typ` mirrors `_all.tex`: one preamble, pseudonymous `#heading` sections in id-sorted order, header comments naming LaTeX as authoritative and Typst as a convenience mirror.
- `AnswerTeXes` stays the gate's attribution source (LaTeX only); a sibling `AllTyp(in)` exposes the `_all.typ` source for the secondary gate, pinned byte-identical to the bundled entry by test (same invariant as `AllTeX`).
- MANIFEST.csv row shape is unchanged; the Typst verdict is one header comment line (`# typst: verified|failed|unverified`) plus the existing mirror-documentation comment. Per-answer flags stay LaTeX-derived (they are shared with the Typst emitter by construction).

## Gate policy (`internal/httpapi`)

- Primary (unchanged): tectonic compiles `_all.tex`; failure attributes per answer via `gateError` and refuses the bundle.
- Secondary: when the report renderer's Typst binary is configured, the gate compiles `_all.typ` once (BuildTypst-style invocation with the runaway kill); on failure the bundle STILL ships and the manifest header records the verdict. No per-answer Typst attribution — bundle-level verdict only (YAGNI for a secondary format; the LaTeX gate already attributes).
- No Typst binary configured → no Typst gate, no flag; the UI's verification line already distinguishes verified/unverified per format.
- Logs stay content-free (existing errContentFree discipline); Typst compile stderr is suppressed the same way tectonic's is.

## UI (`frontend/src/pages/TranscriptionExportCard.tsx`)

- Card title: "Transcriptions (LaTeX + Typst)".
- Description adds one sentence: each answer ships as LaTeX source (primary, `tex/`) with a Typst mirror (`typ/`) beside it.
- The model/verification line reports both formats from what the status endpoint already knows at render time (config, not build outcomes): ".tex compile-checked before it ships" (or unverified) plus "Typst mirror included (best-effort)"; the per-build Typst verdict lives in the manifest, not the card. No new buttons, rows, or states.

## Testing

- Parity: flags equality between `EmitBody` and `EmitTypstBody` across prose/math/code/list/demotion cases; determinism (byte-identical re-emission); `AllTyp` == bundled `_all.typ` entry; entry-order pin keeps `tex/` first.
- Injection: the LaTeX bomb corpus plus Typst-specific hostiles (backticks, `#import`, `#eval`, markup chars, quote/backslash escapes in string literals) must all land inside string literals or demote — never in markup position.
- Gate asymmetry: fake typst binary — bundle ships with manifest flag on Typst failure; tectonic failure still refuses; both-gates-green adds no flags.
- Live compile test (gated, `TestLive_*`): `_all.typ` for the sample fixture compiles with the real typst binary + mitex, verifying mitex covers the allow-listed command set (\therefore, \overset, matrices, cases, CJK in #text).

## Non-goals

- No native Typst math syntax generation; mitex is the only math path.
- No per-format download endpoints or UI pickers; the bundle is the unit.
- No PDF rendering of transcriptions in this feature; compile gates verify, they do not ship PDFs.
