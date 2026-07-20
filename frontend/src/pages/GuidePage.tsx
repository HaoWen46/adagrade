import type { ReactNode } from "react";

import { Card } from "../components/ui";

/**
 * Static usage guide (spec 2026-07-04-guide-page-demo-data-design.md).
 * Content only — no queries, no mutations. Anchored sections so /guide#identify
 * style links work from anywhere.
 */

const PIPELINE = [
  "Roster",
  "Assessment & rubrics",
  "Collect work",
  "Mask",
  "AI grade",
  "Review",
  "Publish",
  "Regrades",
];

function Step({ children }: { children: ReactNode }) {
  return (
    <li className="ml-5 list-decimal pl-1 leading-relaxed marker:font-semibold marker:text-indigo-600">
      {children}
    </li>
  );
}

function Term({ name, children }: { name: string; children: ReactNode }) {
  return (
    <div className="grid gap-1 border-b border-neutral-100 py-2 last:border-b-0 sm:grid-cols-[11rem_1fr] sm:gap-4">
      <dt className="text-sm font-semibold text-neutral-800">{name}</dt>
      <dd className="text-sm text-neutral-600">{children}</dd>
    </div>
  );
}

function Anchor({ id, children }: { id: string; children: ReactNode }) {
  return (
    <section id={id} className="scroll-mt-4">
      {children}
    </section>
  );
}

export function GuidePage() {
  return (
    <div className="max-w-4xl space-y-4">
      <Anchor id="overview">
        <Card title="What AdaGrade is">
          <div className="space-y-4 text-sm text-neutral-700">
            <p>
              AdaGrade grades handwritten exams and assignments with AI assistance while
              keeping humans in charge: rubrics are explicit, every AI score is reviewable and
              overridable, student identity is masked before any image reaches a vision model,
              and nothing goes out to students until a publish gate passes. The typical journey
              through the system:
            </p>
            <div className="flex flex-wrap items-center gap-1.5">
              {PIPELINE.map((step, i) => (
                <span key={step} className="flex items-center gap-1.5">
                  <span className="rounded-md bg-indigo-50 px-2.5 py-1 text-xs font-semibold text-indigo-700 ring-1 ring-indigo-200 ring-inset">
                    {step}
                  </span>
                  {i < PIPELINE.length - 1 && <span className="text-neutral-400">→</span>}
                </span>
              ))}
            </div>
            <p>
              Inside an assessment, the <span className="font-semibold">Overview</span> tab (the
              default) shows this same pipeline as a live checklist — each step with its current
              status and a link to the tab that moves it forward. The views are grouped into five
              stages — <span className="font-semibold">Overview · Problems · Student work ·
              Grading · Results</span> — and the individual tabs appear as pills inside the active
              stage.
            </p>
            <p>
              Look for the <span className="font-semibold">?</span> help tips throughout the app —
              they explain each control in place. This page is the map; the tips are the street
              signs.
            </p>
          </div>
        </Card>
      </Anchor>

      <Anchor id="setup">
        <Card title="One-time setup">
          <ol className="space-y-3 text-sm text-neutral-700">
            <Step>
              <span className="font-semibold">Import the roster</span> on the{" "}
              <span className="font-semibold">Students</span> page — a UTF-8 CSV with header{" "}
              <code className="rounded bg-neutral-100 px-1">student_id,name,email</code> (in Excel:
              Save As → &ldquo;CSV UTF-8 (Comma delimited)&rdquo;). The student ID is the identity
              key everywhere (filenames, scan headers, matching). Re-importing upserts; it never
              deletes — see <span className="font-semibold">Roster through the semester</span>{" "}
              below for add/drop and 停修 withdrawals.
            </Step>
            <Step>
              <span className="font-semibold">Add providers</span> on the{" "}
              <span className="font-semibold">Providers</span> page: an API key, the models you
              plan to use, and their per-model prices ($ per million input/output tokens, under
              each provider&apos;s Pricing section — prices power run cost estimates and the
              monthly budget cap). Use the built-in Test button before relying on one. Providers
              serve both grading and scan OCR.
            </Step>
            <Step>
              <span className="font-semibold">Create grading methods</span> on the{" "}
              <span className="font-semibold">Methods</span> page: a provider + model + prompt
              template + policy. The policy (lenient / standard / strict) tunes judgment under
              ambiguity only — the rubric itself never changes. You can compare methods
              side-by-side later in Analysis.
            </Step>
          </ol>
        </Card>
      </Anchor>

      <Anchor id="roster">
        <Card title="Roster through the semester">
          <div className="space-y-3 text-sm text-neutral-700">
            <p>
              The roster is not a one-shot import — add/drop rounds, mid-semester 停修 withdrawals,
              and retakers all happen while grading is underway. The rule throughout: importing a
              CSV <span className="font-semibold">never withdraws or reinstates anyone by
              itself</span>; every roster change to a student&apos;s status is an explicit click.
            </p>
            <ol className="space-y-2">
              <Step>
                <span className="font-semibold">After each add/drop round</span>, re-export the
                class list (NTU COOL / registrar) and re-import it. New students are added, changed
                names/emails are updated, and the import result shows a{" "}
                <span className="font-semibold">diff</span>: how many active students are missing
                from the CSV, and how many withdrawn students appear in it.
              </Step>
              <Step>
                <span className="font-semibold">Dropped students</span> show up as &ldquo;active
                but not in this CSV&rdquo;. Review the list, then use{" "}
                <span className="font-semibold">Withdraw all</span> to sync — withdrawing keeps
                their submissions, grades, and history, but excludes them from new grading runs and
                new publish batches, and their ungraded answers stop blocking publish. They stay in
                totals and the grades export with a <span className="font-semibold">withdrawn</span>{" "}
                marker, and their regrade rights on already-published results remain open.
              </Step>
              <Step>
                <span className="font-semibold">Retakers:</span> a student who withdrew in an
                earlier semester and re-enrolls appears in the new CSV while still marked withdrawn
                here — re-import alone won&apos;t reactivate them. The diff flags them
                (&ldquo;withdrawn students are in this CSV&rdquo;) with a{" "}
                <span className="font-semibold">Reinstate all</span> button; until reinstated they
                get no new answer rows, no runs, and no result emails.
              </Step>
              <Step>
                <span className="font-semibold">Late adds:</span> students who join after an
                assessment&apos;s uploads started have no answer rows yet, so they surface as a
                warning on that assessment — create their answer rows from the Publish tab (or
                upload their work) once they exist in the roster.
              </Step>
              <Step>
                <span className="font-semibold">Email changes</span> in a re-import are counted in
                the result. Mind students with open regrade threads: replies are verified against
                the <em>current</em> roster email, so changing an address mid-thread means their
                next reply must come from the new address.
              </Step>
            </ol>
          </div>
        </Card>
      </Anchor>

      <Anchor id="workflow">
        <Card title="The per-assessment workflow">
          <ol className="space-y-3 text-sm text-neutral-700">
            <Step>
              <span className="font-semibold">Problems &amp; rubrics.</span> Define each problem
              with max points, then write rubric criteria — criterion points must sum exactly to
              the max. Rubrics are versioned; records remember which version graded them. Write a{" "}
              <span className="font-semibold">reference solution</span> per problem too: the
              default grading methods grade against one, and launching a run is refused while any
              problem in its scope lacks it — cheaper to add now than at launch time.
            </Step>
            <Step>
              <span className="font-semibold">Collect student work</span> — two paths:
              <ul className="mt-2 ml-4 list-disc space-y-2">
                <li>
                  <span className="font-semibold">Submissions tab</span> — for files already
                  sorted per student: PDFs named{" "}
                  <code className="rounded bg-neutral-100 px-1">&lt;student_id&gt;.pdf</code>, one
                  per student, page <em>i</em> mapping to problem <em>i</em>. Unmatched files land
                  in quarantine for manual assignment.
                </li>
                <li>
                  <span className="font-semibold">Identify tab</span> — for scanner piles: loose
                  pages in any order, where every page carries three handwritten header boxes
                  (student ID, name, problem number like{" "}
                  <code className="rounded bg-neutral-100 px-1">Q1</code>). Draw the three regions
                  once, upload the whole pile (multi-GB PDFs or a zip of images), and the system
                  reads each page and places it on a student × problem cell. See the details
                  below.
                </li>
              </ul>
              <p className="mt-2">
                Either way, a warning flags pages whose PDF text did not survive rendering
                (usually a non-embedded CJK font) — the AI grades the rendered image, so compare
                it against the original and re-export or rescan the affected file.
              </p>
            </Step>
            <Step>
              <span className="font-semibold">Masking.</span> Identity is hidden before AI sees
              anything: draw mask rectangles (scan intake seeds the ID and name boxes
              automatically), apply, then review the masked pages — one by one, or accept all
              pending at once after spot-checking a few. The review gate blocks AI runs until
              every page is approved. Editing the regions later invalidates pages accepted under
              the old ones — they fall back to pending and must be re-reviewed.
            </Step>
            <Step>
              <span className="font-semibold">AI runs &amp; Review.</span> Calibrate first:
              Overview&apos;s <em>Start calibration run</em> grades a stratified sample of N
              answers in one run (scope &ldquo;calibration sample&rdquo;) — hand-grade a few of
              the same answers and compare on Analysis before grading the whole class. Then start
              the class-wide run (method × problems × students) from the Overview tab (Start AI
              grading) or the <span className="font-semibold">Runs</span> page; watch it in Runs.
              Then work the Review tab per problem: spot-check the AI&apos;s grades, add manual
              grades (fallbacks where the final source leaves an answer undecided), flag — or
              switch the problem list to <em>By score</em> to compare same-score answers side by
              side for consistency. Grades become official only once a final grading source is
              chosen on Publish. Costs are tracked per record; budget caps stop runaway spend.
            </Step>
            <Step>
              <span className="font-semibold">Consensus (optional).</span> If several methods
              graded the same work, aggregate them (majority or mean with fault tolerance) —
              pure post-processing over existing records, no new API calls.
            </Step>
            <Step>
              <span className="font-semibold">Totals &amp; Publish.</span> On the Publish tab,
              first choose the <span className="font-semibold">final grading source</span> (one AI
              run or consensus) — no grade is official until one is applied. Totals then shows the
              final per-student picture. Publish snapshots the results and emails each student
              their result PDF; a coverage gate blocks publishing while any student is ungraded or
              any answer unofficial, and for a method source a spot-check gate also holds until a
              sample of the run&apos;s grades has been reviewed on the Runs page (an admin can
              waive it). Publishing locks official scores.
            </Step>
            <Step>
              <span className="font-semibold">Regrades.</span> Students reply to their result
              email naming problems in{" "}
              <code className="rounded bg-neutral-100 px-1">&lt;p1&gt;…&lt;/p1&gt;</code> blocks; each
              reply becomes a turn with per-problem sub-items, optional AI re-grade assist, and a
              TA verdict. Adjudicate replies in the sidebar&apos;s{" "}
              <span className="font-semibold">Regrade inbox</span>; the deadline and per-round
              grading live on the assessment&apos;s{" "}
              <span className="font-semibold">Regrade rounds</span> tab. After the final turn the
              thread hands off to the problem&apos;s assigned TA. Once a turn&apos;s result email
              is sent, its verdicts and notes are locked.
            </Step>
          </ol>
        </Card>
      </Anchor>

      <Anchor id="identify">
        <Card title="Scan intake (Identify tab) in detail">
          <div className="space-y-3 text-sm text-neutral-700">
            <p>
              Built for the real exam situation: 250+ papers fed through a scanner as one giant
              PDF of loose pages. The unit is the <em>page</em>, and every page must carry the
              three header boxes.
            </p>
            <ol className="space-y-2">
              <Step>
                <span className="font-semibold">Draw the three regions</span> (student ID, name,
                problem) on a template image. The image never leaves your browser. Regions apply
                to every page — draw them final before uploading.
              </Step>
              <Step>
                <span className="font-semibold">Upload the pile.</span> PDFs and/or a zip of
                images. Pages are split, rendered, and their three crops read by a ladder that
                starts local: the server&apos;s own OCR model first (if installed — the tab warns
                when it isn&apos;t; whoever runs the server installs it with{" "}
                <code className="rounded bg-neutral-100 px-1">make ocr-models</code>), then
                optionally the cloud. The &ldquo;Send unmatched IDs to the cloud AI model&rdquo;
                checkbox is <span className="font-semibold">off by default</span> — crops leave
                the machine only when you tick it and explicitly pick a provider. With neither
                rung, every page goes to the orphan queue for manual identification. Pages that
                error during processing have batch-level recovery: retry them all (optionally on
                a different provider) or discard them.
              </Step>
              <Step>
                <span className="font-semibold">Auto-assign is deliberately strict:</span> a page
                places itself only when the ID and the name independently agree on the same
                student <em>and</em> the problem number is valid. Anything less waits in the{" "}
                <span className="font-semibold">orphan queue</span>, which leads with its best
                roster-resolved guess: a matched ID shows that student&apos;s roster name, and an
                ID one character off a unique roster entry is labeled &ldquo;closest roster
                match&rdquo;. Guesses are never auto-assigned — you confirm each page, picking
                the student from the roster search (there is no free-text ID entry) — keyboard
                through it (j/k, Enter).
              </Step>
              <Step>
                <span className="font-semibold">Nothing is ever overwritten.</span> A page that
                collides with an occupied cell parks as a duplicate (discard it) or a conflict
                (side-by-side chooser). Re-uploading the same pile only fills cells that are still
                empty.
              </Step>
              <Step>
                <span className="font-semibold">The matrix</span> (students × problems) is the
                truth view: what is placed, promoted, covered by a direct submission, or still
                missing.
              </Step>
              <Step>
                <span className="font-semibold">Finalize</span> turns assigned pages into real
                submissions. It is incremental — finalize early, keep resolving orphans, finalize
                again; only new pages promote. Missing cells must be acknowledged explicitly.
              </Step>
            </ol>
          </div>
        </Card>
      </Anchor>

      <Anchor id="components">
        <Card title="Component reference">
          <dl>
            <Term name="Assessments">
              The home list. Each assessment (exam or homework) owns problems, rubrics,
              submissions, runs, and its publish state — all under its tabs.
            </Term>
            <Term name="Students">
              The global roster. CSV import (UTF-8, upsert by student ID) with an add/drop diff and
              bulk withdraw/reinstate, plus per-student withdrawal. Withdrawn students stop
              blocking publish and are excluded from new runs and batches, but stay in exports with
              a withdrawn marker and keep their regrade rights.
            </Term>
            <Term name="Providers">
              LLM endpoints and keys (encrypted at rest), model lists, per-model pricing, rate
              limits. Feeds grading, scan OCR, and regrade assist.
            </Term>
            <Term name="Methods">
              Reusable grading configurations: provider + model + versioned prompt template +
              policy stance.
            </Term>
            <Term name="Runs">
              Every AI grading run with progress, per-item outcomes, retry of failures, and cost.
            </Term>
            <Term name="Regrade inbox">
              The cross-assessment regrade inbox (sidebar): every student appeal turn, its
              sub-items, verdicts, and result-email state.
            </Term>
            <Term name="Overview tab">
              The assessment&apos;s default tab: the grading pipeline as a live numbered
              checklist — each step&apos;s current status plus the link that moves it forward,
              including the Start AI grading button.
            </Term>
            <Term name="Problems tab">
              Problem definitions, rubric editor, per-problem review entry, regrade-TA assignment.
            </Term>
            <Term name="Submissions tab">
              Direct per-student PDF/image intake, upload status, per-student reconciliation, and
              quarantine.
            </Term>
            <Term name="Identify tab">
              Page-level scan intake: regions, batches, matrix, orphan queue, parked pages,
              finalize.
            </Term>
            <Term name="Masking tab">
              Mask regions, per-page masked-image review gate (accept pages one by one, or all
              pending at once). Blocks runs until reviewed.
            </Term>
            <Term name="Review tab">
              Per-problem grading review: AI records vs. the official grade, manual fallback
              grades, flags. Warns when grades exist but no final grading source is chosen.
            </Term>
            <Term name="Consensus tab">
              Aggregation policy over multiple methods&apos; records; writes aggregate records and
              flags disagreement.
            </Term>
            <Term name="Analysis tab">
              Per-problem × method statistics and human-agreement comparison; policy and cost
              columns.
            </Term>
            <Term name="Regrade rounds tab">
              Per-assessment regrade configuration: the reply deadline, one grading method per
              round, per-round work counts, and batch re-grading. Individual replies are
              adjudicated in the Regrade inbox.
            </Term>
            <Term name="Totals tab">
              Final per-student totals across problems, with official/fallback provenance.
            </Term>
            <Term name="Publish tab">
              Final grading source chooser, coverage checks, snapshot + email send, batch
              history, unpublish/re-publish for corrections.
            </Term>
          </dl>
        </Card>
      </Anchor>

      <Anchor id="demo">
        <Card title="Try it with the demo data">
          <div className="space-y-3 text-sm text-neutral-700">
            <p>
              The repo ships a synthetic dataset under{" "}
              <code className="rounded bg-neutral-100 px-1">data/demo/</code> (regenerate anytime
              with <code className="rounded bg-neutral-100 px-1">make demo-data</code>): a
              10-student roster, an exam paper, and a 40-page shuffled scan pile whose answers are
              deliberately mixed — solid, wrong, too short, blank, and off-topic — so grading and
              review have something to disagree about. Nothing in it is a real person.
            </p>
            <ol className="space-y-2">
              <Step>
                Students page → import{" "}
                <code className="rounded bg-neutral-100 px-1">demo-roster.csv</code>.
              </Step>
              <Step>
                Create an assessment (kind: exam) with problems 1–4, 10 points each — the
                statements are in{" "}
                <code className="rounded bg-neutral-100 px-1">demo-exam.pdf</code> pages 2–5.
                Draft a small rubric per problem.
              </Step>
              <Step>
                Identify tab → export page 1 of the exam PDF as an image (any PDF viewer) and pick
                it as the local template, then draw the three boxes over the printed ones. Save.
              </Step>
              <Step>
                Upload <code className="rounded bg-neutral-100 px-1">demo-scan-pile.pdf</code>.
                With a provider configured, tick the cloud checkbox (it&apos;s off by default)
                and watch most pages auto-assign; leave it off on a server without local OCR and
                every page lands in the orphan queue for manual placement — both paths are worth
                trying once.
              </Step>
              <Step>Resolve the orphans, check the matrix fills up, Finalize (ack any gaps).</Step>
              <Step>
                Masking tab → apply and review; then run a method over the assessment and work the
                Review tab. The blank and garbled pages exercise the illegible/flag paths; the
                wrong answers give the rubric something to catch.
              </Step>
            </ol>
          </div>
        </Card>
      </Anchor>
    </div>
  );
}
