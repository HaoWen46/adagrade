# Demo script — presenting ADA-Marker to the professor

Companion to `deck-prof.pdf` (the professor's edition, 14 slides, Traditional Chinese).
The audience is one person: the course professor. He wants the real process shown
honestly — upload to send — without being dragged through configuration screens. The
deck walks the actual pipeline with every step tagged 助教/系統/AI/老師; this script is
how to present it.

## Ground rules for the presenter

- **One step at a time, in pipeline order. Never jump ahead.** The deck IS the
  pipeline; if a slide prompts a question, answer it on that slide.
- **Show process, not settings.** The pipeline slides (upload, identify, masking,
  launch, review, publish) are all fair game — they are the real process. What stays
  closed: Providers, Methods, Users, and anything with raw JSON. If he asks how the AI
  is configured: 「助教設定一次,之後照評分標準走」, and offer a separate session.
- **Words to use**: 改考卷、評分標準、抽查、寄出成績、申訴、遮名。
  **Words to avoid**: pipeline, OCR (say 讀學號), provider, model version, rubric
  (say 評分標準), publish (say 寄出), spot-check (say 抽查), token.
- Speak to the slides in Mandarin; every app screen you touch is a table of names and
  numbers — read the numbers, not the chrome.
- Demo data only. Never present against a database holding real student work.

## Sequence

The deck is one causal chain: 8 stations in true chronological order (rubric BEFORE the
exam), one station per slide. Every station slide reads the same way — what the previous
station handed over → what this one does → 「交給下一站的:…」 — and the progress strip
on top always shows where the line stands. Present it in order; never skip a station.

### Before he arrives

1. `make frontend && make build`, start the dev server, confirm the seeded
   "Demo Exam — completed" (assessment 4) opens.
2. Log in **before** the demo; keep the browser on the assessment's Overview.
3. Close every other tab, hide bookmarks, browser zoom 125%.

### Slides 1–2 (talk)

Slide 2 is the map: 8 numbered stations, roles tagged, the 3 yellow ones are 老師's.
Say it once, slowly: 「整條線八站;黃色三站是老師的。第 1 站在考前,其他都在考後。」
Then announce the rule the deck follows: 每一頁先講上一站交來什麼,再講這一站做什麼、
交什麼給下一站。 That sentence is what makes the rest feel inevitable instead of fast.

### Stations 1–4 (slides 3–6) — before AI ever appears

1 訂評分標準 (his artifact — read ONE criterion + points aloud) → 2 掃描 (整疊丟進去)
→ 3 認人 (讀學號對名冊;讀不出的那頁怎麼被人工指認 — the trust moment for "no exam
vanishes") → 4 遮名 (the second trust moment: AI never knows whose page it is).
Live mirror: assessment 4 → Problems/Rubric, then Student work → Identify / Masking.
At each transition, literally say the handoff line from the slide's footer.

### Station 5 (slides 7–8) — the AI's turn

Slide 7: AI 手上兩樣東西 — 遮了名的答案+評分標準;先報價,再開工;沒備齊會被擋。
Slide 8: what comes back — read one criterion line and its reason, translated. If he
disagrees with the AI's judgment, perfect — that is station 6's opening line.

### Stations 6–7 (slides 9–11) — the human gate

Review: hand over the mouse if he's willing; one score change; 「人改的分數,永遠蓋過
AI 的。」 Publish: the gatekeeper screen (抽查沒完、有人沒成績 → 按不下去). Slide 11:
what lands in the student's inbox, and that replying to it IS the appeal.

### Station 8 + close (slides 12–14)

Appeals with roles tagged (he is only the last chip), the cost numbers, the role recap.

## If he asks…

- **「AI 會不會亂改?」** — slides 6+8: 它只能照評分標準,每一分附理由;老師抽查、改分,
  人的分數蓋過它。改卷前名字已遮掉,它不知道在改誰。
- **「認錯人怎麼辦?」** — slide 4: 認不出的學號進待認清單,助教對照名冊指認;考卷不會
  憑空消失,對不上的會一直掛在那裡直到有人處理。
- **「學生鬧怎麼辦?」** — slide 12: 全部有紀錄、有期限;第一線是 AI 重看+助教。
- **「資料放哪裡?」** — 「系上自己的機器;考卷影像不出系,AI 只看得到遮了名的答案。」
- **「要花多少錢?」** — slide 13 的數字是實測:每題約 0.025 元台幣;開改前系統先報價。

## Regenerating deck-prof.pdf

```sh
node build-prof.mjs
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --headless --print-to-pdf=deck-prof.pdf --no-pdf-header-footer file://$PWD/deck-prof.html
```

Reuses `shots/` captured for the main deck — recapture via `shots.mjs` first if the UI
changed. Keep this deck lean: one step per slide, pipeline order, no config screens.
