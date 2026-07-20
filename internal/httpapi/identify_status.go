// Identify-status endpoint (privacy loudness, 2026-07-12). The masking
// feature exists to keep student identity off cloud providers, yet the
// identify pipeline's privacy-preserving first rung — local OCR (D24) — can be
// silently absent (unset env, missing model file). The Identify tab reads this
// endpoint to tell the operator, right where uploads happen, that ID/name
// crops will go to a cloud provider unless local OCR is installed.
package httpapi

import "net/http"

// handleIdentifyStatus reports whether the local OCR rung is available on this
// server. Any signed-in role may read it (TAs work the Identify tab too).
// Booleans only — the model/keys/onnxruntime locations are operator config and
// are deliberately never exposed here.
func (s *Server) handleIdentifyStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"local_ocr_available": s.scans != nil && s.scans.Local != nil,
	})
}
