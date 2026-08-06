package offline

import (
	"path/filepath"
	"testing"

	"github.com/HaoWen46/adagrade/internal/imaging"
)

const goodRegionsJSON = `{
  "version": 1,
  "regions": [
    {"kind": "student_id", "x": 0.60, "y": 0.02, "w": 0.30, "h": 0.06, "padding": 0.01},
    {"kind": "name",       "x": 0.10, "y": 0.02, "w": 0.30, "h": 0.06},
    {"kind": "problem_id", "x": 0.02, "y": 0.20, "w": 0.10, "h": 0.04}
  ]
}`

func TestLoadRegionsGood(t *testing.T) {
	path := writeFile(t, t.TempDir(), "regions.json", goodRegionsJSON)
	set, err := LoadRegions(path)
	if err != nil {
		t.Fatalf("LoadRegions error = %v, want nil", err)
	}
	if set.Banded() {
		t.Error("Banded() = true for a loaded region file, want false")
	}
	want := map[Kind]Region{
		KindStudentID: {Kind: KindStudentID, X: 0.60, Y: 0.02, W: 0.30, H: 0.06, Padding: 0.01},
		// Padding is optional and defaults rather than being 0.
		KindName:      {Kind: KindName, X: 0.10, Y: 0.02, W: 0.30, H: 0.06, Padding: DefaultRegionPadding},
		KindProblemID: {Kind: KindProblemID, X: 0.02, Y: 0.20, W: 0.10, H: 0.04, Padding: DefaultRegionPadding},
	}
	for kind, wantRegion := range want {
		got, ok := set.Get(kind)
		if !ok {
			t.Errorf("Get(%q) missing", kind)
			continue
		}
		if got != wantRegion {
			t.Errorf("Get(%q) = %+v, want %+v", kind, got, wantRegion)
		}
	}
	if all := set.All(); len(all) != 3 {
		t.Errorf("All() = %d regions, want 3", len(all))
	}
	if _, ok := (RegionSet{}).Get(KindName); ok {
		t.Error("zero RegionSet.Get(name) ok = true, want false")
	}
}

func TestLoadRegionsFailures(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		body string
		want []string // substrings: the offending region index and field
	}{
		{"not json", `{`, []string{"JSON"}},
		{"wrong version", `{"version":2,"regions":[{"kind":"name","x":0,"y":0,"w":0.5,"h":0.1}]}`, []string{"version", "2"}},
		{"missing version", `{"regions":[{"kind":"name","x":0,"y":0,"w":0.5,"h":0.1}]}`, []string{"version"}},
		{"no regions", `{"version":1,"regions":[]}`, []string{"regions"}},
		{
			"unknown kind",
			`{"version":1,"regions":[{"kind":"name","x":0,"y":0,"w":0.5,"h":0.1},{"kind":"seat","x":0,"y":0.5,"w":0.1,"h":0.1}]}`,
			[]string{"regions[1]", "kind", "seat"},
		},
		{
			"duplicate kind",
			`{"version":1,"regions":[{"kind":"name","x":0,"y":0,"w":0.5,"h":0.1},{"kind":"name","x":0,"y":0.5,"w":0.1,"h":0.1}]}`,
			[]string{"regions[1]", "name"},
		},
		{
			"only problem_id",
			`{"version":1,"regions":[{"kind":"problem_id","x":0,"y":0,"w":0.5,"h":0.1}]}`,
			[]string{"student_id", "name"},
		},
		{"negative x", `{"version":1,"regions":[{"kind":"name","x":-0.1,"y":0,"w":0.5,"h":0.1}]}`, []string{"regions[0]", "x"}},
		{"negative y", `{"version":1,"regions":[{"kind":"name","x":0,"y":-0.1,"w":0.5,"h":0.1}]}`, []string{"regions[0]", "y"}},
		{"zero w", `{"version":1,"regions":[{"kind":"name","x":0,"y":0,"w":0,"h":0.1}]}`, []string{"regions[0]", "w"}},
		{"zero h", `{"version":1,"regions":[{"kind":"name","x":0,"y":0,"w":0.5,"h":0}]}`, []string{"regions[0]", "h"}},
		{"x+w past the page", `{"version":1,"regions":[{"kind":"name","x":0.8,"y":0,"w":0.5,"h":0.1}]}`, []string{"regions[0]", "x", "w"}},
		{"y+h past the page", `{"version":1,"regions":[{"kind":"name","x":0,"y":0.95,"w":0.5,"h":0.1}]}`, []string{"regions[0]", "y", "h"}},
		{"padding too big", `{"version":1,"regions":[{"kind":"name","x":0,"y":0,"w":0.5,"h":0.1,"padding":0.2}]}`, []string{"regions[0]", "padding"}},
		{"padding negative", `{"version":1,"regions":[{"kind":"name","x":0,"y":0,"w":0.5,"h":0.1,"padding":-0.01}]}`, []string{"regions[0]", "padding"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, dir, "r.json", tc.body)
			set, err := LoadRegions(path)
			if len(set.All()) != 0 {
				t.Errorf("All() = %v, want empty set on failure", set.All())
			}
			assertErrorType[*RegionsError](t, err, tc.want...)
			if got := ExitCode(err); got != ExitRegions {
				t.Errorf("ExitCode = %d, want %d", got, ExitRegions)
			}
		})
	}
}

// TestLoadRegionsRejectsUnknownFields covers the failure that makes silent key
// tolerance dangerous here: x and y legally default to 0, so a mistyped key
// yields a plausible-looking region in the top-LEFT corner while the real
// student ID at top-right is never masked and goes to the provider in the
// clear. The file must be rejected, not partially understood.
func TestLoadRegionsRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			"mistyped coordinate keys",
			`{"version":1,"regions":[{"kind":"student_id","left":0.6,"top":0.02,"w":0.3,"h":0.06}]}`,
			[]string{"left"},
		},
		{
			// The server's own id-region rows carry a color and no version, so
			// operators will try that shape; the message has to show the one
			// this command accepts instead of just refusing.
			"server-shaped region with color",
			`{"version":1,"regions":[{"kind":"student_id","x":0.6,"y":0.02,"w":0.3,"h":0.06,"color":"#4a4a4a"}]}`,
			[]string{"color", "kind", "padding"},
		},
		{
			"unknown top-level key",
			`{"version":1,"page":"first","regions":[{"kind":"name","x":0,"y":0,"w":0.5,"h":0.1}]}`,
			[]string{"page"},
		},
		{
			"trailing data",
			`{"version":1,"regions":[{"kind":"name","x":0,"y":0,"w":0.5,"h":0.1}]}{"version":1}`,
			[]string{"trailing"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, dir, "r.json", tc.body)
			set, err := LoadRegions(path)
			if len(set.All()) != 0 {
				t.Errorf("All() = %v, want empty set on failure", set.All())
			}
			assertErrorType[*RegionsError](t, err, append([]string{path}, tc.want...)...)
			if got := ExitCode(err); got != ExitRegions {
				t.Errorf("ExitCode = %d, want %d", got, ExitRegions)
			}
		})
	}
}

func TestLoadRegionsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	_, err := LoadRegions(path)
	assertErrorType[*RegionsError](t, err, path)
}

func TestBandRegions(t *testing.T) {
	set := BandRegions(0.18)
	if !set.Banded() {
		t.Error("Banded() = false, want true")
	}
	// The one band region answers for ALL three kinds: without an id-regions
	// file we do not know where anything is, only that it is up top.
	for _, kind := range []Kind{KindStudentID, KindName, KindProblemID} {
		got, ok := set.Get(kind)
		if !ok {
			t.Errorf("Get(%q) missing", kind)
			continue
		}
		want := Region{Kind: kind, X: 0, Y: 0, W: 1, H: 0.18, Padding: DefaultRegionPadding}
		if got != want {
			t.Errorf("Get(%q) = %+v, want %+v", kind, got, want)
		}
	}
}

func TestBandRegionsClampsOutOfRangeBands(t *testing.T) {
	tests := []struct {
		band float64
		want float64
	}{
		{0.18, 0.18},
		{0, DefaultIDBand},
		{-1, DefaultIDBand},
		{1.5, 1},
		{1, 1},
	}
	for _, tc := range tests {
		got, ok := BandRegions(tc.band).Get(KindName)
		if !ok {
			t.Fatalf("BandRegions(%g).Get(name) missing", tc.band)
		}
		if got.H != tc.want {
			t.Errorf("BandRegions(%g) height = %g, want %g", tc.band, got.H, tc.want)
		}
	}
}

func TestMaskRegionsExcludesProblemID(t *testing.T) {
	path := writeFile(t, t.TempDir(), "regions.json", goodRegionsJSON)
	set, err := LoadRegions(path)
	if err != nil {
		t.Fatalf("LoadRegions error = %v", err)
	}
	got := set.MaskRegions()
	want := []imaging.Region{
		{X: 0.60, Y: 0.02, W: 0.30, H: 0.06, Padding: 0.01},
		{X: 0.10, Y: 0.02, W: 0.30, H: 0.06, Padding: DefaultRegionPadding},
	}
	if len(got) != len(want) {
		t.Fatalf("MaskRegions() = %d regions (%+v), want %d (student_id + name only, D66)", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("MaskRegions()[%d] = %+v, want %+v", i, got[i], want[i])
		}
		// Color is left empty on purpose: imaging falls back to #4a4a4a.
		if got[i].Color != "" {
			t.Errorf("MaskRegions()[%d].Color = %q, want \"\" (imaging default fill)", i, got[i].Color)
		}
	}
}

func TestMaskRegionsProblemIDOnlySetMasksNothing(t *testing.T) {
	// Not reachable through LoadRegions (which requires student_id or name),
	// but the D66 rule must hold for any set: problem_id is never masked.
	set := RegionSet{regions: map[Kind]Region{
		KindProblemID: {Kind: KindProblemID, X: 0, Y: 0, W: 0.1, H: 0.1},
	}}
	if got := set.MaskRegions(); len(got) != 0 {
		t.Errorf("MaskRegions() = %+v, want none", got)
	}
}

func TestMaskRegionsBandModeReturnsOneRegion(t *testing.T) {
	got := BandRegions(0.2).MaskRegions()
	want := []imaging.Region{{X: 0, Y: 0, W: 1, H: 0.2, Padding: DefaultRegionPadding}}
	if len(got) != 1 {
		t.Fatalf("MaskRegions() = %d regions (%+v), want 1 (the band, masked once)", len(got), got)
	}
	if got[0] != want[0] {
		t.Errorf("MaskRegions()[0] = %+v, want %+v", got[0], want[0])
	}
}

func TestZeroRegionSetMasksNothing(t *testing.T) {
	var set RegionSet
	if got := set.MaskRegions(); len(got) != 0 {
		t.Errorf("MaskRegions() = %+v, want none", got)
	}
	if set.Banded() {
		t.Error("Banded() = true, want false")
	}
	if got := set.All(); len(got) != 0 {
		t.Errorf("All() = %+v, want none", got)
	}
}
