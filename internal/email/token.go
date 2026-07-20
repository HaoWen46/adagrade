package email

import "errors"

// ErrInvalidToken covers every regrade-token verification failure — malformed shape,
// bad signature, or expiry — deliberately undifferentiated so a caller (and any log
// line) cannot distinguish "well-formed but expired" from "garbage" without inspecting
// the token itself. The token is never included in the error text. Shared by the v2
// token chain (token_v2.go); the v1 single-use token (MintToken/VerifyToken) was removed
// once every caller moved onto the per-turn v2 chain (regrade-v2 §4, D57) — pre-production
// meant no live v1 token existed to migrate, and VerifyTokenV2 rejects the v1 wire shape
// outright, so no compatibility path is kept.
var ErrInvalidToken = errors.New("email: invalid or expired regrade token")
