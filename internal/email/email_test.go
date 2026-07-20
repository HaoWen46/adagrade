package email

import (
	"testing"

	"github.com/HaoWen46/adagrade/internal/domain"
)

func TestNew_File(t *testing.T) {
	p, err := New(Config{Provider: "file", OutboxDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*FileProvider); !ok {
		t.Errorf("New(file) returned %T, want *FileProvider", p)
	}
}

func TestNew_FileRequiresOutboxDir(t *testing.T) {
	if _, err := New(Config{Provider: "file"}); err == nil {
		t.Fatal("file provider without OutboxDir must error")
	}
}

func TestNew_None(t *testing.T) {
	p, err := New(Config{Provider: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*NoneProvider); !ok {
		t.Errorf("New(none) returned %T, want *NoneProvider", p)
	}
}

func TestNew_SMTPRequiresHostAndFrom(t *testing.T) {
	if _, err := New(Config{Provider: "smtp"}); err == nil {
		t.Fatal("smtp provider without host/from must error")
	}
	if _, err := New(Config{Provider: "smtp", SMTPHost: "smtp.example.edu", SMTPPort: "587"}); err == nil {
		t.Fatal("smtp provider without From must error")
	}
	p, err := New(Config{Provider: "smtp", From: "grades@example.edu", SMTPHost: "smtp.example.edu", SMTPPort: "587"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*SMTPProvider); !ok {
		t.Errorf("New(smtp) returned %T, want *SMTPProvider", p)
	}
}

func TestNew_PostmarkRequiresTokenAndFrom(t *testing.T) {
	if _, err := New(Config{Provider: "postmark"}); err == nil {
		t.Fatal("postmark provider without token/from must error")
	}
	if _, err := New(Config{Provider: "postmark", PostmarkToken: "pm-tok"}); err == nil {
		t.Fatal("postmark provider without From must error")
	}
	p, err := New(Config{Provider: "postmark", From: "grades@example.edu", PostmarkToken: "pm-tok"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*PostmarkProvider); !ok {
		t.Errorf("New(postmark) returned %T, want *PostmarkProvider", p)
	}
}

func TestNew_UnknownProviderErrors(t *testing.T) {
	if _, err := New(Config{Provider: "carrier-pigeon"}); err == nil {
		t.Fatal("unknown provider kind must error")
	}
}

// Guard: New's return type is exactly domain.EmailProvider (what Q3/pipeline code
// depends on).
var _ domain.EmailProvider = (*FileProvider)(nil)
var _ domain.EmailProvider = (*NoneProvider)(nil)
var _ domain.EmailProvider = (*SMTPProvider)(nil)
var _ domain.EmailProvider = (*PostmarkProvider)(nil)

// TestRejectAttachmentFields_RejectsCRLFInMIME is A11: domain.Attachment.MIME
// is interpolated into a raw "Content-Type: %s; name=%q" header line (mime.go
// writeAttachmentPart, used by both the file and smtp builders) and into
// Postmark's ContentType field, exactly the same injection surface as
// Filename — so the shared guard must reject a CRLF-poisoned MIME value the
// same way it already rejects a CRLF-poisoned filename.
func TestRejectAttachmentFields_RejectsCRLFInMIME(t *testing.T) {
	err := rejectAttachmentFields([]domain.Attachment{
		{Filename: "results.pdf", MIME: "application/pdf\r\nX-Injected: evil", Content: []byte("x")},
	})
	if err == nil {
		t.Fatal("rejectAttachmentFields must reject a CRLF-poisoned MIME value")
	}
}

// TestRejectAttachmentFields_RejectsCRLFInFilename pins the pre-existing
// filename guard's behavior under the (possibly renamed) function so the A11
// extension didn't drop it.
func TestRejectAttachmentFields_RejectsCRLFInFilename(t *testing.T) {
	err := rejectAttachmentFields([]domain.Attachment{
		{Filename: "results.pdf\r\nX-Injected: evil", MIME: "application/pdf", Content: []byte("x")},
	})
	if err == nil {
		t.Fatal("rejectAttachmentFields must reject a CRLF-poisoned filename")
	}
}

// TestRejectAttachmentFields_AcceptsCleanValues is the negative case: ordinary
// filenames and MIME types must not be rejected.
func TestRejectAttachmentFields_AcceptsCleanValues(t *testing.T) {
	err := rejectAttachmentFields([]domain.Attachment{
		{Filename: "results.pdf", MIME: "application/pdf", Content: []byte("x")},
	})
	if err != nil {
		t.Fatalf("clean attachment fields must not be rejected: %v", err)
	}
}
