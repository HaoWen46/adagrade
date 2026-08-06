// Package offline implements `adamarker offline-grade`: grading scanned exams
// from a terminal when the server is unavailable. It touches no database and
// serves no HTTP — files in, files out — which is exactly why its guarantees
// are weaker than the normal pipeline's. This doc is the contract; the banner
// (banner.go) is the same contract stated to the operator at run time.
//
// Pages are FORCE-MATCHED to roster entries without human review. The normal
// pipeline sends a page it cannot confidently identify to an orphan queue for a
// TA to resolve; here there is no queue and nobody to ask, so the solver
// assigns every page it can and the run continues. Pages below the score or
// margin thresholds are set aside as unmatched rather than guessed at, but the
// pages that ARE matched carry no confirmation beyond a score in the report.
// Wrong assignments are an expected outcome, not an edge case.
//
// Masking is best-effort and fully automatic. It covers the identity regions
// the operator configured (--id-regions) or, with no configuration, the top
// --id-band strip of every page. Nothing detects identity written anywhere
// else: a name in a margin, on the back, or inside an answer is not covered.
// The problem_id region is deliberately never masked (D66) — the grader needs
// the problem number visible, and a problem number is not identity.
//
// Only masked images reach the LLM API. Masking runs before any request, and
// the transcription stage accepts only imaging.MaskedImage, whose unexported
// fields make sending an unmasked page a compile error rather than a review
// comment (D10). The roster, the scans and every intermediate stay on the
// machine; the masked page images are the only bytes that leave it.
//
// The artifacts are the audit trail, and they are the whole audit trail — there
// are no database rows, no audit log and no UI to look back through.
// match-report.csv records who each page was assigned to and how confident the
// match was; masked-preview.jpg shows what the model actually saw where
// identity used to be; unmatched/ holds the pages nobody could place, with a
// reason. Output that has not been checked against these is unverified.
//
// Failures are typed (flags.go) and map to stable exit codes through ExitCode,
// so a script wrapping this command can tell a bad roster (3) from an
// unreachable provider (8) from a run where nothing matched at all (9).
package offline
