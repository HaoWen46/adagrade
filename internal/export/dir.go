package export

// A directory VIEW of the bundle, not a second bundle format.
//
// The offline CLI has no HTTP response to stream a ZIP into: it writes the
// transcription bundle straight to a directory the professor already has open.
// That must not become a second, subtly-different bundle — a directory whose
// _all.tex differs from the download's would quietly split the one artifact
// this package exists to make reproducible.
//
// So this file adds no assembly of its own. archiveEntries (export.go) decides
// what a bundle contains, including every validation and the size preflight;
// BuildZIP adds ZIP framing to that list and Files strips the framing off. The
// equality is pinned mechanically in dir_test.go by unzipping real BuildZIP
// output and comparing names, bodies and order.

// File is one bundle entry as BuildZIP would write it: the ZIP-internal path
// and the bytes, with no ZIP framing.
//
// Name is slash-separated and carries the bundle root as its first path
// element, e.g. "algorithms-midterm-2-p3/tex/b09901002.tex". A caller writing a
// directory joins it onto its destination with filepath.FromSlash; every name
// is pure ASCII and has no "." or ".." element, by the same Slug and
// student-id rules that make it safe as a ZIP entry name.
//
// The privacy invariant holds here exactly as it does for the archive: identity
// lives in Name, never in Body — except MANIFEST.csv, the documented decoder
// ring (see this package's doc comment).
type File struct {
	Name string
	Body []byte
}

// Files returns the bundle entries BuildZIP(in) would produce, in the same
// order and with the same bytes, without the ZIP container. It is for callers
// that write the bundle as a directory instead of an archive.
//
// It validates and preflights exactly as BuildZIP does — the same inputs are
// rejected with the same errors, so the directory path is not a lenient door
// into the bundle. The ONE check it cannot share is BuildZIP's final
// post-compression size assertion, which is a property of the archive rather
// than of the entries; the raw page-image preflight against MaxZipBytes, which
// dominates that total, does apply.
//
// Body for an image entry aliases the masked JPEG the caller supplied in the
// corresponding Answer, so it must be treated as read-only.
func Files(in Input) ([]File, error) {
	entries, err := archiveEntries([]Input{in}, "")
	if err != nil {
		return nil, err
	}
	out := make([]File, 0, len(entries))
	for _, e := range entries {
		out = append(out, File{Name: e.name, Body: e.body})
	}
	return out, nil
}

// ImageName is the bundle's image naming rule: "{id}.jpg" for a single-page
// answer, "{id}-p{n}.jpg" (1-based) when a page number is needed to
// disambiguate. pageIdx is 0-based.
//
// Exported for callers that must name an image before — or without — building
// the bundle, e.g. to reference a page from a report. It is the same namer the
// entries use, so a predicted path and the written file can never disagree.
func ImageName(studentID string, pageIdx, pageCount int) string {
	return imageName(studentID, pageIdx, pageCount)
}
