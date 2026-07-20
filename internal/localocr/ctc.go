package localocr

import "math"

// ctcGreedyDecode performs greedy CTC decoding of a per-step class-probability
// matrix into text plus a mean-probability confidence.
//
// rows is [T][C]: T timesteps, C classes. PP-OCR convention (docs/DECISIONS.md
// D24): class 0 is the CTC blank; class i (1..len(keys)) maps to keys[i-1].
// Some ONNX exports append a trailing space class, giving C = len(keys)+2 with
// the final (highest) class == ' '; pass trailingSpace=true for that variant.
//
// Decoding is standard greedy CTC: take the argmax class at each step, collapse
// runs of the same class, then drop blanks. Confidence is the mean of the
// argmax probability over the EMITTED steps only — the first step of each
// non-blank run — so repeated and blank steps do not dilute or inflate it. An
// empty emission has confidence 0.
//
// rows is assumed already softmaxed (the caller normalizes via softmaxRows when
// looksSoftmaxed reports otherwise), so row values are treated as probabilities.
func ctcGreedyDecode(rows [][]float32, keys []rune, trailingSpace bool) (string, float64) {
	var out []rune
	var probSum float64
	var emitted int

	prev := -1 // previous EMITTED class index (for repeat collapsing)
	for _, row := range rows {
		cls, p := argmax(row)
		if cls == prev {
			// Repeat of the last argmax: collapse (CTC), do not emit.
			continue
		}
		prev = cls
		if cls == 0 {
			// Blank: separates repeats, never emitted.
			continue
		}
		r, ok := classToRune(cls, keys, trailingSpace)
		if !ok {
			// Class index outside the dictionary (defensive): skip it rather
			// than index out of range. Never logged (could encode content).
			continue
		}
		out = append(out, r)
		probSum += float64(p)
		emitted++
	}

	if emitted == 0 {
		return "", 0
	}
	return string(out), probSum / float64(emitted)
}

// classToRune maps a CTC class index (>=1, blank already excluded) to its rune.
// Base classes 1..len(keys) map to keys[cls-1]. When trailingSpace is set, the
// one extra class at index len(keys)+1 maps to ' '.
func classToRune(cls int, keys []rune, trailingSpace bool) (rune, bool) {
	if cls >= 1 && cls <= len(keys) {
		return keys[cls-1], true
	}
	if trailingSpace && cls == len(keys)+1 {
		return ' ', true
	}
	return 0, false
}

// argmax returns the index and value of the largest element in row.
func argmax(row []float32) (int, float32) {
	best := 0
	bestV := float32(math.Inf(-1))
	for i, v := range row {
		if v > bestV {
			bestV = v
			best = i
		}
	}
	return best, bestV
}

// looksSoftmaxed reports whether a single output row already looks like a
// probability distribution: every value in [0,1] and the total near 1. PP-OCRv4
// rec exports usually end in a Softmax node, but some do not; the caller uses
// this on the first step to decide whether to apply softmaxRows.
func looksSoftmaxed(row []float32) bool {
	var sum float64
	for _, v := range row {
		if v < -1e-3 || v > 1+1e-3 {
			return false
		}
		sum += float64(v)
	}
	return math.Abs(sum-1.0) < 1e-2
}

// softmaxRows applies a numerically-stable softmax to each row independently,
// returning a fresh matrix. Used only when looksSoftmaxed is false.
func softmaxRows(rows [][]float32) [][]float32 {
	out := make([][]float32, len(rows))
	for i, row := range rows {
		out[i] = softmax(row)
	}
	return out
}

func softmax(row []float32) []float32 {
	max := float32(math.Inf(-1))
	for _, v := range row {
		if v > max {
			max = v
		}
	}
	out := make([]float32, len(row))
	var sum float64
	for i, v := range row {
		e := math.Exp(float64(v - max))
		out[i] = float32(e)
		sum += e
	}
	if sum == 0 {
		return out
	}
	for i := range out {
		out[i] = float32(float64(out[i]) / sum)
	}
	return out
}
