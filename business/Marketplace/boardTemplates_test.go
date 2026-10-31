package business

import (
	"encoding/json"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/Marketplace"
)

// A board saved as a workspace template (the whiteboard's W-1, miro-19): its
// elements, checked as they're published so an install never fails on them.

func boardPayload(t *testing.T, elements ...map[string]any) string {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(elements))
	for _, e := range elements {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, b)
	}
	b, err := json.Marshal(BoardPayload{Title: " Sprint retro ", Elements: raw})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestABoardTemplateIsItsElementsAndATitle(t *testing.T) {
	p, err := readBoardPayload(boardPayload(t, map[string]any{"id": "n1", "type": "rectangle"}, map[string]any{"id": "t1", "type": "text", "containerId": "n1"}))
	if err != nil || p.Title != "Sprint retro" || len(p.Elements) != 2 {
		t.Fatalf("%+v, %v", p, err)
	}
	if !validKind(KindBoard) {
		t.Fatal("board isn't a template kind")
	}
	if _, err := decodeForKind(KindBoard, json.RawMessage(boardPayload(t, map[string]any{"id": "n1", "type": "rectangle"}))); err != nil {
		t.Fatalf("a board template didn't decode for its kind: %v", err)
	}
}

func TestABoardTemplateRefusesWhatCantComeWithIt(t *testing.T) {
	for name, payload := range map[string]string{
		"a picture":        boardPayload(t, map[string]any{"id": "i1", "type": "image"}),
		"nothing on it":    boardPayload(t),
		"an element no id": boardPayload(t, map[string]any{"type": "rectangle"}),
		"not a template":   `["x"]`,
	} {
		if _, err := readBoardPayload(payload); err == nil {
			t.Errorf("%s was published", name)
		}
	}
	many := make([]map[string]any, maxBoardTemplateElements+1)
	for i := range many {
		many[i] = map[string]any{"id": strings.Repeat("x", 3) + string(rune('a'+i%26)), "type": "rectangle"}
	}
	if _, err := readBoardPayload(boardPayload(t, many...)); err == nil {
		t.Errorf("a template of %d elements was published", len(many))
	}
}

// A board template as the Templates page and the board's picker list it: a
// small drawing of it, never its elements (a board template is up to a
// megabyte; a list of them was every board's every element).
func TestAListedBoardTemplateIsADrawingOfItNotItsElements(t *testing.T) {
	listed := asListed(&model.Template{Kind: KindBoard, Name: "Sprint retro", Payload: boardPayload(t,
		map[string]any{"id": "f1", "type": "frame", "x": -20.4, "y": -20.6, "width": 640, "height": 300, "name": "Went well"},
		map[string]any{"id": "n1", "type": "rectangle", "x": 0, "y": 0, "width": 180, "height": 180, "backgroundColor": "#FFF6D6", "customData": map[string]any{"kind": "note", "colour": "yellow"}},
		map[string]any{"id": "t1", "type": "text", "x": 20, "y": 80, "width": 140, "height": 22, "containerId": "n1", "text": "Shipped the pricing page"},
		map[string]any{"id": "h1", "type": "text", "x": 0, "y": -80, "width": 220, "height": 35, "text": "Retro"},
		map[string]any{"id": "a1", "type": "arrow", "x": 200, "y": 90, "width": 120, "height": 40, "points": [][]float64{{0, 0}, {120, -40}}},
		map[string]any{"id": "e1", "type": "ellipse", "x": 340, "y": 0, "width": 120, "height": 80, "backgroundColor": "transparent"},
		map[string]any{"id": "gone", "type": "rectangle", "x": 0, "y": 0, "width": 10, "height": 10, "isDeleted": true},
	)})
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(listed.Payload), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["elements"]; ok {
		t.Fatalf("a listed board template carries its elements: %s", listed.Payload)
	}
	var p BoardPayload
	if err := json.Unmarshal([]byte(listed.Payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.Title != "Sprint retro" || p.Count != 6 {
		t.Fatalf("title %q, count %d; want the title and the 6 elements a person sees", p.Title, p.Count)
	}
	want := []BoardShape{
		{Type: "frame", X: -20, Y: -21, W: 640, H: 300},
		{Type: "rectangle", X: 0, Y: 0, W: 180, H: 180, Fill: "#FFF6D6", Note: true},
		{Type: "text", X: 0, Y: -80, W: 220, H: 35},
		{Type: "arrow", X: 200, Y: 90, W: 120, H: -40},
		{Type: "ellipse", X: 340, Y: 0, W: 120, H: 80},
	}
	if len(p.Preview) != len(want) {
		t.Fatalf("preview %+v, want %+v (a note's own words and a deleted shape aren't drawn)", p.Preview, want)
	}
	for i := range want {
		if p.Preview[i] != want[i] {
			t.Errorf("shape %d is %+v, want %+v", i, p.Preview[i], want[i])
		}
	}
}

func TestAListedBoardTemplateDrawsAtMostItsCap(t *testing.T) {
	many := make([]map[string]any, maxBoardPreviewShapes+40)
	for i := range many {
		many[i] = map[string]any{"id": "n" + strings.Repeat("x", i%7) + string(rune('a'+i%26)), "type": "rectangle", "x": i * 10, "y": 0, "width": 8, "height": 8}
	}
	var p BoardPayload
	if err := json.Unmarshal([]byte(asListed(&model.Template{Kind: KindBoard, Payload: boardPayload(t, many...)}).Payload), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Preview) != maxBoardPreviewShapes || p.Count != len(many) {
		t.Fatalf("drew %d of %d (count %d), want %d", len(p.Preview), len(many), p.Count, maxBoardPreviewShapes)
	}
}

// A board's template holds a board of a few hundred notes: a megabyte, where
// the other kinds keep their 256 KB.
func TestABoardTemplateMayHoldMoreThanOtherKinds(t *testing.T) {
	if got := payloadLimit(KindBoard); got != 1<<20 {
		t.Errorf("a board template holds %d bytes, want a megabyte", got)
	}
	for _, k := range []string{KindDoc, model.KindTable, model.KindAgent, model.KindWorkflow} {
		if got := payloadLimit(k); got != maxPayloadBytes {
			t.Errorf("a %s template holds %d bytes, want %d", k, got, maxPayloadBytes)
		}
	}
}
