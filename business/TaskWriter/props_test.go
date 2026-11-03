package business

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// A property's key is kept one way, whatever case it came in; a key that
// names no property is "".
func TestAPropertyIsKeptOneWay(t *testing.T) {
	field := uuid.New()
	for in, want := range map[string]string{
		" Priority ": PropPriority,
		"DUE":        PropDue,
		"assignee":   PropAssignee,
		"field:" + strings.ToUpper(field.String()): PropField + field.String(),
		"field:size": "",
		"mood":       "",
		"":           "",
	} {
		if got := PropOf(in); got != want {
			t.Errorf("PropOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// Missing names every required property the task leaves empty, in the order
// asked, and none that it fills.
func TestMissingNamesEveryRequiredPropertyLeftEmpty(t *testing.T) {
	size, gone, cleared := uuid.New(), uuid.New(), uuid.New()
	due, bug := time.Now(), uuid.New()
	full := NewTask{
		Name: "Checkout crashes", Description: "<p>Steps: open the cart</p>", Priority: "high", Label: "bug",
		Due: &due, Start: &due, Type: &bug, Assignee: &dgraphStruct.DgraphUser{Uuid: uuid.NewString()},
		Fields: map[uuid.UUID]json.RawMessage{size: json.RawMessage(`"L"`), cleared: json.RawMessage(`null`)},
	}
	every := []string{PropDescription, PropPriority, PropDue, PropStart, PropTags, PropAssignee, PropType, PropField + size.String()}
	if got := Missing(every, full); len(got) != 0 {
		t.Errorf("a task with every property filled misses %v", got)
	}
	empty := NewTask{Name: "Checkout crashes", Description: "<p> </p><p><br></p>"}
	if got := Missing(every, empty); !slices.Equal(got, every) {
		t.Errorf("an empty task misses %v, want %v", got, every)
	}
	asked := []string{PropField + gone.String(), PropField + cleared.String(), PropPriority}
	if got, want := Missing(asked, full), asked[:2]; !slices.Equal(got, want) {
		t.Errorf("a field it has no value of, or a null one, is missing: got %v, want %v", got, want)
	}
}
