package business

import (
	"context"
	"errors"
	"net/http"
	"testing"

	callQABusiness "github.com/akashc777/OneCamp/business/CallQA"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	pollBusiness "github.com/akashc777/OneCamp/business/Poll"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// Starting, closing and sharing polls, and running Q&A, are for whoever runs
// the call: a channel's participant, an invitee and a guest are refused before
// anything is read or written. Voting, asking and upvoting are everyone's.
func TestOnlyWhoRunsTheCallRunsItsActivities(t *testing.T) {
	for _, op := range []string{OpPoll, OpClosePoll, OpShare, OpQA, OpQuestion} {
		if !RunsActivities(op) {
			t.Errorf("%s is for whoever runs the call", op)
		}
	}
	for _, op := range []string{OpVote, OpAsk, OpUpvote} {
		if RunsActivities(op) {
			t.Errorf("%s is everyone's", op)
		}
	}
	for _, role := range []string{model.RoleParticipant, model.RoleInvitee, model.RoleGuest} {
		s := callWith("channel", callStateBusiness.Person{Identity: "0x2", Role: role})
		for _, op := range []string{OpPoll, OpClosePoll, OpShare, OpQA, OpQuestion} {
			if _, err := do(context.Background(), s, s.Person("0x2"), Activity{Op: op}); !errors.Is(err, ErrNotAllowed) {
				t.Errorf("a channel call's %s doing %s: %v", role, op, err)
			}
		}
	}
	// In a DM or a group nobody hosts, so anyone in the call runs its polls.
	for _, kind := range []string{"dm", "group"} {
		s := callWith(kind, callStateBusiness.Person{Identity: "0x2", Role: model.RoleParticipant})
		if !runsActivities(s, s.Person("0x2")) {
			t.Errorf("in a %s's call, an equal can't run its polls", kind)
		}
	}
}

// A guest's token reaches votes, questions and upvotes only: what whoever
// runs the call does is refused before the token is even read.
func TestAGuestRunsNothing(t *testing.T) {
	for _, op := range []string{OpPoll, OpClosePoll, OpShare, OpQA, OpQuestion} {
		if _, err := GuestDoesActivity(context.Background(), "", Activity{Op: op}); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("a guest doing %s: %v", op, err)
		}
	}
}

// An API token never reaches a call's activities, to read or to act.
func TestActivitiesAreNeverThroughAToken(t *testing.T) {
	ctx := helpers.WithActsThroughToken(context.Background())
	if _, err := ActivitiesFor(ctx, nil, "s"); !errors.Is(err, ErrThroughToken) {
		t.Errorf("a token's read: %v", err)
	}
	if _, err := DoActivity(ctx, nil, "s", Activity{Op: OpVote}); !errors.Is(err, ErrThroughToken) {
		t.Errorf("a token's vote: %v", err)
	}
}

// A refusal says what to fix in a sentence of its own, and a poll or a
// question that isn't the call's is gone, not forbidden.
func TestActivityRefusalsSayWhatToFix(t *testing.T) {
	var r *Refusal
	if err := activityRefusal(pollBusiness.InputError("a poll can have at most 10 options")); !errors.As(err, &r) || r.Status != http.StatusBadRequest || r.Msg != "A poll can have at most 10 options." {
		t.Errorf("a poll's words: %v", err)
	}
	if err := activityRefusal(callQABusiness.InputError("Keep a question under 300 characters.")); !errors.As(err, &r) || r.Msg != "Keep a question under 300 characters." {
		t.Errorf("a question's words: %v", err)
	}
	for _, gone := range []error{pollBusiness.ErrNoAccess, callQABusiness.ErrNotFound} {
		if err := activityRefusal(gone); !errors.Is(err, ErrNoActivity) {
			t.Errorf("%v: %v", gone, err)
		}
	}
	other := errors.New("postgres is down")
	if err := activityRefusal(other); !errors.Is(err, other) {
		t.Errorf("anything else is itself: %v", err)
	}
}

// A packet says which kind of change it was only where a screen with the
// panel shut should say so, and never for what one person did on their own
// account: a vote or an upvote, which would time someone's choice.
func TestAPacketSaysWhatChangedOnlyWhereScreensShouldSaySo(t *testing.T) {
	want := map[string]string{OpPoll: ToldPoll, OpShare: ToldResults, OpQA: ToldQA, OpAsk: ToldQuestion}
	for _, op := range []string{OpPoll, OpClosePoll, OpShare, OpVote, OpQA, OpAsk, OpUpvote, OpQuestion} {
		if got := told(op); got != want[op] {
			t.Errorf("%s's packet says %q, want %q", op, got, want[op])
		}
	}
}
