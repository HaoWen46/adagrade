package httpapi

import (
	"fmt"
	"net/http"
	"testing"
)

// TestListAudit_RBACFiltersAndPagination pins the HTTP surface of the trust
// spec §6/D39 audit read path: admin-only, newest-first, target_kind/target_id/
// action/actor filters, limit/offset pagination.
func TestListAudit_RBACFiltersAndPagination(t *testing.T) {
	env, admin, aid, _, _ := phase4Setup(t) // phase4Setup logs in as "lect@ntu.edu.tw" (lecturer)
	ta := loginAs(t, env.ts, env.st, "ta-audit@ntu.edu.tw", "ta")

	// Non-admin (lecturer, and TA) is forbidden.
	resp, err := admin.Get(fmt.Sprintf("%s/api/audit", env.ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("lecturer GET /api/audit: got %d want 403", resp.StatusCode)
	}
	resp, err = ta.Get(fmt.Sprintf("%s/api/audit", env.ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ta GET /api/audit: got %d want 403", resp.StatusCode)
	}

	adminClient := loginAs(t, env.ts, env.st, "admin-audit@ntu.edu.tw", "admin")

	// Generate a controlled fixture via InsertAudit directly, using a target_kind
	// ("dist_test_target") that nothing else in the harness writes — phase4Setup's
	// own setup (assessment.create, submissions.upload, ...) already leaves audit
	// rows behind, so scoping to a distinctive target keeps counts exact and this
	// test unaffected by unrelated handlers' audit action strings.
	_ = aid
	if err := env.st.InsertAudit(t.Context(), 0, "test.one", "dist_test_target", "1", map[string]any{"n": 1}); err != nil {
		t.Fatalf("InsertAudit 1: %v", err)
	}
	if err := env.st.InsertAudit(t.Context(), 0, "test.two", "dist_test_target", "1", nil); err != nil {
		t.Fatalf("InsertAudit 2: %v", err)
	}
	if err := env.st.InsertAudit(t.Context(), 0, "test.three", "dist_test_target", "2", nil); err != nil {
		t.Fatalf("InsertAudit 3: %v", err)
	}

	got := getJSON[map[string]any](t, adminClient, env.ts.URL+"/api/audit?target_kind=dist_test_target", http.StatusOK)
	entries, ok := got["entries"].([]any)
	if !ok || len(entries) != 3 {
		t.Fatalf("entries: %v", got)
	}
	// Newest first: test.three was inserted last.
	first := entries[0].(map[string]any)
	if first["action"] != "test.three" {
		t.Fatalf("expected newest-first, got %v", first)
	}

	// target_kind + target_id filter (narrows to target_id=1: test.one, test.two).
	byTarget := getJSON[map[string]any](t, adminClient, env.ts.URL+"/api/audit?target_kind=dist_test_target&target_id=1", http.StatusOK)
	targetEntries := byTarget["entries"].([]any)
	if len(targetEntries) != 2 {
		t.Fatalf("target filter: got %d want 2: %v", len(targetEntries), targetEntries)
	}

	// action filter.
	byAction := getJSON[map[string]any](t, adminClient, env.ts.URL+"/api/audit?target_kind=dist_test_target&action=test.one", http.StatusOK)
	actionEntries := byAction["entries"].([]any)
	if len(actionEntries) != 1 {
		t.Fatalf("action filter: got %d want 1: %v", len(actionEntries), actionEntries)
	}
	one := actionEntries[0].(map[string]any)
	detail, _ := one["detail"].(map[string]any)
	if detail["n"] != float64(1) {
		t.Errorf("detail JSONB passthrough: got %v", one["detail"])
	}

	// pagination: limit=1 offset=1 on target_id=1 rows (test.two, test.one in
	// newest-first order) returns exactly the second one.
	paged := getJSON[map[string]any](t, adminClient, env.ts.URL+"/api/audit?target_kind=dist_test_target&target_id=1&limit=1&offset=1", http.StatusOK)
	pagedEntries := paged["entries"].([]any)
	if len(pagedEntries) != 1 {
		t.Fatalf("paged: got %d want 1: %v", len(pagedEntries), pagedEntries)
	}
	if pagedEntries[0].(map[string]any)["action"] != "test.one" {
		t.Errorf("paged[0]: got %v want test.one", pagedEntries[0])
	}
}
