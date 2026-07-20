package scan

import "testing"

func TestParseProblemRef(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
		ok   bool
	}{
		{"bare number", "3", 3, true},
		{"q prefix", "Q1", 1, true},
		{"lower q", "q12", 12, true},
		{"p prefix", "P4", 4, true},
		{"cjk wen prefix", "問2", 2, true},
		{"cjk di-ti wrap", "第4題", 4, true},
		{"hash prefix", "#5", 5, true},
		{"trailing dot", "3.", 3, true},
		{"trailing paren", "2)", 2, true},
		{"fullwidth digit folds", "Ｑ６", 6, true},
		{"surrounding space", " Q 7 ", 7, true},
		{"empty", "", 0, false},
		{"prefix only", "Q", 0, false},
		{"zero", "Q0", 0, false},
		{"letters after digits", "1a", 0, false},
		{"two numbers", "1 2", 0, false},
		{"name noise", "王小明", 0, false},
		{"absurdly long", "12345", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseProblemRef(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("ParseProblemRef(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}
