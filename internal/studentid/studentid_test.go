package studentid

import "testing"

// ---- Normalize ----

// Expectations moved verbatim from internal/scan's TestNormalizeID — the
// extraction must be byte-for-byte behavior-preserving.
func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already normalized", "B10902066", "B10902066"},
		{"lowercase folds to upper", "b10902066", "B10902066"},
		{"full-width digits and letter fold to ASCII", "ｂ１０９０２０６６", "B10902066"},
		{"hyphen stripped", "B109-02066", "B10902066"},
		{"internal whitespace stripped", "B109 02066", "B10902066"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---- NormalizeName ----

// Expectations moved verbatim from internal/scan's TestNormalizeName.
func TestNormalizeName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"CJK name with ASCII spaces", "王 小 明", "王小明"},
		{"CJK name with ideographic space U+3000", "王　小　明", "王小明"},
		{"CJK name no spaces", "王小明", "王小明"},
		{"Latin name case-folds", "John Smith", "johnsmith"},
		{"Latin name mixed case and spaces", "  JoHn   SMITH ", "johnsmith"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeName(tc.in); got != tc.want {
				t.Errorf("NormalizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
