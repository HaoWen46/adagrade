package email

import (
	"context"
	"testing"

	"github.com/HaoWen46/adagrade/internal/domain"
)

func TestNoneProvider_SendReturnsSkippedWithoutError(t *testing.T) {
	p := NewNoneProvider()
	id, err := p.Send(context.Background(), domain.OutboundEmail{
		To:      "s0000001@example.edu",
		Subject: "Midterm 2 — results",
	})
	if err != nil {
		t.Fatalf("none provider Send must not error: %v", err)
	}
	if id != "" {
		t.Errorf("none provider providerID: got %q, want empty (nothing was actually sent)", id)
	}
}

func TestNoneProvider_ParseInboundAlwaysErrors(t *testing.T) {
	p := NewNoneProvider()
	if _, err := p.ParseInbound([]byte(`{}`)); err == nil {
		t.Fatal("none provider has no inbound pipe — ParseInbound must error, not silently accept")
	}
}
