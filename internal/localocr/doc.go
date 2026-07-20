// Package localocr is the fully-offline implementation of the ocr.Reader seam
// (docs/DECISIONS.md D24): it reads student-ID + Chinese-name text lines from a
// tight imaging.IDCrop using an embedded PP-OCR recognition model over
// onnxruntime, with no network call and no student identity ever leaving the
// machine.
//
// D24 chose a deliberately minimal recognition-ONLY pipeline — no text-detection
// model, no polygon clipping. The crop handed in is already a tight TA-drawn box
// holding 1–3 short lines, so line boundaries are recovered in pure Go by a
// horizontal ink-density projection (split.go) rather than a learned detector.
// Each band is preprocessed to the model's fixed height (preprocess.go), run
// through the ch_PP-OCRv4 mobile recognizer, and CTC-decoded greedily (ctc.go).
//
// The numeric dependency (github.com/yalue/onnxruntime_go, which dlopens a
// libonnxruntime shared library) is isolated to engine.go so the whole package
// stays optional: a build that never constructs an Engine pulls the runtime in
// but never touches the shared library.
//
// Privacy (D14): this package MUST NOT log recognized text, confidences tied to
// text, or any crop bytes — those are student PII. Errors describe mechanism and
// file paths only, never content.
package localocr
