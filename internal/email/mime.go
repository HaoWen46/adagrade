package email

import (
	"bytes"
	"encoding/base64"
	"fmt"

	"github.com/HaoWen46/adagrade/internal/domain"
)

// b64LineLen is the RFC 2045 §6.8 line-wrap width for base64-encoded MIME
// body content (76 characters per encoded line).
const b64LineLen = 76

// writeAttachmentPart writes one MIME part for an attachment: a
// Content-Disposition: attachment header carrying the filename, the given
// MIME type, base64 transfer encoding, and the base64-wrapped content —
// terminated by a blank line and the boundary marker like every other part
// in this package's hand-rolled MIME builders (buildRFC5322, buildEML).
func writeAttachmentPart(b *bytes.Buffer, boundary string, a domain.Attachment) {
	fmt.Fprintf(b, "--%s\r\n", boundary)
	fmt.Fprintf(b, "Content-Type: %s; name=%q\r\n", a.MIME, a.Filename)
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	fmt.Fprintf(b, "Content-Disposition: attachment; filename=%q\r\n", a.Filename)
	b.WriteString("\r\n")
	b.WriteString(wrapBase64(a.Content))
	b.WriteString("\r\n")
}

// wrapBase64 base64-encodes data and hard-wraps it at b64LineLen characters
// per line with CRLF line endings, per RFC 2045 §6.8 — required so MTAs that
// enforce a max line length on 7-bit transport don't choke on an
// attachment's encoded body.
func wrapBase64(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	var b bytes.Buffer
	for i := 0; i < len(encoded); i += b64LineLen {
		end := i + b64LineLen
		if end > len(encoded) {
			end = len(encoded)
		}
		b.WriteString(encoded[i:end])
		b.WriteString("\r\n")
	}
	return b.String()
}
