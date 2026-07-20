package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/HaoWen46/adagrade/internal/store"
)

// auditJSON is one audit_log row (trust spec §6, D39). detail is passed through as
// raw JSONB — it may contain ids, but never PII beyond what the UI already shows
// (CLAUDE.md), and this endpoint is admin-only.
type auditJSON struct {
	ID          int64           `json:"id"`
	ActorUserID *int64          `json:"actor_user_id,omitempty"`
	ActorEmail  string          `json:"actor_email,omitempty"`
	Action      string          `json:"action"`
	TargetKind  string          `json:"target_kind"`
	TargetID    string          `json:"target_id"`
	Detail      json.RawMessage `json:"detail,omitempty"`
	CreatedAt   *time.Time      `json:"created_at,omitempty"`
}

func toAuditJSON(row store.AuditRow) auditJSON {
	out := auditJSON{
		ID: row.ID, Action: row.Action, TargetKind: row.TargetKind, TargetID: row.TargetID,
		CreatedAt: tsPtr(row.CreatedAt),
	}
	if row.ActorUserID.Valid {
		out.ActorUserID = &row.ActorUserID.Int64
	}
	if row.ActorEmail.Valid {
		out.ActorEmail = row.ActorEmail.String
	}
	if len(row.Detail) > 0 {
		out.Detail = json.RawMessage(row.Detail)
	}
	return out
}

// handleListAudit is the admin audit-log read path (trust spec §6, D39): all
// filters optional, newest-first, paginated (default/limit 50 — ListAudit itself
// enforces the same default so a filter-less call can never scan the whole
// ever-growing table). Route is admin-gated via requireRole in api.go.
func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	var actorID int64
	if v := q.Get("actor"); v != "" {
		actorID, _ = strconv.ParseInt(v, 10, 64)
	}

	rows, err := s.store.ListAudit(r.Context(), store.ListAuditParams{
		TargetKind: q.Get("target_kind"),
		TargetID:   q.Get("target_id"),
		Action:     q.Get("action"),
		ActorID:    actorID,
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "audit list failed")
		return
	}
	out := make([]auditJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAuditJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}
