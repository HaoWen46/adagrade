// Assemble deck.html from shots/ + inline SVG diagrams. 16:9, print-ready.
import { readFileSync, writeFileSync, existsSync } from "node:fs";
import path from "node:path";

const SHOTS = path.resolve("shots");
const SHOT_W = 1440; // capture viewport width

function img(name, { width = 1080, annIdx = null, tone = "indigo" } = {}) {
  const jsonPath = path.join(SHOTS, `${name}.json`);
  const anns = existsSync(jsonPath) ? JSON.parse(readFileSync(jsonPath, "utf8")).anns : [];
  const scale = width / SHOT_W;
  const height = Math.round(900 * scale);
  const chosen = annIdx === null ? anns : anns.filter((_, i) => annIdx.includes(i));
  const color = tone === "red" ? "#dc2626" : "#4f46e5";
  const overlays = chosen
    .map((a) => {
      const x = a.x * scale, y = a.y * scale, w = a.w * scale, h = a.h * scale;
      const pad = a.pad ?? 6;
      const est = a.label.length * 8.2 + 24; // label chip width estimate
      let lx, ly;
      const pos = a.pos ?? (x + w + 14 + est < width ? "right" : "above");
      if (pos === "right") { lx = x + w + 16; ly = y + h / 2 - 14; }
      else if (pos === "left") { lx = x - est - 16; ly = y + h / 2 - 14; }
      else if (pos === "inside-right") { lx = x + w - est - 10; ly = y + 8; }
      else if (pos === "corner") { lx = x + 14; ly = y - 15; }
      else if (pos === "bottom") { lx = x + 14; ly = y + h - 13; }
      else if (pos === "below") { lx = x - 6; ly = y + h + 14; }
      else { lx = x - 6; ly = y - 40; } // above
      return `
      <div style="position:absolute;left:${x - pad}px;top:${y - pad}px;width:${w + pad * 2}px;height:${h + pad * 2}px;border:3px solid ${color};border-radius:10px;box-shadow:0 0 0 3px rgba(255,255,255,.7)"></div>
      <div style="position:absolute;left:${lx}px;top:${ly}px;background:${color};color:#fff;font-size:15px;font-weight:600;padding:3px 10px;border-radius:999px;white-space:nowrap;box-shadow:0 1px 4px rgba(0,0,0,.25)">${a.label}</div>`;
    })
    .join("");
  return `<div class="shot" style="position:relative;width:${width}px;height:${height}px">
    <img src="shots/${name}.jpg" style="width:${width}px;height:${height}px;border-radius:10px;border:1px solid #e5e5e5" alt="${name}"/>
    ${overlays}
  </div>`;
}

function slide({ kicker, title, caption, body, dark = false }) {
  return `<section class="slide${dark ? " dark" : ""}">
    <header>
      ${kicker ? `<p class="kicker">${kicker}</p>` : ""}
      ${title ? `<h1>${title}</h1>` : ""}
      ${caption ? `<p class="caption">${caption}</p>` : ""}
    </header>
    <div class="body">${body ?? ""}</div>
  </section>`;
}

// ---------- diagrams ----------
const chip = (x, y, w, label, cls = "c1") =>
  `<rect x="${x}" y="${y}" width="${w}" height="46" rx="10" class="${cls}"/>
   <text x="${x + w / 2}" y="${y + 29}" text-anchor="middle" class="ct">${label}</text>`;
const arrow = (x1, y1, x2, y2) =>
  `<line x1="${x1}" y1="${y1}" x2="${x2}" y2="${y2}" class="ar" marker-end="url(#ah)"/>`;

const pipelineSvg = `
<svg viewBox="0 0 1120 380" width="1120" xmlns="http://www.w3.org/2000/svg">
  <defs><marker id="ah" markerWidth="9" markerHeight="9" refX="7" refY="4.5" orient="auto"><path d="M0,0 L8,4.5 L0,9 z" fill="#94a3b8"/></marker></defs>
  <style>
    .c1{fill:#eef2ff;stroke:#6366f1;stroke-width:1.5}.c2{fill:#f0fdf4;stroke:#16a34a;stroke-width:1.5}
    .c3{fill:#fffbeb;stroke:#d97706;stroke-width:1.5}
    .ct{font:600 17px -apple-system,sans-serif;fill:#1e293b}.lb{font:500 15px -apple-system,sans-serif;fill:#64748b}
    .st{font:600 15px -apple-system,sans-serif;fill:#4f46e5}.ar{stroke:#94a3b8;stroke-width:2}
  </style>
  ${chip(20, 60, 130, "Roster CSV")}${arrow(150, 83, 190, 83)}
  ${chip(190, 60, 190, "Problems + rubrics")}${arrow(380, 83, 420, 83)}
  ${chip(420, 60, 180, "Collect the work")}${arrow(600, 83, 640, 83)}
  ${chip(640, 60, 170, "Mask identities")}${arrow(810, 83, 850, 83)}
  ${chip(850, 60, 130, "AI grades", "c2")}
  ${arrow(915, 106, 915, 150)}
  ${chip(790, 150, 250, "Humans review + override", "c3")}
  ${arrow(790, 173, 620, 173)}
  ${chip(430, 150, 190, "Final source chosen")}${arrow(430, 173, 390, 173)}
  ${chip(200, 150, 190, "Publish → email")}
  ${arrow(295, 196, 295, 240)}
  ${chip(160, 240, 270, "Students reply for regrades", "c3")}
  ${arrow(430, 263, 620, 263)}
  ${chip(620, 240, 230, "TA verdicts, result email")}
  <text x="20" y="330" class="lb">Every AI score is reviewable and overridable · identity is masked before any image reaches a model ·</text>
  <text x="20" y="354" class="lb">nothing goes to students until the publish gate passes.</text>
  <text x="850" y="45" class="st">the only automated step</text>
</svg>`;

const regradeSvg = `
<svg viewBox="0 0 1140 420" width="1140" xmlns="http://www.w3.org/2000/svg">
  <defs><marker id="ah3" markerWidth="9" markerHeight="9" refX="7" refY="4.5" orient="auto"><path d="M0,0 L8,4.5 L0,9 z" fill="#94a3b8"/></marker></defs>
  <style>
    .c1{fill:#eef2ff;stroke:#6366f1;stroke-width:1.5}.c3{fill:#fffbeb;stroke:#d97706;stroke-width:1.5}
    .c2{fill:#f0fdf4;stroke:#16a34a;stroke-width:1.5}
    .ct{font:600 19px -apple-system,sans-serif;fill:#1e293b}.lb{font:500 17px -apple-system,sans-serif;fill:#64748b}
    .mono{font:600 18px ui-monospace,monospace;fill:#7c3aed}.ar{stroke:#94a3b8;stroke-width:2}
  </style>
  <rect x="40" y="30" width="290" height="52" rx="10" class="c1"/>
  <text x="185" y="62" text-anchor="middle" class="ct">Grade email to student</text>
  <line x1="330" y1="56" x2="390" y2="56" class="ar" marker-end="url(#ah3)"/>
  <rect x="390" y="30" width="330" height="52" rx="10" class="c3"/>
  <text x="555" y="62" text-anchor="middle" class="ct">Student replies to that email</text>
  <text x="400" y="122" class="mono">&lt;p2&gt; I proved the bound in part b … &lt;/p2&gt;</text>
  <text x="400" y="150" class="lb">the required format is printed in the grade email · full-width and uppercase tags accepted</text>
  <line x1="555" y1="164" x2="555" y2="204" class="ar" marker-end="url(#ah3)"/>
  <rect x="390" y="204" width="330" height="52" rx="10" class="c1"/>
  <text x="555" y="236" text-anchor="middle" class="ct">5-rung verification ladder</text>
  <text x="740" y="236" class="lb">token · window · sender · SPF/DKIM · rate cap</text>
  <line x1="555" y1="256" x2="555" y2="296" class="ar" marker-end="url(#ah3)"/>
  <rect x="205" y="296" width="240" height="52" rx="10" class="c2"/>
  <text x="325" y="328" text-anchor="middle" class="ct">AI regrade assist</text>
  <rect x="475" y="296" width="290" height="52" rx="10" class="c3"/>
  <text x="620" y="328" text-anchor="middle" class="ct">TA verdict per problem</text>
  <line x1="765" y1="322" x2="825" y2="322" class="ar" marker-end="url(#ah3)"/>
  <rect x="825" y="296" width="290" height="52" rx="10" class="c1"/>
  <text x="970" y="328" text-anchor="middle" class="ct">Result email — a TA click</text>
  <text x="205" y="392" class="lb">the assist sees masked images and redacted text, and is never auto-official</text>
  <text x="205" y="416" class="lb">after the turn cap, the thread hands off person-to-person to the problem's TA</text>
</svg>`;

// Who-does-what matrix. Ticks/dashes are derived from the real requireRole gates
// in internal/httpapi/api.go (TA < lecturer < admin); keep them in sync if the
// route guards change.
const ROLE_ROWS = [
  ["Set up problems &amp; rubrics", 0, 1, 1],
  ["Roster, providers, methods, pricing", 0, 1, 1],
  ["Scan intake · mask · launch AI · review", 1, 1, 1],
  ["Choose final source · publish", 0, 1, 1],
  ["Regrade verdicts · result emails", 1, 1, 1],
  ["Assign regrade TAs · round methods", 0, 1, 1],
  ["Unpublish · delete · manage users", 0, 0, 1],
];
const roleCell = (on) =>
  on
    ? `<td style="text-align:center;color:#16a34a;font-size:20px;font-weight:700">✓</td>`
    : `<td style="text-align:center;color:#cbd5e1;font-size:20px">—</td>`;
const rolesTable = `
  <table style="width:100%;border-collapse:collapse;font-size:18px">
    <thead>
      <tr style="border-bottom:2px solid #e2e8f0">
        <th style="text-align:left;padding:10px 8px;color:#64748b;font-weight:600">Task</th>
        <th style="width:150px;text-align:center;padding:10px 8px;color:#0f172a">TA</th>
        <th style="width:150px;text-align:center;padding:10px 8px;color:#0f172a">Lecturer</th>
        <th style="width:150px;text-align:center;padding:10px 8px;color:#0f172a">Admin</th>
      </tr>
    </thead>
    <tbody>
      ${ROLE_ROWS.map(
        ([task, ta, lec, adm]) => `
        <tr style="border-bottom:1px solid #f1f5f9">
          <td style="padding:11px 8px;color:#1e293b">${task}</td>
          ${roleCell(ta)}${roleCell(lec)}${roleCell(adm)}
        </tr>`,
      ).join("")}
    </tbody>
  </table>
  <p class="foot">Roles are ranked: a lecturer can do everything a TA can, an admin everything a lecturer can. The signed-in role is shown top-right on every screen.</p>`;

const dataBoundarySvg = `
<svg viewBox="0 0 1140 430" width="1120" xmlns="http://www.w3.org/2000/svg">
  <defs><marker id="ah4" markerWidth="9" markerHeight="9" refX="7" refY="4.5" orient="auto"><path d="M0,0 L8,4.5 L0,9 z" fill="#94a3b8"/></marker></defs>
  <style>
    .box{fill:#f8fafc;stroke:#cbd5e1;stroke-width:1.5}.c2{fill:#f0fdf4;stroke:#16a34a;stroke-width:1.5}
    .c1{fill:#eef2ff;stroke:#6366f1;stroke-width:1.5}
    .h{font:700 19px -apple-system,sans-serif;fill:#0f172a}.li{font:500 16px -apple-system,sans-serif;fill:#475569}
    .ar{stroke:#94a3b8;stroke-width:2}.tag{font:600 15px -apple-system,sans-serif;fill:#4f46e5}
  </style>
  <rect x="20" y="40" width="420" height="330" rx="14" class="box"/>
  <text x="44" y="78" class="h">Your self-hosted server</text>
  <text x="44" y="118" class="li">• Roster: names, IDs, emails</text>
  <text x="44" y="150" class="li">• Original scanned exams</text>
  <text x="44" y="182" class="li">• Manual grades &amp; overrides</text>
  <text x="44" y="214" class="li">• Provider API keys — encrypted</text>
  <text x="44" y="246" class="li">• The final grade ledger</text>
  <text x="44" y="300" class="tag">Nothing here leaves the box</text>
  <text x="44" y="326" class="tag">except the two arrows →</text>

  <line x1="440" y1="150" x2="620" y2="150" class="ar" marker-end="url(#ah4)"/>
  <text x="452" y="138" class="tag">masked images only</text>
  <rect x="620" y="70" width="500" height="150" rx="14" class="c2"/>
  <text x="644" y="108" class="h">AI provider (the model you chose)</text>
  <text x="644" y="146" class="li">Sees: masked answer images + rubric text.</text>
  <text x="644" y="178" class="li">Never sees: names, IDs, emails, or who</text>
  <text x="644" y="200" class="li">the page belongs to.</text>

  <line x1="440" y1="290" x2="620" y2="290" class="ar" marker-end="url(#ah4)"/>
  <text x="452" y="278" class="tag">after the publish gate</text>
  <rect x="620" y="250" width="500" height="120" rx="14" class="c1"/>
  <text x="644" y="288" class="h">Students</text>
  <text x="644" y="326" class="li">Receive: their own graded result email,</text>
  <text x="644" y="348" class="li">only once you publish. Replies come back as regrades.</text>
</svg>`;

// ---------- slides ----------
const slides = [];

slides.push(slide({
  title: "ADA-Marker",
  caption: "",
  dark: true,
  body: `
   <p style="font-size:30px;color:#c7d2fe;margin:4px 0 34px;max-width:900px">AI-assisted grading for handwritten exams — humans stay in charge.</p>
   <div style="display:flex;gap:12px;flex-wrap:wrap;max-width:900px">
     ${["Roster", "Rubrics", "Scan intake", "Masking", "AI grading", "Review", "Publish", "Regrades"]
       .map((s) => `<span style="background:#312e81;color:#e0e7ff;padding:8px 18px;border-radius:999px;font-size:19px;font-weight:600">${s}</span>`)
       .join("")}
   </div>
   <p style="position:absolute;bottom:56px;font-size:19px;color:#818cf8">A walkthrough for the teaching team · July 2026</p>`,
}));

slides.push(slide({
  kicker: "The idea",
  title: "AI does the reading, you make the calls",
  caption: "One self-hosted app: upload scanned exams, the AI grades per rubric criterion, you review and publish.",
  body: pipelineSvg,
}));

slides.push(slide({
  kicker: "The team",
  title: "Who does what",
  caption: "Three roles. Every TA can grade and handle appeals; setup and publishing need a lecturer; a few destructive actions are admin-only.",
  body: rolesTable,
}));

slides.push(slide({
  kicker: "The workspace",
  title: "One exam, five stages",
  caption: "The tab bar mirrors the workflow. Views live as pills inside each stage — you can't get lost.",
  body: img("stage-nav", { width: 1080 }),
}));

slides.push(slide({
  kicker: "The workspace",
  title: "Overview tells you what's next",
  caption: "Every exam opens on a live checklist: what's done, what's blocked, and the button that moves it forward.",
  body: img("overview-ready", { width: 1010 }),
}));

slides.push(slide({
  kicker: "Setup · once per semester",
  title: "Roster: import, then sync by diff",
  caption: "UTF-8 CSV (student_id, name, email). Re-import after every add/drop — the diff proposes who to withdraw and who to reinstate, but import never changes anyone by itself. Withdrawing never deletes: grades and regrade rights stay.",
  body: img("students-diff", { width: 1010, tone: "red" }),
}));

slides.push(slide({
  kicker: "Setup · once per semester",
  title: "Providers, models, and prices",
  caption: "Paste an API key, list models, test it, enter $/M-token prices — they power live cost estimates and the monthly cap.",
  body: img("providers-pricing", { width: 1080 }),
}));

slides.push(slide({
  kicker: "Setup · once per semester",
  title: "A grading method = model + policy",
  caption: "Pick how strict the AI should be — the policies are written in plain language. The rubric itself never changes.",
  body: img("method-dialog", { width: 1010 }),
}));

slides.push(slide({
  kicker: "Per exam · prepare",
  title: "Problems and rubrics",
  caption: "Criteria must sum to the max — the badge watches as you type. Rubrics are versioned; grades reference their version.",
  body: img("rubric-editor", { width: 1080 }),
}));

slides.push(slide({
  kicker: "Per exam · collect",
  title: "Two ways in: pick by what you have",
  caption: "Pre-sorted per-student PDFs → Submissions. Anything that came off a scanner in one pile → Identify.",
  body: img("submissions", { width: 1080 }),
}));

slides.push(slide({
  kicker: "Per exam · collect",
  title: "Scan intake: the pile sorts itself",
  caption: "Draw the ID boxes once, upload the pile. OCR matches ID + name against the roster — both must agree, or nothing is assigned.",
  body: img("identify-top", { width: 1080 }),
}));

slides.push(slide({
  kicker: "Per exam · collect",
  title: "Leftovers go to a queue, not a black hole",
  caption: "Unmatched pages wait in the orphan queue with cropped hints. Assign, park, or discard — the matrix shows every gap.",
  body: img("orphan-queue", { width: 1080 }),
}));

slides.push(slide({
  kicker: "Per exam · collect",
  title: "Bad uploads land in quarantine, not the void",
  caption: "A file whose name doesn't match one active roster student is held aside with a plain-language reason — assign it to a student or discard it. It never silently becomes someone's grade.",
  body: img("quarantine", { width: 1080 }),
}));

slides.push(slide({
  kicker: "Per exam · protect",
  title: "Names are masked before the AI looks",
  caption: "ID/name regions are hidden on every page. You approve each mask (or accept all pending) — unapproved pages block grading.",
  body: img("masking", { width: 1080 }),
}));

slides.push(slide({
  kicker: "Per exam · protect",
  title: "Where student data actually goes",
  caption: "The hardest question first: yes, you send answers to an AI company — but only masked images, and only after you approve the masks.",
  body: dataBoundarySvg,
}));

slides.push(slide({
  kicker: "Per exam · grade",
  title: "Launch a run, see the cost first",
  caption: "Scope × method × cost cap. The estimate uses your prices; a monthly budget backstops everything.",
  body: img("launch-dialog", { width: 1010 }),
}));

slides.push(slide({
  kicker: "Guardrails",
  title: "It warns before you waste a run",
  caption: "Stranded scan pages, missing rubrics, disabled providers, unmasked pages — surfaced at the moment you click, not after.",
  body: img("launch-warnings", { width: 1010, tone: "red" }),
}));

slides.push(slide({
  kicker: "The bottom line",
  title: "What a real exam costs you",
  caption: "The AI does the reading cheaply; your time goes to judgment, not transcription.",
  body: `
    <div style="display:grid;grid-template-columns:1fr 1fr;gap:28px;width:100%;margin-top:10px">
      <div style="background:#f0fdf4;border:1px solid #bbf7d0;border-radius:14px;padding:26px 28px">
        <p style="font-size:16px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:#16a34a;margin-bottom:10px">The AI bill</p>
        <p style="font-size:64px;font-weight:800;color:#0f172a;line-height:1">≈ $0.50</p>
        <p style="font-size:18px;color:#475569;margin-top:12px;line-height:1.5">A 150-student exam, 4 problems — 600 answers — at the demo's qwen3-vl-plus rate of <strong>$0.0008 / answer</strong> (a real measured run). A monthly budget cap backstops it, and every launch shows the estimate first.</p>
      </div>
      <div style="background:#f8fafc;border:1px solid #e2e8f0;border-radius:14px;padding:26px 28px">
        <p style="font-size:16px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:#6366f1;margin-bottom:14px">Your time</p>
        <ul style="list-style:none;font-size:18px;color:#1e293b;line-height:1.9">
          <li>◦ Draw the ID/mask boxes <strong>once</strong> per exam</li>
          <li>◦ Accept masks — one click for all pending</li>
          <li>◦ Spot-check, plus review what the AI <strong>flags</strong></li>
          <li>◦ Choose the final source &amp; publish</li>
          <li>◦ Verdicts only for students who actually reply</li>
        </ul>
      </div>
    </div>
    <p class="foot">Numbers from a real demo run; your model choice and class size scale them. A stricter or reasoning model costs more per answer — the estimate always reflects your own prices.</p>`,
}));

slides.push(slide({
  kicker: "Per exam · review",
  title: "Review: the page and the reasoning, side by side",
  caption: "Each criterion gets a score and a justification you can check against the handwriting. Override or flag anything.",
  body: img("answer-view", { width: 1010 }),
}));

slides.push(slide({
  kicker: "Per exam · compare",
  title: "Method report cards, disagreement included",
  caption: "One card per method — hand-grade agreement, overrides, confidence, cost — then every answer where methods split, linked for a spot-check. Expand any problem row for its full statistics, agreement, and score distribution.",
  body: img("analysis-cards", { width: 1010 }),
}));

slides.push(slide({
  kicker: "Per exam · decide",
  title: "Nothing is official until you choose the source",
  caption: "One method (or a consensus) grades the exam; manual grades fill gaps. Methods that never graded this exam are disabled, not hidden.",
  body: img("final-source", { width: 1010 }),
}));

slides.push(slide({
  kicker: "Per exam · publish",
  title: "Publish is gated, and the gate explains itself",
  caption: "100% coverage required. Blockers name the student and link to the fix; honest warnings list what you're accepting.",
  body: img("publish-blocked", { width: 1010, tone: "red" }),
}));

slides.push(slide({
  kicker: "Per exam · publish",
  title: "Totals, export, and the paper trail",
  caption: "Per-student totals with sources; CSV export carries a status column (active/withdrawn). Result PDFs attach to grade emails.",
  body: img("totals", { width: 1080 }),
}));

slides.push(slide({
  kicker: "After publish",
  title: "Regrades arrive by email, verdicts stay with you",
  body: regradeSvg + `<p class="foot">Students never trigger AI regrades themselves; every result email is a TA click.</p>`,
}));

slides.push(slide({
  kicker: "After publish",
  title: "Rounds keep multi-problem appeals organized",
  caption: "Each contested problem gets its own AI assist and TA verdict; the result email unlocks only when every verdict is in.",
  body: img("regrade-rounds", { width: 1080 }),
}));

slides.push(slide({
  kicker: "After publish",
  title: "The inbox: triage on the left, verdicts on the right",
  caption: "Cross-exam queue of filed requests; recovery banners surface held, unparsed, and undelivered replies. Open one and each contested problem gets its own AI assist and Upheld/Regraded verdict.",
  body: img("regrade-detail", { width: 1120 }),
}));

slides.push(slide({
  kicker: "Honesty slide",
  title: "Not done yet",
  body: `<div class="gaps">
    <div><h3>One page per problem</h3><p>A student writing one problem across two sheets can't be represented — the second sheet parks as a conflict.</p></div>
    <div><h3>Two TAs, one answer</h3><p>Concurrent manual grading is last-write-wins. Split problems between people, not students.</p></div>
    <div><h3>Fixing one score after publish</h3><p>Requires an admin unpublishing the whole exam, editing, re-publishing. Plan regrade windows accordingly.</p></div>
    <div><h3>Semester reset</h3><p>No archive/new-semester button yet — the import diff + bulk withdraw is the workaround.</p></div>
    <div><h3>Excel Big5 files</h3><p>Rejected with instructions (re-save as CSV UTF-8) rather than converted automatically.</p></div>
    <div><h3>Typos are forever</h3><p>A mistyped student row can be withdrawn but never deleted.</p></div>
  </div>`,
}));

slides.push(slide({
  kicker: "Getting started",
  title: "The Guide inside the app covers the rest",
  caption: "Setup order, the scan-intake walkthrough, roster-through-the-semester, and a glossary — plus ? tips on every control.",
  body: img("guide", { width: 1010 }),
}));

// ---------- html ----------
const html = `<!doctype html>
<html><head><meta charset="utf-8"><title>ADA-Marker — demo deck</title>
<style>
  * { margin:0; padding:0; box-sizing:border-box; }
  @page { size: 1280px 720px; margin: 0; }
  body { font-family: -apple-system, "PingFang TC", sans-serif; }
  .slide { width:1280px; height:720px; page-break-after:always; padding:44px 56px; position:relative; overflow:hidden; background:#fff; }
  .slide.dark { background:#1e1b4b; color:#fff; display:flex; flex-direction:column; justify-content:center; }
  .slide.dark h1 { color:#fff; font-size:76px; }
  .kicker { font-size:16px; font-weight:700; letter-spacing:.12em; text-transform:uppercase; color:#6366f1; margin-bottom:6px; }
  h1 { font-size:36px; font-weight:700; color:#0f172a; letter-spacing:-.01em; }
  .caption { font-size:19px; color:#475569; margin-top:8px; max-width:1100px; }
  .body { margin-top:20px; display:flex; flex-direction:column; align-items:center; }
  .shot { margin:0 auto; }
  .foot { font-size:17px; color:#64748b; margin-top:14px; }
  .gaps { display:grid; grid-template-columns:1fr 1fr 1fr; gap:22px 28px; width:100%; margin-top:18px; }
  .gaps h3 { font-size:21px; color:#0f172a; margin-bottom:8px; }
  .gaps p { font-size:17px; color:#475569; line-height:1.45; }
  .gaps > div { background:#f8fafc; border:1px solid #e2e8f0; border-radius:12px; padding:20px 22px; }
</style></head>
<body>
${slides.join("\n")}
</body></html>`;

writeFileSync("deck.html", html);
console.log(`built deck.html with ${slides.length} slides`);
