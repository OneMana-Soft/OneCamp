package business

import (
	"context"
	"testing"
)

// fake is an adapter that answers nothing.
type fake struct{ name string }

func (f fake) Provider() string                                { return f.name }
func (fake) Calendars(context.Context, Link) ([]Remote, error) { return nil, nil }
func (fake) Pull(context.Context, Remote, string) ([]Change, string, error) {
	return nil, "", nil
}
func (fake) Push(context.Context, Op) (Ref, error)        { return Ref{}, nil }
func (fake) Watch(context.Context, Remote) (Watch, error) { return Watch{}, nil }
func (fake) Stop(context.Context, Watch) error            { return nil }

// Day 0 has no adapter: nothing syncs, as today.
func TestNoProviderSyncsYet(t *testing.T) {
	for _, p := range []string{"google", "microsoft", "caldav", "ics"} {
		if _, ok := Adapters.For(p); ok {
			t.Errorf("%s has an adapter on day 0", p)
		}
	}
}

// An adapter registers once per provider, and is found by it.
func TestAnAdapterRegistersOncePerProvider(t *testing.T) {
	r := &registry{byProvider: map[string]Adapter{}}
	r.Register(fake{"google"})
	if a, ok := r.For("google"); !ok || a.Provider() != "google" {
		t.Fatalf("not found: %v %v", a, ok)
	}
	for _, again := range []Adapter{fake{"google"}, fake{""}, nil} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("registering %v didn't panic", again)
				}
			}()
			r.Register(again)
		}()
	}
}

// readOnly is an adapter of a feed that is only read.
type readOnly struct{ fake }

func (readOnly) ReadsOnly() bool { return true }

// TwoWay names the providers that sync both ways, each once and in order:
// not a feed that is only read, which has a sweep of its own.
func TestTwoWayNamesTheProvidersThatSyncBothWays(t *testing.T) {
	r := &registry{byProvider: map[string]Adapter{}}
	r.Register(fake{"microsoft"})
	r.Register(readOnly{fake{"ics"}})
	r.Register(fake{"google"})
	if got := r.TwoWay(); len(got) != 2 || got[0] != "google" || got[1] != "microsoft" {
		t.Fatalf("two-way %v", got)
	}
}
