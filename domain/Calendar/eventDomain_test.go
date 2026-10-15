package Calendar

import (
	"encoding/json"
	"reflect"
	"testing"
)

// CAL-BUG-1 and 17: what an update takes away is deleted in the same request,
// since a set mutation only adds guests and never writes empty notes.
func TestAnUpdateDeletesWhatItTakesAway(t *testing.T) {
	if got := eventDeletions("0x1", nil, false); got != "" {
		t.Fatalf("nothing taken away, yet a delete: %s", got)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(eventDeletions("0x1", []string{"0x2", "0x3"}, true)), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"uid":                "0x1",
		"event_participants": []any{map[string]any{"uid": "0x2"}, map[string]any{"uid": "0x3"}},
		"event_description":  nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delete %v, want %v", got, want)
	}
	if _, ok := got["event_description"]; !ok {
		t.Fatal("cleared notes must be deleted with a null, which deletes every value")
	}
}
