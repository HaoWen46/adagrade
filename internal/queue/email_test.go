package queue

import (
	"context"
	"errors"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"golang.org/x/time/rate"

	"github.com/HaoWen46/adagrade/internal/publish"
)

type recordingEmailSender struct {
	ref            publish.DeliveryRef
	jobID          int64
	final          bool
	isFirstAttempt bool
	called         bool
	err            error
}

func (s *recordingEmailSender) SendItem(_ context.Context, ref publish.DeliveryRef, jobID int64, final bool, isFirstAttempt bool) error {
	s.ref = ref
	s.jobID = jobID
	s.final = final
	s.isFirstAttempt = isFirstAttempt
	s.called = true
	return s.err
}

func TestEmailWorker_FinalSenderErrorSnoozesInsteadOfDiscarding(t *testing.T) {
	sender := &recordingEmailSender{err: errors.New("terminal state write unavailable")}
	w := &emailSendWorker{client: &Client{}, sender: sender, limiter: rate.NewLimiter(rate.Inf, 1)}
	job := &river.Job[EmailSendArgs]{
		JobRow: &rivertype.JobRow{ID: 77, Attempt: emailSendMaxAttempts},
		Args:   EmailSendArgs{ItemID: 12, Generation: 3},
	}
	err := w.Work(context.Background(), job)
	var snooze *rivertype.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("final sender error = %v, want JobSnooze", err)
	}
	if !sender.called || !sender.final {
		t.Fatalf("sender call = called %v final %v", sender.called, sender.final)
	}

	// Before the final attempt, preserve normal River retry/backoff behavior.
	sender.called = false
	job.Attempt = emailSendMaxAttempts - 1
	if err := w.Work(context.Background(), job); !errors.Is(err, sender.err) {
		t.Fatalf("non-final sender error = %v, want original cause", err)
	}
}

// TestEmailWorker_LimiterCancelAlwaysSnoozes proves cancellation before claim/provider
// never burns an attempt. This includes an ordinary final job timeout: otherwise River
// could discard its last job while the untouched publish item remained pending. The
// snooze delay must be nonzero (A8): a JobSnooze(0) on ctx.Err() would make the job
// immediately available again, so a limiter that keeps seeing a done context (e.g. a
// context that carries an already-past deadline on every retry) would spin the
// scheduler at zero delay instead of merely re-running once the process restarts.
func TestEmailWorker_LimiterCancelAlwaysSnoozes(t *testing.T) {
	for _, stopping := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary-timeout", true: "shutdown"}[stopping], func(t *testing.T) {
			c := &Client{}
			c.stopping.Store(stopping)
			w := &emailSendWorker{client: c, sender: nil, limiter: rate.NewLimiter(rate.Limit(1), 1)}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := w.Work(ctx, &river.Job[EmailSendArgs]{
				JobRow: &rivertype.JobRow{Attempt: emailSendMaxAttempts},
				Args:   EmailSendArgs{ItemID: 123, Generation: 1},
			})
			var snooze *rivertype.JobSnoozeError
			if !errors.As(err, &snooze) {
				t.Fatalf("limiter cancellation: got %v, want JobSnooze", err)
			}
			if snooze.Duration <= 0 {
				t.Fatalf("limiter cancellation snooze duration = %v, want > 0 (zero-delay could spin the scheduler)", snooze.Duration)
			}
		})
	}
}

// TestEmailWorker_ImpossibleLimiterConfigConsumesAttempt proves a non-context limiter
// error is NOT folded into the ctx-done snooze path (A8). rate.Limiter.Wait errors both
// when the context is done AND when burst/limit configuration make the wait impossible
// to ever satisfy (n > burst); only the former is a shutdown/timeout signal. Treating
// both alike created an infinite zero-delay retry loop that bypassed
// emailSendMaxAttempts and never surfaced in the UI. With a live context, Work must
// return the limiter's error unchanged so River applies normal attempt-consuming
// retry/backoff.
func TestEmailWorker_ImpossibleLimiterConfigConsumesAttempt(t *testing.T) {
	sender := &recordingEmailSender{}
	// Burst 0 makes WaitN(ctx, 1) exceed the burst on every call — Wait returns an
	// error synchronously without ever blocking or observing ctx cancellation, so the
	// context passed in stays live (ctx.Err() == nil).
	w := &emailSendWorker{client: &Client{}, sender: sender, limiter: rate.NewLimiter(rate.Limit(1), 0)}
	ctx := context.Background()
	job := &river.Job[EmailSendArgs]{
		JobRow: &rivertype.JobRow{ID: 55, Attempt: 1},
		Args:   EmailSendArgs{ItemID: 5, Generation: 1},
	}

	err := w.Work(ctx, job)
	if err == nil {
		t.Fatal("Work: want error, got nil")
	}
	if ctx.Err() != nil {
		t.Fatalf("test setup: context unexpectedly done: %v", ctx.Err())
	}
	var snooze *rivertype.JobSnoozeError
	if errors.As(err, &snooze) {
		t.Fatalf("Work = JobSnooze(%v), want a plain attempt-consuming error", snooze.Duration)
	}
	if sender.called {
		t.Fatal("sender was called despite the limiter never granting a token")
	}
}

// TestEmailWorker_PassesGenerationAndRiverJobID ensures the durable identity
// reaches the sender unchanged. The sender uses both values for its claim CAS,
// so dropping either here would reopen stale-job and concurrent-send races.
func TestEmailWorker_PassesGenerationAndRiverJobID(t *testing.T) {
	sender := &recordingEmailSender{}
	w := &emailSendWorker{
		client:  &Client{},
		sender:  sender,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}
	job := &river.Job[EmailSendArgs]{
		JobRow: &rivertype.JobRow{ID: 987, Attempt: emailSendMaxAttempts},
		Args:   EmailSendArgs{ItemID: 123, Generation: 4},
	}

	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if !sender.called {
		t.Fatal("sender was not called")
	}
	if got, want := sender.ref, (publish.DeliveryRef{ItemID: 123, Generation: 4}); got != want {
		t.Fatalf("delivery ref = %+v, want %+v", got, want)
	}
	if sender.jobID != 987 {
		t.Fatalf("job id = %d, want 987", sender.jobID)
	}
	if !sender.final {
		t.Fatal("final = false on the maximum attempt, want true")
	}
}

// TestEmailWorker_PassesIsFirstAttempt is A2's seam: the sender's legacy-uncertain-row
// rescue needs to know whether THIS specific job has ever run its Work function
// before. job.Attempt == 1 is River's own guarantee of that; Work must forward it
// unchanged rather than deriving some other approximation.
func TestEmailWorker_PassesIsFirstAttempt(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		want    bool
	}{
		{attempt: 1, want: true},
		{attempt: 2, want: false},
		{attempt: emailSendMaxAttempts, want: false},
	} {
		sender := &recordingEmailSender{}
		w := &emailSendWorker{client: &Client{}, sender: sender, limiter: rate.NewLimiter(rate.Inf, 1)}
		job := &river.Job[EmailSendArgs]{
			JobRow: &rivertype.JobRow{ID: 1, Attempt: tc.attempt},
			Args:   EmailSendArgs{ItemID: 1, Generation: 1},
		}
		if err := w.Work(context.Background(), job); err != nil {
			t.Fatalf("attempt %d: Work: %v", tc.attempt, err)
		}
		if sender.isFirstAttempt != tc.want {
			t.Errorf("attempt %d: isFirstAttempt = %v, want %v", tc.attempt, sender.isFirstAttempt, tc.want)
		}
	}
}
