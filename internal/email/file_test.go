package email

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/HaoWen46/adagrade/internal/domain"
)

func TestFileProvider_WritesEmlUnderOutboxDir(t *testing.T) {
	dir := t.TempDir()
	p, err := NewFileProvider(dir, "inbound.example.edu")
	if err != nil {
		t.Fatal(err)
	}

	msg := domain.OutboundEmail{
		To:       "s0000005@example.edu",
		ReplyTo:  "regrade+v1.9.123.sig@inbound.example.edu",
		Subject:  "Midterm 2 — results",
		TextBody: "Hi Student Five,\n\nTotal: 25/30\n",
		HTMLBody: "<html><body>Hi Student Five,<br>Total: 25/30</body></html>",
	}

	id, err := p.Send(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Error("Send must return a non-empty providerID for the file provider")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("outbox dir: got %d files, want 1: %v", len(entries), entries)
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, ".eml") {
		t.Errorf("file name %q does not end in .eml", name)
	}

	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, want := range []string{
		"To: s0000005@example.edu",
		"Reply-To: regrade+v1.9.123.sig@inbound.example.edu",
		"Subject: Midterm 2 — results",
		"MIME-Version: 1.0",
		"multipart/alternative",
		"Hi Student Five,",
		"Total: 25/30",
	} {
		if !strings.Contains(content, want) {
			t.Errorf(".eml missing %q\n---\n%s", want, content)
		}
	}
}

func TestFileProvider_DeliveryKeyReusesOneAtomicFile(t *testing.T) {
	dir := t.TempDir()
	p, err := NewFileProvider(dir, "")
	if err != nil {
		t.Fatal(err)
	}

	const deliveryKey = "publish/item/42/../../must-not-reach-filename"
	first := domain.OutboundEmail{
		DeliveryKey: deliveryKey,
		To:          "s0000005@example.edu",
		Subject:     "original subject",
		TextBody:    "original body",
	}
	id1, err := p.Send(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}

	// Reusing a key is an idempotent replay, even if a buggy caller supplies
	// changed content: the already-completed file is reused, never overwritten.
	second := first
	second.Subject = "must not overwrite"
	second.TextBody = "must not overwrite"
	id2, err := p.Send(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 || id1 == "" {
		t.Fatalf("provider ids = %q, %q; want one stable non-empty id", id1, id2)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("outbox entries = %d, want exactly one: %v", len(entries), entries)
	}
	name := entries[0].Name()
	if strings.Contains(name, "publish") || strings.ContainsAny(name, `/\\`) || !strings.HasSuffix(name, ".eml") {
		t.Fatalf("delivery filename is not opaque/safe: %q", name)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Subject: original subject") || strings.Contains(string(raw), "must not overwrite") {
		t.Fatalf("idempotent replay overwrote the first completed message:\n%s", raw)
	}
	if !strings.Contains(string(raw), "Message-Id: <"+id1+"@adamarker.local>") {
		t.Fatalf("message lacks stable correlation Message-Id for %q", id1)
	}
}

func TestFileProvider_DeliveryKeyConcurrentReplayCreatesOneFile(t *testing.T) {
	dir := t.TempDir()
	p, err := NewFileProvider(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	msg := domain.OutboundEmail{
		DeliveryKey: "publish-item-84",
		To:          "s0000006@example.edu",
		Subject:     "x",
		TextBody:    "y",
	}

	const sends = 16
	ids := make([]string, sends)
	errs := make([]error, sends)
	var wg sync.WaitGroup
	for i := range sends {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = p.Send(context.Background(), msg)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("send %d id = %q, want %q", i, ids[i], ids[0])
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".eml") {
		t.Fatalf("concurrent replay left entries %v, want one completed .eml", entries)
	}
}

func TestFileProvider_SendClassifiesRejectedMessage(t *testing.T) {
	p, err := NewFileProvider(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Send(context.Background(), domain.OutboundEmail{
		DeliveryKey: "publish-item-85",
		To:          "s0000007@example.edu",
		Subject:     "bad\r\nBcc: injected@example.invalid",
		TextBody:    "y",
	})
	if !domain.IsEmailDefinitelyNotAccepted(err) {
		t.Fatalf("rejected file message outcome = %q, err=%v", domain.EmailDeliveryOutcomeOf(err), err)
	}
}

func TestFileProvider_MultipleSendsProduceDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	p, err := NewFileProvider(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	msg := domain.OutboundEmail{To: "s0000006@example.edu", Subject: "x", TextBody: "y"}

	id1, err := p.Send(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := p.Send(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Errorf("two sends produced the same providerID %q — files would collide", id1)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("outbox dir: got %d files, want 2", len(entries))
	}
}

func TestFileProvider_CreatesOutboxDirIfMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "outbox")
	p, err := NewFileProvider(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Send(context.Background(), domain.OutboundEmail{To: "s0000007@example.edu", Subject: "x", TextBody: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("outbox dir was not created: %v", err)
	}
}

// TestBuildEML_RejectsCRLFInjection is Finding 3 (IMPORTANT): buildEML must
// reject (not silently strip) a \r or \n embedded in any header-bound field —
// Subject is reachable from attacker-influenced data (AssessmentName) and To/
// ReplyTo come from roster/token data. A poisoned value must be loud.
func TestBuildEML_RejectsCRLFInjection(t *testing.T) {
	cases := []struct {
		name string
		msg  domain.OutboundEmail
	}{
		{
			name: "subject with embedded CRLF",
			msg: domain.OutboundEmail{
				To:       "s0000014@example.edu",
				Subject:  "Midterm 2\r\nX-Injected: evil",
				TextBody: "Total: 25/30\n",
			},
		},
		{
			name: "to with embedded CRLF",
			msg: domain.OutboundEmail{
				To:       "s0000014@example.edu\r\nBcc: attacker@evil.example",
				Subject:  "Midterm 2",
				TextBody: "Total: 25/30\n",
			},
		},
		{
			name: "reply-to with embedded CRLF",
			msg: domain.OutboundEmail{
				To:       "s0000014@example.edu",
				ReplyTo:  "regrade+abc@example.edu\r\nBcc: attacker@evil.example",
				Subject:  "Midterm 2",
				TextBody: "Total: 25/30\n",
			},
		},
		{
			name: "subject with bare LF",
			msg: domain.OutboundEmail{
				To:       "s0000014@example.edu",
				Subject:  "Midterm 2\nX-Injected: evil",
				TextBody: "Total: 25/30\n",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildEML(tc.msg, "deadbeef"); err == nil {
				t.Fatal("buildEML must reject a header-bound field containing CR or LF, not silently emit it")
			}
		})
	}
}

// TestFileProvider_Send_RejectsCRLFInjectionAndWritesNoFile exercises the same
// attack through the full Send path: a poisoned Subject must cause Send to
// fail and no .eml file may be written to the outbox dir.
func TestFileProvider_Send_RejectsCRLFInjectionAndWritesNoFile(t *testing.T) {
	dir := t.TempDir()
	p, err := NewFileProvider(dir, "")
	if err != nil {
		t.Fatal(err)
	}

	msg := domain.OutboundEmail{
		To:       "s0000015@example.edu",
		Subject:  "Midterm 2\r\nBcc: attacker@evil.example",
		TextBody: "Total: 25/30\n",
	}
	if _, err := p.Send(context.Background(), msg); err == nil {
		t.Fatal("Send must fail for a CRLF-poisoned Subject — no .eml may be emitted")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("outbox dir: got %d files, want 0 — no file should be written for a rejected message", len(entries))
	}
}

// ---- Attachments (report-attachments spec §3) ----

// TestFileProvider_WritesAttachmentIntoEml asserts a report PDF attachment
// ends up multipart/mixed in the written .eml, with the attachment's
// filename, MIME type, and base64 content transfer encoding all present —
// the file provider is what a developer actually opens to eyeball the real
// output, so this is the most direct "does the attachment plumbing work"
// check.
func TestFileProvider_WritesAttachmentIntoEml(t *testing.T) {
	dir := t.TempDir()
	p, err := NewFileProvider(dir, "")
	if err != nil {
		t.Fatal(err)
	}

	msg := domain.OutboundEmail{
		To:       "s0000016@example.edu",
		Subject:  "Midterm 2 — results",
		TextBody: "Total: 25/30\n",
		HTMLBody: "<p>Total: 25/30</p>",
		Attachments: []domain.Attachment{
			{Filename: "midterm-2-results.pdf", MIME: "application/pdf", Content: []byte("%PDF-1.4 fake\n")},
		},
	}
	if _, err := p.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("outbox dir: got %d files, want 1", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, want := range []string{
		"multipart/mixed",
		"multipart/alternative",
		"application/pdf",
		`filename="midterm-2-results.pdf"`,
		"Content-Disposition: attachment",
		"Content-Transfer-Encoding: base64",
		"Total: 25/30",
	} {
		if !strings.Contains(content, want) {
			t.Errorf(".eml missing %q\n---\n%s", want, content)
		}
	}
}

// TestFileProvider_NoAttachments_StaysMultipartAlternative pins the common
// (no-attachment) case unchanged.
func TestFileProvider_NoAttachments_StaysMultipartAlternative(t *testing.T) {
	dir := t.TempDir()
	p, err := NewFileProvider(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	msg := domain.OutboundEmail{To: "s0000017@example.edu", Subject: "x", TextBody: "y"}
	if _, err := p.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if !strings.Contains(content, "multipart/alternative") {
		t.Errorf(".eml should be multipart/alternative when there are no attachments:\n%s", content)
	}
	if strings.Contains(content, "multipart/mixed") {
		t.Errorf(".eml must not mention multipart/mixed when there are no attachments:\n%s", content)
	}
}

func TestBuildEML_RejectsCRLFInAttachmentFilename(t *testing.T) {
	_, err := buildEML(domain.OutboundEmail{
		To:       "s0000018@example.edu",
		Subject:  "Midterm 2",
		TextBody: "Total: 25/30",
		Attachments: []domain.Attachment{
			{Filename: "results.pdf\r\nX-Injected: evil", MIME: "application/pdf", Content: []byte("x")},
		},
	}, "deadbeef")
	if err == nil {
		t.Fatal("buildEML must reject a CRLF-poisoned attachment filename")
	}
}

// TestBuildEML_RejectsCRLFInAttachmentMIME is A11: MIME is interpolated into a
// raw "Content-Type: %s; name=%q" header line the same as Filename, so it must
// be guarded the same way.
func TestBuildEML_RejectsCRLFInAttachmentMIME(t *testing.T) {
	_, err := buildEML(domain.OutboundEmail{
		To:       "s0000018@example.edu",
		Subject:  "Midterm 2",
		TextBody: "Total: 25/30",
		Attachments: []domain.Attachment{
			{Filename: "results.pdf", MIME: "application/pdf\r\nX-Injected: evil", Content: []byte("x")},
		},
	}, "deadbeef")
	if err == nil {
		t.Fatal("buildEML must reject a CRLF-poisoned attachment MIME type")
	}
}

func TestFileProvider_ParseInboundNotSupported(t *testing.T) {
	p, err := NewFileProvider(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ParseInbound([]byte(`{}`)); err == nil {
		t.Fatal("file provider has no real inbound webhook — ParseInbound must error")
	}
}

// Dev-only inbound parsing (D-note in file.go): with DevInbound enabled the file
// provider parses the same Postmark inbound JSON the real webhook receives, so a
// local setup can simulate student regrade replies. Off by default (test above).
func TestFileProvider_ParseInbound_DevMode(t *testing.T) {
	p, err := NewFileProvider(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	p.devInbound = true
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
}
