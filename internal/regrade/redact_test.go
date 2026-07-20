package regrade

import (
	"strings"
	"testing"
)

// All fixture identities below are INVENTED (CLAUDE.md): no real student PII.

func TestRedact_RemovesNameIDEmailAndToken(t *testing.T) {
	id := Identity{
		Name:      "Ada Fake",
		StudentID: "b09901001",
		Email:     "ada.fake@example.edu",
	}
	text := "Hi, I'm Ada Fake (b09901001). Reply to me at ada.fake@example.edu or regrade+abc123.def@inbound.example.edu. Please recheck problem 3."

	out, counts := Redact(text, id)

	for _, leaked := range []string{"Ada Fake", "b09901001", "ada.fake@example.edu", "regrade+abc123.def@inbound.example.edu"} {
		if strings.Contains(out, leaked) {
			t.Errorf("redacted text still contains %q:\n%s", leaked, out)
		}
	}
	// The substantive request content survives.
	if !strings.Contains(out, "Please recheck problem 3") {
		t.Errorf("redaction removed the substantive request text:\n%s", out)
	}
	if counts.Name != 1 || counts.StudentID != 1 || counts.Email != 1 || counts.Token != 1 {
		t.Errorf("counts = %+v, want name=1 id=1 email=1 token=1", counts)
	}
}

func TestRedact_MultipleOccurrencesAllRemovedAndCounted(t *testing.T) {
	id := Identity{Name: "Ivy Test", StudentID: "b09901002", Email: "ivy@example.edu"}
	text := "Ivy Test here. Ivy Test again. b09901002 b09901002 b09901002. ivy@example.edu"

	out, counts := Redact(text, id)
	if strings.Contains(out, "Ivy Test") || strings.Contains(out, "b09901002") || strings.Contains(out, "ivy@example.edu") {
		t.Errorf("residual identity after redaction:\n%s", out)
	}
	if counts.Name != 2 || counts.StudentID != 3 || counts.Email != 1 {
		t.Errorf("counts = %+v, want name=2 id=3 email=1", counts)
	}
}

func TestRedact_CaseInsensitiveNameAndEmail(t *testing.T) {
	// Roster-name and email matching is case-insensitive: a student who signs "ADA FAKE"
	// or writes their address in a different case must still be scrubbed.
	id := Identity{Name: "Ada Fake", StudentID: "B09901001", Email: "Ada.Fake@Example.edu"}
	text := "ADA FAKE, id b09901001, ada.fake@EXAMPLE.EDU — please look again."

	out, counts := Redact(text, id)
	lower := strings.ToLower(out)
	if strings.Contains(lower, "ada fake") || strings.Contains(lower, "b09901001") || strings.Contains(lower, "ada.fake@example.edu") {
		t.Errorf("case-variant identity survived redaction:\n%s", out)
	}
	if counts.Name != 1 || counts.StudentID != 1 || counts.Email != 1 {
		t.Errorf("counts = %+v, want each 1", counts)
	}
}

func TestRedact_TokenPatternIndependentOfKnownAddress(t *testing.T) {
	// A regrade+<token>@... string is scrubbed by PATTERN even if it isn't the
	// student's own known email (a student might quote the reply-to header verbatim),
	// so the mailbox-hash token never reaches the provider.
	id := Identity{Name: "Sam Roe", StudentID: "b09901003", Email: "sam@example.edu"}
	text := "Original reply-to was regrade+9f8e7d6c5b4a@inbound.example.edu; I disagree with the score."

	out, counts := Redact(text, id)
	if strings.Contains(out, "regrade+9f8e7d6c5b4a@inbound.example.edu") {
		t.Errorf("regrade token survived redaction:\n%s", out)
	}
	if counts.Token != 1 {
		t.Errorf("token count = %d, want 1", counts.Token)
	}
}

func TestRedact_NoIdentityLeavesTextIntactWithZeroCounts(t *testing.T) {
	id := Identity{Name: "Nora Kim", StudentID: "b09901004", Email: "nora@example.edu"}
	text := "I believe the recurrence in part (b) was marked wrong; the base case is correct."

	out, counts := Redact(text, id)
	if out != text {
		t.Errorf("text with no identity mentions should be unchanged:\ngot:  %s\nwant: %s", out, text)
	}
	if counts.Name != 0 || counts.StudentID != 0 || counts.Email != 0 || counts.Token != 0 {
		t.Errorf("counts = %+v, want all zero", counts)
	}
}

func TestRedact_EmptyIdentityFieldsAreSkippedNotWildcards(t *testing.T) {
	// An empty identity field must NOT match everything (a blank Name replacing every
	// empty position, etc.) — empty fields are simply not applied.
	id := Identity{Name: "", StudentID: "", Email: ""}
	text := "Please regrade problem 2."

	out, counts := Redact(text, id)
	if out != text {
		t.Errorf("empty identity must be a no-op, got:\n%s", out)
	}
	if counts != (RedactionCounts{}) {
		t.Errorf("empty identity must produce zero counts, got %+v", counts)
	}
}

// --- case-fold byte-length shifts (PII leak) ------------------------------------------
//
// replaceFoldCount matches on a LOWERCASED copy of the text. Some runes change byte
// length when lowercased (U+212A KELVIN SIGN 3→1 bytes, U+0130 İ 2→1, U+023A Ⱥ 2→3),
// so a byte offset found in the lowered copy is NOT a valid offset into the original.
// Slicing the original with lowered offsets shifted every later match and let identity
// (a student id!) survive "redaction" essentially intact — straight into an AI prompt.

func TestReplaceFoldCount_ShrinkingRunes_KelvinSign_NeedleFullyRemoved(t *testing.T) {
	// Four U+212A KELVIN SIGN runes (3 bytes each) lowercase to 'k' (1 byte each):
	// the lowered copy is 8 bytes shorter, so a match found after them lands 8 bytes
	// early when sliced from the original. Proven leak before the fix:
	// "...m[redacted]09901001 thanks" — the id survived nearly intact.
	text := "sent from my phone KKKK -- my id is b09901001 thanks"
	out, count := replaceFoldCount(text, "b09901001")

	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
	if want := "sent from my phone KKKK -- my id is [redacted] thanks"; out != want {
		t.Errorf("out:\n got %q\nwant %q", out, want)
	}
	if strings.Contains(out, "b09901001") || strings.Contains(out, "09901001") {
		t.Errorf("student id (or its tail) survived redaction: %q", out)
	}
}

func TestReplaceFoldCount_GrowingRunes_NeedleFullyRemoved(t *testing.T) {
	// U+023A Ⱥ (2 bytes) lowercases to U+2C65 ⱥ (3 bytes): the lowered copy is LONGER,
	// so lowered offsets land past the true position in the original — slicing eats
	// trailing context and leaves the needle's head behind.
	text := "ȺȺȺ wrote: my id is b09901001, please recheck"
	out, count := replaceFoldCount(text, "b09901001")

	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
	if want := "ȺȺȺ wrote: my id is [redacted], please recheck"; out != want {
		t.Errorf("out:\n got %q\nwant %q", out, want)
	}
	if strings.Contains(out, "b09901001") || strings.Contains(out, "09901001") {
		t.Errorf("student id (or its tail) survived redaction: %q", out)
	}
}

func TestReplaceFoldCount_AllASCII_Regression(t *testing.T) {
	// The plain path must keep working exactly as before: case-insensitive, every
	// occurrence replaced and counted.
	out, count := replaceFoldCount("ID b09901001 and B09901001.", "B09901001")

	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	if want := "ID [redacted] and [redacted]."; out != want {
		t.Errorf("out:\n got %q\nwant %q", out, want)
	}
}

func TestRedact_StudentIDAfterKelvinSigns_NeverReachesPrompt(t *testing.T) {
	// End-to-end guard on the PII contract: the exact leak shape (Kelvin signs before
	// the id) must scrub the id through the public Redact API too.
	id := Identity{Name: "Ada Fake", StudentID: "b09901001", Email: "ada.fake@example.edu"}
	text := "sent from my phone KKKK -- my id is b09901001 thanks"

	out, counts := Redact(text, id)
	if counts.StudentID != 1 {
		t.Errorf("counts.StudentID = %d, want 1", counts.StudentID)
	}
	if strings.Contains(strings.ToLower(out), "b09901001") || strings.Contains(out, "09901001") {
		t.Errorf("student id survived Redact: %q", out)
	}
}

// TestRedact_CountsOnly_NoContentInReturnedCounts is a guard on the PII contract
// (D51): RedactionCounts must be a plain numeric struct — nothing about it can carry
// the redacted content itself (it is the only thing that gets logged).
func TestRedact_CountsOnly(t *testing.T) {
	id := Identity{Name: "Leo Vance", StudentID: "b09901005", Email: "leo@example.edu"}
	_, counts := Redact("Leo Vance b09901005 leo@example.edu", id)
	// If this compiles and the fields are ints, the counts-only contract holds. Assert
	// the values so the test is meaningful too.
	if counts.Name+counts.StudentID+counts.Email+counts.Token != 3 {
		t.Errorf("expected 3 total redactions, got %+v", counts)
	}
}
