// Package ocr declares the local-OCR seam (docs/DECISIONS.md D24): reading
// text lines from an ID-box crop with a small embedded model, entirely
// offline — no student identity ever leaves the machine. It sits beside the
// cloud-VLM path in scan identification as the preferred first engine; the
// implementation lives in internal/localocr behind this interface so the
// numeric dependency (onnxruntime) stays swappable and optional.
//
// The seam deliberately consumes imaging.IDCrop — the same sealed artifact
// the provider layer accepts (D19) — so local OCR cannot be pointed at
// arbitrary page images any more than a provider can.
package ocr

import (
	"context"

	"github.com/HaoWen46/adagrade/internal/imaging"
)

// Line is one recognized text line from a crop.
type Line struct {
	Text       string
	Confidence float64 // mean per-character probability in [0,1]
}

// Reader reads text lines from an ID-box crop without any network call.
// Implementations must be safe for concurrent use (River runs identify
// workers concurrently).
type Reader interface {
	ReadLines(ctx context.Context, crop imaging.IDCrop) ([]Line, error)
}
