package business

import (
	"context"
	"errors"
	"testing"
)

// peopleRules decides by who asks: a resolver that can't decide for many
// people at once, as a test's is.
type peopleRules struct {
	Resolver
	levels map[string]Level
	err    error
}

func (p peopleRules) Resolve(_ context.Context, who Subject, _ Resource) (Decision, error) {
	if p.err != nil {
		return Decision{}, p.err
	}
	l, ok := p.levels[who.UserUUID]
	if !ok {
		return Decision{}, ErrNotFound
	}
	return Decision{Level: l}, nil
}

// One resource, many people: the resolver decides them in one read (Rules is
// a People); one that can't is asked a person at a time, someone it finds
// nothing for has none, and an empty id is left out.
func TestResolvePeopleDecidesManyPeopleAtOnce(t *testing.T) {
	if _, ok := any(rules{}).(People); !ok {
		t.Fatal("the resolver doesn't decide many people at once")
	}
	if _, err := (rules{}).ResolvePeople(context.Background(), Resource{Kind: "board", UUID: "b"}, []string{"a"}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("a board: %v, want ErrUnknownKind", err)
	}

	was := Rules
	defer func() { Rules = was }()
	Rules = peopleRules{levels: map[string]Level{"ann": LevelEdit, "bea": LevelView}}
	got, err := ResolvePeople(context.Background(), Resource{Kind: KindDoc, UUID: "d"}, []string{"ann", "bea", "zed", ""})
	if err != nil || len(got) != 3 || got["ann"].Level != LevelEdit || got["bea"].Level != LevelView || got["zed"].Level != LevelNone {
		t.Errorf("got %v (%v)", got, err)
	}
	Rules = peopleRules{err: errors.New("graph down")}
	if got, err := ResolvePeople(context.Background(), Resource{Kind: KindDoc, UUID: "d"}, []string{"ann"}); err == nil || got != nil {
		t.Errorf("a resolver that failed answered %v", got)
	}
}
