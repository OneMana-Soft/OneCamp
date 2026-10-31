package scene

import (
	"encoding/base64"
	"reflect"
	"testing"
)

// Who made each note: kept in the board's noteAuthors map, for notes only,
// read back as the web app's notes CSV reads it.
func TestNoteMakersAreKeptInTheirOwnMapForNotesOnly(t *testing.T) {
	ada := Maker{ID: "u-ada", Name: " Ada Lovelace "}
	els := Note{ID: "n1", W: 180, H: 180, Colour: NoteColours[0], Text: "Ship it"}.Elements(1)
	gone := Note{ID: "n2", X: 200, W: 180, H: 180, Colour: NoteColours[1], Text: "Gone"}.Elements(1)
	gone[0]["isDeleted"] = true
	els = append(append(els, gone...), Base("s1", "rectangle", 0, 400, 10, 10, 1))

	made := NotesMadeBy(els, ada)
	if want := map[string]Maker{"n1": ada}; !reflect.DeepEqual(made, want) {
		t.Fatalf("the notes made: %v, want only the live note %v", made, want)
	}
	state, _, err := State(1, els, nil, map[string]any{"style": "clean"}, NoteAuthors(made))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(state)
	back, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"n1": map[string]any{"id": "u-ada", "name": "Ada Lovelace"}}
	if !reflect.DeepEqual(back[NoteAuthorsMap], want) || back["boardMeta"]["style"] != "clean" || len(back["elements"]) != len(els) {
		t.Fatalf("read back: makers %v, meta %v, %d elements", back[NoteAuthorsMap], back["boardMeta"], len(back["elements"]))
	}

	// A maker without a name is no one: no map at all when there's no one to name.
	state, _, err = State(1, els, nil, nil, NoteAuthors(NotesMadeBy(els, Maker{ID: "u-x", Name: "  "})))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = base64.StdEncoding.DecodeString(state)
	if back, _ = Decode(raw); back[NoteAuthorsMap] != nil {
		t.Fatalf("makers without a name: %v", back[NoteAuthorsMap])
	}
}
