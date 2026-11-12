package scene

import (
	"reflect"
	"testing"
)

// A board's frames, by id: true for one on the canvas, false for one deleted
// there and still in the document (undo can bring it back); an id that isn't
// there is a frame gone from the document, or never one.
func TestFramesAreTheBoardsFramesLiveOrDeleted(t *testing.T) {
	elements := map[string]any{
		"f1": map[string]any{"id": "f1", "type": "frame"},
		"m1": map[string]any{"id": "m1", "type": "magicframe"},
		"f2": map[string]any{"id": "f2", "type": "frame", "isDeleted": true},
		"r1": map[string]any{"id": "r1", "type": "rectangle"},
		"x":  "not an element",
	}
	want := map[string]bool{"f1": true, "m1": true, "f2": false}
	if got := Frames(elements); !reflect.DeepEqual(got, want) {
		t.Fatalf("Frames = %v, want %v", got, want)
	}
}
