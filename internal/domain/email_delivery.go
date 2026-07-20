package domain

import "errors"

// EmailDeliveryOutcome describes what a provider error proves about acceptance
// of one outbound message. It intentionally does not claim "failed" versus
// "sent": outcome_unknown means the provider may have accepted the delivery,
// so blindly retrying can duplicate it.
type EmailDeliveryOutcome string

const (
	// EmailDeliveryDefinitelyNotAccepted means the provider did not accept the
	// message. A caller can retry the same DeliveryKey without risking a duplicate
	// of this attempt.
	EmailDeliveryDefinitelyNotAccepted EmailDeliveryOutcome = "definitely_not_accepted"

	// EmailDeliveryOutcomeUnknown means acceptance cannot be determined. This is
	// the conservative default once a provider request may have crossed the
	// process boundary (for example, a lost SMTP DATA or HTTP response).
	EmailDeliveryOutcomeUnknown EmailDeliveryOutcome = "outcome_unknown"
)

// EmailDeliveryError attaches an acceptance outcome to an underlying provider
// error while preserving errors.Is/errors.As traversal through Unwrap.
type EmailDeliveryError struct {
	Outcome EmailDeliveryOutcome
	Err     error
}

func (e *EmailDeliveryError) Error() string {
	if e == nil || e.Err == nil {
		return "email delivery failed"
	}
	return e.Err.Error()
}

func (e *EmailDeliveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NewEmailDeliveryError classifies err. Invalid/empty outcomes are normalized
// to outcome_unknown so a future or legacy caller cannot accidentally make an
// unsafe retry look definitely safe. A nil err remains nil.
func NewEmailDeliveryError(outcome EmailDeliveryOutcome, err error) error {
	if err == nil {
		return nil
	}
	if outcome != EmailDeliveryDefinitelyNotAccepted && outcome != EmailDeliveryOutcomeUnknown {
		outcome = EmailDeliveryOutcomeUnknown
	}
	return &EmailDeliveryError{Outcome: outcome, Err: err}
}

// EmailDeliveryOutcomeOf returns the typed outcome carried anywhere in err's
// unwrap chain. Any non-nil untyped error defaults conservatively to unknown;
// nil returns the empty value because no delivery error occurred.
func EmailDeliveryOutcomeOf(err error) EmailDeliveryOutcome {
	if err == nil {
		return ""
	}
	var deliveryErr *EmailDeliveryError
	if !errors.As(err, &deliveryErr) {
		return EmailDeliveryOutcomeUnknown
	}
	if deliveryErr.Outcome == EmailDeliveryDefinitelyNotAccepted {
		return EmailDeliveryDefinitelyNotAccepted
	}
	return EmailDeliveryOutcomeUnknown
}

// IsEmailDefinitelyNotAccepted reports whether err proves non-acceptance.
func IsEmailDefinitelyNotAccepted(err error) bool {
	return err != nil && EmailDeliveryOutcomeOf(err) == EmailDeliveryDefinitelyNotAccepted
}

// IsEmailOutcomeUnknown reports whether err leaves provider acceptance
// ambiguous. Untyped non-nil provider errors intentionally return true.
func IsEmailOutcomeUnknown(err error) bool {
	return err != nil && EmailDeliveryOutcomeOf(err) == EmailDeliveryOutcomeUnknown
}
