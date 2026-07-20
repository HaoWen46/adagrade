package httpapi

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/store"
)

var filenameSafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// handleExportCSV streams the per-assessment gradebook (B-C11): one row per student
// WITH answers rows for this assessment — a roster student never ingested has no row
// (the publish preview's not_ingested blocker is where that gap surfaces) — one
// column per problem (official total or empty — never a silent 0, D3), the exact-sum
// assessment total, completeness columns so a partial export is visibly partial, and
// a final status column (`active`/`withdrawn`): withdrawn students stay in the export
// with their grades, flagged instead of silently dropped (roster-lifecycle plan
// 2026-07-10, locked semantics (e)).
func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
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
	problems, err := s.store.Q.ListProblems(r.Context(), aid)
	if err != nil || len(problems) == 0 {
		apiError(w, http.StatusBadRequest, "assessment has no problems")
		return
	}
	rows, err := s.store.Q.ExportRows(r.Context(), aid)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "export query failed")
		return
	}

	type cell struct{ total, source string }
	perStudent := map[string]map[int32]cell{}
	names := map[string]string{}
	withdrawn := map[string]bool{}
	order := []string{}
	for _, row := range rows {
		if _, seen := perStudent[row.StudentID]; !seen {
			perStudent[row.StudentID] = map[int32]cell{}
			names[row.StudentID] = row.Name
			withdrawn[row.StudentID] = row.Withdrawn
			order = append(order, row.StudentID)
		}
		c := cell{}
		if row.OfficialSource != "" {
			c.total = store.NumStr(row.Total)
			c.source = row.OfficialSource
		}
		perStudent[row.StudentID][row.ProblemNumber] = c
	}

	name := filenameSafe.ReplaceAllString(assessment.Name, "_")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "adamarker-"+name+"-grades.csv"))

	cw := csv.NewWriter(w)
	header := []string{"student_id", "name"}
	for _, p := range problems {
		header = append(header, fmt.Sprintf("P%d (max %s)", p.Number, store.NumStr(p.MaxPoints)))
	}
	header = append(header, "total", "graded_problems", "sources", "status")
	_ = cw.Write(header)

	for _, sid := range order {
		rec := []string{sid, names[sid]}
		var totals []string
		var sources []string
		graded := 0
		for _, p := range problems {
			c := perStudent[sid][p.Number]
			rec = append(rec, c.total)
			if c.total != "" {
				totals = append(totals, c.total)
				sources = append(sources, c.source)
				graded++
			}
		}
		total := ""
		if graded > 0 {
			if t, err := grading.SumDecimals(totals); err == nil {
				total = t
			}
		}
		status := "active"
		if withdrawn[sid] {
			status = "withdrawn"
		}
		rec = append(rec, total, fmt.Sprintf("%d/%d", graded, len(problems)), strings.Join(sources, "|"), status)
		_ = cw.Write(rec)
	}
	cw.Flush()
	s.audit(r, "grades.export", "assessment", strconv.FormatInt(aid, 10), map[string]any{"students": len(order)})
}
