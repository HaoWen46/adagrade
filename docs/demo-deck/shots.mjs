// Capture annotated screenshots of ADA-Marker for the demo deck.
// Usage: node shots.mjs [only-shot-name]
import puppeteer from "puppeteer-core";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";

const BASE = "http://localhost:8899";
const OUT = path.resolve("shots");
mkdirSync(OUT, { recursive: true });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// find an element whose trimmed textContent matches, return its handle
async function byText(page, selector, text, { exact = true } = {}) {
  const handle = await page.evaluateHandle(
    (sel, txt, ex) => {
      const els = [...document.querySelectorAll(sel)];
      return (
        els.find((e) => (ex ? e.textContent.trim() === txt : e.textContent.includes(txt))) ?? null
      );
    },
    selector,
    text,
    exact,
  );
  return handle.asElement();
}

async function clickText(page, selector, text, opts) {
  const el = await byText(page, selector, text, opts);
  if (!el) throw new Error(`clickText: not found ${selector} "${text}"`);
  await el.click();
}

async function rectOf(page, selector, text, opts) {
  const el = text ? await byText(page, selector, text, opts) : await page.$(selector);
  if (!el) return null;
  const box = await el.boundingBox();
  return box ? { x: box.x, y: box.y, w: box.width, h: box.height } : null;
}

const SHOTS = [
  {
    name: "login",
    async run(page) {
      // capture BEFORE logging in
      await page.goto(`${BASE}/login`, { waitUntil: "networkidle0" });
      await sleep(300);
      return [];
    },
  },
  {
    name: "overview-ready",
    async run(page) {
      await page.goto(`${BASE}/assessments/4?tab=overview`, { waitUntil: "networkidle0" });
      await sleep(800);
      const anns = [];
      const start = await rectOf(page, "a", "Start AI grading");
      if (start) anns.push({ label: "one-click launch", pos: "right", ...start });
      return anns;
    },
  },
  {
    name: "overview-warnings",
    async run(page) {
      await page.goto(`${BASE}/assessments/3?tab=overview`, { waitUntil: "networkidle0" });
      await sleep(1000);
      const anns = [];
      const warn = await page.evaluate(() => {
        const els = [...document.querySelectorAll("div,p")].filter((e) =>
          e.textContent.includes("scanned pages couldn't be identified"),
        );
        const el = els.sort((a,b)=>{const ra=a.getBoundingClientRect(),rb=b.getBoundingClientRect();return ra.width*ra.height-rb.width*rb.height})[0];
        if (!el) return null;
        const b = el.getBoundingClientRect();
        return { x: b.x, y: b.y, w: b.width, h: b.height };
      });
      if (warn) anns.push({ label: "live warnings", pos: "corner", ...warn });
      return anns;
    },
  },
  {
    name: "stage-nav",
    async run(page) {
      await page.goto(`${BASE}/assessments/4?tab=review`, { waitUntil: "networkidle0" });
      await sleep(800);
      const anns = [];
      const nav = await rectOf(page, "nav[aria-label='Assessment stages'] > div:first-child");
      if (nav) anns.push({ label: "5 workflow stages", pos: "inside-right", pad: 2, ...nav });
      const pills = await page.evaluate(() => {
        const els = [...document.querySelectorAll("nav a")].filter((a) =>
          a.className.includes("rounded-full"),
        );
        if (!els.length) return null;
        const first = els[0].getBoundingClientRect();
        const last = els[els.length - 1].getBoundingClientRect();
        return { x: first.x, y: first.y, w: last.right - first.x, h: first.height };
      });
      if (pills) anns.push({ label: "views inside the stage", pos: "right", ...pills });
      return anns;
    },
  },
  {
    name: "students-diff",
    async run(page) {
      await page.goto(`${BASE}/students`, { waitUntil: "networkidle0" });
      await sleep(600);
      // build a CSV missing b01/b02 from the live roster, upload through the real input
      const csv = await page.evaluate(async () => {
        const s = (await fetch("/api/students").then((r) => r.json())).students;
        const kept = s.filter((x) => !["b01", "b02"].includes(x.student_id));
        return (
          "student_id,name,email\n" +
          kept.map((x) => `${x.student_id},${x.name},${x.email}`).join("\n")
        );
      });
      const tmp = path.join(OUT, "_roster.csv");
      writeFileSync(tmp, csv);
      const input = await page.$("input[type=file]");
      await input.uploadFile(tmp);
      await sleep(300);
      await clickText(page, "button", "Import");
      await page.waitForFunction(
        () => document.body.innerText.includes("active students are not in this CSV"),
        { timeout: 8000 },
      );
      await sleep(300);
      const anns = [];
      const wd = await byText(page, "button", "Withdraw all 2", { exact: false });
      if (wd) {
        const b = await wd.boundingBox();
        anns.push({ label: "sync after add/drop", pos: "right", x: b.x, y: b.y, w: b.width, h: b.height });
      }
      return anns;
    },
  },
  {
    name: "submissions",
    async run(page) {
      await page.goto(`${BASE}/assessments/3?tab=submissions`, { waitUntil: "networkidle0" });
      await sleep(800);
      return [];
    },
  },
  {
    name: "identify-top",
    async run(page) {
      await page.goto(`${BASE}/assessments/3?tab=identify`, { waitUntil: "networkidle0" });
      await sleep(1200);
      return [];
    },
  },
  {
    name: "identify-matrix",
    async run(page) {
      await page.goto(`${BASE}/assessments/3?tab=identify`, { waitUntil: "networkidle0" });
      await sleep(1200);
      await page.evaluate(() => {
        const h = [...document.querySelectorAll("*")].find(
          (e) => e.childElementCount < 4 && e.textContent.trim().startsWith("Assignment matrix"),
        );
        h?.scrollIntoView({ block: "start" });
      });
      await sleep(600);
      return [];
    },
  },
  {
    name: "orphan-queue",
    async run(page) {
      await page.goto(`${BASE}/assessments/3?tab=identify`, { waitUntil: "networkidle0" });
      await sleep(1200);
      await page.evaluate(() => {
        const h = [...document.querySelectorAll("*")].find(
          (e) => e.childElementCount < 4 && e.textContent.trim().startsWith("Orphan queue"),
        );
        h?.scrollIntoView({ block: "start" });
      });
      await sleep(800);
      const anns = [];
      const search = await rectOf(page, "input[placeholder='search by id or name…']");
      if (search) anns.push({ label: "roster search", pos: "left", pad: 2, ...search });
      return anns;
    },
  },
  {
    name: "masking",
    async run(page) {
      await page.goto(`${BASE}/assessments/4?tab=masking`, { waitUntil: "networkidle0" });
      await sleep(1500);
      return [];
    },
  },
  {
    name: "providers-pricing",
    async run(page) {
      await page.goto(`${BASE}/providers`, { waitUntil: "networkidle0" });
      await sleep(600);
      // open pricing on the qwen row
      const btn = await page.evaluateHandle(() => {
        const rows = [...document.querySelectorAll("tbody tr")];
        const row = rows.find((r) => r.textContent.includes("dashscope"));
        return [...(row?.querySelectorAll("button") ?? [])].find(
          (b) => b.textContent.trim() === "Pricing",
        );
      });
      const el = btn.asElement();
      if (el) {
        await el.click();
        await sleep(800);
      }
      const anns = [];
      const price = await page.evaluate(() => {
        const els = [...document.querySelectorAll("p,div")].filter((e) =>
          e.textContent.trim().startsWith("Prices power run cost estimates"),
        );
        const el = els.sort((a,b)=>{const ra=a.getBoundingClientRect(),rb=b.getBoundingClientRect();return ra.width*ra.height-rb.width*rb.height})[0];
        if (!el) return null;
        const b = el.getBoundingClientRect();
        return { x: b.x, y: b.y, w: b.width, h: b.height + 60 };
      });
      if (price) anns.push({ label: "per-model pricing", pos: "bottom", ...price });
      return anns;
    },
  },
  {
    name: "method-dialog",
    async run(page) {
      await page.goto(`${BASE}/methods`, { waitUntil: "networkidle0" });
      await sleep(600);
      await clickText(page, "button", "New method");
      await sleep(600);
      const anns = [];
      const pol = await page.evaluate(() => {
        let el = [...document.querySelectorAll("p,span,div")].find(
          (e) => e.textContent.trim() === "Standard" && e.closest("dialog"),
        );
        if (!el) return null;
        for (let i = 0; i < 5 && el.parentElement; i++) {
          const r = el.getBoundingClientRect();
          if (r.height > 120) break;
          el = el.parentElement;
        }
        const b = el.getBoundingClientRect();
        return { x: b.x, y: b.y, w: b.width, h: b.height };
      });
      if (pol) anns.push({ label: "plain-language policies", pos: "below", pad: 3, ...pol });
      return anns;
    },
  },
  {
    name: "rubric-editor",
    async run(page) {
      await page.goto(`${BASE}/assessments/1?tab=problems`, { waitUntil: "networkidle0" });
      await sleep(800);
      await page.click("tbody tr td");
      await sleep(1000);
      await page.evaluate(() => {
        const el = [...document.querySelectorAll("p")].find((e) =>
          e.textContent.startsWith("Saving creates version"));
        el?.scrollIntoView({ block: "center" });
      });
      await sleep(400);
      const anns = [];
      const badge = await byText(page, "span", "sum 10 / 10", { exact: false });
      if (badge) {
        const b = await badge.boundingBox();
        anns.push({ label: "must sum to max", pos: "right", x: b.x, y: b.y, w: b.width, h: b.height });
      }
      return anns;
    },
  },
  {
    name: "launch-dialog",
    async run(page) {
      await page.goto(`${BASE}/runs?launch=1&assessment_id=1`, { waitUntil: "networkidle0" });
      await sleep(1200);
      // choose the Default method so the estimate appears
      await page.evaluate(() => {
        const selects = [...document.querySelectorAll("select")];
        const sel = selects[selects.length - 1];
        const opt = [...sel.options].find((o) => o.text.includes("Default"));
        sel.value = opt.value;
        sel.dispatchEvent(new Event("change", { bubbles: true }));
      });
      await sleep(1500);
      const anns = [];
      const est = await page.evaluate(() => {
        const els = [...document.querySelectorAll("p,div")].filter((e) =>
          e.textContent.trim().startsWith("Estimated cost:"),
        );
        const el = els.sort((a,b)=>{const ra=a.getBoundingClientRect(),rb=b.getBoundingClientRect();return ra.width*ra.height-rb.width*rb.height})[0];
        if (!el) return null;
        const b = el.getBoundingClientRect();
        return { x: b.x, y: b.y, w: b.width, h: b.height };
      });
      if (est) anns.push({ label: "cost before you commit", pos: "left", ...est });
      return anns;
    },
  },
  {
    name: "launch-warnings",
    async run(page) {
      await page.goto(`${BASE}/runs?launch=1&assessment_id=3`, { waitUntil: "networkidle0" });
      await sleep(1200);
      await page.evaluate(() => {
        const selects = [...document.querySelectorAll("select")];
        const sel = selects[selects.length - 1];
        const opt = [...sel.options].find((o) => o.text.includes("Default"));
        sel.value = opt.value;
        sel.dispatchEvent(new Event("change", { bubbles: true }));
      });
      await sleep(1500);
      const anns = [];
      const warn = await page.evaluate(() => {
        const els = [...document.querySelectorAll("p,div")].filter((e) =>
          e.textContent.includes("scanned pages couldn't be identified"),
        );
        const el = els.sort((a,b)=>{const ra=a.getBoundingClientRect(),rb=b.getBoundingClientRect();return ra.width*ra.height-rb.width*rb.height})[0];
        if (!el) return null;
        const b = el.getBoundingClientRect();
        return { x: b.x, y: b.y, w: b.width, h: b.height };
      });
      if (warn) anns.push({ label: "it tells you first", pos: "left", ...warn });
      return anns;
    },
  },
  {
    name: "review",
    async run(page) {
      await page.goto(`${BASE}/assessments/4?tab=review`, { waitUntil: "networkidle0" });
      await sleep(800);
      return [];
    },
  },
  {
    name: "answer-view",
    async run(page) {
      await page.goto(`${BASE}/answers/1`, { waitUntil: "networkidle0" });
      await sleep(1500);
      const anns = [];
      const crit = await page.evaluate(() => {
        const els = [...document.querySelectorAll("p,div,li")].filter((e) =>
          e.textContent.trim().startsWith("Names a correct O(n log n)"),
        );
        const el = els.sort((a,b)=>{const ra=a.getBoundingClientRect(),rb=b.getBoundingClientRect();return ra.width*ra.height-rb.width*rb.height})[0];
        if (!el) return null;
        const b = el.getBoundingClientRect();
        return { x: b.x, y: b.y, w: b.width, h: b.height };
      });
      if (crit) anns.push({ label: "per-criterion reasoning", pos: "left", ...crit });
      return anns;
    },
  },
  {
    name: "analysis-cards",
    async run(page) {
      await page.goto(`${BASE}/assessments/1?tab=analysis`, { waitUntil: "networkidle0" });
      await sleep(1500);
      const anns = [];
      const dis = await page.evaluate(() => {
        const els = [...document.querySelectorAll("h2,h3,div,span")].filter((e) => {
          const r = e.getBoundingClientRect();
          return e.textContent.trim() === "Where methods disagree" && r.width > 10 && r.height > 5;
        });
        const el = els.sort((a,b)=>{const ra=a.getBoundingClientRect(),rb=b.getBoundingClientRect();return ra.width*ra.height-rb.width*rb.height})[0];
        if (!el) return null;
        const b = el.getBoundingClientRect();
        return { x: b.x, y: b.y, w: b.width, h: b.height };
      });
      if (dis) anns.push({ label: "real per-answer gaps", pos: "right", ...dis });
      return anns;
    },
  },
  {
    name: "analysis-matrix",
    async run(page) {
      await page.goto(`${BASE}/assessments/4?tab=analysis`, { waitUntil: "networkidle0" });
      await sleep(1500);
      await page.evaluate(() => {
        const els = [...document.querySelectorAll("h2,h3,div,span")].filter(
          (e) => e.textContent.trim() === "Problem breakdown",
        );
        const el = els.sort((a,b)=>{const ra=a.getBoundingClientRect(),rb=b.getBoundingClientRect();return ra.width*ra.height-rb.width*rb.height})[0];
        el?.scrollIntoView({ block: "start" });
      });
      await sleep(400);
      // expand the second problem row for the details-on-demand story
      await page.evaluate(() => {
        const rows = [...document.querySelectorAll("tr")].filter((r) => r.textContent.includes("max 10"));
        rows[1]?.querySelector("td")?.click();
      });
      await sleep(1500);
      await page.evaluate(() => {
        const el = [...document.querySelectorAll("*")].find(
          (e) => e.childElementCount === 0 && e.textContent.trim() === "Score statistics",
        ) ?? [...document.querySelectorAll("th,td,h3,h4,p,span")].find((e) => /score statistics/i.test(e.textContent) && e.childElementCount < 3);
        el?.scrollIntoView({ block: "center" });
      });
      await sleep(500);
      return [];
    },
  },
  {
    name: "final-source",
    async run(page) {
      await page.goto(`${BASE}/assessments/4?tab=publish`, { waitUntil: "networkidle0" });
      await sleep(1200);
      const anns = [];
      const card = await page.evaluate(() => {
        const els = [...document.querySelectorAll("h2,h3,div,span")].filter(
          (e) => e.textContent.trim() === "Final grading source",
        );
        const el = els.sort((a,b)=>{const ra=a.getBoundingClientRect(),rb=b.getBoundingClientRect();return ra.width*ra.height-rb.width*rb.height})[0];
        if (!el) return null;
        const b = el.getBoundingClientRect();
        return { x: b.x, y: b.y, w: b.width, h: b.height };
      });
      if (card) anns.push({ label: "nothing is official until this", pos: "right", ...card });
      return anns;
    },
  },
  {
    name: "publish-blocked",
    async run(page) {
      await page.goto(`${BASE}/assessments/2?tab=publish`, { waitUntil: "networkidle0" });
      await sleep(1200);
      return [];
    },
  },
  {
    name: "totals",
    async run(page) {
      await page.goto(`${BASE}/assessments/4?tab=totals`, { waitUntil: "networkidle0" });
      await sleep(800);
      return [];
    },
  },
  {
    name: "regrade-rounds",
    async run(page) {
      await page.goto(`${BASE}/assessments/4?tab=regrades`, { waitUntil: "networkidle0" });
      await sleep(1000);
      return [];
    },
  },
  {
    name: "regrade-inbox",
    async run(page) {
      await page.goto(`${BASE}/regrades`, { waitUntil: "networkidle0" });
      await sleep(1000);
      const anns = [];
      // Box the recovery-banner stack (held / unparsed / undelivered) that the
      // queue surfaces above the actionable list.
      const banners = await page.evaluate(() => {
        const first = [...document.querySelectorAll("div")].find((e) =>
          e.textContent.includes("was held as") && e.textContent.includes("superseded"),
        );
        const last = [...document.querySelectorAll("div")].find((e) =>
          e.textContent.includes("undelivered result email"),
        );
        if (!first || !last) return null;
        const a = first.getBoundingClientRect();
        const b = last.getBoundingClientRect();
        return { x: a.x, y: a.y, w: a.width, h: b.bottom - a.y };
      });
      if (banners) anns.push({ label: "recovery sets, not lost", pos: "below", pad: 3, ...banners });
      return anns;
    },
  },
  {
    name: "regrade-detail",
    async run(page) {
      await page.goto(`${BASE}/regrades`, { waitUntil: "networkidle0" });
      await sleep(1000);
      // Open the first filed request; its detail pane is the TA's per-problem
      // adjudication workspace (AI re-grade assist + Upheld/Regraded verdict).
      await page.evaluate(() => {
        document.querySelector("tbody tr")?.click();
      });
      await page.waitForFunction(
        () => [...document.querySelectorAll("button")].some((b) => b.textContent.trim() === "Upheld"),
        { timeout: 8000 },
      );
      await sleep(600);
      const anns = [];
      const verdict = await page.evaluate(() => {
        const up = [...document.querySelectorAll("button")].find((b) => b.textContent.trim() === "Upheld");
        const rg = [...document.querySelectorAll("button")].find((b) => /^Regraded/.test(b.textContent.trim()));
        if (!up || !rg) return null;
        const a = up.getBoundingClientRect();
        const b = rg.getBoundingClientRect();
        return { x: a.x, y: a.y, w: b.right - a.x, h: a.height };
      });
      // One annotation only — the verdict buttons are the point; the AI re-grade
      // button is self-labelled, and a second pill here just crowds the pane.
      if (verdict) anns.push({ label: "your call, per problem", pos: "above", ...verdict });
      return anns;
    },
  },
  {
    name: "quarantine",
    async run(page) {
      await page.goto(`${BASE}/assessments/3?tab=submissions`, { waitUntil: "networkidle0" });
      await sleep(1000);
      // Quarantine is the last section on the page, so it can't scroll to the top —
      // scroll fully to the bottom so the whole card (rows + reason + Assign) is in
      // frame, with the reconciliation tail above it for context.
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      await sleep(600);
      const anns = [];
      const reason = await page.evaluate(() => {
        const el = [...document.querySelectorAll("td,div,span,p")].find((e) =>
          e.textContent.trim().startsWith("filename couldn't be uniquely matched"),
        );
        if (!el) return null;
        const b = el.getBoundingClientRect();
        return { x: b.x, y: b.y, w: b.width, h: b.height };
      });
      if (reason) anns.push({ label: "plain-language reason", pos: "right", ...reason });
      return anns;
    },
  },
  {
    name: "guide",
    async run(page) {
      await page.goto(`${BASE}/guide`, { waitUntil: "networkidle0" });
      await sleep(600);
      return [];
    },
  },
];

const only = process.argv[2];

const browser = await puppeteer.launch({
  executablePath: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  headless: "new",
});
const page = await browser.newPage();
await page.setViewport({ width: 1440, height: 900, deviceScaleFactor: 2 });

// login shot first (pre-auth), then dev login for the rest
for (const shot of SHOTS) {
  if (only && shot.name !== only) continue;
  try {
    if (shot.name !== "login" && !page.__authed) {
      await page.goto(`${BASE}/login`, { waitUntil: "networkidle0" });
      await page.type("input[placeholder='you@example.com']", "b11902156@ntu.edu.tw");
      await clickText(page, "button", "Sign in (dev)");
      await page.waitForFunction(() => location.pathname !== "/login", { timeout: 8000 });
      page.__authed = true;
    }
    const anns = await shot.run(page);
    await page.screenshot({ path: path.join(OUT, `${shot.name}.jpg`), type: "jpeg", quality: 88 });
    writeFileSync(
      path.join(OUT, `${shot.name}.json`),
      JSON.stringify({ viewport: { w: 1440, h: 900 }, anns }, null, 1),
    );
    console.log(`ok ${shot.name} (${anns.length} annotations)`);
  } catch (err) {
    console.error(`FAIL ${shot.name}: ${err.message}`);
  }
}
await browser.close();
