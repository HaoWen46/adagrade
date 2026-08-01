package transcribe

import "testing"

// --- 2026-07-30 multi-line audit: misfiled block content -------------------

func TestParseResponse_MisfiledListTextBecomesProse(t *testing.T) {
	// The model sometimes puts a list's content in "text" and leaves "items"
	// empty. Dropping the block would silently erase the student's writing;
	// re-filing it as prose keeps every byte.
	raw := []byte(`{"blocks":[{"kind":"list","text":"1. sort 2. scan","items":[]}],"confidence":"high"}`)
	doc, _, err := ParseResponse(raw)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if len(doc.Blocks) != 1 {
		t.Fatalf("misfiled list content must survive, got %d blocks", len(doc.Blocks))
	}
	if doc.Blocks[0].Kind != BlockProse || doc.Blocks[0].Text != "1. sort 2. scan" {
		t.Errorf("want prose block with the text, got %+v", doc.Blocks[0])
	}
}

func TestParseResponse_MisfiledItemsBecomeList(t *testing.T) {
	raw := []byte(`{"blocks":[{"kind":"prose","text":"","items":["sort","scan"]}],"confidence":"high"}`)
	doc, _, err := ParseResponse(raw)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if len(doc.Blocks) != 1 || doc.Blocks[0].Kind != BlockList || len(doc.Blocks[0].Items) != 2 {
		t.Errorf("misfiled items must survive as a list, got %+v", doc.Blocks)
	}
}

func TestParseResponse_TrulyEmptyBlocksStillDrop(t *testing.T) {
	raw := []byte(`{"blocks":[{"kind":"list","text":"","items":[]},{"kind":"prose","text":"","items":[]}],"confidence":"low"}`)
	doc, _, err := ParseResponse(raw)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if len(doc.Blocks) != 0 {
		t.Errorf("blocks with no content anywhere are lossless to drop, got %+v", doc.Blocks)
	}
}
