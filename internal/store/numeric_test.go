package store

import "testing"

func TestNum_RoundTripsDecimalStrings(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0", "0"},
		{"12.5", "12.5"},
		{"12.50", "12.5"}, // canonical: trailing fractional zeros trimmed
		{"3.00", "3"},
		{"100", "100"},
		{"0.25", "0.25"},
		{"-3.5", "-3.5"},
	}
	for _, c := range cases {
		n, err := Num(c.in)
		if err != nil {
			t.Errorf("Num(%q): unexpected error %v", c.in, err)
			continue
		}
		if got := NumStr(n); got != c.want {
			t.Errorf("NumStr(Num(%q)): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestNum_RejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "abc", "1.2.3", "NaN"} {
		if _, err := Num(in); err == nil {
			t.Errorf("Num(%q): expected error", in)
		}
	}
}

func TestNumStr_InvalidNumericIsEmpty(t *testing.T) {
	var n = zeroNumeric()
	if got := NumStr(n); got != "" {
		t.Errorf("NumStr(invalid): got %q want empty", got)
	}
}

func TestNumCmp(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.5", "1.50", 0},
		{"2", "1.99", 1},
		{"0.25", "0.5", -1},
		{"-1", "0", -1},
	}
	for _, c := range cases {
		a, _ := Num(c.a)
		b, _ := Num(c.b)
		if got := NumCmp(a, b); got != c.want {
			t.Errorf("NumCmp(%s, %s): got %d want %d", c.a, c.b, got, c.want)
		}
	}
}
