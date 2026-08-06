package offline

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/HaoWen46/adagrade/internal/imaging"
)

// Kind is what a region contains. These are the server's id_regions kinds, so
// an id-regions JSON exported from a normal assessment describes the same three
// rectangles here.
type Kind string

const (
	KindStudentID Kind = "student_id"
	KindName      Kind = "name"
	KindProblemID Kind = "problem_id"
)

// kindOrder is the canonical iteration order: All and MaskRegions are stable
// across runs so the artifacts they feed diff cleanly.
var kindOrder = []Kind{KindStudentID, KindName, KindProblemID}

// Region is one normalized rectangle (fractions of page width/height), so it
// applies at any render DPI. Padding is grown on all four sides when masking.
type Region struct {
	Kind                Kind
	X, Y, W, H, Padding float64
}

// DefaultRegionPadding is the padding used when a region omits it: a little
// slack so a stroke that overshoots the drawn box still gets covered.
const DefaultRegionPadding = 0.004

// maxRegionPadding caps padding. Beyond a tenth of the page, padding is not
// slack any more — it is a second, invisible region.
const maxRegionPadding = 0.1

// regionsFileVersion is the only accepted "version" value. Bumping it is how a
// future incompatible schema announces itself instead of being misread.
const regionsFileVersion = 1

// RegionSet holds at most one region per kind.
//
// It has two shapes. A loaded set (LoadRegions) carries the rectangles someone
// actually drew. A banded set (BandRegions) carries a single top-strip
// rectangle that answers for every kind, which is the no-configuration
// fallback: we do not know where identity is, only that it is up top.
type RegionSet struct {
	regions map[Kind]Region
	banded  bool
}

// Get returns the region for kind. In band mode every kind resolves to the
// same band rectangle (stamped with the requested kind).
func (s RegionSet) Get(kind Kind) (Region, bool) {
	r, ok := s.regions[kind]
	return r, ok
}

// All returns every configured region in canonical kind order. In band mode it
// reports the same rectangle under each of the three kinds — use Banded to tell
// the shapes apart.
func (s RegionSet) All() []Region {
	var out []Region
	for _, kind := range kindOrder {
		if r, ok := s.regions[kind]; ok {
			out = append(out, r)
		}
	}
	return out
}

// Banded reports whether this set came from BandRegions (one shared region)
// rather than from an id-regions file.
func (s RegionSet) Banded() bool { return s.banded }

// MaskRegions is what gets painted over before a page leaves the machine: the
// student_id and name regions ONLY.
//
// The problem_id region is deliberately not masked (D66, mirroring
// scan.seedMaskRegions): masking exists to keep identity out of the provider
// request, and the problem number is not identity — the grader reads it off the
// page to know which problem the answer belongs to. Covering it would break
// grading to protect nothing.
//
// Color is left empty so imaging applies its default fill (#4a4a4a); a dark
// gray is preferred over pure black because some vision models over-attend to
// pure-black boxes.
//
// In band mode the shared rectangle is emitted ONCE: the whole top strip is
// covered, and repeating the same rect three times would only cost encode time.
func (s RegionSet) MaskRegions() []imaging.Region {
	if s.banded {
		if r, ok := s.regions[KindStudentID]; ok {
			return []imaging.Region{toImagingRegion(r)}
		}
		return nil
	}
	var out []imaging.Region
	for _, kind := range []Kind{KindStudentID, KindName} {
		if r, ok := s.regions[kind]; ok {
			out = append(out, toImagingRegion(r))
		}
	}
	return out
}

// unknownJSONField pulls the field name out of encoding/json's
// DisallowUnknownFields error ("json: unknown field \"color\"") so the message
// can name it. A future wording change in the stdlib only costs the name, not
// the rejection: the caller still fails, just with the generic message.
func unknownJSONField(err error) (string, bool) {
	rest, ok := strings.CutPrefix(err.Error(), "json: unknown field ")
	if !ok {
		return "", false
	}
	if name, uerr := strconv.Unquote(rest); uerr == nil {
		return name, true
	}
	return strings.Trim(rest, `"`), true
}

func toImagingRegion(r Region) imaging.Region {
	return imaging.Region{X: r.X, Y: r.Y, W: r.W, H: r.H, Padding: r.Padding}
}

// BandRegions builds the fallback set: one full-width strip across the top
// band of the page, serving as student_id, name and problem_id at once.
//
// band is already validated by ParseArgs (--id-band); it is clamped here anyway
// so no caller bug can produce a zero-height or off-page rectangle.
func BandRegions(band float64) RegionSet {
	switch {
	case band <= 0:
		band = DefaultIDBand
	case band > 1:
		band = 1
	}
	regions := make(map[Kind]Region, len(kindOrder))
	for _, kind := range kindOrder {
		regions[kind] = Region{Kind: kind, X: 0, Y: 0, W: 1, H: band, Padding: DefaultRegionPadding}
	}
	return RegionSet{regions: regions, banded: true}
}

// regionsFile is the on-disk schema (version 1):
//
//	{"version":1,"regions":[{"kind":"student_id","x":0.6,"y":0.02,"w":0.3,"h":0.06,"padding":0.004}, ...]}
//
// Padding is a pointer so an omitted padding (default) is distinguishable from
// an explicit 0 (no slack, which is legal).
//
// Unknown keys are rejected (see LoadRegions) rather than ignored, because x
// and y legally default to 0: a file written with the server's key names, or
// with any typo in "x"/"y", would otherwise decode into a plausible-looking
// region pinned to the top-left corner while the identity it was meant to
// cover stays unmasked.
type regionsFile struct {
	Version int `json:"version"`
	Regions []struct {
		Kind    Kind     `json:"kind"`
		X       float64  `json:"x"`
		Y       float64  `json:"y"`
		W       float64  `json:"w"`
		H       float64  `json:"h"`
		Padding *float64 `json:"padding"`
	} `json:"regions"`
}

// acceptedRegionKeys is the schema hint carried by unknown-field errors. An
// operator who reached for the server's shape needs to see what this command
// takes, not just that their file was refused.
const acceptedRegionKeys = `accepted keys are "version" and "regions", where each region takes "kind", "x", "y", "w", "h" and an optional "padding" (no "color": the mask fill is fixed)`

// LoadRegions reads and validates an id-regions JSON file. Every failure is a
// *RegionsError (exit 7) naming the offending region index and field, because
// coordinates are hand-edited and "invalid regions file" is unfixable advice.
//
// Validation is strict on purpose, including rejecting unknown keys and
// trailing data: a region that is silently clamped, dropped or half-understood
// would mask the wrong part of the page, and the operator would only find out
// by reading the masked preview after identity had already been sent.
func LoadRegions(path string) (RegionSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RegionSet{}, newRegionsError(err, "id-regions file %s does not exist: fix --id-regions, or drop it to fall back to the --id-band top strip", path)
		}
		return RegionSet{}, newRegionsError(err, "cannot read id-regions file %s", path)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var file regionsFile
	if err := dec.Decode(&file); err != nil {
		if field, ok := unknownJSONField(err); ok {
			return RegionSet{}, newRegionsError(err, "id-regions file %s has unknown field %q: %s (a mistyped coordinate key would leave the region in the page corner and the real identity unmasked)", path, field, acceptedRegionKeys)
		}
		return RegionSet{}, newRegionsError(err, "id-regions file %s is not valid JSON", path)
	}
	if dec.More() {
		return RegionSet{}, newRegionsError(nil, "id-regions file %s has trailing data after the top-level JSON object: it must hold exactly one object, %s", path, acceptedRegionKeys)
	}
	if file.Version != regionsFileVersion {
		return RegionSet{}, newRegionsError(nil, "id-regions file %s has version %d, want %d", path, file.Version, regionsFileVersion)
	}
	if len(file.Regions) == 0 {
		return RegionSet{}, newRegionsError(nil, "id-regions file %s lists no regions: it needs at least a %q or %q region", path, KindStudentID, KindName)
	}

	regions := make(map[Kind]Region, len(file.Regions))
	for i, r := range file.Regions {
		switch r.Kind {
		case KindStudentID, KindName, KindProblemID:
		default:
			return RegionSet{}, newRegionsError(nil, "id-regions file %s: regions[%d] has unknown kind %q, want %q, %q or %q", path, i, r.Kind, KindStudentID, KindName, KindProblemID)
		}
		if _, dup := regions[r.Kind]; dup {
			return RegionSet{}, newRegionsError(nil, "id-regions file %s: regions[%d] repeats kind %q, which may appear at most once", path, i, r.Kind)
		}
		if r.X < 0 {
			return RegionSet{}, newRegionsError(nil, "id-regions file %s: regions[%d] has x %g, want x >= 0 (fractions of page width)", path, i, r.X)
		}
		if r.Y < 0 {
			return RegionSet{}, newRegionsError(nil, "id-regions file %s: regions[%d] has y %g, want y >= 0 (fractions of page height)", path, i, r.Y)
		}
		if r.W <= 0 {
			return RegionSet{}, newRegionsError(nil, "id-regions file %s: regions[%d] has w %g, want w > 0", path, i, r.W)
		}
		if r.H <= 0 {
			return RegionSet{}, newRegionsError(nil, "id-regions file %s: regions[%d] has h %g, want h > 0", path, i, r.H)
		}
		if r.X+r.W > 1 {
			return RegionSet{}, newRegionsError(nil, "id-regions file %s: regions[%d] runs off the page: x %g + w %g > 1", path, i, r.X, r.W)
		}
		if r.Y+r.H > 1 {
			return RegionSet{}, newRegionsError(nil, "id-regions file %s: regions[%d] runs off the page: y %g + h %g > 1", path, i, r.Y, r.H)
		}
		padding := DefaultRegionPadding
		if r.Padding != nil {
			padding = *r.Padding
			if padding < 0 || padding > maxRegionPadding {
				return RegionSet{}, newRegionsError(nil, "id-regions file %s: regions[%d] has padding %g, want [0, %g]", path, i, padding, maxRegionPadding)
			}
		}
		regions[r.Kind] = Region{Kind: r.Kind, X: r.X, Y: r.Y, W: r.W, H: r.H, Padding: padding}
	}

	_, hasID := regions[KindStudentID]
	_, hasName := regions[KindName]
	if !hasID && !hasName {
		return RegionSet{}, newRegionsError(nil, "id-regions file %s has no %q or %q region: without one there is nothing to match pages on and nothing to mask", path, KindStudentID, KindName)
	}
	return RegionSet{regions: regions}, nil
}
