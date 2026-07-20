// Package llm defines the VisionProvider seam (spec §2, docs/DECISIONS.md D11):
// the provider-agnostic request/response types and the Provider interface that
// every vision-LLM adapter implements.
//
// Implementations live in sub-packages (internal/llm/anthropiccompat,
// internal/llm/fake) and are wired up from config by internal/llm/registry.
// Keeping this package implementation-free avoids an import cycle: adapters
// import llm, and only the registry imports the adapters.
package llm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/HaoWen46/adagrade/internal/imaging"
)

// Request is one grading call. Images accepts ONLY imaging.ProviderImage — a
// sealed interface implemented solely by imaging.MaskedImage and
// imaging.IDCrop (docs/DECISIONS.md D10, D19): a provider request carries
// identity XOR answer content, never an arbitrary unmasked page.
type Request struct {
	System         string
	Prompt         string
	Images         []imaging.ProviderImage
	Schema         []byte  // JSON Schema the structured output must satisfy
	Temperature    float64 // 0 = deterministic default (D-H2)
	MaxTokens      int     // 0 => 4096
	ReasoningLevel string  // "off"|"low"|"medium"|"high" (mapped per provider; v0 adapters may ignore)
	// ToolName is the forced structured-output tool's name. Empty ⇒ the grading
	// default "submit_grade"; the identification path (D19) passes
	// "submit_identity" so a request's tool intent is self-describing. It only
	// affects the tool/function name sent and matched — the schema still governs
	// the output shape (spec §5).
	ToolName string
}

// Result is a completed call.
type Result struct {
	JSON         []byte // the structured output (forced tool input, or extracted)
	RawText      string // any freeform text the model emitted alongside
	Model        string // resolved model id
	InputTokens  int
	OutputTokens int
}

// RateLimitError signals HTTP 429; RetryAfter may be zero when the server gave none.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited (retry after %s)", e.RetryAfter)
}

// ErrNoStructuredOutput means the model reply contained no parseable JSON payload.
var ErrNoStructuredOutput = errors.New("llm: response contained no structured output")

// Provider is the VisionProvider seam (spec §2).
type Provider interface {
	Name() string
	Grade(ctx context.Context, model string, req Request) (Result, error)
}
