// Package auth holds the authorization decision for Google-Workspace sign-in.
//
// ADA-Marker has no student accounts: the only people who log in are an allowlisted
// team of admins, lecturers, and TAs. This package answers one question — given a
// *already-cryptographically-verified* Google ID token, may this person in?
//
// Token verification (signature, issuer, audience/aud, expiry) happens elsewhere with
// the Google verifier. Authorize takes the resulting claims and applies ADA-Marker's
// own policy: the database allowlist is authoritative; the hosted-domain (hd) claim is
// an extra guard.
package auth

import "strings"

// Role is a user's authorization level. Admin and Lecturer have full access; TA can
// grade, run methods, and handle regrades. (Plan §8.)
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleLecturer Role = "lecturer"
	RoleTA       Role = "ta"
)

// roleRank orders the roles for at-least comparisons: TA < Lecturer < Admin. An
// unknown/empty role ranks 0 (below TA), so it never satisfies any minimum.
var roleRank = map[Role]int{RoleTA: 1, RoleLecturer: 2, RoleAdmin: 3}

// RoleAtLeast reports whether have meets or exceeds the min role in the TA < Lecturer <
// Admin ordering. It is the single place role rank is defined so route middleware and
// handler-level checks (e.g. the TA-assignment endpoint verifying the assignee holds
// TA-or-higher) can't drift.
func RoleAtLeast(have, min Role) bool {
	return roleRank[have] >= roleRank[min]
}

// Claims is the subset of a verified Google ID token that the decision depends on.
type Claims struct {
	Email         string
	EmailVerified bool
	// HostedDomain is the Google Workspace "hd" claim. It is empty for personal
	// (e.g. @gmail.com) accounts, which is itself meaningful.
	HostedDomain string
}

// AllowlistEntry is what the database knows about an allowlisted email.
type AllowlistEntry struct {
	Role   Role
	Active bool
}

// AllowlistLookup resolves a normalized email to its allowlist entry. The boolean is
// false when the email is not on the list at all. Injected so the decision stays a
// pure, testable function independent of the database.
type AllowlistLookup func(normalizedEmail string) (AllowlistEntry, bool)

// Denial reasons (machine-readable, for logging/telemetry — never shown verbatim to a user).
const (
	ReasonEmailUnverified = "email_unverified"
	ReasonWrongDomain     = "wrong_hosted_domain"
	ReasonNotAllowlisted  = "not_allowlisted"
	ReasonInactive        = "account_inactive"
)

// Decision is the outcome of an authorization check.
type Decision struct {
	Authorized bool
	Role       Role
	Reason     string // set only when !Authorized
}

// NormalizeEmail canonicalizes an email for allowlist comparison: trim + lowercase.
//
// NOTE: we deliberately do NOT strip dots or "+tags". On gmail.com those are ignored,
// but on a custom Workspace domain like ntu.edu.tw the local part is opaque and dots
// can be significant, so stripping them could wrongly merge two distinct people.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Authorize applies ADA-Marker's sign-in policy to already-verified token claims.
//
// expectedDomain is the required Workspace hosted domain (e.g. "ntu.edu.tw"); pass ""
// to disable the hd guard entirely.
func Authorize(c Claims, expectedDomain string, lookup AllowlistLookup) Decision {
	if !c.EmailVerified {
		return Decision{Reason: ReasonEmailUnverified}
	}

	entry, ok := lookup(NormalizeEmail(c.Email))
	if !ok {
		return Decision{Reason: ReasonNotAllowlisted}
	}
	if !entry.Active {
		return Decision{Reason: ReasonInactive}
	}

	// Hosted-domain guard: only fires when the token actually carries an hd. A present
	// but mismatched hd is suspicious; an absent hd (personal account) falls through to
	// the allowlist, which is authoritative.
	if expectedDomain != "" && c.HostedDomain != "" && c.HostedDomain != expectedDomain {
		return Decision{Reason: ReasonWrongDomain}
	}

	return Decision{Authorized: true, Role: entry.Role}
}
