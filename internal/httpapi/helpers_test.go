package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// putJSONRaw sends a JSON PUT with the CSRF header (like the SPA does).
func putJSONRaw(t *testing.T, c *http.Client, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// putJSON PUTs and decodes the JSON body, failing the test on an unexpected status.
func putJSON(t *testing.T, c *http.Client, url string, body any, wantStatus int) map[string]any {
	t.Helper()
	resp := putJSONRaw(t, c, url, body)
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		var eb bytes.Buffer
		_, _ = eb.ReadFrom(resp.Body)
		t.Fatalf("PUT %s: got %d want %d — %s", url, resp.StatusCode, wantStatus, eb.String())
	}
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v
}

// decodeJSONResp decodes an already-received response body into v (for tests that
// need to inspect a non-2xx response's JSON envelope, e.g. a 409's payload).
func decodeJSONResp(t *testing.T, resp *http.Response, v any) error {
	t.Helper()
	return json.NewDecoder(resp.Body).Decode(v)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type storeSeeder = *store.Store

func seedRole(t *testing.T, st *store.Store, email, role string) {
	t.Helper()
	if _, err := st.Q.CreateUser(t.Context(), db.CreateUserParams{Email: email, Role: role, Active: true}); err != nil {
		t.Fatalf("seed user %s: %v", role, err)
	}
}

func seedStudent(t *testing.T, st *store.Store, sid, name, email string) {
	t.Helper()
	if _, err := st.Q.UpsertStudent(t.Context(), db.UpsertStudentParams{StudentID: sid, Name: name, Email: email}); err != nil {
		t.Fatalf("seed student: %v", err)
	}
}

func mustExec(t *testing.T, st *store.Store, sql string, args ...any) {
	t.Helper()
	if _, err := st.Pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// driveDirectUploads runs IngestDirectUpload for every pending (finished_at NULL)
// direct_uploads row of the assessment, as the ingest.direct River worker would
// (D27, F1). handleUploadSubmissions now only stages + enqueues; tests that need the
// resulting submission to exist (problem summaries, drill-down review, etc.) drive
// the worker body directly, exactly as scan tests drive PromoteFile/MaskPage.
func driveDirectUploads(t *testing.T, env *testEnv, assessmentID int64) {
	t.Helper()
	ctx := context.Background()
	rows, err := env.st.Q.ListDirectUploadsForAssessment(ctx, db.ListDirectUploadsForAssessmentParams{
		AssessmentID: assessmentID, Limit: 200,
	})
	if err != nil {
		t.Fatalf("ListDirectUploadsForAssessment: %v", err)
	}
	for _, row := range rows {
		if row.FinishedAt.Valid {
			continue
		}
		if err := env.ing.IngestDirectUpload(ctx, row.ID, false); err != nil {
			t.Fatalf("IngestDirectUpload(%d): %v", row.ID, err)
		}
	}
}
