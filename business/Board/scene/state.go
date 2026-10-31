package scene

import (
	"encoding/base64"
	"fmt"
	"sort"
)

// File is a picture on a board: the id its image elements name (fileId), the
// stored object it is (served by the board's attachment route) and its type.
// The board's "files" map holds only this; the bytes are in object storage.
type File struct {
	ID         string
	ObjectUUID string
	MimeType   string
}

// State is a board's saved state as the collaboration service opens one: the
// Yjs update, base64, that sets its elements (each under its id), its files,
// its boardMeta and any more root maps (its note makers: NoteAuthors), written
// as client. It is read back before it is handed over, so a state the
// collaboration service couldn't open is never saved. live is how many
// elements a person sees, for the board's snippet.
func State(client uint32, elements []Element, files []File, meta map[string]any, more ...Root) (state string, live int, err error) {
	els := make([]Entry, 0, len(elements))
	seen := make(map[string]bool, len(elements))
	for _, el := range elements {
		id, _ := el["id"].(string)
		if id == "" || seen[id] {
			return "", 0, fmt.Errorf("an element without an id of its own (%q)", id)
		}
		seen[id] = true
		els = append(els, Entry{Key: id, Value: map[string]any(el)})
	}
	fs := make([]Entry, 0, len(files))
	for _, f := range files {
		fs = append(fs, Entry{Key: f.ID, Value: map[string]any{"id": f.ID, "objectUuid": f.ObjectUUID, "mimeType": f.MimeType}})
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ms := make([]Entry, 0, len(keys))
	for _, k := range keys {
		ms = append(ms, Entry{Key: k, Value: meta[k]})
	}
	roots := []Root{{Name: "elements", Entries: els}}
	if len(fs) > 0 {
		roots = append(roots, Root{Name: "files", Entries: fs})
	}
	if len(ms) > 0 {
		roots = append(roots, Root{Name: "boardMeta", Entries: ms})
	}
	for _, r := range more {
		if len(r.Entries) > 0 {
			roots = append(roots, r)
		}
	}
	update, err := MapsUpdate(client, roots...)
	if err != nil {
		return "", 0, err
	}
	back, err := Decode(update)
	if err != nil {
		return "", 0, err
	}
	if len(back["elements"]) != len(els) || len(back["files"]) != len(fs) {
		return "", 0, fmt.Errorf("the state reads back %d elements and %d files, not %d and %d", len(back["elements"]), len(back["files"]), len(els), len(fs))
	}
	for _, r := range more {
		if len(back[r.Name]) != len(r.Entries) {
			return "", 0, fmt.Errorf("the state reads back %d of %s, not %d", len(back[r.Name]), r.Name, len(r.Entries))
		}
	}
	return base64.StdEncoding.EncodeToString(update), LiveCount(elements), nil
}
