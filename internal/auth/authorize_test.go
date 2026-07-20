package auth

import "testing"

// fixedList builds an AllowlistLookup from a map keyed by normalized email.
func fixedList(entries map[string]AllowlistEntry) AllowlistLookup {
	return func(email string) (AllowlistEntry, bool) {
		e, ok := entries[email]
		return e, ok
	}
}

func TestAuthorize_AllowlistedWorkspaceUser(t *testing.T) {
	list := fixedList(map[string]AllowlistEntry{
		"b11902156@ntu.edu.tw": {Role: RoleTA, Active: true},
	})
	c := Claims{Email: "b11902156@ntu.edu.tw", EmailVerified: true, HostedDomain: "ntu.edu.tw"}

	got := Authorize(c, "ntu.edu.tw", list)

	if !got.Authorized {
		t.Fatalf("expected authorized, got denied: %q", got.Reason)
	}
	if got.Role != RoleTA {
		t.Fatalf("expected role %q, got %q", RoleTA, got.Role)
	}
}

func TestAuthorize_RejectsUnverifiedEmail(t *testing.T) {
	list := fixedList(map[string]AllowlistEntry{
		"ta@ntu.edu.tw": {Role: RoleTA, Active: true},
	})
	c := Claims{Email: "ta@ntu.edu.tw", EmailVerified: false, HostedDomain: "ntu.edu.tw"}

	got := Authorize(c, "ntu.edu.tw", list)

	if got.Authorized {
		t.Fatal("expected denial for unverified email")
	}
	if got.Reason != ReasonEmailUnverified {
		t.Fatalf("expected reason %q, got %q", ReasonEmailUnverified, got.Reason)
	}
}

func TestAuthorize_RejectsEmailNotOnAllowlist(t *testing.T) {
	list := fixedList(map[string]AllowlistEntry{})
	c := Claims{Email: "stranger@ntu.edu.tw", EmailVerified: true, HostedDomain: "ntu.edu.tw"}

	got := Authorize(c, "ntu.edu.tw", list)

	if got.Authorized {
		t.Fatal("expected denial for non-allowlisted email")
	}
	if got.Reason != ReasonNotAllowlisted {
		t.Fatalf("expected reason %q, got %q", ReasonNotAllowlisted, got.Reason)
	}
}

func TestAuthorize_RejectsInactiveAccount(t *testing.T) {
	list := fixedList(map[string]AllowlistEntry{
		"former.ta@ntu.edu.tw": {Role: RoleTA, Active: false},
	})
	c := Claims{Email: "former.ta@ntu.edu.tw", EmailVerified: true, HostedDomain: "ntu.edu.tw"}

	got := Authorize(c, "ntu.edu.tw", list)

	if got.Authorized {
		t.Fatal("expected denial for inactive account")
	}
	if got.Reason != ReasonInactive {
		t.Fatalf("expected reason %q, got %q", ReasonInactive, got.Reason)
	}
}

func TestAuthorize_RejectsWrongHostedDomain(t *testing.T) {
	// An allowlisted address, but the sign-in came through a *different* Workspace
	// tenant than expected — treat as suspicious even though the email is on the list.
	list := fixedList(map[string]AllowlistEntry{
		"ta@ntu.edu.tw": {Role: RoleTA, Active: true},
	})
	c := Claims{Email: "ta@ntu.edu.tw", EmailVerified: true, HostedDomain: "evil.example.com"}

	got := Authorize(c, "ntu.edu.tw", list)

	if got.Authorized {
		t.Fatal("expected denial for wrong hosted domain")
	}
	if got.Reason != ReasonWrongDomain {
		t.Fatalf("expected reason %q, got %q", ReasonWrongDomain, got.Reason)
	}
}

func TestAuthorize_AllowsAllowlistedPersonalAccountWhenNoHostedDomain(t *testing.T) {
	// Policy decision (v0): the allowlist is authoritative. A personal account (no hd)
	// that is explicitly allowlisted is permitted; the hd guard only fires when a
	// hosted domain is actually present. This is a flagged product decision.
	list := fixedList(map[string]AllowlistEntry{
		"guest.lecturer@gmail.com": {Role: RoleLecturer, Active: true},
	})
	c := Claims{Email: "guest.lecturer@gmail.com", EmailVerified: true, HostedDomain: ""}

	got := Authorize(c, "ntu.edu.tw", list)

	if !got.Authorized {
		t.Fatalf("expected allowlisted personal account to be authorized, got %q", got.Reason)
	}
	if got.Role != RoleLecturer {
		t.Fatalf("expected role %q, got %q", RoleLecturer, got.Role)
	}
}

func TestAuthorize_MatchesAllowlistCaseInsensitively(t *testing.T) {
	list := fixedList(map[string]AllowlistEntry{
		"b11902156@ntu.edu.tw": {Role: RoleAdmin, Active: true},
	})
	// Mixed-case + surrounding whitespace in the token email must still match.
	c := Claims{Email: "  B11902156@NTU.edu.TW ", EmailVerified: true, HostedDomain: "ntu.edu.tw"}

	got := Authorize(c, "ntu.edu.tw", list)

	if !got.Authorized {
		t.Fatalf("expected case-insensitive match to authorize, got %q", got.Reason)
	}
	if got.Role != RoleAdmin {
		t.Fatalf("expected role %q, got %q", RoleAdmin, got.Role)
	}
}
