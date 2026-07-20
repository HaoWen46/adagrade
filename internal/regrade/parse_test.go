package regrade

import "testing"

// TestParseBlocks_Table is the normative parser table (spec §2, §10): every rule
// gets a case, including the quoted-template self-match and the embedded "2:"
// example given verbatim in the spec.
func TestParseBlocks_Table(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []Block
	}{
		{
			name: "single block",
			text: "<p1>\nThe base case is wrong.\n</p1>",
			want: []Block{{Number: 1, Text: "\nThe base case is wrong.\n"}},
		},
		{
			name: "spec example: two blocks, embedded '2:' is just text",
			text: "<p1>\n" +
				"The base case n=1 was marked wrong, but rubric line 2 says:\n" +
				"2: partial credit applies when ...\n" +
				"</p1>\n" +
				"<p4>\n" +
				"My exchange argument handles ties — the -2 assumes it doesn't.\n" +
				"</p4>",
			want: []Block{
				{Number: 1, Text: "\nThe base case n=1 was marked wrong, but rubric line 2 says:\n2: partial credit applies when ...\n"},
				{Number: 4, Text: "\nMy exchange argument handles ties — the -2 assumes it doesn't.\n"},
			},
		},
		{
			name: "text outside all tags is ignored (greeting/signature)",
			text: "Hi TA,\n<p2>\nplease recheck\n</p2>\nThanks,\nA Student",
			want: []Block{{Number: 2, Text: "\nplease recheck\n"}},
		},
		{
			name: "duplicate <pN> blocks concatenate in arrival order",
			text: "<p1>\nfirst part\n</p1>\nsome text\n<p1>\nsecond part\n</p1>",
			want: []Block{{Number: 1, Text: "\nfirst part\n" + "\nsecond part\n"}},
		},
		{
			name: "quoted-template self-match: our own '>'-quoted template must never match",
			// Simulates a reply where the mail client quoted the original template
			// (each line prefixed with '>') and the student wrote nothing new.
			text: "> <p1>\n> complaint text goes here\n> </p1>\n",
			want: nil,
		},
		{
			name: "quote-stripped reply still parses the student's own unquoted block",
			text: "> <p1>\n> quoted old complaint\n> </p1>\n" +
				"<p2>\nmy actual new complaint\n</p2>",
			want: []Block{{Number: 2, Text: "\nmy actual new complaint\n"}},
		},
		{
			name: "unknown tag <q1> silently ignored",
			text: "<q1>\nnot a problem tag\n</q1>",
			want: nil,
		},
		{
			name: "full-width tag accepted, digits normalized (D55 amendment 2026-07-10)",
			text: "＜ｐ１＞\nfull-width tag, body verbatim\n＜／ｐ１＞",
			want: []Block{{Number: 1, Text: "\nfull-width tag, body verbatim\n"}},
		},
		{
			name: "uppercase tag accepted (D55 amendment 2026-07-10)",
			text: "<P1>\nuppercase tag\n</P1>",
			want: []Block{{Number: 1, Text: "\nuppercase tag\n"}},
		},
		{
			name: "full-width multi-digit number maps to the same problem as ASCII",
			text: "<p１２>\nfull-width twelve\n</p１２>",
			want: []Block{{Number: 12, Text: "\nfull-width twelve\n"}},
		},
		{
			name: "mixed-width open/close pair up by normalized number",
			text: "<p3>\nascii open, full-width close\n＜／Ｐ３＞",
			want: []Block{{Number: 3, Text: "\nascii open, full-width close\n"}},
		},
		{
			name: "ASCII and full-width duplicates of the same number merge into one block",
			text: "<p1>\nfirst part\n</p1>\n＜ｐ１＞\nsecond part\n＜／ｐ１＞",
			want: []Block{{Number: 1, Text: "\nfirst part\n" + "\nsecond part\n"}},
		},
		{
			name: "body text is byte-for-byte verbatim — full-width digits inside the body are NOT normalized",
			text: "<p2>\nrubric line ２ gives １.５ points\n</p2>",
			want: []Block{{Number: 2, Text: "\nrubric line ２ gives １.５ points\n"}},
		},
		{
			name: "Cyrillic lookalike rejected",
			text: "<р1>\nCyrillic р (U+0440), must not match\n</р1>",
			want: nil,
		},
		{
			name: "inner spaces rejected",
			text: "< p1 >\nspaces inside tag, must not match\n</p1>",
			want: nil,
		},
		{
			name: "unclosed tag silently ignored",
			text: "<p1>\nnever closed",
			want: nil,
		},
		{
			name: "mismatched close tag leaves block unclosed and ignored",
			text: "<p1>\nopened as 1\n</p2>",
			want: nil,
		},
		{
			name: "multi-paragraph body is verbatim, including blank lines",
			text: "<p3>\nFirst paragraph.\n\nSecond paragraph.\n</p3>",
			want: []Block{{Number: 3, Text: "\nFirst paragraph.\n\nSecond paragraph.\n"}},
		},
		{
			name: "no tags at all",
			text: "please regrade my exam, problem 3 seems wrong",
			want: nil,
		},
		{
			name: "empty text",
			text: "",
			want: nil,
		},
		{
			name: "multi-digit problem number",
			text: "<p12>\ndouble digit\n</p12>",
			want: []Block{{Number: 12, Text: "\ndouble digit\n"}},
		},
		{
			name: "valid block alongside an invalid (unclosed) one — only valid files",
			text: "<p1>\nvalid complaint\n</p1>\n<p2>\nnever closed",
			want: []Block{{Number: 1, Text: "\nvalid complaint\n"}},
		},
		{
			name: "three valid blocks, arrival order preserved",
			text: "<p3>\nthird\n</p3>\n<p1>\nfirst\n</p1>\n<p2>\nsecond\n</p2>",
			want: []Block{
				{Number: 3, Text: "\nthird\n"},
				{Number: 1, Text: "\nfirst\n"},
				{Number: 2, Text: "\nsecond\n"},
			},
		},
		{
			name: "zero as problem number is still a syntactically valid tag (N is just digits; unknown-N filtering happens in the translation layer, not here)",
			text: "<p0>\nzero\n</p0>",
			want: []Block{{Number: 0, Text: "\nzero\n"}},
		},
		{
			name: "leading zero in tag number",
			text: "<p01>\nleading zero\n</p01>",
			want: []Block{{Number: 1, Text: "\nleading zero\n"}},
		},
		{
			name: "quoted line in the middle of an otherwise valid block does not break the block, it's just removed from the text",
			text: "<p1>\nline one\n> quoted middle line\nline two\n</p1>",
			want: []Block{{Number: 1, Text: "\nline one\nline two\n"}},
		},
		{
			name: "tag with letters instead of digits is not a tag at all",
			text: "<pN>\nliteral N, not a number\n</pN>",
			want: nil,
		},
		{
			name: "whitespace-only quoted marker line ('>' alone) is stripped",
			text: ">\n<p1>\nbody\n</p1>",
			want: []Block{{Number: 1, Text: "\nbody\n"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseBlocks(tc.text)
			if !blocksEqual(got, tc.want) {
				t.Errorf("ParseBlocks(%q) = %+v, want %+v", tc.text, got, tc.want)
			}
		})
	}
}

func blocksEqual(a, b []Block) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParseBlocks_QuoteStrippingBeforeMatching verifies the ORDER of operations:
// quote-stripping happens before tag matching, so a quoted opening tag combined
// with an unquoted closing tag must NOT accidentally pair up.
func TestParseBlocks_QuoteStrippingBeforeMatching(t *testing.T) {
	text := "> <p1>\nunquoted body\n</p1>"
	got := ParseBlocks(text)
	// After stripping the quoted line "> <p1>", only "unquoted body\n</p1>"
	// remains — an orphaned close tag with no matching open, so nothing files.
	if got != nil {
		t.Errorf("ParseBlocks with quoted open + unquoted close = %+v, want nil (no valid pairing)", got)
	}
}

// TestParseBlocks_DoesNotMutateAcrossCalls guards against shared-state bugs (e.g.
// a package-level regexp with a stateful Longest() flag) since the parser will be
// called repeatedly, once per inbound webhook.
func TestParseBlocks_DoesNotMutateAcrossCalls(t *testing.T) {
	text := "<p1>\nfirst call\n</p1>"
	first := ParseBlocks(text)
	second := ParseBlocks(text)
	if !blocksEqual(first, second) {
		t.Errorf("ParseBlocks not idempotent across calls: %+v vs %+v", first, second)
	}
}
