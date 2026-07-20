package grading

import "testing"

func TestSnapClamp(t *testing.T) {
	cases := []struct {
		name      string
		score     string
		max       string
		increment string
		want      string
		adjusted  bool
	}{
		{"exact multiple untouched", "3.5", "6", "0.5", "3.5", false},
		{"integer untouched", "4", "6", "0.5", "4", false},
		{"snap down", "3.2", "6", "0.5", "3", true},
		{"snap up", "3.3", "6", "0.5", "3.5", true},
		{"snap half rounds away from zero", "3.25", "6", "0.5", "3.5", true},
		{"clamp above max", "7", "6", "0.5", "6", true},
		{"clamp negative", "-1", "6", "0.5", "0", true},
		{"snap then clamp", "6.4", "6", "0.5", "6", true},
		{"quarter increment", "2.3", "10", "0.25", "2.25", true},
		{"increment 1", "2.5", "10", "1", "3", true},
		{"zero stays zero", "0", "10", "0.5", "0", false},
		{"max stays max", "6", "6", "0.5", "6", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, adjusted, err := SnapClamp(c.score, c.max, c.increment)
			if err != nil {
				t.Fatalf("SnapClamp(%s, %s, %s): %v", c.score, c.max, c.increment, err)
			}
			if got != c.want || adjusted != c.adjusted {
				t.Errorf("SnapClamp(%s, max=%s, inc=%s): got (%s, %v) want (%s, %v)",
					c.score, c.max, c.increment, got, adjusted, c.want, c.adjusted)
			}
		})
	}
}

func TestSnapClamp_Errors(t *testing.T) {
	if _, _, err := SnapClamp("abc", "6", "0.5"); err == nil {
		t.Error("garbage score should error")
	}
	if _, _, err := SnapClamp("3", "6", "0"); err == nil {
		t.Error("zero increment should error")
	}
	if _, _, err := SnapClamp("3", "6", "-0.5"); err == nil {
		t.Error("negative increment should error")
	}
}

func TestSumDecimalStrings(t *testing.T) {
	got, err := SumDecimals([]string{"3.5", "4", "0.25"})
	if err != nil || got != "7.75" {
		t.Errorf("SumDecimals: got %q err %v", got, err)
	}
	got, err = SumDecimals(nil)
	if err != nil || got != "0" {
		t.Errorf("SumDecimals(nil): got %q err %v", got, err)
	}
}
