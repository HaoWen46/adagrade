package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/HaoWen46/adagrade/internal/domain"
)

// postmarkDefaultBaseURL is Postmark's real API host; tests override it via
// Config.PostmarkBaseURL (an httptest.Server).
const postmarkDefaultBaseURL = "https://api.postmarkapp.com"

// PostmarkProvider sends via the Postmark HTTP API and decodes Postmark's
// inbound-webhook JSON (spec §3). stdlib net/http only, per the seam rule.
type PostmarkProvider struct {
	from    string
	token   string
	baseURL string
	client  *http.Client
}

// NewPostmarkProvider constructs a PostmarkProvider. Config.PostmarkBaseURL,
// when set, overrides the real endpoint (tests only); empty selects the real one.
func NewPostmarkProvider(cfg Config) *PostmarkProvider {
	base := cfg.PostmarkBaseURL
	if base == "" {
		base = postmarkDefaultBaseURL
	}
	return &PostmarkProvider{
		from:    cfg.From,
		token:   cfg.PostmarkToken,
		baseURL: base,
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

// postmarkSendRequest is the wire shape for POST /email (fields Postmark
// documents; only what this package needs).
type postmarkSendRequest struct {
	From     string `json:"From"`
	To       string `json:"To"`
	ReplyTo  string `json:"ReplyTo,omitempty"`
	Subject  string `json:"Subject"`
	TextBody string `json:"TextBody"`
	HtmlBody string `json:"HtmlBody,omitempty"`
	// MessageStream targets the outbound stream explicitly — Postmark accounts
	// commonly split "outbound" (transactional) from "broadcast"; grade emails
	// are transactional.
	MessageStream string `json:"MessageStream"`
	// Attachments maps domain.OutboundEmail.Attachments (report-attachments
	// spec §3, D42). omitempty keeps the wire payload byte-identical to
	// before D42 for the common no-attachment case.
	Attachments []postmarkAttachment `json:"Attachments,omitempty"`
	// Metadata and Headers carry only the one-way delivery correlation, never
	// the caller's raw DeliveryKey. Postmark does not offer an idempotency key,
	// but these stable fields make duplicate attempts recognizable downstream.
	Metadata map[string]string `json:"Metadata,omitempty"`
	Headers  []postmarkHeader  `json:"Headers,omitempty"`
}

// postmarkAttachment is Postmark's documented attachment shape: Name is the
// filename, Content is base64-encoded bytes, ContentType is the MIME type.
type postmarkAttachment struct {
	Name        string `json:"Name"`
	Content     string `json:"Content"`
	ContentType string `json:"ContentType"`
}

type postmarkSendResponse struct {
	ErrorCode int    `json:"ErrorCode"`
	Message   string `json:"Message"`
	MessageID string `json:"MessageID"`
}

// Send POSTs msg to Postmark's /email endpoint, authenticating with the server
// token header. On a non-2xx or ErrorCode!=0 response the error carries
// Postmark's status/error code but never msg's subject or body (PII rule).
func (p *PostmarkProvider) Send(ctx context.Context, msg domain.OutboundEmail) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", definitelyNotAccepted(fmt.Errorf("email: postmark send cancelled before request: %w", err))
	}
	if err := rejectAttachmentFields(msg.Attachments); err != nil {
		return "", definitelyNotAccepted(err)
	}
	reqBody := postmarkSendRequest{
		From:          p.from,
		To:            msg.To,
		ReplyTo:       msg.ReplyTo,
		Subject:       msg.Subject,
		TextBody:      msg.TextBody,
		HtmlBody:      msg.HTMLBody,
		MessageStream: "outbound",
		Attachments:   postmarkAttachments(msg.Attachments),
	}
	if msg.DeliveryKey != "" {
		correlationID, err := messageCorrelationID(msg.DeliveryKey)
		if err != nil {
			return "", definitelyNotAccepted(fmt.Errorf("email: generate delivery correlation: %w", err))
		}
		reqBody.Metadata = map[string]string{"ada_marker_delivery_key": correlationID}
		reqBody.Headers = []postmarkHeader{
			{Name: "Message-ID", Value: rfcMessageID(correlationID)},
			{Name: deliveryCorrelationHeader, Value: correlationID},
		}
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", definitelyNotAccepted(fmt.Errorf("email: encode postmark request: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/email", bytes.NewReader(payload))
	if err != nil {
		return "", definitelyNotAccepted(fmt.Errorf("email: build postmark request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Postmark-Server-Token", p.token)

	resp, err := p.client.Do(req)
	if err != nil {
		// net/http errors do not reliably reveal whether the request reached the
		// provider. A timeout or lost response can follow successful acceptance.
		return "", outcomeUnknown(fmt.Errorf("email: postmark request failed: %w", err))
	}
	defer resp.Body.Close()

	acceptedStatus := resp.StatusCode >= 200 && resp.StatusCode < 300
	// rejectedStatus captures the HTTP-level rejection signal on its own (A7): a
	// 4xx status proves the API refused the request regardless of what shape the
	// body is in. A WAF, reverse proxy, or rate limiter sitting in front of
	// Postmark can return a 4xx whose body never matches Postmark's own
	// {ErrorCode,...} JSON shape (e.g. a bare {"error":"rate limited"} on a 429,
	// or an HTML error page on a 403) — that must still classify as
	// definitely-not-accepted, not fall through to outcome-unknown just because
	// the body doesn't parse or lacks an ErrorCode field.
	rejectedStatus := resp.StatusCode >= 400 && resp.StatusCode < 500
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// Without a complete provider response there is no deterministic
		// rejection to rely on; an intermediary may have failed after acceptance.
		return "", outcomeUnknown(fmt.Errorf("email: read postmark response: %w", err))
	}

	var parsed postmarkSendResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		failure := fmt.Errorf("email: postmark response status %d not decodable JSON", resp.StatusCode)
		if rejectedStatus {
			return "", definitelyNotAccepted(failure)
		}
		// 5xx and undecodable 2xx bodies remain genuinely ambiguous — the message
		// might have been accepted before an intermediary mangled the response.
		return "", outcomeUnknown(failure)
	}
	if acceptedStatus && parsed.ErrorCode == 0 {
		if parsed.MessageID == "" {
			// The provider may have accepted the message, but without its durable
			// identifier the caller cannot safely record a confirmed outcome.
			return "", outcomeUnknown(fmt.Errorf("email: postmark success response missing MessageID"))
		}
		return parsed.MessageID, nil
	}
	// Postmark's Message is provider-controlled and can echo recipient/subject
	// data. Keep only structural status/code diagnostics in returned errors because
	// callers persist these on publish_items and River job errors.
	failure := fmt.Errorf("email: postmark send failed: status=%d error_code=%d", resp.StatusCode, parsed.ErrorCode)
	// Either rejection signal alone proves the message was refused (A7):
	//   - a 4xx HTTP status, regardless of body shape (rejectedStatus above), or
	//   - a decoded Postmark body with ErrorCode != 0 on a non-5xx response —
	//     Postmark documents API-level rejections (e.g. ErrorCode 406,
	//     inactive/bounced recipient) that arrive on an HTTP 200.
	// 5xx responses and proxy/intermediary failures remain ambiguous even if
	// they happen to carry an ErrorCode-shaped JSON body.
	if rejectedStatus || (resp.StatusCode < 500 && parsed.ErrorCode != 0) {
		return "", definitelyNotAccepted(failure)
	}
	return "", outcomeUnknown(failure)
}

// postmarkAttachments maps domain.Attachment slices to Postmark's documented
// wire shape (Name/Content base64/ContentType). A nil/empty input returns
// nil so the request's omitempty keeps the no-attachment payload unchanged.
func postmarkAttachments(attachments []domain.Attachment) []postmarkAttachment {
	if len(attachments) == 0 {
		return nil
	}
	out := make([]postmarkAttachment, len(attachments))
	for i, a := range attachments {
		out[i] = postmarkAttachment{
			Name:        a.Filename,
			Content:     base64.StdEncoding.EncodeToString(a.Content),
			ContentType: a.MIME,
		}
	}
	return out
}

// postmarkInboundPayload models the fields ADA-Marker needs from Postmark's
// inbound-webhook JSON (the full payload has many more fields — attachments,
// full header dump, etc. — that this package doesn't consume). SPF/DKIM
// verdicts do not arrive as booleans; Postmark relays them as the raw
// Received-SPF and Authentication-Results headers, which this type's Headers
// field captures for parseVerdicts to interpret (A7 in PLAN_GAPS: the contract
// this package chooses is "parse the standard header text", matching what
// Postmark actually sends as of 2026).
type postmarkInboundPayload struct {
	From string `json:"From"`
	// FromFull is Postmark's structured sender: {Email, Name, MailboxHash}. The top-level
	// From is often display-name-formatted ("Ada Fake <ada@example.edu>"), which the
	// verification ladder's exact-email comparison (rung 3) would reject; FromFull.Email
	// is the bare address, so it's preferred when present (M5).
	FromFull    postmarkAddress `json:"FromFull"`
	MailboxHash string          `json:"MailboxHash"`
	Subject     string          `json:"Subject"`
	// MessageID is Postmark's unique id for this delivery attempt — used
	// upstream as the webhook idempotency key (F1: retries on timeout/non-2xx
	// re-send the same MessageID).
	MessageID string `json:"MessageID"`
	Date      string `json:"Date"`
	// TextBody is the full plain-text body (quoted original included).
	// StrippedTextReply is Postmark's best-effort reply-only extraction (the
	// quoted original stripped out) — preferred when non-empty, since it's
	// closer to what the student actually wrote. Both are student PII, never
	// logged.
	TextBody          string           `json:"TextBody"`
	StrippedTextReply string           `json:"StrippedTextReply"`
	Headers           []postmarkHeader `json:"Headers"`
}

type postmarkHeader struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// postmarkAddress is Postmark's structured address object (FromFull/ToFull/etc.). Only
// Email is consumed here — the bare address, free of any display-name wrapper.
type postmarkAddress struct {
	Email       string `json:"Email"`
	Name        string `json:"Name"`
	MailboxHash string `json:"MailboxHash"`
}

// ParseInbound decodes Postmark's inbound-webhook JSON body into the
// provider-agnostic domain.InboundEmail shape. The token is recovered from
// MailboxHash — the plus-address's tag, which Postmark splits out for exactly
// this purpose (spec §4/§5, PLAN_GAPS A7). SPF/DKIM verdicts are read from the
// standard Received-SPF / Authentication-Results header text; a missing or
// unparseable verdict is treated as failed (fail-closed — the caller's
// verification ladder is warn-not-block per spec §5 rung 4, but this parser
// itself never upgrades an absent verdict to "pass").
func (p *PostmarkProvider) ParseInbound(raw []byte) (domain.InboundEmail, error) {
	return parsePostmarkInbound(raw)
}

// parsePostmarkInbound is the provider-independent core of ParseInbound, shared
// with FileProvider's dev-only inbound simulation (see file.go DevInbound).
func parsePostmarkInbound(raw []byte) (domain.InboundEmail, error) {
	var payload postmarkInboundPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return domain.InboundEmail{}, fmt.Errorf("email: decode postmark inbound payload: %w", err)
	}
	// Prefer FromFull.Email (bare address) over the top-level From, which is often
	// display-name-formatted ("Name <addr>") and would fail the ladder's exact-email
	// comparison (M5). Fall back to From when FromFull is absent.
	from := payload.FromFull.Email
	if from == "" {
		from = payload.From
	}
	if from == "" {
		return domain.InboundEmail{}, fmt.Errorf("email: postmark inbound payload missing From")
	}

	spfPass, dkimValid := parseVerdicts(payload.Headers)

	receivedAt, err := parsePostmarkDate(payload.Date)
	if err != nil {
		// A malformed Date is not fatal to processing an otherwise-legitimate
		// reply — the verification ladder in the httpapi layer doesn't key off
		// ReceivedAt, only the caller's insertion time would. Fall back to the
		// zero time rather than rejecting the whole message.
		receivedAt = time.Time{}
	}

	textBody := payload.StrippedTextReply
	if textBody == "" {
		textBody = payload.TextBody
	}

	return domain.InboundEmail{
		From:        from,
		MailboxHash: payload.MailboxHash,
		Subject:     payload.Subject,
		TextBody:    textBody,
		SPFPass:     spfPass,
		DKIMValid:   dkimValid,
		ReceivedAt:  receivedAt,
		RawJSON:     raw,
		MessageID:   payload.MessageID,
	}, nil
}

// parseVerdicts reads SPF/DKIM pass-fail out of the Received-SPF and
// Authentication-Results header values Postmark relays verbatim. Absent or
// ambiguous values are treated as failed, not passed.
func parseVerdicts(headers []postmarkHeader) (spfPass, dkimValid bool) {
	for _, h := range headers {
		switch strings.ToLower(h.Name) {
		case "received-spf":
			spfPass = strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Value)), "pass")
		case "authentication-results":
			dkimValid = hasResult(h.Value, "dkim", "pass")
			// Authentication-Results often also carries the SPF verdict; prefer
			// the dedicated Received-SPF header when present (checked above),
			// but fall back to this one if Received-SPF was absent.
			if !headerPresent(headers, "received-spf") {
				spfPass = hasResult(h.Value, "spf", "pass")
			}
		}
	}
	return spfPass, dkimValid
}

func headerPresent(headers []postmarkHeader, name string) bool {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return true
		}
	}
	return false
}

// hasResult looks for "<mechanism>=<verdict>" (e.g. "dkim=pass") anywhere in an
// Authentication-Results header value.
func hasResult(value, mechanism, verdict string) bool {
	needle := mechanism + "=" + verdict
	return strings.Contains(strings.ToLower(value), needle)
}

// parsePostmarkDate parses the RFC-1123-ish Date header Postmark relays from
// the original message.
func parsePostmarkDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty date")
	}
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, "Mon, 2 Jan 2006 15:04:05 -0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date format")
}
