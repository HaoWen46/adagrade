// Central plain-language help copy, rendered via <HelpTip title="…">{block}</HelpTip>.
// Audience: course staff with no LLM background — no jargon without explanation.
// Keeping the text here (not inline in pages) keeps wording consistent.

import type { ReactNode } from "react";

/** Providers page intro card. */
export const providersIntro =
  "ADA-Marker grades by sending each (masked) answer image to an AI model over the internet. " +
  "A provider is the company/endpoint that hosts the model. Add one here with the API key from " +
  "their console — the key is stored encrypted on this server and never shown again.";

// --- provider form fields -------------------------------------------------------------

export const providerHelp: Record<
  "name" | "baseUrl" | "apiKey" | "models" | "rateLimit",
  ReactNode
> = {
  name: (
    <p>
      A short handle you&apos;ll pick in grading methods, e.g. &lsquo;qwen&rsquo;. Lowercase, no
      spaces.
    </p>
  ),
  baseUrl: (
    <p>
      Where API requests are sent. Comes with the preset — only change it if your provider&apos;s
      docs say so.
    </p>
  ),
  apiKey: (
    <>
      <p>
        A password-like token from the provider&apos;s console that authorizes (paid) API calls.
      </p>
      <p>
        Treat it like a password: it&apos;s stored encrypted and only the last 4 characters stay
        visible. If it leaks, revoke it in the provider console.
      </p>
    </>
  ),
  models: (
    <p>
      Model names this provider offers, used for the picker when building grading methods.
      &lsquo;Test&rsquo; can fill this automatically on endpoints that support listing.
    </p>
  ),
  rateLimit: (
    <p>
      How many requests per second ADA-Marker may send. Keep the default unless the provider
      documents other limits — going too fast causes errors, not faster grading.
    </p>
  ),
};

// --- method (grading config) fields ---------------------------------------------------

export const methodHelp: Record<
  "provider" | "model" | "temperature" | "refSolutions" | "reaskCap" | "promptTemplate",
  ReactNode
> = {
  provider: <p>Which configured AI endpoint to use — set these up on the Providers page.</p>,
  model: (
    <p>
      The specific AI model. For handwriting you need a vision-capable model, e.g.{" "}
      <code className="font-mono text-xs">qwen3-vl-plus</code>.
    </p>
  ),
  temperature: (
    <p>
      0 = most deterministic: the same answer grades the same way on re-runs. Leave at 0 for
      grading.
    </p>
  ),
  refSolutions: (
    <p>
      Include the problem&apos;s reference solution in the prompt so the model grades against your
      canonical answer (recommended).
    </p>
  ),
  reaskCap: (
    <p>
      If the model returns malformed output, how many times to re-ask before giving up on that
      answer.
    </p>
  ),
  promptTemplate: (
    <p>
      The versioned instruction text sent with every answer. Managed centrally; methods pin a
      version so old grades stay reproducible.
    </p>
  ),
};

/** Methods dialog — the max output tokens field. */
export const maxTokensHelp: ReactNode = (
  <>
    <p>
      The most text the model may write back for one answer. Tokens are the word-sized pieces
      models read and write (roughly 3–4 characters each) — providers bill per token.
    </p>
    <p>
      Leave this blank and ADA-Marker applies its built-in cap of 4096 tokens, which is enough
      for normal grading output. Setting it too low does not save money in a useful way: the
      model&apos;s grading output gets cut off mid-answer, which shows up as malformed-output
      re-asks and failed items — costing more, not less.
    </p>
  </>
);

/** Methods detail — why versions are append-only, and what Archive does. */
export const methodVersionsHelp: ReactNode = (
  <>
    <p>
      Methods are never edited in place. &ldquo;New version&rdquo; saves a fresh copy of the
      settings, and every grading run records exactly which version it used — so old grades
      always trace back to the settings that produced them.
    </p>
    <p>
      Archive just hides the method: it disappears from the launch-run picker and from the
      methods list (tick &ldquo;Include archived&rdquo; to see it). Nothing is deleted — past
      runs and their grades are untouched, and Unarchive brings it back any time.
    </p>
  </>
);

// --- grading policy (D25) --------------------------------------------------------------

/**
 * Core mental model, repeated wherever policy shows up: the rubric decides what earns
 * points; the policy decides how the model resolves ambiguity while applying it.
 */
export const gradingPolicyHelp: ReactNode = (
  <>
    <p>
      The rubric always decides <strong>what</strong> earns points — the policy only changes{" "}
      <strong>how the model resolves ambiguity</strong> while applying it: messy handwriting, a
      step left implicit, a borderline justification.
    </p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>Lenient</strong> — gives the benefit of the doubt; ambiguity resolves in the
        student&apos;s favor. Good for formative homework where the goal is encouragement.
      </li>
      <li>
        <strong>Standard</strong> — rubric-faithful, the default for most grading: exactly what
        the rubric supports, as a careful human TA would score it.
      </li>
      <li>
        <strong>Strict</strong> — the exam standard: only complete, demonstrated work earns
        points, and the model would rather flag an answer as low-confidence than guess in the
        student&apos;s favor.
      </li>
    </ul>
    <p>
      The exact wording sent to the model is system-managed (not editable here) — use the prompt
      preview to see precisely what changes between policies.
    </p>
    <p>
      Every grading record keeps the policy it was produced under, so results are always
      traceable back to the stance that was in effect.
    </p>
  </>
);

export const policyMixHelp: ReactNode = (
  <p>
    These official grades for the same problem were produced under different policies. Because
    policy changes how ambiguity is resolved, mixing it within one problem means some
    students&apos; borderline cases were judged by a different standard than others&apos; — pick
    one policy per problem before publishing.
  </p>
);

// --- analysis tab ---------------------------------------------------------------------

export const analysisHelp: Record<"scoreStats" | "agreement" | "overrideRate" | "cost", ReactNode> = {
  scoreStats: (
    <>
      <p>
        Every AI grading record for this assessment, grouped by problem and grading method.
      </p>
      <p>
        Use it to sanity-check a method before trusting it: a spread of 0 or a huge share of
        zeros/max-scores usually means the rubric or prompt needs work — not that every student
        performed identically.
      </p>
    </>
  ),
  agreement: (
    <p>
      Compares each method&apos;s scores with the most recent human grade on the same answer and
      rubric version. Low average difference and high within-1 = the method behaves like your
      human graders on this data.
    </p>
  ),
  overrideRate: (
    <>
      <p>
        For each method, the share of its AI-graded answers whose <em>official</em> grade ended up
        being a human record — i.e. a human replaced or adjusted that method&apos;s suggestion —
        plus the mean absolute difference between the AI total and the final official total.
      </p>
      <p>
        Only answers with both an AI record from this method AND an official grade already set
        are counted. A method missing from this table has no officials set yet for anything it
        graded — that means &quot;no data&quot;, not &quot;never overridden&quot;.
      </p>
    </>
  ),
  cost: (
    <p>
      Actual spend for every run launched against this assessment, from the run&apos;s own priced
      grading records (never an estimate). &quot;Per answer&quot; divides a run&apos;s total cost
      by its succeeded-item count — exact only once the run has finished; still directionally
      useful mid-flight.
    </p>
  ),
};

// --- consensus tab ----------------------------------------------------------------------

/** Consensus tab intro card (semantics: docs/DECISIONS.md D17). */
export const consensusIntro =
  "Consensus combines the grades your selected methods ALREADY produced — no new AI calls, " +
  "free to re-run after every grading run or settings change. One standard applies to every " +
  "problem and submission in this assessment.";

export const consensusHelp: Record<"combiner" | "faultTolerance" | "flagTriggers", ReactNode> = {
  combiner: (
    <>
      <p>
        <strong>majority</strong> — at least (panel − fault tolerance) models must give exactly
        the same criterion score; disagreements fall back to the average and get flagged.
      </p>
      <p>
        <strong>mean</strong> — average per criterion, snapped to the rubric increment; models
        further than one increment away count against the fault tolerance.
      </p>
    </>
  ),
  faultTolerance: (
    <p>
      How many models may be missing or off-track before an answer is flagged instead of
      trusted. Cannot reach half the panel.
    </p>
  ),
  flagTriggers: (
    <>
      <p>
        Which situations mark an answer for human review on the Review tab. Each consensus run
        adds <em>and clears</em> these flags to match its latest result:
      </p>
      <ul className="list-disc space-y-1 pl-4">
        <li>
          <strong>models disagree</strong> — a criterion stayed contested even after allowing for
          the fault tolerance.
        </li>
        <li>
          <strong>too few grades</strong> — not enough usable grades from the panel; no consensus
          is written for that answer.
        </li>
        <li>
          <strong>models unsure</strong> — more models than the fault tolerance reported low or
          illegible confidence.
        </li>
      </ul>
    </>
  ),
};

export const consensusPolicyMixHelp: ReactNode = (
  <p>
    The methods in this panel were configured with different grading policies. The{" "}
    <strong>models disagree</strong> flag can no longer tell a genuine model error apart from a
    model simply following a stricter or more lenient stance — for a clean disagreement signal,
    panel methods should share one policy.
  </p>
);

export const reasoningLevelHelp: ReactNode = (
  <>
    <p>
      Many newer models can &quot;think&quot; before answering — they spend extra (billed) output
      tokens working through the answer, which usually helps on judgment-heavy tasks like
      grading against a rubric.
    </p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>default</strong> — let the model do whatever it normally does.
      </li>
      <li>
        <strong>off</strong> — ask for no thinking (fastest/cheapest; models with built-in
        mandatory reasoning still run their minimum).
      </li>
      <li>
        <strong>low / medium / high</strong> — how much thinking effort to request.
      </li>
    </ul>
    <p>
      On OpenAI-style providers (incl. OpenRouter) this maps to the standard reasoning-effort
      control; Anthropic-style providers currently ignore it. Comparing the same model at
      different levels is exactly the kind of experiment methods are for.
    </p>
  </>
);

export const apiKindHelp: ReactNode = (
  <>
    <p>
      Providers speak one of two common request formats (&quot;wire protocols&quot;). ADA-Marker
      supports both — pick whichever the provider&apos;s docs say:
    </p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>OpenAI-compatible</strong> — the most widespread. Used by OpenRouter, OpenAI, and
        most self-hosted gateways.
      </li>
      <li>
        <strong>Anthropic-compatible</strong> — used by Anthropic (Claude) and the DeepSeek/Qwen
        compatibility endpoints.
      </li>
    </ul>
    <p>Presets pick this for you; you only choose it for custom endpoints.</p>
  </>
);

// --- roster ---------------------------------------------------------------------------

export const rosterCsvHelp: ReactNode = (
  <>
    <p>
      The roster is a plain CSV (exportable from Excel/Sheets/NTU COOL) with a header row and
      three required columns, in any order:
    </p>
    <pre className="overflow-x-auto rounded bg-neutral-100 px-2 py-1.5 font-mono text-xs">
      student_id,name,email{"\n"}b11902001,Alice Liddell,alice@ntu.edu.tw
    </pre>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        The file must be <strong>UTF-8</strong>. Excel&apos;s plain &ldquo;CSV&rdquo; save (Big5 on
        Chinese Windows) is rejected with an error — in Excel, use{" "}
        <strong>Save As → &ldquo;CSV UTF-8 (Comma delimited)&rdquo;</strong>; Google Sheets and NTU
        COOL exports are already UTF-8.
      </li>
      <li>
        <strong>student_id</strong> is the identity everywhere: re-imports update rows by it, and
        uploaded submissions are matched by filename{" "}
        <code className="font-mono text-xs">&lt;student_id&gt;.pdf</code>.
      </li>
      <li>Extra columns are ignored; column order and header capitalization don&apos;t matter.</li>
      <li>
        If any line has a problem (duplicate id, missing email, an email already belonging to a
        different student…), the <strong>whole file is rejected</strong> with line numbers — the
        roster is never half-imported. Two students may never share an email: grade emails would go
        to the same mailbox.
      </li>
      <li>Re-importing later is safe: existing students are updated, new ones added.</li>
      <li>
        Importing <strong>never withdraws or reinstates anyone by itself</strong>. Instead the
        result shows a diff — active students missing from the CSV (add/drop) and withdrawn
        students present in it (retakers) — with one-click <em>Withdraw all</em> /{" "}
        <em>Reinstate all</em> buttons, so syncing to the registrar list is always an explicit,
        reviewable step.
      </li>
    </ul>
  </>
);

/** Students page — Status column + withdraw dialog (D23, roster-lifecycle 2026-07-10). */
export const withdrawHelp: ReactNode = (
  <>
    <p>
      <strong>Withdraw</strong> is an archive, not a delete: the student row and everything already
      graded stay in the system, and re-importing the roster won&apos;t bring them back to active.
    </p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        Withdrawn students <strong>stop blocking publish</strong>: their ungraded answers no longer
        count against the coverage gate.
      </li>
      <li>
        They are excluded from <em>new</em> grading runs and <em>new</em> publish batches — no
        result email is prepared for them. Results already published stay exactly as sent.
      </li>
      <li>
        Resending a withdrawn student&apos;s result is refused (reinstate first);
        resend-all/resend-failed skip them and report how many were skipped.
      </li>
      <li>
        They stay visible in totals and the grades CSV export with an explicit{" "}
        <strong>withdrawn</strong> marker — never silently dropped, so the registrar sheet stays
        complete.
      </li>
      <li>
        Their regrade channel stays open: a withdrawn student can still dispute a grade that was
        already published; those queue entries just carry a withdrawn flag.
      </li>
      <li>
        They are also skipped by future grading preparation, ingest reports, and scan matching;
        existing submissions, grades, and audit history are untouched.
      </li>
      <li>
        <strong>Reinstate</strong> flips them back to active at any time — nothing is lost either
        way.
      </li>
    </ul>
  </>
);

// --- masking --------------------------------------------------------------------------

export const maskingHelp: ReactNode = (
  <>
    <p>
      Masking draws opaque boxes over name/ID areas on <strong>copies</strong> of each page. Only
      masked copies are ever sent to the AI provider; you and other staff always see the original.
    </p>
    <p>
      Draw a box over wherever students write their name, apply, then review each page — one by
      one, or accept all pending at once after spot-checking a few. Grading runs stay blocked
      until every page&apos;s mask is accepted.
    </p>
    <p>
      Applying masks runs in the background, so it stays responsive even for a full class of
      submissions — the review list below fills in as pages finish. Re-applying after you tweak a
      region only re-masks pages whose regions actually changed; everything else keeps its
      existing review status.
    </p>
  </>
);

/** MaskingTab — the per-region scope/color/padding controls. */
export const maskRegionControlsHelp: ReactNode = (
  <>
    <p>Each row is one box.</p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>Page scope</strong> — &ldquo;first page&rdquo; masks only the first page of each
        student&apos;s uploaded PDF (where names usually are); &ldquo;all pages&rdquo; masks
        every page — beware of covering actual work.
      </li>
      <li>
        <strong>Color</strong> — the hex fill of the printed box. It is drawn fully solid on the
        masked copy (the editor preview is see-through so you can aim), and an invalid value
        falls back to the default dark gray.
      </li>
      <li>
        <strong>Padding</strong> — grows the box on all four sides, as a fraction of the page
        size (0.01 = 1% of the page&apos;s width/height): a little padding hides handwriting that
        pokes out past the box you drew. Boxes are clipped at the page edge, so generous padding
        is safe.
      </li>
    </ul>
  </>
);

// --- users ------------------------------------------------------------------------------

/** Users page — the ta / lecturer / admin role ladder. */
export const userRolesHelp: ReactNode = (
  <>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>ta</strong> — day-to-day grading: review and grade answers, launch AI runs, handle
        regrade requests and result sending.
      </li>
      <li>
        <strong>lecturer</strong> — everything a TA can, plus course administration: create
        assessments and problems, import the roster, withdraw students, manage AI providers and
        pricing, assign TAs, publish results.
      </li>
      <li>
        <strong>admin</strong> — everything, plus managing users, waiving spot-check gates,
        unpublishing, deleting assessments and problems, and the audit log.
      </li>
    </ul>
    <p>
      Changes here apply immediately — no sign-out needed. Unchecking <strong>active</strong> cuts
      that person off right away, even if they are signed in; their history and past actions are
      kept. (You can&apos;t demote or deactivate your own account.)
    </p>
  </>
);

// --- submissions + identify -------------------------------------------------------------

/** OrphanQueue / MatrixCard header — the page-level identify pipeline states. */
export const scanPageLifecycleHelp: ReactNode = (
  <>
    <p>Every uploaded page moves through a fixed set of states:</p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>processing</strong> — identity is still being read (local OCR, then cloud OCR if
        enabled).
      </li>
      <li>
        <strong>orphan</strong> — nothing could place it automatically; it needs a manual student
        + problem in the queue below. <strong>assigned</strong> is the automatic counterpart: the
        student id and name agreed with each other and matched a valid problem, so it was placed
        without a human.
      </li>
      <li>
        <strong>parked</strong> — a duplicate or a conflict with an existing page/submission; see
        the Parked pages card.
      </li>
      <li>
        <strong>promoted</strong> — finalize has turned it into a real submission answer. This is
        the only state that actually counts toward grading.
      </li>
    </ul>
    <p>
      Auto-assign is deliberately conservative: it only fires when the student id and name both
      point to the same roster row and the problem number is one that exists in this assessment —
      any disagreement or ambiguity falls back to a human in the orphan queue.
    </p>
  </>
);

/** ParkedCard header — duplicates vs conflicts, and what a re-upload does. */
export const scanParkedHelp: ReactNode = (
  <>
    <p>Parked pages are set aside because placing them automatically would be unsafe:</p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>Duplicates</strong> — this page looks like the same page as one already assigned.
        Usually a re-scan or a double feed; discard the extra copy.
      </li>
      <li>
        <strong>Conflicts</strong> — this page and an existing page (or submission) both claim the
        same student × problem cell. When the incumbent is another scan page, choose to keep it or
        replace it with the parked page. When the incumbent is a direct submission (uploaded from
        the Submissions tab, no backing scan page), Replace isn&apos;t available here — go to the
        Submissions tab, find that student&apos;s reconciliation row, and click <strong>Retract</strong>{" "}
        to free the cell, then re-run identification or assign manually.
      </li>
    </ul>
    <p>
      Re-uploading a batch never overwrites an already-filled cell — it only fills cells that are
      still empty. A page that would collide with existing work always lands here for a human
      decision instead of silently replacing anything.
    </p>
  </>
);

/** FinalizeCard — what finalize does and the ack-missing flow. */
export const scanFinalizeHelp: ReactNode = (
  <>
    <p>
      Finalize promotes every currently-assigned page into a real submission answer, across the
      whole assessment — not scoped to one upload batch.
    </p>
    <p>
      It is incremental and safe to re-run: pages already promoted are left alone, so you can
      finalize early, keep identifying more pages, and finalize again later to pick up the rest.
    </p>
    <p>
      If some student × problem cells still have no page at all, finalize stops and asks you to
      acknowledge the gap first — this catches missing submissions before they silently fall
      through. Acknowledging and finalizing anyway does not fill the gap; it just proceeds without
      those cells, which you can revisit later.
    </p>
    <p>
      Student names shown while resolving pages come from the roster seeded during import — the
      same identity masking used elsewhere in ADA-Marker applies once a page becomes a graded
      submission.
    </p>
  </>
);

/** SubmissionsTab reconciliation table — the Mapped/expected column. */
export const mappedExpectedHelp: ReactNode = (
  <>
    <p>
      <strong>Expected</strong> is the number of problems in this assessment — whole-paper uploads
      assume one page per problem, in problem order. <strong>Mapped</strong> is how many pages
      actually got attached to answers.
    </p>
    <p>
      A PDF with more pages than Expected is rejected before mapping, so trailing pages cannot be
      silently discarded. A PDF with fewer pages remains visible as a red mismatch here (for
      example, a missed scanner page); click the row to inspect what mapped, then re-upload a
      corrected PDF if needed.
    </p>
  </>
);

/** SubmissionsTab upload card — the "force replace graded" checkbox (D1). */
export const forceReplaceGradedHelp: ReactNode = (
  <>
    <p>
      Normally, once a student has grades recorded, re-uploading their PDF is refused — so nobody
      silently swaps out the pages a grade was based on.
    </p>
    <p>
      Tick this only when you really mean to replace a graded student&apos;s file (say, the wrong
      scan was uploaded). The old grades are kept, but flagged as based on a replaced image so you
      know to re-check or re-grade them. For students with no grades yet, this box changes nothing.
    </p>
  </>
);

// --- regrades ---------------------------------------------------------------------------

/** AssessmentDetail problems table — the "Regrade TA" column (D60). */
export const regradeTAHelp: ReactNode = (
  <>
    <p>
      Who owns regrade requests for this problem. Students dispute published grades by email, and
      the system handles the first rounds automatically. If a student is still contesting this
      problem after the final round, the request is handed to this TA by email — with the
      complaint and the full back-and-forth so far.
    </p>
    <p>
      One TA per problem; one TA can own many problems. Leaving it unassigned blocks nothing, but
      a final-round complaint about this problem then reaches nobody — the publish preview warns
      about unassigned problems.
    </p>
  </>
);

/** Regrades page — the Upheld / Regraded verdict buttons. */
export const regradeVerdictHelp: ReactNode = (
  <>
    <p>
      <strong>Upheld</strong> = the original grade stands. <strong>Regraded</strong> = you changed
      the grade. The verdict itself never changes any score — it only records your decision for
      the result email, and your note is included in that email.
    </p>
    <p>
      To actually change a grade: open the answer and set a new official grade. Published grades
      are locked, so an admin must unpublish first; fix the grades, re-publish, and only then send
      the result — a &ldquo;regraded&rdquo; result email reports the problem&apos;s official score
      at send time as the new score.
    </p>
  </>
);

/** Regrades page — the queue kind filter vocabulary and "turn". */
export const regradeKindsHelp: ReactNode = (
  <>
    <p>Every student reply becomes one row.</p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>Filed</strong> — a valid regrade request; the only kind you adjudicate.
      </li>
      <li>
        <strong>Unparsed</strong> — the reply didn&apos;t follow the required format, so no
        attempt was used; you can send a one-time reminder from its detail pane. Rejected replies
        (bad reply address or wrong sender) also land under this kind.
      </li>
      <li>
        <strong>Addendum</strong> — an extra reply on an attempt that was already used;
        informational only, no attempt spent.
      </li>
      <li>
        <strong>Handed off</strong> — the student&apos;s final reply: their contested problems
        went by email straight to each problem&apos;s assigned TA, and the system is done with
        that thread (later replies file as addenda).
      </li>
    </ul>
    <p>
      <strong>turn N</strong> is the attempt number — the original results email and each regrade
      result email each allow one reply. Dimmed rows need no adjudication.
    </p>
  </>
);

/** Regrades page — the per-problem AI re-grade button (D53). */
export const aiRegradeHelp: ReactNode = (
  <>
    <p>
      Sends this one problem back to the same AI model that produced the contested grade, as a
      second opinion: it sees the masked answer pages, the original scores, and the
      student&apos;s complaint with identifying details removed.
    </p>
    <p>
      It grades under a fixed, stricter re-grade standard — separate from the three normal
      grading policies — that only moves a score, up or down, for a demonstrable grading error.
      It is a paid AI call and counts against the monthly budget.
    </p>
    <p>
      The result is advisory only — it appears below for comparison and never changes the
      official grade; you still decide the verdict.
    </p>
  </>
);

// --- grading records + review -----------------------------------------------------------

/** AnswerView — reading the grading history card. */
export const recordHistoryHelp: ReactNode = (
  <>
    <p>
      Each entry is one saved grade, newest first — records are only ever added, never deleted or
      overwritten. The badge shows who produced it:
    </p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>human</strong> — a TA.
      </li>
      <li>
        <strong>model</strong> — one AI grading run; the model&apos;s name sits next to the badge.
      </li>
      <li>
        <strong>aggregate</strong> — the combined result of several AI models from a Consensus
        run.
      </li>
      <li>
        <strong>regrade_ai</strong> — an AI second opinion produced while handling a regrade
        request; it never becomes official on its own.
      </li>
    </ul>
    <p>
      <strong>conf</strong> is the AI&apos;s own certainty that it could both read the
      handwriting and apply the rubric — low or illegible means check the pages yourself. The
      green-highlighted record is the official grade, the one that counts and gets published.
    </p>
  </>
);

/** AnswerView — the flag chips row. */
export const flagChipsHelp: ReactNode = (
  <>
    <p>
      Flags mark an answer for attention. They never change scores, but any flag blocks the
      exam&apos;s grading source from deciding this answer — it turns <strong>unresolved</strong>
      (red on the publish preview) until a human grades it manually or clears the flag.
    </p>
    <p>The outlined chips toggle when you click them:</p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>blank page</strong> — the student left this empty.
      </li>
      <li>
        <strong>needs review</strong> — look again later.
      </li>
      <li>
        <strong>image replaced</strong> — the page images were replaced after grading —
        re-check. The system also sets this itself when a graded student&apos;s file is force
        re-uploaded.
      </li>
    </ul>
    <p>
      The red <code className="font-mono text-xs">agg_…</code> flags are set automatically by
      Consensus runs (models disagree, too few grades, models unsure) and every new run clears
      and re-applies them — you don&apos;t remove those by hand.
    </p>
  </>
);

/** ReviewTab — what each count column counts. */
export const reviewCountsHelp: ReactNode = (
  <>
    <p>
      Each problem has one answer slot per active roster student, created when submissions are
      ingested (withdrawn students get no new slots).
    </p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>With pages</strong> — slots that actually have scanned pages; only these can be
        graded. A gap means the student submitted nothing for this problem, or its pages were
        never mapped.
      </li>
      <li>
        <strong>Official</strong> — answers whose official grade has been chosen; publishing
        requires one for every answer that has pages.
      </li>
      <li>
        <strong>AI / Human</strong> — answers with at least one AI or human grade record.
      </li>
      <li>
        <strong>Flagged</strong> — answers carrying any attention flag: set by grading runs
        (illegible or low confidence), consensus runs, a replaced scan, or by hand.
      </li>
      <li>
        <strong>Published</strong> — answers locked by publishing. The whole assessment is
        stamped the moment results are published, whether or not each student&apos;s email has
        actually gone out yet.
      </li>
    </ul>
  </>
);

/** AssessmentDetail — the Totals card columns (D3). */
export const totalsHelp: ReactNode = (
  <>
    <p>
      <strong>Answers</strong> = how many answer slots this student has — one per problem,
      created for the whole roster once uploads start, so it counts slots, not what the student
      actually submitted. <strong>Graded</strong> = how many of those already have an official
      grade — the one that counts, set by a human or accepted from the AI.
    </p>
    <p>
      <strong>Total</strong> adds up the official grades so far: a dash means nothing is official
      yet (it is never shown as 0), and while Graded is below Answers the total is a partial sum,
      not the student&apos;s final score.
    </p>
  </>
);

/** AnswerView manual card — fallback-only human grading (0027). */
export const officialGradeHelp: ReactNode = (
  <>
    <p>
      Every grade saved here becomes a permanent record; nothing is ever overwritten. Which record
      is <strong>official</strong> is derived from the exam&apos;s final grading source (Publish
      tab) — never picked by hand.
    </p>
    <p>
      A manual grade is a <strong>fallback</strong>: it takes effect only where the chosen source
      left the answer undecided (a consensus conflict, a flagged answer, a failed model call).
      Everywhere else it is recorded but ignored.
    </p>
  </>
);

/** ProblemReview — the per-answer Status column vocabulary. */
export const answerStatusHelp: ReactNode = (
  <ul className="list-disc space-y-1 pl-4">
    <li>
      <strong>no submission</strong> — the student&apos;s upload has no pages for this problem.
    </li>
    <li>
      <strong>ungraded</strong> — pages exist but no grades at all yet.
    </li>
    <li>
      <strong>graded (not official)</strong> — grades exist, but none is official: the
      exam&apos;s grading source hasn&apos;t decided this answer (or no source is chosen yet) and
      there&apos;s no manual fallback. Unresolved answers sit here.
    </li>
    <li>
      <strong>official</strong> — the grading source (or a manual fallback) settled this answer;
      ready to publish.
    </li>
    <li>
      <strong>published</strong> — this assessment&apos;s results went out with this answer
      included, so it is now locked (an admin must unpublish to change anything).
    </li>
  </ul>
);

// --- publish ----------------------------------------------------------------------------

/** PublishTab — the coverage gate and its numbers. */
export const publishCoverageHelp: ReactNode = (
  <>
    <p>
      Publishing emails every student their results, so every student × problem must first be
      accounted for: either the exam&apos;s grading source (or a manual fallback) produced an
      official grade (<strong>Graded</strong>), or the student genuinely submitted nothing
      (<strong>No submission</strong>). <strong>Coverage</strong> is the share of answers meeting
      that.
    </p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>Blocked / unresolved</strong> — answers the chosen source left undecided and no
        one has graded manually yet. The red list below names them; each links to its answer page.
      </li>
      <li>
        <strong>Not ingested</strong> — roster students who appear in no upload at all (listed
        below as &ldquo;not in any upload&rdquo;). Add their pages via the Submissions or
        Identify tab.
      </li>
    </ul>
    <p>
      Publish stays disabled until a final source is chosen, the unresolved list is empty, and —
      for an AI source — that exact selected run&apos;s spot-check sample is reviewed.
    </p>
  </>
);

// --- runs -----------------------------------------------------------------------------

/** Runs page — status vocabulary for runs and their per-answer items. */
export const runStatusHelp: ReactNode = (
  <>
    <p>
      A run moves <strong>pending</strong> (queued) → <strong>running</strong> → one of:
    </p>
    <ul className="list-disc space-y-1 pl-4">
      <li>
        <strong>completed</strong> — every answer was processed, but some may still have{" "}
        <em>failed</em>: check the counts before trusting it.
      </li>
      <li>
        <strong>cancelled / paused</strong> — someone or something stopped it early; answers not
        yet graded stay ungraded.
      </li>
      <li>
        <strong>failed</strong> — the run itself hit a fatal problem.
      </li>
    </ul>
    <p>
      Inside a run, each answer is an item: <strong>succeeded</strong> (a grade was recorded),{" "}
      <strong>failed</strong> (that answer errored — &ldquo;Retry failed&rdquo; re-queues them; a{" "}
      <code className="font-mono text-xs">budget_exceeded</code> error means the cost cap was
      reached), or <strong>skipped</strong> (the run was stopped before this answer&apos;s turn).
    </p>
    <p>
      Rule of thumb: <em>completed</em> means the run finished working, not that every grade
      succeeded.
    </p>
  </>
);

/** Runs page — the spot-check trust gate (trust spec §4, D37). */
export const spotCheckHelp: ReactNode = (
  <>
    <p>
      Before this run&apos;s AI grades can be accepted as official, you hand-review a small random
      sample of them — that&apos;s this list.
    </p>
    <p>
      For each sampled answer, <strong>Agree</strong> records that the AI&apos;s grade looks
      right; <strong>Adjust</strong> records that you&apos;d change it and opens the answer page
      where you can enter a human grade. Neither button edits a grade by itself — they only record
      your verdict.
    </p>
    <p>
      Once every sample is reviewed (or an admin waives the check, with a reason kept in the audit
      log), publishing with this run&apos;s method as the exam&apos;s final grading source
      unblocks.
    </p>
  </>
);

export const runScopeHelp: ReactNode = (
  <ul className="list-disc space-y-1 pl-4">
    <li>
      <strong>assessment</strong> — every answer with pages
    </li>
    <li>
      <strong>problem</strong> — one problem across all students
    </li>
    <li>
      <strong>answer</strong> — a single answer; handy for testing a method cheaply before a full
      run
    </li>
  </ul>
);
