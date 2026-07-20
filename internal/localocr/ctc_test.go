package localocr

import (
	"math"
	"testing"
)

// keys3 is a tiny dictionary for decoder table tests: dict index i maps to
// class i+1 (PP-OCR convention, class 0 = blank).
//
//	blank -> 0
//	'a'   -> 1
//	'b'   -> 2
//	'c'   -> 3
var keys3 = []rune{'a', 'b', 'c'}

// logitsFrom builds a [T, C] argmax-dominated probability matrix: for each
// step it puts prob p on the winning class and spreads the remainder evenly.
// C is len(keys3)+1 = 4 unless overridden by wide.
func logitsFrom(t *testing.T, wins []int, p float64, c int) [][]float32 {
	t.Helper()
	out := make([][]float32, len(wins))
	for i, w := range wins {
		row := make([]float32, c)
		rem := (1 - p) / float64(c-1)
		for j := range row {
			row[j] = float32(rem)
		}
		row[w] = float32(p)
		out[i] = row
	}
	return out
}

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestCTCDecode_CollapseRepeatsAndBlanks(t *testing.T) {
	// steps: a a blank a  -> "aa" (repeat collapses, blank separates the
	// third 'a' so it is a new emission): classes 1,1,0,1
	rows := logitsFrom(t, []int{1, 1, 0, 1}, 0.9, 4)
	text, conf := ctcGreedyDecode(rows, keys3, false)
	if text != "aa" {
		t.Fatalf("text: got %q want %q", text, "aa")
	}
	// confidence = mean of argmax probs over EMITTED steps only (the two
	// non-collapsed 'a's and... the blank is dropped, the repeated 'a' is
	// collapsed) => emitted steps are step0 and step3, both p=0.9.
	if !approx(conf, 0.9, 1e-6) {
		t.Fatalf("conf: got %v want ~0.9", conf)
	}
}

func TestCTCDecode_SimpleWord(t *testing.T) {
	// a b c -> "abc": classes 1,2,3
	rows := logitsFrom(t, []int{1, 2, 3}, 0.8, 4)
	text, conf := ctcGreedyDecode(rows, keys3, false)
	if text != "abc" {
		t.Fatalf("text: got %q want %q", text, "abc")
	}
	if !approx(conf, 0.8, 1e-6) {
		t.Fatalf("conf: got %v want ~0.8", conf)
	}
}

func TestCTCDecode_AllBlankIsEmpty(t *testing.T) {
	rows := logitsFrom(t, []int{0, 0, 0}, 0.99, 4)
	text, conf := ctcGreedyDecode(rows, keys3, false)
	if text != "" {
		t.Fatalf("text: got %q want empty", text)
	}
	if conf != 0 {
		t.Fatalf("conf: got %v want 0 for empty emission", conf)
	}
}

func TestCTCDecode_IndexShift(t *testing.T) {
	// Verify the +1 shift: class 1 must map to keys3[0]='a', NOT keys3[1].
	rows := logitsFrom(t, []int{2}, 0.7, 4) // class 2 -> keys3[1]='b'
	text, _ := ctcGreedyDecode(rows, keys3, false)
	if text != "b" {
		t.Fatalf("index shift wrong: class 2 should be 'b', got %q", text)
	}
}

func TestCTCDecode_TrailingSpaceClassVariant(t *testing.T) {
	// C = len(keys)+2 = 5. Convention: class 0 = blank, classes 1..3 = keys,
	// class 4 (the final/highest) = ' '. Decode "a b" via classes 1,4,2.
	rows := logitsFrom(t, []int{1, 4, 2}, 0.9, 5)
	text, conf := ctcGreedyDecode(rows, keys3, true)
	if text != "a b" {
		t.Fatalf("trailing-space variant: got %q want %q", text, "a b")
	}
	if !approx(conf, 0.9, 1e-6) {
		t.Fatalf("conf: got %v want ~0.9", conf)
	}
}

func TestCTCDecode_ConfidenceIsMeanOfEmitted(t *testing.T) {
	// classes 1,1,2 with differing probs: emitted are step0 (first 'a',
	// p=0.6) and step2 ('b', p=0.9). step1 is a collapsed repeat -> excluded.
	rows := [][]float32{
		{0.1, 0.6, 0.3, 0.0},   // 'a' p=0.6
		{0.1, 0.8, 0.1, 0.0},   // 'a' repeat p=0.8 (collapsed, excluded)
		{0.05, 0.05, 0.9, 0.0}, // 'b' p=0.9
	}
	text, conf := ctcGreedyDecode(rows, keys3, false)
	if text != "ab" {
		t.Fatalf("text: got %q want %q", text, "ab")
	}
	want := (0.6 + 0.9) / 2
	if !approx(conf, want, 1e-6) {
		t.Fatalf("conf: got %v want %v", conf, want)
	}
}

func TestSoftmaxRows_NormalizesUnsoftmaxedLogits(t *testing.T) {
	// Raw logits (not summing to 1, large magnitudes) -> softmax per row.
	raw := [][]float32{{2, 1, 0, -1}}
	out := softmaxRows(raw)
	var sum float64
	for _, v := range out[0] {
		sum += float64(v)
	}
	if !approx(sum, 1.0, 1e-5) {
		t.Fatalf("softmax row should sum to 1, got %v", sum)
	}
	// argmax preserved: class 0 had the largest logit.
	max := 0
	for i, v := range out[0] {
		if v > out[0][max] {
			max = i
		}
		_ = i
	}
	if max != 0 {
		t.Fatalf("softmax should preserve argmax at 0, got %d", max)
	}
}

func TestLooksSoftmaxed(t *testing.T) {
	softmaxed := []float32{0.1, 0.6, 0.3}
	if !looksSoftmaxed(softmaxed) {
		t.Errorf("row summing to 1 in [0,1] should look softmaxed")
	}
	raw := []float32{2.5, -3.1, 8.0}
	if looksSoftmaxed(raw) {
		t.Errorf("raw logits should not look softmaxed")
	}
}
