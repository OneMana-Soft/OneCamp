package business

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// oneEventDecision is an Access Resolver that answers d, or err, for the
// event it is asked about, and records what it was asked.
type oneEventDecision struct {
	accessBusiness.Resolver
	d     accessBusiness.Decision
	err   error
	asked *[]accessBusiness.Resource
	who   *accessBusiness.Subject
}

func (r oneEventDecision) Resolve(_ context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
	*r.asked = append(*r.asked, res)
	*r.who = who
	return r.d, r.err
}

// TestTheEventReachIsWhetherTheEventIsShown: an MCP event tool reaches an
// event the calendar shows the originating person (business/Calendar
// access.go, through the Access Resolver), for a read and for a write alike.
// What a write may change is the calendar's own rule, asked as them when the
// op runs (mayEdit for a change, the organiser for cancel and restore), so a
// guest the organiser lets change the event changes it over MCP as over
// PATCH /v1. An event in the trash answers no read, and a write reaches it
// only for someone it was shown to (Was), for restore_event. An event not
// shown to them is refused, and so is any lookup that fails.
//
// The resolver is stubbed and the person found, so this branch is the only
// guard here: a branch that granted without the resolver fails the cases it
// must refuse.
func TestTheEventReachIsWhetherTheEventIsShown(t *testing.T) {
	const person = "11111111-1111-4111-8111-111111111111"
	const event = "22222222-2222-4222-8222-222222222222"
	savedRules, savedNode := accessBusiness.Rules, principalNode
	t.Cleanup(func() { accessBusiness.Rules, principalNode = savedRules, savedNode })
	principalNode = func(context.Context, string) (*dgraphStruct.DgraphUser, error) {
		return &dgraphStruct.DgraphUser{Uid: "0x1f"}, nil
	}
	view, full, none := accessBusiness.LevelView, accessBusiness.LevelFull, accessBusiness.LevelNone
	cases := []struct {
		name        string
		d           accessBusiness.Decision
		read, write bool
	}{
		{"a guest, a reader or someone who sees its calendar", accessBusiness.Decision{Level: view, Was: view}, true, true},
		{"its organiser", accessBusiness.Decision{Level: full, Owner: true, Was: full}, true, true},
		{"someone it isn't shown to", accessBusiness.Decision{Level: none, Was: none}, false, false},
		{"in the trash, for someone it was shown to", accessBusiness.Decision{Level: none, Deleted: true, Was: view}, false, true},
		{"in the trash, for its organiser", accessBusiness.Decision{Level: none, Owner: true, Deleted: true, Was: full}, false, true},
		{"in the trash, for someone it was never shown to", accessBusiness.Decision{Level: none, Deleted: true, Was: none}, false, false},
	}
	for _, c := range cases {
		for _, access := range []Access{AccessRead, AccessWrite} {
			var asked []accessBusiness.Resource
			var who accessBusiness.Subject
			accessBusiness.Rules = oneEventDecision{d: c.d, asked: &asked, who: &who}
			got := PrincipalCanReach(context.Background(), person, ResourceRef{Kind: ResourceEvent, ID: " " + event + " ", Access: access})
			want := c.read
			if access == AccessWrite {
				want = c.write
			}
			if got.Allow != want {
				t.Errorf("%s, %s: allowed %v (%q), want %v", c.name, access, got.Allow, got.Reason, want)
			}
			if got.Allow && got.PrincipalDgraphUID != "0x1f" {
				t.Errorf("%s, %s: allowed for graph uid %q, want the person's 0x1f", c.name, access, got.PrincipalDgraphUID)
			}
			if len(asked) != 1 || asked[0] != (accessBusiness.Resource{Kind: string(ResourceEvent), UUID: event}) ||
				who != (accessBusiness.Subject{UserUID: "0x1f", UserUUID: person}) {
				t.Errorf("%s, %s: the resolver was asked %v for %+v, want the event %s for the person", c.name, access, asked, who, event)
			}
		}
	}
	// A lookup that fails is a refusal, never a pass.
	for _, access := range []Access{AccessRead, AccessWrite} {
		var asked []accessBusiness.Resource
		var who accessBusiness.Subject
		accessBusiness.Rules = oneEventDecision{d: accessBusiness.Decision{Level: full, Was: full}, err: errors.New("store down"), asked: &asked, who: &who}
		if got := PrincipalCanReach(context.Background(), person, ResourceRef{Kind: ResourceEvent, ID: event, Access: access}); got.Allow {
			t.Errorf("a failed lookup, %s: allowed (%q)", access, got.Reason)
		}
	}
}
