package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/auth"
	"github.com/HaoWen46/adagrade/internal/domain"
	"github.com/HaoWen46/adagrade/internal/email"
	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/regrade"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// Phase 7 / regrade v2 (spec 2026-07-03-regrade-v2-design.md §2-§8): the inbound email
// webhook + single-use turn-token chain, multi-problem filing, per-problem adjudication,
// TA-clicked gated result email, unparsed-row reminder, final-turn handoff, and the
// per-sub-item AI re-scope. The webhook route is registered OUTSIDE session auth (see
// api.go, mirroring the OAuth callback); the queue routes below are normal session-authed
// /api/ routes. PII rule (CLAUDE.md): regrade body/complaint text, subject, from-address,
// and TA-notify bodies are student content — never logged, only counts/ids/statuses/kinds.

// maxInboundWebhookBody bounds the inbound webhook body (F5 per-route limit,
// mirroring json.go's maxJSONBody but sized for a full inbound-email JSON payload,
// which is larger than an API request body — it carries the student's reply text and
// provider header dump).
const maxInboundWebhookBody = 5 << 20 // 5 MiB

// Regrade request status vocabulary — the values the DB CHECK admits. The v2 chain
// keeps the received/under_review/resolved_* lifecycle for a FILED request's
// adjudication, plus the ladder rejection statuses (unchanged from v1: v1/garbage
// tokens, superseded, sender mismatch). The v2 KIND column (filed|addendum|unparsed|
// handed_off) is orthogonal to status and lives on the same row.
const (
	regradeStatusReceived         = "received"
	regradeStatusUnderReview      = "under_review"
	regradeStatusResolvedUpheld   = "resolved_upheld"
	regradeStatusResolvedRegraded = "resolved_regraded"
	regradeStatusRejectedBadToken = "rejected_bad_token"
	regradeStatusRejectedSupersed = "rejected_superseded"
	regradeStatusRejectedSender   = "rejected_sender_mismatch"
	regradeStatusRejectedDeadline = "rejected_deadline"
)

// Request kinds (spec §2-§4, migration 0025). The webhook path always knows which
// bucket a reply landed in before recording it.
const (
	regradeKindFiled     = "filed"
	regradeKindAddendum  = "addendum"
	regradeKindUnparsed  = "unparsed"
	regradeKindHandedOff = "handed_off"
)

// handleInboundWebhook is POST /webhooks/email/inbound/{secret} (spec §4): the email
// provider's inbound webhook. No session auth — the path secret IS the auth,
// constant-time compared against config so a timing side-channel can't help an
// attacker guess it; on ANY mismatch (including an unconfigured secret) this 404s
// rather than 403s, so the route's very existence isn't an oracle.
func (s *Server) handleInboundWebhook(w http.ResponseWriter, r *http.Request) {
	secret := s.cfg.InboundWebhookSecret
	given := r.PathValue("secret")
	// An empty configured secret must never "match" an empty path segment — constant-
	// time compare of two empty strings is trivially equal, so guard explicitly.
	if secret == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(given)) != 1 {
		http.NotFound(w, r)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxInboundWebhookBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		// Over the size limit or a genuine read failure: nothing to record (we don't
		// even have a From address yet) — reject without a DB row or a reply, same as
		// any other malformed delivery.
		apiError(w, http.StatusBadRequest, "could not read request body")
		return
	}

	if s.email == nil {
		// No provider configured to parse the payload — same shape as a config error;
		// fail closed without a reply (no-backscatter).
		apiError(w, http.StatusServiceUnavailable, "inbound email processing unavailable")
		return
	}
	in, err := s.email.ParseInbound(raw)
	if err != nil {
		// Unparseable payload: not even a From address we can trust. Nothing useful to
		// record against a student/assessment, and definitely no reply.
		apiError(w, http.StatusBadRequest, "invalid inbound payload")
		return
	}

	s.walkRegradeChain(r, in)
	// The provider only cares that we accepted the delivery; chain outcome (filed or
	// any rejection/kind) is never reflected in the HTTP response — that would leak
	// verification state to whoever can reach this endpoint.
	w.WriteHeader(http.StatusOK)
}

// walkRegradeChain runs the v2 single-use turn-token chain against one parsed inbound
// email (spec §3-§6), recording the outcome on a regrade_requests row at every exit
// point. It sends a filing confirmation ONLY when a request actually files (≥1 valid
// block). No reply is ever sent for a rejected/unparsed/addendum message
// (no-backscatter). The rungs, in order:
//
//	rung 1  VerifyTokenV2 (v1/garbage → rejected_bad_token, no oracle) → (item, turn)
//	rung 2  batch superseded → re-bind to the live item (C3), REMAPPING the turn to the
//	        live chain's next open slot (capped at MAX+1 so handoff semantics survive)
//	rung 3  sender == current roster email (case-insensitive) → else rejected_sender
//	        (SPF/DKIM verdicts recorded, warn-not-block, unchanged)
//	filing  consumed token (ConsumedTokenExists) → addendum row, silence
//	        turn == MAX+1 → handoff (§6): handed_off + grouped TA-notify + audits
//	        ParseBlocks ≥1 valid → file request + sub-items in one tx, consume via the
//	                unique index; on a 23505 race the loser records an addendum
//	        0 valid blocks → unparsed row, token NOT consumed, silence
func (s *Server) walkRegradeChain(r *http.Request, in domain.InboundEmail) {
	ctx := r.Context()
	spfVerdict, dkimVerdict := verdictString(in.SPFPass), verdictString(in.DKIMValid)

	// record inserts a non-filing outcome row (rejected/addendum/unparsed). Filing
	// rows go through fileRequest (which needs the tx-backed sub-item insert + unique
	// index). turn==0 stores NULL; publishItemID/studentID/assessmentID==0 store NULL.
	record := func(publishItemID, studentID, assessmentID int64, status, kind string, turn int) {
		if _, err := s.store.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
			PublishItemID: publishItemID,
			StudentID:     studentID,
			AssessmentID:  assessmentID,
			FromEmail:     in.From,
			SPFVerdict:    spfVerdict,
			DKIMVerdict:   dkimVerdict,
			Subject:       in.Subject,
			Body:          in.TextBody,
			Status:        status,
			MessageID:     in.MessageID,
			Kind:          kind,
			Turn:          turn,
		}); err != nil {
			if in.MessageID != "" && store.IsUniqueViolation(err) {
				// F1: a Postmark retry of THIS EXACT delivery collides with the
				// message_id partial unique index — already processed, treat as done.
				s.log.Info("regrade: duplicate webhook delivery ignored", "message_id_present", true)
				return
			}
			s.log.Error("regrade: record request failed", "status", status, "kind", kind, "err", err)
		}
	}

	// Rung 1: token parses + HMAC valid + not expired + turn>=1 (v2 only; v1/garbage
	// rejected). No oracle — status is recorded, HTTP is always 200.
	itemID, turn, err := email.VerifyTokenV2(s.tokenKey, in.MailboxHash, time.Now())
	if err != nil {
		record(0, 0, 0, regradeStatusRejectedBadToken, regradeKindUnparsed, 0)
		s.log.Info("regrade: rejected", "reason", regradeStatusRejectedBadToken)
		return
	}

	item, err := s.store.Q.GetPublishItemForSend(ctx, itemID)
	if err != nil {
		// The token verified but names a publish_item that no longer exists — treat the
		// same as a bad token: nothing legitimate to attach the request to.
		record(0, 0, 0, regradeStatusRejectedBadToken, regradeKindUnparsed, 0)
		s.log.Info("regrade: rejected", "reason", regradeStatusRejectedBadToken, "detail", "item not found")
		return
	}

	// Rung 2: batch not superseded. When the token's batch WAS superseded (an
	// unpublish → re-publish cycle), don't kill the request outright (C3) — RE-BIND to
	// the same student's item in the assessment's current LIVE (non-superseded) batch,
	// if one exists, and continue against that live item. The re-bind REMAPS the
	// token's turn to the live chain's next open slot (wave-5 verifier finding): the
	// stale turn belongs to the OLD chain's position, and preserving it let a replayed
	// old turn-2 token consume (live item, turn 2) before the live chain ever sent
	// result #1 — the student's later legitimate turn-2 reply then hit the
	// consumed-token check and died as a silent addendum with no escape hatch. The
	// remapped turn is capped at MAX+1 (the handoff slot) so the turn-budget policy
	// survives: a stale replay against a fully-consumed live chain lands on the
	// already-consumed handoff slot (→ addendum) rather than minting a fresh turn past
	// MAX+1 and firing a second handoff. Keyed by the SAME (assessment_id, student_id)
	// the verified token already identified, so it can never cross to another student.
	if item.BatchSuperseded {
		live, err := s.store.Q.LiveBatchItemForStudent(ctx, db.LiveBatchItemForStudentParams{
			AssessmentID: item.AssessmentID,
			StudentID:    item.StudentID,
		})
		if err != nil {
			record(item.ID, item.StudentID, item.AssessmentID, regradeStatusRejectedSupersed, regradeKindUnparsed, turn)
			s.log.Info("regrade: rejected", "reason", regradeStatusRejectedSupersed, "item_id", item.ID)
			return
		}
		rebound, err := s.store.Q.GetPublishItemForSend(ctx, live.ID)
		if err != nil {
			record(item.ID, item.StudentID, item.AssessmentID, regradeStatusRejectedSupersed, regradeKindUnparsed, turn)
			s.log.Info("regrade: rejected", "reason", regradeStatusRejectedSupersed, "item_id", item.ID)
			return
		}
		nextTurn, err := s.store.NextOpenTurn(ctx, rebound.ID)
		if err != nil {
			s.log.Error("regrade: next-open-turn lookup failed", "item_id", rebound.ID, "err", err)
			return
		}
		if cap := s.regradeMaxTurns() + 1; nextTurn > cap {
			nextTurn = cap
		}
		s.log.Info("regrade: re-bound superseded token to live item",
			"from_item_id", item.ID, "to_item_id", rebound.ID, "token_turn", turn, "turn", nextTurn)
		item = rebound
		turn = nextTurn
	}

	// Rung 3: sender email == CURRENT roster email for the token's student
	// (case-insensitive; a roster-changed email invalidates old addresses, B-H10). Live
	// students.email, not the recipient_email frozen on the publish item at send time.
	student, err := s.store.Q.GetStudent(ctx, item.StudentID)
	if err != nil || !strings.EqualFold(strings.TrimSpace(student.Email), strings.TrimSpace(in.From)) {
		record(item.ID, item.StudentID, item.AssessmentID, regradeStatusRejectedSender, regradeKindUnparsed, turn)
		s.log.Info("regrade: rejected", "reason", regradeStatusRejectedSender, "item_id", item.ID)
		return
	}

	// Rung 3.5 — regrade deadline (rounds design): each exam closes its regrade
	// window on its own date. Past-deadline replies are recorded (auditable, shown
	// in the inbox as rejected) but never file, never burn a turn.
	if a, err := s.store.Q.GetAssessment(ctx, item.AssessmentID); err == nil &&
		a.RegradeDeadline.Valid && time.Now().After(a.RegradeDeadline.Time) {
		record(item.ID, item.StudentID, item.AssessmentID, regradeStatusRejectedDeadline, regradeKindUnparsed, turn)
		s.log.Info("regrade: rejected", "reason", regradeStatusRejectedDeadline, "item_id", item.ID)
		return
	}

	// SPF/DKIM verdicts are recorded (warn-not-block, v0) — carried through record/file
	// below for a human to review, never a gate.

	// Consumed-token check (spec §4 D57): if this (item, turn) token has already been
	// consumed — by a filed request OR by an already-fired final-turn handoff — this is a
	// later reply to an already-consumed token: record an addendum (dimmed under the
	// consuming request), total silence, no turn burned. ConsumedTokenExists checks BOTH
	// kinds deliberately: MarkRequestHandedOff flips the consuming row out of the partial
	// index's WHERE kind='filed', so a filed-only check would let a second reply to the
	// MAX+1 handoff token consume it again. The partial unique index remains the
	// authority under a live race; this is the cheap pre-check for the common
	// already-consumed case.
	if consumed, err := s.store.ConsumedTokenExists(ctx, item.ID, turn); err != nil {
		s.log.Error("regrade: consumed-token check failed", "item_id", item.ID, "turn", turn, "err", err)
		return
	} else if consumed {
		record(item.ID, item.StudentID, item.AssessmentID, regradeStatusReceived, regradeKindAddendum, turn)
		s.log.Info("regrade: addendum to consumed token", "item_id", item.ID, "turn", turn)
		return
	}

	// Turn budget (spec §4 D57): value read at receipt time; in-flight tokens carry
	// their turn, so a mid-term MAX change stays coherent per-thread. Turns 1..MAX
	// adjudicate. A token with turn == MAX+1 is the HANDOFF token minted on result
	// #MAX — consuming it (with ≥1 valid block) fires the handoff (§6), not adjudication.
	maxTurns := s.regradeMaxTurns()

	// Parse the reply into contested blocks (spec §2) and translate pN → problem_id
	// within the token's assessment (spec §3). Unknown-N blocks evaporate silently.
	problems, err := s.store.Q.ListProblems(ctx, item.AssessmentID)
	if err != nil {
		s.log.Error("regrade: list problems for translation failed", "err", err)
		return
	}
	numToID := make(map[int]int64, len(problems))
	for _, p := range problems {
		numToID[int(p.Number)] = p.ID
	}
	blocks := regrade.ParseBlocks(in.TextBody)
	type filedProblem struct {
		number    int
		problemID int64
		text      string
	}
	filed := make([]filedProblem, 0, len(blocks))
	for _, b := range blocks {
		pid, ok := numToID[b.Number]
		if !ok {
			continue // unknown N — silently ignored (spec §3)
		}
		filed = append(filed, filedProblem{number: b.Number, problemID: pid, text: b.Text})
	}

	if len(filed) == 0 {
		// 0 valid blocks: nothing files, token NOT consumed (spec §4 D58 — consuming
		// without filing would strand the student with no result email and no next
		// token). Recorded as unparsed (live token) for the queue's Unparsed filter, total
		// silence.
		record(item.ID, item.StudentID, item.AssessmentID, regradeStatusReceived, regradeKindUnparsed, turn)
		s.log.Info("regrade: unparsed reply, token not consumed", "item_id", item.ID, "turn", turn)
		return
	}

	reqProblems := make([]store.RequestProblemInput, 0, len(filed))
	nums := make([]int, 0, len(filed))
	for _, f := range filed {
		reqProblems = append(reqProblems, store.RequestProblemInput{ProblemID: f.problemID, ComplaintText: f.text})
		nums = append(nums, f.number)
	}

	if turn > maxTurns {
		// Final-turn (handoff) token consumed with ≥1 valid block (spec §6 D60). RACE
		// PATTERN: insert as kind='filed' FIRST — winning the (item,turn) slot via the
		// partial unique index exactly like a normal filing — THEN flip to handed_off,
		// in the SAME transaction (whole-branch review F5: FileAndHandOffRegradeRequestV2
		// folds the filed insert + sub-items + the flip into one WithTx, so a failure
		// flipping to handed_off rolls back the filed insert too — no live kind='filed'
		// row is ever left sitting at turn MAX+1 for handleSendResult to later mistake for
		// an ordinary adjudicable request). Inserting directly as handed_off would bypass
		// the index's WHERE kind='filed' guard and let two concurrent final-turn replies
		// both "win". A 23505 here means a concurrent reply already claimed the slot →
		// record an addendum, no second handoff.
		handedOff, _, err := s.store.FileAndHandOffRegradeRequestV2(ctx, store.FileRegradeRequestParams{
			PublishItemID: item.ID, StudentID: item.StudentID, AssessmentID: item.AssessmentID,
			FromEmail: in.From, SPFVerdict: spfVerdict, DKIMVerdict: dkimVerdict,
			Subject: in.Subject, Body: in.TextBody,
			Status: regradeStatusReceived, MessageID: in.MessageID,
			Kind: regradeKindFiled, Turn: turn,
			Problems: reqProblems,
		})
		if err != nil {
			if store.IsUniqueViolation(err) {
				s.log.Info("regrade: handoff lost the (item,turn) race — recording addendum", "item_id", item.ID, "turn", turn)
				record(item.ID, item.StudentID, item.AssessmentID, regradeStatusReceived, regradeKindAddendum, turn)
				return
			}
			s.log.Error("regrade: file handoff request failed", "err", err)
			return
		}
		s.log.Info("regrade: handed off", "request_id", handedOff.ID, "item_id", item.ID, "turn", turn, "problems", len(filed))
		s.notifyHandoffTAs(r, handedOff, item, student, nums)
		return
	}

	// File the request (spec §3/§5): insert the request row (kind='filed', consuming
	// the token via the partial unique index) + its sub-items in one transaction (Finding
	// 2 fix — FileRegradeRequestV2 replaces the old two-step InsertRegradeRequestV2 +
	// InsertRequestProblems pair, which committed as two separate transactions and could
	// strand a consumed slot with zero sub-items on a failure in between), then send the
	// filing confirmation. A 23505 on the request insert means a concurrent reply already
	// filed this (item, turn) — the loser records an addendum instead of erroring (the
	// structural race-killer, D57).
	rr, _, err := s.store.FileRegradeRequestV2(ctx, store.FileRegradeRequestParams{
		PublishItemID: item.ID, StudentID: item.StudentID, AssessmentID: item.AssessmentID,
		FromEmail: in.From, SPFVerdict: spfVerdict, DKIMVerdict: dkimVerdict,
		Subject: in.Subject, Body: in.TextBody,
		Status: regradeStatusReceived, MessageID: in.MessageID,
		Kind: regradeKindFiled, Turn: turn,
		Problems: reqProblems,
	})
	if err != nil {
		if store.IsUniqueViolation(err) {
			// Either the (item, turn) filed-slot race (D57) or the F1 message_id retry.
			// Both mean "someone already filed / we already processed this" — record an
			// addendum for the slot race; for a pure F1 message_id retry the addendum
			// insert will itself collide and be ignored. Either way: no second confirmation.
			s.log.Info("regrade: file lost the (item,turn) race — recording addendum", "item_id", item.ID, "turn", turn)
			record(item.ID, item.StudentID, item.AssessmentID, regradeStatusReceived, regradeKindAddendum, turn)
			return
		}
		s.log.Error("regrade: file request failed", "err", err)
		return
	}
	s.log.Info("regrade: filed", "request_id", rr.ID, "item_id", item.ID, "turn", turn, "problems", len(filed))

	// Filing confirmation (automatic, spec §5): lists WHICH problem numbers filed
	// (numbers only) + attempt counter. No token / no Reply-To (physically inert).
	msg, err := email.RenderRegradeConfirmation(email.RegradeConfirmationData{
		AssessmentName:   item.AssessmentName,
		StudentName:      item.StudentName,
		ReceivedAt:       rr.ReceivedAt.Time,
		FiledProblemNums: nums,
		Turn:             turn,
		MaxTurns:         maxTurns,
	})
	if err != nil {
		s.log.Error("regrade: render confirmation failed", "request_id", rr.ID, "err", err)
		return
	}
	msg.To = student.Email
	if _, err := s.email.Send(ctx, msg); err != nil {
		s.log.Error("regrade: send confirmation failed", "request_id", rr.ID, "err", err)
	}
}

// regradeMaxTurns resolves ADAMARKER_REGRADE_MAX with a belt-and-suspenders default for
// a zero-value Config in tests (config.Load already defaults it to 3).
func (s *Server) regradeMaxTurns() int {
	m := s.cfg.RegradeMax
	if m <= 0 {
		m = 3
	}
	return m
}

// notifyHandoffTAs sends the person-to-person handoff email to each contested problem's
// assigned TA (spec §6 D60/D61). One email per assigned TA covering ALL of THIS student's
// contested problems that TA owns; problems with no assigned TA are flagged on the row
// (no email — no target). It emits a regrade.handoff audit per notified TA. This is
// deliberate PII-to-authorized-grader over email — the bodies are NEVER logged (only
// counts/ids). filedNums is the parsed problem numbers (for logging scope only).
func (s *Server) notifyHandoffTAs(r *http.Request, rr db.RegradeRequest, item db.GetPublishItemForSendRow, student db.Student, filedNums []int) {
	ctx := r.Context()
	if s.email == nil {
		return
	}
	subs, err := s.store.ListRequestProblems(ctx, rr.ID)
	if err != nil {
		s.log.Error("regrade: handoff list sub-items failed", "request_id", rr.ID, "err", err)
		return
	}

	// Group contested problems by their assigned TA. A problem with no assignment is
	// collected separately and flagged (lecturer-visible via the row's sub-items; no mail).
	type problemInfo struct {
		number    int32
		complaint string
	}
	byTA := map[int64][]problemInfo{}
	var unassigned []int32
	for _, sub := range subs {
		p, err := s.store.Q.GetProblem(ctx, sub.ProblemID)
		if err != nil {
			s.log.Error("regrade: handoff problem lookup failed", "request_id", rr.ID, "err", err)
			continue
		}
		assign, err := s.store.GetProblemTA(ctx, sub.ProblemID)
		if errors.Is(err, pgx.ErrNoRows) {
			unassigned = append(unassigned, p.Number)
			continue
		}
		if err != nil {
			s.log.Error("regrade: handoff TA lookup failed", "request_id", rr.ID, "err", err)
			continue
		}
		byTA[assign.UserID] = append(byTA[assign.UserID], problemInfo{number: p.Number, complaint: sub.ComplaintText})
	}
	if len(unassigned) > 0 {
		// Flag only — no PII in the log (numbers are not PII).
		s.log.Warn("regrade: handoff problems with no assigned TA", "request_id", rr.ID, "problem_numbers", unassigned)
	}

	// Stable TA order for deterministic sends/tests.
	taIDs := make([]int64, 0, len(byTA))
	for id := range byTA {
		taIDs = append(taIDs, id)
	}
	sort.Slice(taIDs, func(i, j int) bool { return taIDs[i] < taIDs[j] })

	// F4: an absolute deep link needs ADAMARKER_APP_BASE_URL configured — a bare
	// "/regrades/42" path (the old unconditional behavior) is dead in any mail
	// client. Unset ⇒ empty DeepLink; RenderTANotify drops the "Open in app" line
	// entirely rather than emit a dead link (the email still names the assessment
	// and problems either way).
	deepLink := ""
	if s.cfg.AppBaseURL != "" {
		deepLink = fmt.Sprintf("%s/regrades/%d", s.cfg.AppBaseURL, rr.ID)
	}
	for _, taID := range taIDs {
		taUser, err := s.store.Q.GetUserByID(ctx, taID)
		if err != nil {
			s.log.Error("regrade: handoff TA user lookup failed", "request_id", rr.ID, "err", err)
			continue
		}
		infos := byTA[taID]
		sort.Slice(infos, func(i, j int) bool { return infos[i].number < infos[j].number })
		tps := make([]email.TANotifyProblem, 0, len(infos))
		for _, pi := range infos {
			tps = append(tps, email.TANotifyProblem{
				Number:    int(pi.number),
				Complaint: pi.complaint,
				History:   s.priorTurnHistory(ctx, item.ID, pi.number),
			})
		}
		msg, err := email.RenderTANotify(email.TANotifyData{
			AssessmentName: item.AssessmentName,
			StudentName:    student.Name,
			StudentID:      student.StudentID,
			StudentEmail:   student.Email,
			Problems:       tps,
			DeepLink:       deepLink,
		})
		if err != nil {
			s.log.Error("regrade: render TA-notify failed", "request_id", rr.ID, "err", err)
			continue
		}
		msg.To = taUser.Email
		if _, err := s.email.Send(ctx, msg); err != nil {
			s.log.Error("regrade: send TA-notify failed", "request_id", rr.ID, "err", err)
			continue
		}
		s.audit(r, "regrade.handoff", "regrade_request", strconv.FormatInt(rr.ID, 10), map[string]any{
			"ta_user_id": taID,
			"problems":   len(infos),
		})
	}
}

// priorTurnHistory assembles a problem's verdict history across the prior FILED turns of
// this token chain (spec §6: full prior-turn history for that problem). It walks the
// (item, problem) sub-items of every earlier filed request, in turn order, surfacing each
// verdict+note. Best-effort: a lookup failure yields a shorter history, never a failed
// handoff. Complaint/note text is PII — never logged here.
func (s *Server) priorTurnHistory(ctx context.Context, itemID int64, problemNumber int32) []email.TANotifyHistoryEntry {
	rows, err := s.store.PriorProblemVerdicts(ctx, itemID, problemNumber)
	if err != nil {
		s.log.Error("regrade: prior-turn history lookup failed", "item_id", itemID, "err", err)
		return nil
	}
	out := make([]email.TANotifyHistoryEntry, 0, len(rows))
	for _, v := range rows {
		if !v.Verdict.Valid {
			continue // unverdicted prior turn — nothing to show
		}
		out = append(out, email.TANotifyHistoryEntry{
			Turn:    int(v.Turn.Int32),
			Verdict: v.Verdict.String,
			Note:    v.VerdictNote,
		})
	}
	return out
}

// verdictString stringifies a pass/fail bool for storage (SPF/DKIM verdicts are recorded,
// not enforced).
func verdictString(pass bool) string {
	if pass {
		return "pass"
	}
	return "fail"
}

// --- Regrade queue (spec §7-§8, session-authed) -------------------------------------

// regradeListJSON is one row of GET /api/regrades — enough for the queue table (no
// body text; that's detail-only). kind/turn drive the queue's filed/addendum/unparsed/
// handed_off grouping and the Unparsed filter (spec §7).
type regradeListJSON struct {
	ID                int64   `json:"id"`
	Status            string  `json:"status"`
	Kind              string  `json:"kind"`
	Turn              *int32  `json:"turn,omitempty"`
	AssessmentID      *int64  `json:"assessment_id,omitempty"`
	AssessmentName    *string `json:"assessment_name,omitempty"`
	StudentExternalID *string `json:"student_external_id,omitempty"`
	StudentName       *string `json:"student_name,omitempty"`
	// StudentWithdrawn flags a 停修 student's rows (roster-lifecycle plan 2026-07-10,
	// locked semantics (f)): the regrade channel stays open — nothing about the
	// verification ladder or adjudication changes — the queue only shows the status.
	// Computed live from the roster join; always false with no linked student.
	StudentWithdrawn bool    `json:"student_withdrawn"`
	ReceivedAt       *string `json:"received_at,omitempty"`
	Subject          string  `json:"subject"`
}

func toRegradeListJSON(row db.ListRegradeRequestsRow) regradeListJSON {
	out := regradeListJSON{
		ID: row.ID, Status: row.Status, Kind: row.Kind, Subject: row.Subject,
		StudentWithdrawn: row.StudentWithdrawn,
	}
	if row.Turn.Valid {
		out.Turn = &row.Turn.Int32
	}
	if row.AssessmentID.Valid {
		out.AssessmentID = &row.AssessmentID.Int64
	}
	if row.AssessmentName.Valid {
		out.AssessmentName = &row.AssessmentName.String
	}
	if row.StudentExternalID.Valid {
		out.StudentExternalID = &row.StudentExternalID.String
	}
	if row.StudentName.Valid {
		out.StudentName = &row.StudentName.String
	}
	if row.ReceivedAt.Valid {
		ts := row.ReceivedAt.Time.UTC().Format(time.RFC3339)
		out.ReceivedAt = &ts
	}
	return out
}

// maxRegradeListLimit caps the per-page size a caller can request on the regrade queue
// (I5): the store defaulted to newest-50 with no way to page past it. Callers page with
// ?limit (≤ this cap) & ?offset.
const maxRegradeListLimit = 200

// defaultRegradeListLimit is the page size when ?limit is absent.
const defaultRegradeListLimit = 50

// handleListRegrades is GET
// /api/regrades?status=&assessment=&kind=&student=&open=&undelivered_result=&limit=&offset=
// (spec §7-§8, I5 pagination). limit defaults to 50, is capped at 200; offset defaults to
// 0. kind scopes to filed|addendum|unparsed|handed_off (the Unparsed queue filter);
// student is a case-insensitive external-student-ID prefix search. The response's
// total counts every row the filters match so the UI can render numbered pages.
//
// Status-group filters (HCI audit, regrades-list correctness) — both accept only
// "1"/"true" (presence-only flags; anything else 400s rather than being silently
// ignored):
//   - open=1: status IN (received, under_review). kind=filed&open=1 is the UI's
//     default "Actionable" queue, now paginated/counted server-side over the same set
//     the tab shows.
//   - undelivered_result=1: resolved filed requests whose result email never got
//     delivered (result_sent_at NULL, migration 0026) — the resend-result recovery
//     set, surfaced at queue level instead of only inside the detail pane.
func (s *Server) handleListRegrades(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ListRegradeRequestsFilters{
		Status:    q.Get("status"),
		Kind:      q.Get("kind"),
		StudentID: strings.TrimSpace(q.Get("student")),
		Limit:     defaultRegradeListLimit,
	}
	for _, flag := range []struct {
		param string
		dst   *bool
	}{
		{"open", &f.OnlyOpen},
		{"undelivered_result", &f.OnlyUndeliveredResult},
	} {
		switch raw := q.Get(flag.param); raw {
		case "":
		case "1", "true":
			*flag.dst = true
		default:
			apiError(w, http.StatusBadRequest, "invalid "+flag.param+" filter (only "+flag.param+"=1 is supported)")
			return
		}
	}
	if raw := q.Get("assessment"); raw != "" {
		aid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || aid <= 0 {
			apiError(w, http.StatusBadRequest, "invalid assessment filter")
			return
		}
		f.AssessmentID = aid
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			apiError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n > maxRegradeListLimit {
			n = maxRegradeListLimit
		}
		f.Limit = n
	}
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			apiError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		f.Offset = n
	}
	rows, err := s.store.ListRegradeRequests(r.Context(), f)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list regrades failed")
		return
	}
	total, err := s.store.CountRegradeRequests(r.Context(), f)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list regrades failed")
		return
	}
	out := make([]regradeListJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, toRegradeListJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"regrades": out,
		"limit":    f.Limit,
		"offset":   f.Offset,
		"total":    total,
		"has_more": int64(f.Offset+len(rows)) < total,
	})
}

// regradeSubItemJSON is one contested problem's adjudication state within a request
// detail (spec §5/§8): the complaint, the verdict (null until adjudicated), the note,
// and the per-sub-item AI record/error.
type regradeSubItemJSON struct {
	ID            int64         `json:"id"`
	ProblemID     int64         `json:"problem_id"`
	ProblemNumber *int32        `json:"problem_number,omitempty"`
	ComplaintText string        `json:"complaint_text"`
	Verdict       *string       `json:"verdict,omitempty"`
	VerdictNote   string        `json:"verdict_note,omitempty"`
	VerdictBy     *int64        `json:"verdict_by,omitempty"`
	VerdictAt     *string       `json:"verdict_at,omitempty"`
	AIError       string        `json:"ai_error,omitempty"`
	AIRecord      *aiRecordJSON `json:"ai_record,omitempty"`
}

// regradeDetailJSON is GET /api/regrades/{id} (spec §7-§8): the email body text, the
// student's published snapshot, kind/turn, and the per-problem sub-items with their
// verdicts and AI records. RegradeMax is the configured turn budget (regrade v2 UI review
// Finding 1) — without it the send-result card can only show "Attempt {turn}" with no
// ceiling, since RegradeMax otherwise never reaches any JSON response.
type regradeDetailJSON struct {
	ID             int64   `json:"id"`
	Status         string  `json:"status"`
	Kind           string  `json:"kind"`
	Turn           *int32  `json:"turn,omitempty"`
	RegradeMax     int     `json:"regrade_max"`
	FromEmail      string  `json:"from_email"`
	SPFVerdict     string  `json:"spf_verdict,omitempty"`
	DKIMVerdict    string  `json:"dkim_verdict,omitempty"`
	Subject        string  `json:"subject"`
	Body           string  `json:"body"`
	ReceivedAt     *string `json:"received_at,omitempty"`
	ResolverID     *int64  `json:"resolver_id,omitempty"`
	ResolutionNote string  `json:"resolution_note,omitempty"`
	ResolvedAt     *string `json:"resolved_at,omitempty"`
	// ResultSentAt is the send-failure recovery marker (whole-branch review F1,
	// migration 0026): non-nil only after result email #N was actually DELIVERED. A
	// resolved (resolved_upheld/resolved_regraded) request with this still nil means
	// the provider send failed after the atomic resolve flip — recoverable via
	// POST /api/regrades/{id}/resend-result. Always omitted (nil) for a request that
	// was never resolved.
	ResultSentAt      *string              `json:"result_sent_at,omitempty"`
	PublishItemID     *int64               `json:"publish_item_id,omitempty"`
	StudentID         *int64               `json:"student_id,omitempty"`
	StudentExternalID string               `json:"student_external_id,omitempty"`
	StudentName       string               `json:"student_name,omitempty"`
	AssessmentID      *int64               `json:"assessment_id,omitempty"`
	AssessmentName    string               `json:"assessment_name,omitempty"`
	Snapshot          []byte               `json:"snapshot,omitempty"`
	Problems          []regradeSubItemJSON `json:"problems"`
}

// aiRecordJSON is the AI re-grade comparison embedded on a sub-item (spec §8): enough for
// an "old vs. new" pane without a second round-trip. Per-criterion fields are resolved
// against the record's own rubric_version_id.
type aiRecordJSON struct {
	AnswerID  int64                   `json:"answer_id"`
	Criteria  []aiRecordCriterionJSON `json:"criteria"`
	Total     *string                 `json:"total"`
	Policy    *string                 `json:"policy,omitempty"`
	CreatedAt *time.Time              `json:"created_at,omitempty"`
}

type aiRecordCriterionJSON struct {
	CriterionID int64  `json:"criterion_id"`
	Name        string `json:"name"`
	Score       string `json:"score"`
	Max         string `json:"max"`
	Comment     string `json:"comment,omitempty"`
}

// toAIRecordJSON builds the embedded ai_record object from a grading_records row.
func (s *Server) toAIRecordJSON(ctx context.Context, rec db.GradingRecord) aiRecordJSON {
	out := aiRecordJSON{AnswerID: rec.AnswerID, CreatedAt: tsPtr(rec.CreatedAt)}
	if rec.Total.Valid {
		t := store.NumStr(rec.Total)
		out.Total = &t
	}
	if rec.Policy.Valid {
		out.Policy = &rec.Policy.String
	}

	var scores []grading.CriterionScore
	if err := json.Unmarshal(rec.CriterionScores, &scores); err != nil {
		return out
	}
	names := map[int64]string{}
	maxes := map[int64]string{}
	if criteria, err := s.store.Q.ListRubricCriteria(ctx, rec.RubricVersionID); err == nil {
		for _, c := range criteria {
			names[c.ID] = c.Description
			maxes[c.ID] = store.NumStr(c.Points)
		}
	}
	out.Criteria = make([]aiRecordCriterionJSON, 0, len(scores))
	for _, sc := range scores {
		out.Criteria = append(out.Criteria, aiRecordCriterionJSON{
			CriterionID: sc.CriterionID,
			Name:        names[sc.CriterionID],
			Score:       sc.Score,
			Max:         maxes[sc.CriterionID],
			Comment:     sc.Rationale,
		})
	}
	return out
}

// handleGetRegrade is GET /api/regrades/{id} (spec §7-§8).
func (s *Server) handleGetRegrade(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid regrade id")
		return
	}
	ctx := r.Context()
	rr, err := s.store.GetRegradeRequest(ctx, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such regrade request")
		return
	}
	out := regradeDetailJSON{
		ID: rr.ID, Status: rr.Status, Kind: rr.Kind, FromEmail: rr.FromEmail,
		Subject: rr.Subject, Body: rr.Body, ResolutionNote: rr.ResolutionNote,
		RegradeMax: s.regradeMaxTurns(),
		Problems:   []regradeSubItemJSON{},
	}
	if rr.Turn.Valid {
		out.Turn = &rr.Turn.Int32
	}
	if rr.SpfVerdict.Valid {
		out.SPFVerdict = rr.SpfVerdict.String
	}
	if rr.DkimVerdict.Valid {
		out.DKIMVerdict = rr.DkimVerdict.String
	}
	if rr.ReceivedAt.Valid {
		ts := rr.ReceivedAt.Time.UTC().Format(time.RFC3339)
		out.ReceivedAt = &ts
	}
	if rr.ResolverID.Valid {
		out.ResolverID = &rr.ResolverID.Int64
	}
	if rr.ResolvedAt.Valid {
		ts := rr.ResolvedAt.Time.UTC().Format(time.RFC3339)
		out.ResolvedAt = &ts
	}
	if rr.ResultSentAt.Valid {
		ts := rr.ResultSentAt.Time.UTC().Format(time.RFC3339)
		out.ResultSentAt = &ts
	}
	if rr.PublishItemID.Valid {
		out.PublishItemID = &rr.PublishItemID.Int64
		if item, err := s.store.Q.GetPublishItem(ctx, rr.PublishItemID.Int64); err == nil {
			out.Snapshot = item.Snapshot
		}
	}
	if rr.StudentID.Valid {
		out.StudentID = &rr.StudentID.Int64
		if student, err := s.store.Q.GetStudent(ctx, rr.StudentID.Int64); err == nil {
			out.StudentExternalID = student.StudentID
			out.StudentName = student.Name
		}
	}
	if rr.AssessmentID.Valid {
		out.AssessmentID = &rr.AssessmentID.Int64
		if assessment, err := s.store.Q.GetAssessment(ctx, rr.AssessmentID.Int64); err == nil {
			out.AssessmentName = assessment.Name
		}
	}

	subs, err := s.store.ListRequestProblems(ctx, id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "load sub-items failed")
		return
	}
	for _, sub := range subs {
		out.Problems = append(out.Problems, s.toSubItemJSON(ctx, sub))
	}
	writeJSON(w, http.StatusOK, out)
}

// toSubItemJSON renders one sub-item, resolving its problem number and any linked AI
// record for the detail view.
func (s *Server) toSubItemJSON(ctx context.Context, sub db.RegradeRequestProblem) regradeSubItemJSON {
	j := regradeSubItemJSON{
		ID: sub.ID, ProblemID: sub.ProblemID, ComplaintText: sub.ComplaintText,
		VerdictNote: sub.VerdictNote,
	}
	if p, err := s.store.Q.GetProblem(ctx, sub.ProblemID); err == nil {
		n := p.Number
		j.ProblemNumber = &n
	}
	if sub.Verdict.Valid {
		j.Verdict = &sub.Verdict.String
	}
	if sub.VerdictBy.Valid {
		j.VerdictBy = &sub.VerdictBy.Int64
	}
	if sub.VerdictAt.Valid {
		ts := sub.VerdictAt.Time.UTC().Format(time.RFC3339)
		j.VerdictAt = &ts
	}
	if sub.AiError.Valid {
		j.AIError = sub.AiError.String
	}
	if sub.AiRecordID.Valid {
		if rec, err := s.store.Q.GetRecord(ctx, sub.AiRecordID.Int64); err == nil {
			ai := s.toAIRecordJSON(ctx, rec)
			j.AIRecord = &ai
		}
	}
	return j
}

// handlePatchRegradeProblem is PATCH /api/regrades/{id}/problems/{pid} {outcome, note}
// (spec §5/§8): a TA's verdict on ONE sub-item. outcome must be "upheld" or "regraded".
// pid is the SUB-ITEM id (regrade_request_problems.id), which must belong to THIS request.
// Audited regrade.verdict.
func (s *Server) handlePatchRegradeProblem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid regrade id")
		return
	}
	subID, ok := pathID(r, "pid")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid sub-item id")
		return
	}
	var body struct {
		Outcome string `json:"outcome"`
		Note    string `json:"note"`
		// AdoptedRecordID (rounds design, 0028): the record a "regraded" verdict
		// adopts as this turn's overlay grade. Defaults to the sub-item's round
		// AI record; supplied explicitly only for the manual-fallback case (the
		// round method failed and the adjudicator graded by hand).
		AdoptedRecordID *int64 `json:"adopted_record_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Outcome != "upheld" && body.Outcome != "regraded" {
		apiError(w, http.StatusBadRequest, "outcome must be 'upheld' or 'regraded'")
		return
	}

	sub, err := s.store.GetRequestProblem(r.Context(), subID)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusNotFound, "no such sub-item")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if sub.RequestID != id {
		apiError(w, http.StatusBadRequest, "sub-item does not belong to this request")
		return
	}

	// Adjudicable-state gate (regrade-round correctness fix): a verdict may only be set
	// or changed while the request is still OPEN — a filed request in received/
	// under_review. Once the result email is sent the request resolves; flipping a
	// verdict or swapping the adopted record afterwards would silently change the
	// student's EXPORTED grade to a value they were never told, with no way to send a
	// correction (send-result and resend-result both 409 on a resolved row). Mirror
	// handleSendResult's allowed states so the two agree on "open".
	rr, err := s.store.GetRegradeRequest(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusNotFound, "no such regrade request")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "request lookup failed")
		return
	}
	if rr.Kind != regradeKindFiled {
		apiError(w, http.StatusConflict, "only a filed request's problems can be adjudicated")
		return
	}
	if rr.Status != regradeStatusReceived && rr.Status != regradeStatusUnderReview {
		apiError(w, http.StatusConflict, "regrade request is not open (a result was already sent) — its verdicts can no longer be changed")
		return
	}

	// Adoption (rounds design): "regraded" must name the record that becomes
	// this turn's grade — the round AI record by default, or an explicitly
	// attached one (manual fallback). "Upheld" adopts nothing (the layer below
	// carries through). adoptedRecordID stays 0 for upheld ⇒ SQL NULL.
	var adoptedRecordID int64
	if body.Outcome == "regraded" {
		recordID := sub.AiRecordID.Int64
		if body.AdoptedRecordID != nil {
			recordID = *body.AdoptedRecordID
		} else if !sub.AiRecordID.Valid {
			apiError(w, http.StatusConflict, "no AI re-grade record to adopt — run the round's AI re-grade first, or grade manually and pass adopted_record_id")
			return
		}
		rec, err := s.store.Q.GetRecord(r.Context(), recordID)
		if err != nil {
			apiError(w, http.StatusBadRequest, "adopted record not found")
			return
		}
		if !rr.StudentID.Valid {
			apiError(w, http.StatusInternalServerError, "request lookup failed")
			return
		}
		ans, err := s.store.Q.GetAnswer(r.Context(), rec.AnswerID)
		if err != nil || ans.StudentID != rr.StudentID.Int64 || ans.ProblemID != sub.ProblemID {
			apiError(w, http.StatusBadRequest, "adopted record does not belong to this student's contested answer")
			return
		}
		if !rec.Total.Valid {
			apiError(w, http.StatusBadRequest, "adopted record has no total (illegible refusal) — grade manually and adopt that record")
			return
		}
		adoptedRecordID = recordID
	}

	// One atomic write for verdict + adoption (regrade-round correctness fix): the old
	// SetProblemVerdict-then-SetSubItemAdoptedRecord pair could fail between the two and
	// strand a verdict without (or with a stale) adoption. SetProblemVerdictAndAdoption
	// sets both in a single UPDATE.
	me, _ := currentUser(r.Context())
	updated, err := s.store.SetProblemVerdictAndAdoption(r.Context(), store.SetProblemVerdictAndAdoptionParams{
		SubItemID: subID, Verdict: body.Outcome, Note: body.Note, VerdictBy: me.ID,
		AdoptedRecordID: adoptedRecordID,
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "verdict update failed")
		return
	}

	s.audit(r, "regrade.verdict", "regrade_request", strconv.FormatInt(id, 10), map[string]any{
		"sub_item_id": subID, "problem_id": sub.ProblemID, "outcome": body.Outcome,
		"adopted_record_id": body.AdoptedRecordID,
	})
	writeJSON(w, http.StatusOK, s.toSubItemJSON(r.Context(), updated))
}

// handleAddRegradeProblem is POST /api/regrades/{id}/problems {problem_id, complaint}
// (spec §5 escape hatch, TA+): manually add (or correct) a sub-item on a FILED request —
// never on unparsed/addendum/handed_off rows. UNIQUE(request_id, problem_id) rejects a
// duplicate problem with a 409. problem_id must belong to this request's assessment.
// Audited regrade.add_problem.
func (s *Server) handleAddRegradeProblem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid regrade id")
		return
	}
	var body struct {
		ProblemID int64  `json:"problem_id"`
		Complaint string `json:"complaint"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ProblemID <= 0 {
		apiError(w, http.StatusBadRequest, "problem_id is required")
		return
	}

	rr, err := s.store.GetRegradeRequest(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusNotFound, "no such regrade request")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if rr.Kind != regradeKindFiled {
		apiError(w, http.StatusConflict, "sub-items can only be added to a filed request")
		return
	}
	if !rr.AssessmentID.Valid {
		apiError(w, http.StatusConflict, "regrade request has no associated assessment")
		return
	}
	problem, err := s.store.Q.GetProblem(r.Context(), body.ProblemID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && problem.AssessmentID != rr.AssessmentID.Int64) {
		apiError(w, http.StatusBadRequest, "problem does not belong to this request's assessment")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "problem lookup failed")
		return
	}

	created, err := s.store.InsertRequestProblems(r.Context(), id, []store.RequestProblemInput{
		{ProblemID: body.ProblemID, ComplaintText: body.Complaint},
	})
	if err != nil {
		if store.IsUniqueViolation(err) {
			apiError(w, http.StatusConflict, "this problem is already on the request")
			return
		}
		apiError(w, http.StatusInternalServerError, "add sub-item failed")
		return
	}

	s.audit(r, "regrade.add_problem", "regrade_request", strconv.FormatInt(id, 10), map[string]any{
		"problem_id": body.ProblemID,
	})
	writeJSON(w, http.StatusCreated, s.toSubItemJSON(r.Context(), created[0]))
}

// --- Result send gate (spec §5) ------------------------------------------------------

// handleSendResult is POST /api/regrades/{id}/send-result (spec §5, TA+): renders and
// sends result email #N, hard-gated server-side — 409 until EVERY sub-item has a
// verdict (AllProblemsVerdicted, which treats zero sub-items as NOT passable). Result #N
// carries the NEXT-turn token (turn N+1); the final turn (N == MAX) instead gets the
// handoff token + final-attempt copy. Sending resolves the request (once — a second send
// 409s because the request is no longer open). Audited regrade.send_result.
func (s *Server) handleSendResult(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid regrade id")
		return
	}
	ctx := r.Context()
	rr, err := s.store.GetRegradeRequest(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusNotFound, "no such regrade request")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if rr.Kind != regradeKindFiled {
		apiError(w, http.StatusConflict, "only a filed request can send a result email")
		return
	}
	if rr.Status != regradeStatusReceived && rr.Status != regradeStatusUnderReview {
		// Already resolved (a prior send) or otherwise not open — send is once-only.
		apiError(w, http.StatusConflict, "regrade request is not open (a result was already sent)")
		return
	}

	// The gate: 409 until all sub-items are verdicted (zero sub-items does NOT pass —
	// the store-level check handles that vacuous-truth trap). The 409 body is
	// structured ({error, unverdicted:[{problem_id, problem_number}]}) so the UI can
	// render the per-problem checklist authoritatively instead of only deriving it
	// client-side (regrade v2 gap 3). Computed straight from the sub-items lacking a
	// verdict — no separate round-trip.
	allVerdicted, err := s.store.AllProblemsVerdicted(ctx, id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "gate check failed")
		return
	}
	if !allVerdicted {
		unverdicted, err := s.unverdictedProblems(ctx, id)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "gate check failed")
			return
		}
		apiError409Unverdicted(w, "every contested problem must have a verdict before a result can be sent", unverdicted)
		return
	}

	if !rr.PublishItemID.Valid || !rr.StudentID.Valid || !rr.AssessmentID.Valid || !rr.Turn.Valid {
		apiError(w, http.StatusConflict, "filed request is missing the item/turn context needed to send a result")
		return
	}

	turn := int(rr.Turn.Int32)
	maxTurns := s.regradeMaxTurns()
	isFinal := turn >= maxTurns

	msg, subs, student, err := s.buildRegradeResultEmail(ctx, rr)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The atomic status flip is the race arbiter (Finding 1, CRITICAL fix): a plain
	// pre-check read (rr.Status above) cannot stop two concurrent send-result calls from
	// both passing it and both emailing the student — only a guarded WRITE can decide the
	// race. This must happen BEFORE the send, not after: ResolveRegradeRequest's atomic
	// UPDATE ... WHERE status IN ('received','under_review') ... RETURNING (the same guard
	// the resolve/remind paths already rely on) is what actually decides who gets to send.
	// Whichever caller's UPDATE matches a row wins the right to send; a concurrent loser
	// gets zero rows back (pgx.ErrNoRows) and 409s here WITHOUT ever calling the email
	// provider. Both resolved_upheld and resolved_regraded are valid outcomes of this
	// guard (the WHERE clause doesn't care which).
	resolveStatus := regradeStatusResolvedRegraded
	if allUpheld(subs) {
		resolveStatus = regradeStatusResolvedUpheld
	}
	me, _ := currentUser(ctx)
	if _, err := s.store.ResolveRegradeRequest(ctx, id, resolveStatus, me.ID, ""); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Lost the race (or the pre-check read above was stale for some other
			// reason) — someone else already resolved this request. No email sent.
			apiError(w, http.StatusConflict, "regrade request is not open (a result was already sent)")
			return
		}
		apiError(w, http.StatusInternalServerError, "resolve failed")
		return
	}

	// We won the flip: this request is now ours alone to send for. If the provider send
	// fails here, we deliberately do NOT roll the status back to under_review to "allow a
	// retry" — reopening the row would reopen the exact double-send window the
	// flip-before-send ordering above closes (a retry could then race a second concurrent
	// send-result). The request stays resolved with no email delivered; the failure is
	// recorded the same way every other send failure in this file is recorded (filing
	// confirmation, TA-notify, reminder): logged, surfaced as a 500, never rolled back.
	// result_sent_at (migration 0026, whole-branch review F1) stays NULL in this branch —
	// that is the recoverable-state marker: POST /api/regrades/{id}/resend-result re-sends
	// for exactly this "resolved but result_sent_at IS NULL" case, so a send failure here
	// is no longer a dead end.
	if s.email != nil {
		msg.To = student.Email
		if _, err := s.email.Send(ctx, msg); err != nil {
			s.log.Error("regrade: send result failed after winning the resolve flip — request stays resolved, no email delivered, recoverable via resend-result", "request_id", id, "err", err)
			apiError(w, http.StatusInternalServerError, "send result failed")
			return
		}
		if _, err := s.store.SetRegradeResultSentAt(ctx, id); err != nil {
			// The email is already out the door; failing to record the marker only
			// affects the (rare) UI hint / resend-result eligibility, not delivery — log
			// and continue rather than surfacing a 500 for an email that DID send.
			s.log.Error("regrade: set result_sent_at failed after a successful send", "request_id", id, "err", err)
		}
	}

	s.audit(r, "regrade.send_result", "regrade_request", strconv.FormatInt(id, 10), map[string]any{
		"turn": turn, "final": isFinal,
	})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "turn": turn, "final": isFinal, "status": resolveStatus})
}

// buildRegradeResultEmail renders result email #N for a FILED, fully-verdicted request
// (shared by handleSendResult and handleResendResult so the two paths can never drift):
// student/assessment lookups, the per-problem result sections (a regraded problem's
// NewScore is the ADOPTED overlay record's total — rounds design, 0028; round 0 is
// never rewritten), and the next-turn Reply-To token.
// Requires rr.PublishItemID/StudentID/AssessmentID/Turn all valid — callers check that
// before calling (handleSendResult already does; handleResendResult only reaches a
// resolved row, which always carries that context).
func (s *Server) buildRegradeResultEmail(ctx context.Context, rr db.RegradeRequest) (domain.OutboundEmail, []db.RegradeRequestProblem, db.Student, error) {
	student, err := s.store.Q.GetStudent(ctx, rr.StudentID.Int64)
	if err != nil {
		return domain.OutboundEmail{}, nil, db.Student{}, fmt.Errorf("student lookup failed")
	}
	assessment, err := s.store.Q.GetAssessment(ctx, rr.AssessmentID.Int64)
	if err != nil {
		return domain.OutboundEmail{}, nil, db.Student{}, fmt.Errorf("assessment lookup failed")
	}
	subs, err := s.store.ListRequestProblems(ctx, rr.ID)
	if err != nil {
		return domain.OutboundEmail{}, nil, db.Student{}, fmt.Errorf("load sub-items failed")
	}

	turn := int(rr.Turn.Int32)
	maxTurns := s.regradeMaxTurns()

	resultProblems := make([]email.ResultProblem, 0, len(subs))
	for _, sub := range subs {
		p, err := s.store.Q.GetProblem(ctx, sub.ProblemID)
		if err != nil {
			return domain.OutboundEmail{}, nil, db.Student{}, fmt.Errorf("problem lookup failed")
		}
		rp := email.ResultProblem{
			Number:    int(p.Number),
			Complaint: sub.ComplaintText,
			Outcome:   sub.Verdict.String, // guaranteed valid by the gate
			Note:      sub.VerdictNote,
		}
		if sub.Verdict.String == "regraded" && sub.AdoptedRecordID.Valid {
			if rec, err := s.store.Q.GetRecord(ctx, sub.AdoptedRecordID.Int64); err == nil && rec.Total.Valid {
				rp.NewScore, rp.Max = store.NumStr(rec.Total), store.NumStr(p.MaxPoints)
			}
		}
		resultProblems = append(resultProblems, rp)
	}

	// The NEXT-turn token (turn+1). Result #N carries it; on the final turn its
	// consumption fires the handoff rather than another adjudication round.
	replyTo := ""
	if s.cfg.Email.ReplyDomain != "" {
		expiry := time.Now().Add(s.cfg.RegradeWindow)
		nextToken := email.MintTokenV2(s.tokenKey, rr.PublishItemID.Int64, turn+1, expiry)
		replyTo = fmt.Sprintf("regrade+%s@%s", nextToken, s.cfg.Email.ReplyDomain)
	}

	msg, err := email.RenderRegradeResult(email.ResultData{
		AssessmentName: assessment.Name,
		StudentName:    student.Name,
		Problems:       resultProblems,
		Turn:           turn,
		MaxTurns:       maxTurns,
		ReplyTo:        replyTo,
		FormatTemplate: email.RegradeReplyFormatTemplate,
	})
	if err != nil {
		return domain.OutboundEmail{}, nil, db.Student{}, fmt.Errorf("render result failed")
	}
	return msg, subs, student, nil
}

// handleResendResult is POST /api/regrades/{id}/resend-result (spec F1 recovery path,
// TA+): re-sends result email #N for a request that was already resolved but never
// actually delivered — the provider send failed AFTER handleSendResult's atomic
// resolve-flip, leaving result_sent_at NULL. Guard: kind='filed' AND status IN
// (resolved_upheld, resolved_regraded) AND result_sent_at IS NULL; anything else 409s
// (not yet sent at all — still open; or already delivered — nothing to recover).
//
// Unlike handleSendResult this is deliberately SEND-THEN-MARK, not flip-then-send:
// there is no open/resolved race to arbitrate here (the request is already resolved,
// permanently), so the only failure mode is a duplicate email if this is called twice
// concurrently on the same never-delivered row. That duplicate is structurally
// harmless in v2 — token consumption is slot-based (the (publish_item_id, turn)
// partial unique index caps filings per turn regardless of how many copies of a result
// email exist), so a double-send here is a cosmetic duplicate, not a chain-breaking
// bug. Recoverability (never stranding the student with a dead chain) is worth that
// trade. Audited regrade.resend_result.
func (s *Server) handleResendResult(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid regrade id")
		return
	}
	ctx := r.Context()
	rr, err := s.store.GetRegradeRequest(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusNotFound, "no such regrade request")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if rr.Kind != regradeKindFiled {
		apiError(w, http.StatusConflict, "only a filed request can resend a result email")
		return
	}
	if rr.Status != regradeStatusResolvedUpheld && rr.Status != regradeStatusResolvedRegraded {
		apiError(w, http.StatusConflict, "resend-result is only for a resolved request whose result was never delivered")
		return
	}
	if rr.ResultSentAt.Valid {
		apiError(w, http.StatusConflict, "the result for this request was already delivered")
		return
	}
	if !rr.PublishItemID.Valid || !rr.StudentID.Valid || !rr.AssessmentID.Valid || !rr.Turn.Valid {
		apiError(w, http.StatusConflict, "resolved request is missing the item/turn context needed to resend a result")
		return
	}

	msg, _, student, err := s.buildRegradeResultEmail(ctx, rr)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.email != nil {
		msg.To = student.Email
		if _, err := s.email.Send(ctx, msg); err != nil {
			s.log.Error("regrade: resend result failed", "request_id", id, "err", err)
			apiError(w, http.StatusInternalServerError, "resend result failed")
			return
		}
	}
	if _, err := s.store.SetRegradeResultSentAt(ctx, id); err != nil {
		apiError(w, http.StatusInternalServerError, "failed to record delivery marker")
		return
	}

	s.audit(r, "regrade.resend_result", "regrade_request", strconv.FormatInt(id, 10), map[string]any{
		"turn": int(rr.Turn.Int32),
	})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "resent": true})
}

// unverdictedProblemJSON is one entry of the send-result 409 payload's unverdicted list
// (spec §5 gap 3): just enough for the UI's per-problem checklist to key off.
type unverdictedProblemJSON struct {
	ProblemID     int64 `json:"problem_id"`
	ProblemNumber int32 `json:"problem_number"`
}

// unverdictedProblems lists the request's sub-items that still lack a verdict, resolving
// each to its problem number for the send-result 409's structured payload. Always
// returns a non-nil (possibly empty) slice so the JSON encodes as [] rather than null —
// the zero-sub-items vacuous-truth case still needs an unambiguous empty list.
func (s *Server) unverdictedProblems(ctx context.Context, requestID int64) ([]unverdictedProblemJSON, error) {
	subs, err := s.store.ListRequestProblems(ctx, requestID)
	if err != nil {
		return nil, err
	}
	out := make([]unverdictedProblemJSON, 0, len(subs))
	for _, sub := range subs {
		if sub.Verdict.Valid {
			continue
		}
		p, err := s.store.Q.GetProblem(ctx, sub.ProblemID)
		if err != nil {
			return nil, err
		}
		out = append(out, unverdictedProblemJSON{ProblemID: sub.ProblemID, ProblemNumber: p.Number})
	}
	return out, nil
}

// apiError409Unverdicted writes the send-result gate's structured 409 body (spec §5 gap
// 3): {error, unverdicted:[{problem_id, problem_number}]}, mirroring the
// apiError409Budget pattern elsewhere in this package.
func apiError409Unverdicted(w http.ResponseWriter, msg string, unverdicted []unverdictedProblemJSON) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":       msg,
		"unverdicted": unverdicted,
	})
}

// allUpheld reports whether every sub-item's verdict is "upheld" (so the request
// resolves as resolved_upheld rather than resolved_regraded). All sub-items are
// guaranteed verdicted here by the send gate.
func allUpheld(subs []db.RegradeRequestProblem) bool {
	for _, sub := range subs {
		if sub.Verdict.String != "upheld" {
			return false
		}
	}
	return true
}

// problemOfficialTotal returns the current official (score, max) for one (student,
// problem) — the fresh grade to announce on a regraded result (C2). ok=false ⇒ nothing
// official yet, so the caller omits the New-score line rather than announcing a stale
// figure.
func (s *Server) problemOfficialTotal(ctx context.Context, studentID, problemID int64) (score, max string, ok bool) {
	row, err := s.store.Q.StudentOfficialTotalForProblem(ctx, db.StudentOfficialTotalForProblemParams{
		StudentID: studentID, ID: problemID,
	})
	if err != nil || row.Graded == 0 {
		return "", "", false
	}
	return store.NumStr(row.Total), store.NumStr(row.Max), true
}

// --- Reminder (spec §7) --------------------------------------------------------------

// handleRemindRegrade is POST /api/regrades/{id}/remind (spec §7, TA+): sends the
// anchored reminder for an UNPARSED row (0 valid blocks, live token) — once per row
// (disabled after send), never on filed/addendum/handed_off. The reminder names the
// assessment, the exact subject + sent date of the email whose token is still live, the
// attempt counter, and the literal format template; it carries NO token / NO Reply-To
// (structurally reply-proof). Audited regrade.remind. A second call 409s.
func (s *Server) handleRemindRegrade(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid regrade id")
		return
	}
	ctx := r.Context()
	rr, err := s.store.GetRegradeRequest(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusNotFound, "no such regrade request")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if rr.Kind != regradeKindUnparsed {
		apiError(w, http.StatusConflict, "reminders can only be sent for unparsed replies")
		return
	}
	// Once-only: the resolve guard doubles as the "already reminded" guard — a reminded
	// row is moved out of the open state so a second remind 409s. (An unparsed row is
	// recorded 'received'; after a reminder it is resolved_upheld with a marker note.)
	if rr.Status != regradeStatusReceived && rr.Status != regradeStatusUnderReview {
		apiError(w, http.StatusConflict, "a reminder was already sent for this row")
		return
	}
	if !rr.PublishItemID.Valid || !rr.StudentID.Valid || !rr.AssessmentID.Valid || !rr.Turn.Valid {
		apiError(w, http.StatusConflict, "unparsed row is missing the item/turn context needed to remind")
		return
	}

	student, err := s.store.Q.GetStudent(ctx, rr.StudentID.Int64)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "student lookup failed")
		return
	}
	assessment, err := s.store.Q.GetAssessment(ctx, rr.AssessmentID.Int64)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "assessment lookup failed")
		return
	}

	// Anchor to the email whose token is still live: the message at this turn. The grade
	// email (turn 1) is «assessment» — results; a result email (turn N>1) is
	// «assessment» — regrade result #(N-1). Its sent date is the publish item's send
	// time (the row's own received_at is the STUDENT's reply time, not ours) — best
	// available anchor without storing per-email send timestamps. APPROXIMATION: for a
	// turn>1 reminder, publish_items.sent_at is still the ORIGINAL grade email's
	// timestamp (there is no per-result-email send_at column), so the displayed date can
	// be earlier than when result email #(N-1) actually went out — the subject line is
	// exact, only the date is an approximation for turn>1.
	turn := int(rr.Turn.Int32)
	anchorSubject := fmt.Sprintf("%s — results", assessment.Name)
	if turn > 1 {
		anchorSubject = fmt.Sprintf("%s — regrade result #%d", assessment.Name, turn-1)
	}
	anchorSentAt := rr.ReceivedAt.Time
	if item, err := s.store.Q.GetPublishItem(ctx, rr.PublishItemID.Int64); err == nil && item.SentAt.Valid {
		anchorSentAt = item.SentAt.Time
	}

	msg, err := email.RenderRegradeReminder(email.ReminderData{
		AssessmentName: assessment.Name,
		StudentName:    student.Name,
		AnchorSubject:  anchorSubject,
		AnchorSentAt:   anchorSentAt,
		Turn:           turn,
		MaxTurns:       s.regradeMaxTurns(),
		FormatTemplate: email.RegradeReplyFormatTemplate,
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "render reminder failed")
		return
	}
	me, _ := currentUser(ctx)
	errReminderAlreadySent := errors.New("reminder already sent")
	err = s.store.WithTx(ctx, func(q *db.Queries) error {
		// Hold the row lock across the provider call. A concurrent reminder waits,
		// then observes the resolved status and exits without sending. If the
		// provider fails, the transaction rolls back so a deliberate retry remains
		// possible; the Postmark client bounds the lock to a 15-second timeout.
		locked, err := q.LockRegradeRequest(ctx, id)
		if err != nil {
			return err
		}
		if locked.Kind != regradeKindUnparsed ||
			(locked.Status != regradeStatusReceived && locked.Status != regradeStatusUnderReview) {
			return errReminderAlreadySent
		}
		if s.email != nil {
			msg.To = student.Email
			if _, err := s.email.Send(ctx, msg); err != nil {
				return fmt.Errorf("send reminder: %w", err)
			}
		}
		_, err = q.ResolveRegradeRequest(ctx, db.ResolveRegradeRequestParams{
			ID:             id,
			Status:         regradeStatusResolvedUpheld,
			ResolverID:     pgtype.Int8{Int64: me.ID, Valid: me.ID != 0},
			ResolutionNote: reminderResolutionNote,
		})
		return err
	})
	if errors.Is(err, errReminderAlreadySent) {
		apiError(w, http.StatusConflict, "a reminder was already sent for this row")
		return
	}
	if err != nil {
		s.log.Error("regrade: send reminder failed", "request_id", id, "err", err)
		apiError(w, http.StatusInternalServerError, "send reminder failed")
		return
	}

	s.audit(r, "regrade.remind", "regrade_request", strconv.FormatInt(id, 10), map[string]any{"turn": turn})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "reminded": true})
}

// reminderResolutionNote marks an unparsed row that has been reminded — it moves the row
// out of the open state (so a second remind 409s) without inventing a new status value.
const reminderResolutionNote = "reminder sent"

// --- TA assignment (spec §6, §8) -----------------------------------------------------

// taAssignmentJSON is one problem's TA-assignment state (regrade v2 gap 1): the regrade
// queue's "handed to <TA>" badge and the problems-editor picker's current-assignment
// display both key off this. An unassigned problem is still present in the list (so the
// UI can render "no TA assigned" without a second lookup) but with user_id/user_name
// null — a clearer shape than omitting the problem entirely.
type taAssignmentJSON struct {
	ProblemID     int64   `json:"problem_id"`
	ProblemNumber int32   `json:"problem_number"`
	UserID        *int64  `json:"user_id"`
	UserName      *string `json:"user_name"`
}

// handleListTAAssignments is GET /api/assessments/{id}/ta-assignments (regrade v2 spec
// §6/§8 gap 1): per-problem TA assignment for every problem in the assessment, reusing
// ListTAAssignments. Gated the SAME as the regrade queue's other read routes (GET
// /api/regrades, GET /api/regrades/{id}) — no requireRole wrapper, i.e. any signed-in
// role (TA+) can view it, since a TA viewing the queue needs to see assignment badges
// too (picked to match the queue's existing read-route gating, not lecturer+).
func (s *Server) handleListTAAssignments(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid assessment id")
		return
	}
	ctx := r.Context()
	if _, err := s.store.Q.GetAssessment(ctx, id); err != nil {
		apiError(w, http.StatusNotFound, "no such assessment")
		return
	}
	rows, err := s.store.ListTAAssignments(ctx, id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list TA assignments failed")
		return
	}
	out := make([]taAssignmentJSON, 0, len(rows))
	for _, row := range rows {
		j := taAssignmentJSON{ProblemID: row.ProblemID, ProblemNumber: row.ProblemNumber}
		if row.UserID.Valid {
			uid := row.UserID.Int64
			j.UserID = &uid
			if u, err := s.store.Q.GetUserByID(ctx, uid); err == nil {
				name := u.DisplayName
				j.UserName = &name
			}
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"assignments": out})
}

// graderJSON is one assignable grader (regrade v2 spec §6/§8 gap 2): the TA-assignment
// picker's data source. Deliberately MINIMAL — id/name/role only, no email or other PII
// beyond name — distinct from the full admin user payload GET /api/users returns.
type graderJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// handleListGraders is GET /api/graders (regrade v2 spec §6/§8 gap 2, lecturer+):
// assignable graders (active users holding TA-or-higher role) for the TA-assignment
// picker. GET /api/users stays admin-only and unrelaxed — this is a narrower, separate
// endpoint at the lecturer+ floor the picker (and PUT /api/problems/{id}/ta) already
// sits at, returning only the minimal fields the picker needs.
func (s *Server) handleListGraders(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Q.ListAssignableGraders(r.Context())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list graders failed")
		return
	}
	out := make([]graderJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, graderJSON{ID: row.ID, Name: row.DisplayName, Role: row.Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{"graders": out})
}

// handleAssignProblemTA is PUT /api/problems/{id}/ta {user_id} (spec §8, lecturer+):
// assigns (or, with user_id null/0, unassigns) the TA responsible for a problem's
// handoff. The assignee must hold TA-or-higher role — the store's AssignProblemTA does
// not check roles, so this handler is the enforcement point (400 otherwise). Audited
// problem.assign_ta.
func (s *Server) handleAssignProblemTA(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid problem id")
		return
	}
	var body struct {
		UserID *int64 `json:"user_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}

	if _, err := s.store.Q.GetProblem(r.Context(), pid); errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusNotFound, "no such problem")
		return
	} else if err != nil {
		apiError(w, http.StatusInternalServerError, "problem lookup failed")
		return
	}

	// Unassign.
	if body.UserID == nil || *body.UserID == 0 {
		if err := s.store.RemoveProblemTA(r.Context(), pid); err != nil {
			apiError(w, http.StatusInternalServerError, "unassign failed")
			return
		}
		s.audit(r, "problem.assign_ta", "problem", strconv.FormatInt(pid, 10), map[string]any{"user_id": nil})
		writeJSON(w, http.StatusOK, map[string]any{"problem_id": pid, "user_id": nil})
		return
	}

	// Assign — the assignee must be TA or higher (handler-enforced, per store contract).
	assignee, err := s.store.Q.GetUserByID(r.Context(), *body.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusBadRequest, "no such user")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "user lookup failed")
		return
	}
	if !auth.RoleAtLeast(auth.Role(assignee.Role), auth.RoleTA) {
		apiError(w, http.StatusBadRequest, "assignee must hold the TA role or higher")
		return
	}

	me, _ := currentUser(r.Context())
	if _, err := s.store.AssignProblemTA(r.Context(), pid, *body.UserID, me.ID); err != nil {
		apiError(w, http.StatusInternalServerError, "assign failed")
		return
	}
	s.audit(r, "problem.assign_ta", "problem", strconv.FormatInt(pid, 10), map[string]any{"user_id": *body.UserID})
	writeJSON(w, http.StatusOK, map[string]any{"problem_id": pid, "user_id": *body.UserID})
}

// --- AI re-grade assist (spec §8, per-sub-item re-scope) ------------------------------

// aiSubItemEligible reports whether a sub-item is eligible for an AI re-grade (spec §8):
// its request is a FILED request still open (received/under_review), and the sub-item has
// no AI record yet — UNLESS rerun bypasses the "no record yet" clause. It never bypasses
// the request-open gate: a resolved request's sub-items are not AI-regradable.
func aiSubItemEligible(rr db.RegradeRequest, sub db.RegradeRequestProblem, rerun bool) (ok bool, reason string) {
	if rr.Kind != regradeKindFiled {
		return false, "only a filed request's problems can be AI re-graded"
	}
	if rr.Status != regradeStatusReceived && rr.Status != regradeStatusUnderReview {
		return false, "regrade request is not open (a result was already sent) — its problems cannot be AI re-graded"
	}
	if sub.AiRecordID.Valid && !rerun {
		return false, "this problem already has an AI re-grade record (pass ?rerun=1 to run again)"
	}
	return true, ""
}

// handleAIRegrade is POST /api/regrades/{id}/ai-regrade?rerun=0|1 {problem_id} (spec §8,
// TA+): enqueues ONE stricter AI re-grade job for ONE sub-item — the (request, problem)
// pair. Eligibility per aiSubItemEligible (409 otherwise). Budget gate prices the single
// contested answer via the shared helper. IDs only reach the queue; the job re-resolves +
// re-redacts at execution time. Audited regrade.ai_regrade.
func (s *Server) handleAIRegrade(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid regrade id")
		return
	}
	if s.queue == nil {
		apiError(w, http.StatusServiceUnavailable, "queue unavailable")
		return
	}
	var body struct {
		ProblemID int64 `json:"problem_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ProblemID <= 0 {
		apiError(w, http.StatusBadRequest, "problem_id is required")
		return
	}
	rerun := r.URL.Query().Get("rerun") == "1"
	ctx := r.Context()

	rr, err := s.store.GetRegradeRequest(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusNotFound, "no such regrade request")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}

	sub, err := s.subItemForProblem(ctx, id, body.ProblemID)
	if errors.Is(err, pgx.ErrNoRows) {
		apiError(w, http.StatusNotFound, "no such contested problem on this request")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "sub-item lookup failed")
		return
	}
	if ok, reason := aiSubItemEligible(rr, sub, rerun); !ok {
		apiError(w, http.StatusConflict, reason)
		return
	}

	// Budget gate (D36, I2): this sub-item re-grades one contested answer — price it with
	// the SAME estimate helper + budget gate the batch path uses, so a TA can't re-spend
	// past the cap (including with ?rerun=1). "unknown" pricing fails open per D35.
	pricing, err := s.subItemPricing(ctx, rr, sub)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "estimate lookup failed")
		return
	}
	estimate, estKnown := s.estimateAIRegradeCost(ctx, pricing)
	if s.enforceAIRegradeBudget(w, ctx, estimate, estKnown) {
		return
	}

	enqueued, err := s.queue.EnqueueRegradeAI(ctx, []int64{sub.ID})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "enqueue failed")
		return
	}
	s.audit(r, "regrade.ai_regrade", "regrade_request", strconv.FormatInt(id, 10), map[string]any{
		"sub_item_id": sub.ID, "problem_id": body.ProblemID, "rerun": rerun, "enqueued": enqueued,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "sub_item_id": sub.ID, "enqueued": enqueued})
}

// handleAIRegradeAll is POST /api/regrades/ai-regrade-all {assessment_id, dry_run}
// (spec §8, TA+): enqueues an AI re-grade for every ELIGIBLE SUB-ITEM in the assessment —
// filed requests still open, sub-items with no AI record yet. The response reports
// {enqueued, skipped, estimated_cost}; the monthly budget gate applies (409). dry_run
// computes the same numbers, runs the same gate, but enqueues nothing and writes no audit.
// Audited regrade.ai_regrade_all.
func (s *Server) handleAIRegradeAll(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		apiError(w, http.StatusServiceUnavailable, "queue unavailable")
		return
	}
	var body struct {
		AssessmentID int64 `json:"assessment_id"`
		DryRun       bool  `json:"dry_run"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.AssessmentID <= 0 {
		apiError(w, http.StatusBadRequest, "assessment_id is required")
		return
	}
	dryRun := body.DryRun || r.URL.Query().Get("dry_run") == "1"
	ctx := r.Context()

	// Enumerate eligible sub-items (spec §8): every sub-item of an open filed request in
	// the assessment that has no AI record yet.
	eligible, err := s.store.ListEligibleAIRegradeSubItems(ctx, body.AssessmentID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "eligibility lookup failed")
		return
	}
	skipped, err := s.store.CountSkippedAIRegradeSubItems(ctx, body.AssessmentID)
	if err != nil {
		s.log.Error("regrade: count skipped sub-items failed", "assessment_id", body.AssessmentID, "err", err)
	}

	pricing := make([]contestedAnswerPricing, 0, len(eligible))
	ids := make([]int64, 0, len(eligible))
	for _, e := range eligible {
		ids = append(ids, e.SubItemID)
		pricing = append(pricing, contestedAnswerPricing{Provider: e.Provider, ModelID: e.ModelID})
	}
	estimate, estKnown := s.estimateAIRegradeCost(ctx, pricing)
	if s.enforceAIRegradeBudget(w, ctx, estimate, estKnown) {
		return
	}

	estStr := store.NumStr(estimate)
	if !estKnown {
		estStr = "" // unknown — the UI shows "unknown", never $0 (D35)
	}

	if dryRun {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"enqueued": len(ids), "skipped": skipped, "estimated_cost": estStr, "dry_run": true,
		})
		return
	}

	enqueued, err := s.queue.EnqueueRegradeAI(ctx, ids)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "enqueue failed")
		return
	}
	s.audit(r, "regrade.ai_regrade_all", "assessment", strconv.FormatInt(body.AssessmentID, 10), map[string]any{
		"enqueued": enqueued,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"enqueued": enqueued, "skipped": skipped, "estimated_cost": estStr,
	})
}

// subItemForProblem finds the sub-item on a request for a given problem_id (the
// (request, problem) pair the per-sub-item AI endpoint keys off). ErrNoRows ⇒ this
// problem is not contested on the request.
func (s *Server) subItemForProblem(ctx context.Context, requestID, problemID int64) (db.RegradeRequestProblem, error) {
	subs, err := s.store.ListRequestProblems(ctx, requestID)
	if err != nil {
		return db.RegradeRequestProblem{}, err
	}
	for _, sub := range subs {
		if sub.ProblemID == problemID {
			return sub, nil
		}
	}
	return db.RegradeRequestProblem{}, pgx.ErrNoRows
}

// subItemPricing resolves the provider/model of the single contested official answer a
// sub-item re-grades, for the budget estimate. An empty slice (no official answer to
// re-examine) prices as "unknown" (fails open) — the enqueue still proceeds and the job
// records a terminal ai_error at execution time.
func (s *Server) subItemPricing(ctx context.Context, rr db.RegradeRequest, sub db.RegradeRequestProblem) ([]contestedAnswerPricing, error) {
	if !rr.StudentID.Valid || !rr.AssessmentID.Valid {
		return nil, nil
	}
	answers, err := s.store.ContestedAnswerForSubItem(ctx, rr.AssessmentID.Int64, rr.StudentID.Int64, sub.ProblemID, sub.ID)
	if err != nil {
		return nil, err
	}
	pricing := make([]contestedAnswerPricing, 0, len(answers))
	for _, a := range answers {
		pricing = append(pricing, contestedAnswerPricing{Provider: a.Provider, ModelID: a.ModelID})
	}
	return pricing, nil
}

// contestedAnswerPricing is the provider/model pair the AI re-grade cost estimate needs
// from one contested official answer.
type contestedAnswerPricing struct {
	Provider pgtype.Text
	ModelID  pgtype.Text
}

// estimateAIRegradeCost prices every contested answer via the existing pricing path (per-
// model pricing × the answers heuristic): one "answer" of estimated tokens per contested
// answer, summed by its own pinned provider/model. ok is false only when NO answer had a
// resolvable price.
func (s *Server) estimateAIRegradeCost(ctx context.Context, answers []contestedAnswerPricing) (pgtype.Numeric, bool) {
	sum := new(big.Rat)
	any := false
	for _, a := range answers {
		if !a.Provider.Valid || !a.ModelID.Valid {
			continue
		}
		est, ok := s.estimateRunCost(ctx, a.Provider.String, a.ModelID.String, 1)
		if !ok {
			continue
		}
		sum.Add(sum, store.NumRat(est))
		any = true
	}
	if !any {
		return pgtype.Numeric{}, false
	}
	n, err := store.Num(sum.FloatString(6))
	if err != nil {
		return pgtype.Numeric{}, false
	}
	return n, true
}

// enforceAIRegradeBudget runs the monthly budget gate (D36) shared by BOTH AI re-grade
// paths — the single sub-item (handleAIRegrade) and the batch (handleAIRegradeAll) — so
// the two can't drift (I2). Given an already-computed estimate, it 409s when projected
// month-to-date spend would exceed the cap, returning handled=true so the caller returns
// immediately. No budget configured or "unknown" estimate ⇒ no-op (fails open, D35). A
// 500 for an internal error also returns handled=true.
func (s *Server) enforceAIRegradeBudget(w http.ResponseWriter, ctx context.Context, estimate pgtype.Numeric, estKnown bool) (handled bool) {
	if strings.TrimSpace(s.cfg.MonthlyBudgetUSD) == "" || !estKnown {
		return false
	}
	budget, err := store.Num(s.cfg.MonthlyBudgetUSD)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "invalid configured monthly budget")
		return true
	}
	monthToDate, err := s.store.MonthToDateCost(ctx)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "month-to-date spend check failed")
		return true
	}
	mtdNum, err := store.Num(monthToDate)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "month-to-date spend check failed")
		return true
	}
	projected := new(big.Rat).Add(store.NumRat(mtdNum), store.NumRat(estimate))
	if projected.Cmp(store.NumRat(budget)) > 0 {
		apiError409Budget(w, monthToDate, store.NumStr(estimate), s.cfg.MonthlyBudgetUSD)
		return true
	}
	return false
}
