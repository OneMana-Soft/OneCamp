package scene

// Who made each note (lib/board/authors.ts in the web app): the board
// document's noteAuthors map holds each note's maker by the note's id, written
// once, by whoever first puts the note on the board. A board drawn here names
// who it's drawn for (an import's notes are the importer's) or the people a
// seed gives its notes; the web app's notes CSV reads the map. Notes made
// before makers were kept have none.

import (
	"sort"
	"strings"
)

// NoteAuthorsMap is the root map of note makers (the web app's NOTE_AUTHORS).
const NoteAuthorsMap = "noteAuthors"

// Maker is a note's maker: a member's uuid (empty for a person a seed names
// without one) and the name the board shows for them.
type Maker struct {
	ID, Name string
}

// IsNote is whether el is a live sticky note, marked as one
// (customData.kind "note"), as every note made here is. Pure.
func IsNote(el Element) bool {
	if deleted, _ := el["isDeleted"].(bool); deleted {
		return false
	}
	data, _ := el["customData"].(map[string]any)
	kind, _ := data["kind"].(string)
	return kind == "note"
}

// NotesMadeBy is every note among elements, each made by maker: a board drawn
// for one person. Pure.
func NotesMadeBy(elements []Element, maker Maker) map[string]Maker {
	out := map[string]Maker{}
	for _, el := range elements {
		if id, _ := el["id"].(string); id != "" && IsNote(el) {
			out[id] = maker
		}
	}
	return out
}

// NoteAuthors is the noteAuthors root naming these makers, by note id, in id
// order. A maker without a name is left out: the note says no one. Pure.
func NoteAuthors(makers map[string]Maker) Root {
	ids := make([]string, 0, len(makers))
	for id, m := range makers {
		if id != "" && strings.TrimSpace(m.Name) != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	entries := make([]Entry, 0, len(ids))
	for _, id := range ids {
		m := makers[id]
		entries = append(entries, Entry{Key: id, Value: map[string]any{"id": m.ID, "name": strings.TrimSpace(m.Name)}})
	}
	return Root{Name: NoteAuthorsMap, Entries: entries}
}
