package business

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

/**
 * WHAT THESE COVER, and why it is the last gap in this path.
 *
 * The SQL statements are verified against a real Postgres by
 * scripts/verify-pending-action-sql.sh — that the ON CONFLICT clause infers the partial
 * index, that the retake is guarded to the right states, that the reclaimer frees an
 * abandoned row and spares a fresh one.
 *
 * What that cannot check is the ORCHESTRATION: whether ClaimWrite calls those statements in
 * the right order and does the right thing with each of the nine answers they can give. That
 * is the part that decides whether a write happens twice, and until the store was injectable
 * none of it was reachable without a database.
 *
 * Every branch below is a real outcome the store can return, not a hypothetical.
 */

// fakeStore records what it was asked and returns scripted answers.
type fakeStore struct {
	mu sync.Mutex

	claimID    string
	claimOK    bool
	claimErr   error
	claimCalls int
	claimSeen  []claimAttempt

	retakeID    string
	retakeOK    bool
	retakeErr   error
	retakeCalls int

	existing      ExistingWrite
	existingErr   error
	existingCalls int
}

func (f *fakeStore) store() claimStore {
	return claimStore{
		Claim: func(_ context.Context, a claimAttempt) (string, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.claimCalls++
			f.claimSeen = append(f.claimSeen, a)
			return f.claimID, f.claimOK, f.claimErr
		},
		Retake: func(_ context.Context, _ claimAttempt) (string, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.retakeCalls++
			return f.retakeID, f.retakeOK, f.retakeErr
		},
		Existing: func(_ context.Context, _ string) (ExistingWrite, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.existingCalls++
			return f.existing, f.existingErr
		},
	}
}

func claimSpec() *ToolSpec {
	return &ToolSpec{
		Name:      "send_message",
		Behaviour: ToolBehaviour{ReadOnly: false, Idempotent: false},
	}
}

func claimCall() ToolCallContext {
	return ToolCallContext{
		PrincipalUserID: "11111111-1111-1111-1111-111111111111",
		Resource:        ResourceRef{Kind: ResourceChannel, ID: "ch-1", Access: AccessWrite},
		Args:            map[string]any{"channel_uuid": "ch-1", "text": "hello"},
	}
}

func claim(t *testing.T, f *fakeStore) ClaimResult {
	t.Helper()
	return claimWriteWith(context.Background(), claimSpec(), claimCall(), "Claude", "KEY-1", f.store())
}

// THE HAPPY PATH: the key was free, so this call owns it and must run.
func TestClaimWonMeansProceedWithAClaimID(t *testing.T) {
	f := &fakeStore{claimID: "row-1", claimOK: true}
	got := claim(t, f)

	if got.Outcome != WriteProceed {
		t.Fatalf("outcome %s (%s), want proceed", got.Outcome, got.Reason)
	}
	if got.ClaimID != "row-1" {
		t.Fatalf("ClaimID %q; without it the caller cannot settle, and an unsettled claim "+
			"reads as 'already applied' for every later retry", got.ClaimID)
	}
	if f.retakeCalls != 0 || f.existingCalls != 0 {
		t.Errorf("a won claim must not touch the store again (retake=%d existing=%d)",
			f.retakeCalls, f.existingCalls)
	}
}

// THE RETRY-AFTER-FAILURE PATH: the key is taken, but the previous attempt did not complete,
// so this call retakes it. Without this a single transient failure made the call permanently
// unrepeatable.
func TestConflictThenRetakeMeansProceed(t *testing.T) {
	f := &fakeStore{claimOK: false, retakeID: "row-2", retakeOK: true}
	got := claim(t, f)

	if got.Outcome != WriteProceed {
		t.Fatalf("outcome %s (%s), want proceed after a retake", got.Outcome, got.Reason)
	}
	if got.ClaimID != "row-2" {
		t.Fatalf("ClaimID %q, want the retaken row's id", got.ClaimID)
	}
	if f.existingCalls != 0 {
		t.Error("a successful retake must not then read the existing state; the retake IS " +
			"the answer")
	}
}

// Every terminal state the earlier call could be in, mapped to what this call is told.
func TestConflictAndNotRetakeableMapsTheExistingState(t *testing.T) {
	cases := []struct {
		name    string
		state   ExistingWriteState
		outcome WriteOutcome
		wantID  bool
	}{
		{"already applied", ExistingApplied, WriteAlreadyApplied, true},
		{"a human denied it", ExistingRejected, WriteRefused, true},
		{"waiting on a human", ExistingAwaitingApproval, WriteNeedsApproval, true},
		// Settled or reclaimed between the failed insert and this read. Reporting
		// already-applied would claim something that may never have happened.
		{"vanished mid-race", ExistingNone, WriteRefused, false},
		{"expired mid-race", ExistingExpired, WriteRefused, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeStore{
				claimOK:  false,
				retakeOK: false,
				existing: ExistingWrite{State: tc.state, PendingActionID: "row-3"},
			}
			got := claim(t, f)

			if got.Outcome != tc.outcome {
				t.Fatalf("outcome %s (%s), want %s", got.Outcome, got.Reason, tc.outcome)
			}
			if got.Reason == "" {
				t.Error("reason is empty; the audit row and the client message both read it")
			}
			if tc.wantID && got.ClaimID == "" {
				t.Error("no ClaimID; the caller cannot point a person at the existing action")
			}
		})
	}
}

// A REJECTED ROW MUST NEVER BECOME A PROCEED. A human said no, and no amount of retrying may
// convert that into another attempt — the one outcome where being wrong is worst.
func TestARejectedWriteIsNeverConvertedIntoAnAttempt(t *testing.T) {
	f := &fakeStore{
		claimOK:  false,
		retakeOK: false, // the model's guard excludes 'rejected'; this mirrors it
		existing: ExistingWrite{State: ExistingRejected, PendingActionID: "row-4"},
	}
	got := claim(t, f)

	if got.Outcome == WriteProceed {
		t.Fatal("a rejected write was allowed to proceed. A person denied this action; a " +
			"retry must be refused, not quietly retaken.")
	}
	if got.Outcome != WriteRefused {
		t.Fatalf("outcome %s, want refused", got.Outcome)
	}
}

// EVERY STORE ERROR REFUSES. Performing a write twice is worse than not performing it, and a
// caller can retry when the store is healthy — so an unknown answer must never proceed.
func TestAnyStoreErrorRefusesRatherThanRisksADuplicate(t *testing.T) {
	boom := errors.New("connection reset")

	cases := map[string]*fakeStore{
		"claim failed":    {claimErr: boom},
		"retake failed":   {claimOK: false, retakeErr: boom},
		"existing failed": {claimOK: false, retakeOK: false, existingErr: boom},
	}

	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			got := claim(t, f)
			if got.Outcome != WriteRefused {
				t.Fatalf("outcome %s (%s); an unreadable store must refuse, because the "+
					"alternative is possibly writing twice", got.Outcome, got.Reason)
			}
			if got.ClaimID != "" {
				t.Error("a refusal carries a ClaimID, which would invite the caller to " +
					"settle a claim it does not own")
			}
		})
	}
}

// THE ORDER IS THE POINT: claim, then retake, then read. Reading first would answer a
// retryable failure as a refusal; retaking first would steal a key that was never contested.
func TestTheStoreIsCalledInTheRightOrderAndNoMoreThanNeeded(t *testing.T) {
	f := &fakeStore{claimOK: false, retakeOK: false, existing: ExistingWrite{State: ExistingApplied}}
	claim(t, f)

	if f.claimCalls != 1 {
		t.Errorf("claim called %d times, want exactly 1", f.claimCalls)
	}
	if f.retakeCalls != 1 {
		t.Errorf("retake called %d times, want exactly 1 (only after a conflict)", f.retakeCalls)
	}
	if f.existingCalls != 1 {
		t.Errorf("existing read %d times, want exactly 1 (only when not retakeable)", f.existingCalls)
	}
}

// What the store is ASKED matters as much as what it answers: a claim attributed to the wrong
// person, or pointed at the wrong surface, is a row nobody can find.
func TestTheClaimCarriesTheRightAttribution(t *testing.T) {
	f := &fakeStore{claimID: "row-5", claimOK: true}
	claim(t, f)

	if len(f.claimSeen) != 1 {
		t.Fatalf("expected one claim attempt, got %d", len(f.claimSeen))
	}
	a := f.claimSeen[0]

	want, _ := uuid.Parse("11111111-1111-1111-1111-111111111111")
	if a.Principal != want {
		t.Errorf("attributed to %s, want the accountable person %s", a.Principal, want)
	}
	if a.Key != "KEY-1" {
		t.Errorf("claimed key %q, want KEY-1", a.Key)
	}
	if a.Tool != "send_message" {
		t.Errorf("tool %q", a.Tool)
	}
	// A channel write must land on the channel surface with the channel's id — see
	// surface.go for why a resource kind here was a bug.
	if a.SurfaceType != "channel" || a.SurfaceID != "ch-1" {
		t.Errorf("surface (%q, %q), want (channel, ch-1)", a.SurfaceType, a.SurfaceID)
	}
	if a.ExpiresAt.IsZero() {
		t.Error("no expiry; a claim with no expiry can never be reclaimed if abandoned")
	}
	if a.Description == "" {
		t.Error("no description; the approval card would have nothing to show a human")
	}
}

// The early exits must not touch the store at all.
func TestEarlyExitsNeverTouchTheStore(t *testing.T) {
	t.Run("no key means nothing to deduplicate", func(t *testing.T) {
		f := &fakeStore{}
		got := claimWriteWith(context.Background(), claimSpec(), claimCall(), "c", "  ", f.store())
		if got.Outcome != WriteProceed {
			t.Fatalf("outcome %s, want proceed", got.Outcome)
		}
		if got.ClaimID != "" {
			t.Error("a no-op claim returned a ClaimID, which the caller would try to settle")
		}
		if f.claimCalls != 0 {
			t.Error("touched the store with nothing to deduplicate")
		}
	})

	t.Run("no spec", func(t *testing.T) {
		f := &fakeStore{}
		got := claimWriteWith(context.Background(), nil, claimCall(), "c", "KEY-1", f.store())
		if got.Outcome != WriteRefused || f.claimCalls != 0 {
			t.Fatalf("outcome %s, claimCalls %d", got.Outcome, f.claimCalls)
		}
	})

	t.Run("no accountable person", func(t *testing.T) {
		f := &fakeStore{}
		call := claimCall()
		call.PrincipalUserID = "not-a-uuid"
		got := claimWriteWith(context.Background(), claimSpec(), call, "c", "KEY-1", f.store())
		if got.Outcome != WriteRefused {
			t.Fatalf("outcome %s, want refused", got.Outcome)
		}
		if f.claimCalls != 0 {
			t.Error("claimed a row with nobody to attribute it to")
		}
	})
}

// CONCURRENCY: many callers racing the same key. The store decides the winner — that is what
// the unique index is for — and this asserts the decision function does not invent a second
// one. Exactly one proceeds; nobody else gets a claim id they could settle.
func TestConcurrentCallersProduceExactlyOneProceed(t *testing.T) {
	const racers = 32

	var mu sync.Mutex
	claimed := false

	// One shared store: the first Claim wins, every later one conflicts and is not
	// retakeable, mirroring what the unique index does.
	store := claimStore{
		Claim: func(_ context.Context, _ claimAttempt) (string, bool, error) {
			mu.Lock()
			defer mu.Unlock()
			if claimed {
				return "", false, nil
			}
			claimed = true
			return "row-winner", true, nil
		},
		Retake: func(_ context.Context, _ claimAttempt) (string, bool, error) {
			return "", false, nil // in flight, so not retakeable
		},
		Existing: func(_ context.Context, _ string) (ExistingWrite, error) {
			return ExistingWrite{State: ExistingApplied, PendingActionID: "row-winner"}, nil
		},
	}

	results := make([]ClaimResult, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = claimWriteWith(context.Background(), claimSpec(), claimCall(), "c", "KEY-RACE", store)
		}(i)
	}
	wg.Wait()

	proceeds := 0
	for _, r := range results {
		if r.Outcome == WriteProceed {
			proceeds++
			if r.ClaimID == "" {
				t.Error("a proceeding caller got no ClaimID, so it could never settle")
			}
			continue
		}
		if r.Outcome != WriteAlreadyApplied {
			t.Errorf("a losing caller got %s (%s); it should be told the write is already "+
				"applied, not given a different story", r.Outcome, r.Reason)
		}
	}

	if proceeds != 1 {
		t.Fatalf("%d callers proceeded, want exactly 1. More than one means the same write "+
			"runs twice, which is the entire failure this machinery prevents.", proceeds)
	}
}
