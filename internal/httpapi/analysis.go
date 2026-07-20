package httpapi

import (
	"errors"
	"math/big"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// handleAssessmentAnalysis treats the assessment as a dataset for method comparison
// (plan §8): per-problem stats for every method that graded it, plus agreement with
// the latest human grades (same rubric version only, B-H20).
func (s *Server) handleAssessmentAnalysis(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	statRows, err := s.store.Q.MethodStatsForAssessment(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "stats query failed")
		return
	}
	agreeRows, err := s.store.Q.HumanAgreementForAssessment(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "agreement query failed")
		return
	}
	mixRows, err := s.store.Q.PolicyMixForAssessment(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "policy mix query failed")
		return
	}

	type stat struct {
		ProblemID       int64  `json:"problem_id"`
		ProblemNumber   int32  `json:"problem_number"`
		MaxPoints       string `json:"max_points"`
		MethodVersionID int64  `json:"method_version_id"`
		MethodID        int64  `json:"method_id"`
		MethodName      string `json:"method_name"`
		MethodVersion   int32  `json:"method_version"`
		Policy          string `json:"policy"` // empty for legacy (pre-D25) configs (D25)
		Records         int64  `json:"records"`
		MeanTotal       string `json:"mean_total"`
		MedianTotal     string `json:"median_total"`
		StddevTotal     string `json:"stddev_total"`
		Zeros           int64  `json:"zeros"`
		Maxes           int64  `json:"maxes"`
		ConfHigh        int64  `json:"conf_high"`
		ConfMedium      int64  `json:"conf_medium"`
		ConfLow         int64  `json:"conf_low"`
		ConfIllegible   int64  `json:"conf_illegible"`
		InputTokens     int64  `json:"input_tokens"`
		OutputTokens    int64  `json:"output_tokens"`
	}
	stats := make([]stat, 0, len(statRows))
	for _, row := range statRows {
		stats = append(stats, stat{
			ProblemID: row.ProblemID, ProblemNumber: row.ProblemNumber,
			MaxPoints:       store.NumStr(row.MaxPoints),
			MethodVersionID: row.MethodVersionID, MethodID: row.MethodID,
			MethodName: row.MethodName, MethodVersion: row.MethodVersion,
			Policy:    row.Policy,
			Records:   row.Records,
			MeanTotal: store.NumStr(row.MeanTotal), MedianTotal: store.NumStr(row.MedianTotal),
			StddevTotal: store.NumStr(row.StddevTotal),
			Zeros:       row.Zeros, Maxes: row.Maxes,
			ConfHigh: row.ConfHigh, ConfMedium: row.ConfMedium,
			ConfLow: row.ConfLow, ConfIllegible: row.ConfIllegible,
			InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
		})
	}

	type agreement struct {
		ProblemNumber   int32  `json:"problem_number"`
		MethodVersionID int64  `json:"method_version_id"`
		MethodName      string `json:"method_name"`
		MethodVersion   int32  `json:"method_version"`
		Pairs           int64  `json:"pairs"`
		MeanAbsDiff     string `json:"mean_abs_diff"`
		ExactMatches    int64  `json:"exact_matches"`
		WithinOne       int64  `json:"within_one"`
	}
	agreements := make([]agreement, 0, len(agreeRows))
	for _, row := range agreeRows {
		agreements = append(agreements, agreement{
			ProblemNumber:   row.ProblemNumber,
			MethodVersionID: row.MethodVersionID, MethodName: row.MethodName, MethodVersion: row.MethodVersion,
			Pairs:        row.Pairs,
			MeanAbsDiff:  store.NumStr(row.MeanAbsDiff),
			ExactMatches: row.ExactMatches, WithinOne: row.WithinOne,
		})
	}

	type policyMix struct {
		ProblemID     int64    `json:"problem_id"`
		ProblemNumber int32    `json:"problem_number"`
		Policies      []string `json:"policies"`
	}
	mix := make([]policyMix, 0, len(mixRows))
	for _, row := range mixRows {
		mix = append(mix, policyMix{ProblemID: row.ProblemID, ProblemNumber: row.ProblemNumber, Policies: row.Policies})
	}

	overrideRows, err := s.store.Q.OverrideRateByMethod(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "override rate query failed")
		return
	}
	type overrideRate struct {
		MethodVersionID     int64  `json:"method_version_id"`
		MethodName          string `json:"method_name"`
		MethodVersion       int32  `json:"method_version"`
		Answers             int64  `json:"answers"`
		HumanOverrides      int64  `json:"human_overrides"`
		ScoredDisagreements int64  `json:"scored_disagreements"` // human replaced a real AI score
		FilledBlanks        int64  `json:"filled_blanks"`        // human filled a cell the AI abstained on (illegible)
		OverrideRate        string `json:"override_rate"`        // decimal string in [0,1]; Answers is always >=1 per row (GROUP BY never emits an empty group)
		MeanAbsDiff         string `json:"mean_abs_diff"`
	}
	overrides := make([]overrideRate, 0, len(overrideRows))
	for _, row := range overrideRows {
		overrides = append(overrides, overrideRate{
			MethodVersionID: row.MethodVersionID, MethodName: row.MethodName, MethodVersion: row.MethodVersion,
			Answers: row.Answers, HumanOverrides: row.HumanOverrides,
			ScoredDisagreements: row.ScoredDisagreements, FilledBlanks: row.FilledBlanks,
			OverrideRate: store.NumStr(row.OverrideRate),
			MeanAbsDiff:  store.NumStr(row.MeanAbsDiff),
		})
	}

	// Disagreement block (analysis redesign plan, Task B1): where the methods
	// give the same student different scores. Same record-selection semantics
	// as the agreement query (see analysis.sql), so its numbers can sit next to
	// the stats/agreement tables without contradicting them. Both arrays stay
	// empty until at least two method-versions have comparable records — the
	// frontend's signal to hide the section entirely.
	disProblemRows, err := s.store.Q.DisagreementByProblem(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "disagreement query failed")
		return
	}
	type disagreementProblem struct {
		ProblemID       int64  `json:"problem_id"`
		ProblemNumber   int32  `json:"problem_number"`
		MaxPoints       string `json:"max_points"`
		AnswersCompared int64  `json:"answers_compared"`
		MedianSpread    string `json:"median_spread"`
		BigGapCount     int64  `json:"big_gap_count"`
	}
	disProblems := make([]disagreementProblem, 0, len(disProblemRows))
	for _, row := range disProblemRows {
		disProblems = append(disProblems, disagreementProblem{
			ProblemID: row.ProblemID, ProblemNumber: row.ProblemNumber,
			MaxPoints:       store.NumStr(row.MaxPoints),
			AnswersCompared: row.AnswersCompared,
			MedianSpread:    store.NumStr(row.MedianSpread),
			BigGapCount:     row.BigGapCount,
		})
	}

	topRows, err := s.store.Q.DisagreementTopAnswers(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "disagreement answers query failed")
		return
	}
	type disagreementScore struct {
		MethodVersionID int64  `json:"method_version_id"`
		MethodName      string `json:"method_name"`
		Total           string `json:"total"`
	}
	type disagreementAnswer struct {
		AnswerID       int64               `json:"answer_id"`
		StudentDisplay string              `json:"student_display"` // roster external id, never the name (PII)
		ProblemNumber  int32               `json:"problem_number"`
		Scores         []disagreementScore `json:"scores"`
		Spread         string              `json:"spread"`
	}
	// One SQL row per (answer, method-version), ordered by spread then answer —
	// fold consecutive rows of the same answer into its scores array.
	topAnswers := make([]disagreementAnswer, 0, 10)
	for _, row := range topRows {
		score := disagreementScore{
			MethodVersionID: row.MethodVersionID.Int64,
			MethodName:      row.MethodName,
			Total:           store.NumStr(row.Total),
		}
		if n := len(topAnswers); n > 0 && topAnswers[n-1].AnswerID == row.AnswerID {
			topAnswers[n-1].Scores = append(topAnswers[n-1].Scores, score)
			continue
		}
		topAnswers = append(topAnswers, disagreementAnswer{
			AnswerID: row.AnswerID, StudentDisplay: row.StudentDisplay,
			ProblemNumber: row.ProblemNumber,
			Scores:        []disagreementScore{score},
			Spread:        store.NumStr(row.Spread),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"stats": stats, "agreement": agreements, "policy_mix": mix, "override_rate": overrides,
		"disagreement": map[string]any{"problems": disProblems, "top_answers": topAnswers},
	})
}

// handlePromptPreview renders the EXACT prompt a method would send for a problem —
// the trust/debug window into "what does the model actually see" (plan §4: the
// prompt is an experimental surface, so it must be inspectable).
func (s *Server) handlePromptPreview(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid problem id")
		return
	}
	methodID, _ := strconv.ParseInt(r.URL.Query().Get("method_id"), 10, 64)
	if methodID == 0 {
		apiError(w, http.StatusBadRequest, "method_id query parameter is required")
		return
	}
	problem, err := s.store.Q.GetProblem(r.Context(), pid)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such problem")
		return
	}
	mv, err := s.store.Q.LatestMethodVersion(r.Context(), methodID)
	if err != nil {
		apiError(w, http.StatusBadRequest, "method has no versions")
		return
	}
	cfg, err := grading.ParseMethodConfig(mv.Config)
	if err != nil {
		apiError(w, http.StatusBadRequest, "method config invalid: "+err.Error())
		return
	}
	// Optional policy override (D25): lets the Methods UI preview all three stances
	// without saving a new method version.
	if p := r.URL.Query().Get("policy"); p != "" {
		if !grading.ValidPolicy(p) {
			apiError(w, http.StatusBadRequest, "policy must be lenient|standard|strict")
			return
		}
		cfg.Policy = p
	}
	rv, err := s.store.Q.LatestRubricVersion(r.Context(), pid)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusBadRequest, "this problem has no rubric yet — create one first")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "rubric fetch failed")
		return
	}
	criteria, err := s.store.Q.ListRubricCriteria(r.Context(), rv.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "criteria fetch failed")
		return
	}
	refSolution := ""
	refVersion := int32(0)
	if cfg.RefSolutions > 0 {
		sv, err := s.store.Q.LatestSolutionVersion(r.Context(), pid)
		if errors.Is(err, pgx.ErrNoRows) {
			apiError(w, http.StatusBadRequest, "this method includes a reference solution, but the problem has none yet")
			return
		}
		if err != nil {
			apiError(w, http.StatusInternalServerError, "solution fetch failed")
			return
		}
		refSolution = sv.Content
		refVersion = sv.Version
	}
	tmpl, err := s.store.Q.GetPromptTemplateVersion(r.Context(), cfg.PromptTemplateVersionID)
	if err != nil {
		apiError(w, http.StatusBadRequest, "the method's pinned prompt template no longer exists")
		return
	}

	data := grading.BuildPromptData(problem, rv, criteria, refSolution, cfg.Policy)
	system, err := grading.RenderPrompt(tmpl.SystemTemplate, data)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "system template render failed: "+err.Error())
		return
	}
	user, err := grading.RenderPrompt(tmpl.UserTemplate, data)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "user template render failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"system": system,
		"user":   user,
		"schema": string(grading.BuildOutputSchema(criteria)),
		"pins": map[string]any{
			"rubric_version":             rv.Version,
			"reference_solution_version": refVersion,
			"prompt_template":            tmpl.Name,
			"prompt_template_version":    tmpl.Version,
			"provider":                   cfg.Provider,
			"model":                      cfg.Model,
			"temperature":                cfg.Temperature,
			"policy":                     cfg.Policy,
		},
	})
}

// distTotals is the total-score stats JSON shape, from either source (trust spec §5).
type distTotals struct {
	N       int64  `json:"n"`
	Mean    string `json:"mean"`
	Stddev  string `json:"stddev"`
	Zeros   int64  `json:"zeros"`
	Maxes   int64  `json:"maxes"`
	ZeroPct string `json:"zero_pct"`
	MaxPct  string `json:"max_pct"`
}

// distCriterion is one rubric criterion's stats, from either source.
type distCriterion struct {
	CriterionID int64  `json:"criterion_id"`
	Description string `json:"description"`
	Points      string `json:"points"`
	N           int64  `json:"n"`
	Mean        string `json:"mean"`
	Stddev      string `json:"stddev"`
	Zeros       int64  `json:"zeros"`
	Maxes       int64  `json:"maxes"`
	ZeroPct     string `json:"zero_pct"`
	MaxPct      string `json:"max_pct"`
}

// pctStr formats 100*num/den as a decimal string to 1dp ("" for a zero denominator),
// exact big.Rat math — matching the money/points decimal-string convention, never
// float64.
func pctStr(num, den int64) string {
	if den == 0 {
		return ""
	}
	pct := new(big.Rat).SetFrac(big.NewInt(num*100), big.NewInt(den))
	return pct.FloatString(1)
}

// distSource is the query result set for one distribution source (official grades,
// or one run's model records) — DistributionTotals{Official,ForRun},
// DistributionHistogram{Official,ForRun}, and DistributionCriteria{Official,ForRun}
// all share this shape, just scoped by a different WHERE clause (see analysis.sql).
type distSource struct {
	totals    db.DistributionTotalsOfficialRow
	totalsErr error
	histogram []db.DistributionHistogramOfficialRow
	criteria  []db.DistributionCriteriaOfficialRow
}

// handleScoreDistribution answers "is Problem 2 all zeros" (trust spec §5, B-H14,
// D38): per-criterion + total mean/stddev/%zero/%max and a 10-bucket histogram over
// this problem's OFFICIAL grades. When the problem has NO official grades yet (v0
// sparse-officials default, D38: any official grade set at all is enough to prefer
// the human-reviewed source over a raw AI run), it falls back to the single latest
// run that graded this problem, with the response's "source" field labeled
// "ai_fallback" so the UI can show the caveat rather than presenting AI output as if
// it were reviewed truth. No data at all (neither officials nor any run) ⇒
// "source":"none" with a null total and an empty histogram — never a crash or a fake
// all-zero distribution.
func (s *Server) handleScoreDistribution(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid problem id")
		return
	}
	problem, err := s.store.Q.GetProblem(r.Context(), pid)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such problem")
		return
	}

	officialCounts, err := s.store.Q.CountOfficialForProblem(r.Context(), pid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "official-count query failed")
		return
	}

	empty := func(source string) {
		writeJSON(w, http.StatusOK, map[string]any{
			"problem_id": pid, "max_points": store.NumStr(problem.MaxPoints),
			"source": source, "total": nil, "histogram": [10]int64{}, "criteria": []distCriterion{},
		})
	}

	var src distSource
	source := "official"
	if officialCounts.Officials > 0 {
		src.totals, src.totalsErr = s.store.Q.DistributionTotalsOfficial(r.Context(), pid)
		if src.histogram, err = s.store.Q.DistributionHistogramOfficial(r.Context(), pid); err != nil {
			apiError(w, http.StatusInternalServerError, "distribution histogram query failed")
			return
		}
		if src.criteria, err = s.store.Q.DistributionCriteriaOfficial(r.Context(), pid); err != nil {
			apiError(w, http.StatusInternalServerError, "distribution criteria query failed")
			return
		}
	} else {
		source = "ai_fallback"
		runID, err := s.store.Q.LatestRunForProblem(r.Context(), pid)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			apiError(w, http.StatusInternalServerError, "latest-run query failed")
			return
		}
		if !runID.Valid {
			empty("none") // no officials, and no run has ever graded this problem.
			return
		}

		t, terr := s.store.Q.DistributionTotalsForRun(r.Context(), db.DistributionTotalsForRunParams{ProblemID: pid, RunID: runID})
		src.totals, src.totalsErr = db.DistributionTotalsOfficialRow(t), terr

		h, err := s.store.Q.DistributionHistogramForRun(r.Context(), db.DistributionHistogramForRunParams{ProblemID: pid, RunID: runID})
		if err != nil {
			apiError(w, http.StatusInternalServerError, "distribution histogram query failed")
			return
		}
		for _, row := range h {
			src.histogram = append(src.histogram, db.DistributionHistogramOfficialRow(row))
		}

		c, err := s.store.Q.DistributionCriteriaForRun(r.Context(), db.DistributionCriteriaForRunParams{ProblemID: pid, RunID: runID})
		if err != nil {
			apiError(w, http.StatusInternalServerError, "distribution criteria query failed")
			return
		}
		for _, row := range c {
			src.criteria = append(src.criteria, db.DistributionCriteriaOfficialRow(row))
		}
	}

	if src.totalsErr != nil && !errors.Is(src.totalsErr, pgx.ErrNoRows) {
		apiError(w, http.StatusInternalServerError, "distribution totals query failed")
		return
	}
	// GROUP BY collapses to zero rows when this source has no gradable records at
	// all — that's ErrNoRows on a :one query, not a real error.
	if errors.Is(src.totalsErr, pgx.ErrNoRows) || src.totals.N == 0 {
		empty(source)
		return
	}

	total := distTotals{
		N: src.totals.N, Mean: store.NumStr(src.totals.MeanTotal), Stddev: store.NumStr(src.totals.StddevTotal),
		Zeros: src.totals.Zeros, Maxes: src.totals.Maxes,
		ZeroPct: pctStr(src.totals.Zeros, src.totals.N), MaxPct: pctStr(src.totals.Maxes, src.totals.N),
	}

	var histogram [10]int64
	for _, row := range src.histogram {
		if row.Bucket >= 1 && row.Bucket <= 10 {
			histogram[row.Bucket-1] = row.N
		}
	}

	criteria := make([]distCriterion, 0, len(src.criteria))
	for _, row := range src.criteria {
		criteria = append(criteria, distCriterion{
			CriterionID: row.CriterionID, Description: row.Description, Points: store.NumStr(row.Points),
			N: row.N, Mean: store.NumStr(row.MeanScore), Stddev: store.NumStr(row.StddevScore),
			Zeros: row.Zeros, Maxes: row.Maxes,
			ZeroPct: pctStr(row.Zeros, row.N), MaxPct: pctStr(row.Maxes, row.N),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"problem_id": pid, "max_points": store.NumStr(problem.MaxPoints),
		"source": source, "total": total, "histogram": histogram, "criteria": criteria,
	})
}
