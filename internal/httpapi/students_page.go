package httpapi

import (
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/publish"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// The per-student page (design doc 2026-07-28-student-page-design.md): two read-only
// staff endpoints answering "what is the story with this student" without a SQL
// session. Both are addressed by the SCHOOL id (students.student_id) — the same
// vocabulary as the export filenames, the CSV and the totals table — and neither
// mutates anything.
//
//	GET /api/students/{sid}                       header + per-assessment cards
//	GET /api/students/{sid}/assessments/{aid}     the expanded card
//
// Three rules run through everything below:
//
//   - D3/D4: points are exact decimal STRINGS (store.NumStr), and an ungraded
//     problem or an all-ungraded assessment is JSON null — never a fake 0, which
//     would be a claim about the student's work that nobody has made.
//   - D14 / CLAUDE.md: regrade_request_problems.complaint_text and
//     regrade_requests.body are student content. They are not selected by the
//     queries and never reach this file, let alone the wire or a log line. The
//     regrade section is verdicts, statuses and timestamps only.
//   - Effective grades, not the bare official pointer: the adopted regrade overlay
//     wins (rounds design, 0028). See students_page.sql's header for why the
//     changed-since-publish diff would be wrong otherwise.

// --- wire types ---------------------------------------------------------------------

type studentPageProblemJSON struct {
	Number int32  `json:"number"`
	Title  string `json:"title"`
	// AnswerID/Score are null when the student has no answers row for this problem
	// (the page renders "absent" and does not link the row) or nothing is graded.
	AnswerID *int64  `json:"answer_id"`
	Score    *string `json:"score"`
	Max      string  `json:"max"`
}

type studentPageAssessmentJSON struct {
	AssessmentID int64  `json:"assessment_id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Answers      int64  `json:"answers"`
	Graded       int64  `json:"graded"`
	// Total is null until something is graded (D3).
	Total     *string                  `json:"total"`
	Max       string                   `json:"max"`
	Published bool                     `json:"published"`
	Problems  []studentPageProblemJSON `json:"problems"`
}

type studentPageHeaderJSON struct {
	StudentID string `json:"student_id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Withdrawn bool   `json:"withdrawn"` // withdrawn_at IS NOT NULL (D23)
}

type studentPagePublishJSON struct {
	BatchCreatedAt *time.Time `json:"batch_created_at"`
	EmailStatus    string     `json:"email_status"`
	SentAt         *time.Time `json:"sent_at"`
	RecipientEmail string     `json:"recipient_email"` // the roster email at publish time (B-H10)
	SnapshotTotal  *string    `json:"snapshot_total"`
	// ChangedSincePublish is the badge the page exists for: what the student was
	// told vs. what is true now.
	ChangedSincePublish bool `json:"changed_since_publish"`
}

type studentPageDetailProblemJSON struct {
	Number   int32  `json:"number"`
	AnswerID *int64 `json:"answer_id"`
	// Source/ModelID/Confidence describe the record that produced CurrentScore —
	// a bare overridden 8/10 reading as AI-given is a lie by omission.
	Source         *string  `json:"source"`
	ModelID        *string  `json:"model_id"`
	Confidence     *string  `json:"confidence"`
	Flags          []string `json:"flags"`
	PublishedScore *string  `json:"published_score"`
	CurrentScore   *string  `json:"current_score"`
	Changed        bool     `json:"changed"`
}

type studentPageRegradeProblemJSON struct {
	Number  int32   `json:"number"`
	Verdict *string `json:"verdict"` // null while the sub-item is unadjudicated
}

type studentPageRegradeJSON struct {
	RequestID  int64      `json:"request_id"`
	ReceivedAt *time.Time `json:"received_at"`
	// Status is regrade_requests.status verbatim (migrations 0017/0025/0028):
	// received | under_review | resolved_upheld | resolved_regraded | rejected_*.
	Status   string                          `json:"status"`
	Problems []studentPageRegradeProblemJSON `json:"problems"`
}

type studentPageDetailJSON struct {
	Publish  *studentPagePublishJSON        `json:"publish"`
	Problems []studentPageDetailProblemJSON `json:"problems"`
	Regrades []studentPageRegradeJSON       `json:"regrades"`
}

// --- handlers -----------------------------------------------------------------------

// handleStudentPage is the collapsed page: roster header plus one card per assessment
// the student has at least one answers row in, newest first. Archived assessments are
// included — history is history.
func (s *Server) handleStudentPage(w http.ResponseWriter, r *http.Request) {
	student, err := s.store.Q.GetStudentByExternalID(r.Context(), r.PathValue("sid"))
	if err != nil {
		// Deliberately does not echo the requested id back (D14): the response
		// envelope is a fixed string.
		apiError(w, http.StatusNotFound, "no such student on the roster")
		return
	}

	cards, err := s.store.Q.StudentAssessmentSummaries(r.Context(), student.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "student summary failed")
		return
	}
	rows, err := s.store.Q.StudentAssessmentProblems(r.Context(), student.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "student summary failed")
		return
	}

	byAssessment := make(map[int64][]studentPageProblemJSON, len(cards))
	for _, p := range rows {
		byAssessment[p.AssessmentID] = append(byAssessment[p.AssessmentID], studentPageProblemJSON{
			Number:   p.ProblemNumber,
			Title:    p.Title,
			AnswerID: int8Ptr(p.AnswerID),
			Score:    numStrPtr(p.Score),
			Max:      store.NumStr(p.MaxPoints),
		})
	}

	out := make([]studentPageAssessmentJSON, 0, len(cards))
	for _, c := range cards {
		problems := byAssessment[c.AssessmentID]
		if problems == nil {
			problems = []studentPageProblemJSON{}
		}
		out = append(out, studentPageAssessmentJSON{
			AssessmentID: c.AssessmentID,
			Name:         c.Name,
			Kind:         c.Kind,
			Answers:      c.Answers,
			Graded:       c.Graded,
			Total:        numStrPtr(c.Total),
			Max:          store.NumStr(c.Max),
			Published:    c.Published,
			Problems:     problems,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"student": studentPageHeaderJSON{
			StudentID: student.StudentID, Name: student.Name, Email: student.Email,
			Withdrawn: student.WithdrawnAt.Valid,
		},
		"assessments": out,
	})
}

// handleStudentAssessmentDetail is the expanded card: everything that is not a score
// — publish/delivery state, the changed-since-publish diff, per-problem provenance,
// and the regrade threads.
func (s *Server) handleStudentAssessmentDetail(w http.ResponseWriter, r *http.Request) {
	student, err := s.store.Q.GetStudentByExternalID(r.Context(), r.PathValue("sid"))
	if err != nil {
		apiError(w, http.StatusNotFound, "no such student on the roster")
		return
	}
	aid, ok := pathID(r, "aid")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	if _, err := s.store.Q.GetAssessment(r.Context(), aid); err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}

	// The live (non-superseded, D29) publish item is the snapshot of what the
	// student was actually told. No live item ⇒ no publish section at all, rather
	// than a zero-valued one that would render as "sent, blank".
	var snapshot publish.Snapshot
	var pub *studentPagePublishJSON
	item, err := s.store.Q.StudentLivePublishItem(r.Context(), db.StudentLivePublishItemParams{
		AssessmentID: aid, StudentID: student.ID,
	})
	switch {
	case err == nil:
		// A snapshot that fails to decode leaves the zero Snapshot: every
		// published_score reads null and the badge lights up, which is the
		// honest failure direction (it says "look at this"), not a silent
		// "nothing changed".
		if uerr := json.Unmarshal(item.Snapshot, &snapshot); uerr != nil {
			s.log.Error("publish snapshot decode failed", "publish_item_id", item.ID)
		}
		pub = &studentPagePublishJSON{
			BatchCreatedAt: tsPtr(item.BatchCreatedAt),
			EmailStatus:    item.EmailStatus,
			SentAt:         tsPtr(item.SentAt),
			RecipientEmail: item.RecipientEmail,
			SnapshotTotal:  decimalStrPtr(snapshot.Total),
		}
	case errors.Is(err, pgx.ErrNoRows):
		// Never published (or only in a superseded batch): pub stays nil.
	default:
		apiError(w, http.StatusInternalServerError, "publish state lookup failed")
		return
	}

	snapByNumber := make(map[int32]publish.SnapProblem, len(snapshot.Problems))
	if pub != nil {
		for _, sp := range snapshot.Problems {
			snapByNumber[sp.Number] = sp
		}
	}

	rows, err := s.store.Q.StudentAssessmentProblemDetail(r.Context(), db.StudentAssessmentProblemDetailParams{
		AssessmentID: aid, StudentID: student.ID,
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "student detail failed")
		return
	}

	problems := make([]studentPageDetailProblemJSON, 0, len(rows))
	var currentTotal *big.Rat // nil ⇒ nothing graded (D3), never an implicit 0
	for _, p := range rows {
		flags := p.Flags
		if flags == nil {
			flags = []string{}
		}
		current := numericRat(p.Score)
		if current != nil {
			if currentTotal == nil {
				currentTotal = new(big.Rat)
			}
			currentTotal.Add(currentTotal, current)
		}

		var publishedScore *string
		var published *big.Rat
		if pub != nil {
			if sp, ok := snapByNumber[p.ProblemNumber]; ok {
				publishedScore = decimalStrPtr(sp.Total)
				published = decimalRat(sp.Total)
			}
		}

		problems = append(problems, studentPageDetailProblemJSON{
			Number:         p.ProblemNumber,
			AnswerID:       int8Ptr(p.AnswerID),
			Source:         nonEmptyPtr(p.Source),
			ModelID:        textPtr(p.ModelID),
			Confidence:     textPtr(p.Confidence),
			Flags:          flags,
			PublishedScore: publishedScore,
			CurrentScore:   numStrPtr(p.Score),
			// Nothing published ⇒ nothing to differ from.
			Changed: pub != nil && !ratEqual(published, current),
		})
	}
	if pub != nil {
		pub.ChangedSincePublish = !ratEqual(decimalRat(snapshot.Total), currentTotal)
	}

	regrades, err := s.studentRegradeThreads(r, aid, student.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "regrade lookup failed")
		return
	}

	writeJSON(w, http.StatusOK, studentPageDetailJSON{
		Publish: pub, Problems: problems, Regrades: regrades,
	})
}

// studentRegradeThreads assembles this (student, assessment)'s regrade requests with
// their per-problem verdicts. Student text (complaint_text, body, subject) is not
// selected by either query and has no field to land in here.
func (s *Server) studentRegradeThreads(r *http.Request, aid, studentID int64) ([]studentPageRegradeJSON, error) {
	reqs, err := s.store.Q.StudentRegradeRequests(r.Context(), db.StudentRegradeRequestsParams{
		AssessmentID: aid, StudentID: studentID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]studentPageRegradeJSON, 0, len(reqs))
	if len(reqs) == 0 {
		return out, nil
	}

	ids := make([]int64, 0, len(reqs))
	for _, rr := range reqs {
		ids = append(ids, rr.ID)
	}
	subs, err := s.store.Q.StudentRegradeProblems(r.Context(), ids)
	if err != nil {
		return nil, err
	}
	byRequest := make(map[int64][]studentPageRegradeProblemJSON, len(reqs))
	for _, sub := range subs {
		byRequest[sub.RequestID] = append(byRequest[sub.RequestID], studentPageRegradeProblemJSON{
			Number: sub.ProblemNumber, Verdict: textPtr(sub.Verdict),
		})
	}
	for _, rr := range reqs {
		ps := byRequest[rr.ID]
		if ps == nil {
			ps = []studentPageRegradeProblemJSON{}
		}
		out = append(out, studentPageRegradeJSON{
			RequestID: rr.ID, ReceivedAt: tsPtr(rr.ReceivedAt), Status: rr.Status, Problems: ps,
		})
	}
	return out, nil
}

// --- small conversions ---------------------------------------------------------------

// numStrPtr renders a nullable NUMERIC as an exact decimal string, or nil for SQL
// NULL — the D3 "ungraded is null, never 0" boundary.
func numStrPtr(n pgtype.Numeric) *string {
	if !n.Valid || n.Int == nil {
		return nil
	}
	v := store.NumStr(n)
	return &v
}

// decimalStrPtr is numStrPtr for snapshot fields, which carry "" (not NULL) for
// "ungraded at publish time" (publish.Snapshot's on-disk convention).
func decimalStrPtr(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func int8Ptr(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	out := t.String
	return &out
}

// nonEmptyPtr maps a coalesced-to-empty column back to JSON null (see
// students_page.sql on why source arrives as "" rather than NULL).
func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	out := s
	return &out
}

// numericRat / decimalRat lift a NUMERIC column and a stored decimal string to exact
// rationals. Comparison happens on rationals, never float64 (D4), and never on the
// raw strings — "8.50" and "8.5" are the same grade, and a snapshot written at a
// different column scale must not read as "changed".
func numericRat(n pgtype.Numeric) *big.Rat {
	if !n.Valid || n.Int == nil {
		return nil
	}
	return store.NumRat(n)
}

func decimalRat(s string) *big.Rat {
	if s == "" {
		return nil
	}
	n, err := store.Num(s)
	if err != nil || !n.Valid || n.Int == nil {
		return nil
	}
	return store.NumRat(n)
}

// ratEqual treats "both absent" as equal and "exactly one absent" as different: a
// problem that went from ungraded to graded (or back) has changed.
func ratEqual(a, b *big.Rat) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Cmp(b) == 0
}
