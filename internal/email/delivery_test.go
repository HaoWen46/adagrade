package email

import "testing"

func TestStableCorrelationFitsEveryMIMEBoundary(t *testing.T) {
	id, err := messageCorrelationID("publish-item-stable-key")
	if err != nil {
		t.Fatal(err)
	}
	// RFC 2046 limits a MIME boundary parameter to at most 70 characters. The
	// mixed prefix is the longest prefix used by file/SMTP message builders.
	if got := len("adamarker-mixed-" + id); got > 70 {
		t.Fatalf("stable MIME boundary length = %d, want <= 70 (id=%q)", got, id)
	}
}
