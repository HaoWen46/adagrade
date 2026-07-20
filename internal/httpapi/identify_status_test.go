// Tests for GET /api/identify/status (privacy loudness, 2026-07-12): the
// Identify tab needs to know whether the privacy-preserving local OCR rung
// (D24) is installed, because its absence silently routes every ID/name crop
// to a cloud provider. The response is availability-only — never filesystem
// paths.
package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/cookiejar"
	"testing"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/ocr"
)

// nopOCRReader is the smallest possible ocr.Reader — the endpoint only checks
// presence, never calls ReadLines.
type nopOCRReader struct{}

func (nopOCRReader) ReadLines(ctx context.Context, crop imaging.IDCrop) ([]ocr.Line, error) {
	return nil, nil
}

func TestIdentifyStatus_RequiresSession(t *testing.T) {
	ts, _, _ := harness(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.Get(ts.URL + "/api/identify/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("identify status without session: got %d want 401", resp.StatusCode)
	}
}

func TestIdentifyStatus_ReflectsLocalReader(t *testing.T) {
	env := harnessEnv(t)
	// TAs work the Identify tab too: any signed-in role may read this.
	c := loginAs(t, env.ts, env.st, "ta@ntu.edu.tw", "ta")

	// The harness wires no local reader — exactly a server booted without
	// ADAMARKER_OCR_MODEL.
	got := getJSON[map[string]any](t, c, env.ts.URL+"/api/identify/status", http.StatusOK)
	if v, ok := got["local_ocr_available"].(bool); !ok || v {
		t.Fatalf("local_ocr_available without reader: got %v want false", got["local_ocr_available"])
	}

	env.scans.Local = nopOCRReader{}
	got = getJSON[map[string]any](t, c, env.ts.URL+"/api/identify/status", http.StatusOK)
	if v, ok := got["local_ocr_available"].(bool); !ok || !v {
		t.Fatalf("local_ocr_available with reader: got %v want true", got["local_ocr_available"])
	}

	// Counts/booleans only: the body must never leak filesystem paths (the
	// model/keys/onnxruntime locations are operator config, not API surface).
	raw := getJSONRaw(t, c, env.ts.URL+"/api/identify/status")
	for _, needle := range []string{"path", "model", "/", "\\"} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Errorf("identify status body should not contain %q: %s", needle, raw)
		}
	}
}
