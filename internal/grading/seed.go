package grading

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// preferredSeedModels maps provider names to a sensible vision-capable default
// model, in preference order (D11).
var preferredSeedModels = []struct{ provider, model string }{
	{"qwen", "qwen3-vl-plus"},
	{"openrouter", "qwen/qwen3.5-flash-02-23"},
	{"anthropic", "claude-sonnet-5"},
}

// EnsureSeeds makes grading work out of the box (plan §4): the default prompt
// template always exists, and — when a vision-capable provider row is enabled — a
// default GradingMethod pointing at it. Idempotent; never overwrites user edits.
func EnsureSeeds(ctx context.Context, st *store.Store, log *slog.Logger) error {
	enabled, err := st.Q.ListEnabledProviders(ctx)
	if err != nil {
		return err
	}
	providers := make(map[string]struct{}, len(enabled))
	for _, p := range enabled {
		providers[p.Name] = struct{}{}
	}
	return ensureSeedsWith(ctx, st, providers, log)
}

func ensureSeedsWith(ctx context.Context, st *store.Store, providers map[string]struct{}, log *slog.Logger) error {
	// The default template is versioned data (D5/D25): seed it on a fresh DB, and
	// append version N+1 whenever the constants change (e.g. the policy-branch v2).
	// Existing versions are never mutated, so old method pins stay reproducible.
	tmpl, err := st.Q.LatestPromptTemplate(ctx, DefaultTemplateName)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		tmpl, err = st.Q.CreatePromptTemplateVersion(ctx, db.CreatePromptTemplateVersionParams{
			Name:           DefaultTemplateName,
			SystemTemplate: DefaultSystemTemplate,
			UserTemplate:   DefaultUserTemplate,
		})
		if err == nil {
			log.Info("seeded default prompt template", "name", DefaultTemplateName, "version", tmpl.Version)
		}
	case err != nil:
		return fmt.Errorf("seed prompt template: %w", err)
	case tmpl.SystemTemplate != DefaultSystemTemplate || tmpl.UserTemplate != DefaultUserTemplate:
		tmpl, err = st.Q.CreatePromptTemplateVersion(ctx, db.CreatePromptTemplateVersionParams{
			Name:           DefaultTemplateName,
			SystemTemplate: DefaultSystemTemplate,
			UserTemplate:   DefaultUserTemplate,
		})
		if err == nil {
			log.Info("bumped default prompt template", "name", DefaultTemplateName, "version", tmpl.Version)
		}
	}
	if err != nil {
		return fmt.Errorf("seed prompt template: %w", err)
	}

	// The AI re-grade assist template (spec §8, D50) is a separate versioned KIND,
	// seeded read-only with the same append-on-change discipline as the grading
	// template. Its version isn't wired to any default method — the runner resolves the
	// latest regrade_v1 at re-grade time (pinning it on the record) — so seeding it here
	// just guarantees it always exists.
	if rt, err := EnsureRegradeTemplateSeed(ctx, st); err != nil {
		return err
	} else {
		log.Info("ensured AI re-grade prompt template", "name", RegradeTemplateName, "version", rt.Version)
	}

	n, err := st.Q.CountGradingMethods(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	for _, pref := range preferredSeedModels {
		if _, ok := providers[pref.provider]; !ok {
			continue
		}
		cfg := MethodConfig{
			Provider:                pref.provider,
			Model:                   pref.model,
			Temperature:             0,
			RefSolutions:            1,
			ReaskCap:                2,
			PromptTemplateVersionID: tmpl.ID,
		}
		raw, _ := json.Marshal(cfg)
		m, err := st.Q.CreateGradingMethod(ctx, fmt.Sprintf("Default — %s", pref.model))
		if err != nil {
			return err
		}
		if _, err := st.Q.CreateMethodVersion(ctx, db.CreateMethodVersionParams{
			MethodID: m.ID, Config: raw,
		}); err != nil {
			return err
		}
		log.Info("seeded default grading method", "provider", pref.provider, "model", pref.model)
		return nil
	}
	log.Info("no vision-capable provider configured; skipped default method seed")
	return nil
}
