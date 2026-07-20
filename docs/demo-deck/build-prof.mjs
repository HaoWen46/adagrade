// Assemble deck-prof.html — the professor's edition of the demo deck.
//
// Audience: the course professor. One exam's journey told as a single causal
// chain, in strict chronological order: rubric before the exam, then scan →
// identify → mask → AI grading → spot-check → send → appeals. Every station
// slide says three things: what the previous station handed over, what this
// station does, and what it passes on ("交給下一站的：…"), with a persistent
// progress strip on top so the reader always knows where in the line they
// are. Neutral document tone (roles 助教/系統/AI/老師 — never direct
// address), Traditional Chinese, no config screens, no jargon.
//
// Build: node build-prof.mjs  → deck-prof.html  (then print via headless
// Chrome to deck-prof.pdf — see README.md).
import { writeFileSync } from "node:fs";

const SHOT_W = 1440; // capture viewport width (matches shots.mjs)

// Like build.mjs's img(), but annotations are passed in directly (Chinese
// labels for this deck) and label-width estimation is tuned for CJK glyphs.
function img(name, anns = [], { width = 960, crop = 900 } = {}) {
  const scale = width / SHOT_W;
  const height = Math.round(crop * scale);
  const fullHeight = Math.round(900 * scale);
  const overlays = anns
    .map((a) => {
      const x = a.x * scale, y = a.y * scale, w = a.w * scale, h = a.h * scale;
      const pad = a.pad ?? 6;
      const est = [...a.label].reduce((n, ch) => n + (ch.charCodeAt(0) > 0x2e7f ? 16 : 8.2), 24);
      const pos = a.pos ?? (x + w + 14 + est < width ? "right" : "above");
      let lx, ly;
      if (pos === "right") { lx = x + w + 16; ly = y + h / 2 - 14; }
      else if (pos === "left") { lx = x - est - 16; ly = y + h / 2 - 14; }
      else if (pos === "below") { lx = x - 6; ly = y + h + 14; }
      else { lx = x - 6; ly = y - 40; } // above
      return `
      <div style="position:absolute;left:${x - pad}px;top:${y - pad}px;width:${w + pad * 2}px;height:${h + pad * 2}px;border:3px solid #4f46e5;border-radius:10px;box-shadow:0 0 0 3px rgba(255,255,255,.7)"></div>
      <div style="position:absolute;left:${lx}px;top:${ly}px;background:#4f46e5;color:#fff;font-size:16px;font-weight:600;padding:4px 12px;border-radius:999px;white-space:nowrap;box-shadow:0 1px 4px rgba(0,0,0,.25)">${a.label}</div>`;
    })
    .join("");
  return `<div class="shot" style="position:relative;width:${width}px;height:${height}px;overflow:hidden;border-radius:10px;border:1px solid #e5e5e5">
    <img src="shots/${name}.jpg" style="width:${width}px;height:${fullHeight}px;display:block" alt="${name}"/>
    ${overlays}
  </div>`;
}

// The eight stations of the line, in true chronological order. The overview
// slide, the per-slide progress strip, and the kickers all use these exact
// names so the reader never has to reconcile two vocabularies.
const STATIONS = [
  { t: "訂評分標準", r: "you" },
  { t: "掃描", r: "ta" },
  { t: "認人", r: "sys" },
  { t: "遮名", r: "sys" },
  { t: "AI 改卷", r: "ai" },
  { t: "抽查", r: "you" },
  { t: "寄出", r: "you" },
  { t: "申訴", r: "sys" },
];

const ROLES = {
  ta: { label: "助教", cls: "r-ta" },
  sys: { label: "系統", cls: "r-sys" },
  ai: { label: "AI", cls: "r-ai" },
  you: { label: "老師", cls: "r-you" },
};
const roleChip = (r) => `<span class="role ${ROLES[r].cls}">${ROLES[r].label}</span>`;

// Progress strip: where we are on the line. Past stations dim, the current
// one is lit; the reader never loses their place between slides.
function strip(current) {
  return `<div class="strip">${STATIONS
    .map((st, i) => {
      const n = i + 1;
      const cls = n < current ? "done" : n === current ? "cur" : "todo";
      return `<span class="st ${cls}">${n} ${st.t}</span>`;
    })
    .join('<span class="st-arrow">→</span>')}</div>`;
}

function slide({ station, part, role, title, caption, body, foot, dark = false }) {
  const kicker = station
    ? `第 ${station} 站${part ? `（${part}）` : ""} `
    : "";
  return `<section class="slide${dark ? " dark" : ""}">
    ${station ? strip(station) : ""}
    <header>
      ${kicker || role ? `<p class="kicker">${kicker}${role ? roleChip(role) : ""}</p>` : ""}
      ${title ? `<h1>${title}</h1>` : ""}
      ${caption ? `<p class="caption">${caption}</p>` : ""}
    </header>
    <div class="body">${body ?? ""}</div>
    ${foot ? `<p class="foot">${foot}</p>` : ""}
  </section>`;
}

// Horizontal chip flow (overview + regrade slides).
function flow(chips, cls = "") {
  return `<div class="flow ${cls}">${chips
    .map((c, i) => `${i ? '<div class="fa">→</div>' : ""}
      <div class="fc${c.you ? " you" : ""}">
        ${c.n ? `<span class="fc-n">${c.n}</span>` : ""}${c.r ? `<span class="fc-role">${ROLES[c.r].label}</span>` : ""}${c.t}${c.s ? `<span class="fc-sub">${c.s}</span>` : ""}
      </div>`)
    .join("")}</div>`;
}

const slides = [];

// ── 1 · title ────────────────────────────────────────────────────────────────
slides.push(slide({
  dark: true,
  title: "一份考卷的旅程",
  caption: "從考前訂下評分標準，到每個學生收到成績、申訴有處可去 — 一站接一站，照時間順序走一遍。黃色的站是老師的，一共三站；其餘的，助教和系統處理。",
}));

// ── 2 · the line, in one view ────────────────────────────────────────────────
slides.push(slide({
  title: "整條線，八站",
  caption: "接下來一頁一站。每一頁都先說：上一站交來了什麼 → 這一站做什麼 → 交什麼給下一站。頁首的進度條，隨時標著走到哪裡。",
  body:
    flow(STATIONS.slice(0, 4).map((st, i) => ({ n: i + 1, r: st.r, t: st.t, you: st.r === "you" }))) +
    flow(STATIONS.slice(4).map((st, i) => ({ n: i + 5, r: st.r, t: st.t, you: st.r === "you" })), "second"),
  foot: "第 1 站在考試之前；第 2 站到第 8 站，都在考完之後。",
}));

// ── 3 · station 1: the rubric (before the exam) ──────────────────────────────
slides.push(slide({
  station: 1, role: "you",
  title: "考前：出題時，評分標準一起訂好",
  caption: "一切從出題那天開始。題目定稿的同時，把評分標準一條一條寫進系統：哪一條給幾分、部分給分怎麼算。之後整條線 — AI 改卷、抽查、學生申訴 — 全都以這一份為準，誰都不能自由發揮。",
  body: img("rubric-editor", [
    { x: 346, y: 511, w: 756, h: 36, label: "一條標準、幾分", pos: "below" },
    { x: 1043, y: 709, w: 80, h: 22, label: "加總自動核對配分", pos: "left" },
  ], { width: 760, crop: 750 }),
  foot: "<b>交給下一站的：</b>一把改卷的尺。接著，考試照常舉行。",
}));

// ── 4 · station 2: scan the pile ─────────────────────────────────────────────
slides.push(slide({
  station: 2, role: "ta",
  title: "考完了：一整疊，掃描，丟進系統",
  caption: "考完當天，手上是一整疊紙。監考收齊、掃描器掃成一個大檔，整疊直接丟進系統 — 不必先一份一份拆開。（若已經一人一檔，直接上傳也可以。）",
  body: img("identify-top", [
    { x: 328, y: 540, w: 240, h: 30, label: "一整疊，或 zip，直接丟", pos: "right" },
    { x: 328, y: 718, w: 915, h: 32, label: "學號在系上機器讀，不外流", pos: "above" },
  ], { width: 710, crop: 800 }),
  foot: "<b>交給下一站的：</b>一疊「還不知道是誰的」頁面。下一站就是把每一頁認回人。",
}));

// ── 5 · station 3: identify every page ───────────────────────────────────────
slides.push(slide({
  station: 3, role: "sys",
  title: "每一頁，認回它的主人",
  caption: "系統讀每一頁上的學號，對上修課名冊，一頁一頁分回學生名下。字太潦草、讀不出來的，就像下面這頁 — 排進待認清單，助教對照名冊點一下，指認回去。沒有任何一頁會不明不白地消失。",
  body: img("orphan-queue", [
    { x: 336, y: 184, w: 140, h: 47, label: "學號讀不出來", pos: "below" },
    { x: 832, y: 228, w: 486, h: 100, label: "對照名冊，點一下指認", pos: "above" },
  ], { width: 900, crop: 520 }),
  foot: "<b>交給下一站的：</b>每一頁都對到人了；誰缺交，也點得出名字。",
}));

// ── 6 · station 4: mask names ────────────────────────────────────────────────
slides.push(slide({
  station: 4, role: "sys",
  title: "改卷之前：姓名、學號，先遮掉",
  caption: "頁面都認回人了 — 但接下來要改卷的 AI，不該知道卷子是誰的。所以送出去之前，系統把每一頁的姓名、學號蓋住。和人工改卷先摺名條，同一個道理，只是自動的。",
  body: img("masking", [
    { x: 358, y: 333, w: 143, h: 49, label: "學號，遮掉", pos: "left" },
    { x: 531, y: 333, w: 143, h: 49, label: "姓名，遮掉", pos: "below" },
  ], { width: 820, crop: 620 }),
  foot: "<b>交給下一站的：</b>一疊遮了名的答案。",
}));

// ── 7 · station 5: the AI grades ─────────────────────────────────────────────
slides.push(slide({
  station: 5, role: "ai",
  title: "AI 拿到兩樣東西：答案，和尺",
  caption: "到這一站，AI 手上有：剛遮了名的答案，和第 1 站訂好的評分標準。助教按「開始改卷」— 按下去之前，系統先報這一次要花多少錢；若前面幾站沒走完（有頁面沒認回、名字沒遮好），按鈕會被擋住，半套的卷子進不去。",
  body: img("launch-dialog", [
    { x: 513, y: 512, w: 170, h: 22, label: "先報價，再開工", pos: "right" },
    { x: 853, y: 691, w: 75, h: 32, label: "按下去，AI 開工", pos: "right" },
  ], { width: 750, crop: 760 }),
  foot: "三百人的期中考，大約半小時改完。改完交回來的東西長什麼樣？下一頁。",
}));

// ── 8 · station 5 (continued): what comes back ───────────────────────────────
slides.push(slide({
  station: 5, part: "改完的樣子", role: "ai",
  title: "交回來的：每一分，都有理由",
  caption: "這就是 AI 交回來的一份：左邊是那頁手寫答案，右邊照著評分標準一條一條給分 — 哪一條扣了幾分、為什麼扣，白紙黑字。全班每個學生的每一題，都是這樣一份。",
  body: img("answer-view", [
    { x: 250, y: 255, w: 560, h: 230, label: "學生的手寫答案", pos: "below" },
    { x: 882, y: 495, w: 504, h: 72, label: "依評分標準逐條給分", pos: "left" },
  ], { width: 780, crop: 720 }),
  foot: "<b>交給下一站的：</b>每題有分數、有理由 — 但還沒定案。定案的是人。",
}));

// ── 9 · station 6: spot-check ────────────────────────────────────────────────
slides.push(slide({
  station: 6, role: "you",
  title: "人來驗收：抽查、改分",
  caption: "AI 交卷了，輪到人。系統自動挑出樣本、和 AI 自己沒把握的卷子，排成一張單子 — 翻一翻，改錯的直接改分數。人改的分數，永遠蓋過 AI 的。",
  body: img("review", [], { width: 800, crop: 700 }),
  foot: "助教也能分頭驗收；誰改了什麼，系統都留紀錄。<b>交給下一站的：</b>驗收過的成績單。",
}));

// ── 10 · station 7: send ─────────────────────────────────────────────────────
slides.push(slide({
  station: 7, role: "you",
  title: "寄出成績：一個按鈕，但有守門員",
  caption: "成績驗收完，最後一關：先白紙黑字選定「以哪一次改卷為準」，再按寄出。若抽查沒做完、還有人沒成績 — 按鈕按不下去，寄不出半成品。",
  body: img("final-source", [
    { x: 329, y: 245, w: 300, h: 20, label: "以哪一次改卷為準，白紙黑字", pos: "right" },
  ], { width: 780, crop: 700 }),
  foot: "按下去之後，學生收到什麼？下一頁。",
}));

// ── 11 · station 7 (continued): the student's inbox ──────────────────────────
slides.push(slide({
  station: 7, part: "學生那頭", role: "sys",
  title: "每位學生，收到自己的一封信",
  caption: "一人一封：總分、每一題的得分、扣分理由 — 和系統裡看到的一模一樣。信末寫明：不服氣，回這封信，期限之內受理。",
  body: `<div class="send">
    <div class="send-btn">寄出成績</div>
    <div class="fa big">→</div>
    <div class="mail">
      <p class="mail-h">學生信箱</p>
      <div class="mail-line"><span>總分</span>28 / 40</div>
      <div class="mail-line"><span>第 1 題</span>10 / 10 — 遞迴式與主定理皆正確</div>
      <div class="mail-line"><span>第 2 題</span>2 / 10 — 未證明貪婪法之最佳性……</div>
      <div class="mail-foot">回覆這封信，就是申訴的入口 — 有期限，過期不收。</div>
    </div>
  </div>`,
  foot: "<b>交給下一站的：</b>申訴的入口，就在信裡。",
}));

// ── 12 · station 8: appeals ──────────────────────────────────────────────────
slides.push(slide({
  station: 8, role: "sys",
  title: "申訴：有紀錄、有期限、有結果",
  caption: "學生回了信，系統自動對回那一題的考卷，排進清單。AI 先用更嚴的標準重看一次，助教回覆；需要拍板的，才會送到老師面前。每一件都查得到、關得掉。",
  body: flow([
    { r: "sys", t: "學生回信" },
    { r: "sys", t: "自動歸檔", s: "對到那一題" },
    { r: "ai", t: "AI 重看", s: "更嚴的標準" },
    { r: "ta", t: "助教回覆" },
    { r: "you", t: "需要時拍板", you: true },
  ]),
  foot: "到這裡，一份考卷的旅程走完了。不再是散在信箱裡的口水戰。",
}));

// ── 13 · cost ────────────────────────────────────────────────────────────────
slides.push(slide({
  title: "走完這一趟，要花多少錢？",
  body: `<div class="big-numbers">
    <div><p class="num">0.1 元</p><p class="lbl">改一位學生的整份考卷（新台幣）</p></div>
    <div><p class="num">約 30 元</p><p class="lbl">全班 300 人的期中考 — 比一杯咖啡便宜</p></div>
  </div>`,
  foot: "實測數字：每題約 0.025 元（台幣）。第 5 站按下去之前，系統都會先報價 — 不會有意外帳單。",
}));

// ── 14 · close ───────────────────────────────────────────────────────────────
slides.push(slide({
  dark: true,
  title: "下次考試，試一次",
  body: `<ol class="steps dark-steps">
    <li><b>老師</b>：考前訂評分標準（和以前出題一樣）；考後抽查、按寄出。</li>
    <li><b>助教</b>：掃描上傳、指認讀不出的頁面、申訴第一線。</li>
    <li><b>AI</b>：拿遮了名的答案，照評分標準改，每一分附理由。</li>
  </ol>`,
  foot: "整條線都跑在系上自己的機器；考卷影像不出系，AI 只看得到遮了名的答案。",
}));

const html = `<!doctype html>
<html><head><meta charset="utf-8"><title>ADA-Marker — 給老師的簡報</title>
<style>
  * { margin:0; padding:0; box-sizing:border-box; }
  @page { size: 1280px 720px; margin: 0; }
  body { font-family: -apple-system, "PingFang TC", "Noto Sans TC", sans-serif; }
  .slide { width:1280px; height:720px; page-break-after:always; padding:36px 60px 42px; position:relative; overflow:hidden; background:#fff; }
  .slide.dark { background:#1e1b4b; color:#fff; display:flex; flex-direction:column; justify-content:center; padding:42px 60px; }
  .slide.dark h1 { color:#fff; font-size:62px; }
  .slide.dark .caption { color:#c7d2fe; font-size:26px; margin-top:18px; }
  .slide.dark .foot { color:#a5b4fc; }
  .strip { display:flex; align-items:center; gap:5px; margin-bottom:14px; }
  .st { font-size:14.5px; font-weight:600; padding:4px 11px; border-radius:999px; white-space:nowrap; }
  .st.done { background:#eef2ff; color:#818cf8; }
  .st.cur { background:#4f46e5; color:#fff; box-shadow:0 2px 6px rgba(79,70,229,.35); }
  .st.todo { background:#f8fafc; color:#cbd5e1; border:1px solid #f1f5f9; }
  .st-arrow { color:#e2e8f0; font-size:13px; }
  .kicker { font-size:18px; font-weight:700; letter-spacing:.14em; color:#6366f1; margin-bottom:8px; display:flex; align-items:center; gap:10px; }
  .role { font-size:15px; font-weight:700; letter-spacing:0; padding:3px 12px; border-radius:999px; }
  .r-ta { background:#f1f5f9; color:#475569; border:1px solid #cbd5e1; }
  .r-sys { background:#eef2ff; color:#4338ca; border:1px solid #c7d2fe; }
  .r-ai { background:#eef2ff; color:#4338ca; border:1px solid #c7d2fe; }
  .r-you { background:#fef3c7; color:#92400e; border:1px solid #fcd34d; }
  h1 { font-size:40px; font-weight:700; color:#0f172a; letter-spacing:-.01em; }
  .caption { font-size:21px; color:#334155; margin-top:10px; max-width:1150px; line-height:1.55; }
  .body { margin-top:12px; display:flex; flex-direction:column; align-items:center; }
  .shot { margin:0 auto; }
  .foot { position:absolute; left:60px; right:60px; bottom:24px; font-size:18px; color:#64748b; line-height:1.5; }
  .foot b { color:#4f46e5; font-weight:700; }
  .slide.dark .foot { position:static; margin-top:40px; }
  ol.steps { list-style:none; margin-top:30px; width:100%; counter-reset: step; }
  ol.steps li { font-size:29px; color:#0f172a; line-height:1.55; padding:20px 0 20px 88px; position:relative; counter-increment: step; }
  ol.steps li::before { content: counter(step); position:absolute; left:8px; top:20px; width:52px; height:52px; border-radius:999px; background:#4f46e5; color:#fff; font-size:27px; font-weight:700; display:flex; align-items:center; justify-content:center; }
  ol.steps li b { color:#a5b4fc; }
  ol.dark-steps li { color:#e0e7ff; }
  .flow { display:flex; align-items:stretch; gap:12px; margin-top:34px; justify-content:center; width:100%; }
  .flow.second { margin-top:30px; }
  .fc { position:relative; background:#eef2ff; border:2px solid #c7d2fe; border-radius:14px; padding:26px 22px 20px; font-size:24px; font-weight:700; color:#312e81; display:flex; flex-direction:column; justify-content:center; text-align:center; line-height:1.35; white-space:nowrap; }
  .fc.you { background:#fef3c7; border-color:#fcd34d; color:#78350f; }
  .fc-n { position:absolute; top:-15px; right:-10px; width:30px; height:30px; border-radius:999px; background:#0f172a; color:#fff; font-size:16px; font-weight:700; display:flex; align-items:center; justify-content:center; }
  .fc-role { position:absolute; top:-13px; left:14px; font-size:14px; font-weight:700; background:#4f46e5; color:#fff; padding:2px 10px; border-radius:999px; }
  .fc.you .fc-role { background:#d97706; }
  .fc-sub { display:block; font-size:16px; font-weight:500; color:#6366f1; margin-top:6px; white-space:normal; }
  .fc.you .fc-sub { color:#b45309; }
  .fa { font-size:30px; color:#94a3b8; align-self:center; }
  .fa.big { font-size:56px; }
  .send { display:flex; align-items:center; gap:44px; margin-top:40px; }
  .send-btn { background:#4f46e5; color:#fff; font-size:38px; font-weight:800; padding:32px 54px; border-radius:20px; box-shadow:0 10px 24px rgba(79,70,229,.35); }
  .mail { background:#fff; border:1px solid #e2e8f0; border-radius:16px; box-shadow:0 8px 22px rgba(15,23,42,.08); padding:26px 32px; min-width:560px; }
  .mail-h { font-size:20px; font-weight:700; color:#0f172a; margin-bottom:14px; }
  .mail-line { font-size:20px; color:#334155; padding:9px 0; border-top:1px solid #f1f5f9; }
  .mail-line span { display:inline-block; min-width:88px; color:#6366f1; font-weight:700; }
  .mail-foot { font-size:17px; color:#64748b; margin-top:14px; }
  .big-numbers { display:flex; gap:48px; margin-top:56px; width:100%; justify-content:center; }
  .big-numbers > div { background:#f8fafc; border:1px solid #e2e8f0; border-radius:16px; padding:52px 56px; text-align:center; min-width:420px; }
  .num { font-size:84px; font-weight:800; color:#4f46e5; letter-spacing:-.02em; }
  .lbl { font-size:24px; color:#334155; margin-top:14px; }
</style></head>
<body>
${slides.join("\n")}
</body></html>`;

writeFileSync("deck-prof.html", html);
console.log(`built deck-prof.html with ${slides.length} slides`);
