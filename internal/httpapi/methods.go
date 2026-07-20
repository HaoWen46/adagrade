package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

type methodVersionJSON struct {
	ID        int64           `json:"id"`
	Version   int32           `json:"version"`
	Config    json.RawMessage `json:"config"`
	CreatedAt *time.Time      `json:"created_at,omitempty"`
}

type methodJSON struct {
	ID       int64              `json:"id"`
	Name     string             `json:"name"`
	Archived bool               `json:"archived"`
	Latest   *methodVersionJSON `json:"latest,omitempty"`
}

// validateMethodConfig parses the config and checks the pinned prompt template
// exists. Non-standard policies (D25) additionally require the pinned template to be
// policy-aware — i.e. it actually branches on {{.Policy}} — since a v1-era template
// would silently render the same text regardless of the configured stance. Standard
// stays allowed on old templates: it renders the legacy (pre-policy) behavior.
func (s *Server) validateMethodConfig(r *http.Request, raw json.RawMessage) (grading.MethodConfig, string) {
	cfg, err := grading.ParseMethodConfig(raw)
	if err != nil {
		return cfg, err.Error()
	}
	tmpl, err := s.store.Q.GetPromptTemplateVersion(r.Context(), cfg.PromptTemplateVersionID)
	if err != nil {
		return cfg, "prompt_template_version_id does not exist"
	}
	if cfg.Policy != grading.PolicyStandard &&
		!strings.Contains(tmpl.SystemTemplate, ".Policy") && !strings.Contains(tmpl.UserTemplate, ".Policy") {
		return cfg, "the pinned prompt template predates grading policies; pin the latest template version"
	}
	return cfg, ""
}

func (s *Server) handleListMethods(w http.ResponseWriter, r *http.Request) {
	methods, err := s.store.Q.ListGradingMethods(r.Context(), r.URL.Query().Get("include_archived") == "1")
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]methodJSON, 0, len(methods))
	for _, m := range methods {
		mj := methodJSON{ID: m.ID, Name: m.Name, Archived: m.ArchivedAt.Valid}
		if v, err := s.store.Q.LatestMethodVersion(r.Context(), m.ID); err == nil {
			mj.Latest = &methodVersionJSON{ID: v.ID, Version: v.Version, Config: v.Config, CreatedAt: tsPtr(v.CreatedAt)}
		}
		out = append(out, mj)
	}
	writeJSON(w, http.StatusOK, map[string]any{"methods": out})
}

func (s *Server) handleCreateMethod(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string          `json:"name"`
		Config json.RawMessage `json:"config"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.Name == "" || body.Config == nil {
		apiError(w, http.StatusBadRequest, "name and config are required")
		return
	}
	if _, msg := s.validateMethodConfig(r, body.Config); msg != "" {
		apiError(w, http.StatusBadRequest, msg)
		return
	}
	me, _ := currentUser(r.Context())
	var m db.GradingMethod
	var v db.GradingMethodVersion
	err := s.store.WithTx(r.Context(), func(q *db.Queries) error {
		var err error
		if m, err = q.CreateGradingMethod(r.Context(), body.Name); err != nil {
			return err
		}
		v, err = q.CreateMethodVersion(r.Context(), db.CreateMethodVersionParams{
			MethodID: m.ID, Config: body.Config,
			CreatedBy: pgtype.Int8{Int64: me.ID, Valid: true},
		})
		return err
	})
	if err != nil {
		apiError(w, http.StatusConflict, "method create failed (duplicate name?)")
		return
	}
	s.audit(r, "method.create", "method", strconv.FormatInt(m.ID, 10), map[string]any{"name": m.Name})
	writeJSON(w, http.StatusCreated, methodJSON{ID: m.ID, Name: m.Name,
		Latest: &methodVersionJSON{ID: v.ID, Version: v.Version, Config: v.Config, CreatedAt: tsPtr(v.CreatedAt)}})
}

func (s *Server) handleGetMethod(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid method id")
		return
	}
	m, err := s.store.Q.GetGradingMethod(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such method")
		return
	}
	versions, err := s.store.Q.ListMethodVersions(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "versions fetch failed")
		return
	}
	vj := make([]methodVersionJSON, 0, len(versions))
	for _, v := range versions {
		vj = append(vj, methodVersionJSON{ID: v.ID, Version: v.Version, Config: v.Config, CreatedAt: tsPtr(v.CreatedAt)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"method":   methodJSON{ID: m.ID, Name: m.Name, Archived: m.ArchivedAt.Valid},
		"versions": vj,
	})
}

func (s *Server) handleCreateMethodVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid method id")
		return
	}
	if _, err := s.store.Q.GetGradingMethod(r.Context(), id); err != nil {
		apiError(w, http.StatusNotFound, "no such method")
		return
	}
	var body struct {
		Config json.RawMessage `json:"config"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.Config == nil {
		apiError(w, http.StatusBadRequest, "config is required")
		return
	}
	if _, msg := s.validateMethodConfig(r, body.Config); msg != "" {
		apiError(w, http.StatusBadRequest, msg)
		return
	}
	me, _ := currentUser(r.Context())
	v, err := s.store.Q.CreateMethodVersion(r.Context(), db.CreateMethodVersionParams{
		MethodID: id, Config: body.Config,
		CreatedBy: pgtype.Int8{Int64: me.ID, Valid: true},
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "version create failed")
		return
	}
	s.audit(r, "method.version", "method", strconv.FormatInt(id, 10), map[string]any{"version": v.Version})
	writeJSON(w, http.StatusCreated, methodVersionJSON{ID: v.ID, Version: v.Version, Config: v.Config, CreatedAt: tsPtr(v.CreatedAt)})
}

func (s *Server) handleArchiveMethod(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid method id")
		return
	}
	var body struct {
		Archived bool `json:"archived"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	m, err := s.store.Q.SetMethodArchived(r.Context(), db.SetMethodArchivedParams{ID: id, Archived: body.Archived})
	if err != nil {
		apiError(w, http.StatusNotFound, "no such method")
		return
	}
	s.audit(r, "method.archive", "method", strconv.FormatInt(id, 10), map[string]any{"archived": body.Archived})
	writeJSON(w, http.StatusOK, methodJSON{ID: m.ID, Name: m.Name, Archived: m.ArchivedAt.Valid})
}

// handleGradingPolicies exposes the curated policy catalog (D25) straight from
// grading.Policies — the single source of truth for both the runner's ValidPolicy
// check and this UI-facing list. Any authenticated role may read it.
func (s *Server) handleGradingPolicies(w http.ResponseWriter, r *http.Request) {
	type policyJSON struct {
		Key       string `json:"key"`
		Label     string `json:"label"`
		Tagline   string `json:"tagline"`
		WhenToUse string `json:"when_to_use"`
	}
	out := make([]policyJSON, 0, len(grading.Policies))
	for _, p := range grading.Policies {
		out = append(out, policyJSON{Key: p.Key, Label: p.Label, Tagline: p.Tagline, WhenToUse: p.WhenToUse})
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": out})
}

func (s *Server) handleGetPromptTemplate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	v, err := s.store.Q.LatestPromptTemplate(r.Context(), name)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such prompt template")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": v.ID, "name": v.Name, "version": v.Version,
		"system_template": v.SystemTemplate, "user_template": v.UserTemplate,
	})
}
