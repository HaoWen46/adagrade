package domain

import (
	"errors"
	"fmt"
	"testing"
)

func TestEmailDeliveryOutcomeValuesAreStable(t *testing.T) {
	if got, want := string(EmailDeliveryDefinitelyNotAccepted), "definitely_not_accepted"; got != want {
		t.Fatalf("EmailDeliveryDefinitelyNotAccepted = %q, want %q", got, want)
	}
	if got, want := string(EmailDeliveryOutcomeUnknown), "outcome_unknown"; got != want {
		t.Fatalf("EmailDeliveryOutcomeUnknown = %q, want %q", got, want)
	}
}

func TestEmailDeliveryErrorHelpers(t *testing.T) {
	underlying := errors.New("provider failed")
	definite := fmt.Errorf("send: %w", NewEmailDeliveryError(EmailDeliveryDefinitelyNotAccepted, underlying))
	if got := EmailDeliveryOutcomeOf(definite); got != EmailDeliveryDefinitelyNotAccepted {
		t.Fatalf("definite outcome = %q", got)
	}
	if !IsEmailDefinitelyNotAccepted(definite) || IsEmailOutcomeUnknown(definite) {
		t.Fatal("definite error helper classification is inconsistent")
	}
	if !errors.Is(definite, underlying) {
		t.Fatal("EmailDeliveryError must unwrap to its underlying error")
	}

	unknown := NewEmailDeliveryError(EmailDeliveryOutcomeUnknown, underlying)
	if got := EmailDeliveryOutcomeOf(unknown); got != EmailDeliveryOutcomeUnknown {
		t.Fatalf("explicit unknown outcome = %q", got)
	}
	if !IsEmailOutcomeUnknown(unknown) || IsEmailDefinitelyNotAccepted(unknown) {
		t.Fatal("unknown error helper classification is inconsistent")
	}
}

func TestEmailDeliveryOutcomeOfDefaultsUntypedErrorsConservatively(t *testing.T) {
	if got := EmailDeliveryOutcomeOf(errors.New("legacy provider error")); got != EmailDeliveryOutcomeUnknown {
		t.Fatalf("untyped provider error outcome = %q, want %q", got, EmailDeliveryOutcomeUnknown)
	}
	if got := EmailDeliveryOutcomeOf(nil); got != "" {
		t.Fatalf("nil error outcome = %q, want empty", got)
	}
	if IsEmailOutcomeUnknown(nil) || IsEmailDefinitelyNotAccepted(nil) {
		t.Fatal("nil is not a delivery error")
	}
}

func TestNewEmailDeliveryErrorInvalidOutcomeDefaultsUnknown(t *testing.T) {
	err := NewEmailDeliveryError(EmailDeliveryOutcome("future_value"), errors.New("failed"))
	if got := EmailDeliveryOutcomeOf(err); got != EmailDeliveryOutcomeUnknown {
		t.Fatalf("invalid outcome = %q, want conservative unknown", got)
	}
	if got := NewEmailDeliveryError(EmailDeliveryDefinitelyNotAccepted, nil); got != nil {
		t.Fatalf("wrapping nil = %v, want nil", got)
	}
}
