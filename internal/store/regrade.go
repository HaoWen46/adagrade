package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

// IsUniqueViolation reports whether err is a Postgres unique-constraint violation
// (SQLSTATE 23505) — several partial unique indexes raise this: message_id
// (migration 0020, re-delivered webhook payloads) and, as of migration 0025, the
// (publish_item_id, turn) WHERE kind='filed' race-killer (D57). Callers use this to
// distinguish "this insert lost a race / was already processed" from a genuine
// insert failure.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation
}

// RegradeRequest aliases the generated row type.
type RegradeRequest = db.RegradeRequest

// RegradeRequestProblem aliases the generated sub-item row type (migration 0025,
// spec §5 D59) — one row per contested problem within a request.
type RegradeRequestProblem = db.RegradeRequestProblem

// ProblemTAAssignment aliases the generated row type (migration 0025, spec §6 D60).
type ProblemTAAssignment = db.ProblemTaAssignment

// InsertRegradeRequestV2Params is the input to InsertRegradeRequestV2. publishItemID
// may be 0 when the token didn't even parse — in that case studentID/assessmentID are
// also unknown and left zero, same zero-means-NULL convention as v1.
type InsertRegradeRequestV2Params struct {
	PublishItemID int64
	StudentID     int64
	AssessmentID  int64
	FromEmail     string
	SPFVerdict    string
	DKIMVerdict   string
	Subject       string
	Body          string
	Status        string
	// MessageID is the provider's per-delivery id (F1 idempotency key). Empty is a
	// valid "no id available" value — the partial unique index only dedups non-empty
	// ids (migration 0020).
	MessageID string
	// Kind is filed|addendum|unparsed|handed_off (spec §2-§4, migration 0025). Always
	// required — unlike v1's status ladder, kind has no sensible default for a caller
	// to omit; the parser/webhook path always knows which bucket a reply landed in
	// before it calls this.
	Kind string
	// Turn is the token turn this request corresponds to (spec §4 D57): the turn
	// carried by the token being replied to. 0 means "unknown/not applicable" (e.g.
	// the token didn't parse) and is stored as SQL NULL, matching
	// PublishItemID/StudentID/AssessmentID's zero-means-NULL convention. The partial
	// unique index (publish_item_id, turn) WHERE kind='filed' enforces that at most
	// one 'filed' row exists per (item, turn) — callers passing Kind: "filed" must be
	// prepared for IsUniqueViolation(err) and re-record the reply as an addendum.
	Turn int
}

// InsertRegradeRequestV2 records one inbound reply's outcome (spec §2-§4, migration
// 0025): filed (>=1 valid block, token consumed), addendum (reply to an
// already-consumed token), unparsed (0 valid blocks, token NOT consumed), or
// handed_off is never passed here directly (see MarkRequestHandedOff — handoff is a
// state transition on an existing filed request's final-turn successor, not a
// distinct insert kind at receipt time... except when the handoff-triggering reply
// itself is the new row being recorded, in which case the caller passes
// Kind: "handed_off" directly). A caller inserting Kind: "filed" whose (PublishItemID,
// Turn) is already occupied by another filed row gets a unique-violation error
// (IsUniqueViolation) — the structural race-killer (D57): the loser must re-record its
// own reply as an addendum rather than treating this as a hard failure.
func (s *Store) InsertRegradeRequestV2(ctx context.Context, arg InsertRegradeRequestV2Params) (RegradeRequest, error) {
	// Belt for the partial unique index's suspenders (review finding): Turn/
	// PublishItemID below are mapped to SQL NULL whenever they're zero, and a partial
	// unique index treats NULLs as distinct rows -- so a caller passing Kind: "filed"
	// with Turn 0 or PublishItemID 0 would otherwise silently insert NULLs into the
	// columns regrade_requests_filed_item_turn_uniq keys on, letting two such rows
	// both succeed and defeating the D57 race-killer. The database CHECK
	// regrade_requests_filed_needs_slot (migration 0025) enforces this too, but reject
	// here first for a clearer, earlier error on the common caller mistake.
	if arg.Kind == "filed" && (arg.Turn == 0 || arg.PublishItemID == 0) {
		return RegradeRequest{}, fmt.Errorf("InsertRegradeRequestV2: kind=filed requires a non-zero PublishItemID and Turn (got PublishItemID=%d, Turn=%d)", arg.PublishItemID, arg.Turn)
	}
	return s.Q.InsertRegradeRequestV2(ctx, db.InsertRegradeRequestV2Params{
		PublishItemID: pgtype.Int8{Int64: arg.PublishItemID, Valid: arg.PublishItemID != 0},
		StudentID:     pgtype.Int8{Int64: arg.StudentID, Valid: arg.StudentID != 0},
		AssessmentID:  pgtype.Int8{Int64: arg.AssessmentID, Valid: arg.AssessmentID != 0},
		FromEmail:     arg.FromEmail,
		SpfVerdict:    pgtype.Text{String: arg.SPFVerdict, Valid: arg.SPFVerdict != ""},
		DkimVerdict:   pgtype.Text{String: arg.DKIMVerdict, Valid: arg.DKIMVerdict != ""},
		Subject:       arg.Subject,
		Body:          arg.Body,
		Status:        arg.Status,
		MessageID:     pgtype.Text{String: arg.MessageID, Valid: arg.MessageID != ""},
		Kind:          arg.Kind,
		Turn:          pgtype.Int4{Int32: int32(arg.Turn), Valid: arg.Turn != 0},
	})
}

// FileRegradeRequestParams is the input to FileRegradeRequestV2: everything
// InsertRegradeRequestV2Params needs to insert the request row, plus the sub-items to
// attach to it in the SAME transaction.
type FileRegradeRequestParams struct {
	PublishItemID int64
	StudentID     int64
	AssessmentID  int64
	FromEmail     string
	SPFVerdict    string
	DKIMVerdict   string
	Subject       string
	Body          string
	Status        string
	MessageID     string
	// Kind is normally "filed" — the only kind this method exists for (a request with
	// sub-items is, by definition, a filed request; addendum/unparsed/handed_off rows
	// never carry sub-items and go through InsertRegradeRequestV2 directly).
	Kind     string
	Turn     int
	Problems []RequestProblemInput
}

// FileRegradeRequestV2 inserts a filed regrade_requests row AND its
// regrade_request_problems sub-items as ONE atomic unit (Finding 2, IMPORTANT fix,
// spec §3/§5). Before this method existed, the webhook path called
// InsertRegradeRequestV2 (commits alone, consuming the (publish_item_id, turn) slot via
// the partial unique index) and then InsertRequestProblems in a SEPARATE transaction —
// a crash or error between the two committed a "filed" request with zero sub-items and
// permanently stranded that (item, turn) slot (the unique index has no way to
// distinguish "legitimately filed" from "orphaned by a partial failure", so no retry
// could ever file against it again). Wrapping both inserts in one WithTx means either
// the whole filing lands or none of it does — a failed sub-item insert (bad FK, or a
// concurrent unique-violation on the slot itself) rolls back the request row too, and
// IsUniqueViolation(err) still correctly reports the D57 filed-slot race so callers keep
// their existing "loser records an addendum" branch.
func (s *Store) FileRegradeRequestV2(ctx context.Context, arg FileRegradeRequestParams) (RegradeRequest, []RegradeRequestProblem, error) {
	if arg.Kind == "filed" && (arg.Turn == 0 || arg.PublishItemID == 0) {
		return RegradeRequest{}, nil, fmt.Errorf("FileRegradeRequestV2: kind=filed requires a non-zero PublishItemID and Turn (got PublishItemID=%d, Turn=%d)", arg.PublishItemID, arg.Turn)
	}

	var rr RegradeRequest
	subs := make([]RegradeRequestProblem, 0, len(arg.Problems))
	err := s.WithTx(ctx, func(q *db.Queries) error {
		var err error
		rr, err = q.InsertRegradeRequestV2(ctx, db.InsertRegradeRequestV2Params{
			PublishItemID: pgtype.Int8{Int64: arg.PublishItemID, Valid: arg.PublishItemID != 0},
			StudentID:     pgtype.Int8{Int64: arg.StudentID, Valid: arg.StudentID != 0},
			AssessmentID:  pgtype.Int8{Int64: arg.AssessmentID, Valid: arg.AssessmentID != 0},
			FromEmail:     arg.FromEmail,
			SpfVerdict:    pgtype.Text{String: arg.SPFVerdict, Valid: arg.SPFVerdict != ""},
			DkimVerdict:   pgtype.Text{String: arg.DKIMVerdict, Valid: arg.DKIMVerdict != ""},
			Subject:       arg.Subject,
			Body:          arg.Body,
			Status:        arg.Status,
			MessageID:     pgtype.Text{String: arg.MessageID, Valid: arg.MessageID != ""},
			Kind:          arg.Kind,
			Turn:          pgtype.Int4{Int32: int32(arg.Turn), Valid: arg.Turn != 0},
		})
		if err != nil {
			return fmt.Errorf("insert regrade request: %w", err)
		}
		for _, p := range arg.Problems {
			row, err := q.InsertRequestProblem(ctx, db.InsertRequestProblemParams{
				RequestID:     rr.ID,
				ProblemID:     p.ProblemID,
				ComplaintText: p.ComplaintText,
			})
			if err != nil {
				return fmt.Errorf("insert request problem (problem_id=%d): %w", p.ProblemID, err)
			}
			subs = append(subs, row)
		}
		return nil
	})
	if err != nil {
		return RegradeRequest{}, nil, err
	}
	return rr, subs, nil
}

// FileAndHandOffRegradeRequestV2 is FileRegradeRequestV2 immediately followed by
// MarkRequestHandedOff, in the SAME transaction (whole-branch review F5, spec §6
// D60's handoff race pattern). Before this method existed, the webhook path called
// FileRegradeRequestV2 (commits alone, winning the (publish_item_id, turn) slot via
// the partial unique index as kind='filed') and then MarkRequestHandedOff in a
// SEPARATE call — if that second call errored (e.g. a dropped connection between the
// two round-trips), the row was left as a live kind='filed' request at turn MAX+1,
// which handleSendResult (no turn<=MAX guard) would treat as an ordinary adjudicable
// filed request and send an ordinary result email #(MAX+1) for, rather than the
// handoff having actually happened. Wrapping both statements in one WithTx means
// either the whole handoff lands (filed insert + sub-items + the flip to
// handed_off) or none of it does — a failure anywhere rolls back the insert too,
// leaving no adjudicable MAX+1 filed row behind. IsUniqueViolation(err) on the
// insert still correctly reports the D57 slot race (the flip never runs in that
// case) so callers keep their existing "loser records an addendum" branch.
func (s *Store) FileAndHandOffRegradeRequestV2(ctx context.Context, arg FileRegradeRequestParams) (RegradeRequest, []RegradeRequestProblem, error) {
	if arg.Kind == "filed" && (arg.Turn == 0 || arg.PublishItemID == 0) {
		return RegradeRequest{}, nil, fmt.Errorf("FileAndHandOffRegradeRequestV2: kind=filed requires a non-zero PublishItemID and Turn (got PublishItemID=%d, Turn=%d)", arg.PublishItemID, arg.Turn)
	}

	var rr RegradeRequest
	subs := make([]RegradeRequestProblem, 0, len(arg.Problems))
	err := s.WithTx(ctx, func(q *db.Queries) error {
		var err error
		rr, err = q.InsertRegradeRequestV2(ctx, db.InsertRegradeRequestV2Params{
			PublishItemID: pgtype.Int8{Int64: arg.PublishItemID, Valid: arg.PublishItemID != 0},
			StudentID:     pgtype.Int8{Int64: arg.StudentID, Valid: arg.StudentID != 0},
			AssessmentID:  pgtype.Int8{Int64: arg.AssessmentID, Valid: arg.AssessmentID != 0},
			FromEmail:     arg.FromEmail,
			SpfVerdict:    pgtype.Text{String: arg.SPFVerdict, Valid: arg.SPFVerdict != ""},
			DkimVerdict:   pgtype.Text{String: arg.DKIMVerdict, Valid: arg.DKIMVerdict != ""},
			Subject:       arg.Subject,
			Body:          arg.Body,
			Status:        arg.Status,
			MessageID:     pgtype.Text{String: arg.MessageID, Valid: arg.MessageID != ""},
			Kind:          arg.Kind,
			Turn:          pgtype.Int4{Int32: int32(arg.Turn), Valid: arg.Turn != 0},
		})
		if err != nil {
			return fmt.Errorf("insert regrade request: %w", err)
		}
		for _, p := range arg.Problems {
			row, err := q.InsertRequestProblem(ctx, db.InsertRequestProblemParams{
				RequestID:     rr.ID,
				ProblemID:     p.ProblemID,
				ComplaintText: p.ComplaintText,
			})
			if err != nil {
				return fmt.Errorf("insert request problem (problem_id=%d): %w", p.ProblemID, err)
			}
			subs = append(subs, row)
		}
		rr, err = q.MarkRequestHandedOff(ctx, rr.ID)
		if err != nil {
			return fmt.Errorf("mark handed off: %w", err)
		}
		return nil
	})
	if err != nil {
		return RegradeRequest{}, nil, err
	}
	return rr, subs, nil
}

// ConsumedTokenExists reports whether this (item, turn) token has already been CONSUMED —
// by a filed request OR by an already-fired final-turn handoff (spec §4 D57). This is
// the webhook's actual consumed-token pre-check (used at rung "filing" — see
// httpapi.walkRegradeChain): a 'filed'-only pre-check would wrongly let a second reply
// to an already-fired MAX+1 handoff token consume it again, since MarkRequestHandedOff
// flips the consuming row out of the partial unique index's WHERE kind='filed' (freeing
// the raw index slot) — this treats both 'filed' and 'handed_off' as consumed to close
// that gap. The partial unique index remains the actual race-killer for the concurrent
// case (two racing filers can both pass this read and then race on the INSERT — the
// loser must handle IsUniqueViolation regardless); this is the cheap pre-check for the
// common already-consumed case.
func (s *Store) ConsumedTokenExists(ctx context.Context, itemID int64, turn int) (bool, error) {
	return s.Q.ConsumedTokenExists(ctx, db.ConsumedTokenExistsParams{
		PublishItemID: pgtype.Int8{Int64: itemID, Valid: itemID != 0},
		Turn:          pgtype.Int4{Int32: int32(turn), Valid: turn != 0},
	})
}

// NextOpenTurn reports the live chain's next open turn slot for a publish item: max
// consumed turn + 1 over the slot-consuming kinds ('filed', 'handed_off' — the same set
// ConsumedTokenExists checks), 1 for a chain with no filings yet. The webhook's rung-2
// re-bind remaps a superseded chain's token here instead of preserving its stale turn
// (wave-5 verifier finding: a preserved stale turn consumed the live chain's future slot
// and stranded the student's later legitimate reply as a silent addendum). Advisory read
// like ConsumedTokenExists — the partial unique index remains the race arbiter.
func (s *Store) NextOpenTurn(ctx context.Context, itemID int64) (int, error) {
	n, err := s.Q.NextOpenTurn(ctx, pgtype.Int8{Int64: itemID, Valid: itemID != 0})
	return int(n), err
}

// ListRegradeRequestsFilters selects the regrade queue. Zero values mean "no filter";
// Limit<=0 defaults to a sane page size so callers can't accidentally request
// everything. Kind (migration 0025) lets the queue's "Unparsed" filter (spec §7 D62)
// and similar views scope to one kind. StudentID matches the external student ID
// by case-insensitive prefix (the queue is server-paginated, so search must be too).
type ListRegradeRequestsFilters struct {
	Status       string
	AssessmentID int64
	Kind         string
	StudentID    string
	// OnlyOpen narrows to the open status group: received | under_review (HCI audit,
	// regrades-list correctness). The UI's default "Actionable" queue is
	// Kind=filed + OnlyOpen — a compound the single-value Status filter can't
	// express; filtering server-side keeps page slices and the pager total computed
	// over the same set the tab shows.
	OnlyOpen bool
	// OnlyUndeliveredResult narrows to the resolved-but-never-delivered recovery set
	// (migration 0026): kind='filed' AND status IN (resolved_upheld,
	// resolved_regraded) AND result_sent_at IS NULL — exactly the rows
	// resend-result's guard accepts. kind='filed' is part of the predicate itself
	// (not left to the Kind filter) because a reminded unparsed row is also
	// resolved_upheld with a NULL result_sent_at, yet no result email was ever owed.
	OnlyUndeliveredResult bool
	Limit                 int
	Offset                int
}

// onlyFlag maps a presence-only bool filter onto its true-or-NULL narg.
func onlyFlag(set bool) pgtype.Bool {
	return pgtype.Bool{Bool: true, Valid: set}
}

// likeEscaper neutralizes LIKE/ILIKE wildcards in user input so a search for
// "b%" matches a literal percent sign instead of everything.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// studentPrefix builds the ILIKE pattern for StudentID ("" => no filter).
func (f ListRegradeRequestsFilters) studentPrefix() pgtype.Text {
	if f.StudentID == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: likeEscaper.Replace(f.StudentID) + "%", Valid: true}
}

// ListRegradeRequests returns the queue view (joined with student/assessment names
// for display), newest first, paginated.
func (s *Store) ListRegradeRequests(ctx context.Context, f ListRegradeRequestsFilters) ([]db.ListRegradeRequestsRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	return s.Q.ListRegradeRequests(ctx, db.ListRegradeRequestsParams{
		Status:                pgtype.Text{String: f.Status, Valid: f.Status != ""},
		AssessmentID:          pgtype.Int8{Int64: f.AssessmentID, Valid: f.AssessmentID != 0},
		Kind:                  pgtype.Text{String: f.Kind, Valid: f.Kind != ""},
		StudentPrefix:         f.studentPrefix(),
		OnlyOpen:              onlyFlag(f.OnlyOpen),
		OnlyUndeliveredResult: onlyFlag(f.OnlyUndeliveredResult),
		RowLimit:              int32(limit),
		RowOffset:             int32(f.Offset),
	})
}

// CountRegradeRequests returns the total the same filters would match across all
// pages — the pager total. Shares ListRegradeRequests' WHERE clause by design.
func (s *Store) CountRegradeRequests(ctx context.Context, f ListRegradeRequestsFilters) (int64, error) {
	return s.Q.CountRegradeRequests(ctx, db.CountRegradeRequestsParams{
		Status:                pgtype.Text{String: f.Status, Valid: f.Status != ""},
		AssessmentID:          pgtype.Int8{Int64: f.AssessmentID, Valid: f.AssessmentID != 0},
		Kind:                  pgtype.Text{String: f.Kind, Valid: f.Kind != ""},
		StudentPrefix:         f.studentPrefix(),
		OnlyOpen:              onlyFlag(f.OnlyOpen),
		OnlyUndeliveredResult: onlyFlag(f.OnlyUndeliveredResult),
	})
}

// GetRegradeRequest fetches one queue item by id.
func (s *Store) GetRegradeRequest(ctx context.Context, id int64) (RegradeRequest, error) {
	return s.Q.GetRegradeRequest(ctx, id)
}

// ResolveRegradeRequest closes out a queue item: outcome is "resolved_upheld" or
// "resolved_regraded", resolverID + note are recorded. Unchanged from v1 (F2's atomic
// status guard still applies).
func (s *Store) ResolveRegradeRequest(ctx context.Context, id int64, outcome string, resolverID int64, note string) (RegradeRequest, error) {
	return s.Q.ResolveRegradeRequest(ctx, db.ResolveRegradeRequestParams{
		ID:             id,
		Status:         outcome,
		ResolverID:     pgtype.Int8{Int64: resolverID, Valid: resolverID != 0},
		ResolutionNote: note,
	})
}

// MarkRequestHandedOff records that this request's reply consumed the final-turn
// (handoff) token (spec §6 D60): kind becomes 'handed_off'. Callers separately notify
// each contested problem's assigned TA — this method only flips the schema state.
func (s *Store) MarkRequestHandedOff(ctx context.Context, id int64) (RegradeRequest, error) {
	return s.Q.MarkRequestHandedOff(ctx, id)
}

// SetRegradeStatus flips a request's status without the resolve path's TOCTOU guard —
// used for the intermediate under_review transition. NOTE (whole-branch review F3):
// an earlier design had send-result compensating-revert this back to under_review on a
// provider send failure; that was REVERTED (spec §5, D59) in favor of the atomic
// flip-before-send staying resolved on failure, recoverable via the dedicated
// POST /api/regrades/{id}/resend-result path (migration 0026, result_sent_at) instead
// of reopening the row. Production code never calls this for that case anymore — the
// resolve path always goes through ResolveRegradeRequest's guarded UPDATE.
func (s *Store) SetRegradeStatus(ctx context.Context, id int64, status string) (RegradeRequest, error) {
	return s.Q.SetRegradeStatus(ctx, db.SetRegradeStatusParams{ID: id, Status: status})
}

// SetRegradeResultSentAt records that result email #N was actually DELIVERED for this
// request (whole-branch review F1, migration 0026): called only after a successful
// provider send, by both the original send-result handler and the resend-result
// recovery route. A resolved request with result_sent_at still NULL is exactly the
// "send failed after the atomic flip, never recovered" state resend-result's guard
// looks for.
func (s *Store) SetRegradeResultSentAt(ctx context.Context, id int64) (RegradeRequest, error) {
	return s.Q.SetRegradeResultSentAt(ctx, id)
}

// RequestProblemInput is one contested problem to attach to a request (spec §5 D59):
// the problem being contested plus its complaint text (concatenated in arrival order
// by the caller/parser for duplicate <pN> blocks — this layer just stores what it's
// given).
type RequestProblemInput struct {
	ProblemID     int64
	ComplaintText string
}

// InsertRequestProblems attaches one or more sub-items to a request in a single
// transaction (spec §5 D59) — used both for the initial parse-driven fan-out (one
// call per <pN> block that matched a real problem) and the TA escape-hatch
// add/correct path (§5, one problem at a time). UNIQUE(request_id, problem_id)
// (migration 0025) rejects a duplicate problem on the same request; the caller sees
// that as IsUniqueViolation on this call and the whole batch rolls back — a
// deliberate all-or-nothing insert so a request never ends up with only some of its
// contested problems recorded.
func (s *Store) InsertRequestProblems(ctx context.Context, requestID int64, problems []RequestProblemInput) ([]RegradeRequestProblem, error) {
	out := make([]RegradeRequestProblem, 0, len(problems))
	err := s.WithTx(ctx, func(q *db.Queries) error {
		for _, p := range problems {
			row, err := q.InsertRequestProblem(ctx, db.InsertRequestProblemParams{
				RequestID:     requestID,
				ProblemID:     p.ProblemID,
				ComplaintText: p.ComplaintText,
			})
			if err != nil {
				return fmt.Errorf("insert request problem (problem_id=%d): %w", p.ProblemID, err)
			}
			out = append(out, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListRequestProblems returns every sub-item on a request, ordered by problem_id.
func (s *Store) ListRequestProblems(ctx context.Context, requestID int64) ([]RegradeRequestProblem, error) {
	return s.Q.ListRequestProblems(ctx, requestID)
}

// GetRequestProblem fetches one sub-item by id.
func (s *Store) GetRequestProblem(ctx context.Context, id int64) (RegradeRequestProblem, error) {
	return s.Q.GetRequestProblem(ctx, id)
}

// SetProblemVerdictParams is the input to SetProblemVerdict.
type SetProblemVerdictParams struct {
	SubItemID int64
	// Verdict must be "upheld" or "regraded" — the database CHECK (migration 0025)
	// rejects anything else.
	Verdict   string
	Note      string
	VerdictBy int64
}

// SetProblemVerdict records a TA's adjudication of one sub-item (spec §5): outcome,
// note, who, and when (verdict_at is set to now() by the query).
func (s *Store) SetProblemVerdict(ctx context.Context, arg SetProblemVerdictParams) (RegradeRequestProblem, error) {
	return s.Q.SetProblemVerdict(ctx, db.SetProblemVerdictParams{
		ID:          arg.SubItemID,
		Verdict:     pgtype.Text{String: arg.Verdict, Valid: arg.Verdict != ""},
		VerdictNote: arg.Note,
		VerdictBy:   pgtype.Int8{Int64: arg.VerdictBy, Valid: arg.VerdictBy != 0},
	})
}

// SetProblemVerdictAndAdoptionParams is the input to SetProblemVerdictAndAdoption.
type SetProblemVerdictAndAdoptionParams struct {
	SubItemID int64
	// Verdict must be "upheld" or "regraded" — the database CHECK (migration 0025)
	// rejects anything else.
	Verdict   string
	Note      string
	VerdictBy int64
	// AdoptedRecordID is the record a "regraded" verdict adopts as this turn's overlay
	// grade; 0 stores SQL NULL (an "upheld" verdict adopts nothing).
	AdoptedRecordID int64
}

// SetProblemVerdictAndAdoption records a TA's adjudication AND its overlay in ONE atomic
// statement (regrade-round correctness fix): verdict + note + who + when + adopted
// record together, so a partial failure can never leave a verdict without its adoption
// (or, worse, a flip to 'upheld' still carrying a stale adopted_record_id that overlay
// consumers would apply). Replaces the old SetProblemVerdict-then-SetSubItemAdoptedRecord
// two-write sequence in the PATCH-verdict handler.
func (s *Store) SetProblemVerdictAndAdoption(ctx context.Context, arg SetProblemVerdictAndAdoptionParams) (RegradeRequestProblem, error) {
	return s.Q.SetProblemVerdictAndAdoption(ctx, db.SetProblemVerdictAndAdoptionParams{
		ID:              arg.SubItemID,
		Verdict:         pgtype.Text{String: arg.Verdict, Valid: arg.Verdict != ""},
		VerdictNote:     arg.Note,
		VerdictBy:       pgtype.Int8{Int64: arg.VerdictBy, Valid: arg.VerdictBy != 0},
		AdoptedRecordID: pgtype.Int8{Int64: arg.AdoptedRecordID, Valid: arg.AdoptedRecordID != 0},
	})
}

// SetProblemAIRecord links the per-sub-item AI re-grade result (spec §5: AI assist
// re-scopes to one sub-item per job, so this replaces v1's request-level
// SetRegradeAIRecord). Never touches verdict; clears any stale ai_error so a
// successful re-grade doesn't leave an old failure reason visible next to the freshly
// linked record.
func (s *Store) SetProblemAIRecord(ctx context.Context, subItemID, recordID int64) (RegradeRequestProblem, error) {
	return s.Q.SetProblemAIRecord(ctx, db.SetProblemAIRecordParams{
		ID:         subItemID,
		AiRecordID: pgtype.Int8{Int64: recordID, Valid: recordID != 0},
	})
}

// SetProblemAIError persists a terminal AI re-grade failure reason on one sub-item: a
// short constant string only (e.g. "AI unavailable — provider removed") — NEVER
// student or request text (CLAUDE.md PII rule). Never touches verdict or
// ai_record_id.
func (s *Store) SetProblemAIError(ctx context.Context, subItemID int64, reason string) (RegradeRequestProblem, error) {
	return s.Q.SetProblemAIError(ctx, db.SetProblemAIErrorParams{
		ID:      subItemID,
		AiError: pgtype.Text{String: reason, Valid: reason != ""},
	})
}

// AllProblemsVerdicted reports whether EVERY sub-item on a request has a verdict
// (spec §5: the send-result gate, 409 until true). A request with zero sub-items is
// NOT considered all-verdicted — there is nothing to send a result about, so the gate
// must stay closed rather than vacuously passing.
//
// Advisory only: the two count queries below are not run in a single transaction/snapshot,
// so a concurrent verdict between them could change the answer between this call
// returning and the caller acting on it. That's fine — this method exists to give
// callers (e.g. a UI's "can I send yet" check) a cheap non-authoritative read; the
// send-result handler's 409 gate is the authoritative check and must re-verify at
// write time rather than trusting a prior call to this method.
func (s *Store) AllProblemsVerdicted(ctx context.Context, requestID int64) (bool, error) {
	total, err := s.Q.CountRequestProblems(ctx, requestID)
	if err != nil {
		return false, fmt.Errorf("count request problems: %w", err)
	}
	if total == 0 {
		return false, nil
	}
	unverdicted, err := s.Q.CountUnverdictedProblems(ctx, requestID)
	if err != nil {
		return false, fmt.Errorf("count unverdicted problems: %w", err)
	}
	return unverdicted == 0, nil
}

// AssignProblemTA sets (or replaces) the TA assigned to a problem (spec §6 D60: at
// most one TA per problem — assigning a new one to an already-assigned problem
// replaces the row via ON CONFLICT, it does not error). assignedBy is the acting
// lecturer/admin's user id; 0 stores SQL NULL (unknown/system).
func (s *Store) AssignProblemTA(ctx context.Context, problemID, userID, assignedBy int64) (ProblemTAAssignment, error) {
	return s.Q.AssignProblemTA(ctx, db.AssignProblemTAParams{
		ProblemID:  problemID,
		UserID:     userID,
		AssignedBy: pgtype.Int8{Int64: assignedBy, Valid: assignedBy != 0},
	})
}

// RemoveProblemTA unassigns a problem's TA (spec §6 D60 UI: "assign/unassign TA").
// A problem with no assignment row is simply "no TA" — deleting an already-unassigned
// problem is a no-op, not an error.
func (s *Store) RemoveProblemTA(ctx context.Context, problemID int64) error {
	return s.Q.RemoveProblemTA(ctx, problemID)
}

// GetProblemTA fetches one problem's TA assignment. Returns pgx.ErrNoRows if
// unassigned — callers use ListTAAssignments instead when they need the "no TA
// assigned" case to render as a value rather than an error.
func (s *Store) GetProblemTA(ctx context.Context, problemID int64) (ProblemTAAssignment, error) {
	return s.Q.GetProblemTA(ctx, problemID)
}

// ListTAAssignments returns every problem in an assessment with its TA assignment, if
// any (spec §6: assignment state visible on regrade rows; publish preview's
// unassigned-problem warning; handoff's "no TA assigned" flag, spec §6). Unassigned
// problems still appear, with NULL assignment fields.
func (s *Store) ListTAAssignments(ctx context.Context, assessmentID int64) ([]db.ListTAAssignmentsRow, error) {
	return s.Q.ListTAAssignments(ctx, assessmentID)
}

// PriorProblemVerdicts returns the verdict history for one problem (by NUMBER) across
// every filed request of a publish item's token chain (spec §6 handoff history). turn
// order; NULL verdicts (still-open prior turns) are included and skipped by the caller.
func (s *Store) PriorProblemVerdicts(ctx context.Context, itemID int64, problemNumber int32) ([]db.PriorProblemVerdictsRow, error) {
	return s.Q.PriorProblemVerdicts(ctx, db.PriorProblemVerdictsParams{
		PublishItemID: pgtype.Int8{Int64: itemID, Valid: itemID != 0},
		Number:        problemNumber,
	})
}

// ContestedAnswerForSubItem resolves the single contested official answer a sub-item's
// AI re-grade re-examines (spec §8 per-sub-item re-scope), with the contested record's
// pins. 0 or 1 row (a student has at most one answer per problem); an empty slice means
// nothing official to re-examine — the AI job fails open with a terminal ai_error.
// excludeSubItemID names the sub-item being adjudicated NOW so it never briefs its
// re-grade against its own prior adoption (regrade-round correctness fix); pass 0 to
// exclude nothing (no sub-item has id 0).
func (s *Store) ContestedAnswerForSubItem(ctx context.Context, assessmentID, studentID, problemID, excludeSubItemID int64) ([]db.ContestedAnswerForSubItemRow, error) {
	return s.Q.ContestedAnswerForSubItem(ctx, db.ContestedAnswerForSubItemParams{
		AssessmentID: assessmentID, StudentID: studentID, ProblemID: problemID,
		ExcludeSubItemID: excludeSubItemID,
	})
}

// ListEligibleAIRegradeSubItems enumerates the "AI re-grade all" enqueue set as SUB-ITEMS
// (spec §8): every contested problem of an open filed request in the assessment with no
// AI record yet, paired with the contested record's provider/model for the cost estimate.
func (s *Store) ListEligibleAIRegradeSubItems(ctx context.Context, assessmentID int64) ([]db.ListEligibleAIRegradeSubItemsRow, error) {
	return s.Q.ListEligibleAIRegradeSubItems(ctx, pgtype.Int8{Int64: assessmentID, Valid: assessmentID != 0})
}

// CountSkippedAIRegradeSubItems counts sub-items the batch skips (already carry an AI
// record) — reported alongside the enqueued count (spec §8).
func (s *Store) CountSkippedAIRegradeSubItems(ctx context.Context, assessmentID int64) (int64, error) {
	return s.Q.CountSkippedAIRegradeSubItems(ctx, pgtype.Int8{Int64: assessmentID, Valid: assessmentID != 0})
}
