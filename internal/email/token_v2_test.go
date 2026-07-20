package email

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Token v2 (spec §4, D57): "v2.<publish_item_id>.<turn>.<expiry-unix>.<b64url
// HMAC-SHA256(key, "v2|item|turn|expiry")>" — same HKDF subkey as v1, but the
// payload ADDS turn, and v1 tokens are rejected outright (pre-production, no
// live tokens to migrate).

// testKey is a deterministic 32-byte HKDF subkey stand-in for the token round-trip
// tests (the v1 token_test.go that once defined it was removed with the v1 functions).
func testKey() []byte {
	return bytes.Repeat([]byte{0x42}, 32)
}

func TestMintVerifyTokenV2_RoundTrip(t *testing.T) {
	key := testKey()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(14 * 24 * time.Hour)

	tok := MintTokenV2(key, 12345, 2, expiry)
	if tok == "" {
		t.Fatal("MintTokenV2 returned empty string")
	}

	itemID, turn, err := VerifyTokenV2(key, tok, now)
	if err != nil {
		t.Fatalf("VerifyTokenV2: %v", err)
	}
	if itemID != 12345 {
		t.Errorf("itemID: got %d want 12345", itemID)
	}
	if turn != 2 {
		t.Errorf("turn: got %d want 2", turn)
	}
}

func TestMintTokenV2_Format(t *testing.T) {
	key := testKey()
	expiry := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	tok := MintTokenV2(key, 42, 3, expiry)

	parts := strings.Split(tok, ".")
	if len(parts) != 5 {
		t.Fatalf("token format: got %d parts, want 5 (v2.itemID.turn.expiry.sig): %q", len(parts), tok)
	}
	if parts[0] != "v2" {
		t.Errorf("version prefix: got %q want v2", parts[0])
	}
	if parts[1] != "42" {
		t.Errorf("itemID segment: got %q want 42", parts[1])
	}
	if parts[2] != "3" {
		t.Errorf("turn segment: got %q want 3", parts[2])
	}
	wantExpiry := "1784289600" // 2026-07-17T12:00:00Z unix
	if parts[3] != wantExpiry {
		t.Errorf("expiry segment: got %q want %q", parts[3], wantExpiry)
	}
	if parts[4] == "" {
		t.Error("signature segment is empty")
	}
}

func TestMintTokenV2_DifferentTurnsProduceDifferentTokens(t *testing.T) {
	key := testKey()
	expiry := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	tok1 := MintTokenV2(key, 42, 1, expiry)
	tok2 := MintTokenV2(key, 42, 2, expiry)
	if tok1 == tok2 {
		t.Fatal("tokens for different turns of the same item must differ")
	}

	// Cross-verification: a turn-1 token's signature must not verify if its turn
	// segment is swapped for turn 2 (the turn is part of the signed message, not
	// just a label) — i.e. the signature actually binds turn.
	parts := strings.Split(tok1, ".")
	parts[2] = "2"
	tampered := strings.Join(parts, ".")
	if _, _, err := VerifyTokenV2(key, tampered, expiry.Add(-time.Hour)); err == nil {
		t.Fatal("swapping the turn segment must invalidate the signature")
	}
}

func TestVerifyTokenV2_RejectsTamperedItemID(t *testing.T) {
	key := testKey()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	tok := MintTokenV2(key, 100, 1, now.Add(time.Hour))

	parts := strings.Split(tok, ".")
	parts[1] = "999"
	tampered := strings.Join(parts, ".")

	if _, _, err := VerifyTokenV2(key, tampered, now); err == nil {
		t.Fatal("tampered itemID must not verify")
	}
}

func TestVerifyTokenV2_RejectsTamperedTurn(t *testing.T) {
	key := testKey()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	tok := MintTokenV2(key, 100, 1, now.Add(time.Hour))

	parts := strings.Split(tok, ".")
	parts[2] = "99"
	tampered := strings.Join(parts, ".")

	if _, _, err := VerifyTokenV2(key, tampered, now); err == nil {
		t.Fatal("tampered turn must not verify")
	}
}

func TestVerifyTokenV2_RejectsTamperedExpiry(t *testing.T) {
	key := testKey()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	tok := MintTokenV2(key, 100, 1, now.Add(time.Hour))

	parts := strings.Split(tok, ".")
	parts[3] = "9999999999"
	tampered := strings.Join(parts, ".")

	if _, _, err := VerifyTokenV2(key, tampered, now); err == nil {
		t.Fatal("tampered expiry must not verify")
	}
}

func TestVerifyTokenV2_RejectsTamperedSignature(t *testing.T) {
	key := testKey()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	tok := MintTokenV2(key, 100, 1, now.Add(time.Hour))

	parts := strings.Split(tok, ".")
	sig := []byte(parts[4])
	if sig[len(sig)-1] == 'A' {
		sig[len(sig)-1] = 'B'
	} else {
		sig[len(sig)-1] = 'A'
	}
	parts[4] = string(sig)
	tampered := strings.Join(parts, ".")

	if _, _, err := VerifyTokenV2(key, tampered, now); err == nil {
		t.Fatal("tampered signature must not verify")
	}
}

func TestVerifyTokenV2_RejectsExpired(t *testing.T) {
	key := testKey()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	tok := MintTokenV2(key, 100, 1, now.Add(-time.Second)) // already expired

	if _, _, err := VerifyTokenV2(key, tok, now); err == nil {
		t.Fatal("expired token must not verify")
	}
}

func TestVerifyTokenV2_RejectsWrongKey(t *testing.T) {
	key := testKey()
	other := bytes.Repeat([]byte{0x99}, 32)
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	tok := MintTokenV2(key, 100, 1, now.Add(time.Hour))

	if _, _, err := VerifyTokenV2(other, tok, now); err == nil {
		t.Fatal("token signed with a different key must not verify")
	}
}

// TestVerifyTokenV2_RejectsV1Token is the pre-production compatibility rule
// (spec §4): v1 tokens are REJECTED outright by the v2 verifier, no migration
// path. The v1 Mint/VerifyToken functions were removed once every caller moved
// onto the v2 chain, so the v1 wire shape is exercised as a literal 4-part
// "v1.<item>.<expiry>.<sig>" string — VerifyTokenV2 only ever accepts the 5-part
// "v2." shape, so any v1-shaped token fails the length/version check outright.
func TestVerifyTokenV2_RejectsV1Token(t *testing.T) {
	key := testKey()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	// A structurally-valid v1 token shape (4 dot-separated parts, version "v1").
	// Whatever the signature, the version/arity check rejects it before signature
	// verification even runs.
	v1Shaped := "v1.100.9999999999.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	if _, _, err := VerifyTokenV2(key, v1Shaped, now); err == nil {
		t.Fatal("a v1-shaped token must not verify under VerifyTokenV2")
	}
}

func TestVerifyTokenV2_RejectsMalformed(t *testing.T) {
	key := testKey()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	cases := []string{
		"",
		"garbage",
		"v2.abc.1.123.sig",         // itemID not a number
		"v2.100.abc.123.sig",       // turn not a number
		"v2.100.1.notanumber.sig",  // expiry not a number
		"v1.100.9999999999.sig",    // v1 shape (4 parts) entirely
		"v2.100.1.9999999999",      // missing signature segment
		"v2.100.1.9999999999.a.b",  // too many segments
		"v2.100.0.9999999999.sig",  // turn zero (turns are 1-based per spec §4)
		"v2.100.-1.9999999999.sig", // negative turn
	}
	for _, c := range cases {
		if _, _, err := VerifyTokenV2(key, c, now); err == nil {
			t.Errorf("malformed/invalid token %q must not verify", c)
		}
	}
}

func TestVerifyTokenV2_ErrorNeverLeaksToken(t *testing.T) {
	key := testKey()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	tok := MintTokenV2(key, 100, 1, now.Add(-time.Hour)) // expired

	_, _, err := VerifyTokenV2(key, tok, now)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), tok) {
		t.Errorf("error message must not contain the raw token: %v", err)
	}
}

func TestMintVerifyTokenV2_MultipleTurnsRoundTrip(t *testing.T) {
	key := testKey()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(14 * 24 * time.Hour)

	for turn := 1; turn <= 6; turn++ {
		tok := MintTokenV2(key, 7, turn, expiry)
		gotItem, gotTurn, err := VerifyTokenV2(key, tok, now)
		if err != nil {
			t.Fatalf("turn %d: VerifyTokenV2: %v", turn, err)
		}
		if gotItem != 7 || gotTurn != turn {
			t.Errorf("turn %d: got (item=%d turn=%d), want (item=7 turn=%d)", turn, gotItem, gotTurn, turn)
		}
	}
}
