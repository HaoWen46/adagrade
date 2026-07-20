# Demo deck

Two decks, two audiences:

- **`deck-prof.pdf`** — the professor's edition (Traditional Chinese, 14 slides, large
  type). The real pipeline end to end — scan upload → identify → mask → AI grading →
  spot-check → publish → regrades — one step per slide, each tagged by who does it
  (助教/系統/AI/您), with config screens (Providers/Methods/Users) deliberately out of
  frame. Built by `build-prof.mjs` from the same screenshots plus Chinese diagrams.
  Present it with `prof-demo-script.md`.
- **`deck.pdf`** — the full 28-slide walkthrough for the teaching team, below.

`deck.pdf` — a 28-slide visual walkthrough of ADA-Marker for the teaching team:
annotated screenshots of the live app plus diagrams (pipeline, who-does-what
role matrix, data boundary, regrade loop). It covers what each role does, how the
work flows, what the AI actually costs, and — near the end — an honest "not done
yet" slide.

## Regenerating

The deck is built from live screenshots of the dev server, so it stays current
by re-running the pipeline (needs `node`, Chrome, and the dev server with demo
data on :8899):

```sh
npm i puppeteer-core
node shots.mjs     # drives the app via dev login, captures annotated shots
node build.mjs     # assembles deck.html from shots + inline SVG diagrams
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --headless --print-to-pdf=deck.pdf --no-pdf-header-footer file://$PWD/deck.html
node qa.mjs        # optional: cheap OpenRouter VLM checks each slide for layout defects
```

A few slides need demo states that a fresh dev DB may not have — the
`regrade-detail` shot wants a filed request open for adjudication, and the
`quarantine` shot wants unmatched uploads on assessment 3. The recovery banners
(held / unparsed / undelivered) on the inbox slide likewise need one
`rejected_superseded` request and one resolved-but-undelivered result. Seed these
in the dev DB before capturing (a filed reply held while unpublished; a resolved
request with `result_sent_at` cleared; two `upload_quarantine` rows).

Screenshots contain only the fake demo roster — never regenerate against a
database holding real student data.
