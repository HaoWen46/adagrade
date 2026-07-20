package httpapi

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"testing"
)

// TestExportCSV_StatusColumn_FlagsWithdrawnNeverDrops pins locked semantics (e)
// (roster-lifecycle plan 2026-07-10, Task R2): withdrawn students stay in the grades
// export — never silently dropped — flagged by a final `status` column that reads
// `active` for everyone else.
func TestExportCSV_StatusColumn_FlagsWithdrawnNeverDrops(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	setStudentWithdrawnByExt(t, f.st, "b02", true)

	resp, err := f.c.Get(fmt.Sprintf("%s/api/assessments/%d/export.csv", f.ts.URL, f.aid))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export: got %d want 200", resp.StatusCode)
	}
	records, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("csv rows = %d, want header + b01 + b02 (withdrawn never dropped): %v", len(records), records)
	}

	head := records[0]
	if head[len(head)-1] != "status" {
		t.Fatalf("last header column = %q, want status (header: %v)", head[len(head)-1], head)
	}
	statusByID := map[string]string{}
	for _, row := range records[1:] {
		statusByID[row[0]] = row[len(row)-1]
	}
	if statusByID["b01"] != "active" {
		t.Errorf("b01 status = %q, want active", statusByID["b01"])
	}
	if statusByID["b02"] != "withdrawn" {
		t.Errorf("b02 status = %q, want withdrawn", statusByID["b02"])
	}
}
