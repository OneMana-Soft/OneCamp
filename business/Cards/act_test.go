package business

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A press on a card is checked on the server: the card is ours and current,
// the action is one it has, its host and its actors let who press, and only
// then does its owner hear of it.

type fakeHost struct{ allow map[string]bool }

func (fakeHost) Kind() string { return "test-host" }
func (h fakeHost) MayAct(_ context.Context, who Who, _ CardRef) bool {
	return h.allow[who.UserUUID]
}

func sealFor(t *testing.T, ref CardRef, owner Owner, actors []string, ttl time.Duration) string {
	t.Helper()
	card, _ := Reduce([]byte(`[{"type": "actions", "elements": [{"type": "button", "text": "Approve", "action_id": "approve", "value": "1"}]}]`))
	tok, err := Seal(ref, owner, card, actors, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestAPressReachesTheOwnerOnlyWhenEveryCheckPasses(t *testing.T) {
	t.Setenv("JWT_SECRET", "cards-test-secret")
	RegisterHost(fakeHost{allow: map[string]bool{"sam": true, "ana": true}})
	var heard []Action
	RegisterOwner("test-owner", func(_ context.Context, a Action) (*Reply, error) {
		heard = append(heard, a)
		return &Reply{Text: "Approved."}, nil
	})
	ref := CardRef{Host: "test-host", ID: "m1"}
	owner := Owner{Kind: "test-owner", ID: "deploys"}

	tok := sealFor(t, ref, owner, nil, time.Hour)
	r, err := Act(context.Background(), Who{UserUUID: "sam"}, tok, "approve", "1")
	if err != nil || r.Text != "Approved." || len(heard) != 1 || heard[0].Ref != ref || heard[0].Owner != owner || heard[0].Value != "1" {
		t.Fatalf("a good press: %+v, %v, heard %+v", r, err, heard)
	}

	cases := []struct {
		name string
		who  string
		tok  string
		act  string
		want error
	}{
		{"an action the card hasn't", "sam", tok, "deploy", ErrNoAction},
		{"someone the host refuses", "eve", tok, "approve", ErrNotYours},
		{"no one", "", tok, "approve", ErrNotYours},
		{"a token changed by hand", "sam", tok[:len(tok)-2] + "xx", "approve", ErrBadToken},
		{"not a token", "sam", "nonsense", "approve", ErrBadToken},
		{"an expired card", "sam", sealFor(t, ref, owner, nil, -time.Minute), "approve", ErrExpired},
		{"someone the card doesn't name", "ana", sealFor(t, ref, owner, []string{"sam"}, time.Hour), "approve", ErrNotYours},
		{"a card no host holds", "sam", sealFor(t, CardRef{Host: "nowhere", ID: "x"}, owner, nil, time.Hour), "approve", ErrNotYours},
		{"a card nothing answers", "sam", sealFor(t, ref, Owner{Kind: "gone", ID: "x"}, nil, time.Hour), "approve", ErrNoOwner},
	}
	for _, c := range cases {
		if _, err := Act(context.Background(), Who{UserUUID: c.who}, c.tok, c.act, ""); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, not %v", c.name, err, c.want)
		}
	}
	if len(heard) != 1 {
		t.Fatalf("the owner heard %d presses; only the good one should reach it", len(heard))
	}
}

func TestACardShownToOnePersonIsTheirsAlone(t *testing.T) {
	t.Setenv("JWT_SECRET", "cards-test-secret")
	RegisterOwner("test-owner-2", func(context.Context, Action) (*Reply, error) { return &Reply{}, nil })
	tok := sealFor(t, CardRef{Host: HostEphemeral, ID: "trigger-1"}, Owner{Kind: "test-owner-2"}, []string{"sam"}, time.Hour)
	if _, err := Act(context.Background(), Who{UserUUID: "sam"}, tok, "approve", ""); err != nil {
		t.Fatalf("its person: %v", err)
	}
	if _, err := Act(context.Background(), Who{UserUUID: "ana"}, tok, "approve", ""); !errors.Is(err, ErrNotYours) {
		t.Fatalf("someone else: %v", err)
	}
}

func TestNoSecretNoCards(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	card, _ := Reduce([]byte(`[{"type": "divider"}]`))
	if _, err := Seal(CardRef{Host: HostEphemeral}, Owner{}, card, nil, time.Hour); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("sealing with no secret: %v", err)
	}
}
