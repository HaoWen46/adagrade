package email

import (
	"context"
	"errors"

	"github.com/HaoWen46/adagrade/internal/domain"
)

// NoneProvider is the "grading without notification" mode (spec §3): Send
// succeeds without transmitting anything so the publish pipeline records every
// item as skipped (Q3's job) instead of failing the whole publish. There is no
// inbound pipe, so ParseInbound always errors — nothing should ever be routed to
// this provider's (nonexistent) webhook.
type NoneProvider struct{}

// NewNoneProvider constructs the no-op provider.
//
// Caller obligation (spec §3): selecting "none" must be a loud, deliberate
// choice, not a silent default — this constructor does not enforce that.
// Spec §3 requires (1) a loud warning at startup when ADAMARKER_EMAIL_PROVIDER
// resolves to "none", and (2) a warning at publish time, on every publish that
// would otherwise have sent notifications, so instructors don't discover
// after the fact that grades were never emailed. Both obligations belong to
// the caller — config wiring for (1), the publish handler for (2) — since
// this package has no logging/warning seam of its own and NoneProvider.Send
// intentionally returns (empty id, nil error) to keep "disabled" distinct
// from "failed" (see Send's doc comment). Whoever wires config and the
// publish handler must not miss this.
func NewNoneProvider() *NoneProvider {
	return &NoneProvider{}
}

// Send is a deliberate no-op: it returns an empty providerID and a nil error so
// callers can distinguish "sent" (non-empty id) from "provider disabled"
// (empty id, no error) without treating disablement as a failure.
func (p *NoneProvider) Send(ctx context.Context, msg domain.OutboundEmail) (string, error) {
	return "", nil
}

// ErrNoInboundPipe is returned by NoneProvider.ParseInbound — this mode has no
// webhook configured, so any call here indicates a misconfigured route.
var ErrNoInboundPipe = errors.New("email: none provider has no inbound pipe")

func (p *NoneProvider) ParseInbound(raw []byte) (domain.InboundEmail, error) {
	return domain.InboundEmail{}, ErrNoInboundPipe
}
