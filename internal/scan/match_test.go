package scan

import "testing"

// ---- NormalizeID ----

func TestNormalizeID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already normalized", "B10902066", "B10902066"},
		{"lowercase folds to upper", "b10902066", "B10902066"},
		{"full-width digits and letter fold to ASCII", "ｂ１０９０２０６６", "B10902066"},
		{"hyphen stripped", "B109-02066", "B10902066"},
		{"internal whitespace stripped", "B109 02066", "B10902066"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeID(tc.in); got != tc.want {
				t.Errorf("NormalizeID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---- NormalizeName ----

func TestNormalizeName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"CJK name with ASCII spaces", "王 小 明", "王小明"},
		{"CJK name with ideographic space U+3000", "王　小　明", "王小明"},
		{"CJK name no spaces", "王小明", "王小明"},
		{"Latin name case-folds", "John Smith", "johnsmith"},
		{"Latin name mixed case and spaces", "  JoHn   SMITH ", "johnsmith"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeName(tc.in); got != tc.want {
				t.Errorf("NormalizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---- levenshtein ----

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want int
	}{
		{"identical", "B10902066", "B10902066", 0},
		{"one substitution", "B10902066", "B10902067", 1},
		{"one insertion", "B1090206", "B10902066", 1},
		{"one deletion", "B10902066", "B1090206", 1},
		{"empty a", "", "abc", 3},
		{"empty b", "abc", "", 3},
		{"both empty", "", "", 0},
		{"multi-byte CJK counts per rune", "王小明", "王小華", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := levenshtein(tc.a, tc.b); got != tc.want {
				t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// ---- matchOCRID ----

func TestMatchOCRID(t *testing.T) {
	roster := []RosterEntry{
		{ID: 1, ExternalID: "B10902066", Name: "王小明"},
		{ID: 2, ExternalID: "B10902067", Name: "李小華"},
	}

	t.Run("exact match", func(t *testing.T) {
		id, ok := matchOCRID("B10902066", roster)
		if !ok || id != 1 {
			t.Fatalf("matchOCRID() = (%d, %v), want (1, true)", id, ok)
		}
	})

	t.Run("full-width normalizes and matches", func(t *testing.T) {
		id, ok := matchOCRID("ｂ１０９０２０６７", roster)
		if !ok || id != 2 {
			t.Fatalf("matchOCRID() = (%d, %v), want (2, true)", id, ok)
		}
	})

	t.Run("no match", func(t *testing.T) {
		id, ok := matchOCRID("totally-unrecognizable-garbage", roster)
		if ok {
			t.Fatalf("matchOCRID() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		id, ok := matchOCRID("", roster)
		if ok {
			t.Fatalf("matchOCRID() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("empty roster", func(t *testing.T) {
		id, ok := matchOCRID("B10902066", nil)
		if ok {
			t.Fatalf("matchOCRID() = (%d, %v), want (_, false)", id, ok)
		}
	})

	// Two roster rows whose external ids collide under NormalizeID (possible when the
	// roster predates the import-time normalization guard): resolving to the FIRST hit
	// would silently attach pages to whichever row happens to come first. Ambiguity
	// must refuse to match, mirroring matchOCRName's duplicate-name posture.
	t.Run("two roster entries share a normalized id -> ambiguous, no match", func(t *testing.T) {
		dup := []RosterEntry{
			{ID: 1, ExternalID: "B10902066", Name: "王小明"},
			{ID: 2, ExternalID: "b10902066", Name: "李小華"}, // same id after normalization, on purpose
		}
		id, ok := matchOCRID("B10902066", dup)
		if ok {
			t.Fatalf("matchOCRID() = (%d, %v), want (_, false) (ambiguous normalized id)", id, ok)
		}
	})

	t.Run("normalized duplicate elsewhere in roster does not block a unique match", func(t *testing.T) {
		mixed := []RosterEntry{
			{ID: 1, ExternalID: "B10902066", Name: "王小明"},
			{ID: 2, ExternalID: "b10902066", Name: "李小華"}, // collides with ID 1, not with the probe below
			{ID: 3, ExternalID: "B10902067", Name: "陳大文"},
		}
		id, ok := matchOCRID("B10902067", mixed)
		if !ok || id != 3 {
			t.Fatalf("matchOCRID() = (%d, %v), want (3, true)", id, ok)
		}
	})
}

// MatchStudent with an ambiguous normalized id: the ID rung refuses, so resolution
// falls through to the name rung exactly like an unreadable ID box.
func TestMatchStudent_AmbiguousNormalizedID_FallsThroughToName(t *testing.T) {
	dup := []RosterEntry{
		{ID: 1, ExternalID: "B10902066", Name: "王小明"},
		{ID: 2, ExternalID: "b10902066", Name: "李小華"},
	}
	got := MatchStudent("B10902066", "李小華", dup)
	want := StudentMatch{ProposedID: 2, ProposalSource: "ocr_name"}
	if got != want {
		t.Fatalf("MatchStudent() = %+v, want %+v (ambiguous id must never auto-assign)", got, want)
	}
	if got2 := MatchStudent("B10902066", "", dup); got2.AgreedID != 0 || got2.ProposedID != 0 {
		t.Fatalf("MatchStudent() with ambiguous id and no name = %+v, want no proposal", got2)
	}
}

// ---- matchOCRName ----

func TestMatchOCRName(t *testing.T) {
	roster := []RosterEntry{
		{ID: 1, ExternalID: "B10902066", Name: "王小明"},
		{ID: 2, ExternalID: "B10902067", Name: "李小華"},
		{ID: 4, ExternalID: "B10902069", Name: "王小明"}, // duplicate name with ID 1, on purpose
	}

	t.Run("unique exact match", func(t *testing.T) {
		id, ok := matchOCRName("李小華", roster)
		if !ok || id != 2 {
			t.Fatalf("matchOCRName() = (%d, %v), want (2, true)", id, ok)
		}
	})

	t.Run("CJK name with ASCII spaces normalizes and matches", func(t *testing.T) {
		id, ok := matchOCRName("李 小 華", roster)
		if !ok || id != 2 {
			t.Fatalf("matchOCRName() = (%d, %v), want (2, true)", id, ok)
		}
	})

	t.Run("duplicate names in roster -> ambiguous, no match", func(t *testing.T) {
		id, ok := matchOCRName("王小明", roster)
		if ok {
			t.Fatalf("matchOCRName() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("no match", func(t *testing.T) {
		id, ok := matchOCRName("不存在的名字", roster)
		if ok {
			t.Fatalf("matchOCRName() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		id, ok := matchOCRName("", roster)
		if ok {
			t.Fatalf("matchOCRName() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("blank name never matches a roster student with a blank name", func(t *testing.T) {
		blankRoster := []RosterEntry{{ID: 1, ExternalID: "B10902099", Name: ""}}
		id, ok := matchOCRName("   ", blankRoster)
		if ok {
			t.Fatalf("matchOCRName() = (%d, %v), want (_, false) (blank name must never match)", id, ok)
		}
	})
}

// ---- matchOCRIDNear ----

func TestMatchOCRIDNear(t *testing.T) {
	roster := []RosterEntry{
		{ID: 1, ExternalID: "B11902001", Name: "王小明"},
		{ID: 2, ExternalID: "B11902002", Name: "李大華"},
	}

	t.Run("unique distance-1 hit", func(t *testing.T) {
		// "A11902001" is distance 1 from B11902001 and distance 2 from
		// B11902002 -> unambiguous near miss.
		id, ok := matchOCRIDNear("A11902001", roster)
		if !ok || id != 1 {
			t.Fatalf("matchOCRIDNear() = (%d, %v), want (1, true)", id, ok)
		}
	})

	t.Run("normalization applies to both sides", func(t *testing.T) {
		// full-width "ａ１１９０２００１" folds to A11902001 -> distance 1.
		id, ok := matchOCRIDNear("ａ１１９０２００１", roster)
		if !ok || id != 1 {
			t.Fatalf("matchOCRIDNear() = (%d, %v), want (1, true)", id, ok)
		}
	})

	t.Run("two candidates at distance 1 -> ambiguous, no match", func(t *testing.T) {
		// "B11902003" is distance 1 from BOTH roster IDs.
		id, ok := matchOCRIDNear("B11902003", roster)
		if ok {
			t.Fatalf("matchOCRIDNear() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("distance 2 -> no match", func(t *testing.T) {
		id, ok := matchOCRIDNear("A11902005", roster)
		if ok {
			t.Fatalf("matchOCRIDNear() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("exact hit is the exact rung's business, never a near miss", func(t *testing.T) {
		// Called standalone with an exact roster ID, matchOCRIDNear must not
		// return the OTHER student one edit away.
		id, ok := matchOCRIDNear("B11902001", roster)
		if ok {
			t.Fatalf("matchOCRIDNear() = (%d, %v), want (_, false) (exact hit)", id, ok)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		id, ok := matchOCRIDNear("", roster)
		if ok {
			t.Fatalf("matchOCRIDNear() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("empty roster", func(t *testing.T) {
		id, ok := matchOCRIDNear("A11902001", nil)
		if ok {
			t.Fatalf("matchOCRIDNear() = (%d, %v), want (_, false)", id, ok)
		}
	})
}

// ---- matchOCRIDEmbedded ----

func TestMatchOCRIDEmbedded(t *testing.T) {
	roster := []RosterEntry{
		{ID: 1, ExternalID: "B11902003", Name: "王小明"},
		{ID: 2, ExternalID: "B11902004", Name: "李大華"},
	}

	t.Run("leading junk around a verbatim roster ID", func(t *testing.T) {
		// Live-observed shape: a spurious leading character glued onto an
		// otherwise-perfect read ("1B11902003" for B11902003).
		id, ok := matchOCRIDEmbedded("1B11902003", roster)
		if !ok || id != 1 {
			t.Fatalf("matchOCRIDEmbedded() = (%d, %v), want (1, true)", id, ok)
		}
	})

	t.Run("trailing junk around a verbatim roster ID", func(t *testing.T) {
		id, ok := matchOCRIDEmbedded("B119020031", roster)
		if !ok || id != 1 {
			t.Fatalf("matchOCRIDEmbedded() = (%d, %v), want (1, true)", id, ok)
		}
	})

	t.Run("junk on both sides of a verbatim roster ID", func(t *testing.T) {
		id, ok := matchOCRIDEmbedded("XB11902003Y", roster)
		if !ok || id != 1 {
			t.Fatalf("matchOCRIDEmbedded() = (%d, %v), want (1, true)", id, ok)
		}
	})

	t.Run("normalization applies before the substring check", func(t *testing.T) {
		// full-width "１ｂ１１９０２００３" folds to 1B11902003.
		id, ok := matchOCRIDEmbedded("１ｂ１１９０２００３", roster)
		if !ok || id != 1 {
			t.Fatalf("matchOCRIDEmbedded() = (%d, %v), want (1, true)", id, ok)
		}
	})

	t.Run("two roster IDs embedded -> ambiguous, no match", func(t *testing.T) {
		// A glued double-read containing both roster IDs verbatim.
		id, ok := matchOCRIDEmbedded("B11902003B11902004", roster)
		if ok {
			t.Fatalf("matchOCRIDEmbedded() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("exact roster hit is the exact rung's business, never embedded", func(t *testing.T) {
		id, ok := matchOCRIDEmbedded("B11902003", roster)
		if ok {
			t.Fatalf("matchOCRIDEmbedded() = (%d, %v), want (_, false) (exact hit)", id, ok)
		}
	})

	t.Run("off-roster ID with junk prefix -> no match", func(t *testing.T) {
		// Live-observed shape: "CB99999999" wraps B99999999, which is NOT on
		// the roster — must refuse, exactly as the live run did.
		id, ok := matchOCRIDEmbedded("CB99999999", roster)
		if ok {
			t.Fatalf("matchOCRIDEmbedded() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		id, ok := matchOCRIDEmbedded("", roster)
		if ok {
			t.Fatalf("matchOCRIDEmbedded() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("empty roster", func(t *testing.T) {
		id, ok := matchOCRIDEmbedded("1B11902003", nil)
		if ok {
			t.Fatalf("matchOCRIDEmbedded() = (%d, %v), want (_, false)", id, ok)
		}
	})

	// Degenerate roster: a roster ID that is itself a substring of another
	// roster ID. A junk-wrapped read containing the short ID could equally be
	// a partial/corrupted read of the longer ID -> refuse.
	t.Run("contained ID is a substring of a longer roster ID -> refuse", func(t *testing.T) {
		degenerate := []RosterEntry{
			{ID: 1, ExternalID: "B0100", Name: "王小明"},
			{ID: 2, ExternalID: "AB0100C", Name: "李大華"},
		}
		// "XB0100" contains B0100 uniquely, but B0100 is a prefix-embedded
		// substring of AB0100C — the read could be a partial read of ID 2.
		id, ok := matchOCRIDEmbedded("XB0100", degenerate)
		if ok {
			t.Fatalf("matchOCRIDEmbedded() = (%d, %v), want (_, false) (degenerate roster)", id, ok)
		}
		// "AB0100" is literally AB0100C missing its last character AND
		// junk+B0100 — hopelessly ambiguous, must refuse.
		id, ok = matchOCRIDEmbedded("AB0100", degenerate)
		if ok {
			t.Fatalf("matchOCRIDEmbedded() = (%d, %v), want (_, false) (degenerate roster)", id, ok)
		}
	})

	t.Run("read exactly equals the longer of two nested roster IDs -> refuse (exact rung's business)", func(t *testing.T) {
		degenerate := []RosterEntry{
			{ID: 1, ExternalID: "B0100", Name: "王小明"},
			{ID: 2, ExternalID: "AB0100C", Name: "李大華"},
		}
		id, ok := matchOCRIDEmbedded("AB0100C", degenerate)
		if ok {
			t.Fatalf("matchOCRIDEmbedded() = (%d, %v), want (_, false) (exact hit for ID 2)", id, ok)
		}
	})
}

// ---- matchOCRIDFar (gated distance-2) ----

func TestMatchOCRIDFar(t *testing.T) {
	// Sparse roster: the two IDs are far apart, so a d-2 read off one of them
	// has a uniqueness gap >= 2 to everything else.
	sparse := []RosterEntry{
		{ID: 1, ExternalID: "B11902001", Name: "王小明"},
		{ID: 2, ExternalID: "C21813555", Name: "李大華"},
	}

	t.Run("unique distance-2 hit with gap >= 2", func(t *testing.T) {
		// "A1190200X": d=2 from B11902001, far beyond d=3 from C21813555.
		id, ok := matchOCRIDFar("A1190200X", sparse)
		if !ok || id != 1 {
			t.Fatalf("matchOCRIDFar() = (%d, %v), want (1, true)", id, ok)
		}
	})

	t.Run("normalization applies to both sides", func(t *testing.T) {
		id, ok := matchOCRIDFar("ａ１１９０２００Ｘ", sparse)
		if !ok || id != 1 {
			t.Fatalf("matchOCRIDFar() = (%d, %v), want (1, true)", id, ok)
		}
	})

	t.Run("exact hit refuses (exact rung's business)", func(t *testing.T) {
		id, ok := matchOCRIDFar("B11902001", sparse)
		if ok {
			t.Fatalf("matchOCRIDFar() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("distance-1 hit refuses (near rung's business)", func(t *testing.T) {
		id, ok := matchOCRIDFar("A11902001", sparse)
		if ok {
			t.Fatalf("matchOCRIDFar() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("distance 3 from everything -> no match", func(t *testing.T) {
		id, ok := matchOCRIDFar("AX190200X", sparse)
		if ok {
			t.Fatalf("matchOCRIDFar() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("unique d-2 but another roster ID within d-3 -> gap too small, refuse", func(t *testing.T) {
		gapRoster := []RosterEntry{
			{ID: 1, ExternalID: "B11902001", Name: "王小明"},
			{ID: 2, ExternalID: "B11902002", Name: "李大華"},
		}
		// "AX1902001": d=2 from B11902001, d=3 from B11902002 — only ONE
		// candidate at d-2, but the runner-up is within d-3, so the read is
		// too deep in a crowded neighborhood to trust.
		id, ok := matchOCRIDFar("AX1902001", gapRoster)
		if ok {
			t.Fatalf("matchOCRIDFar() = (%d, %v), want (_, false) (gap < 2)", id, ok)
		}
	})

	t.Run("sequential roster virtually always refuses", func(t *testing.T) {
		// B11902001..B11902010: real rosters are sequential, IDs mutually at
		// d 1-2. Any read within d-2 of one of them is within d-3 (usually
		// d-2) of its neighbors -> the gap gate must refuse.
		seq := make([]RosterEntry, 0, 10)
		for i := 1; i <= 10; i++ {
			seq = append(seq, RosterEntry{
				ID:         int64(i),
				ExternalID: "B1190200" + string(rune('0'+i%10)),
				Name:       "學生" + string(rune('0'+i%10)),
			})
		}
		// Fix the 10th entry to the realistic sequential form.
		seq[9].ExternalID = "B11902010"
		probes := []string{
			"A1190200X",  // d=2 from B11902001..B11902009 (several)
			"AX1902001",  // d=2 from B11902001, d=3 from most neighbors
			"B11902XX1",  // d=2 from B11902001, d<=3 from neighbors
			"BX19020022", // insertion+sub shapes
		}
		for _, probe := range probes {
			if id, ok := matchOCRIDFar(probe, seq); ok {
				t.Fatalf("matchOCRIDFar(%q, sequential) = (%d, %v), want (_, false)", probe, id, ok)
			}
		}
	})

	t.Run("empty input", func(t *testing.T) {
		id, ok := matchOCRIDFar("", sparse)
		if ok {
			t.Fatalf("matchOCRIDFar() = (%d, %v), want (_, false)", id, ok)
		}
	})

	t.Run("empty roster", func(t *testing.T) {
		id, ok := matchOCRIDFar("A1190200X", nil)
		if ok {
			t.Fatalf("matchOCRIDFar() = (%d, %v), want (_, false)", id, ok)
		}
	})
}

// TestMatchCallerFiltersRoster documents that RosterEntry has no withdrawn
// flag: the caller (the service layer) is responsible for excluding withdrawn
// students before calling matchOCRID/matchOCRName. Here we just confirm that a
// student absent from the roster slice is never proposed, even with an exact
// ID hit on what would otherwise have matched.
func TestMatchCallerFiltersRoster(t *testing.T) {
	roster := []RosterEntry{
		{ID: 2, ExternalID: "B10902067", Name: "李小華"},
	}
	id, ok := matchOCRID("B10902066", roster)
	if ok {
		t.Fatalf("matchOCRID() = (%d, %v), want (_, false) (student not in roster slice)", id, ok)
	}
}

// ---- MatchStudent ----

func TestMatchStudent(t *testing.T) {
	roster := []RosterEntry{
		{ID: 1, ExternalID: "B11902001", Name: "王小明"},
		{ID: 2, ExternalID: "B11902002", Name: "李大華"},
		{ID: 3, ExternalID: "B11902003", Name: "陳同名"},
		{ID: 4, ExternalID: "B11902004", Name: "陳同名"}, // duplicate name: name rung must fail
	}
	cases := []struct {
		name string
		id   string
		nm   string
		want StudentMatch
	}{
		{"agree", "B11902001", "王小明", StudentMatch{AgreedID: 1, ProposedID: 1, ProposalSource: "ocr_agree"}},
		{"agree with noise", " b11902001 ", "王 小 明", StudentMatch{AgreedID: 1, ProposedID: 1, ProposalSource: "ocr_agree"}},
		{"id only (name illegible)", "B11902002", "", StudentMatch{ProposedID: 2, ProposalSource: "ocr_id"}},
		{"id only (name unknown)", "B11902002", "無此人", StudentMatch{ProposedID: 2, ProposalSource: "ocr_id"}},
		{"name only", "", "李大華", StudentMatch{ProposedID: 2, ProposalSource: "ocr_name"}},
		{"disagreement", "B11902001", "李大華", StudentMatch{ProposalSource: "ocr_disagree"}},
		{"duplicate name never matches", "B11902003", "陳同名", StudentMatch{ProposedID: 3, ProposalSource: "ocr_id"}},
		{"one digit off never matches", "B11902009", "王小明", StudentMatch{ProposedID: 1, ProposalSource: "ocr_name"}},
		{"nothing", "", "", StudentMatch{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchStudent(tc.id, tc.nm, roster)
			if got != tc.want {
				t.Fatalf("MatchStudent(%q, %q) = %+v, want %+v", tc.id, tc.nm, got, tc.want)
			}
		})
	}
}

// TestMatchStudentNearMiss pins the near-miss rung: an OCR'd ID with no exact
// roster hit that is edit distance 1 from EXACTLY ONE active roster ID becomes
// a proposal ("ocr_id_near") for the orphan queue — never an auto-assign
// (AgreedID stays 0 in every case below; auto-assign remains exact-ID+name
// agreement only, the wrong-student firewall).
func TestMatchStudentNearMiss(t *testing.T) {
	roster := []RosterEntry{
		{ID: 1, ExternalID: "B11902001", Name: "王小明"},
		{ID: 2, ExternalID: "B11902002", Name: "李大華"},
	}
	cases := []struct {
		name string
		id   string
		nm   string
		want StudentMatch
	}{
		// "A11902001" is distance 1 from B11902001 only (distance 2 from
		// B11902002: first char AND last digit differ).
		{"unique near miss, no name -> proposal only",
			"A11902001", "", StudentMatch{ProposedID: 1, ProposalSource: "ocr_id_near"}},
		{"unique near miss, unknown name -> proposal only",
			"A11902001", "無此人", StudentMatch{ProposedID: 1, ProposalSource: "ocr_id_near"}},
		// The name agreeing with the near-miss student upgrades confidence in
		// the PROPOSAL — it must never upgrade into AgreedID.
		{"near miss + matching name -> still proposal only, never auto-assign",
			"A11902001", "王小明", StudentMatch{ProposedID: 1, ProposalSource: "ocr_id_near"}},
		// An exact, unique name hit on a DIFFERENT student beats the fuzzy ID
		// signal: propose the name match exactly as before near-miss existed.
		{"near miss vs exact name on another student -> name proposal wins",
			"A11902001", "李大華", StudentMatch{ProposedID: 2, ProposalSource: "ocr_name"}},
		// "B11902003" is distance 1 from BOTH roster IDs -> ambiguous, no
		// near proposal at all.
		{"two candidates at distance 1 -> no proposal",
			"B11902003", "", StudentMatch{}},
		{"two candidates at distance 1 but name resolves -> name proposal",
			"B11902003", "王小明", StudentMatch{ProposedID: 1, ProposalSource: "ocr_name"}},
		// distance >= 2 from everyone -> garbage, no proposal.
		{"distance 2 -> no proposal",
			"A11902005", "", StudentMatch{}},
		// Near-miss normalizes both sides (full-width folds, distance 1).
		{"full-width near miss normalizes",
			"ａ１１９０２００１", "", StudentMatch{ProposedID: 1, ProposalSource: "ocr_id_near"}},
		// Exact behavior unchanged by the near-miss rung existing.
		{"exact ID hit stays ocr_id",
			"B11902001", "", StudentMatch{ProposedID: 1, ProposalSource: "ocr_id"}},
		{"exact agreement stays ocr_agree",
			"B11902001", "王小明", StudentMatch{AgreedID: 1, ProposedID: 1, ProposalSource: "ocr_agree"}},
		{"exact disagreement stays ocr_disagree",
			"B11902001", "李大華", StudentMatch{ProposalSource: "ocr_disagree"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchStudent(tc.id, tc.nm, roster)
			if got != tc.want {
				t.Fatalf("MatchStudent(%q, %q) = %+v, want %+v", tc.id, tc.nm, got, tc.want)
			}
			if got.AgreedID != 0 && tc.want.AgreedID == 0 {
				t.Fatalf("near-miss rung auto-assigned: %+v", got)
			}
		})
	}
}

// TestMatchStudentEmbeddedAndFar pins the two stronger-evidence proposal
// rungs added above distance-1: a UNIQUE roster ID embedded verbatim in the
// normalized read (box-edge junk around a perfect read), and a gated unique
// distance-2 hit. Both reuse the "ocr_id_near" source (migration 0033's CHECK
// constraint allows no new values) and are proposals ONLY — AgreedID stays 0
// in every case below, the wrong-student firewall.
func TestMatchStudentEmbeddedAndFar(t *testing.T) {
	roster := []RosterEntry{
		{ID: 1, ExternalID: "B11902003", Name: "王小明"},
		{ID: 2, ExternalID: "C21813555", Name: "李大華"},
	}
	cases := []struct {
		name string
		id   string
		nm   string
		want StudentMatch
	}{
		// Embedded rung: live-observed leading-artifact shape.
		{"embedded unique, no name -> proposal only",
			"1B11902003", "", StudentMatch{ProposedID: 1, ProposalSource: "ocr_id_near"}},
		{"embedded unique, junk both sides -> proposal only",
			"XB11902003Y", "", StudentMatch{ProposedID: 1, ProposalSource: "ocr_id_near"}},
		// Name agreement upgrades confidence in the PROPOSAL — never into
		// AgreedID (see the firewall comment in MatchStudent: the junk proves
		// the read is corrupted, so verbatim containment still never
		// auto-assigns).
		{"embedded + matching name -> still proposal only, never auto-assign",
			"1B11902003", "王小明", StudentMatch{ProposedID: 1, ProposalSource: "ocr_id_near"}},
		// An exact, unique name hit on a DIFFERENT student beats the fuzzy ID.
		{"embedded vs exact name on another student -> name proposal wins",
			"1B11902003", "李大華", StudentMatch{ProposedID: 2, ProposalSource: "ocr_name"}},
		// Off-roster embedded shape from the live run: must refuse entirely.
		{"junk prefix on off-roster ID -> no proposal",
			"CB99999999", "", StudentMatch{}},
		// Far (distance-2) rung: unique with gap >= 2 on this sparse roster.
		{"d-2 unique with gap, no name -> proposal only",
			"A1190200X", "", StudentMatch{ProposedID: 1, ProposalSource: "ocr_id_near"}},
		{"d-2 unique with gap + matching name -> still proposal only",
			"A1190200X", "王小明", StudentMatch{ProposedID: 1, ProposalSource: "ocr_id_near"}},
		{"d-2 vs exact name on another student -> name proposal wins",
			"A1190200X", "李大華", StudentMatch{ProposedID: 2, ProposalSource: "ocr_name"}},
		// Exact and d-1 behavior is untouched by the new rungs existing.
		{"exact agreement stays ocr_agree",
			"B11902003", "王小明", StudentMatch{AgreedID: 1, ProposedID: 1, ProposalSource: "ocr_agree"}},
		{"exact ID hit stays ocr_id",
			"B11902003", "", StudentMatch{ProposedID: 1, ProposalSource: "ocr_id"}},
		{"d-1 near miss stays ocr_id_near",
			"B11902103", "", StudentMatch{ProposedID: 1, ProposalSource: "ocr_id_near"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchStudent(tc.id, tc.nm, roster)
			if got != tc.want {
				t.Fatalf("MatchStudent(%q, %q) = %+v, want %+v", tc.id, tc.nm, got, tc.want)
			}
			if got.AgreedID != 0 && tc.want.AgreedID == 0 {
				t.Fatalf("proposal rung auto-assigned: %+v", got)
			}
		})
	}
}

// TestMatchStudentSequentialRosterRefusesFar proves the distance-2 rung's
// uniqueness gap on the roster shape that makes it dangerous: sequential IDs
// (B11902001..B11902010) mutually differ by 1-2 digits, so a d-2 read sits in
// a crowded neighborhood and must virtually always refuse.
func TestMatchStudentSequentialRosterRefusesFar(t *testing.T) {
	seq := make([]RosterEntry, 0, 10)
	for i := 1; i <= 9; i++ {
		seq = append(seq, RosterEntry{
			ID:         int64(i),
			ExternalID: "B1190200" + string(rune('0'+i)),
			Name:       "學生" + string(rune('0'+i)),
		})
	}
	seq = append(seq, RosterEntry{ID: 10, ExternalID: "B11902010", Name: "學生十"})

	probes := []string{
		"A1190200X", // d=2 from several -> ambiguous
		"AX1902001", // d=2 from one, d=3 from neighbors -> gap too small
		"B11902XX1", // d=2 from one, d=3 from neighbors -> gap too small
	}
	for _, probe := range probes {
		got := MatchStudent(probe, "", seq)
		if got != (StudentMatch{}) {
			t.Fatalf("MatchStudent(%q, \"\", sequential) = %+v, want no proposal", probe, got)
		}
	}
}

// TestMatchStudentRungPriority pins the ladder ordering: exact ->
// embedded-unique -> distance-1-unique -> distance-2. When the embedded rung
// and the d-1 rung would BOTH fire (on different students), the embedded rung
// wins: every character of its candidate is present verbatim in the read,
// which is stronger evidence than an edit-distance mutation.
func TestMatchStudentRungPriority(t *testing.T) {
	roster := []RosterEntry{
		{ID: 1, ExternalID: "B11902003", Name: "王小明"},
		{ID: 2, ExternalID: "XXB11902093", Name: "李大華"},
	}
	// Read "XXB11902003": contains B11902003 verbatim (embedded -> ID 1) and
	// is edit distance 1 from XXB11902093 (near -> ID 2). Embedded must win.
	t.Run("embedded beats d-1 when both would fire", func(t *testing.T) {
		got := MatchStudent("XXB11902003", "", roster)
		want := StudentMatch{ProposedID: 1, ProposalSource: "ocr_id_near"}
		if got != want {
			t.Fatalf("MatchStudent() = %+v, want %+v", got, want)
		}
	})
	// Sanity: the d-1 rung alone WOULD have proposed ID 2 for this read, so
	// the case above genuinely exercises priority, not d-1 refusal.
	t.Run("d-1 alone would fire on the other student", func(t *testing.T) {
		id, ok := matchOCRIDNear("XXB11902003", roster)
		if !ok || id != 2 {
			t.Fatalf("matchOCRIDNear() = (%d, %v), want (2, true)", id, ok)
		}
	})
	// d-1 beats d-2 by construction: any roster ID at distance 1 makes the
	// far rung refuse outright (gap gate), so a d-2 candidate can never
	// outrank a d-1 one.
	t.Run("d-1 beats d-2", func(t *testing.T) {
		sparse := []RosterEntry{
			{ID: 1, ExternalID: "B11902001", Name: "王小明"},
			{ID: 2, ExternalID: "C21813555", Name: "李大華"},
		}
		got := MatchStudent("A11902001", "", sparse) // d=1 from ID 1
		want := StudentMatch{ProposedID: 1, ProposalSource: "ocr_id_near"}
		if got != want {
			t.Fatalf("MatchStudent() = %+v, want %+v", got, want)
		}
	})
}
