// Render each slide of deck.html to PNG and ask a cheap VLM to spot layout defects.
import puppeteer from "puppeteer-core";
import { readFileSync, mkdirSync } from "node:fs";
import path from "node:path";

const envFile = readFileSync("/Users/haowenchen/Files/projects/ADA-Marker/.env", "utf8");
const KEY = envFile.match(/^OPENROUTER_API_KEY=(.+)$/m)?.[1]?.trim();
const BASE = (envFile.match(/^OPENROUTER_BASE_URL=(.+)$/m)?.[1]?.trim() ?? "https://openrouter.ai/api/v1");
if (!KEY) throw new Error("no OPENROUTER_API_KEY in .env");
const MODEL = "google/gemini-3.1-flash-lite";

mkdirSync("qa-slides", { recursive: true });
const browser = await puppeteer.launch({
  executablePath: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  headless: "new",
});
const page = await browser.newPage();
await page.setViewport({ width: 1280, height: 720, deviceScaleFactor: 1 });
await page.goto(`file://${path.resolve("deck.html")}`, { waitUntil: "networkidle0" });
const count = await page.evaluate(() => document.querySelectorAll("section.slide").length);

const results = [];
for (let i = 0; i < count; i++) {
  const el = (await page.$$("section.slide"))[i];
  const buf = await el.screenshot({ type: "jpeg", quality: 80 });
  const b64 = Buffer.from(buf).toString("base64");
  const res = await fetch(`${BASE}/chat/completions`, {
    method: "POST",
    headers: { Authorization: `Bearer ${KEY}`, "Content-Type": "application/json" },
    body: JSON.stringify({
      model: MODEL,
      max_tokens: 300,
      messages: [
        {
          role: "user",
          content: [
            { type: "text", text: "This is one slide of a presentation deck. Report ONLY genuine layout defects: text clipped or overflowing its container or the slide edge, elements overlapping so text is unreadable, annotation labels/circles covering the exact content they point at, images cut off mid-content in a way that loses meaning, or unreadably small essential text. Screenshots being cropped at the slide's bottom edge is intentional and fine. If none, reply exactly OK. Otherwise list each defect in one short line." },
            { type: "image_url", image_url: { url: `data:image/jpeg;base64,${b64}` } },
          ],
        },
      ],
    }),
  });
  const j = await res.json();
  const verdict = j.choices?.[0]?.message?.content?.trim() ?? `API-ERROR: ${JSON.stringify(j).slice(0, 200)}`;
  results.push({ slide: i + 1, verdict });
  console.log(`slide ${i + 1}: ${verdict.replace(/\n/g, " | ")}`);
}
await browser.close();
const bad = results.filter((r) => r.verdict !== "OK");
console.log(`\n${count - bad.length}/${count} clean`);
