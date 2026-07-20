# MODELS.md — vision-model cheat sheet for grading

*Snapshot of OpenRouter's live catalog, **2026-07-02** (`GET https://openrouter.ai/api/v1/models`,
168 vision-capable models). Prices are per **1M tokens** and drift — the Providers page
"Test" button fetches the live catalog, and this file's numbers should be re-checked
before a big run. A typical grading call ≈ 1.5k input tokens (one masked page + prompt)
+ 400 output, so **per-answer cost ≈ (1.5 × in + 0.4 × out) / 1000**.*

## Very cheap reasoning VLMs (the sweet spot for per-criterion grading)

| Model id | $/M in | $/M out | ~$/answer | Reasoning | Notes |
|---|---|---|---|---|---|
| `qwen/qwen3.5-flash-02-23` | 0.065 | 0.26 | ~$0.0002 | optional | 1M ctx; tools+structured; **best default candidate** — a full 1,600-answer run ≈ $0.33 |
| `openai/gpt-5-nano` | 0.05 | 0.40 | ~$0.0002 | **mandatory**, efforts minimal→high | cheapest reasoning; "off" still thinks a little |
| `google/gemma-4-26b-a4b-it` | 0.06 | 0.33 | ~$0.0002 | optional (default off) | has a **`:free` variant** — $0, rate-limited; perfect for pipeline testing |
| `bytedance-seed/seed-2.0-mini` | 0.10 | 0.40 | ~$0.0003 | optional, effort levels | |
| `xiaomi/mimo-v2.5` | 0.105 | 0.28 | ~$0.0003 | optional | Apr 2026, 1M ctx |
| `google/gemini-2.5-flash-lite` | 0.10 | 0.40 | ~$0.0003 | optional | older but battle-tested |
| `qwen/qwen3-vl-8b-thinking` | 0.117 | 1.365 | ~$0.0007 | **mandatory** | dedicated thinking VLM; pricier output |

## Mid tier

| Model id | $/M in | $/M out | ~$/answer | Notes |
|---|---|---|---|---|
| `openai/gpt-5.4-nano` | 0.20 | 1.25 | ~$0.0008 | efforts none→xhigh |
| `google/gemini-3.1-flash-lite` | 0.25 | 1.50 | ~$0.001 | reasoning default minimal |
| `qwen/qwen3.5-plus-20260420` | 0.30 | 1.80 | ~$0.0012 | qwen3-vl-plus successor line |

## Premium (arbiter / spot-check tier)

| Model id | $/M in | $/M out | ~$/answer | Notes |
|---|---|---|---|---|
| `z-ai/glm-5v-turbo` | 1.20 | 4.00 | ~$0.0034 | |
| `google/gemini-3.5-flash` | 1.50 | 9.00 | ~$0.006 | reasoning mandatory |
| `anthropic/claude-sonnet-5` | 2.00 | 10.00 | ~$0.007 | efforts low→max; a 1,600-answer run ≈ $11 |

## How to use this

- **Method experiments**: make one method per candidate (Methods → duplicate, change
  model / reasoning level), run each on the same problem scope, compare on the
  assessment's **Analysis** tab (agreement-with-human is the metric that matters).
- **Reasoning level** in a method maps to the provider's effort control on
  OpenAI-style providers (incl. OpenRouter); `off` disables thinking where the model
  allows it. Higher effort = more output tokens = higher cost per answer.
- The `:free` variants cost nothing but are heavily rate-limited — fine for verifying
  masking/prompts, not for real runs.
- Direct-vendor alternative ids (non-OpenRouter providers): Qwen DashScope serves
  `qwen3-vl-plus` (validated live in this repo's opt-in test); Anthropic serves
  `claude-sonnet-5`.
- **Per-model pricing now lives in the Providers UI** (`model_pricing` table, one row per
  provider+model) — enter the $/Mtok numbers from this cheat-sheet there once; the app
  uses them to compute `cost_usd` on every graded record and to show pre-flight cost
  estimates at run creation (DECISIONS D35).
