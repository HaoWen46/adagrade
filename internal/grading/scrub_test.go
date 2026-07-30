package grading

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/regrade"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// ---------------------------------------------------------------------------
// B-C10 — identity must be scrubbed out of every model-authored free-text field
// BEFORE it lands in the append-only grading_records row. These tests fix the
// contract at both layers: the pure transform (no DB) and the two real insert
// sites (gradeLeaf and RegradeAssist), end to end against Postgres.
//
// Every identity below is INVENTED (CLAUDE.md: never paste real student PII).
// ---------------------------------------------------------------------------

const (
	fixtureName    = "Wendell Quokka"
	fixtureStudent = "b09901777"
	fixtureEmail   = "wendell.quokka@example.edu"
)

// ---------------------------------------------------------------------------
// Pure transform
// ---------------------------------------------------------------------------

func TestScrubModelOutput_RemovesNameStudentIDAndEmail(t *testing.T) {
	id := regrade.Identity{Name: fixtureName, StudentID: fixtureStudent, Email: fixtureEmail}
	in := ModelOutput{
		Transcription:  "By Wendell Quokka, ID b09901777. Let T(n) = 2T(n/2) + n.",
		Confidence:     "high",
		OverallComment: "Reachable at wendell.quokka@example.edu; the base case is right.",
		Criteria: []CriterionScore{
			{CriterionID: 7, Score: "3.5", Rationale: "WENDELL QUOKKA proves the inductive step."},
		},
	}

	got, counts := ScrubModelOutput(in, id)

	for _, leaked := range []string{fixtureName, fixtureStudent, fixtureEmail} {
		assertScrubbed(t, "transcription", got.Transcription, leaked)
		assertScrubbed(t, "overall_comment", got.OverallComment, leaked)
		assertScrubbed(t, "rationale", got.Criteria[0].Rationale, leaked)
	}

	// Grading signal survives untouched.
	if !strings.Contains(got.Transcription, "Let T(n) = 2T(n/2) + n.") {
		t.Errorf("transcription lost its substantive content: %q", got.Transcription)
	}
	if !strings.Contains(got.OverallComment, "the base case is right") {
		t.Errorf("comment lost its substantive content: %q", got.OverallComment)
	}
	if !strings.Contains(got.Criteria[0].Rationale, "proves the inductive step") {
		t.Errorf("rationale lost its substantive content: %q", got.Criteria[0].Rationale)
	}
	if got.Criteria[0].Score != "3.5" || got.Criteria[0].CriterionID != 7 {
		t.Errorf("scores must never be touched by the scrub: %+v", got.Criteria[0])
	}
	if got.Confidence != "high" {
		t.Errorf("confidence = %q, want high (not a free-text field)", got.Confidence)
	}

	// Counts: name twice (transcription + rationale, case-insensitively), id and
	// email once each, no reply token.
	if counts.Name != 2 {
		t.Errorf("counts.Name = %d, want 2", counts.Name)
	}
	if counts.StudentID != 1 {
		t.Errorf("counts.StudentID = %d, want 1", counts.StudentID)
	}
	if counts.Email != 1 {
		t.Errorf("counts.Email = %d, want 1", counts.Email)
	}
	if counts.Token != 0 {
		t.Errorf("counts.Token = %d, want 0", counts.Token)
	}
	if counts.Total() != 4 {
		t.Errorf("counts.Total() = %d, want 4", counts.Total())
	}

	// The transform is pure: the caller's ModelOutput (and its criteria backing
	// array) must not be mutated in place.
	if !strings.Contains(in.Transcription, fixtureName) ||
		!strings.Contains(in.Criteria[0].Rationale, "WENDELL QUOKKA") {
		t.Errorf("ScrubModelOutput mutated its input")
	}
}

func TestScrubModelOutput_CleanOutputUnchangedZeroCounts(t *testing.T) {
	id := regrade.Identity{Name: fixtureName, StudentID: fixtureStudent, Email: fixtureEmail}
	in := ModelOutput{
		Transcription:  "Let T(n) = 2T(n/2) + n; by the master theorem T(n) = O(n log n).",
		Confidence:     "medium",
		OverallComment: "Correct recurrence, missing the base case.",
		Criteria:       []CriterionScore{{CriterionID: 7, Score: "3", Rationale: "Step is sound."}},
	}

	got, counts := ScrubModelOutput(in, id)

	if got.Transcription != in.Transcription {
		t.Errorf("clean transcription was altered:\n got %q\nwant %q", got.Transcription, in.Transcription)
	}
	if got.OverallComment != in.OverallComment {
		t.Errorf("clean comment was altered: %q", got.OverallComment)
	}
	if got.Criteria[0].Rationale != in.Criteria[0].Rationale {
		t.Errorf("clean rationale was altered: %q", got.Criteria[0].Rationale)
	}
	if counts.Total() != 0 {
		t.Errorf("counts.Total() = %d, want 0 for clean output (%+v)", counts.Total(), counts)
	}
}

func TestScrubModelOutput_EmptyIdentityFieldsAreNotWildcards(t *testing.T) {
	text := "Let T(n) = 2T(n/2) + n."

	// Fully empty identity: nothing to match, nothing may be removed.
	got, counts := ScrubModelOutput(ModelOutput{
		Transcription:  text,
		OverallComment: text,
		Criteria:       []CriterionScore{{CriterionID: 1, Score: "1", Rationale: text}},
	}, regrade.Identity{})
	if got.Transcription != text || got.OverallComment != text || got.Criteria[0].Rationale != text {
		t.Errorf("empty identity behaved like a wildcard: %+v", got)
	}
	if counts.Total() != 0 {
		t.Errorf("empty identity produced %d redactions, want 0", counts.Total())
	}

	// Partially empty identity: only the populated field may match.
	got, counts = ScrubModelOutput(ModelOutput{
		Transcription: "Wendell Quokka wrote: " + text,
	}, regrade.Identity{Name: fixtureName})
	assertScrubbed(t, "transcription", got.Transcription, fixtureName)
	if !strings.Contains(got.Transcription, text) {
		t.Errorf("populated-name-only identity removed unrelated text: %q", got.Transcription)
	}
	if counts.Name != 1 || counts.StudentID != 0 || counts.Email != 0 {
		t.Errorf("counts = %+v, want name=1 and the empty kinds at 0", counts)
	}
}

func TestBuildScrubbedRawOutput_ValidatedSubsetCarriesCountsAndScores(t *testing.T) {
	out := ModelOutput{
		Transcription:  "Let T(n) = 2T(n/2) + n.",
		Confidence:     "high",
		OverallComment: "Correct.",
		Criteria:       []CriterionScore{{CriterionID: 7, Score: "3.5", Rationale: "Sound."}},
	}
	counts := regrade.RedactionCounts{Name: 2, StudentID: 1}

	raw := BuildScrubbedRawOutput("fake-vision-1@2026-07", out, counts)

	var env rawOutputEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("raw_output is not valid JSON: %v", err)
	}
	if env.ResolvedModel != "fake-vision-1@2026-07" {
		t.Errorf("resolved_model = %q, want the provider-resolved version string", env.ResolvedModel)
	}
	if !env.Scrubbed {
		t.Errorf("raw_output must mark itself as a scrubbed subset, not verbatim provider bytes")
	}
	if env.Redaction.Name != 2 || env.Redaction.StudentID != 1 {
		t.Errorf("redaction counts not carried into raw_output: %+v", env.Redaction)
	}
	if env.Output.Transcription != out.Transcription || env.Output.Confidence != "high" ||
		env.Output.OverallComment != "Correct." {
		t.Errorf("raw_output lost validated fields: %+v", env.Output)
	}
	if len(env.Output.Criteria) != 1 || env.Output.Criteria[0].CriterionID != 7 ||
		env.Output.Criteria[0].Score.String() != "3.5" || env.Output.Criteria[0].Rationale != "Sound." {
		t.Errorf("raw_output lost the pre-snap per-criterion scores: %+v", env.Output.Criteria)
	}
}

// ---------------------------------------------------------------------------
// The two real insert sites, end to end (Postgres via storetest).
// ---------------------------------------------------------------------------

// TestGradeLeaf_ScrubsRosterNameFromPersistedRecord is the core B-C10 case: the
// region mask missed a margin where the student wrote their name, the model
// faithfully transcribed it, and the append-only record must NOT keep it.
func TestGradeLeaf_ScrubsRosterNameFromPersistedRecord(t *testing.T) {
	h := newSpotCheckHarness(t)
	runID := h.buildRun(t, 1)
	h.setRosterIdentity(t, fixtureName, fixtureStudent, fixtureEmail)
	h.fakeProv.Transcription = "Wendell Quokka\nLet T(n) = 2T(n/2) + n."
	h.fakeProv.OverallComment = "Wendell Quokka states the recurrence correctly."

	driveSpotCheckRun(t, h, runID, true)

	rec := latestRecord(t, h)
	assertRecordScrubbed(t, rec, fixtureName)
	if !strings.Contains(rec.Transcription.String, "Let T(n) = 2T(n/2) + n.") {
		t.Errorf("scrub destroyed the transcription's substantive content: %q", rec.Transcription.String)
	}

	env := decodeRawOutput(t, rec.RawOutput)
	if env.Redaction.Name != 2 {
		t.Errorf("raw_output redaction.name = %d, want 2 (transcription + comment)", env.Redaction.Name)
	}
}

func TestGradeLeaf_ScrubsStudentIDFromPersistedRecord(t *testing.T) {
	h := newSpotCheckHarness(t)
	runID := h.buildRun(t, 1)
	h.setRosterIdentity(t, fixtureName, fixtureStudent, fixtureEmail)
	h.fakeProv.Transcription = "ID: B09901777 (upper case in the margin)\nProof by induction."

	driveSpotCheckRun(t, h, runID, true)

	rec := latestRecord(t, h)
	assertRecordScrubbed(t, rec, fixtureStudent)
	if !strings.Contains(rec.Transcription.String, "Proof by induction.") {
		t.Errorf("scrub destroyed the transcription's substantive content: %q", rec.Transcription.String)
	}
	env := decodeRawOutput(t, rec.RawOutput)
	if env.Redaction.StudentID != 1 {
		t.Errorf("raw_output redaction.student_id = %d, want 1", env.Redaction.StudentID)
	}
}

func TestGradeLeaf_CleanTranscriptionPersistedUnchangedZeroCounts(t *testing.T) {
	h := newSpotCheckHarness(t)
	runID := h.buildRun(t, 1)
	h.setRosterIdentity(t, fixtureName, fixtureStudent, fixtureEmail)
	const clean = "Let T(n) = 2T(n/2) + n; by the master theorem T(n) = O(n log n)."
	h.fakeProv.Transcription = clean

	driveSpotCheckRun(t, h, runID, true)

	rec := latestRecord(t, h)
	if rec.Transcription.String != clean {
		t.Errorf("clean transcription was altered:\n got %q\nwant %q", rec.Transcription.String, clean)
	}
	env := decodeRawOutput(t, rec.RawOutput)
	if env.Redaction.Total() != 0 {
		t.Errorf("clean answer reported %d redactions, want 0 (%+v)", env.Redaction.Total(), env.Redaction)
	}
	if env.Output.Transcription != clean {
		t.Errorf("raw_output transcription was altered: %q", env.Output.Transcription)
	}
}

// TestGradeLeaf_EmptyRosterIdentityDoesNotWildcardRedact guards the empty-needle
// trap at the persistence layer: a roster row with a blank name/email must not
// turn the scrub into a wildcard that shreds the transcription.
func TestGradeLeaf_EmptyRosterIdentityDoesNotWildcardRedact(t *testing.T) {
	h := newSpotCheckHarness(t)
	runID := h.buildRun(t, 1)
	h.setRosterIdentity(t, "", "b09901888", "") // name + email blank on the roster row
	const clean = "Let T(n) = 2T(n/2) + n."
	h.fakeProv.Transcription = clean

	driveSpotCheckRun(t, h, runID, true)

	rec := latestRecord(t, h)
	if rec.Transcription.String != clean {
		t.Errorf("blank roster fields behaved like a wildcard:\n got %q\nwant %q", rec.Transcription.String, clean)
	}
	env := decodeRawOutput(t, rec.RawOutput)
	if env.Redaction.Total() != 0 {
		t.Errorf("blank roster fields produced %d redactions, want 0 (%+v)", env.Redaction.Total(), env.Redaction)
	}
}

// TestRegradeAssist_ScrubsIdentityFromPersistedRecord covers the SECOND insert
// site (internal/grading/regrade_assist.go): a source='regrade_ai' record is
// just as immutable as a run record and needs the same scrub.
func TestRegradeAssist_ScrubsIdentityFromPersistedRecord(t *testing.T) {
	h := newSpotCheckHarness(t)
	subItemID, _, _, _, _ := regradeReady(t, h, "please recheck part (b)")
	ctx := context.Background()

	h.setRosterIdentity(t, fixtureName, fixtureStudent, fixtureEmail)
	h.fakeProv.Transcription = "Wendell Quokka (b09901777)\nPart (b): the recurrence unrolls to n log n."
	h.fakeProv.OverallComment = "Contact wendell.quokka@example.edu about part (b)."

	if err := h.runner.RegradeAssistForSubItem(ctx, subItemID); err != nil {
		t.Fatalf("RegradeAssistForSubItem: %v", err)
	}

	sub, err := h.st.GetRequestProblem(ctx, subItemID)
	if err != nil {
		t.Fatalf("get sub-item: %v", err)
	}
	if !sub.AiRecordID.Valid {
		t.Fatal("sub-item should link an ai_record_id after the AI re-grade")
	}
	rec, err := h.st.Q.GetRecord(ctx, sub.AiRecordID.Int64)
	if err != nil {
		t.Fatalf("get ai record: %v", err)
	}
	for _, leaked := range []string{fixtureName, fixtureStudent, fixtureEmail} {
		assertRecordScrubbed(t, rec, leaked)
	}
	if !strings.Contains(rec.Transcription.String, "the recurrence unrolls to n log n") {
		t.Errorf("scrub destroyed the re-grade transcription's content: %q", rec.Transcription.String)
	}
	env := decodeRawOutput(t, rec.RawOutput)
	if env.Redaction.Total() == 0 {
		t.Errorf("regrade raw_output should record the redaction counts, got %+v", env.Redaction)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// rawOutputEnvelope is the test's independent view of the validated-subset
// raw_output shape, so a change to the stored shape breaks these tests loudly.
type rawOutputEnvelope struct {
	ResolvedModel string                  `json:"resolved_model"`
	Scrubbed      bool                    `json:"scrubbed"`
	Redaction     regrade.RedactionCounts `json:"redaction"`
	Output        struct {
		Transcription  string `json:"transcription"`
		Confidence     string `json:"confidence"`
		OverallComment string `json:"overall_comment"`
		Criteria       []struct {
			CriterionID int64       `json:"criterion_id"`
			Score       json.Number `json:"score"`
			Rationale   string      `json:"rationale"`
		} `json:"criteria"`
	} `json:"output"`
}

func decodeRawOutput(t *testing.T, raw []byte) rawOutputEnvelope {
	t.Helper()
	var env rawOutputEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("raw_output is not valid JSON: %v", err)
	}
	if !env.Scrubbed {
		t.Errorf("persisted raw_output is not marked as a scrubbed subset")
	}
	return env
}

// assertScrubbed fails when needle survives in text (case-insensitively). It
// reports only the NEEDLE LABEL, never the surrounding text (CLAUDE.md).
func assertScrubbed(t *testing.T, field, text, needle string) {
	t.Helper()
	if needle == "" {
		return
	}
	if strings.Contains(strings.ToLower(text), strings.ToLower(needle)) {
		t.Errorf("%s still contains the identity %q after scrubbing", field, needle)
	}
}

// assertRecordScrubbed checks every model-authored free-text surface of a
// persisted record: the transcription column, the comment column, the
// criterion_scores rationales, and the raw_output JSONB.
func assertRecordScrubbed(t *testing.T, rec db.GradingRecord, needle string) {
	t.Helper()
	assertScrubbed(t, "grading_records.transcription", rec.Transcription.String, needle)
	assertScrubbed(t, "grading_records.comment", rec.Comment, needle)
	assertScrubbed(t, "grading_records.criterion_scores", string(rec.CriterionScores), needle)
	assertScrubbed(t, "grading_records.raw_output", string(rec.RawOutput), needle)
}

// latestRecord returns the most recently inserted grading_records row.
func latestRecord(t *testing.T, h *spotCheckHarness) db.GradingRecord {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := h.st.Pool.QueryRow(ctx, `SELECT id FROM grading_records ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("no grading record was written: %v", err)
	}
	rec, err := h.st.Q.GetRecord(ctx, id)
	if err != nil {
		t.Fatalf("get record %d: %v", id, err)
	}
	return rec
}

// setRosterIdentity rewrites the harness's single roster row so the fixtures can
// use a distinctive, INVENTED identity (buildRun's default "sa" is too short to
// prove anything about substring matching).
func (h *spotCheckHarness) setRosterIdentity(t *testing.T, name, externalID, email string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.st.Pool.Exec(ctx,
		`UPDATE students SET name = $1, student_id = $2, email = $3
		   WHERE id = (SELECT student_id FROM answers ORDER BY id LIMIT 1)`,
		name, externalID, email); err != nil {
		t.Fatalf("set roster identity: %v", err)
	}
}
