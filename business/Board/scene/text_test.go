package scene

import (
	"encoding/base64"
	"reflect"
	"testing"
)

// What a board says, as search and the AI read it (W6-B, miro-05): its frames'
// names, and the words of its notes, shapes, arrows and free text, read from a
// whole saved state as the collaboration service saves one.
func TestTextItemsReadWhatABoardSays(t *testing.T) {
	ice, ok := NoteColourByID("ice")
	if !ok {
		t.Fatal("no ice note colour")
	}
	els := []Element{Frame("f1", "Risks", 0, 0, 800, 600, 1)}
	els = append(els, Note{ID: "n1", X: 10, Y: 20, W: 200, H: 228, Colour: ice, Text: "Book the load test a week earlier", FrameID: "f1"}.Elements(2)...)
	box := Base("r1", "rectangle", 900, 0, 200, 100, 3)
	els = append(els, box, Label("r1-label", box, "Payments", TextStyle{}, 8, 3))
	els = append(els, Text("t1", 0, 700, 300, "  Owner:\n\n Priya ", TextStyle{}, 4))
	gone := Text("t2", 0, 800, 300, "Words someone deleted", TextStyle{}, 5)
	gone["isDeleted"] = true
	els = append(els, gone, Frame("f2", "", 0, 900, 100, 100, 6))
	goneBox := Base("g1", "rectangle", 0, 1000, 100, 100, 7)
	goneBox["isDeleted"] = true
	els = append(els, goneBox, Label("g1-label", goneBox, "Words on a deleted shape", TextStyle{}, 8, 7))
	arrow := Base("a1", "arrow", 300, 300, 100, 0, 8)
	els = append(els, arrow, Label("a1-label", arrow, "blocks", TextStyle{}, 8, 8))
	Index(els)

	state, _, err := State(1, els, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(state)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := Maps(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := TextItems(roots["elements"])
	want := []TextItem{
		{ID: "f1", Kind: "frame", Frame: "f1", Text: "Risks"},
		{ID: "n1", Kind: "note", Frame: "f1", Text: "Book the load test a week earlier"},
		{ID: "r1", Kind: "shape", Text: "Payments"},
		{ID: "t1", Kind: "text", Text: "Owner: Priya"},
		{ID: "a1", Kind: "arrow", Text: "blocks"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("what the board says:\n got %+v\nwant %+v", got, want)
	}
	if TextItems(nil) != nil {
		t.Fatal("an empty board says something")
	}
}

// The outline is what the board's search entry and its AI entry hold: each
// frame's name over its words, then the words in no frame, cut at a whole item.
func TestOutlinePutsEachFramesWordsUnderItsName(t *testing.T) {
	items := []TextItem{
		{ID: "f1", Kind: "frame", Frame: "f1", Text: "Risks"},
		{ID: "n1", Kind: "note", Frame: "f1", Text: "Book the load test"},
		{ID: "n2", Kind: "note", Frame: "f1", Text: "Hire QA"},
		{ID: "t1", Kind: "text", Text: "Owner: Priya"},
	}
	if got, want := Outline(items, 0), "Risks\nBook the load test\nHire QA\n\nOwner: Priya"; got != want {
		t.Fatalf("outline %q, want %q", got, want)
	}
	if got, want := Outline(items, 30), "Risks\nBook the load test"; got != want {
		t.Fatalf("cut at 30: %q, want %q", got, want)
	}
	if got, want := Outline([]TextItem{{ID: "t", Kind: "text", Text: "abcdefghij"}}, 4), "abcd"; got != want {
		t.Fatalf("one item longer than the cap: %q, want %q", got, want)
	}
	if Outline(nil, 0) != "" {
		t.Fatal("an empty board has an outline")
	}
}
