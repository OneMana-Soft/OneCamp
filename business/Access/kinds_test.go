package business

import (
	"context"
	"errors"
	"testing"
)

// fixedKind decides by id, and counts its reads.
type fixedKind struct {
	levels map[string]Level
	reads  *int
}

func (f fixedKind) DecideMany(_ context.Context, _ Subject, ids []string) (map[string]Decision, error) {
	*f.reads++
	out := map[string]Decision{}
	for _, id := range ids {
		if l, ok := f.levels[id]; ok {
			out[id] = Decision{Level: l}
		}
	}
	return out, nil
}

// A registered kind is decided by its own rule, all of its ids in one read,
// through the resolver everything asks; an id it doesn't know is left out, and
// a kind nothing registered is ErrUnknownKind, as it always was.
func TestARegisteredKindIsDecidedByItsRule(t *testing.T) {
	reads := 0
	RegisterKind("test.kind", fixedKind{levels: map[string]Level{"a": LevelView, "b": LevelEdit}, reads: &reads})

	a, b, gone := Resource{Kind: "test.kind", UUID: "a"}, Resource{Kind: "test.kind", UUID: "b"}, Resource{Kind: "test.kind", UUID: "x"}
	got, err := Rules.(Batch).ResolveAll(context.Background(), Subject{UserUUID: "me"}, []Resource{a, b, gone})
	if err != nil || len(got) != 2 || got[a].Level != LevelView || got[b].Level != LevelEdit || reads != 1 {
		t.Fatalf("got %v (%v) in %d reads", got, err, reads)
	}
	if _, err := Rules.Resolve(context.Background(), Subject{}, gone); !errors.Is(err, ErrNotFound) {
		t.Errorf("an id the rule doesn't know: %v, want ErrNotFound", err)
	}
	if kept, err := Rules.Filter(context.Background(), Subject{}, []Resource{a, b}, LevelEdit); err != nil || len(kept) != 1 || kept[0] != b {
		t.Errorf("filter at edit: %v (%v)", kept, err)
	}
	if _, err := Rules.Resolve(context.Background(), Subject{}, Resource{Kind: "board", UUID: "a"}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("an unregistered kind: %v, want ErrUnknownKind", err)
	}
}

// Two rules for one kind, a rule for docs, or a rule with no name panic where
// they are registered.
func TestAKindIsRegisteredOnce(t *testing.T) {
	reads := 0
	RegisterKind("test.once", fixedKind{reads: &reads})
	for _, kind := range []string{"test.once", KindDoc, ""} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("registering %q didn't panic", kind)
				}
			}()
			RegisterKind(kind, fixedKind{reads: &reads})
		}()
	}
}
