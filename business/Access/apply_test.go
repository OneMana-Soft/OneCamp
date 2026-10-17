package business

import (
	"context"
	"errors"
	"testing"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
)

// Apply's rule, the one apply asks when a way of changing sharing names no
// rule of its own: full access, and nothing less.
func TestApplysRuleIsFullAccess(t *testing.T) {
	for l, want := range map[Level]error{LevelFull: nil, LevelEdit: ErrForbidden, LevelComment: ErrForbidden, LevelView: ErrForbidden} {
		if got := fullAccess(Decision{Level: l}, Change{}); !errors.Is(got, want) {
			t.Errorf("%s: %v, want %v", l, got, want)
		}
	}
	if (applyOpts{}).allow != nil {
		t.Error("Apply's options name a rule; nil is full access")
	}
}

// apply decides through Rules, as every other access check does, and says no
// before it reads the sharing: a doc that isn't there, one in the trash and a
// change its rule refuses are answered with no read of the graph (this test
// has none). An editor's invite (B6) wraps apply and keeps this order.
func TestApplyAsksTheResolverAndItsRuleBeforeReading(t *testing.T) {
	defer func(r Resolver) { Rules = r }(Rules)
	res := Resource{Kind: KindDoc, UUID: "dddddddd-0000-4000-8000-000000000001"}
	add := Change{Resource: res, Add: []Grant{{Kind: PrincipalUser, UUID: "dddddddd-0000-4000-8000-000000000002", Level: LevelView}}}
	refuse := errors.New("refused by the rule")
	for _, tc := range []struct {
		d     Decision
		allow func(Decision, Change) error
		want  error
	}{
		{Decision{Level: LevelNone}, nil, ErrNotFound},
		{Decision{Deleted: true, Was: LevelFull}, nil, docBusiness.ErrDocDeleted},
		{Decision{Level: LevelEdit}, nil, ErrForbidden},
		{Decision{Level: LevelFull}, func(Decision, Change) error { return refuse }, refuse},
	} {
		Rules = oneDecision{d: tc.d}
		if _, err := apply(context.Background(), Subject{UserUUID: "x"}, add, applyOpts{allow: tc.allow}); !errors.Is(err, tc.want) {
			t.Errorf("%+v: %v, want %v", tc.d, err, tc.want)
		}
	}
	if _, err := Apply(context.Background(), Subject{}, Change{Resource: Resource{Kind: "board", UUID: res.UUID}}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("a board: %v", err)
	}
}

// oneDecision answers one decision for everything.
type oneDecision struct {
	rules
	d Decision
}

func (o oneDecision) Resolve(context.Context, Subject, Resource) (Decision, error) { return o.d, nil }
