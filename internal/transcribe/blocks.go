// Package transcribe turns masked answer-page images into LaTeX source.
//
// The load-bearing design rule is that the vision model NEVER writes LaTeX. It
// emits a constrained block list (this file); deterministic Go code validates
// and renders it (latex.go). Everything after the model call is a pure function
// of the model's output, so re-exporting is free and byte-identical, and no
// model output ever reaches the TeX compiler unexamined.
//
// See docs/superpowers/specs/2026-07-25-latex-transcription-export-design.md.
package transcribe

// BlockKind enumerates the shapes a transcribed answer can take. Keeping this
// closed is what lets the emitter be exhaustive: an unrecognised kind is a
// programming error, not a passthrough.
type BlockKind string

const (
	// BlockProse is running text. It may embed inline math in $…$ spans; the
	// text is escaped and the math spans are validated (never escaped, since
	// escaping math would defeat the point of math mode).
	BlockProse BlockKind = "prose"
	// BlockMath is a display-math fragment, without delimiters.
	BlockMath BlockKind = "math"
	// BlockCode is pseudocode, reproduced verbatim. Layout carries grading
	// signal in an algorithms course, so indentation is preserved exactly.
	BlockCode BlockKind = "code"
	// BlockList is an enumerated sequence; each item is treated as prose.
	BlockList BlockKind = "list"
)

// Block is one unit of a transcribed answer.
type Block struct {
	Kind BlockKind `json:"kind"`
	// Text carries the content for prose, math, and code blocks.
	Text string `json:"text,omitempty"`
	// Items carries the content for list blocks.
	Items []string `json:"items,omitempty"`
}

// Doc is a complete transcription of one answer (one student, one problem,
// spanning however many pages the answer occupies).
type Doc struct {
	// Title is app-controlled (e.g. "Problem 2"), never student-derived.
	Title string `json:"title,omitempty"`
	// Blocks are rendered in order.
	Blocks []Block `json:"blocks"`
}
