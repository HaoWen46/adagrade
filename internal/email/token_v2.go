package email

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// tokenVersionV2 is the version segment minted/accepted by MintTokenV2 and
// VerifyTokenV2 (spec §4, D57). v1 tokens (tokenVersion = "v1") are rejected
// outright by VerifyTokenV2 — this is pre-production, so there was no live v1
// token to migrate; v1's Mint/VerifyToken were removed once every caller moved
// onto the turn-token chain (see token.go's ErrInvalidToken doc) — there is no
// compatibility path, only the wire-shape check that still recognizes and
// rejects a well-formed v1 token rather than erroring on it as garbage.
const tokenVersionV2 = "v2"

// MintTokenV2 signs a regrade turn token binding a publish_item id AND a turn
// number to an expiry, per spec §4: "v2.<publish_item_id>.<turn>.<expiry-unix>.
// <b64url HMAC-SHA256(key, "v2|item|turn|expiry")>". key is the same HKDF
// subkey as v1 (secrets.Derive(masterKey, "regrade-token-v1") — the subkey name
// itself is unchanged, only the token payload/version gain a field). turn is
// the 1-based turn this specific email's Reply-To grants: the grade email
// mints turn=1; result email #N mints turn=N+1 (§4).
func MintTokenV2(key []byte, itemID int64, turn int, expiry time.Time) string {
	itemStr := strconv.FormatInt(itemID, 10)
	turnStr := strconv.Itoa(turn)
	expiryStr := strconv.FormatInt(expiry.Unix(), 10)
	sig := signV2(key, itemStr, turnStr, expiryStr)
	return strings.Join([]string{tokenVersionV2, itemStr, turnStr, expiryStr, sig}, ".")
}

// VerifyTokenV2 checks a v2 token's shape, signature (constant-time), and
// expiry against now, returning the bound publish_item id and turn. A
// well-formed v1 token (four dot-separated parts, version "v1") is rejected
// here just like any other malformed input — VerifyTokenV2 only ever accepts
// "v2" in the version slot. turn must be >= 1 (turns are 1-based per spec §4);
// a token carrying turn <= 0 is rejected even if its signature is otherwise
// valid, since such a token could never have been legitimately minted. It
// never logs or echoes the raw token — the caller must not either (PII/
// security rule: a valid token is a bearer credential for a student's regrade
// reply address).
func VerifyTokenV2(key []byte, tok string, now time.Time) (itemID int64, turn int, err error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 5 {
		return 0, 0, ErrInvalidToken
	}
	version, itemStr, turnStr, expiryStr, sig := parts[0], parts[1], parts[2], parts[3], parts[4]
	if version != tokenVersionV2 {
		return 0, 0, ErrInvalidToken
	}

	wantSig := signV2(key, itemStr, turnStr, expiryStr)
	if !hmac.Equal([]byte(sig), []byte(wantSig)) {
		return 0, 0, ErrInvalidToken
	}

	id, err := strconv.ParseInt(itemStr, 10, 64)
	if err != nil {
		return 0, 0, ErrInvalidToken
	}
	turnVal, err := strconv.Atoi(turnStr)
	if err != nil {
		return 0, 0, ErrInvalidToken
	}
	if turnVal < 1 {
		return 0, 0, ErrInvalidToken
	}
	expiryUnix, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return 0, 0, ErrInvalidToken
	}
	if now.After(time.Unix(expiryUnix, 0)) {
		return 0, 0, ErrInvalidToken
	}
	return id, turnVal, nil
}

// signV2 computes the base64url (no padding) HMAC-SHA256 over the pipe-joined
// version|itemID|turn|expiry — matching spec §4's "HMAC-SHA256(key,
// "v2|item|turn|expiry")" literally, so the signed message is unambiguous even
// though the token's own field separator is ".". Binding turn into the signed
// message (not just the token's positional segment) means a tampered turn
// segment fails signature verification, not just a value-swap.
func signV2(key []byte, itemStr, turnStr, expiryStr string) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s|%s|%s|%s", tokenVersionV2, itemStr, turnStr, expiryStr)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
