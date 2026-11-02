package scene

// What a board says (W6-B, miro-05): the words on it, read from a whole saved
// state, for the board's search entry and its AI entry. Both carry the board's
// own access, and every hit on them is checked against the board again
// (business/GlobalSearch readableBoards.go, business/AI docHits.go).

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// TextItem is one thing a board says: a note's, a shape's or an arrow's words,
// a free text, or a frame's name.
type TextItem struct {
	// ID is the element a person points at: the note, shape or arrow the
	// words are bound in, the text itself, or the frame.
	ID string
	// Kind is note, shape, arrow, text or frame.
	Kind string
	// Frame is the frame the item sits in, "" for none; a frame names itself.
	Frame string
	Text  string
}

// TextItems is what a board says, read from its elements root map (Maps'
// "elements"): each frame's name and then the words in it, frame by frame in
// stacking order, then the words in no frame. A deleted element, and words
// bound to one, say nothing; an unnamed frame has no item of its own; runs
// of white space read as one space. Pure.
func TextItems(elements map[string]any) []TextItem {
	live := make(map[string]map[string]any, len(elements))
	ordered := make([]map[string]any, 0, len(elements))
	for id, v := range elements {
		if el, ok := v.(map[string]any); ok && el["isDeleted"] != true {
			live[id] = el
			ordered = append(ordered, el)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return StackedBefore(ordered[i], ordered[j]) })

	var frames []string
	named := map[string]string{}
	inFrame := map[string][]TextItem{}
	var loose []TextItem
	for _, el := range ordered {
		id, _ := el["id"].(string)
		switch kind, _ := el["type"].(string); kind {
		case "frame", "magicframe":
			frames = append(frames, id)
			named[id] = oneLine(el["name"])
		case "text":
			words := oneLine(el["originalText"])
			if words == "" {
				words = oneLine(el["text"])
			}
			if words == "" {
				continue
			}
			item := TextItem{ID: id, Kind: "text", Text: words}
			at := el
			if c, _ := el["containerId"].(string); c != "" {
				container, ok := live[c]
				if !ok {
					continue // bound to a deleted element
				}
				item.ID, item.Kind, at = c, containerKind(container), container
			}
			frame, _ := at["frameId"].(string)
			if _, ok := live[frame]; frame == "" || !ok {
				loose = append(loose, item)
				continue
			}
			item.Frame = frame
			inFrame[frame] = append(inFrame[frame], item)
		}
	}

	var out []TextItem
	for _, f := range frames {
		if named[f] != "" {
			out = append(out, TextItem{ID: f, Kind: "frame", Frame: f, Text: named[f]})
		}
		out = append(out, inFrame[f]...)
	}
	return append(out, loose...)
}

// Outline is what a board says as one text: one item a line, a blank line
// before each frame and before the words in no frame. Cut at max runes (0 is
// no cap) after the last whole item that fits; a first item longer than max
// is cut at max. Pure.
func Outline(items []TextItem, max int) string {
	var b strings.Builder
	n := 0
	for i, it := range items {
		sep := ""
		if i > 0 {
			sep = "\n"
			if it.Kind == "frame" || it.Frame != items[i-1].Frame {
				sep = "\n\n"
			}
		}
		size := utf8.RuneCountInString(sep) + utf8.RuneCountInString(it.Text)
		if max > 0 && n+size > max {
			if i == 0 {
				b.WriteString(string([]rune(it.Text)[:max]))
			}
			break
		}
		b.WriteString(sep)
		b.WriteString(it.Text)
		n += size
	}
	return b.String()
}

// StackedBefore is Excalidraw's stacking rule (orderByFractionalIndex): by
// fractional index, ties and elements without one by id. Pure.
func StackedBefore(a, b map[string]any) bool {
	ai, _ := a["index"].(string)
	bi, _ := b["index"].(string)
	if ai != bi {
		return ai < bi
	}
	aid, _ := a["id"].(string)
	bid, _ := b["id"].(string)
	return aid < bid
}

// containerKind is what words bound in el are: a note's, an arrow's or a
// shape's.
func containerKind(el map[string]any) string {
	if IsNote(el) {
		return "note"
	}
	if kind, _ := el["type"].(string); kind == "arrow" || kind == "line" {
		return "arrow"
	}
	return "shape"
}

// oneLine is v's text with each run of white space read as one space.
func oneLine(v any) string {
	s, _ := v.(string)
	return strings.Join(strings.Fields(s), " ")
}
