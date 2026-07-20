// Package publish is the Phase 6 publish state machine and grade-email send
// pipeline (spec §2, §3, §7). It owns the per-student snapshot JSONB shape,
// the coverage gate, the changed-only re-publish diff, and the seam the River
// email_send job calls into.
//
// PII rule (CLAUDE.md): a snapshot is student content (names, comments,
// transcribed scores). It is never logged — this package logs only counts,
// statuses, and ids. Snapshots are marshalled canonically (stable field order,
// sorted slices) so the changed-only diff can compare batches byte-for-byte.
package publish

import (
	"encoding/json"
	"math/big"
	"sort"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// Snapshot is one student's published result set for an assessment (spec §2). It is
// the canonical unit of the changed-only re-publish diff: two snapshots are "equal"
// iff their canonical JSON bytes match, so every field is a decimal string or a
// determinate value and every slice is sorted. The struct field order below IS the
// serialized key order (encoding/json emits struct fields in declaration order),
// which — together with the sorted slices — makes Marshal deterministic without a
// custom marshaller.
type Snapshot struct {
	AssessmentName    string        `json:"assessment_name"`
	StudentExternalID string        `json:"student_external_id"`
	StudentName       string        `json:"student_name"`
	Total             string        `json:"total"` // Σ official problem totals, decimal string; "" if none graded
	Max               string        `json:"max"`   // Σ problem max_points
	AllNoSubmission   bool          `json:"all_no_submission"`
	Problems          []SnapProblem `json:"problems"`
}

// SnapProblem is one problem's line within a snapshot (spec §2 per-problem totals).
//
// It deliberately does NOT persist answer_id (finding 1, post-hoc fix). An earlier
// version did, so the send job (report attachments, spec §3) could look up this
// problem's original page images from blobs without a live query. But persisting it
// broke the changed-only republish diff (D30): snapshots stored before the field
// existed decode with AnswerID zero-valued and re-marshal as "answer_id":0, while a
// freshly rebuilt snapshot for the SAME unchanged grade carries the real id — so
// every pre-existing student's snapshot would byte-diff as "changed" across that
// upgrade boundary and the whole cohort would be re-emailed. The send job instead
// resolves each problem's answer id LIVE from (student_id, problem number) at send/
// resend time (see Sender.resolveAnswerID) — answers are pre-materialized with a
// natural unique key (assessment_id, student_id, problem_id), so that resolution is
// stable and deterministic. Grade content (scores, comments, totals) still comes
// entirely from the snapshot; only the image-ref lookup goes live.
type SnapProblem struct {
	Number       int32           `json:"number"`
	Title        string          `json:"title"`
	Max          string          `json:"max"`
	NoSubmission bool            `json:"no_submission"`
	Total        string          `json:"total"` // official record total; "" if ungraded/no_submission
	Comment      string          `json:"comment"`
	Criteria     []SnapCriterion `json:"criteria"`
}

// SnapCriterion is one rubric criterion's scored line (spec §2 per-criterion
// scores + comments). Name/Max come from the pinned rubric version; Score/Comment
// from the official grading record.
type SnapCriterion struct {
	Name  string `json:"name"`
	Score string `json:"score"`
	Max   string `json:"max"`
}

// criterionMeta is the rubric-side metadata (name, max) keyed by criterion id — the
// PublishCriteria rows folded into a lookup the snapshot builder joins onto each
// record's criterion_scores.
type criterionMeta struct {
	name string
	max  string
}

// recordScore is one entry of grading_records.criterion_scores JSONB (mirrors
// grading.CriterionScore's on-disk shape without importing the grading package).
type recordScore struct {
	CriterionID int64  `json:"criterion_id"`
	Score       string `json:"score"`
}

// buildSnapshots turns the store's PublishSnapshotInputs rows + criterion metadata
// into one canonical Snapshot per student, keyed by internal student id, plus the
// per-student recipient email captured at publish time (B-H10). Rows must be the full
// per-(student,problem) set for the assessment (PublishSnapshotInputs already orders
// by student then problem number). assessmentName is stamped into every snapshot so a
// rename between publishes counts as a change.
func buildSnapshots(assessmentName string, rows []db.PublishSnapshotInputsRow, criteria []db.PublishCriteriaRow) (snaps map[int64]Snapshot, emails map[int64]string, names map[int64]string, externalIDs map[int64]string) {
	meta := make(map[int64]criterionMeta, len(criteria))
	for _, c := range criteria {
		meta[c.CriterionID] = criterionMeta{name: c.Description, max: store.NumStr(c.Points)}
	}

	snaps = make(map[int64]Snapshot)
	emails = make(map[int64]string)
	names = make(map[int64]string)
	externalIDs = make(map[int64]string)

	// Accumulators per student (rows are grouped by student then problem).
	type acc struct {
		snap      Snapshot
		totals    []*big.Rat // official problem totals present
		maxes     []*big.Rat
		anyGraded bool
		allNoSub  bool
	}
	byStudent := make(map[int64]*acc)
	order := make([]int64, 0)

	for _, r := range rows {
		a, ok := byStudent[r.StudentID]
		if !ok {
			a = &acc{
				snap: Snapshot{
					AssessmentName:    assessmentName,
					StudentExternalID: r.StudentExternalID,
					StudentName:       r.StudentName,
				},
				allNoSub: true,
			}
			byStudent[r.StudentID] = a
			order = append(order, r.StudentID)
			emails[r.StudentID] = r.StudentEmail
			names[r.StudentID] = r.StudentName
			externalIDs[r.StudentID] = r.StudentExternalID
		}

		sp := SnapProblem{
			Number:       r.ProblemNumber,
			Title:        r.ProblemTitle,
			Max:          store.NumStr(r.MaxPoints),
			NoSubmission: r.NoSubmission,
			Comment:      r.Comment.String,
			Criteria:     []SnapCriterion{},
		}
		if r.MaxPoints.Valid {
			a.maxes = append(a.maxes, store.NumRat(r.MaxPoints))
		}
		if !r.NoSubmission {
			a.allNoSub = false
		}

		// An official record present ⇒ scored problem: attach total + per-criterion lines.
		if r.RecordID.Valid {
			a.anyGraded = true
			sp.Total = store.NumStr(r.Total)
			if r.Total.Valid {
				a.totals = append(a.totals, store.NumRat(r.Total))
			}
			var scores []recordScore
			_ = json.Unmarshal(r.CriterionScores, &scores)
			for _, s := range scores {
				m := meta[s.CriterionID]
				sp.Criteria = append(sp.Criteria, SnapCriterion{Name: m.name, Score: s.Score, Max: m.max})
			}
			sort.Slice(sp.Criteria, func(i, j int) bool {
				if sp.Criteria[i].Name != sp.Criteria[j].Name {
					return sp.Criteria[i].Name < sp.Criteria[j].Name
				}
				return sp.Criteria[i].Score < sp.Criteria[j].Score
			})
		}
		a.snap.Problems = append(a.snap.Problems, sp)
	}

	for _, sid := range order {
		a := byStudent[sid]
		sort.Slice(a.snap.Problems, func(i, j int) bool {
			return a.snap.Problems[i].Number < a.snap.Problems[j].Number
		})
		if a.anyGraded {
			a.snap.Total = ratSliceStr(a.totals)
		}
		a.snap.Max = ratSliceStr(a.maxes)
		a.snap.AllNoSubmission = a.allNoSub
		snaps[sid] = a.snap
	}
	return snaps, emails, names, externalIDs
}

// canonicalJSON marshals a snapshot to its stable byte form. Struct field order +
// pre-sorted slices make json.Marshal deterministic, so equal snapshots ⇒ equal
// bytes and the changed-only diff is a []byte compare.
func canonicalJSON(s Snapshot) ([]byte, error) {
	return json.Marshal(s)
}

// ratSliceStr sums a slice of rationals and formats the result as a canonical
// decimal string via pgtype.Numeric (so the output matches store.NumStr formatting
// used everywhere else). An empty slice sums to "0".
func ratSliceStr(rs []*big.Rat) string {
	sum := new(big.Rat)
	for _, r := range rs {
		sum.Add(sum, r)
	}
	return ratStr(sum)
}

// ratStr renders an exact rational as a canonical decimal string. It defers to
// pgtype.Numeric formatting by going through a big.Float-free path: the rational is
// exact for our inputs (sums of NUMERIC values), so we render via its decimal
// expansion. For the value ranges here (points totals) a scale of 6 fractional
// digits is exact and then trailing zeros are trimmed, matching store.NumStr.
func ratStr(r *big.Rat) string {
	// Fast path: integer.
	if r.IsInt() {
		return r.Num().String()
	}
	// r is exact; NUMERIC inputs have finite decimal expansions (denominators are
	// products of 2s and 5s only after reduction? not guaranteed — but rubric points
	// and increments are decimals, so denominators divide a power of 10). Render with
	// enough precision and trim.
	f := r.FloatString(6)
	// Trim trailing zeros / dot (FloatString pads to fixed scale).
	return trimDecimal(f)
}

// trimDecimal drops trailing fractional zeros (and a bare trailing dot) from a
// fixed-scale decimal string — mirrors store.NumStr's trimming so snapshot totals
// compare byte-equal to values formatted elsewhere.
func trimDecimal(s string) string {
	if !hasDot(s) {
		return s
	}
	i := len(s)
	for i > 0 && s[i-1] == '0' {
		i--
	}
	if i > 0 && s[i-1] == '.' {
		i--
	}
	return s[:i]
}

func hasDot(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return true
		}
	}
	return false
}
