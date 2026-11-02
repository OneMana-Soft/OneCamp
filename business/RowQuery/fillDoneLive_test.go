package business

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	computedStore "github.com/akashc777/OneCamp/business/ComputedStore"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// said is one message a fill's end sent on a table's live topic.
type said struct {
	table string
	data  interface{}
}

// hearFillsEnd stands in for the store's mark (it answers marked, or fails)
// and for the tables' live topics, and returns what was said on them.
func hearFillsEnd(t *testing.T, marked []uuid.UUID, fails error) *[]said {
	t.Helper()
	savedMark, savedPublish := markComputedFilled, publishTableRow
	t.Cleanup(func() { markComputedFilled, publishTableRow = savedMark, savedPublish })
	markComputedFilled = func(context.Context, []model.ComputedFill, int64) ([]uuid.UUID, error) { return marked, fails }
	heard := &[]said{}
	publishTableRow = func(table string, data interface{}) { *heard = append(*heard, said{table, data}) }
	return heard
}

// V-124: a fill's end says so once, on its table's live topic (the fan-out
// every table change uses, which business/MqttAccess opens to the table's
// readers alone), naming the fields it marked filled and no other, so a view
// the server held for the fill reads again at once, not at its next wait.
func TestAFillsEndSaysOnceOnItsTablesTopicWhichFieldsItFilled(t *testing.T) {
	table, a, b, movedOn := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	heard := hearFillsEnd(t, []uuid.UUID{b, a}, nil)
	k := &keeping{table: table, gens: map[uuid.UUID]int64{a: 1, b: 2, movedOn: 3}, mark: 7, w: &computedStore.Worked{}}
	if err := k.done(context.Background()); err != nil {
		t.Fatalf("a fill's end: %v", err)
	}
	if len(*heard) != 1 {
		t.Fatalf("a fill's end said %d messages on its table's topic, where one is", len(*heard))
	}
	got := (*heard)[0]
	if got.table != table.String() {
		t.Errorf("said on table %s's topic, where its own, %s, is", got.table, table)
	}
	want := []string{a.String(), b.String()}
	sort.Strings(want)
	msg, _ := got.data.(map[string]interface{})
	if len(msg) != 2 || msg["action"] != "computed" || !reflect.DeepEqual(msg["fields"], want) {
		t.Errorf("said %v, where {action: computed, fields: %v} is: the fields it marked filled, in order, ids only", got.data, want)
	}
}

// A fill that marked nothing (each field's generation moved on meanwhile: the
// sweep's next fill says it) says nothing, and one whose mark failed says
// nothing and hands the error back.
func TestAFillThatMarkedNothingOrFailedToMarkSaysNothing(t *testing.T) {
	f := uuid.New()
	for _, c := range []struct {
		name   string
		marked []uuid.UUID
		fails  error
	}{
		{"marked nothing", nil, nil},
		{"failed to mark", []uuid.UUID{f}, errors.New("canceling statement due to statement timeout")},
	} {
		heard := hearFillsEnd(t, c.marked, c.fails)
		k := &keeping{table: uuid.New(), gens: map[uuid.UUID]int64{f: 1}, mark: 1, w: &computedStore.Worked{}}
		if err := k.done(context.Background()); !errors.Is(err, c.fails) {
			t.Errorf("%s: done answered %v, where %v is", c.name, err, c.fails)
		}
		if len(*heard) != 0 {
			t.Errorf("%s: said %v on the table's topic, where nothing is", c.name, *heard)
		}
	}
}
