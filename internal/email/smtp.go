package email

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"

	"github.com/HaoWen46/adagrade/internal/domain"
)

// defaultSMTPPort is used when Config.SMTPPort is unset — STARTTLS on 587,
// matching the vast majority of "any university or personal mailbox" setups
// spec §3 targets.
const defaultSMTPPort = "587"

// implicitTLSPort is the well-known SMTPS port; a provider configured with it
// dials straight into TLS instead of negotiating STARTTLS after a plaintext
// EHLO (spec §3: "implicit TLS (465)").
const implicitTLSPort = "465"

// SMTPProvider sends mail via any SMTP server reachable with STARTTLS (587) or
// implicit TLS (465), stdlib net/smtp + crypto/tls only (spec §3).
type SMTPProvider struct {
	from string
	host string
	port string
	user string
	pass string

	// testTLSConfig, when set (tests only), is used instead of a bare
	// &tls.Config{ServerName: host} — lets tests point at an in-process fake
	// server presenting a self-signed cert without touching real trust roots.
	testTLSConfig *tls.Config

	// testDialAddr, when set (tests only), overrides the host:port actually
	// dialed while leaving implicitTLS()'s decision keyed on the configured
	// port (465/587/etc). Tests bind their in-process fake server to an
	// ephemeral OS port (":0") — real port 465/587 can't be bound without
	// root/conflicting with a real mail server — so this lets a test assert
	// "given SMTPPort=465, the provider selects implicit TLS" while still
	// dialing the ephemeral port the fake server actually listens on.
	testDialAddr string
}

// NewSMTPProvider constructs an SMTPProvider. Config.SMTPHost is required;
// Config.From is required (validated by New, but also checked here so direct
// callers get the same guarantee).
func NewSMTPProvider(cfg Config) (*SMTPProvider, error) {
	if cfg.SMTPHost == "" {
		return nil, fmt.Errorf("email: smtp provider requires SMTPHost")
	}
	if cfg.From == "" {
		return nil, fmt.Errorf("email: smtp provider requires From")
	}
	port := cfg.SMTPPort
	if port == "" {
		port = defaultSMTPPort
	}
	return &SMTPProvider{
		from: cfg.From,
		host: cfg.SMTPHost,
		port: port,
		user: cfg.SMTPUser,
		pass: cfg.SMTPPass,
	}, nil
}

func (p *SMTPProvider) implicitTLS() bool {
	return p.port == implicitTLSPort
}

func (p *SMTPProvider) tlsConfig() *tls.Config {
	if p.testTLSConfig != nil {
		return p.testTLSConfig
	}
	return &tls.Config{ServerName: p.host}
}

// Send dials the configured host:port, negotiates TLS (implicit or STARTTLS per
// the port), authenticates if credentials are set, and transmits msg as a
// multipart/alternative message. The error path never includes msg's subject or
// body — only host/port/recipient-count-level detail (PII rule); the recipient
// address itself is operational routing data the caller already possesses, not
// logged content, so it's safe in a returned error but not in a log line.
func (p *SMTPProvider) Send(ctx context.Context, msg domain.OutboundEmail) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", definitelyNotAccepted(fmt.Errorf("email: smtp send cancelled before dial: %w", err))
	}
	id, err := messageCorrelationID(msg.DeliveryKey)
	if err != nil {
		return "", definitelyNotAccepted(fmt.Errorf("email: generate message id: %w", err))
	}
	raw, err := buildRFC5322(p.from, msg, id)
	if err != nil {
		return "", definitelyNotAccepted(err)
	}

	addr := net.JoinHostPort(p.host, p.port)
	dialAddr := addr
	if p.testDialAddr != "" {
		dialAddr = p.testDialAddr
	}

	var conn net.Conn
	dialer := &net.Dialer{}
	if p.implicitTLS() {
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: p.tlsConfig()}
		conn, err = tlsDialer.DialContext(ctx, "tcp", dialAddr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", dialAddr)
	}
	if err != nil {
		return "", definitelyNotAccepted(fmt.Errorf("email: dial %s: %w", addr, err))
	}
	defer conn.Close()
	// net/smtp has no context-aware command methods. Bind the established
	// connection to cancellation so a stalled greeting, STARTTLS handshake,
	// envelope command, DATA response, or QUIT cannot outlive the River job.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stopContextClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopContextClose()

	client, err := smtp.NewClient(conn, p.host)
	if err != nil {
		return "", definitelyNotAccepted(safeSMTPError("handshake", err))
	}
	defer client.Close()

	if !p.implicitTLS() {
		// TLS is mandatory on the non-implicit (587) path: if the server does
		// not advertise STARTTLS, or the STARTTLS command itself fails, Send
		// must abort here — never fall back to AUTH/DATA in plaintext. A
		// downgrade attacker who strips the STARTTLS advertisement (or
		// intercepts the STARTTLS command) must not be able to make this
		// client transmit credentials or message content unencrypted.
		//
		// Extension parameters are provider-controlled and intentionally ignored;
		// returning/logging them could persist arbitrary server text.
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return "", definitelyNotAccepted(fmt.Errorf("email: server %s does not advertise STARTTLS; refusing to send in plaintext", addr))
		}
		if err := client.StartTLS(p.tlsConfig()); err != nil {
			return "", definitelyNotAccepted(safeSMTPError("STARTTLS", err))
		}
	}

	if p.user != "" {
		auth := smtp.PlainAuth("", p.user, p.pass, p.host)
		if err := client.Auth(auth); err != nil {
			return "", definitelyNotAccepted(safeSMTPError("AUTH", err))
		}
	}

	if err := client.Mail(bareAddr(p.from)); err != nil {
		return "", definitelyNotAccepted(safeSMTPError("MAIL FROM", err))
	}
	if err := client.Rcpt(bareAddr(msg.To)); err != nil {
		return "", definitelyNotAccepted(safeSMTPError("RCPT TO", err))
	}

	w, err := client.Data()
	if err != nil {
		return "", definitelyNotAccepted(safeSMTPError("DATA", err))
	}
	if _, err := w.Write(raw); err != nil {
		w.Close()
		return "", outcomeUnknown(safeSMTPError("message write", err))
	}
	if err := w.Close(); err != nil {
		return "", outcomeUnknown(safeSMTPError("finish DATA", err))
	}

	if err := client.Quit(); err != nil {
		// Message was already accepted by DATA's closing dot; a QUIT failure
		// afterward doesn't undo the send, so it's a non-fatal cleanup issue —
		// still surfaced but not returned as the Send error, matching most SMTP
		// client wrappers' behavior.
		slog.Warn("email: smtp QUIT after successful send", "detail", safeSMTPError("QUIT", err).Error())
	}

	return id, nil
}

// safeSMTPError strips provider-controlled reply text, which can echo recipient
// addresses or subject content and is unsafe to persist/log. textproto exposes a
// numeric SMTP status separately; retain that structural diagnostic when present.
func safeSMTPError(stage string, err error) error {
	var reply *textproto.Error
	if errors.As(err, &reply) && reply.Code >= 100 && reply.Code <= 999 {
		return fmt.Errorf("email: SMTP %s failed: code=%d", stage, reply.Code)
	}
	return fmt.Errorf("email: SMTP %s failed", stage)
}

// ErrSMTPNoInbound is returned by ParseInbound — plain SMTP has no webhook.
var ErrSMTPNoInbound = errors.New("email: smtp provider does not implement inbound parsing")

func (p *SMTPProvider) ParseInbound(raw []byte) (domain.InboundEmail, error) {
	return domain.InboundEmail{}, ErrSMTPNoInbound
}

// bareAddr extracts the bare "user@host" from a possibly "Name <user@host>"
// address for the SMTP envelope commands, which want the bare form.
func bareAddr(addr string) string {
	if a, err := mail.ParseAddress(addr); err == nil {
		return a.Address
	}
	return addr
}

// buildRFC5322 renders a full message (headers + body) ready to hand to
// net/smtp's DATA writer. With no attachments the body is a bare
// multipart/alternative (text+html) — unchanged shape from before D42.
// With one or more Attachments (report-attachments spec §3), the top-level
// Content-Type becomes multipart/mixed, nesting the multipart/alternative
// body as its first part and each attachment as a subsequent part
// (Content-Disposition: attachment, base64-encoded) — the standard "email
// with attachments" MIME shape.
func buildRFC5322(from string, msg domain.OutboundEmail, id string) ([]byte, error) {
	if err := rejectHeaderFields(map[string]string{
		"From":     from,
		"To":       msg.To,
		"Reply-To": msg.ReplyTo,
		"Subject":  msg.Subject,
	}); err != nil {
		return nil, err
	}
	if err := rejectAttachmentFields(msg.Attachments); err != nil {
		return nil, err
	}

	var b bytes.Buffer
	altBoundary := "adamarker-" + id

	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", msg.To)
	if msg.ReplyTo != "" {
		fmt.Fprintf(&b, "Reply-To: %s\r\n", msg.ReplyTo)
	}
	fmt.Fprintf(&b, "Message-Id: %s\r\n", rfcMessageID(id))
	if msg.DeliveryKey != "" {
		fmt.Fprintf(&b, "%s: %s\r\n", deliveryCorrelationHeader, id)
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Subject)
	b.WriteString("MIME-Version: 1.0\r\n")

	if len(msg.Attachments) == 0 {
		fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n", altBoundary)
		b.WriteString("\r\n")
		writeAlternativeBody(&b, altBoundary, msg, dotStuff)
		return b.Bytes(), nil
	}

	mixedBoundary := "adamarker-mixed-" + id
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n", mixedBoundary)
	b.WriteString("\r\n")

	fmt.Fprintf(&b, "--%s\r\n", mixedBoundary)
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n", altBoundary)
	b.WriteString("\r\n")
	writeAlternativeBody(&b, altBoundary, msg, dotStuff)

	for _, a := range msg.Attachments {
		writeAttachmentPart(&b, mixedBoundary, a)
	}
	fmt.Fprintf(&b, "--%s--\r\n", mixedBoundary)
	return b.Bytes(), nil
}

// writeAlternativeBody writes the text+html multipart/alternative part set
// under boundary, applying bodyFilter (dot-stuffing for SMTP, identity for
// the file provider's .eml output) to each body before writing it.
func writeAlternativeBody(b *bytes.Buffer, boundary string, msg domain.OutboundEmail, bodyFilter func(string) string) {
	fmt.Fprintf(b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(bodyFilter(msg.TextBody))
	b.WriteString("\r\n")

	if msg.HTMLBody != "" {
		fmt.Fprintf(b, "--%s\r\n", boundary)
		b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		b.WriteString(bodyFilter(msg.HTMLBody))
		b.WriteString("\r\n")
	}

	fmt.Fprintf(b, "--%s--\r\n", boundary)
}

// dotStuff escapes a leading "." on any line per RFC 5321 §4.5.2 so the body
// can't be misread as the DATA terminator; net/smtp's Data() writer does NOT do
// this for the caller.
func dotStuff(s string) string {
	if !strings.Contains(s, "\n.") && !strings.HasPrefix(s, ".") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, ".") {
			lines[i] = "." + line
		}
	}
	return strings.Join(lines, "\n")
}
