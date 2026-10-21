package business

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/TaskStatus"
	"github.com/google/uuid"
)

func custom(name, category string, position int) *model.TaskStatus {
	return &model.TaskStatus{ID: uuid.New(), Name: name, Category: category, Position: position}
}

// A project with no order of its own keeps today's: each built-in status
// followed by its own, in the order they were given.
func TestTheDefaultOrderIsEachBuiltInFollowedByItsOwn(t *testing.T) {
	qa, blocked, shipped := custom("QA", "inReview", 2), custom("Blocked", "inProgress", 1), custom("Shipped", "done", 3)
	got := DefaultOrder([]*model.TaskStatus{qa, blocked, shipped})
	want := []string{"backlog", "todo", "inProgress", blocked.ID.String(), "inReview", qa.ID.String(), "done", shipped.ID.String(), "canceled"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default order\n got %v\nwant %v", got, want)
	}
}

func TestTheOrderShownIsTheSavedOneKeptWhole(t *testing.T) {
	qa, blocked, shipped := custom("QA", "inReview", 1), custom("Blocked", "inProgress", 2), custom("Shipped", "done", 3)
	customs := []*model.TaskStatus{qa, blocked, shipped}
	saved := []string{"todo", "inReview", qa.ID.String(), "inProgress", "backlog", blocked.ID.String(), "canceled", shipped.ID.String(), "done"}
	if got := EffectiveOrder(saved, customs); !reflect.DeepEqual(got, saved) {
		t.Fatalf("saved order kept\n got %v\nwant %v", got, saved)
	}

	// A status made since goes after the one before it in the default order:
	// Ready (In review) after QA, the last of In review's own.
	ready := custom("Ready", "inReview", 9)
	withReady := append(customs, ready)
	got := EffectiveOrder(saved, withReady)
	want := []string{"todo", "inReview", qa.ID.String(), ready.ID.String(), "inProgress", "backlog", blocked.ID.String(), "canceled", shipped.ID.String(), "done"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a new status\n got %v\nwant %v", got, want)
	}

	// A deleted status leaves it, and an id in another case is the same.
	upper := append([]string{strings.ToUpper(qa.ID.String())}, saved[:1]...)
	got = EffectiveOrder(append(upper, saved[2:]...), []*model.TaskStatus{qa, shipped})
	for _, v := range got {
		if v == blocked.ID.String() {
			t.Fatalf("a deleted status stayed: %v", got)
		}
	}
	if got[0] != qa.ID.String() {
		t.Fatalf("an upper-case id: %v", got)
	}

	// A status that now counts as done moves after the open ones, keeping its
	// place among the closed.
	early := []string{"todo", blocked.ID.String(), "inReview", qa.ID.String(), "inProgress", "backlog", "canceled", shipped.ID.String(), "done"}
	blocked.Category = "done"
	got = EffectiveOrder(early, customs)
	want = []string{"todo", "inReview", qa.ID.String(), "inProgress", "backlog", blocked.ID.String(), "canceled", shipped.ID.String(), "done"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a status gone closed\n got %v\nwant %v", got, want)
	}
	blocked.Category = "inProgress"

	// An order that names only some, closed first: the rest go by the
	// default order, and the closed after the open.
	got = EffectiveOrder([]string{"done", "todo"}, nil)
	if want := []string{"backlog", "todo", "inProgress", "inReview", "done", "canceled"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a partial order\n got %v\nwant %v", got, want)
	}
}

func TestAnOrderNamesEveryStatusOnceWithClosedOnesLast(t *testing.T) {
	qa := custom("QA", "inReview", 1)
	customs := []*model.TaskStatus{qa}
	good := []string{"inReview", qa.ID.String(), "todo", "backlog", "inProgress", "canceled", "done"}
	norm, err := CheckOrder(append([]string{}, good...), customs)
	if err != nil || !reflect.DeepEqual(norm, good) {
		t.Fatalf("a good order: %v %v", norm, err)
	}
	if norm, err := CheckOrder([]string{"inReview", strings.ToUpper(qa.ID.String()), "todo", "backlog", "inProgress", "canceled", "done"}, customs); err != nil || norm[1] != qa.ID.String() {
		t.Fatalf("an upper-case id is kept in its usual form: %v %v", norm, err)
	}
	for name, order := range map[string][]string{
		"one missing":             {"inReview", "todo", "backlog", "inProgress", "canceled", "done"},
		"one twice":               {"inReview", qa.ID.String(), "todo", "todo", "backlog", "inProgress", "canceled", "done"},
		"one not the project's":   {"inReview", uuid.NewString(), "todo", "backlog", "inProgress", "canceled", "done"},
		"not a status":            {"inReview", qa.ID.String(), "todo", "backlog", "inProgress", "canceled", "Done!"},
		"Done before an open one": {"inReview", qa.ID.String(), "todo", "done", "backlog", "inProgress", "canceled"},
	} {
		if _, err := CheckOrder(order, customs); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := CheckOrder([]string{"todo", "backlog", "inProgress", "inReview", "done", qa.ID.String(), "canceled"}, customs); !errors.Is(err, ErrClosedFirst) {
		t.Errorf("an open status of its own after Done: %v", err)
	}
}

func TestReorderingSomeKeepsTheRestInPlace(t *testing.T) {
	got := InPlaces([]string{"a", "b", "c", "d", "e"}, []string{"d", "b", "x"})
	if want := []string{"a", "d", "c", "b", "e"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("in places: %v, want %v", got, want)
	}
}

func TestSettingAnOrderSavesItAsChecked(t *testing.T) {
	qa := custom("QA", "inReview", 1)
	oldC, oldS := customsOf, saveOrder
	t.Cleanup(func() { customsOf, saveOrder = oldC, oldS })
	customsOf = func(context.Context, uuid.UUID) ([]*model.TaskStatus, error) { return []*model.TaskStatus{qa}, nil }
	var saved []string
	var by uuid.UUID
	saveOrder = func(_ context.Context, _ uuid.UUID, order []string, who uuid.UUID) error {
		saved, by = order, who
		return nil
	}
	admin := uuid.New()
	order := []string{"todo", "inProgress", strings.ToUpper(qa.ID.String()), "inReview", "backlog", "done", "canceled"}
	got, err := SetOrder(context.Background(), uuid.New(), order, admin)
	if err != nil || !reflect.DeepEqual(got, saved) || saved[2] != qa.ID.String() || by != admin {
		t.Fatalf("saved %v by %s, returned %v, %v", saved, by, got, err)
	}
	saved = nil
	if _, err := SetOrder(context.Background(), uuid.New(), []string{"todo"}, admin); !errors.Is(err, ErrInvalid) || saved != nil {
		t.Fatalf("a short order: %v, saved %v", err, saved)
	}
}
