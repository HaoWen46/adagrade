package export

import (
	"strings"
	"unicode"
)

// maxSlugRunes bounds the archive's root directory name. The assessment name
// is operator-supplied free text with no length limit of its own, and every
// entry in the archive is prefixed with the slug — an unbounded name would
// push per-student paths past the 255-byte component limit on ext4/APFS and
// past MAX_PATH on Windows extraction.
const maxSlugRunes = 64

// slugFallback is used when nothing survives slugification. It must be
// non-empty: an empty slug would make every entry name start with "-p3/",
// which reads as a command-line flag to half the tooling that touches it.
const slugFallback = "assessment"

// Slug turns an operator-supplied assessment name into a directory name that
// is safe inside a ZIP and on every platform that will extract it: lowercased,
// every non-alphanumeric run collapsed to a single "-", trimmed, and
// length-bounded.
//
// NON-ASCII IS DROPPED, and that is a measured decision rather than an
// Anglocentric default — the corpus this feature targets is Traditional
// Chinese, so keeping CJK was the obvious first implementation. It was
// rejected on evidence: macOS ships Info-ZIP UnZip 6.00, which does not honour
// the UTF-8 name flag that archive/zip sets, and refuses the whole archive with
//
//	checkdir error: cannot create …/演算法-期中考-2-p2 — Illegal byte sequence
//
// Finder (ditto) and Windows Explorer both handle it, so the failure is a
// coin-flip on the extraction tool rather than an outright break — which is
// worse, because it is unreproducible from the professor's description. The
// same choice also sidesteps APFS's NFD normalisation, which silently rewrites
// non-ASCII path components on extraction.
//
// The cost is real and bounded: a purely zh-Hant assessment name slugs to
// "assessment", so the ROOT DIRECTORY is uninformative. The human-readable
// name belongs on the download filename instead, where RFC 5987 gives HTTP a
// properly specified UTF-8 encoding — that is the caller's layer, not this one.
// See TestLive_ArchiveExtractsWithSystemUnzip, which is what caught this.
//
// Slug is pure and idempotent: Slug(Slug(x)) == Slug(x).
func Slug(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	pendingDash := false
	for _, r := range name {
		if isASCIIAlphanumeric(r) {
			if pendingDash && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingDash = false
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		// Everything else — punctuation, spaces, separators, control
		// characters, path separators, dots, and every non-ASCII rune —
		// becomes a single collapsed dash, and only if something alphanumeric
		// follows it. Collapsing on write is what makes leading and trailing
		// trimming fall out for free.
		pendingDash = true
	}

	out := b.String()
	if out == "" {
		return slugFallback
	}
	if runes := []rune(out); len(runes) > maxSlugRunes {
		out = strings.TrimRight(string(runes[:maxSlugRunes]), "-")
		if out == "" {
			return slugFallback
		}
	}
	return out
}

func isASCIIAlphanumeric(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
