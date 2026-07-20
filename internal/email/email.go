// Package email implements domain.EmailProvider three ways — file (dev/test),
// smtp (any mailbox), postmark (the plan's production provider) — plus a none
// provider that records everything but sends nothing, selected by
// ADAMARKER_EMAIL_PROVIDER (spec §3, D31). It also carries the signed regrade
// token (spec §4, D32) and the text/html templates for grade and regrade emails
// (spec §3, §5–§6).
//
// This package is pure: it takes a Config and hands back a domain.EmailProvider.
// It never touches the database or the queue — wiring those seams is Q2/Q3's job.
//
// PII rule (CLAUDE.md): message bodies (student names, comments, regrade notes)
// must never appear in logs or error strings anywhere in this package. Errors
// returned here carry only structural detail (which field, which host) — never
// body content or the raw student email address beyond what a caller already has.
package email

import (
	"fmt"
	"strings"

	"github.com/HaoWen46/adagrade/internal/domain"
)

// Config selects and configures one EmailProvider implementation (spec §3).
type Config struct {
	Provider string // "file" | "smtp" | "postmark" | "none"

	From        string // required for smtp/postmark
	ReplyDomain string // inbound domain for regrade+<token>@<ReplyDomain>; empty ⇒ replies not monitored

	// file provider: directory .eml files are written under (the caller resolves
	// this — spec says "<blobdir>/../outbox/", this package just takes the path).
	OutboxDir string

	// smtp provider.
	SMTPHost string
	SMTPPort string // "587" (STARTTLS) or "465" (implicit TLS); default "587"
	SMTPUser string
	SMTPPass string

	// postmark provider.
	PostmarkToken string
	// PostmarkBaseURL overrides the real Postmark API base URL — tests point this
	// at an httptest.Server. Empty ⇒ the real endpoint.
	PostmarkBaseURL string

	Rate float64 // sends/sec; 0 ⇒ caller (Q3's queue wiring) applies its own default

	// DevInbound (development env only, set by config.Load) lets the file
	// provider parse simulated Postmark inbound payloads so the regrade
	// pipeline can be exercised locally. Ignored by every other provider.
	DevInbound bool
}

// New constructs the EmailProvider selected by cfg.Provider, validating that the
// fields it needs are present. An unknown Provider value is an error, not a
// silent fallback to none — a typo in ADAMARKER_EMAIL_PROVIDER must fail loudly
// at boot rather than quietly stop sending grades.
func New(cfg Config) (domain.EmailProvider, error) {
	switch cfg.Provider {
	case "file":
		if cfg.OutboxDir == "" {
			return nil, fmt.Errorf("email: file provider requires OutboxDir")
		}
		p, err := NewFileProvider(cfg.OutboxDir, cfg.ReplyDomain)
		if err != nil {
			return nil, err
		}
		p.devInbound = cfg.DevInbound
		return p, nil
	case "smtp":
		if cfg.SMTPHost == "" {
			return nil, fmt.Errorf("email: smtp provider requires SMTPHost")
		}
		if cfg.From == "" {
			return nil, fmt.Errorf("email: smtp provider requires From")
		}
		return NewSMTPProvider(cfg)
	case "postmark":
		if cfg.PostmarkToken == "" {
			return nil, fmt.Errorf("email: postmark provider requires PostmarkToken")
		}
		if cfg.From == "" {
			return nil, fmt.Errorf("email: postmark provider requires From")
		}
		return NewPostmarkProvider(cfg), nil
	case "none":
		return NewNoneProvider(), nil
	default:
		return nil, fmt.Errorf("email: unknown provider %q (want file|smtp|postmark|none)", cfg.Provider)
	}
}

// headerFieldOrder is the canonical check order for rejectHeaderFields' four
// original top-level header fields — kept as an explicit slice (rather than
// ranging over the map) so error messages are deterministic regardless of Go's
// randomized map iteration order.
var headerFieldOrder = []string{"From", "To", "Reply-To", "Subject"}

// rejectHeaderFields checks every (name, value) pair for an embedded CR or LF
// and errors on the first one found, naming the offending header. It is the
// shared guard buildRFC5322 (smtp.go) and buildEML (file.go) call before
// writing To/ReplyTo/Subject/From into a raw RFC-5322 header block.
//
// A \r or \n in a header-bound field lets an attacker splice extra headers
// (e.g. "Bcc: attacker@evil.example") into the message, or terminate the
// header block early and inject arbitrary body content — classic SMTP/MIME
// header injection. Values here can originate from data an attacker
// influences (e.g. Subject is built from AssessmentName, a course-controlled
// string), so this rejects rather than strips: silently stripping would let a
// poisoned value slip through looking "fine", whereas rejecting fails loudly
// at the point of construction.
func rejectHeaderFields(fields map[string]string) error {
	for _, name := range headerFieldOrder {
		v, ok := fields[name]
		if !ok {
			continue
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("email: %s contains a CR or LF character; refusing to build message (possible header injection)", name)
		}
	}
	return nil
}

// rejectAttachmentFields extends the CRLF header-injection guard to attachment
// filenames AND MIME types (spec §3's "CRLF guard extends to filenames",
// broadened by A11 to every domain.Attachment field that lands in a
// header-shaped value): Filename is written into a Content-Disposition header
// value by mime.go's writeAttachmentPart (shared by the file and smtp
// builders), and MIME is written into a raw "Content-Type: %s; name=%q" header
// line by that same function, and into Postmark's ContentType field. Both are
// the same injection surface as Subject/To/Reply-To — a poisoned value in
// either must be rejected the same way rather than silently passed through
// (A11: MIME was missing this guard even though Filename already had it).
func rejectAttachmentFields(attachments []domain.Attachment) error {
	for _, a := range attachments {
		if strings.ContainsAny(a.Filename, "\r\n") {
			return fmt.Errorf("email: attachment filename contains a CR or LF character; refusing to build message (possible header injection)")
		}
		if strings.ContainsAny(a.MIME, "\r\n") {
			return fmt.Errorf("email: attachment MIME type contains a CR or LF character; refusing to build message (possible header injection)")
		}
	}
	return nil
}
