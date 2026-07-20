package store

import "testing"

// TestCostUSD_TokensTimesPricing pins the core cost formula: (tokens / 1e6) *
// $/Mtok for input and output, summed, computed via big.Rat (never float64) and
// rounded to NUMERIC(10,6) — the grading_records.cost_usd column's scale.
func TestCostUSD_TokensTimesPricing(t *testing.T) {
	cases := []struct {
		name                  string
		inputTok, outputTok   int64
		inPerMtok, outPerMtok string
		want                  string
	}{
		// MODELS.md heuristic case from the spec: 1500 in + 400 out tokens.
		{"heuristic-shape", 1500, 400, "3.00", "15.00", "0.0105"},
		{"zero-tokens", 0, 0, "3.00", "15.00", "0"},
		{"zero-price", 1500, 400, "0", "0", "0"},
		// Rounding: exercise a case that doesn't terminate cleanly at 6 dp.
		{"rounds-to-6dp", 1, 1, "1.00", "1.00", "0.000002"},
		{"rounds-to-6dp-again", 333333, 0, "3.00", "0", "0.999999"},
		{"large-run", 1_000_000, 1_000_000, "3.50", "10.50", "14"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, err := Num(c.inPerMtok)
			if err != nil {
				t.Fatalf("Num(in): %v", err)
			}
			out, err := Num(c.outPerMtok)
			if err != nil {
				t.Fatalf("Num(out): %v", err)
			}
			got := CostUSD(c.inputTok, c.outputTok, in, out)
			if gotStr := NumStr(got); gotStr != c.want {
				t.Errorf("CostUSD(%d, %d, %s, %s) = %q, want %q",
					c.inputTok, c.outputTok, c.inPerMtok, c.outPerMtok, gotStr, c.want)
			}
		})
	}
}

// TestCostUSD_InvalidPricingIsNull pins "absence is visible, not zero" (trust spec
// §2): an invalid/NULL pricing numeric on either side must produce a NULL result,
// never a fake $0 cost.
func TestCostUSD_InvalidPricingIsNull(t *testing.T) {
	valid, err := Num("3.00")
	if err != nil {
		t.Fatalf("Num: %v", err)
	}

	got := CostUSD(1500, 400, zeroNumeric(), valid)
	if got.Valid {
		t.Errorf("CostUSD with invalid input price: got valid %q, want NULL", NumStr(got))
	}
	got = CostUSD(1500, 400, valid, zeroNumeric())
	if got.Valid {
		t.Errorf("CostUSD with invalid output price: got valid %q, want NULL", NumStr(got))
	}
}

// TestEstimateCostUSD_AnswersTimesHeuristicTimesPricing pins the run-creation
// pre-flight estimate (trust spec §3): answers x (1500 in + 400 out) x pricing.
func TestEstimateCostUSD_AnswersTimesHeuristicTimesPricing(t *testing.T) {
	in, err := Num("3.00")
	if err != nil {
		t.Fatalf("Num(in): %v", err)
	}
	out, err := Num("15.00")
	if err != nil {
		t.Fatalf("Num(out): %v", err)
	}

	// 40 answers: 40*1500=60000 input, 40*400=16000 output.
	est, ok := EstimateCostUSD(40, in, out)
	if !ok {
		t.Fatalf("EstimateCostUSD: expected ok=true with valid pricing")
	}
	// Cross-check against CostUSD directly for the same scaled token counts.
	want := CostUSD(60000, 16000, in, out)
	if NumStr(est) != NumStr(want) {
		t.Errorf("EstimateCostUSD(40, ...) = %q, want %q", NumStr(est), NumStr(want))
	}

	zero, ok := EstimateCostUSD(0, in, out)
	if !ok || NumStr(zero) != "0" {
		t.Errorf("EstimateCostUSD(0, ...) = %q ok=%v, want 0 true", NumStr(zero), ok)
	}
}

// TestEstimateCostUSD_MissingPricingIsUnknown pins "unknown, never a fake $0"
// (trust spec §3): when either price is unresolved, ok must be false so the
// caller renders "unknown" rather than treating a zero-valued Numeric as real.
func TestEstimateCostUSD_MissingPricingIsUnknown(t *testing.T) {
	valid, err := Num("3.00")
	if err != nil {
		t.Fatalf("Num: %v", err)
	}
	if _, ok := EstimateCostUSD(40, zeroNumeric(), valid); ok {
		t.Error("EstimateCostUSD with missing input price: expected ok=false")
	}
	if _, ok := EstimateCostUSD(40, valid, zeroNumeric()); ok {
		t.Error("EstimateCostUSD with missing output price: expected ok=false")
	}
}
