package grading

import "testing"

// baseConfigJSON returns a minimal-valid method config with the given policy JSON
// fragment spliced in (pass "" for no policy key at all).
func baseConfigJSON(policyFrag string) []byte {
	return []byte(`{"provider":"fake","model":"m","prompt_template_version_id":1` + policyFrag + `}`)
}

func TestParseMethodConfig_Policy(t *testing.T) {
	tests := []struct {
		name       string
		frag       string
		wantPolicy string
		wantErr    bool
	}{
		{"empty defaults to standard", "", PolicyStandard, false},
		{"explicit lenient", `,"policy":"lenient"`, PolicyLenient, false},
		{"explicit standard", `,"policy":"standard"`, PolicyStandard, false},
		{"explicit strict", `,"policy":"strict"`, PolicyStrict, false},
		{"invalid value rejected", `,"policy":"harsh"`, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseMethodConfig(baseConfigJSON(tc.frag))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (policy=%q)", cfg.Policy)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Policy != tc.wantPolicy {
				t.Errorf("policy = %q, want %q", cfg.Policy, tc.wantPolicy)
			}
		})
	}
}

func TestParseMethodConfig_InvalidPolicyNamesValue(t *testing.T) {
	_, err := ParseMethodConfig(baseConfigJSON(`,"policy":"harsh"`))
	if err == nil {
		t.Fatal("expected error for bad policy")
	}
	if !contains(err.Error(), "harsh") {
		t.Errorf("error should name the bad value; got %q", err.Error())
	}
}

func TestValidPolicy(t *testing.T) {
	for _, ok := range []string{PolicyLenient, PolicyStandard, PolicyStrict} {
		if !ValidPolicy(ok) {
			t.Errorf("ValidPolicy(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "harsh", "Lenient", "STRICT"} {
		if ValidPolicy(bad) {
			t.Errorf("ValidPolicy(%q) = true, want false", bad)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
