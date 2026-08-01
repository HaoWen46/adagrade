package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/export"
	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/regrade"
	"github.com/HaoWen46/adagrade/internal/report"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/transcribe"
)

// LaTeX transcription export (spec 2026-07-25). Three endpoints:
//
//	GET /api/assessments/{id}/transcription-status               ladder + cost preview
//	GET /api/assessments/{id}/problems/{n}/transcription.zip     one problem's bundle
//	GET /api/assessments/{id}/transcription.zip                  the whole exam
//
// The ZIP is produced synchronously. That is a deliberate v0 choice: the
// expensive step is cached content-addressed, so only the FIRST export of a
// problem pays, and a cohort of 40 answers at ~8 concurrent calls lands well
// inside a browser's patience. Everything after is a cache read.

const (
	// transcribeDefaultModel is used when ADAMARKER_TRANSCRIBE_MODEL is unset.
	// Chosen for reproducibility as much as price: it supports seed and
	// structured outputs, and every one of its OpenRouter endpoints is
	// first-party Google, so provider routing cannot silently swap in a
	// differently-quantised backend (spec §5).
	transcribeDefaultModel = "google/gemini-3.1-flash-lite"

	// transcribeDefaultProvider matches the seeded provider name.
	transcribeDefaultProvider = "openrouter"

	// transcribeConcurrency bounds in-flight provider calls for one export.
	transcribeConcurrency = 8

	// Rough per-answer token estimate for the pre-flight cost preview. A masked
	// page plus the prompt measured ~1.5k in / ~0.3k out on real pages.
	estInputTokens  = 1600
	estOutputTokens = 300
)

func (s *Server) transcribeModel() string {
	if s.cfg.TranscribeModel != "" {
		return s.cfg.TranscribeModel
	}
	return transcribeDefaultModel
}

func (s *Server) transcribeProviderName() string {
	if s.cfg.TranscribeProvider != "" {
		return s.cfg.TranscribeProvider
	}
	return transcribeDefaultProvider
}

// paramsHash pins the decoding knobs into the cache key so a temperature change
// is a new row rather than a silent reuse.
func paramsHash() string {
	sum := sha256.Sum256([]byte("temperature=0;" + transcribe.PromptVersion))
	return hex.EncodeToString(sum[:8])
}

// problemGate is one rung of the export ladder for one problem, and the single
// definition of "ready" in this package. The status endpoint renders it; both
// ZIP endpoints refuse on it. Keeping one struct is the point: a bundle the UI
// offered and the server then refused (or worse, the reverse) is exactly the
// mismatch this shape exists to make impossible.
type problemGate struct {
	number  int32
	title   string
	answers int64
	cached  int64
	pages   int64
	// answersWithPages counts answers that actually have scanned pages. answers
	// rows are materialized per (assessment, student, problem) for the WHOLE
	// roster, so `answers` includes students with nothing scanned — those export
	// as free `absent` rows and must not inflate the cost preview.
	answersWithPages int64
	// pagesAccepted counts this problem's answer_pages rows whose mask a human
	// has ACCEPTED — grading's D10 line (masks.sql), which the export holds too.
	pagesAccepted int64
}

// pending is the number of answers that would cost a provider call — answers
// with pages minus already-cached. Pageless answers are absent rows: free, so
// excluded (caught live: the pilot's ladder previewed 45 pending when only 15
// answers had any pages to transcribe). Clamped: stale cache rows for answers
// that have since been deleted could otherwise drive this negative.
func (g problemGate) pending() int64 {
	if p := g.answersWithPages - g.cached; p > 0 {
		return p
	}
	return 0
}

func (g problemGate) pagesPendingMask() int64 {
	if p := g.pages - g.pagesAccepted; p > 0 {
		return p
	}
	return 0
}

// ready means this problem can be exported right now: it has work to export, and
// every page of that work has a human-accepted mask. A problem with answers but
// zero pages is vacuously mask-clean — the bundle it produces is all-absent rows,
// which is a truthful answer to "nothing was scanned", not a blocked state.
func (g problemGate) ready() bool { return g.answers > 0 && g.pagesPendingMask() == 0 }

// loadProblemGates reads every problem's ladder row in one round trip.
func (s *Server) loadProblemGates(ctx context.Context, aid int64) ([]problemGate, error) {
	rows, err := s.store.Q.CountProblemTranscriptionStatus(ctx, db.CountProblemTranscriptionStatusParams{
		AssessmentID:  aid,
		ModelID:       s.transcribeModel(),
		PromptVersion: transcribe.PromptVersion,
		ParamsHash:    paramsHash(),
	})
	if err != nil {
		return nil, err
	}
	gates := make([]problemGate, 0, len(rows))
	for _, row := range rows {
		gates = append(gates, problemGate{
			number:           row.ProblemNumber,
			title:            row.ProblemTitle,
			answers:          row.Answers,
			cached:           row.Cached,
			pages:            row.Pages,
			answersWithPages: row.AnswersWithPages,
			pagesAccepted:    row.PagesMaskAccepted,
		})
	}
	return gates, nil
}

// maskRefusal is the 409 body for a bundle request that includes a problem whose
// mask review is unfinished. Empty means "nothing to refuse".
//
// exportAnswer still degrades an individual mask-failed row to status=failed as
// defence in depth, but that is no longer the user-visible contract: a bundle
// silently missing a third of its images is worse than no bundle, because the
// professor cannot tell it happened. Refusing names the problem and the number of
// pages still awaiting review — counts only, never a student (CLAUDE.md).
func maskRefusal(gates []problemGate) string {
	var parts []string
	for _, g := range gates {
		if n := g.pagesPendingMask(); n > 0 {
			parts = append(parts, fmt.Sprintf("problem %d: mask review incomplete (%d pages)", g.number, n))
		}
	}
	return strings.Join(parts, "; ")
}

func findGate(gates []problemGate, number int32) (problemGate, bool) {
	for _, g := range gates {
		if g.number == number {
			return g, true
		}
	}
	return problemGate{}, false
}

// transcriptionGateCounts are the prerequisite stages the Overview card renders
// as a ladder: problems defined → scans ingested → masks reviewed → ready.
type transcriptionGateCounts struct {
	Problems          int64 `json:"problems"`
	StudentsTotal     int64 `json:"students_total"`
	StudentsWithWork  int64 `json:"students_with_work"`
	PagesTotal        int64 `json:"pages_total"`
	PagesMaskAccepted int64 `json:"pages_mask_accepted"`
}

type transcriptionProblemStatus struct {
	Number           int32  `json:"number"`
	Title            string `json:"title"`
	Answers          int64  `json:"answers"`
	Cached           int64  `json:"cached"`
	Pending          int64  `json:"pending"`
	EstCostUSD       string `json:"est_cost_usd"`
	Ready            bool   `json:"ready"`
	PagesPendingMask int64  `json:"pages_pending_mask"`
}

type transcriptionStatusResponse struct {
	Model    string `json:"model"`
	Verified bool   `json:"verified"`
	// Typst reports whether bundles will carry a compile-checked Typst
	// mirror (spec 2026-07-30) — config knowledge, not a build outcome; the
	// per-build verdict lives in the bundle's manifest.
	Typst           bool                         `json:"typst"`
	Configured      bool                         `json:"configured"`
	Ready           bool                         `json:"ready"`
	Gates           transcriptionGateCounts      `json:"gates"`
	TotalPending    int64                        `json:"total_pending"`
	TotalEstCostUSD string                       `json:"total_est_cost_usd"`
	Problems        []transcriptionProblemStatus `json:"problems"`
}

func (s *Server) handleTranscriptionStatus(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	gates, err := s.loadProblemGates(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "transcription status query failed")
		return
	}
	coverage, err := s.store.Q.CountAssessmentAnswerCoverage(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "transcription status query failed")
		return
	}
	inUSD, outUSD, priced := s.transcribePricing(r.Context())

	out := transcriptionStatusResponse{
		Model:      s.transcribeModel(),
		Verified:   s.cfg.TectonicBinPath != "",
		Typst:      s.cfg.TypstBinPath != "",
		Configured: true,
		Gates: transcriptionGateCounts{
			Problems:         int64(len(gates)),
			StudentsTotal:    coverage.StudentsTotal,
			StudentsWithWork: coverage.StudentsWithWork,
		},
		Problems: make([]transcriptionProblemStatus, 0, len(gates)),
	}

	// The whole assessment is ready when there is something to export and every
	// problem that HAS work is exportable. A problem nobody answered must not
	// hold the exam hostage — the bundle simply omits it — but its row still
	// reports answers:0 so the UI can say why it is missing.
	ready := len(gates) > 0 && coverage.StudentsWithWork > 0
	for _, g := range gates {
		// Whole-assessment page totals are the sum of the per-problem counts:
		// every answer_pages row hangs off an answer, and every answer off
		// exactly one of this assessment's problems (transcriptions.sql).
		out.Gates.PagesTotal += g.pages
		out.Gates.PagesMaskAccepted += g.pagesAccepted

		if g.answers > 0 && !g.ready() {
			ready = false
		}
		if g.ready() {
			out.TotalPending += g.pending()
		}
		out.Problems = append(out.Problems, transcriptionProblemStatus{
			Number:           g.number,
			Title:            g.title,
			Answers:          g.answers,
			Cached:           g.cached,
			Pending:          g.pending(),
			EstCostUSD:       estimateTranscribeCost(g.pending(), inUSD, outUSD, priced),
			Ready:            g.ready(),
			PagesPendingMask: g.pagesPendingMask(),
		})
	}
	out.Ready = ready
	// The headline number is priced from the pending work that is actually
	// exportable — quoting a figure that includes mask-blocked problems would
	// advertise a spend the server would then refuse to make.
	out.TotalEstCostUSD = estimateTranscribeCost(out.TotalPending, inUSD, outUSD, priced)

	writeJSON(w, http.StatusOK, out)
}

// estimateTranscribeCost reuses the operator-entered model_pricing rows so the
// preview and the recorded cost come from one source (D35).
//
// The two empty-ish answers are NOT interchangeable and the split is the whole
// point: "0" means nothing is pending and the export is genuinely free, while ""
// means nobody has told this system what the model costs. Collapsing the second
// into a fake $0 would be a promise the system cannot keep.
//
// Pricing is passed in rather than looked up here so a status response with N
// problems resolves it once instead of 2N times.
func estimateTranscribeCost(answers int64, inUSD, outUSD pgtype.Numeric, priced bool) string {
	if answers <= 0 {
		return "0"
	}
	if !priced {
		return ""
	}
	cost := store.CostUSD(answers*estInputTokens, answers*estOutputTokens, inUSD, outUSD)
	if !cost.Valid {
		return ""
	}
	return store.NumStr(cost)
}

// transcribePricing resolves the operator-entered $/Mtok row for the
// transcription model (D35). Missing pricing yields ok=false so callers show
// "unknown" rather than a fake $0.
func (s *Server) transcribePricing(ctx context.Context) (inUSD, outUSD pgtype.Numeric, ok bool) {
	provider, err := s.store.Q.GetProviderByName(ctx, s.transcribeProviderName())
	if err != nil {
		return pgtype.Numeric{}, pgtype.Numeric{}, false
	}
	pricing, err := s.store.Q.GetModelPricing(ctx, db.GetModelPricingParams{
		ProviderID: provider.ID, Model: s.transcribeModel(),
	})
	if err != nil {
		return pgtype.Numeric{}, pgtype.Numeric{}, false
	}
	return pricing.InputUsdPerMtok, pricing.OutputUsdPerMtok, true
}

// transcriptionDeadline replaces the default 30 s request deadline for the ZIP
// handlers. A first-run export transcribes a cohort and then compile-checks it —
// legitimately minutes of work. Without this, the read deadline expires mid-way,
// Go cancels the request context, and the compile gate reports a phantom
// failure at exactly 30 s (observed live on the handwriting pilot; the earlier
// 30.0 s demo-exam success was winning that race by luck).
const transcriptionDeadline = 10 * time.Minute

func (s *Server) handleTranscriptionZIP(w http.ResponseWriter, r *http.Request) {
	_ = extendBodyDeadline(w, transcriptionDeadline)
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		apiError(w, http.StatusBadRequest, "invalid problem number")
		return
	}
	assessment, err := s.store.Q.GetAssessment(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}

	// Gate BEFORE building: buildExportInput pays for uncached transcriptions,
	// and spending money on a bundle we are about to refuse is the one outcome
	// nobody would forgive.
	gates, err := s.loadProblemGates(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "transcription status query failed")
		return
	}
	gate, found := findGate(gates, int32(number))
	if !found || gate.answers == 0 {
		apiError(w, http.StatusBadRequest, "no answers for that problem")
		return
	}
	if msg := maskRefusal([]problemGate{gate}); msg != "" {
		apiError(w, http.StatusConflict, msg)
		return
	}

	in, err := s.buildExportInput(r.Context(), aid, int32(number), assessment.Name)
	if err != nil {
		s.log.Error("transcription export failed", "assessment_id", aid, "problem", number, "err", err)
		apiError(w, http.StatusInternalServerError, "transcription export failed")
		return
	}
	if len(in.Answers) == 0 {
		apiError(w, http.StatusBadRequest, "no answers for that problem")
		return
	}
	if err := s.verifyProblemTeX(r.Context(), in); err != nil {
		s.log.Error("transcription compile gate failed", "assessment_id", aid, "problem", number, "err", errContentFree(err))
		apiError(w, http.StatusInternalServerError, gateFailureMessage(number, err))
		return
	}
	in.TypstVerdict = s.typstVerdict(r.Context(), in)

	zipBytes, err := export.BuildZIP(in)
	if err != nil {
		s.log.Error("transcription bundle failed", "assessment_id", aid, "problem", number, "err", err)
		apiError(w, http.StatusInternalServerError, "could not build the export bundle")
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", in.RootDir()+".zip"))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(zipBytes)
}

// handleExamTranscriptionZIP is the whole-exam bundle: every problem's
// per-problem tree side by side under one root. Problems nobody answered are
// dropped entirely — an empty directory would read as "the cohort answered
// nothing", a claim about the students that this export cannot support.
func (s *Server) handleExamTranscriptionZIP(w http.ResponseWriter, r *http.Request) {
	_ = extendBodyDeadline(w, transcriptionDeadline)
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	assessment, err := s.store.Q.GetAssessment(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}
	gates, err := s.loadProblemGates(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "transcription status query failed")
		return
	}

	included := make([]problemGate, 0, len(gates))
	for _, g := range gates {
		if g.answers > 0 {
			included = append(included, g)
		}
	}
	if len(included) == 0 {
		apiError(w, http.StatusBadRequest, "no answers for this assessment")
		return
	}
	// One refusal for the whole request, naming every offending problem: fixing
	// them one 409 at a time would be a needlessly long conversation.
	if msg := maskRefusal(included); msg != "" {
		apiError(w, http.StatusConflict, msg)
		return
	}

	// Sequential, not fanned out across problems: buildExportInput already runs
	// transcribeConcurrency provider calls in flight, and multiplying that by the
	// problem count is how an 8-problem export earns a rate-limit ban.
	out := export.ExamInput{
		AssessmentName: assessment.Name,
		Problems:       make([]export.Input, 0, len(included)),
	}
	for _, g := range included {
		in, err := s.buildExportInput(r.Context(), aid, g.number, assessment.Name)
		if err != nil {
			s.log.Error("transcription export failed", "assessment_id", aid, "problem", g.number, "err", err)
			apiError(w, http.StatusInternalServerError, "transcription export failed")
			return
		}
		if len(in.Answers) == 0 {
			// The gate said this problem had answers; they vanished between the
			// two queries. Dropping it silently is fine — it is genuinely empty
			// now — but an empty tree is not.
			continue
		}
		out.Problems = append(out.Problems, in)
	}
	if len(out.Problems) == 0 {
		apiError(w, http.StatusBadRequest, "no answers for this assessment")
		return
	}
	for i, in := range out.Problems {
		if err := s.verifyProblemTeX(r.Context(), in); err != nil {
			s.log.Error("transcription compile gate failed", "assessment_id", aid, "problem", in.ProblemNumber, "err", errContentFree(err))
			apiError(w, http.StatusInternalServerError, gateFailureMessage(in.ProblemNumber, err))
			return
		}
		out.Problems[i].TypstVerdict = s.typstVerdict(r.Context(), in)
	}

	zipBytes, err := export.BuildExamZIP(out)
	if err != nil {
		s.log.Error("transcription exam bundle failed", "assessment_id", aid, "err", err)
		apiError(w, http.StatusInternalServerError, "could not build the export bundle")
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", out.RootDir()+".zip"))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(zipBytes)
}

// buildExportInput assembles the bundle: cached transcriptions where they
// exist, fresh provider calls where they do not, and an explicit status for
// every answer so a blank .tex is never ambiguous.
func (s *Server) buildExportInput(ctx context.Context, aid int64, number int32, assessmentName string) (export.Input, error) {
	answers, err := s.store.Q.ListProblemAnswersForExport(ctx, db.ListProblemAnswersForExportParams{
		AssessmentID: aid, Number: number,
	})
	if err != nil {
		return export.Input{}, fmt.Errorf("list answers: %w", err)
	}
	pageRows, err := s.store.Q.ListAnswerPagesForExport(ctx, db.ListAnswerPagesForExportParams{
		AssessmentID: aid, Number: number,
	})
	if err != nil {
		return export.Input{}, fmt.Errorf("list pages: %w", err)
	}
	pagesByAnswer := map[int64][]db.ListAnswerPagesForExportRow{}
	for _, p := range pageRows {
		pagesByAnswer[p.AnswerID] = append(pagesByAnswer[p.AnswerID], p)
	}

	out := export.Input{
		AssessmentName: assessmentName,
		ProblemNumber:  int(number),
		TeX:            transcribe.Options{CJKFontFile: absFontPath(s.cfg.ReportFontPath)},
		Answers:        make([]export.Answer, len(answers)),
	}

	sem := make(chan struct{}, transcribeConcurrency)
	var wg sync.WaitGroup
	for i, a := range answers {
		wg.Add(1)
		go func(i int, a db.ListProblemAnswersForExportRow) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out.Answers[i] = s.exportAnswer(ctx, a, pagesByAnswer[a.AnswerID])
		}(i, a)
	}
	wg.Wait()
	return out, nil
}

// exportAnswer resolves one answer to an export.Answer, never returning an
// error: a single failure must degrade that student's row to a recorded status,
// not sink the whole bundle.
func (s *Server) exportAnswer(ctx context.Context, a db.ListProblemAnswersForExportRow, pages []db.ListAnswerPagesForExportRow) export.Answer {
	ans := export.Answer{
		Identity: regrade.Identity{Name: a.StudentName, StudentID: a.StudentID, Email: a.StudentEmail},
		Status:   export.StatusAbsent,
		Source:   export.SourceDedicated,
	}
	if len(pages) == 0 {
		// Nothing was scanned, so nothing was transcribed: export.validated()
		// rightly rejects an absent row claiming a transcription source (caught
		// live by the pilot — the demo cohort never had a pageless answer).
		ans.Source = ""
		return ans
	}

	masked := make([]imaging.MaskedImage, 0, len(pages))
	shas := make([]string, 0, len(pages))
	for _, p := range pages {
		if !p.MaskedImageRef.Valid || p.MaskedImageRef.String == "" {
			// Unmasked pages must never reach a provider OR the bundle.
			ans.Status = export.StatusFailed
			ans.Flags = append(ans.Flags, "page not masked")
			return ans
		}
		if p.MaskReviewStatus != "accepted" {
			// Grading's D10 gate refuses to run while any page's mask is not
			// human-accepted; the export holds the same line. A pending or
			// flagged mask is a rectangle nobody has confirmed covers the
			// identity, so neither the provider nor the bundle may see it.
			ans.Status = export.StatusFailed
			ans.Flags = append(ans.Flags, "mask not accepted")
			return ans
		}
		img, err := s.loadMasked(ctx, p.MaskedImageRef.String)
		if err != nil {
			ans.Status = export.StatusFailed
			ans.Flags = append(ans.Flags, "page image unreadable")
			return ans
		}
		masked = append(masked, img)
		shas = append(shas, p.ImageSha256)
	}
	ans.Pages = masked

	doc, confidence, source, err := s.transcribeAnswer(ctx, a.AnswerID, shas, masked)
	if err != nil {
		ans.Status = export.StatusFailed
		ans.Flags = append(ans.Flags, "transcription failed")
		return ans
	}
	ans.Doc = doc
	ans.Confidence = confidence
	ans.Source = source
	if confidence == "illegible" || len(doc.Blocks) == 0 {
		ans.Status = export.StatusIllegible
	} else {
		ans.Status = export.StatusOK
	}
	return ans
}

func (s *Server) loadMasked(ctx context.Context, key string) (imaging.MaskedImage, error) {
	rc, err := s.blobs.Get(ctx, key)
	if err != nil {
		return imaging.MaskedImage{}, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return imaging.MaskedImage{}, err
	}
	return imaging.LoadMasked(key, b)
}

// transcribeAnswer returns the cached blocks when the content address matches,
// and otherwise pays for exactly one provider call and caches the result.
func (s *Server) transcribeAnswer(ctx context.Context, answerID int64, shas []string, pages []imaging.MaskedImage) (transcribe.Doc, string, export.Source, error) {
	key := db.GetAnswerTranscriptionParams{
		AnswerID:      answerID,
		ImageShas:     shas,
		ModelID:       s.transcribeModel(),
		PromptVersion: transcribe.PromptVersion,
		ParamsHash:    paramsHash(),
	}
	if row, err := s.store.Q.GetAnswerTranscription(ctx, key); err == nil {
		var blocks []transcribe.Block
		if err := json.Unmarshal(row.Blocks, &blocks); err == nil {
			// SourceDedicated, not SourceGradingCache: answer_transcriptions
			// holds results of the dedicated transcription pass, so a cache HIT
			// and a cache MISS describe the same provenance. Reporting them
			// differently made the manifest — and therefore the archive bytes —
			// change between the first and second export of the same answers,
			// breaking the re-export-is-byte-identical guarantee. Only a
			// transcription lifted from grading_records is SourceGradingCache.
			return transcribe.Doc{Blocks: blocks}, row.Confidence, export.SourceDedicated, nil
		}
	}

	provider, limiter, err := s.providers.Provider(ctx, s.transcribeProviderName())
	if err != nil {
		return transcribe.Doc{}, "", export.SourceDedicated, fmt.Errorf("provider %q: %w", s.transcribeProviderName(), err)
	}
	if limiter != nil {
		if err := limiter.Wait(ctx); err != nil {
			return transcribe.Doc{}, "", export.SourceDedicated, err
		}
	}

	imgs := make([]imaging.ProviderImage, len(pages))
	for i := range pages {
		imgs[i] = pages[i]
	}
	res, err := provider.Grade(ctx, s.transcribeModel(), llm.Request{
		System:      transcribe.SystemPrompt,
		Prompt:      transcribe.UserPrompt(0),
		Images:      imgs,
		Schema:      transcribe.BuildSchema(),
		Temperature: 0,
		ToolName:    transcribe.ToolName,
	})
	if err != nil {
		return transcribe.Doc{}, "", export.SourceDedicated, err
	}
	doc, confidence, err := transcribe.ParseResponse(res.JSON)
	if err != nil {
		return transcribe.Doc{}, "", export.SourceDedicated, err
	}

	blocksJSON, _ := json.Marshal(doc.Blocks)
	var cost pgtype.Numeric
	if inUSD, outUSD, ok := s.transcribePricing(ctx); ok {
		cost = store.CostUSD(int64(res.InputTokens), int64(res.OutputTokens), inUSD, outUSD)
	}
	_, _ = s.store.Q.UpsertAnswerTranscription(ctx, db.UpsertAnswerTranscriptionParams{
		AnswerID:        answerID,
		ImageShas:       shas,
		ModelID:         s.transcribeModel(),
		PromptVersion:   transcribe.PromptVersion,
		ParamsHash:      paramsHash(),
		Blocks:          blocksJSON,
		Confidence:      confidence,
		RedactionCounts: []byte("{}"),
		InputTokens:     pgtype.Int4{Int32: int32(res.InputTokens), Valid: true},
		OutputTokens:    pgtype.Int4{Int32: int32(res.OutputTokens), Valid: true},
		CostUsd:         cost,
	})
	return doc, confidence, export.SourceDedicated, nil
}

// verifyProblemTeX is the compile gate (spec §2 stage 4): one tectonic compile
// of the problem's _all.tex verifies every student body in the bundle, because
// they all share one preamble. When no engine is configured the gate is a
// no-op and the status endpoint's `verified` field already tells the UI so —
// the bundle ships unverified, never blocked.
//
// When the bundle FAILS, the gate compiles each answer's standalone document —
// the same bytes the archive ships as tex/{id}.tex — and returns a *gateError
// naming the offending answer(s) (2026-07-30 audit finding 8). Before this, a
// single pathological answer refused the whole cohort with an error nobody
// could act on.
func (s *Server) verifyProblemTeX(ctx context.Context, in export.Input) error {
	if s.cfg.TectonicBinPath == "" {
		return nil
	}
	tex, err := export.AllTeX(in)
	if err != nil {
		return err
	}
	cache := transcribe.DefaultCacheDir()
	if _, err = transcribe.Compile(ctx, s.cfg.TectonicBinPath, cache, tex); err == nil {
		return nil
	}
	if !errors.Is(err, transcribe.ErrCompileFailed) {
		// Timeout, cancellation, engine trouble: attribution would repeat the
		// same infrastructure failure N more times.
		return err
	}

	singles, aerr := export.AnswerTeXes(in)
	if aerr != nil {
		return err
	}
	ge := &gateError{}
	for _, one := range singles {
		if one.Status == export.StatusAbsent {
			continue // no student content; cannot be the cause
		}
		ge.total++
		if ctx.Err() != nil {
			// Attribution was cut short, but the bundle DEFINITELY does not
			// compile — report that (with partial attribution if any), never
			// a "timed out, try again" that misdescribes a deterministic
			// failure.
			if len(ge.studentIDs) > 0 {
				return ge
			}
			return err
		}
		_, cerr := transcribe.Compile(ctx, s.cfg.TectonicBinPath, cache, one.TeX)
		if errors.Is(cerr, transcribe.ErrCompileFailed) {
			ge.studentIDs = append(ge.studentIDs, one.StudentID)
		} else if cerr != nil {
			if len(ge.studentIDs) > 0 {
				return ge
			}
			return err
		}
	}
	switch {
	case len(ge.studentIDs) == 0:
		// Every standalone compiles but the aggregate does not — an
		// interaction between bodies the validator did not foresee.
		return fmt.Errorf("compile gate: bundle fails but every standalone answer compiles: %w", err)
	case len(ge.studentIDs) == ge.total:
		// Everyone fails: the fault is the shared preamble (font, package),
		// not the students — blaming the whole cohort by id is blaming no
		// one (the missing-CJK-font incident absFontPath records did this).
		return fmt.Errorf("compile gate: every answer fails standalone — environment fault, not student content: %w", err)
	}
	return ge
}

// typstVerdict is the SECONDARY compile gate (spec 2026-07-30): one
// best-effort compile of the bundle's _all.typ mirror. It never blocks — a
// failed mirror ships anyway with its verdict in the manifest header. ""
// means no Typst binary is configured (rendered "unverified").
func (s *Server) typstVerdict(ctx context.Context, in export.Input) string {
	if s.cfg.TypstBinPath == "" {
		return ""
	}
	src, err := export.AllTyp(in)
	if err != nil {
		s.log.Warn("typst mirror assembly failed", "problem", in.ProblemNumber, "err", errContentFree(err))
		return "failed"
	}
	fontDir := ""
	if s.cfg.ReportFontPath != "" {
		fontDir = filepath.Dir(absFontPath(s.cfg.ReportFontPath))
	}
	if _, err := report.CompileTypstSource(ctx, s.cfg.TypstBinPath, fontDir, src); err != nil {
		// Compile error, timeout, engine trouble — all just "failed": the
		// mirror is best-effort and the log stays content-free.
		s.log.Warn("typst mirror compile failed", "problem", in.ProblemNumber, "err", errContentFree(err))
		return "failed"
	}
	return "verified"
}

// gateError is a compile-gate failure attributed to the specific answers
// whose standalone documents fail. Error() carries counts only — it flows
// into logs, where student ids are PII (CLAUDE.md); studentIDs feeds the HTTP
// response the professor reads, which names ids the way every other endpoint
// already does.
type gateError struct {
	studentIDs []string
	total      int
}

func (e *gateError) Error() string {
	return fmt.Sprintf("compile gate: %d of %d answers failed standalone compile", len(e.studentIDs), e.total)
}

func (e *gateError) Unwrap() error { return transcribe.ErrCompileFailed }

// gateFailureMessage is the professor-facing message for a gate failure:
// attributed when the gate could attribute, honest about timeouts (a timeout
// is not a compile failure and retrying can succeed), generic otherwise.
func gateFailureMessage(problem int, err error) string {
	var ge *gateError
	switch {
	case errors.As(err, &ge):
		return fmt.Sprintf("problem %d: the transcription for student(s) %s failed to compile; the remaining %d answer(s) are fine — not shipping an unverified bundle",
			problem, strings.Join(ge.studentIDs, ", "), ge.total-len(ge.studentIDs))
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return fmt.Sprintf("problem %d: the compile gate timed out before verification finished — try again", problem)
	default:
		return fmt.Sprintf("problem %d: generated LaTeX failed to compile — not shipping an unverified bundle", problem)
	}
}

// errContentFree strips a compile error down to its type for logging: tectonic
// diagnostics quote source lines, and the source embeds student answer text
// (transcribe.Compile already suppresses stderr, so this is belt and braces).
func errContentFree(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, transcribe.ErrCompileFailed):
		return "tex compile failed"
	case errors.Is(err, transcribe.ErrNoEngine):
		return "no engine"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "canceled or timed out"
	default:
		// Compile's own wrapper messages are content-free by construction
		// (stderr is suppressed at the source), so the chain is safe to log.
		return err.Error()
	}
}

// absFontPath resolves the configured CJK font to an absolute path. The config
// value is commonly relative (./data/fonts/…), which works for the report
// renderer (it runs in the repo cwd) but silently breaks the compile gate:
// tectonic runs in a throwaway temp dir, so a relative Path= in the preamble
// resolves to nowhere and fontspec fails the build. Caught live by the
// handwriting pilot — the gate refused every bundle while the same documents
// compiled fine in tests that absolutize the path.
func absFontPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
