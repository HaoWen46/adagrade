package roster

import (
	"context"
	"fmt"
	"strings"

	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/studentid"
)

// Queries is the slice of the store the importer needs (satisfied by *db.Queries).
type Queries interface {
	ListStudents(ctx context.Context) ([]db.Student, error)
	UpsertStudent(ctx context.Context, arg db.UpsertStudentParams) (db.Student, error)
}

// RosterDiff reports how the CSV differs from the current roster (roster-lifecycle
// plan 2026-07-10, fix 1). It is informational only — the import never withdraws
// or reinstates anyone; the Students page proposes bulk sync from these lists.
// Changed-field counts are counts only (names/emails are PII, D14).
type RosterDiff struct {
	MissingActive    []string `json:"missing_active"`    // active student_ids in DB, absent from CSV
	WithdrawnPresent []string `json:"withdrawn_present"` // withdrawn student_ids present in CSV (retaker trap)
	EmailChanged     int64    `json:"email_changed"`     // count only (PII)
	NameChanged      int64    `json:"name_changed"`
}

// Report summarizes an import for the operator (D13). RosterDiff is embedded so
// its fields flatten into the JSON response additively (existing fields
// unchanged) — the shape frontend/src/lib/types.ts ImportReport declares.
type Report struct {
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Total     int `json:"total"`
	RosterDiff
}

// Import upserts rows keyed by student_id. Run it inside a transaction so a failed
// import leaves the roster untouched. Row-level problems (a CSV email that already
// belongs to a different existing student) come back as ParseErrors with nothing
// upserted — the import stays all-or-nothing.
func Import(ctx context.Context, q Queries, rows []Row) (Report, []ParseError, error) {
	existing, err := q.ListStudents(ctx)
	if err != nil {
		return Report{}, nil, err
	}
	byID := make(map[string]db.Student, len(existing))
	emailOwner := make(map[string]db.Student, len(existing)) // lowercase email -> student
	normOwner := make(map[string]db.Student, len(existing))  // studentid.Normalize(id) -> student
	for _, s := range existing {
		byID[s.StudentID] = s
		emailOwner[strings.ToLower(s.Email)] = s
		normOwner[studentid.Normalize(s.StudentID)] = s
	}

	// Cross-DB duplicate guards BEFORE any write. Email: a row may keep its own
	// student's address (that is just an update), never a different student's.
	// Normalized id: students.student_id is UNIQUE on the exact string only, so a row
	// whose id differs exactly but collides under studentid.Normalize (case/width/
	// punctuation noise) would insert a SECOND roster row for the same person — and
	// scan matching, which folds ids through Normalize, would resolve OCR proposals to
	// whichever row it saw first. csv.go's seenNorm only guards within one file; this
	// guards a later CSV against the existing roster. Messages carry student_ids only
	// — never emails/names (D14).
	var rowErrs []ParseError
	for _, r := range rows {
		if owner, ok := emailOwner[r.Email]; ok && owner.StudentID != r.StudentID {
			rowErrs = append(rowErrs, ParseError{Line: r.Line, Msg: fmt.Sprintf("email already belongs to student %s", owner.StudentID)})
		}
		if owner, ok := normOwner[studentid.Normalize(r.StudentID)]; ok && owner.StudentID != r.StudentID {
			rowErrs = append(rowErrs, ParseError{Line: r.Line, Msg: fmt.Sprintf("student_id %s duplicates existing student %s after normalization", r.StudentID, owner.StudentID)})
		}
	}
	if len(rowErrs) > 0 {
		return Report{}, rowErrs, nil
	}

	// Diff (informational, no mutation). ListStudents orders by student_id, so
	// both lists come out sorted. Non-nil so the JSON is [] rather than null.
	diff := RosterDiff{MissingActive: []string{}, WithdrawnPresent: []string{}}
	inCSV := make(map[string]bool, len(rows))
	for _, r := range rows {
		inCSV[r.StudentID] = true
	}
	for _, s := range existing {
		switch {
		case inCSV[s.StudentID] && s.WithdrawnAt.Valid:
			diff.WithdrawnPresent = append(diff.WithdrawnPresent, s.StudentID)
		case !inCSV[s.StudentID] && !s.WithdrawnAt.Valid:
			diff.MissingActive = append(diff.MissingActive, s.StudentID)
		}
	}

	rep := Report{Total: len(rows), RosterDiff: diff}
	for _, r := range rows {
		prev, exists := byID[r.StudentID]
		if exists {
			if prev.Email != r.Email {
				rep.EmailChanged++
			}
			if prev.Name != r.Name {
				rep.NameChanged++
			}
		}
		if exists && prev.Name == r.Name && prev.Email == r.Email {
			rep.Unchanged++
			continue
		}
		if _, err := q.UpsertStudent(ctx, db.UpsertStudentParams{
			StudentID: r.StudentID, Name: r.Name, Email: r.Email,
		}); err != nil {
			return Report{}, nil, err
		}
		if exists {
			rep.Updated++
		} else {
			rep.Added++
		}
	}
	return rep, nil, nil
}
