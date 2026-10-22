package business

import (
	"os"
	"strings"
	"testing"
)

// The board's safety net (whiteboard W-0b): a version from before a sharp
// shrink, kept so it can be restored. It watched the state's size alone. An
// undo, a select-all delete or a teammate's eraser only marks elements
// deleted, so the size barely moves and nothing was kept; ten minutes later
// the collaboration service prunes the deleted elements, the size drops, and
// the version kept then was the board already emptied. testdata holds the two
// saves of W-0's undo bug: Ann's note, its words, her rectangle and Sam's,
// then all four taken back (4 and 0 elements a person sees).

func testState(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestABoardEmptiedByAnUndoIsKeptAsItWas(t *testing.T) {
	before, after := testState(t, "undo-before.b64"), testState(t, "undo-after.b64")
	if !massDeletion(saveOf(before, "4 elements"), saveOf(after, "0 elements")) {
		t.Fatalf("an undo that took every element back (%d bytes to %d) kept no version of the board as it was", len(before), len(after))
	}
}

func TestPruningDeletedElementsKeepsNoVersionOfAnEmptiedBoard(t *testing.T) {
	// Ten minutes on: the deleted elements are pruned, the state shrinks,
	// and nothing a person sees changed. The board before it was already
	// empty, so it isn't a version worth keeping.
	if massDeletion(boardSave{bytes: 50_000, elements: 0, counted: true}, boardSave{bytes: 4_000, elements: 0, counted: true}) {
		t.Fatal("pruning tombstones from an emptied board kept it as a version")
	}
	if massDeletion(boardSave{bytes: 50_000, elements: 120, counted: true}, boardSave{bytes: 15_000, elements: 120, counted: true}) {
		t.Fatal("pruning tombstones, with all 120 elements still there, kept a version")
	}
}

func TestASharpShrinkStillCountsWhenTheCountIsUnknown(t *testing.T) {
	// A save from before the service counted what a person sees, or a
	// snippet that says nothing: the size alone decides, as it always has.
	if !massDeletion(saveOf(strings.Repeat("x", 50_000), ""), saveOf(strings.Repeat("x", 15_000), "3 elements")) {
		t.Fatal("a sharp shrink with no count before it kept nothing")
	}
}

func TestSmallChangesAreNotMassDeletions(t *testing.T) {
	for _, c := range []struct {
		prev, next boardSave
	}{
		{boardSave{bytes: 9_000, elements: 3, counted: true}, boardSave{bytes: 9_000, elements: 0, counted: true}},    // too few to call it a mass deletion
		{boardSave{bytes: 9_000, elements: 10, counted: true}, boardSave{bytes: 9_100, elements: 5, counted: true}},   // half: an ordinary tidy-up
		{boardSave{bytes: 9_000, elements: 10, counted: true}, boardSave{bytes: 12_000, elements: 14, counted: true}}, // growth
		{boardSave{bytes: 1_000, elements: 0, counted: false}, boardSave{bytes: 100, elements: 0, counted: false}},    // a tiny board's size
	} {
		if massDeletion(c.prev, c.next) {
			t.Fatalf("%+v to %+v counted as a mass deletion", c.prev, c.next)
		}
	}
}

func TestASnippetSaysHowManyElementsAPersonSees(t *testing.T) {
	for snippet, want := range map[string]boardSave{
		"42 elements": {bytes: 3, elements: 42, counted: true},
		"0 elements":  {bytes: 3, elements: 0, counted: true},
		"":            {bytes: 3},
		"elements":    {bytes: 3},
	} {
		if got := saveOf("abc", snippet); got != want {
			t.Fatalf("saveOf(%q) = %+v, want %+v", snippet, got, want)
		}
	}
}
