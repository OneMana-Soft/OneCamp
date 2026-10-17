package businness

import (
	"context"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"
)

// Every hook runs once per save, with the save's change, and one that panics
// holds up no other.
func TestEverySaveHookRunsOnItsOwn(t *testing.T) {
	var r SaveHookRegistry
	var mu sync.Mutex
	seen := map[string]Change{}
	done := make(chan struct{}, 3)
	record := func(name string) SaveHook {
		return func(_ context.Context, c Change) {
			mu.Lock()
			seen[name] = c
			mu.Unlock()
			done <- struct{}{}
		}
	}
	r.Register("first", record("first"))
	r.Register("panics", func(context.Context, Change) {
		done <- struct{}{}
		panic("a broken hook")
	})
	r.Register("second", record("second"))

	ctx, cancel := context.WithCancel(context.Background())
	want := Change{DocUUID: "doc-1", Body: "<p>hi</p>", Contributors: []string{"u1"}, SavedAt: time.Now()}
	r.run(ctx, want)
	cancel() // the save's request ending does not stop its hooks
	for range 3 {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("a hook never ran")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"first", "second"} {
		if got := seen[name]; got.DocUUID != want.DocUUID || got.Body != want.Body || len(got.Contributors) != 1 {
			t.Errorf("hook %s saw %+v, want %+v", name, got, want)
		}
	}
}

// A hook with no name or nothing to run, or a second under the same name, fails
// where it is registered.
func TestABadSaveHookFailsWhereItIsRegistered(t *testing.T) {
	hook := func(context.Context, Change) {}
	for _, c := range []struct {
		name  string
		names []string
		hooks []SaveHook
	}{
		{"no name", []string{""}, []SaveHook{hook}},
		{"nothing to run", []string{"a"}, []SaveHook{nil}},
		{"a name taken", []string{"a", "a"}, []SaveHook{hook, hook}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: registering did not panic", c.name)
				}
			}()
			var r SaveHookRegistry
			for i := range c.names {
				r.Register(c.names[i], c.hooks[i])
			}
		}()
	}
}

// A registry with no hook starts nothing at all; the save path's holds the
// hooks the docs programme registered here (A6: who a save mentions), and a
// new one is added here on purpose. doc.updated is no hook: the save emits it
// once itself (savedEvent), and a hook emitting it too made two of every save.
func TestASaveWithNoHooksDoesNothing(t *testing.T) {
	var r SaveHookRegistry
	r.run(context.Background(), Change{DocUUID: "doc-1"}) // must neither block nor panic
	var names []string
	for _, h := range SaveHooks.hooks {
		names = append(names, h.name)
	}
	sort.Strings(names)
	if want := []string{"doc.mentions"}; !slices.Equal(names, want) {
		t.Errorf("the save path's hooks are %v, want %v", names, want)
	}
}
