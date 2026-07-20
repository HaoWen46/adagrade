package grading

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// noProviders seeds with no enabled providers so only the prompt-template path runs
// (the default-method seed needs a vision provider, which we don't care about here).
func noProviders() map[string]struct{} { return map[string]struct{}{} }

// TestSeed_FreshDBSeedsNewTextAsV1: a fresh DB gets the current (v2 curated) text as
// version 1.
func TestSeed_FreshDBSeedsNewTextAsV1(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()

	if err := ensureSeedsWith(ctx, st, noProviders(), discardTestLogger()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tmpl, err := st.Q.LatestPromptTemplate(ctx, DefaultTemplateName)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if tmpl.Version != 1 {
		t.Errorf("fresh DB version = %d, want 1", tmpl.Version)
	}
	if tmpl.SystemTemplate != DefaultSystemTemplate || tmpl.UserTemplate != DefaultUserTemplate {
		t.Errorf("fresh DB should seed the current constants verbatim")
	}
}

// TestSeed_OldTextGetsV2Appended: a DB holding the OLD (v1-era) text gets a v2 with
// the new constants appended; the old v1 row is never mutated (reproducibility).
func TestSeed_OldTextGetsV2Appended(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()

	const oldSystem = `You are a strict but fair teaching assistant grading one student's handwritten answer to one exam problem in an algorithms course. Grade only what the student actually wrote — never invent content, and never let presentation quality influence scores beyond what the rubric says. Score each rubric criterion independently. If the handwriting cannot be read reliably, say so via the confidence field instead of guessing.`
	const oldUser = `# Problem {{.ProblemNumber}} (max {{.MaxPoints}} points)` // any non-matching text

	v1, err := st.Q.CreatePromptTemplateVersion(ctx, db.CreatePromptTemplateVersionParams{
		Name:           DefaultTemplateName,
		SystemTemplate: oldSystem,
		UserTemplate:   oldUser,
	})
	if err != nil {
		t.Fatalf("seed old v1: %v", err)
	}

	if err := ensureSeedsWith(ctx, st, noProviders(), discardTestLogger()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	latest, err := st.Q.LatestPromptTemplate(ctx, DefaultTemplateName)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.Version != 2 {
		t.Fatalf("expected v2 appended, got version %d", latest.Version)
	}
	if latest.SystemTemplate != DefaultSystemTemplate || latest.UserTemplate != DefaultUserTemplate {
		t.Errorf("v2 should carry the new constants")
	}
	// The old v1 row must be intact (reproducibility of existing pins).
	old, err := st.Q.GetPromptTemplateVersion(ctx, v1.ID)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if old.SystemTemplate != oldSystem || old.UserTemplate != oldUser {
		t.Errorf("v1 row was mutated — pins broken")
	}
}

// TestSeed_MatchingTextGetsNothing_AndIdempotent: a DB already matching the constants
// gets no new version, and running twice never appends.
func TestSeed_MatchingTextGetsNothing_AndIdempotent(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()

	// First seed → v1 with current text.
	if err := ensureSeedsWith(ctx, st, noProviders(), discardTestLogger()); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	// Second and third seed → still v1 (matches, nothing appended).
	if err := ensureSeedsWith(ctx, st, noProviders(), discardTestLogger()); err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	if err := ensureSeedsWith(ctx, st, noProviders(), discardTestLogger()); err != nil {
		t.Fatalf("seed 3: %v", err)
	}

	var n int
	if err := st.Pool.QueryRow(ctx,
		"SELECT count(*) FROM prompt_template_versions WHERE name = $1", DefaultTemplateName,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("expected exactly 1 version after idempotent seeds, got %d", n)
	}
}

// TestSeed_RegradeTemplateSeededReadOnly: EnsureSeeds also seeds the regrade_v1 kind at
// version 1 on a fresh DB, appends v2 when the constants change (old version intact),
// and is idempotent — same firmware discipline as the grading template (D25).
func TestSeed_RegradeTemplateSeededReadOnly(t *testing.T) {
	st := storetest.Fresh(t)
	ctx := context.Background()

	if err := ensureSeedsWith(ctx, st, noProviders(), discardTestLogger()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tmpl, err := st.Q.LatestPromptTemplate(ctx, RegradeTemplateName)
	if err != nil {
		t.Fatalf("latest regrade template: %v", err)
	}
	if tmpl.Version != 1 {
		t.Errorf("fresh DB regrade template version = %d, want 1", tmpl.Version)
	}
	if tmpl.SystemTemplate != RegradeSystemTemplate || tmpl.UserTemplate != RegradeUserTemplate {
		t.Errorf("fresh DB should seed the regrade constants verbatim")
	}
	v1ID := tmpl.ID

	// Re-seed twice: idempotent, still v1.
	for i := 0; i < 2; i++ {
		if err := ensureSeedsWith(ctx, st, noProviders(), discardTestLogger()); err != nil {
			t.Fatalf("re-seed %d: %v", i, err)
		}
	}
	var n int
	if err := st.Pool.QueryRow(ctx,
		"SELECT count(*) FROM prompt_template_versions WHERE name = $1", RegradeTemplateName,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("idempotent re-seed should keep exactly 1 regrade version, got %d", n)
	}

	// A constant change appends v2 and leaves v1 intact (reproducibility of pins).
	if _, err := EnsureRegradeTemplateSeed(ctx, st); err != nil {
		t.Fatalf("ensure (no change): %v", err)
	}
	edited, err := st.Q.CreatePromptTemplateVersion(ctx, db.CreatePromptTemplateVersionParams{
		Name:           RegradeTemplateName,
		SystemTemplate: "OLD REGRADE SYSTEM",
		UserTemplate:   "OLD REGRADE USER",
	})
	if err != nil {
		t.Fatalf("simulate old v: %v", err)
	}
	_ = edited
	latest, err := EnsureRegradeTemplateSeed(ctx, st)
	if err != nil {
		t.Fatalf("ensure after divergent latest: %v", err)
	}
	if latest.SystemTemplate != RegradeSystemTemplate || latest.UserTemplate != RegradeUserTemplate {
		t.Errorf("divergent latest should get the current constants appended")
	}
	// v1 (the original seed) is untouched.
	orig, err := st.Q.GetPromptTemplateVersion(ctx, v1ID)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if orig.SystemTemplate != RegradeSystemTemplate {
		t.Errorf("original regrade v1 was mutated — pins broken")
	}
}

// discardTestLogger returns a logger that swallows output for tests.
func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
