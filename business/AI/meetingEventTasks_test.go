package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// actionTaskWorld is one event's call with two action items, and everything
// Make task reads and writes, recorded.
type actionTaskWorld struct {
	keys     calendarBusiness.CallKeys
	refuse   error // the calendar's change rule's answer
	call     *recordingDomain.CallArtefact
	project  error // the project's answer
	made     []ProposedTask
	madeIn   []uuid.UUID
	noted    map[int]uuid.UUID
	linked   []string
	linkErr  error
	claimed  bool
	released int
}

func newActionTaskWorld(t *testing.T) *actionTaskWorld {
	t.Helper()
	w := &actionTaskWorld{
		keys: calendarBusiness.CallKeys{Events: []uuid.UUID{uuid.New()}},
		call: &recordingDomain.CallArtefact{Key: "call:s1", ActionItems: []recordingDomain.ActionItem{
			{Text: "Send the deck to legal", Owner: "Maya", Due: "2026-11-14"},
			{Text: "Book the room"},
		}},
		noted: map[int]uuid.UUID{},
	}
	project := uuid.New()
	old := []any{eventKeysToChange, eventCallOf, projectForTask, makeActionTask, noteActionTask, linkTaskToEvent, claimActionItem, releaseActionItem}
	t.Cleanup(func() {
		eventKeysToChange = old[0].(func(context.Context, *userModels.UserInfo, string) (calendarBusiness.CallKeys, error))
		eventCallOf = old[1].(func(context.Context, calendarBusiness.CallKeys, string) (*recordingDomain.CallArtefact, error))
		projectForTask = old[2].(func(context.Context, *userModels.UserInfo, string) (uuid.UUID, *dgraphStruct.DgraphProject, error))
		makeActionTask = old[3].(func(context.Context, *userModels.UserInfo, uuid.UUID, *dgraphStruct.DgraphProject, ProposedTask, string) (uuid.UUID, error))
		noteActionTask = old[4].(func(context.Context, string, int, uuid.UUID) error)
		linkTaskToEvent = old[5].(func(context.Context, *userModels.UserInfo, string, string) error)
		claimActionItem = old[6].(func(context.Context, string, int, string) (bool, error))
		releaseActionItem = old[7].(func(context.Context, string, int, string))
	})
	eventKeysToChange = func(context.Context, *userModels.UserInfo, string) (calendarBusiness.CallKeys, error) {
		return w.keys, w.refuse
	}
	eventCallOf = func(_ context.Context, keys calendarBusiness.CallKeys, key string) (*recordingDomain.CallArtefact, error) {
		if w.call == nil || key != w.call.Key || len(keys.Events) == 0 {
			return nil, nil
		}
		c := *w.call
		c.ActionItems = append([]recordingDomain.ActionItem(nil), w.call.ActionItems...)
		for i, task := range w.noted {
			c.ActionItems[i].Task = task.String()
		}
		return &c, nil
	}
	projectForTask = func(context.Context, *userModels.UserInfo, string) (uuid.UUID, *dgraphStruct.DgraphProject, error) {
		if w.project != nil {
			return uuid.Nil, nil, w.project
		}
		return project, &dgraphStruct.DgraphProject{Uuid: project.String()}, nil
	}
	makeActionTask = func(_ context.Context, _ *userModels.UserInfo, in uuid.UUID, _ *dgraphStruct.DgraphProject, t ProposedTask, _ string) (uuid.UUID, error) {
		w.made, w.madeIn = append(w.made, t), append(w.madeIn, in)
		return uuid.New(), nil
	}
	noteActionTask = func(_ context.Context, _ string, i int, task uuid.UUID) error {
		w.noted[i] = task
		return nil
	}
	linkTaskToEvent = func(_ context.Context, _ *userModels.UserInfo, id, task string) error {
		w.linked = append(w.linked, id+"|"+task)
		return w.linkErr
	}
	claimActionItem = func(context.Context, string, int, string) (bool, error) {
		if w.claimed {
			return false, nil
		}
		w.claimed = true
		return true, nil
	}
	releaseActionItem = func(context.Context, string, int, string) { w.claimed = false; w.released++ }
	return w
}

var maya = &userModels.UserInfo{}

// Make task makes the item's task through the task path (its owner and due
// date with it) in the project asked for, notes it on the item and puts it on
// the event through the event's own files (591's task link): one task, once.
func TestMakeTaskMakesTheItemsTaskAndPutsItOnTheEvent(t *testing.T) {
	w := newActionTaskWorld(t)
	// The calendar's Make task is this package's (its init).
	got, err := calendarBusiness.MakeActionTask(context.Background(), maya, "ev-1", "call:s1", 0, uuid.NewString())
	if err != nil || got == nil || !got.Made || !got.Linked {
		t.Fatalf("MakeActionTask = %+v, %v", got, err)
	}
	if len(w.made) != 1 || w.made[0].Title != "Send the deck to legal" || w.made[0].AssigneeName != "Maya" || w.made[0].Due != "2026-11-14T17:00:00Z" {
		t.Errorf("made %+v, want the item's words, owner and due date", w.made)
	}
	if w.noted[0].String() != got.TaskUUID {
		t.Errorf("noted %v on the item, want %s", w.noted, got.TaskUUID)
	}
	if len(w.linked) != 1 || w.linked[0] != "ev-1|"+got.TaskUUID {
		t.Errorf("linked %v, want the task on the event", w.linked)
	}
	if w.claimed || w.released != 1 {
		t.Errorf("the claim was left held (%v) or let go %d times", w.claimed, w.released)
	}

	// Again: the item's task, nothing new made or linked.
	again, err := MakeEventActionTask(context.Background(), maya, "ev-1", "call:s1", 0, uuid.NewString())
	if err != nil || again == nil || again.Made || again.TaskUUID != got.TaskUUID || len(w.made) != 1 || len(w.linked) != 1 {
		t.Errorf("again = %+v, %v; made %d, linked %d; want the same task and nothing new", again, err, len(w.made), len(w.linked))
	}
}

// A guest who may only read the event, and anyone the event isn't theirs to
// change, is refused by the calendar's change rule before anything is made.
func TestMakeTaskIsForWhoeverMayChangeTheEvent(t *testing.T) {
	for _, refusal := range []error{calendarBusiness.ErrOnlyCreatorChanges, calendarBusiness.ErrEventNotFound} {
		w := newActionTaskWorld(t)
		w.refuse = refusal
		got, err := MakeEventActionTask(context.Background(), maya, "ev-1", "call:s1", 0, uuid.NewString())
		if !errors.Is(err, refusal) || got != nil || len(w.made) != 0 || len(w.linked) != 0 {
			t.Errorf("refused %v: %+v, %v; made %d", refusal, got, err, len(w.made))
		}
	}
}

// A project the person may not add tasks to makes nothing; nor does an item
// or a call the event doesn't have; nor a second Make task while one is
// being made.
func TestMakeTaskMakesNothingItMayNot(t *testing.T) {
	w := newActionTaskWorld(t)
	w.project = &ProjectRefusal{msg: "project 'Payroll': you can't add tasks"}
	if _, err := MakeEventActionTask(context.Background(), maya, "ev-1", "call:s1", 1, uuid.NewString()); !errors.Is(err, calendarBusiness.ErrCantAddTasks) ||
		!strings.Contains(err.Error(), "Payroll") || len(w.made) != 0 {
		t.Errorf("a project they can't add to: %v, made %d", err, len(w.made))
	}
	w = newActionTaskWorld(t)
	for _, c := range []struct {
		key string
		i   int
	}{{"call:s1", 2}, {"call:s1", -1}, {"call:other", 0}} {
		if _, err := MakeEventActionTask(context.Background(), maya, "ev-1", c.key, c.i, uuid.NewString()); !errors.Is(err, calendarBusiness.ErrNoActionItem) {
			t.Errorf("%s item %d: %v, want ErrNoActionItem", c.key, c.i, err)
		}
	}
	w.claimed = true
	if _, err := MakeEventActionTask(context.Background(), maya, "ev-1", "call:s1", 0, uuid.NewString()); !errors.Is(err, calendarBusiness.ErrActionItemBusy) || len(w.made) != 0 {
		t.Errorf("while one is being made: %v, made %d", err, len(w.made))
	}
}

// An event that can't take the task (it carries as many files as it may)
// still has the task made and noted on the item, said as not linked.
func TestMakeTaskKeepsTheTaskWhenTheEventCantTakeIt(t *testing.T) {
	w := newActionTaskWorld(t)
	w.linkErr = errors.New("An event carries up to 25 files.")
	got, err := MakeEventActionTask(context.Background(), maya, "ev-1", "call:s1", 1, uuid.NewString())
	if err != nil || got == nil || !got.Made || got.Linked || w.noted[1].String() != got.TaskUUID {
		t.Errorf("MakeEventActionTask = %+v, %v; noted %v", got, err, w.noted)
	}
}

// A task someone else made from the item between this maker's first read and
// its claim is the item's: the read under the claim finds it, and nothing new
// is made or put on the event.
func TestMakeTaskFindsATaskMadeBeforeItsClaim(t *testing.T) {
	w := newActionTaskWorld(t)
	theirs := uuid.New()
	claim := claimActionItem
	claimActionItem = func(ctx context.Context, key string, i int, token string) (bool, error) {
		w.noted[i] = theirs // a slower maker's task, noted once it let the claim go
		return claim(ctx, key, i, token)
	}
	got, err := MakeEventActionTask(context.Background(), maya, "ev-1", "call:s1", 0, uuid.NewString())
	if err != nil || got == nil || got.Made || got.TaskUUID != theirs.String() {
		t.Fatalf("MakeEventActionTask = %+v, %v; want the task made before the claim", got, err)
	}
	if len(w.made) != 0 || len(w.linked) != 0 || w.released != 1 {
		t.Errorf("made %d, linked %d, the claim let go %d times; want nothing new and the claim let go once", len(w.made), len(w.linked), w.released)
	}
}
