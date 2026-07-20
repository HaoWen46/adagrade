package email

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/HaoWen46/adagrade/internal/domain"
)

// FileProvider writes each outbound message as an RFC-5322 .eml file under a
// directory (dev/test mode, spec §3). It never transmits anything — just gives a
// developer something to open in a mail client to check the rendering.
type FileProvider struct {
	dir string
	// devInbound, set only from Config.DevInbound (development env), lets the
	// provider parse Postmark-shaped inbound JSON so local setups can simulate
	// student regrade replies through the real webhook. In production the file
	// provider still refuses inbound outright — nothing should route mail to it.
	devInbound bool
}

// NewFileProvider constructs a FileProvider writing under dir, creating it if
// absent. replyDomain is currently unused here (ReplyTo already arrives fully
// formed on OutboundEmail) but accepted for symmetry with the other
// constructors and to keep New's call sites uniform.
func NewFileProvider(dir string, replyDomain string) (*FileProvider, error) {
	if dir == "" {
		return nil, fmt.Errorf("email: FileProvider requires a non-empty directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("email: create outbox dir: %w", err)
	}
	return &FileProvider{dir: dir}, nil
}

// Send writes msg as a multipart/alternative .eml file and logs only the path
// and a generated id — never the subject or body (PII rule).
func (p *FileProvider) Send(ctx context.Context, msg domain.OutboundEmail) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", definitelyNotAccepted(fmt.Errorf("email: file send cancelled before write: %w", err))
	}

	id, err := messageCorrelationID(msg.DeliveryKey)
	if err != nil {
		return "", definitelyNotAccepted(fmt.Errorf("email: generate message id: %w", err))
	}

	raw, err := buildEML(msg, id)
	if err != nil {
		return "", definitelyNotAccepted(err)
	}

	filename := fmt.Sprintf("%s-%s.eml", time.Now().UTC().Format("20060102T150405.000000000"), id)
	if msg.DeliveryKey != "" {
		// Stable-key deliveries deliberately omit a timestamp: every replay maps
		// to one opaque filename, so the completed first write can be reused.
		filename = id + ".eml"
	}
	path := filepath.Join(p.dir, filename)
	created, err := writeOutboxFileOnce(path, raw)
	if err != nil {
		return "", definitelyNotAccepted(fmt.Errorf("email: write outbox file: %w", err))
	}

	if created {
		slog.Info("email: wrote outbox file", "path", path, "provider_id", id)
	} else {
		slog.Info("email: reused outbox file", "path", path, "provider_id", id)
	}
	return id, nil
}

// writeOutboxFileOnce publishes a complete file without ever replacing an
// existing path. A same-directory temp file is fully written and synced first;
// hard-link creation then atomically wins or observes EEXIST. The latter is the
// idempotent replay path for a stable DeliveryKey. Unlike os.Rename on Unix,
// os.Link cannot silently overwrite the first completed delivery.
func writeOutboxFileOnce(path string, raw []byte) (created bool, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".adamarker-email-*.tmp")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}

	if err := os.Link(tmpPath, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return false, err
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return false, statErr
		}
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("existing outbox path is not a regular file")
		}
		return false, nil
	}
	return true, nil
}

// ErrFileProviderNoInbound is returned by ParseInbound — the file provider has no
// real webhook; nothing should ever route inbound mail to it. The one exception
// is DevInbound (development only), which parses simulated Postmark payloads so
// the regrade pipeline can be exercised locally.
var ErrFileProviderNoInbound = errors.New("email: file provider does not implement inbound parsing")

func (p *FileProvider) ParseInbound(raw []byte) (domain.InboundEmail, error) {
	if p.devInbound {
		return parsePostmarkInbound(raw)
	}
	return domain.InboundEmail{}, ErrFileProviderNoInbound
}

// identity is the no-op body filter buildEML passes to writeAlternativeBody:
// unlike SMTP's DATA command, a plain .eml file on disk has no "line
// starting with a bare dot terminates the message" convention, so no
// dot-stuffing is needed (mirrors the pre-attachment code, which never
// dot-stuffed here either).
func identity(s string) string { return s }

// buildEML renders msg as a minimal RFC-5322 message, given a message id
// (used for both the Message-Id header and the MIME boundary — both need
// only be locally unique, not cryptographically unpredictable). With no
// attachments the body is multipart/alternative (text + html) — unchanged
// from before D42. With Attachments set (report-attachments spec §3), the
// top level becomes multipart/mixed nesting that same alternative body plus
// one part per attachment, mirroring buildRFC5322's shape exactly so a
// developer's .eml preview matches what SMTP/Postmark actually send.
func buildEML(msg domain.OutboundEmail, id string) ([]byte, error) {
	if err := rejectHeaderFields(map[string]string{
		"To":       msg.To,
		"Reply-To": msg.ReplyTo,
		"Subject":  msg.Subject,
	}); err != nil {
		return nil, err
	}
	if err := rejectAttachmentFields(msg.Attachments); err != nil {
		return nil, err
	}

	altBoundary := "adamarker-" + id
	var b bytes.Buffer

	fmt.Fprintf(&b, "Message-Id: %s\r\n", rfcMessageID(id))
	if msg.DeliveryKey != "" {
		fmt.Fprintf(&b, "%s: %s\r\n", deliveryCorrelationHeader, id)
	}
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "To: %s\r\n", msg.To)
	if msg.ReplyTo != "" {
		fmt.Fprintf(&b, "Reply-To: %s\r\n", msg.ReplyTo)
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Subject)
	b.WriteString("MIME-Version: 1.0\r\n")

	if len(msg.Attachments) == 0 {
		fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n", altBoundary)
		b.WriteString("\r\n")
		writeAlternativeBody(&b, altBoundary, msg, identity)
		return b.Bytes(), nil
	}

	mixedBoundary := "adamarker-mixed-" + id
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n", mixedBoundary)
	b.WriteString("\r\n")

	fmt.Fprintf(&b, "--%s\r\n", mixedBoundary)
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n", altBoundary)
	b.WriteString("\r\n")
	writeAlternativeBody(&b, altBoundary, msg, identity)

	for _, a := range msg.Attachments {
		writeAttachmentPart(&b, mixedBoundary, a)
	}
	fmt.Fprintf(&b, "--%s--\r\n", mixedBoundary)
	return b.Bytes(), nil
}

func randomID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
