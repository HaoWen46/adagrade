// Decimal-string arithmetic for point values (docs/DECISIONS.md D4).
//
// Points travel as decimal STRINGS ("10", "2.5") end-to-end; floats would drift
// (0.1 + 0.2 !== 0.3). We parse strings into integer cents (2 decimal places —
// matching the 0.5-step grading scale) and do exact integer math.

/** Parses a decimal string with at most 2 fraction digits into integer cents. */
export function toCents(s: string): number | null {
  const m = /^(-?)(\d+)(?:\.(\d{1,2}))?$/.exec(s.trim());
  if (!m) return null;
  const cents = parseInt(m[2], 10) * 100 + (m[3] ? parseInt(m[3].padEnd(2, "0"), 10) : 0);
  return m[1] === "-" ? -cents : cents;
}

/** Formats integer cents back into a minimal decimal string ("250" → "2.5"). */
export function centsToString(cents: number): string {
  const sign = cents < 0 ? "-" : "";
  const abs = Math.abs(cents);
  const whole = Math.floor(abs / 100);
  const frac = abs % 100;
  if (frac === 0) return `${sign}${whole}`;
  if (frac % 10 === 0) return `${sign}${whole}.${frac / 10}`;
  return `${sign}${whole}.${String(frac).padStart(2, "0")}`;
}

/** Adds two decimal strings exactly; null when either is invalid. */
export function addDecimalStrings(a: string, b: string): string | null {
  const ca = toCents(a);
  const cb = toCents(b);
  if (ca === null || cb === null) return null;
  return centsToString(ca + cb);
}

/** Sums decimal strings exactly; null when any entry is invalid. */
export function sumDecimalStrings(values: string[]): string | null {
  let total = 0;
  for (const v of values) {
    const c = toCents(v);
    if (c === null) return null;
    total += c;
  }
  return centsToString(total);
}

/** Numeric equality on decimal strings ("10" equals "10.0"). */
export function decimalEquals(a: string, b: string): boolean {
  const ca = toCents(a);
  const cb = toCents(b);
  return ca !== null && cb !== null && ca === cb;
}

// =====================================================================================
// --- Task S3: money helpers (cost_usd NUMERIC(10,6), $/Mtok pricing NUMERIC(10,4)) ----
// Points above are always 2dp; money needs more precision (pricing/cost estimates), so
// these use BigInt micros (1e6 scale) rather than toCents' integer-cents. Never
// parseFloat on money — see CLAUDE.md.
// =====================================================================================

const MICROS = 1_000_000n;

/** Parses a non-negative decimal string (any fraction length) into integer micros. */
export function toMicros(s: string): bigint | null {
  const m = /^(\d+)(?:\.(\d+))?$/.exec(s.trim());
  if (!m) return null;
  const whole = BigInt(m[1]);
  const fracDigits = (m[2] ?? "").slice(0, 6).padEnd(6, "0");
  const overflow = (m[2] ?? "").slice(6);
  // Truncate (never round up) any precision finer than micros — irrelevant for USD.
  void overflow;
  return whole * MICROS + BigInt(fracDigits || "0");
}

/** Formats integer micros back into a minimal decimal string ("21000" -> "0.021"). */
export function microsToString(micros: bigint): string {
  const neg = micros < 0n;
  const abs = neg ? -micros : micros;
  const whole = abs / MICROS;
  const frac = abs % MICROS;
  let fracStr = frac.toString().padStart(6, "0").replace(/0+$/, "");
  const out = fracStr ? `${whole}.${fracStr}` : `${whole}`;
  return neg ? `-${out}` : out;
}

/**
 * Pre-flight cost estimate: answers × (1500 in + 400 out tokens) × $/Mtok pricing —
 * mirrors internal/store/pricing.go EstimateCostUSD exactly (same heuristic constants),
 * computed client-side in integer micros so the create-run dialog can show a number
 * before the server round-trip. Returns null when pricing is missing/invalid — the
 * caller must render "unknown", never a fake $0 (D35).
 */
export function estimateRunCostUSD(
  answers: number,
  inputUsdPerMtok: string,
  outputUsdPerMtok: string,
): string | null {
  if (!Number.isFinite(answers) || answers < 0) return null;
  const inRate = toMicros(inputUsdPerMtok);
  const outRate = toMicros(outputUsdPerMtok);
  if (inRate === null || outRate === null) return null;
  const inputTokens = BigInt(Math.round(answers)) * 1500n;
  const outputTokens = BigInt(Math.round(answers)) * 400n;
  // cost = tokens/1e6 * rate(micros); tokens and rate are both already scaled, so
  // divide once by 1e6 (Mtok) after multiplying — integer division truncates, which
  // is fine for a display estimate (the server computes the authoritative rounded value).
  const totalMicros = (inputTokens * inRate + outputTokens * outRate) / 1_000_000n;
  return microsToString(totalMicros);
}

/** True when a decimal-string USD amount is >= $0.01 (the cost_cap_usd NUMERIC(10,2) floor —
 * sub-cent caps round to $0 and kill the run fail-closed, per the money-review handoff). */
export function meetsMinCostCap(s: string): boolean {
  const micros = toMicros(s);
  return micros !== null && micros >= 10_000n; // $0.01 = 10,000 micros
}
