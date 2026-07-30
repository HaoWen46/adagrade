package httpapi

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// LaTeX transcription export — the ladder endpoint and the two bundle endpoints
// (spec 2026-07-25). The status shape below is decoded into explicit structs
// rather than map[string]any so the field NAMES are pinned by the compiler: the
// Overview card's ladder is written against this exact contract in parallel, and
// a rename here is a silent break there.

type txGates struct {
	Problems          int64 `json:"problems"`
	StudentsTotal     int64 `json:"students_total"`
	StudentsWithWork  int64 `json:"students_with_work"`
	PagesTotal        int64 `json:"pages_total"`
	PagesMaskAccepted int64 `json:"pages_mask_accepted"`
}

type txProblem struct {
	Number           int32  `json:"number"`
	Title            string `json:"title"`
	Answers          int64  `json:"answers"`
	Cached           int64  `json:"cached"`
	Pending          int64  `json:"pending"`
	EstCostUSD       string `json:"est_cost_usd"`
	Ready            bool   `json:"ready"`
	PagesPendingMask int64  `json:"pages_pending_mask"`
}

type txStatus struct {
	Model           string      `json:"model"`
	Verified        bool        `json:"verified"`
	Configured      bool        `json:"configured"`
	Ready           bool        `json:"ready"`
	Gates           txGates     `json:"gates"`
	TotalPending    int64       `json:"total_pending"`
	TotalEstCostUSD string      `json:"total_est_cost_usd"`
	Problems        []txProblem `json:"problems"`
}

// --- fixture -----------------------------------------------------------------
//
// Built at the store layer rather than by driving the scan pipeline: this suite
// is about the gates and the bundle, and a hand-built roster/page graph is the
// only way to hold "one page is still pending review" still.
//
// Shape: three problems (1, 2, 3), three rostered students, and answers for the
// first TWO students on problems 1 and 2 only. So:
//
//	problems            3
//	students_total      3   (the whole roster)
//	students_with_work  2
//	pages_total         4   (one page per answer)
//	problem 3           answers 0 — present in the list, never a blocker
//
// Every identity is invented (CLAUDE.md: never real student PII).

const (
	txStudentA = "B11902701"
	txStudentB = "B11902702"
	txStudentC = "B11902703" // rostered, never sat: proves students_with_work < students_total
	txNameA    = "Ada Fictional"
	txEmailA   = "ada.fictional@example.edu"
)

type txFixture struct {
	env      *testEnv
	c        *http.Client
	st       *store.Store
	aid      int64
	problems map[int32]int64 // number -> problem id
	answers  map[string]int64
	pages    map[string]int64 // "student/problem" -> answer_pages id
}

func transcriptionSetup(t *testing.T) txFixture {
	t.Helper()
	env := harnessEnv(t)
	c := loginAs(t, env.ts, env.st, "lect-tx@ntu.edu.tw", "lecturer")
	st := env.st
	ctx := t.Context()

	tenPts, _ := store.Num("10")
	assessment, err := st.Q.CreateAssessment(ctx, db.CreateAssessmentParams{
		Kind: "exam", Name: "Algorithms Midterm 2",
	})
	if err != nil {
		t.Fatalf("create assessment: %v", err)
	}

	f := txFixture{
		env: env, c: c, st: st, aid: assessment.ID,
		problems: map[int32]int64{},
		answers:  map[string]int64{},
		pages:    map[string]int64{},
	}
	for n := int32(1); n <= 3; n++ {
		p, err := st.Q.CreateProblem(ctx, db.CreateProblemParams{
			AssessmentID: assessment.ID, Number: n, Title: fmt.Sprintf("Q%d", n),
			MaxPoints: tenPts, Position: n,
		})
		if err != nil {
			t.Fatalf("create problem %d: %v", n, err)
		}
		f.problems[n] = p.ID
	}

	seedStudent(t, st, txStudentA, txNameA, txEmailA)
	seedStudent(t, st, txStudentB, "Bruno Fictional", "bruno.fictional@example.edu")
	seedStudent(t, st, txStudentC, "Cleo Fictional", "cleo.fictional@example.edu")

	pageSeq := 0
	for _, sid := range []string{txStudentA, txStudentB} {
		stu, err := st.Q.GetStudentByExternalID(ctx, sid)
		if err != nil {
			t.Fatalf("load student: %v", err)
		}
		sub, err := st.Q.CreateSubmission(ctx, db.CreateSubmissionParams{
			AssessmentID: assessment.ID, StudentID: stu.ID,
			OriginalFilename: sid + ".pdf", SourceRef: "raw/" + sid + ".pdf",
			SourceSha256: "sha-" + sid, SourceKind: "pdf", PageCount: 2,
		})
		if err != nil {
			t.Fatalf("create submission: %v", err)
		}
		for _, n := range []int32{1, 2} {
			answer, err := st.Q.EnsureAnswer(ctx, db.EnsureAnswerParams{
				AssessmentID: assessment.ID, StudentID: stu.ID, ProblemID: f.problems[n],
			})
			if err != nil {
				t.Fatalf("ensure answer: %v", err)
			}
			key := fmt.Sprintf("%s/%d", sid, n)
			f.answers[key] = answer.ID

			sha := fmt.Sprintf("sha-%s-p%d", sid, n)
			page, err := st.Q.CreateAnswerPage(ctx, db.CreateAnswerPageParams{
				AnswerID: answer.ID, PageIndex: 0, SubmissionID: sub.ID,
				PdfPageIndex: n - 1, ImageRef: "raw/" + sha, ImageSha256: sha,
				ImageWidth: 100, ImageHeight: 100,
			})
			if err != nil {
				t.Fatalf("create answer page: %v", err)
			}
			f.pages[key] = page.ID

			// The masked artifact is the ONLY image that may leave the box
			// (D10/D19); imaging.LoadMasked refuses any key without "/masked/".
			// The stand-in "JPEG" body must NOT be derived from the student id:
			// the bundle copies these bytes verbatim, and a fixture that smuggled
			// an id into them would fail the privacy sweep for a reason that says
			// nothing about the code under test.
			pageSeq++
			maskedKey := fmt.Sprintf("assessments/%d/pages/masked/%s.jpg", assessment.ID, sha)
			if _, _, err := env.blobs.Put(ctx, maskedKey, strings.NewReader(fmt.Sprintf("fake-masked-jpeg-%d", pageSeq))); err != nil {
				t.Fatalf("put masked blob: %v", err)
			}
			if _, err := st.Q.SetPageMasked(ctx, db.SetPageMaskedParams{
				ID: page.ID, MaskedImageRef: pgtype.Text{String: maskedKey, Valid: true},
			}); err != nil {
				t.Fatalf("set page masked: %v", err)
			}
			if _, err := st.Q.SetMaskReview(ctx, db.SetMaskReviewParams{
				ID: page.ID, MaskReviewStatus: "accepted",
			}); err != nil {
				t.Fatalf("accept mask: %v", err)
			}
		}
	}
	return f
}

// cache seeds the content-addressed transcription for one answer, so the export
// path is a pure cache read: these tests must never depend on a provider, and a
// cache MISS here would try to reach one.
func (f txFixture) cache(t *testing.T, sid string, number int32, blocks []transcribe.Block) {
	t.Helper()
	body, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Q.UpsertAnswerTranscription(t.Context(), db.UpsertAnswerTranscriptionParams{
		AnswerID:      f.answers[fmt.Sprintf("%s/%d", sid, number)],
		ImageShas:     []string{fmt.Sprintf("sha-%s-p%d", sid, number)},
		ModelID:       transcribeDefaultModel,
		PromptVersion: transcribe.PromptVersion,
		ParamsHash:    paramsHash(),
		Blocks:        body, Confidence: "high", RedactionCounts: []byte("{}"),
	}); err != nil {
		t.Fatalf("seed transcription cache: %v", err)
	}
}

// cacheAll seeds every answer, which is what the ZIP tests need.
func (f txFixture) cacheAll(t *testing.T) {
	t.Helper()
	for _, sid := range []string{txStudentA, txStudentB} {
		for _, n := range []int32{1, 2} {
			f.cache(t, sid, n, []transcribe.Block{
				{Kind: transcribe.BlockProse, Text: `Greedy by end time; runs in $O(n \log n)$.`},
			})
		}
	}
}

// unaccept knocks one page back to pending review — the state the whole refusal
// rule exists for.
func (f txFixture) unaccept(t *testing.T, sid string, number int32) {
	t.Helper()
	if _, err := f.st.Q.SetMaskReview(t.Context(), db.SetMaskReviewParams{
		ID: f.pages[fmt.Sprintf("%s/%d", sid, number)], MaskReviewStatus: "pending",
	}); err != nil {
		t.Fatalf("un-accept mask: %v", err)
	}
}

// price seeds the operator-entered $/Mtok row the estimate reads (D35).
func (f txFixture) price(t *testing.T, inPerMtok, outPerMtok string) {
	t.Helper()
	prov, err := f.st.Q.CreateProvider(t.Context(), db.CreateProviderParams{
		Name: transcribeDefaultProvider, Kind: "openai-compat",
		BaseUrl: "https://openrouter.example/api", ApiKeyCiphertext: []byte("x"),
		ApiKeyHint: "…x", Models: []string{transcribeDefaultModel},
		RequestsPerSecond: 1, Burst: 1,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	in, _ := store.Num(inPerMtok)
	out, _ := store.Num(outPerMtok)
	if _, err := f.st.Q.UpsertModelPricing(t.Context(), db.UpsertModelPricingParams{
		ProviderID: prov.ID, Model: transcribeDefaultModel,
		InputUsdPerMtok: in, OutputUsdPerMtok: out,
	}); err != nil {
		t.Fatalf("upsert pricing: %v", err)
	}
}

func (f txFixture) statusURL() string {
	return fmt.Sprintf("%s/api/assessments/%d/transcription-status", f.env.ts.URL, f.aid)
}

func (f txFixture) examZIPURL() string {
	return fmt.Sprintf("%s/api/assessments/%d/transcription.zip", f.env.ts.URL, f.aid)
}

func (f txFixture) problemZIPURL(number int32) string {
	return fmt.Sprintf("%s/api/assessments/%d/problems/%d/transcription.zip", f.env.ts.URL, f.aid, number)
}

func (f txFixture) status(t *testing.T) txStatus {
	t.Helper()
	return getJSON[txStatus](t, f.c, f.statusURL(), http.StatusOK)
}

func problemByNumber(t *testing.T, s txStatus, number int32) txProblem {
	t.Helper()
	for _, p := range s.Problems {
		if p.Number == number {
			return p
		}
	}
	t.Fatalf("no problem %d in the status response (%+v)", number, s.Problems)
	return txProblem{}
}

// --- status: the ladder --------------------------------------------------------

// TestTranscriptionStatus_GatesReportEveryPrerequisiteStage is the ladder
// contract: the card must be able to say WHICH rung the export is stuck on
// without a second request, so every prerequisite count rides on this response.
func TestTranscriptionStatus_GatesReportEveryPrerequisiteStage(t *testing.T) {
	f := transcriptionSetup(t)
	f.cache(t, txStudentA, 1, []transcribe.Block{{Kind: transcribe.BlockProse, Text: "one"}})
	f.cache(t, txStudentB, 1, []transcribe.Block{{Kind: transcribe.BlockProse, Text: "two"}})

	got := f.status(t)

	if got.Model != transcribeDefaultModel {
		t.Errorf("model = %q, want %q", got.Model, transcribeDefaultModel)
	}
	if !got.Configured {
		t.Error("configured = false")
	}
	want := txGates{Problems: 3, StudentsTotal: 3, StudentsWithWork: 2, PagesTotal: 4, PagesMaskAccepted: 4}
	if got.Gates != want {
		t.Errorf("gates = %+v, want %+v", got.Gates, want)
	}
	if !got.Ready {
		t.Error("ready = false with every page mask-accepted and work present")
	}

	// P1 is fully cached, P2 is not: total_pending is the work still to buy.
	p1, p2, p3 := problemByNumber(t, got, 1), problemByNumber(t, got, 2), problemByNumber(t, got, 3)
	if p1.Answers != 2 || p1.Cached != 2 || p1.Pending != 0 || !p1.Ready || p1.PagesPendingMask != 0 {
		t.Errorf("P1 = %+v, want 2 answers fully cached and ready", p1)
	}
	if p2.Answers != 2 || p2.Cached != 0 || p2.Pending != 2 || !p2.Ready {
		t.Errorf("P2 = %+v, want 2 answers, 2 pending, ready", p2)
	}
	if got.TotalPending != 2 {
		t.Errorf("total_pending = %d, want 2", got.TotalPending)
	}

	// A problem nobody answered is listed with answers:0 — visible, never a
	// blocker, and never "ready" (there is nothing to export).
	if p3.Answers != 0 || p3.Ready {
		t.Errorf("P3 = %+v, want answers 0 and ready false", p3)
	}
	if p3.Title != "Q3" {
		t.Errorf("P3 title = %q, want the problem's own title", p3.Title)
	}
}

// TestTranscriptionStatus_UnreviewedMaskBlocksItsProblemAndTheAssessment is the
// D10 line as a gate: a page whose mask nobody has confirmed is a rectangle that
// may not cover the identity, so neither the provider nor the bundle may see it.
func TestTranscriptionStatus_UnreviewedMaskBlocksItsProblemAndTheAssessment(t *testing.T) {
	f := transcriptionSetup(t)
	f.unaccept(t, txStudentA, 2)

	got := f.status(t)

	if got.Gates.PagesTotal != 4 || got.Gates.PagesMaskAccepted != 3 {
		t.Errorf("gates pages = %d/%d, want 4 total and 3 accepted", got.Gates.PagesTotal, got.Gates.PagesMaskAccepted)
	}
	if got.Ready {
		t.Error("ready = true while a page's mask is still pending review")
	}
	p1, p2 := problemByNumber(t, got, 1), problemByNumber(t, got, 2)
	if !p1.Ready || p1.PagesPendingMask != 0 {
		t.Errorf("P1 = %+v — an unrelated problem must not be blocked", p1)
	}
	if p2.Ready || p2.PagesPendingMask != 1 {
		t.Errorf("P2 = %+v, want ready false with 1 page pending mask", p2)
	}
	// The headline cost must not quote work the server would refuse to do.
	if got.TotalPending != 2 {
		t.Errorf("total_pending = %d, want only P1's pending work (2)", got.TotalPending)
	}
}

// TestTranscriptionStatus_UnknownPricingIsBlankNeverAFakeZero — D35. An operator
// who has not entered $/Mtok for the transcription model must be told "unknown",
// because "$0.00" is a promise the system cannot keep.
func TestTranscriptionStatus_UnknownPricingIsBlankNeverAFakeZero(t *testing.T) {
	f := transcriptionSetup(t)
	f.cache(t, txStudentA, 1, nil)
	f.cache(t, txStudentB, 1, nil)

	got := f.status(t)
	if got.TotalEstCostUSD != "" {
		t.Errorf("total_est_cost_usd = %q, want \"\" with no pricing row (D35)", got.TotalEstCostUSD)
	}
	if p := problemByNumber(t, got, 2); p.EstCostUSD != "" {
		t.Errorf("P2 est_cost_usd = %q, want \"\" with no pricing row", p.EstCostUSD)
	}
	// Zero pending is genuinely free, and says so — the two cases must not blur.
	if p := problemByNumber(t, got, 1); p.EstCostUSD != "0" {
		t.Errorf("P1 est_cost_usd = %q, want \"0\" — nothing pending is a real zero", p.EstCostUSD)
	}
}

// TestTranscriptionStatus_PricedEstimateUsesThePendingCount pins the wiring:
// the estimate is the transcription token heuristic times the pending count
// times the operator's own pricing row, not a constant.
func TestTranscriptionStatus_PricedEstimateUsesThePendingCount(t *testing.T) {
	f := transcriptionSetup(t)
	f.price(t, "0.25", "1.50")
	f.cache(t, txStudentA, 1, nil)
	f.cache(t, txStudentB, 1, nil)

	in, _ := store.Num("0.25")
	out, _ := store.Num("1.50")
	want := store.NumStr(store.CostUSD(2*estInputTokens, 2*estOutputTokens, in, out))
	if want == "" {
		t.Fatal("fixture: the expected cost did not compute")
	}

	got := f.status(t)
	if got.TotalEstCostUSD != want {
		t.Errorf("total_est_cost_usd = %q, want %q (2 pending answers)", got.TotalEstCostUSD, want)
	}
	if p := problemByNumber(t, got, 2); p.EstCostUSD != want {
		t.Errorf("P2 est_cost_usd = %q, want %q", p.EstCostUSD, want)
	}
}

// TestTranscriptionStatus_EmptyAssessmentIsNotReady: nothing defined, nothing
// scanned — the ladder must report the bottom rung rather than an empty success.
func TestTranscriptionStatus_EmptyAssessmentIsNotReady(t *testing.T) {
	env := harnessEnv(t)
	c := loginAs(t, env.ts, env.st, "lect-tx2@ntu.edu.tw", "lecturer")
	a := postExpect(t, c, env.ts.URL+"/api/assessments",
		map[string]string{"kind": "exam", "name": "Nothing Yet"}, http.StatusCreated)
	aid := int64(a["id"].(float64))

	got := getJSON[txStatus](t, c,
		fmt.Sprintf("%s/api/assessments/%d/transcription-status", env.ts.URL, aid), http.StatusOK)
	if got.Ready {
		t.Error("ready = true for an assessment with no problems and no work")
	}
	if got.Gates != (txGates{}) {
		t.Errorf("gates = %+v, want all zero", got.Gates)
	}
	if got.Problems == nil {
		t.Error("problems must be an array, never null")
	}

	// A roster with no work in THIS assessment still reports its size: the card
	// distinguishes "no roster" from "roster loaded, nothing scanned".
	seedStudent(t, env.st, "B11902709", "Dee Fictional", "dee.fictional@example.edu")
	got2 := getJSON[txStatus](t, c,
		fmt.Sprintf("%s/api/assessments/%d/transcription-status", env.ts.URL, aid), http.StatusOK)
	if got2.Gates.StudentsTotal != 1 || got2.Gates.StudentsWithWork != 0 {
		t.Errorf("gates = %+v, want the roster counted and no work", got2.Gates)
	}
	if got2.Ready {
		t.Error("ready = true with a roster but no answers")
	}
}

// TestTranscriptionStatus_WithdrawnStudentsStayInTheRosterCount — D23: withdrawing
// labels a student, it does not delete them, and every other surface counts them.
// A shrinking denominator here would make the ladder disagree with the Students
// page about how big the class is.
func TestTranscriptionStatus_WithdrawnStudentsStayInTheRosterCount(t *testing.T) {
	f := transcriptionSetup(t)
	stu, err := f.st.Q.GetStudentByExternalID(t.Context(), txStudentC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Q.SetStudentWithdrawn(t.Context(), db.SetStudentWithdrawnParams{
		ID: stu.ID, Withdrawn: true,
	}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}

	if got := f.status(t).Gates.StudentsTotal; got != 3 {
		t.Errorf("students_total = %d, want 3 — withdrawn students are still roster rows", got)
	}
}

func TestTranscriptionStatus_RequiresSession(t *testing.T) {
	f := transcriptionSetup(t)
	anon := &http.Client{}
	resp, err := anon.Get(f.statusURL())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous GET status: got %d want 401", resp.StatusCode)
	}
}

// --- the whole-exam bundle -----------------------------------------------------

// TestExamTranscriptionZIP_NestsEveryAnsweredProblemUnderOneRoot is the download
// contract: one archive, one root, each problem's per-problem tree beneath it,
// and no directory at all for the problem nobody answered.
func TestExamTranscriptionZIP_NestsEveryAnsweredProblemUnderOneRoot(t *testing.T) {
	f := transcriptionSetup(t)
	f.cacheAll(t)

	resp, body := getBytes(t, f.c, f.examZIPURL())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET exam zip: got %d — %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `"algorithms-midterm-2.zip"`) {
		t.Errorf("Content-Disposition = %q, want the {slug}.zip filename", cd)
	}

	names := zipNames(t, body)
	joined := strings.Join(names, "\n")
	for _, want := range []string{
		"algorithms-midterm-2/algorithms-midterm-2-p1/_all.tex",
		"algorithms-midterm-2/algorithms-midterm-2-p1/MANIFEST.csv",
		"algorithms-midterm-2/algorithms-midterm-2-p1/tex/" + txStudentA + ".tex",
		"algorithms-midterm-2/algorithms-midterm-2-p1/images/" + txStudentA + ".jpg",
		"algorithms-midterm-2/algorithms-midterm-2-p2/_all.tex",
		"algorithms-midterm-2/algorithms-midterm-2-p2/tex/" + txStudentB + ".tex",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("exam bundle missing %q; entries:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "-p3/") {
		t.Error("a problem with no answers must not get an empty directory in the bundle")
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "algorithms-midterm-2/") {
			t.Errorf("entry %q is outside the single archive root", n)
		}
	}
}

// TestExamTranscriptionZIP_MatchesThePerProblemDownload: the whole-exam bundle is
// the per-problem bundles, not a second rendering of them. Anything else and the
// professor's two download buttons disagree about what a student wrote.
func TestExamTranscriptionZIP_MatchesThePerProblemDownload(t *testing.T) {
	f := transcriptionSetup(t)
	f.cacheAll(t)

	_, examBody := getBytes(t, f.c, f.examZIPURL())
	resp, oneBody := getBytes(t, f.c, f.problemZIPURL(1))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET problem zip: got %d — %s", resp.StatusCode, oneBody)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `"algorithms-midterm-2-p1.zip"`) {
		t.Errorf("per-problem Content-Disposition = %q", cd)
	}

	for _, name := range zipNames(t, oneBody) {
		want := zipRead(t, oneBody, name)
		got := zipRead(t, examBody, "algorithms-midterm-2/"+name)
		if !bytes.Equal(got, want) {
			t.Errorf("exam bundle's %q differs from the per-problem download", name)
		}
	}
}

// TestExamTranscriptionZIP_NoEntryBytesCarryARosterIdentity is the spec §3
// invariant asserted end-to-end, over the real store rather than a hand-built
// export.Input: identity lives in FILENAMES, never in file bytes, with
// MANIFEST.csv the single documented exception (student ids only).
func TestExamTranscriptionZIP_NoEntryBytesCarryARosterIdentity(t *testing.T) {
	f := transcriptionSetup(t)
	// The B-C10 case: masking is rectangular, students write their name in the
	// margin, so the transcription itself can carry identity. Seeding it here is
	// the non-vacuity guarantee — a clean bundle means the export scrubbed it,
	// not that nothing was there.
	f.cacheAll(t)
	f.cache(t, txStudentA, 1, []transcribe.Block{
		{Kind: transcribe.BlockProse, Text: txNameA + " (" + txStudentA + ", " + txEmailA + ") — see over."},
	})

	resp, body := getBytes(t, f.c, f.examZIPURL())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET exam zip: got %d — %s", resp.StatusCode, body)
	}
	for _, name := range zipNames(t, body) {
		entry := strings.ToLower(string(zipRead(t, body, name)))
		if strings.Contains(entry, strings.ToLower(txNameA)) {
			t.Errorf("entry %q carries a roster NAME in its bytes", name)
		}
		if strings.Contains(entry, strings.ToLower(txEmailA)) {
			t.Errorf("entry %q carries a roster EMAIL in its bytes", name)
		}
		if strings.HasSuffix(name, "/MANIFEST.csv") {
			continue // the decoder ring may carry student ids, and only ids
		}
		if strings.Contains(entry, strings.ToLower(txStudentA)) {
			t.Errorf("entry %q carries a student ID in its bytes", name)
		}
	}
}

// --- the refusal rule ----------------------------------------------------------

// TestTranscriptionZIP_RefusesWhileMaskReviewIsIncomplete is the server-side half
// of "no bundle with mask-failed rows". The per-answer degrade path inside
// exportAnswer stays as defence in depth, but a bundle silently missing a third
// of its images is worse than no bundle: the professor cannot tell it happened.
func TestTranscriptionZIP_RefusesWhileMaskReviewIsIncomplete(t *testing.T) {
	f := transcriptionSetup(t)
	f.cacheAll(t)
	f.unaccept(t, txStudentA, 2)

	// The whole-exam bundle names the offending problem and the page count.
	resp, body := getBytes(t, f.c, f.examZIPURL())
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("GET exam zip with a pending mask: got %d want 409 — %s", resp.StatusCode, body)
	}
	msg := errorMessage(t, body)
	if want := "problem 2: mask review incomplete (1 pages)"; msg != want {
		t.Errorf("409 message = %q, want %q", msg, want)
	}

	// So does the per-problem endpoint, for the SAME problem.
	resp2, body2 := getBytes(t, f.c, f.problemZIPURL(2))
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("GET problem-2 zip with a pending mask: got %d want 409 — %s", resp2.StatusCode, body2)
	}
	if got := errorMessage(t, body2); !strings.Contains(got, "problem 2") {
		t.Errorf("409 message = %q, want it to name problem 2", got)
	}

	// An unaffected problem still downloads: the refusal is scoped to the pages
	// that are actually blocked, not to the assessment.
	resp3, body3 := getBytes(t, f.c, f.problemZIPURL(1))
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("GET problem-1 zip: got %d want 200 — %s", resp3.StatusCode, body3)
	}
}

// TestTranscriptionZIP_RefusalNamesEveryBlockedProblem: fixing them one 409 at a
// time would be a needlessly long conversation.
func TestTranscriptionZIP_RefusalNamesEveryBlockedProblem(t *testing.T) {
	f := transcriptionSetup(t)
	f.cacheAll(t)
	f.unaccept(t, txStudentA, 1)
	f.unaccept(t, txStudentA, 2)
	f.unaccept(t, txStudentB, 2)

	resp, body := getBytes(t, f.c, f.examZIPURL())
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("got %d want 409 — %s", resp.StatusCode, body)
	}
	msg := errorMessage(t, body)
	for _, want := range []string{"problem 1: mask review incomplete (1 pages)", "problem 2: mask review incomplete (2 pages)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("409 message = %q, want it to contain %q", msg, want)
		}
	}
}

// TestTranscriptionZIP_RefusalMentionsNoStudent — CLAUDE.md's PII rule reaches
// error bodies too: counts and problem numbers, never who.
func TestTranscriptionZIP_RefusalMentionsNoStudent(t *testing.T) {
	f := transcriptionSetup(t)
	f.unaccept(t, txStudentA, 2)

	_, body := getBytes(t, f.c, f.examZIPURL())
	lower := strings.ToLower(string(body))
	for _, needle := range []string{strings.ToLower(txStudentA), strings.ToLower(txNameA), strings.ToLower(txEmailA)} {
		if strings.Contains(lower, needle) {
			t.Errorf("the 409 body names a student: %s", body)
		}
	}
}

// --- empty / auth guards -------------------------------------------------------

func TestExamTranscriptionZIP_NoAnswersIs400(t *testing.T) {
	env := harnessEnv(t)
	c := loginAs(t, env.ts, env.st, "lect-tx3@ntu.edu.tw", "lecturer")
	a := postExpect(t, c, env.ts.URL+"/api/assessments",
		map[string]string{"kind": "exam", "name": "Nothing Yet"}, http.StatusCreated)
	aid := int64(a["id"].(float64))

	resp, body := getBytes(t, c, fmt.Sprintf("%s/api/assessments/%d/transcription.zip", env.ts.URL, aid))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d want 400 — %s", resp.StatusCode, body)
	}
}

func TestExamTranscriptionZIP_UnknownAssessmentIs404(t *testing.T) {
	f := transcriptionSetup(t)
	resp, _ := getBytes(t, f.c, fmt.Sprintf("%s/api/assessments/999999/transcription.zip", f.env.ts.URL))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d want 404", resp.StatusCode)
	}
}

// TestTranscriptionZIP_ProblemWithNoAnswersIs400: unchanged behaviour, and it
// must NOT become the mask-review 409 — "nothing to export" and "not allowed to
// export yet" are different problems with different fixes.
func TestTranscriptionZIP_ProblemWithNoAnswersIs400(t *testing.T) {
	f := transcriptionSetup(t)
	resp, _ := getBytes(t, f.c, f.problemZIPURL(3))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("problem 3 (no answers): got %d want 400", resp.StatusCode)
	}
}

func TestExamTranscriptionZIP_RequiresSession(t *testing.T) {
	f := transcriptionSetup(t)
	anon := &http.Client{}
	for _, url := range []string{f.examZIPURL(), f.problemZIPURL(1)} {
		resp, err := anon.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s: got %d want 401", url, resp.StatusCode)
		}
	}
}

// --- helpers -------------------------------------------------------------------

func getBytes(t *testing.T, c *http.Client, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func errorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope %q: %v", body, err)
	}
	return env.Error
}

func zipNames(t *testing.T, body []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("open zip (%d bytes): %v", len(body), err)
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

func zipRead(t *testing.T, body []byte, name string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open entry %q: %v", name, err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read entry %q: %v", name, err)
		}
		return b
	}
	t.Fatalf("entry %q not found (have %v)", name, zipNames(t, body))
	return nil
}
