package Calendar

import (
	"context"
	"testing"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	model "github.com/akashc777/OneCamp/models/postgres/User"
)

// A person's choice to keep when they're busy to themselves is theirs alone:
// setting it writes their own value and nobody else's, reading it reads their
// own, it says whether the workspace shows anyone's at all, and someone who
// is nobody (no person on the request) reads and sets nothing.
func TestOnlyAPersonSetsTheirOwnFreeBusyChoice(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	maya, jonas := h.member("Maya"), h.member("Jonas")
	hidden := map[string]bool{}
	prevGet, prevSet, prevPolicy := personFreeBusySet, setPersonFreeBusy, freeBusyPolicy
	t.Cleanup(func() { personFreeBusySet, setPersonFreeBusy, freeBusyPolicy = prevGet, prevSet, prevPolicy })
	personFreeBusySet = func(_ context.Context, person string) (bool, error) { return hidden[person], nil }
	setPersonFreeBusy = func(_ context.Context, person string, hide bool, by policyBusiness.Changer) error {
		if by.UserID.String() != person {
			t.Fatalf("%s set %s's choice", by.UserID, person)
		}
		hidden[person] = hide
		return nil
	}
	freeBusyPolicy = func(context.Context) bool { return true }

	got, err := SetMyFreeBusy(ctx, maya, true)
	if err != nil || !got.Hidden || !got.Workspace {
		t.Fatalf("Maya hides: %+v (%v)", got, err)
	}
	if len(hidden) != 1 || !hidden[personOf(maya).String()] {
		t.Fatalf("only Maya's own choice is written: %v", hidden)
	}
	if mine, err := MyFreeBusy(ctx, jonas); err != nil || mine.Hidden {
		t.Fatalf("Jonas's own choice is untouched: %+v (%v)", mine, err)
	}
	freeBusyPolicy = func(context.Context) bool { return false }
	if mine, err := MyFreeBusy(ctx, maya); err != nil || !mine.Hidden || mine.Workspace {
		t.Fatalf("with the workspace off, Maya's choice reads as hers and the workspace's off: %+v (%v)", mine, err)
	}
	if got, err := SetMyFreeBusy(ctx, maya, false); err != nil || got.Hidden || hidden[personOf(maya).String()] {
		t.Fatalf("Maya shows again: %+v (%v)", got, err)
	}
	if _, err := SetMyFreeBusy(ctx, &model.UserInfo{}, true); err != ErrEventNotFound {
		t.Fatalf("nobody set a choice: %v", err)
	}
	if _, err := MyFreeBusy(ctx, &model.UserInfo{}); err != ErrEventNotFound {
		t.Fatalf("nobody read a choice: %v", err)
	}
}
