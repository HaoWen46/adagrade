# Assessment stage navigation — design

**Problem.** The assessment page grew to 11 flat tabs (Overview, Problems, Submissions, Identify, Masking, Review, Consensus, Analysis, Regrade rounds, Totals, Publish). Eleven undifferentiated peers force the user to hold the workflow in their head, and the strip clips on narrow windows with no scroll affordance. User verdict: "too many tabs … it looks painful."

**Decision (user-approved 2026-07-10).** Two-level stage navigation, presentation-only. Chosen over per-stage merged mega-pages (major component surgery; PublishTab alone ~1.1k lines) and a Gradescope-style per-assessment left rail (fights the global sidebar for width; more churn; reversible later since the stage map below doubles as rail groupings).

## Structure

| # | Stage | Sub-view pills (order) | `?tab=` keys | Stage click lands on |
|---|---|---|---|---|
| 1 | Overview | — | `overview` | `overview` (default + unknown-key fallback, unchanged) |
| 2 | Problems | — | `problems` | `problems` |
| 3 | Student work | Submissions · Identify · Masking | `submissions`, `identify`, `masking` | `submissions` |
| 4 | Grading | Review · Consensus · Analysis + right-aligned **Start AI grading** button → `/runs?launch=1&assessment_id={id}` | `review`, `consensus`, `analysis` | `review` |
| 5 | Results | Totals · Publish · Regrade rounds (workflow order) | `totals`, `publish`, `regrades` | `totals` |

## Rules

- `?tab=` stays the single URL param and source of truth; the active stage is **derived** from the tab key via a static lookup. No `?stage=` param. All existing deep links keep working unchanged.
- Deterministic stage-click landings (first pill, with Grading→Review and Results→Totals keeping the frequent Review↔Totals hop at one click each way). No last-visited memory in v1 — unpredictable landings confuse the professor more than a visible pill click costs the TA.
- Both rows render as react-router `<Link>`s (middle-click works) with `replace` + `preventScrollReset`; `aria-current="true"` on the active stage, `aria-current="page"` on the active pill, inside `<nav aria-label>`. No tablist/roving-tabindex.
- The pill-row container has a fixed min height so single-view stages don't shift content vertically; pills render only when the active stage has >1 view.
- Both rows get `overflow-x-auto` + `whitespace-nowrap` + `shrink-0` items (also fixes the pre-existing narrow-window clipping).
- Pill `title` tooltips on Submissions ("one PDF per student") and Identify ("scanner pile").
- Visual rank: stages keep the underline-tab treatment; pills are small rounded-full, filled when active.
- Cut from v1 (revisit on demand): per-stage attention dots (false-alarm-prone before setup completes; the Overview checklist already shows these states), keyboard shortcuts (collide with Masking/AnswerView key handlers), last-visited memory, per-stage description strings.

## Scope

One file: `frontend/src/pages/AssessmentDetail.tsx` (STAGES structure replacing the flat TABS const, new nav block, updated header comment). Zero changes to tab components, routes, backend, or URL scheme. Verify in the browser: all 5 stages, all pills, ProblemReview back-link, Runs "Review masks" link, a Publish blocker link.

Origin: 3-lens design panel (professor-first, power-TA, IA rigor) + judge synthesis, 2026-07-10.
