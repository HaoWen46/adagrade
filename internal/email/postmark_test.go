package email

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/domain"
)

func TestPostmarkProvider_SendSetsAuthHeaderAndBody(t *testing.T) {
	var (
		gotPath   string
		gotToken  string
		gotAccept string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Postmark-Server-Token")
		gotAccept = r.Header.Get("Accept")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ErrorCode": 0,
			"Message":   "OK",
			"MessageID": "pm-message-id-123",
		})
	}))
	defer srv.Close()

	p := NewPostmarkProvider(Config{
		From:            "grades@example.edu",
		PostmarkToken:   "pm-server-token",
		PostmarkBaseURL: srv.URL,
	})

	msg := domain.OutboundEmail{
		To:       "s0000010@example.edu",
		ReplyTo:  "regrade+v1.5.123.sig@inbound.example.edu",
		Subject:  "Midterm 2 — results",
		TextBody: "Total: 25/30",
		HTMLBody: "<p>Total: 25/30</p>",
	}

	id, err := p.Send(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if id != "pm-message-id-123" {
		t.Errorf("providerID: got %q want pm-message-id-123", id)
	}
	if gotToken != "pm-server-token" {
		t.Errorf("X-Postmark-Server-Token: got %q", gotToken)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept header: got %q", gotAccept)
	}
	if !strings.HasSuffix(gotPath, "/email") {
		t.Errorf("path: got %q, want suffix /email", gotPath)
	}
	if gotBody["To"] != msg.To {
		t.Errorf("body To: got %v", gotBody["To"])
	}
	if gotBody["From"] != "grades@example.edu" {
		t.Errorf("body From: got %v", gotBody["From"])
	}
	if gotBody["ReplyTo"] != msg.ReplyTo {
		t.Errorf("body ReplyTo: got %v", gotBody["ReplyTo"])
	}
	if gotBody["Subject"] != msg.Subject {
		t.Errorf("body Subject: got %v", gotBody["Subject"])
	}
	if gotBody["TextBody"] != msg.TextBody {
		t.Errorf("body TextBody: got %v", gotBody["TextBody"])
	}
	if gotBody["HtmlBody"] != msg.HTMLBody {
		t.Errorf("body HtmlBody: got %v", gotBody["HtmlBody"])
	}
}

func TestPostmarkProvider_DeliveryKeyAddsStableOpaqueCorrelation(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ErrorCode": 0, "Message": "OK", "MessageID": "pm-id"})
	}))
	defer srv.Close()
	p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "token", PostmarkBaseURL: srv.URL})
	msg := domain.OutboundEmail{
		DeliveryKey: "publish/item/102/opaque",
		To:          "s0000023@example.edu",
		Subject:     "Midterm results",
		TextBody:    "result",
	}
	if _, err := p.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	for i, body := range bodies {
		metadata, ok := body["Metadata"].(map[string]any)
		if !ok {
			t.Fatalf("request %d Metadata = %#v", i, body["Metadata"])
		}
		correlation, _ := metadata["ada_marker_delivery_key"].(string)
		if correlation == "" || strings.Contains(correlation, "publish") {
			t.Fatalf("request %d correlation is missing or exposes raw key: %q", i, correlation)
		}
		if i > 0 && correlation != bodies[0]["Metadata"].(map[string]any)["ada_marker_delivery_key"] {
			t.Fatalf("request %d correlation = %q, want stable replay value", i, correlation)
		}
		headers, ok := body["Headers"].([]any)
		if !ok {
			t.Fatalf("request %d Headers = %#v", i, body["Headers"])
		}
		got := map[string]string{}
		for _, raw := range headers {
			h := raw.(map[string]any)
			got[h["Name"].(string)] = h["Value"].(string)
		}
		if got["Message-ID"] != "<"+correlation+"@adamarker.local>" {
			t.Errorf("request %d Message-ID = %q", i, got["Message-ID"])
		}
		if got["X-ADA-Marker-Delivery-Key"] != correlation {
			t.Errorf("request %d correlation header = %q", i, got["X-ADA-Marker-Delivery-Key"])
		}
	}
}

func TestPostmarkProvider_WithoutDeliveryKeyOmitsCorrelationFields(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"ErrorCode": 0, "Message": "OK", "MessageID": "pm-id"})
	}))
	defer srv.Close()
	p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "token", PostmarkBaseURL: srv.URL})
	if _, err := p.Send(context.Background(), domain.OutboundEmail{To: "s@example.edu", Subject: "x", TextBody: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["Metadata"]; ok {
		t.Fatalf("legacy message unexpectedly has Metadata: %#v", body["Metadata"])
	}
	if _, ok := body["Headers"]; ok {
		t.Fatalf("legacy message unexpectedly has Headers: %#v", body["Headers"])
	}
}

// ---- Attachments (report-attachments spec §3): postmark maps
// domain.Attachment to its Attachments JSON array (Name/Content
// base64/ContentType). ----

func TestPostmarkProvider_SendMapsAttachments(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ErrorCode": 0, "Message": "OK", "MessageID": "pm-message-id-456",
		})
	}))
	defer srv.Close()

	p := NewPostmarkProvider(Config{
		From:            "grades@example.edu",
		PostmarkToken:   "pm-server-token",
		PostmarkBaseURL: srv.URL,
	})

	pdfBytes := []byte("%PDF-1.4 fake pdf bytes\n")
	msg := domain.OutboundEmail{
		To:       "s0000012@example.edu",
		Subject:  "Midterm 2 — results",
		TextBody: "Total: 25/30",
		Attachments: []domain.Attachment{
			{Filename: "results.pdf", MIME: "application/pdf", Content: pdfBytes},
		},
	}
	if _, err := p.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	rawAttachments, ok := gotBody["Attachments"]
	if !ok {
		t.Fatalf("request body missing Attachments field: %+v", gotBody)
	}
	attachments, ok := rawAttachments.([]any)
	if !ok || len(attachments) != 1 {
		t.Fatalf("Attachments: got %+v, want a 1-element array", rawAttachments)
	}
	att, ok := attachments[0].(map[string]any)
	if !ok {
		t.Fatalf("attachment entry is not an object: %+v", attachments[0])
	}
	if att["Name"] != "results.pdf" {
		t.Errorf("attachment Name: got %v want results.pdf", att["Name"])
	}
	if att["ContentType"] != "application/pdf" {
		t.Errorf("attachment ContentType: got %v want application/pdf", att["ContentType"])
	}
	wantB64 := base64.StdEncoding.EncodeToString(pdfBytes)
	if att["Content"] != wantB64 {
		t.Errorf("attachment Content: got %v want base64 %v", att["Content"], wantB64)
	}
}

// TestPostmarkProvider_SendNoAttachments_OmitsAttachmentsField asserts the
// common no-attachment case doesn't add an empty Attachments array to the
// JSON payload (Postmark treats an empty array the same as omitted, but
// omitting keeps the wire payload identical to before D42 for the common
// case, matching this package's "unchanged shape when nothing new is used"
// convention already established for multipart/alternative).
func TestPostmarkProvider_SendNoAttachments_OmitsAttachmentsField(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ErrorCode": 0, "Message": "OK", "MessageID": "pm-message-id-789",
		})
	}))
	defer srv.Close()

	p := NewPostmarkProvider(Config{
		From:            "grades@example.edu",
		PostmarkToken:   "pm-server-token",
		PostmarkBaseURL: srv.URL,
	})
	msg := domain.OutboundEmail{To: "s0000013@example.edu", Subject: "x", TextBody: "y"}
	if _, err := p.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if _, ok := gotBody["Attachments"]; ok {
		t.Errorf("Attachments field should be omitted when there are no attachments, got: %+v", gotBody["Attachments"])
	}
}

// TestPostmarkProvider_Send_RejectsCRLFInAttachmentFilename mirrors the
// smtp/file guard: Postmark's Attachments[].Name is JSON-encoded, not a raw
// header value, but the guard is applied uniformly at the domain boundary
// (rejectAttachmentFields) rather than trusting each provider's own
// serialization to be injection-safe.
func TestPostmarkProvider_Send_RejectsCRLFInAttachmentFilename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Postmark endpoint must not be called when the filename is rejected")
	}))
	defer srv.Close()

	p := NewPostmarkProvider(Config{
		From:            "grades@example.edu",
		PostmarkToken:   "pm-server-token",
		PostmarkBaseURL: srv.URL,
	})
	msg := domain.OutboundEmail{
		To:       "s0000019@example.edu",
		Subject:  "x",
		TextBody: "y",
		Attachments: []domain.Attachment{
			{Filename: "results.pdf\r\nX-Injected: evil", MIME: "application/pdf", Content: []byte("x")},
		},
	}
	if _, err := p.Send(context.Background(), msg); err == nil {
		t.Fatal("Send must reject a CRLF-poisoned attachment filename")
	}
}

// TestPostmarkProvider_Send_RejectsCRLFInAttachmentMIME is A11: MIME lands in
// Postmark's ContentType field. Postmark's own JSON encoding would escape a
// literal CRLF inside that string value harmlessly, but the guard is applied
// uniformly at the domain boundary (rejectAttachmentFields) — this package
// does not special-case "this provider's serialization happens to be safe"
// per field, both because Postmark's ContentType is echoed back into MIME
// clients downstream of the API and to keep one guard covering every
// provider path (spec: CRLF guard extends to attachment fields).
func TestPostmarkProvider_Send_RejectsCRLFInAttachmentMIME(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Postmark endpoint must not be called when the MIME type is rejected")
	}))
	defer srv.Close()

	p := NewPostmarkProvider(Config{
		From:            "grades@example.edu",
		PostmarkToken:   "pm-server-token",
		PostmarkBaseURL: srv.URL,
	})
	msg := domain.OutboundEmail{
		To:       "s0000020@example.edu",
		Subject:  "x",
		TextBody: "y",
		Attachments: []domain.Attachment{
			{Filename: "results.pdf", MIME: "application/pdf\r\nX-Injected: evil", Content: []byte("x")},
		},
	}
	if _, err := p.Send(context.Background(), msg); err == nil {
		t.Fatal("Send must reject a CRLF-poisoned attachment MIME type")
	}
}

func TestPostmarkProvider_SendErrorsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ErrorCode": 300,
			"Message":   "Invalid email request",
		})
	}))
	defer srv.Close()

	p := NewPostmarkProvider(Config{
		From:            "grades@example.edu",
		PostmarkToken:   "pm-server-token",
		PostmarkBaseURL: srv.URL,
	})
	_, err := p.Send(context.Background(), domain.OutboundEmail{To: "s0000011@example.edu", Subject: "x", TextBody: "y"})
	if err == nil {
		t.Fatal("non-2xx Postmark response must be an error")
	}
	if !domain.IsEmailDefinitelyNotAccepted(err) {
		t.Fatalf("explicit Postmark rejection outcome = %q, err=%v", domain.EmailDeliveryOutcomeOf(err), err)
	}
}

func TestPostmarkProvider_ErrorDoesNotExposeProviderMessage(t *testing.T) {
	const providerEcho = "student-private@example.edu — Secret Exam Subject"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ErrorCode": 300,
			"Message":   providerEcho,
		})
	}))
	defer srv.Close()
	p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "token", PostmarkBaseURL: srv.URL})
	_, err := p.Send(context.Background(), domain.OutboundEmail{To: "student-private@example.edu", Subject: "Secret Exam Subject", TextBody: "private"})
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), providerEcho) || strings.Contains(err.Error(), "student-private") || strings.Contains(err.Error(), "Secret Exam") {
		t.Fatalf("Postmark error exposes provider-controlled PII: %q", err)
	}
	if !strings.Contains(err.Error(), "status=422") || !strings.Contains(err.Error(), "error_code=300") {
		t.Fatalf("sanitized error lacks structural diagnostics: %q", err)
	}
}

func TestPostmarkProvider_ClassifiesTransportAndAmbiguousSuccessErrorsUnknown(t *testing.T) {
	t.Run("transport error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "token", PostmarkBaseURL: url})
		_, err := p.Send(context.Background(), domain.OutboundEmail{DeliveryKey: "pm-unknown", To: "s@example.edu", Subject: "x", TextBody: "y"})
		if !domain.IsEmailOutcomeUnknown(err) {
			t.Fatalf("transport outcome = %q, err=%v", domain.EmailDeliveryOutcomeOf(err), err)
		}
	})

	t.Run("accepted status with malformed response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "token", PostmarkBaseURL: srv.URL})
		_, err := p.Send(context.Background(), domain.OutboundEmail{DeliveryKey: "pm-ambiguous", To: "s@example.edu", Subject: "x", TextBody: "y"})
		if !domain.IsEmailOutcomeUnknown(err) {
			t.Fatalf("malformed success outcome = %q, err=%v", domain.EmailDeliveryOutcomeOf(err), err)
		}
	})

	t.Run("success response missing provider message id", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ErrorCode": 0, "Message": "OK",
			})
		}))
		defer srv.Close()
		p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "token", PostmarkBaseURL: srv.URL})
		id, err := p.Send(context.Background(), domain.OutboundEmail{DeliveryKey: "pm-missing-id", To: "s@example.edu", Subject: "x", TextBody: "y"})
		if id != "" || !domain.IsEmailOutcomeUnknown(err) {
			t.Fatalf("missing MessageID = id %q outcome %q err=%v, want empty/unknown", id, domain.EmailDeliveryOutcomeOf(err), err)
		}
	})

	t.Run("provider 5xx response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ErrorCode": 500, "Message": "temporarily unavailable",
			})
		}))
		defer srv.Close()
		p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "token", PostmarkBaseURL: srv.URL})
		_, err := p.Send(context.Background(), domain.OutboundEmail{DeliveryKey: "pm-5xx", To: "s@example.edu", Subject: "x", TextBody: "y"})
		if !domain.IsEmailOutcomeUnknown(err) {
			t.Fatalf("5xx outcome = %q, err=%v", domain.EmailDeliveryOutcomeOf(err), err)
		}
	})

	t.Run("proxy error without provider rejection", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("upstream reset"))
		}))
		defer srv.Close()
		p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "token", PostmarkBaseURL: srv.URL})
		_, err := p.Send(context.Background(), domain.OutboundEmail{DeliveryKey: "pm-proxy", To: "s@example.edu", Subject: "x", TextBody: "y"})
		if !domain.IsEmailOutcomeUnknown(err) {
			t.Fatalf("proxy outcome = %q, err=%v", domain.EmailDeliveryOutcomeOf(err), err)
		}
	})
}

// TestPostmarkProvider_ClassifiesRejectionsByStatusAndErrorCode is A7: two
// independent signals each prove the message was refused, and losing either
// one strands a deterministically-failed send in the sender's manual-review
// quarantine instead of retrying automatically.
//
//  1. A 4xx HTTP status is itself proof of rejection — a WAF, reverse proxy,
//     or rate limiter sitting in front of Postmark can return a 4xx whose body
//     doesn't carry Postmark's {ErrorCode,...} shape at all (e.g. a bare
//     {"error":"rate limited"} on a 429, or an HTML error page on a 403).
//  2. A decoded Postmark body with ErrorCode != 0 on any non-5xx response is
//     also proof of rejection even when the HTTP status is 200 — Postmark
//     documents API-level errors (e.g. ErrorCode 406, inactive/bounced
//     recipient) that can arrive on a 200.
//
// 5xx and undecodable 2xx bodies remain genuinely ambiguous — the message
// might have been accepted before an intermediary mangled the response.
func TestPostmarkProvider_ClassifiesRejectionsByStatusAndErrorCode(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		wantDefinite bool
		wantUnknown  bool
	}{
		{name: "429 no ErrorCode field", status: http.StatusTooManyRequests, body: `{"error":"rate limited"}`, wantDefinite: true},
		{name: "403 HTML body", status: http.StatusForbidden, body: `<html><body>Forbidden</body></html>`, wantDefinite: true},
		{name: "422 with ErrorCode", status: http.StatusUnprocessableEntity, body: `{"ErrorCode":300,"Message":"Invalid email request"}`, wantDefinite: true},
		{name: "200 with ErrorCode 406 inactive recipient", status: http.StatusOK, body: `{"ErrorCode":406,"Message":"Inactive recipient"}`, wantDefinite: true},
		{name: "200 malformed JSON", status: http.StatusOK, body: `not json`, wantUnknown: true},
		{name: "503 service unavailable", status: http.StatusServiceUnavailable, body: `{"ErrorCode":500,"Message":"temporarily unavailable"}`, wantUnknown: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "token", PostmarkBaseURL: srv.URL})
			_, err := p.Send(context.Background(), domain.OutboundEmail{To: "s@example.edu", Subject: "x", TextBody: "y"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.wantDefinite && !domain.IsEmailDefinitelyNotAccepted(err) {
				t.Fatalf("outcome = %q, want definitely-not-accepted; err=%v", domain.EmailDeliveryOutcomeOf(err), err)
			}
			if tc.wantUnknown && !domain.IsEmailOutcomeUnknown(err) {
				t.Fatalf("outcome = %q, want outcome-unknown; err=%v", domain.EmailDeliveryOutcomeOf(err), err)
			}
		})
	}
}

func TestPostmarkProvider_DefaultBaseURL(t *testing.T) {
	p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "tok"})
	if p.baseURL != postmarkDefaultBaseURL {
		t.Errorf("default base URL: got %q want %q", p.baseURL, postmarkDefaultBaseURL)
	}
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPostmarkProvider_ParseInbound_SPFDKIMPass(t *testing.T) {
	p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "tok"})
	raw := loadFixture(t, "inbound_postmark.json")

	in, err := p.ParseInbound(raw)
	if err != nil {
		t.Fatal(err)
	}
	if in.From != "s0000001@example.edu" {
		t.Errorf("From: got %q", in.From)
	}
	if in.MailboxHash != "v1.42.1784289600.dGVzdHNpZ25hdHVyZQ" {
		t.Errorf("MailboxHash: got %q", in.MailboxHash)
	}
	if !in.SPFPass {
		t.Error("expected SPFPass=true for the passing fixture")
	}
	if !in.DKIMValid {
		t.Error("expected DKIMValid=true for the passing fixture")
	}
	if in.ReceivedAt.IsZero() {
		t.Error("ReceivedAt must be parsed, not zero")
	}
	if len(in.RawJSON) == 0 {
		t.Error("RawJSON must be preserved for audit/debug")
	}
	if in.Subject != "Re: Midterm 2 — results" {
		t.Errorf("Subject: got %q", in.Subject)
	}
	// F1: idempotency key for webhook retries — Postmark's MessageID uniquely
	// identifies this delivery attempt, so a re-delivered payload can be recognized
	// and deduplicated before it burns rate-cap budget or double-sends mail.
	if in.MessageID != "3c9e1f2a-0000-0000-0000-000000000001" {
		t.Errorf("MessageID: got %q want %q", in.MessageID, "3c9e1f2a-0000-0000-0000-000000000001")
	}
	// The fixture's StrippedTextReply differs from TextBody (no trailing
	// newline) — ParseInbound must prefer StrippedTextReply when present.
	if in.TextBody != "I think problem 2 was undercounted. Can you take another look?" {
		t.Errorf("TextBody: got %q, want StrippedTextReply preferred over TextBody", in.TextBody)
	}
}

func TestPostmarkProvider_ParseInbound_TextBodyFallsBackWhenStrippedTextReplyEmpty(t *testing.T) {
	p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "tok"})
	raw := []byte(`{
		"From": "s0000099@example.edu",
		"MailboxHash": "v1.99.1784289600.ZmFsbGJhY2s",
		"Subject": "Re: Final — results",
		"MessageID": "3c9e1f2a-0000-0000-0000-000000000099",
		"TextBody": "please recheck problem 3\n",
		"StrippedTextReply": "",
		"Date": "Sun, 5 Jul 2026 09:32:00 -0400",
		"Headers": []
	}`)

	in, err := p.ParseInbound(raw)
	if err != nil {
		t.Fatal(err)
	}
	if in.Subject != "Re: Final — results" {
		t.Errorf("Subject: got %q", in.Subject)
	}
	if in.TextBody != "please recheck problem 3\n" {
		t.Errorf("TextBody: got %q, want fallback to TextBody when StrippedTextReply is empty", in.TextBody)
	}
	if in.MessageID != "3c9e1f2a-0000-0000-0000-000000000099" {
		t.Errorf("MessageID: got %q want %q", in.MessageID, "3c9e1f2a-0000-0000-0000-000000000099")
	}
}

// TestPostmarkProvider_ParseInbound_PrefersFromFullEmail covers M5: when the top-level
// From is display-name-formatted ("Name <addr>"), ParseInbound must use FromFull.Email
// (the bare address) so the verification ladder's exact-email comparison matches the
// roster email instead of rejecting a legitimate reply.
func TestPostmarkProvider_ParseInbound_PrefersFromFullEmail(t *testing.T) {
	p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "tok"})
	raw := []byte(`{
		"From": "Ada Fake <ada@example.edu>",
		"FromFull": {"Email": "ada@example.edu", "Name": "Ada Fake", "MailboxHash": ""},
		"MailboxHash": "v1.7.1784289600.QUJD",
		"Subject": "Re: results",
		"TextBody": "please look again",
		"Date": "Sun, 5 Jul 2026 09:32:00 -0400",
		"Headers": []
	}`)

	in, err := p.ParseInbound(raw)
	if err != nil {
		t.Fatal(err)
	}
	if in.From != "ada@example.edu" {
		t.Errorf("From: got %q, want the bare FromFull.Email ada@example.edu (not the display-name form)", in.From)
	}
}

// TestPostmarkProvider_ParseInbound_FallsBackToFromWhenNoFromFull confirms the M5 change
// is a preference, not a requirement: a payload with only the top-level From (no
// FromFull) still parses using From.
func TestPostmarkProvider_ParseInbound_FallsBackToFromWhenNoFromFull(t *testing.T) {
	p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "tok"})
	raw := []byte(`{
		"From": "plain@example.edu",
		"MailboxHash": "v1.7.1784289600.QUJD",
		"Subject": "Re: results",
		"TextBody": "hi",
		"Date": "Sun, 5 Jul 2026 09:32:00 -0400",
		"Headers": []
	}`)
	in, err := p.ParseInbound(raw)
	if err != nil {
		t.Fatal(err)
	}
	if in.From != "plain@example.edu" {
		t.Errorf("From fallback: got %q want plain@example.edu", in.From)
	}
}

func TestPostmarkProvider_ParseInbound_SPFDKIMFail(t *testing.T) {
	p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "tok"})
	raw := loadFixture(t, "inbound_postmark_spf_fail.json")

	in, err := p.ParseInbound(raw)
	if err != nil {
		t.Fatal(err)
	}
	if in.From != "s0000002@example.edu" {
		t.Errorf("From: got %q", in.From)
	}
	if in.MailboxHash != "v1.7.1784289600.YW5vdGhlcnNpZw" {
		t.Errorf("MailboxHash: got %q", in.MailboxHash)
	}
	if in.MessageID != "3c9e1f2a-0000-0000-0000-000000000002" {
		t.Errorf("MessageID: got %q want %q", in.MessageID, "3c9e1f2a-0000-0000-0000-000000000002")
	}
	if in.SPFPass {
		t.Error("expected SPFPass=false for the failing fixture")
	}
	if in.DKIMValid {
		t.Error("expected DKIMValid=false for the failing fixture")
	}
}

func TestPostmarkProvider_ParseInbound_RejectsGarbage(t *testing.T) {
	p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "tok"})
	if _, err := p.ParseInbound([]byte(`not json`)); err == nil {
		t.Fatal("malformed JSON must error")
	}
}

func TestPostmarkProvider_ParseInbound_ErrorNeverEchoesBody(t *testing.T) {
	p := NewPostmarkProvider(Config{From: "grades@example.edu", PostmarkToken: "tok"})
	// Malformed JSON that nonetheless contains a distinctive "student content"
	// marker — the error message must not leak it (PII rule: nothing from
	// message bodies may appear in error strings).
	raw := []byte(`{"TextBody": "SUPER SECRET STUDENT ANSWER CONTENT", invalid`)
	_, err := p.ParseInbound(raw)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if strings.Contains(err.Error(), "SUPER SECRET STUDENT ANSWER CONTENT") {
		t.Errorf("error message leaked body content: %v", err)
	}
}

// Guard against the real endpoint accidentally answering unit tests offline —
// this only checks the constant, not a live call.
const postmarkRealBaseURLGuard = "https://api.postmarkapp.com"

func TestPostmarkProvider_DefaultBaseURLIsRealEndpoint(t *testing.T) {
	if postmarkDefaultBaseURL != postmarkRealBaseURLGuard {
		t.Errorf("postmarkDefaultBaseURL changed: got %q", postmarkDefaultBaseURL)
	}
}
