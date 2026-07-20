package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

type aggregationPolicyJSON struct {
	MethodVersionIDs []int64  `json:"method_version_ids"`
	Combiner         string   `json:"combiner"`
	FaultTolerance   int32    `json:"fault_tolerance"`
	FlagTriggers     []string `json:"flag_triggers"`
	SetOfficial      bool     `json:"set_official"`
}

func (s *Server) handleGetAggregationPolicy(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	p, err := s.store.Q.GetAggregationPolicy(r.Context(), aid)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"policy": nil})
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": aggregationPolicyJSON{
		MethodVersionIDs: p.MethodVersionIds, Combiner: p.Combiner,
		FaultTolerance: p.FaultTolerance, FlagTriggers: p.FlagTriggers, SetOfficial: p.SetOfficial,
	}})
}

var validTriggers = map[string]bool{
	grading.FlagAggDisagreement: true,
	grading.FlagAggMissing:      true,
	grading.FlagAggLowConf:      true,
}

func (s *Server) handlePutAggregationPolicy(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	var body aggregationPolicyJSON
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := grading.ValidatePolicyShape(len(body.MethodVersionIDs), int(body.FaultTolerance)); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Combiner != "majority" && body.Combiner != "mean" {
		apiError(w, http.StatusBadRequest, "combiner must be majority or mean")
		return
	}
	for _, t := range body.FlagTriggers {
		if !validTriggers[t] {
			apiError(w, http.StatusBadRequest, "unknown flag trigger "+t)
			return
		}
	}
	for _, mv := range body.MethodVersionIDs {
		if _, err := s.store.Q.GetMethodVersion(r.Context(), mv); err != nil {
			apiError(w, http.StatusBadRequest, "method version "+strconv.FormatInt(mv, 10)+" does not exist")
			return
		}
	}
	if body.FlagTriggers == nil {
		body.FlagTriggers = []string{}
	}
	me, _ := currentUser(r.Context())
	p, err := s.store.Q.UpsertAggregationPolicy(r.Context(), db.UpsertAggregationPolicyParams{
		AssessmentID: aid, MethodVersionIds: body.MethodVersionIDs,
		Combiner: body.Combiner, FaultTolerance: body.FaultTolerance,
		FlagTriggers: body.FlagTriggers, SetOfficial: body.SetOfficial,
		UpdatedBy: pgtype.Int8{Int64: me.ID, Valid: true},
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "save failed")
		return
	}
	s.audit(r, "aggregation.policy", "assessment", strconv.FormatInt(aid, 10), map[string]any{
		"combiner": p.Combiner, "fault_tolerance": p.FaultTolerance, "panel": len(p.MethodVersionIds),
	})
	writeJSON(w, http.StatusOK, map[string]any{"policy": body})
}

func (s *Server) handleRunAggregation(w http.ResponseWriter, r *http.Request) {
	aid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	me, _ := currentUser(r.Context())
	rep, err := grading.ExecuteAggregation(r.Context(), s.store, aid, me.ID)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "aggregation.run", "assessment", strconv.FormatInt(aid, 10), map[string]any{
		"written": rep.AggregatesWritten, "officials": rep.OfficialsSet,
	})
	writeJSON(w, http.StatusOK, rep)
}
