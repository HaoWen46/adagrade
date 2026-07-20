package scan

import (
	"testing"

	"github.com/HaoWen46/adagrade/internal/ocr"
)

// ---- PickID ----

func TestPickID(t *testing.T) {
	cases := []struct {
		name  string
		lines []ocr.Line
		want  string
	}{
		{
			name:  "single mixed line ID + CJK name",
			lines: []ocr.Line{{Text: "B11902156 王小明", Confidence: 0.9}},
			want:  "B11902156",
		},
		{
			name:  "full-width digits and letter fold to ASCII before extraction",
			lines: []ocr.Line{{Text: "Ｂ１１９０２１５６", Confidence: 0.9}},
			want:  "B11902156",
		},
		{
			name: "two lines: ID line and name line",
			lines: []ocr.Line{
				{Text: "B11902156", Confidence: 0.9},
				{Text: "王小明", Confidence: 0.9},
			},
			want: "B11902156",
		},
		{
			name:  "noise line with only 3 alnum chars is not picked (below length 5)",
			lines: []ocr.Line{{Text: "abc", Confidence: 0.9}},
			want:  "",
		},
		{
			name:  "empty input",
			lines: nil,
			want:  "",
		},
		{
			name:  "run breaks on internal space, longest run wins",
			lines: []ocr.Line{{Text: "B1 19021 56789", Confidence: 0.9}},
			want:  "19021", // "B1"(2) and "56789"(5) and "19021"(5); "19021" first among len-5 ties
		},
		{
			name: "ties broken by higher confidence line",
			lines: []ocr.Line{
				{Text: "AAAAA", Confidence: 0.5},
				{Text: "BBBBB", Confidence: 0.95},
			},
			want: "BBBBB",
		},
		{
			name: "ties broken by first line when confidence equal",
			lines: []ocr.Line{
				{Text: "AAAAA", Confidence: 0.8},
				{Text: "BBBBB", Confidence: 0.8},
			},
			want: "AAAAA",
		},
		{
			name:  "hyphen breaks the run (no gluing across punctuation)",
			lines: []ocr.Line{{Text: "B119-02156", Confidence: 0.9}},
			want:  "02156", // "B119"(4, too short) then "02156"(5)
		},
		{
			name:  "CJK adjacent to ID does not extend the alnum run",
			lines: []ocr.Line{{Text: "王小明B11902156", Confidence: 0.9}},
			want:  "B11902156",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PickID(tc.lines); got != tc.want {
				t.Errorf("PickID(%+v) = %q, want %q", tc.lines, got, tc.want)
			}
		})
	}
}

// ---- PickName ----

func TestPickName(t *testing.T) {
	cases := []struct {
		name  string
		lines []ocr.Line
		want  string
	}{
		{
			name:  "single mixed line ID + CJK name",
			lines: []ocr.Line{{Text: "B11902156 王小明", Confidence: 0.9}},
			want:  "王小明",
		},
		{
			name: "two lines: ID line and name line",
			lines: []ocr.Line{
				{Text: "B11902156", Confidence: 0.9},
				{Text: "王小明", Confidence: 0.9},
			},
			want: "王小明",
		},
		{
			name:  "single Han char below length 2 is not picked",
			lines: []ocr.Line{{Text: "王", Confidence: 0.9}},
			want:  "",
		},
		{
			name:  "empty input",
			lines: nil,
			want:  "",
		},
		{
			name: "ties broken by higher confidence line",
			lines: []ocr.Line{
				{Text: "李小龍", Confidence: 0.4},
				{Text: "王小明", Confidence: 0.9},
			},
			want: "王小明",
		},
		{
			name:  "run breaks on non-Han rune (ASCII digits between Han runs); tie -> first run wins",
			lines: []ocr.Line{{Text: "王小123明德", Confidence: 0.9}},
			want:  "王小", // "王小"(2) vs "明德"(2): tie on confidence (same line) -> first run wins
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PickName(tc.lines); got != tc.want {
				t.Errorf("PickName(%+v) = %q, want %q", tc.lines, got, tc.want)
			}
		})
	}
}
