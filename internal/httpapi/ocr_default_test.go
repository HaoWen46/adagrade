// Cloud opt-in default (privacy audit 2026-07-12): the cloud identify rung is
// opt-IN — a scan-batch upload that never mentions ocr_enabled must create a
// batch with the cloud step OFF. Before this change an absent field meant ON,
// which both shipped identity crops to a cloud provider by default and (after
// the empty-provider guard) made a bare upload 400.
package httpapi

import (
	"net/http"
	"testing"
)

func TestCreateScanBatch_AbsentOCREnabledDefaultsOff(t *testing.T) {
	env, c, aid, _ := scanSetup(t)

	// No ocr_enabled field, no provider: must succeed (no ErrOCRProviderRequired
	// 400 — the cloud step simply stays off) and record ocr_enabled=false.
	view := uploadLooseFilesExpect(t, c, env.ts, aid, []string{"plain.pdf"}, nil, http.StatusOK)
	batch := view["batch"].(map[string]any)
	if v, ok := batch["ocr_enabled"].(bool); !ok || v {
		t.Fatalf("absent ocr_enabled: batch ocr_enabled got %v want false", batch["ocr_enabled"])
	}
}

func TestCreateScanBatch_OCREnabledExplicitValues(t *testing.T) {
	env, c, aid, _ := scanSetup(t)

	// Explicit "1" (what the SPA sends) turns the cloud step on.
	view := uploadLooseFilesExpect(t, c, env.ts, aid, []string{"on.pdf"}, map[string]string{
		"ocr_enabled": "1", "ocr_provider": "p", "ocr_model": "m",
	}, http.StatusOK)
	if v := view["batch"].(map[string]any)["ocr_enabled"].(bool); !v {
		t.Fatalf(`ocr_enabled="1": batch ocr_enabled got false want true`)
	}

	// "true" is accepted as well (curl-friendly).
	view = uploadLooseFilesExpect(t, c, env.ts, aid, []string{"on2.pdf"}, map[string]string{
		"ocr_enabled": "true", "ocr_provider": "p",
	}, http.StatusOK)
	if v := view["batch"].(map[string]any)["ocr_enabled"].(bool); !v {
		t.Fatalf(`ocr_enabled="true": batch ocr_enabled got false want true`)
	}

	// Explicit "0" stays off.
	view = uploadLooseFilesExpect(t, c, env.ts, aid, []string{"off.pdf"}, map[string]string{
		"ocr_enabled": "0",
	}, http.StatusOK)
	if v := view["batch"].(map[string]any)["ocr_enabled"].(bool); v {
		t.Fatalf(`ocr_enabled="0": batch ocr_enabled got true want false`)
	}
}
