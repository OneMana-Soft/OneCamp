package businness

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/Doc"
)

type eventsSeen struct {
	mu     sync.Mutex
	events []resourceEventBusiness.Event
}

func (e *eventsSeen) Emit(_ context.Context, ev resourceEventBusiness.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

// saveWorld swaps the graph, the re-index, the hooks and the event bus for
// counters, and answers prevHash as the hash the upsert found.
type saveWorld struct {
	writes  []domain.CollabWrite
	fanOuts int
	hooks   chan Change
	events  *eventsSeen
	stored  map[string]string // doc -> body hash
	trashed map[string]bool
}

func newSaveWorld(t *testing.T) *saveWorld {
	t.Helper()
	w := &saveWorld{hooks: make(chan Change, 8), events: &eventsSeen{}, stored: map[string]string{}, trashed: map[string]bool{}}
	prevWrite, prevFan, prevHooks, prevBus, prevRead := writeCollabSave, fanOutSavedBody, SaveHooks, resourceEventBusiness.Bus, readStoredBody
	writeCollabSave = func(_ context.Context, cw domain.CollabWrite) (domain.CollabWritten, error) {
		w.writes = append(w.writes, cw)
		if w.trashed[cw.DocUUID] {
			return domain.CollabWritten{}, domain.ErrDocDeleted
		}
		prev, ok := w.stored[cw.DocUUID]
		if !ok {
			return domain.CollabWritten{}, ErrNotFound
		}
		if cw.StateOnly {
			return domain.CollabWritten{PrevHash: prev, Stored: prev == cw.Hash}, nil
		}
		w.stored[cw.DocUUID] = cw.Hash
		return domain.CollabWritten{PrevHash: prev, Stored: true}, nil
	}
	fanOutSavedBody = func(context.Context, Change) { w.fanOuts++ }
	SaveHooks = &SaveHookRegistry{}
	SaveHooks.Register("test.counter", func(_ context.Context, c Change) { w.hooks <- c })
	resourceEventBusiness.Bus = w.events
	readStoredBody = func(context.Context, string) (string, error) { return "<p>stored</p>", nil }
	t.Cleanup(func() {
		writeCollabSave, fanOutSavedBody, SaveHooks, resourceEventBusiness.Bus, readStoredBody = prevWrite, prevFan, prevHooks, prevBus, prevRead
	})
	return w
}

const saveDoc = "8a1b2c3d-4e5f-4a6b-9c7d-8e9f0a1b2c3d"

// A save whose body hashes the same as the stored one changes nothing anyone
// can see, and fans out to nothing: no re-index, no hook, no event (I15).
func TestAnUnchangedSaveSkipsItsSideEffects(t *testing.T) {
	w := newSaveWorld(t)
	body := "<p>Ship on Thursday.</p>"
	w.stored[saveDoc] = collabBodyHash(body)
	for i := 0; i < 5; i++ {
		got, err := SaveFromCollab(context.Background(), CollabSave{DocUUID: saveDoc, HTML: body, YjsState: "c3RhdGU=", Contributors: []string{"ana"}})
		if err != nil || !got.Stored || got.Changed {
			t.Fatalf("save %d: %+v %v", i, got, err)
		}
	}
	if w.fanOuts != 0 || len(w.events.events) != 0 {
		t.Errorf("unchanged saves fanned out %d times and emitted %d events", w.fanOuts, len(w.events.events))
	}
	select {
	case c := <-w.hooks:
		t.Errorf("a hook ran for an unchanged save: %+v", c)
	case <-time.After(50 * time.Millisecond):
	}
	if len(w.writes) != 5 {
		t.Errorf("%d writes, want one per save (the state still moves)", len(w.writes))
	}
}

// A changed save is one write holding the body, its preview, the state, the
// hash and the lineage; then it re-indexes once, runs the hooks once with what
// changed, and says doc.updated, credited to whoever made it (I14).
func TestAChangedSaveIsOneWriteThenEverythingOnce(t *testing.T) {
	w := newSaveWorld(t)
	w.stored[saveDoc] = "an-older-hash"
	body := "<h1>Launch</h1><p>Ship on Friday.</p>"
	got, err := SaveFromCollab(context.Background(), CollabSave{
		DocUUID: saveDoc, HTML: body, YjsState: "c3RhdGU=", Lineage: "L1",
		Agents: []AgentContributor{{AgentUUID: "captain", AskerUUID: "sam"}},
	})
	if err != nil || !got.Changed {
		t.Fatalf("%+v %v", got, err)
	}
	if len(w.writes) != 1 {
		t.Fatalf("%d writes, want 1", len(w.writes))
	}
	cw := w.writes[0]
	if cw.Body != body || cw.State != "c3RhdGU=" || cw.Hash != collabBodyHash(body) || cw.Lineage != "L1" || cw.Snippet != "Launch Ship on Friday." || cw.StateOnly {
		t.Errorf("the write: %+v", cw)
	}
	if w.fanOuts != 1 {
		t.Errorf("re-indexed %d times", w.fanOuts)
	}
	select {
	case c := <-w.hooks:
		if c.Hash != cw.Hash || c.PrevHash != "an-older-hash" || c.Lineage != "L1" || len(c.Agents) != 1 {
			t.Errorf("the hook saw %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no hook ran")
	}
	if len(w.events.events) != 1 {
		t.Fatalf("%d events", len(w.events.events))
	}
	e := w.events.events[0]
	if e.Kind != resourceEventBusiness.DocUpdated || e.ResourceUUID != saveDoc || e.Actor.AgentUUID != "captain" || e.Actor.UserUUID != "sam" {
		t.Errorf("event %+v", e)
	}
}

// A state-only save stores the state against the body it goes with, and says
// when that body is not the stored one; the trash takes nothing; a blank body
// is refused.
func TestStateOnlyTrashAndBlankSaves(t *testing.T) {
	w := newSaveWorld(t)
	w.stored[saveDoc] = collabBodyHash("<p>stored</p>")

	got, err := SaveFromCollab(context.Background(), CollabSave{DocUUID: saveDoc, KeepBody: true, YjsState: "eA==", ExpectBodyHash: "another"})
	if err != nil || got.Stored {
		t.Errorf("a state for another body: %+v %v", got, err)
	}
	// No hash: the service could not write the HTML; stored against the stored body.
	got, err = SaveFromCollab(context.Background(), CollabSave{DocUUID: saveDoc, KeepBody: true, YjsState: "eA=="})
	if err != nil || !got.Stored || w.writes[len(w.writes)-1].Hash != collabBodyHash("<p>stored</p>") {
		t.Errorf("a state without a hash: %+v %v", got, err)
	}
	if _, err := SaveFromCollab(context.Background(), CollabSave{DocUUID: saveDoc, KeepBody: true}); err == nil {
		t.Error("a state-only save with no state was accepted")
	}

	w.trashed[saveDoc] = true
	if _, err := SaveFromCollab(context.Background(), CollabSave{DocUUID: saveDoc, HTML: "<p>x</p>", YjsState: "eA=="}); !errors.Is(err, ErrDocDeleted) {
		t.Errorf("trash: %v", err)
	}
	for _, blank := range []string{"", "  ", "<p>Error generating HTML</p>"} {
		if _, err := SaveFromCollab(context.Background(), CollabSave{DocUUID: saveDoc, HTML: blank, YjsState: "eA=="}); !errors.Is(err, ErrUnsafeCollabBody) {
			t.Errorf("blank %q: %v", blank, err)
		}
	}
	if w.fanOuts != 0 {
		t.Errorf("fanned out %d times", w.fanOuts)
	}
}

// An emptied doc's body is saved: what the service sends for one is an empty
// paragraph, not a blank.
func TestAnEmptiedDocIsSaved(t *testing.T) {
	w := newSaveWorld(t)
	w.stored[saveDoc] = collabBodyHash("<p>Something</p>")
	got, err := SaveFromCollab(context.Background(), CollabSave{DocUUID: saveDoc, HTML: `<p xmlns="http://www.w3.org/1999/xhtml"></p>`, YjsState: "eA=="})
	if err != nil || !got.Changed {
		t.Fatalf("%+v %v", got, err)
	}
}

// A changed save names its last editor: the one person, an agent's asker, or
// the first of several in order; ids that are not uuids are never written.
func TestAChangedSaveNamesItsLastEditor(t *testing.T) {
	sam, ana := "5a3e0c1e-0000-4000-8000-000000000001", "1b2c3d4e-0000-4000-8000-000000000002"
	for _, c := range []struct {
		in   CollabSave
		want string
	}{
		{CollabSave{Contributors: []string{sam}}, sam},
		{CollabSave{Agents: []AgentContributor{{AgentUUID: "captain", AskerUUID: ana}}}, ana},
		{CollabSave{Contributors: []string{sam, ana}}, ana},
		{CollabSave{Contributors: []string{"unknown"}, Agents: []AgentContributor{{AgentUUID: "captain", AskerUUID: "not-a-uuid"}}}, ""},
		{CollabSave{}, ""},
	} {
		if got := lastEditor(c.in); got != c.want {
			t.Errorf("%+v: %q, want %q", c.in, got, c.want)
		}
	}
	w := newSaveWorld(t)
	w.stored[saveDoc] = "an-older-hash"
	if _, err := SaveFromCollab(context.Background(), CollabSave{DocUUID: saveDoc, HTML: "<p>x</p>", YjsState: "eA==", Contributors: []string{sam}}); err != nil {
		t.Fatal(err)
	}
	if w.writes[0].EditorUUID != sam {
		t.Errorf("the write credits %q", w.writes[0].EditorUUID)
	}
}

// A save with nobody's edit in it (the service's own conversion as the doc
// loaded) is stored as a conversion, which leaves the doc's edited time; a
// person's or an agent's edit is not (QA DSA-08).
func TestASaveWithNobodysEditIsAConversion(t *testing.T) {
	sam := "5a3e0c1e-0000-4000-8000-000000000001"
	for _, c := range []struct {
		name       string
		in         CollabSave
		conversion bool
	}{
		{"the service's block ids", CollabSave{}, true},
		{"a person's edit", CollabSave{Contributors: []string{sam}}, false},
		{"an agent's edit", CollabSave{Agents: []AgentContributor{{AgentUUID: "captain", AskerUUID: sam}}}, false},
	} {
		w := newSaveWorld(t)
		w.stored[saveDoc] = "an-older-hash"
		in := c.in
		in.DocUUID, in.HTML, in.YjsState = saveDoc, "<p data-block-id=\"b1\">Ship</p>", "eA=="
		got, err := SaveFromCollab(context.Background(), in)
		if err != nil || !got.Changed {
			t.Fatalf("%s: %+v %v", c.name, got, err)
		}
		if w.writes[0].Conversion != c.conversion {
			t.Errorf("%s: stored as a conversion %v, want %v", c.name, w.writes[0].Conversion, c.conversion)
		}
	}
}
