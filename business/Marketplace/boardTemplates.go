package business

// Board templates (the whiteboard's W-1, miro-19 "save any board as a
// workspace template"): a board's elements, saved from a board and listed on
// the Templates page beside the docs', tables' and agents'. A board's
// pictures stay with it: their files are the board's own, so a template keeps
// no image. Installing one makes a new private board, as the boards list
// does, and the board lays the template's elements on itself when it opens
// (components/board/boardWorkbench, ?template=) with new ids, so a copy never
// points back into the board it came from (lib/board/copyElements).

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	boardAdapter "github.com/akashc777/OneCamp/adapter/Board"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// KindBoard is a board template's kind.
const KindBoard = "board"

const (
	// maxBoardTemplateElements bounds a template: a board of retros and
	// canvases is a few hundred; past this it's a board to copy, not a start.
	maxBoardTemplateElements = 3000
	maxBoardTitleLen         = 200
	// maxBoardPayloadBytes is a board template's size: a board of a few
	// hundred notes (a note and its words are about 1.2 KB).
	maxBoardPayloadBytes = 1 << 20
	// maxBoardPreviewShapes bounds a listed template's drawing: enough for a
	// card to show its layout.
	maxBoardPreviewShapes = 160
)

// BoardPayload is a board template's payload: the title a board made from it
// starts with, and its Excalidraw elements as the board stored them. A list
// carries Count and Preview in place of Elements (asListed).
type BoardPayload struct {
	Title    string            `json:"title"`
	Elements []json.RawMessage `json:"elements,omitempty"`
	// Count is how many elements a person sees on it (listed form).
	Count int `json:"count,omitempty"`
	// Preview is a small drawing of it (listed form).
	Preview []BoardShape `json:"preview,omitempty"`
}

// BoardShape is one shape of a listed board template's drawing, in the
// board's own units, rounded. An arrow's or a line's W and H run from its
// start to its end, so either may be negative.
type BoardShape struct {
	Type string `json:"type"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
	W    int    `json:"w"`
	H    int    `json:"h"`
	Fill string `json:"fill,omitempty"`
	Note bool   `json:"note,omitempty"`
}

// readBoardPayload decodes and checks a board template's payload: elements
// that are objects with an id and a type, no pictures, not too many. Pure.
func readBoardPayload(raw string) (BoardPayload, error) {
	var p BoardPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, fmt.Errorf("not a board template")
	}
	p.Title = strings.TrimSpace(p.Title)
	if len(p.Title) > maxBoardTitleLen {
		return p, fmt.Errorf("the title is too long")
	}
	if len(p.Elements) == 0 {
		return p, fmt.Errorf("a board template needs something on it")
	}
	if len(p.Elements) > maxBoardTemplateElements {
		return p, fmt.Errorf("a board template holds at most %d elements", maxBoardTemplateElements)
	}
	for i, raw := range p.Elements {
		var el struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &el); err != nil || el.ID == "" || el.Type == "" {
			return p, fmt.Errorf("element %d has no id or type", i)
		}
		if el.Type == "image" {
			return p, fmt.Errorf("a board template keeps no pictures: their files stay with the board")
		}
	}
	return p, nil
}

// listedBoardPayload is a board template's payload as a list carries it: its
// title, how many elements a person sees on it, and a drawing of the first
// maxBoardPreviewShapes of them (a note's own words are drawn with the note,
// so bound text isn't). Pure; a payload it can't read lists as an empty one.
func listedBoardPayload(raw string) string {
	var p BoardPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return "{}"
	}
	out := BoardPayload{Title: strings.TrimSpace(p.Title)}
	for _, item := range p.Elements {
		var el struct {
			Type            string      `json:"type"`
			X               float64     `json:"x"`
			Y               float64     `json:"y"`
			Width           float64     `json:"width"`
			Height          float64     `json:"height"`
			IsDeleted       bool        `json:"isDeleted"`
			ContainerID     *string     `json:"containerId"`
			BackgroundColor string      `json:"backgroundColor"`
			Points          [][]float64 `json:"points"`
			CustomData      struct {
				Kind string `json:"kind"`
			} `json:"customData"`
		}
		if json.Unmarshal(item, &el) != nil || el.IsDeleted {
			continue
		}
		out.Count++
		if (el.Type == "text" && el.ContainerID != nil && *el.ContainerID != "") || len(out.Preview) >= maxBoardPreviewShapes {
			continue
		}
		shape := BoardShape{Type: el.Type, X: roundUnit(el.X), Y: roundUnit(el.Y), W: roundUnit(el.Width), H: roundUnit(el.Height), Note: el.CustomData.Kind == "note"}
		if n := len(el.Points); (el.Type == "arrow" || el.Type == "line") && n > 0 && len(el.Points[n-1]) == 2 {
			shape.W, shape.H = roundUnit(el.Points[n-1][0]), roundUnit(el.Points[n-1][1])
		}
		if el.BackgroundColor != "" && el.BackgroundColor != "transparent" {
			shape.Fill = el.BackgroundColor
		}
		out.Preview = append(out.Preview, shape)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// roundUnit is a board's coordinate to the nearest unit, for a drawing.
func roundUnit(v float64) int {
	return int(math.Round(v))
}

// installBoard makes a new private board from a board template, as the
// installing person; the board lays the template on itself when it opens.
func installBoard(ctx context.Context, payload, name string, userInfo *userModels.UserInfo) (*InstallResult, error) {
	p, err := readBoardPayload(payload)
	if err != nil {
		return nil, err
	}
	title := p.Title
	if title == "" {
		title = name
	}
	board, err := boardBusiness.CreateBoard(ctx, &userInfo.UserDgraphInfo, &boardAdapter.InputCreateBoard{BoardTitle: title, BoardPrivate: true})
	if err != nil || board == nil || board.Uuid == "" {
		return nil, fmt.Errorf("failed to make a board from the template")
	}
	return &InstallResult{Kind: KindBoard, EntityID: board.Uuid, Name: title}, nil
}
