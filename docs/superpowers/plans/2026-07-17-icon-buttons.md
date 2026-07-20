# Plan 2026-07-17 — icon buttons: stop spelling out every repeated action

User directive: "not every button has to be a literal big box containing the exact word
describing exactly what the action is. it's painful to read. figure out what can be
simplified into icon for better experience."

## Design rules (binding)

- **Icons replace text only where context already tells the story**: repeated row-level
  actions in tables (the column position + tooltip carries the meaning), close/dismiss
  controls, small inline edit affordances next to a titled thing.
- **Text stays** on: primary CTAs (Publish, Launch, Apply, Upload, Import, Save…, New…,
  Add…, Send result, Grade pending (N), Start AI grading, Create answer rows…),
  destructive confirms inside dialogs (Cancel / the typed-confirm action button), and any
  action whose meaning is NOT recoverable from its surroundings. Verdict buttons in the
  regrade drawer (Upheld / Regraded…) stay text — they are semantic decisions.
- **No new npm dependency.** Icons are inline SVGs in a new
  `frontend/src/components/icons.tsx`: 16×16 viewBox 24 stroke-based (`stroke="currentColor"`,
  `strokeWidth={2}`, `fill="none"`, `strokeLinecap="round" strokeLinejoin="round"`),
  Lucide/Feather-style paths, one exported component per icon, `aria-hidden="true"`.
- **New `IconButton` in `frontend/src/components/ui.tsx`**: square (p-1.5), rounded-md,
  text-neutral-500, hover:text-neutral-900 hover:bg-neutral-100, focus-visible ring
  matching Button, disabled state matching Button; `danger` variant hovers red
  (hover:text-red-600 hover:bg-red-50). Props: required `label: string` (becomes BOTH
  `aria-label` and `title` — the tooltip is mandatory, never optional), `variant?`,
  standard button attrs. Never render an IconButton without a label.
- Row-action cells keep a stable rightward order: benign actions first, destructive last.
- Every touched page must still pass `npx tsc --noEmit` + `npm run build`.

## Task 12 — icon system + first three pages

Files owned: `frontend/src/components/icons.tsx` (new), `frontend/src/components/ui.tsx`
(IconButton addition only), `frontend/src/pages/Students.tsx`,
`frontend/src/pages/Providers.tsx`, `frontend/src/pages/Assessments.tsx`.

1. Create `icons.tsx` with exactly the icons the sweep needs (add here as needed):
   Pencil (edit/rename), Trash (delete), X (close/remove), Archive (box), Send
   (paper-plane, resend), RotateCcw (retract/reinstate-style undo), UserMinus (withdraw),
   UserCheck (reinstate), Eye (review/view), Zap (provider test), Tag (pricing),
   ChevronDown/ChevronRight ONLY if a page hand-rolls ▸/▾ glyphs today (check; do not
   convert expanders that already work).
2. Add `IconButton` per the design rules.
3. **Students.tsx**: per-row Withdraw → IconButton(UserMinus, label "Withdraw
   B11902001 — keeps history, excluded from new runs/publishes" or the existing tooltip
   copy if any; label must include the student id for screen readers), Reinstate →
   IconButton(UserCheck, label "Reinstate <id>"), Delete → danger IconButton(Trash,
   label "Delete <id> — only possible while no artifacts reference them"). The typed-confirm
   delete dialog keeps its text buttons.
4. **Providers.tsx**: per-row Test → IconButton(Zap, "Test <name>"), Pricing →
   IconButton(Tag, "Pricing for <name>"), Edit → IconButton(Pencil, "Edit <name>"),
   Delete → danger IconButton(Trash, "Delete <name>"). "Add provider" stays a text Button.
5. **Assessments.tsx**: per-row Archive → IconButton(Archive, "Archive <name>") /
   Unarchive if present. "New assessment" stays text. The archive confirm dialog keeps text.

## Task 13 — remaining sweep

Files owned: `frontend/src/pages/PublishTab.tsx`, `frontend/src/pages/SubmissionsTab.tsx`,
`frontend/src/pages/MaskingTab.tsx`, `frontend/src/pages/AssessmentDetail.tsx`,
`frontend/src/pages/Regrades.tsx`, `frontend/src/pages/Methods.tsx`,
`frontend/src/pages/Users.tsx` (only if a candidate exists), `frontend/src/components/icons.tsx`
(additions only), reusing Task 12's IconButton unchanged.

1. **PublishTab.tsx**: ledger per-row Resend → IconButton(Send, "Resend to <student>").
   The batch-level buttons (Publish/Unpublish/resend-failed etc.) stay text.
2. **SubmissionsTab.tsx**: per-row Retract → danger IconButton(RotateCcw,
   "Retract <file> — removes ungraded pages"). Upload stays text.
3. **MaskingTab.tsx**: per-region Redraw → IconButton(Pencil, "Redraw region #N"),
   Remove → danger IconButton(X or Trash, "Remove region #N"). "Save regions" /
   "Apply masks" stay text. Mask-review "Accept (a)" / "Flag (f)" stay text (they teach
   keyboard shortcuts).
4. **AssessmentDetail.tsx**: title "Rename" → IconButton(Pencil, "Rename assessment")
   beside the h1; problems-table per-row "Edit" → IconButton(Pencil, "Edit problem N").
   The per-row "Review" is navigation — keep it as a compact text link (it names a
   destination, not an action); do NOT icon-ify tab pills or workflow links.
5. **Regrades.tsx**: drawer "Close" → IconButton(X, "Close") top-right of the drawer
   header; "Edit note" → IconButton(Pencil, "Edit note") if the layout still reads
   clearly. "Send result", verdict buttons, "AI re-grade" stay text.
6. **Methods.tsx**: version-history "Archive" per method → IconButton(Archive,
   "Archive <method>") if it is a row action; "New method"/"New version" stay text.
7. Grep for any remaining ≥3-times-repeated identical text button in
   frontend/src/pages and either convert it under the same rules or list it in the
   report with the reason it should stay text.

## Verification (both tasks)

`cd frontend && npx tsc --noEmit && npm run build`; implementer browser-verifies the
changed pages on the :8899 dev server (rebuild via `make frontend && make build`; the
dev-e2e script self-rebuilds the Go binary) — dev-login b11902156@ntu.edu.tw, check
tooltips appear on hover (title attr) and that keyboard focus shows a visible ring.
Controller browser-verifies after each task. No hooks below early returns. Commit per
task with explicit paths, style `feat(ui): …`.
