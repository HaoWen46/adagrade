package grading

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/template"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// PolicyRegradeStrict is the single baked-in stance pinned on every AI re-grade record
// (spec §8, D50) — NOT one of the three curated grading policies (D25). It is stored in
// grading_records.policy alongside source='regrade_ai' (migration 0024 admits it).
const PolicyRegradeStrict = "regrade_strict"

// Terminal AI re-grade failure reasons (spec §8, Finding 3): short constant strings
// persisted verbatim to regrade_requests.ai_error so a TA sees WHY an AI re-grade
// didn't produce a record, without ever writing student text, request text, or raw
// model output there (CLAUDE.md PII rule — these are the only three literal values
// that ever reach that column). A mid-flight shutdown cancellation (F17) is
// deliberately NOT one of these — it isn't terminal, the job simply re-runs.
const (
	// AIErrorProviderRemoved matches spec §8's exact promised wording: the contested
	// method's provider is gone/unconfigured.
	AIErrorProviderRemoved = "AI unavailable — provider removed"
	// AIErrorNoContestedRecord: the request's student has no officially-graded answer
	// in scope to re-examine (ErrNoContestedRecord).
	AIErrorNoContestedRecord = "AI re-grade failed — no graded answer to re-examine"
	// AIErrorOutputInvalid: the model's output stayed malformed past the re-ask cap
	// (D12) — a MalformedError surfaced by RegradeAssist.
	AIErrorOutputInvalid = "AI re-grade failed — model output was invalid after re-asking"
	// AIErrorNoMethodPinned: the contested official record has no pinned method version
	// (e.g. an old human official graded before methods were pinned), so there's no
	// provider/model to re-run with — the answer is skipped. Surfaced so the TA's button
	// doesn't appear to silently do nothing (M-drift4), consistent with provider-removed.
	AIErrorNoMethodPinned = "AI unavailable — no method pinned on the original grade; resolve manually"
)

// RenderRegradePrompt executes an AI re-grade template body against RegradePromptData.
// Separate from RenderPrompt because the regrade template references the re-grade-only
// fields (OriginalScores, RequestText, ProblemIDHint) that a plain PromptData lacks —
// under missingkey=error the grading render func would reject them.
func RenderRegradePrompt(tmpl string, data RegradePromptData) (string, error) {
	t, err := template.New("regrade-prompt").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("regrade prompt template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("regrade prompt render: %w", err)
	}
	return buf.String(), nil
}

// RegradeAssistInput is one contested answer to re-examine (spec §8, per-sub-item
// re-scope). All version ids are pinned from the CONTESTED official record so the
// re-grade is apples-to-apples with the original (D53); RequestText is the student's
// complaint for THIS problem only, already REDACTED (D51) by the caller before it
// reaches here — this executor never sees or logs the raw identity.
type RegradeAssistInput struct {
	SubItemID int64 // the regrade_request_problems row this re-grade links its record to (spec §5)
	AnswerID  int64

	// Pinned from the contested record (D53).
	MethodVersionID            int64
	RubricVersionID            int64
	ReferenceSolutionVersionID int64 // 0 ⇒ none
	PromptTemplateVersionID    int64 // the regrade_v1 template version (resolved by the caller)

	// Original grade context (from the DB, never the student text).
	OriginalScores  []CriterionScore
	OriginalComment string

	// Redacted request context (D51).
	RequestText   string
	ProblemNumber int32 // the assessment problem number the request named, 0 ⇒ none
}

// RegradeAssist runs one stricter AI re-grade for a single contested answer and appends
// a grading_records row (source='regrade_ai', policy='regrade_strict'), linking it to
// the regrade SUB-ITEM via ai_record_id (spec §5). It mirrors gradeLeaf's provider/validation/clamp
// path but at single-record scope: no run, no run-item state machine, no per-run cost
// cap. NEVER touches the official pointer — the TA compares old vs new and walks the
// normal path (D50).
//
// It returns the appended record and reuses the runner's provider source, blob store,
// and pricing lookup. Returned errors are plain (the caller — a River job — decides
// retry/terminal via its own attempt budget); a masked-copy gate failure is terminal
// (the same D10 law grading obeys: only masked images may reach the provider).
func (r *Runner) RegradeAssist(ctx context.Context, in RegradeAssistInput) (db.GradingRecord, error) {
	mv, err := r.Store.Q.GetMethodVersion(ctx, in.MethodVersionID)
	if err != nil {
		return db.GradingRecord{}, fmt.Errorf("regrade: pinned method version %d: %w", in.MethodVersionID, err)
	}
	cfg, err := ParseMethodConfig(mv.Config)
	if err != nil {
		return db.GradingRecord{}, fmt.Errorf("regrade: method config: %w", err)
	}
	provider, limiter, err := r.Providers.Provider(ctx, cfg.Provider)
	if err != nil {
		// A removed/disabled provider is terminal — the contested method's provider is
		// gone (spec §8: "AI unavailable — provider removed"). Surface it as-is so the
		// caller can record it rather than spin.
		return db.GradingRecord{}, err
	}

	// Load masked page images — the ONLY thing that may reach the provider (D10),
	// through the SAME sealed imaging.LoadMasked path grading uses.
	pages, err := r.Store.Q.ListAnswerPages(ctx, in.AnswerID)
	if err != nil {
		return db.GradingRecord{}, err
	}
	if len(pages) == 0 {
		return db.GradingRecord{}, errors.New("regrade: answer has no pages")
	}
	images := make([]imaging.ProviderImage, 0, len(pages))
	shas := make([]string, 0, len(pages))
	for _, pg := range pages {
		if !pg.MaskedImageRef.Valid || pg.MaskReviewStatus != "accepted" {
			return db.GradingRecord{}, errors.New("regrade: page lacks an accepted masked copy (mask gate)")
		}
		rc, err := r.Blobs.Get(ctx, pg.MaskedImageRef.String)
		if err != nil {
			return db.GradingRecord{}, fmt.Errorf("regrade: masked blob missing: %w", err)
		}
		raw, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return db.GradingRecord{}, err
		}
		m, err := imaging.LoadMasked(pg.MaskedImageRef.String, raw)
		if err != nil {
			return db.GradingRecord{}, err
		}
		images = append(images, m)
		shas = append(shas, pg.ImageSha256)
	}

	// Assemble the frozen prompt inputs. Rubric/reference are pinned from the contested
	// record (D53); the prompt template is the pinned regrade_v1 version.
	answer, err := r.Store.Q.GetAnswer(ctx, in.AnswerID)
	if err != nil {
		return db.GradingRecord{}, err
	}
	problem, err := r.Store.Q.GetProblem(ctx, answer.ProblemID)
	if err != nil {
		return db.GradingRecord{}, err
	}
	rv, err := r.Store.Q.GetRubricVersion(ctx, in.RubricVersionID)
	if err != nil {
		return db.GradingRecord{}, err
	}
	criteria, err := r.Store.Q.ListRubricCriteria(ctx, rv.ID)
	if err != nil {
		return db.GradingRecord{}, err
	}
	refSolution := ""
	var refSolVersionID pgtype.Int8
	if in.ReferenceSolutionVersionID != 0 {
		sv, err := r.Store.Q.GetSolutionVersion(ctx, in.ReferenceSolutionVersionID)
		if err != nil {
			return db.GradingRecord{}, err
		}
		refSolution = sv.Content
		refSolVersionID = pgtype.Int8{Int64: in.ReferenceSolutionVersionID, Valid: true}
	}
	tmpl, err := r.Store.Q.GetPromptTemplateVersion(ctx, in.PromptTemplateVersionID)
	if err != nil {
		return db.GradingRecord{}, fmt.Errorf("regrade: pinned prompt template missing: %w", err)
	}

	data := BuildRegradePromptData(problem, rv, criteria, refSolution, in)
	systemPrompt, err := RenderRegradePrompt(tmpl.SystemTemplate, data)
	if err != nil {
		return db.GradingRecord{}, err
	}
	userPrompt, err := RenderRegradePrompt(tmpl.UserTemplate, data)
	if err != nil {
		return db.GradingRecord{}, err
	}
	schema := BuildOutputSchema(criteria)

	// Call with bounded re-asks on malformed output (same contract as grading, spec §5).
	var output ModelOutput
	var result llm.Result
	prompt := userPrompt
	for attempt := 0; ; attempt++ {
		if limiter != nil {
			if err := limiter.Wait(ctx); err != nil {
				return db.GradingRecord{}, retryableError{err}
			}
		}
		result, err = provider.Grade(ctx, cfg.Model, llm.Request{
			System:         systemPrompt,
			Prompt:         prompt,
			Images:         images,
			Schema:         schema,
			Temperature:    cfg.Temperature,
			MaxTokens:      cfg.MaxTokens,
			ReasoningLevel: cfg.ReasoningLevel,
		})
		if err != nil {
			return db.GradingRecord{}, retryableError{err} // provider/transport → caller retries
		}
		output, err = ParseModelOutput(result.JSON, criteria)
		if err == nil {
			break
		}
		var mal MalformedError
		if !errors.As(err, &mal) {
			return db.GradingRecord{}, err
		}
		if attempt >= cfg.ReaskCap {
			return db.GradingRecord{}, mal // terminal (D12)
		}
		r.log().Warn("regrade: re-asking after malformed output", "answer_id", in.AnswerID, "attempt", attempt+1)
		prompt = userPrompt + "\n\nYour previous response was rejected: " + mal.Msg +
			". Respond again following the schema exactly — every rubric criterion exactly once."
	}

	// Snap/clamp in Go; the model's arithmetic is never trusted (D4).
	byID := make(map[int64]db.RubricCriterium, len(criteria))
	for _, c := range criteria {
		byID[c.ID] = c
	}
	increment := store.NumStr(rv.ScoreIncrement)
	final := make([]CriterionScore, 0, len(output.Criteria))
	var adjustments []Adjustment
	var totals []string
	for _, sc := range output.Criteria {
		crit := byID[sc.CriterionID]
		snapped, adjusted, err := SnapClamp(sc.Score, store.NumStr(crit.Points), increment)
		if err != nil {
			return db.GradingRecord{}, MalformedError{fmt.Sprintf("criterion %d score %q: %v", sc.CriterionID, sc.Score, err)}
		}
		if adjusted {
			adjustments = append(adjustments, Adjustment{CriterionID: sc.CriterionID, From: sc.Score, To: snapped})
		}
		final = append(final, CriterionScore{CriterionID: sc.CriterionID, Score: snapped, Rationale: sc.Rationale})
		totals = append(totals, snapped)
	}

	illegible := output.Confidence == "illegible"
	var total pgtype.Numeric // NULL for the refusal path (D12)
	if !illegible {
		totalStr, err := SumDecimals(totals)
		if err != nil {
			return db.GradingRecord{}, err
		}
		if total, err = store.Num(totalStr); err != nil {
			return db.GradingRecord{}, err
		}
	}

	scoresJSON, _ := json.Marshal(final)
	adjJSON := []byte("[]")
	if adjustments != nil {
		adjJSON, _ = json.Marshal(adjustments)
	}
	rawOutput, _ := json.Marshal(map[string]any{
		"resolved_model": result.Model,
		"output":         json.RawMessage(result.JSON),
	})
	costUSD := r.lookupCost(ctx, cfg.Provider, cfg.Model, int64(result.InputTokens), int64(result.OutputTokens))

	// Append the record + link it to the request atomically. The record is source=
	// 'regrade_ai', policy='regrade_strict', run_id NULL, no created_by — NEVER official.
	var rec db.GradingRecord
	err = r.Store.WithTx(ctx, func(q *db.Queries) error {
		var err error
		rec, err = q.InsertRegradeAIRecord(ctx, db.InsertRegradeAIRecordParams{
			AnswerID:                   in.AnswerID,
			Provider:                   pgtype.Text{String: cfg.Provider, Valid: true},
			ModelID:                    pgtype.Text{String: cfg.Model, Valid: true},
			MethodVersionID:            pgtype.Int8{Int64: mv.ID, Valid: true},
			RubricVersionID:            rv.ID,
			ReferenceSolutionVersionID: refSolVersionID,
			PromptTemplateVersionID:    pgtype.Int8{Int64: tmpl.ID, Valid: true},
			GradedImageShas:            shas,
			CriterionScores:            scoresJSON,
			Total:                      total,
			Comment:                    output.OverallComment,
			Transcription:              pgtype.Text{String: output.Transcription, Valid: output.Transcription != ""},
			Confidence:                 pgtype.Text{String: output.Confidence, Valid: true},
			Adjustments:                adjJSON,
			RawOutput:                  rawOutput,
			InputTokens:                pgtype.Int4{Int32: int32(result.InputTokens), Valid: true},
			OutputTokens:               pgtype.Int4{Int32: int32(result.OutputTokens), Valid: true},
			CostUsd:                    costUSD,
			Temperature:                pgtype.Float4{Float32: float32(cfg.Temperature), Valid: true},
			Policy:                     pgtype.Text{String: PolicyRegradeStrict, Valid: true},
		})
		if err != nil {
			return err
		}
		_, err = q.SetProblemAIRecord(ctx, db.SetProblemAIRecordParams{
			ID:         in.SubItemID,
			AiRecordID: pgtype.Int8{Int64: rec.ID, Valid: true},
		})
		return err
	})
	if err != nil {
		return db.GradingRecord{}, err
	}
	r.log().Info("regrade: appended AI re-grade record",
		"sub_item_id", in.SubItemID, "answer_id", in.AnswerID, "record_id", rec.ID)
	return rec, nil
}

// BuildRegradePromptData assembles the AI re-grade template data: the shared grading
// fields (via the same field mapping BuildPromptData uses) plus the contested record's
// original scores/comment and the redacted request text. Policy is deliberately left
// empty on the embedded PromptData — the regrade template does not branch on it.
func BuildRegradePromptData(problem db.Problem, rv db.RubricVersion, criteria []db.RubricCriterium, refSolution string, in RegradeAssistInput) RegradePromptData {
	base := BuildPromptData(problem, rv, criteria, refSolution, "")
	base.Policy = "" // never rendered by the regrade template; keep it empty for clarity
	return RegradePromptData{
		PromptData:      base,
		OriginalScores:  in.OriginalScores,
		OriginalComment: in.OriginalComment,
		RequestText:     in.RequestText,
		ProblemIDHint:   in.ProblemNumber,
	}
}

// EnsureRegradeTemplateSeed seeds the regrade_v1 prompt template read-only, using the
// same version-bump-on-constant-change pattern as the grading template (D25): create
// version 1 on a fresh DB, append N+1 when the constants change, never mutate an
// existing version (old regrade_ai record pins stay reproducible). Returns the latest
// version row so the caller can pin its id.
func EnsureRegradeTemplateSeed(ctx context.Context, st *store.Store) (db.PromptTemplateVersion, error) {
	tmpl, err := st.Q.LatestPromptTemplate(ctx, RegradeTemplateName)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return st.Q.CreatePromptTemplateVersion(ctx, db.CreatePromptTemplateVersionParams{
			Name:           RegradeTemplateName,
			SystemTemplate: RegradeSystemTemplate,
			UserTemplate:   RegradeUserTemplate,
		})
	case err != nil:
		return db.PromptTemplateVersion{}, fmt.Errorf("seed regrade template: %w", err)
	case tmpl.SystemTemplate != RegradeSystemTemplate || tmpl.UserTemplate != RegradeUserTemplate:
		return st.Q.CreatePromptTemplateVersion(ctx, db.CreatePromptTemplateVersionParams{
			Name:           RegradeTemplateName,
			SystemTemplate: RegradeSystemTemplate,
			UserTemplate:   RegradeUserTemplate,
		})
	default:
		return tmpl, nil
	}
}
