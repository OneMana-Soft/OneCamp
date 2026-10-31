package business

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// ?ids= is row ids, each once, in order; anything else is refused.
func TestRowIDsAreParsed(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	got, err := ParseRowIDs(" " + a.String() + "," + b.String() + ",," + a.String())
	if err != nil || !reflect.DeepEqual(got, []uuid.UUID{a, b}) {
		t.Errorf("got %v, %v", got, err)
	}
	many := make([]string, MaxRowsByID+1)
	for i := range many {
		many[i] = uuid.NewString()
	}
	for _, bad := range []string{"", " , ", a.String() + ",row-7", strings.Join(many, ",")} {
		if _, err := ParseRowIDs(bad); !errors.Is(err, ErrBadRowIDs) {
			t.Errorf("%.40q: %v", bad, err)
		}
	}
}

// Rows come back in the order named, without those that aren't there.
func TestRowsByIDKeepTheOrderNamed(t *testing.T) {
	a, b, c := &model.Row{Id: uuid.New()}, &model.Row{Id: uuid.New()}, uuid.New()
	got := inOrder([]*model.Row{a, b}, []uuid.UUID{b.Id, c, a.Id})
	if len(got) != 2 || got[0] != b || got[1] != a {
		t.Errorf("got %v", got)
	}
}

// A live message names each changed row's fields, who changed them and when,
// and never a value.
func TestALiveMessageNamesEachRowsFields(t *testing.T) {
	row, made1, who := uuid.New(), uuid.New(), uuid.New()
	at := time.Date(2026, 10, 19, 12, 0, 0, 5, time.UTC)
	cs := &model.ChangeSet{ID: uuid.New(), Actor: who, At: at, Changes: []model.Change{
		cellChange(row, "f1", `"secret"`, `"new"`), cellChange(row, "f2", `1`, `2`), made(made1)}}
	msg := liveChanged(cs)
	if !reflect.DeepEqual(msg["fields"], map[string][]string{row.String(): {"f1", "f2"}}) {
		t.Errorf("fields: %v", msg["fields"])
	}
	if msg["by"] != who.String() || msg["at"] != "2026-10-19T12:00:00.000000005Z" {
		t.Errorf("by %v at %v", msg["by"], msg["at"])
	}
	for _, v := range []string{"secret", "new"} {
		if strings.Contains(fmt.Sprint(msg), v) {
			t.Errorf("the message carries a value: %v", msg)
		}
	}
}
