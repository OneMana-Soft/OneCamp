package business

// Files (tables B1): a cell holds files of its table, each an attachment
// whose place is the table (business/FileAccess), uploaded through
// /uploadFile as every file is and served to whoever may read the table's
// rows (GET /tables/{id}/files/{file}). The cell keeps each file's id with its
// name, size and type as it was uploaded: a file has no title of its own to
// read again, unlike a doc a relation names.

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// FileRef is one file in a cell.
type FileRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size,omitempty"`
	Mime string `json:"mime,omitempty"`
}

// maxFiles is the most files one cell holds; maxFileName the longest name.
const (
	maxFiles    = 100
	maxFileName = 255
)

// ParseFiles reads a files cell, as stored or as written: each file once, in
// order, its name cleaned (no control characters, at most maxFileName
// characters, "File" for none). bad is the first item that isn't a file, as
// given. Pure.
func ParseFiles(raw any) (refs []FileRef, bad string) {
	list, isList := raw.([]any)
	if !isList {
		if raw == nil {
			return nil, ""
		}
		list = []any{raw}
	}
	seen := map[string]bool{}
	for _, it := range list {
		m, ok := it.(map[string]any)
		var id uuid.UUID
		var err error = fmt.Errorf("no id")
		if ok {
			s, _ := m["id"].(string)
			id, err = uuid.Parse(strings.TrimSpace(s))
		}
		if err != nil {
			if bad == "" {
				b, _ := json.Marshal(it)
				bad = string(b)
			}
			continue
		}
		if seen[id.String()] {
			continue
		}
		seen[id.String()] = true
		name, _ := m["name"].(string)
		mime, _ := m["mime"].(string)
		size, _ := m["size"].(float64)
		if size < 0 || size != float64(int64(size)) {
			size = 0
		}
		refs = append(refs, FileRef{ID: id.String(), Name: cleanName(name), Size: int64(size), Mime: cleanName(mime)})
	}
	return refs, bad
}

// cleanName is a name as a cell keeps it.
func cleanName(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > maxFileName {
		s = string(r[:maxFileName])
	}
	return s
}

func coerceFiles(f *model.Field, v any, in Input) (any, error) {
	refs, bad := ParseFiles(v)
	if bad != "" {
		if r := []rune(bad); len(r) > maxQuoted {
			bad = string(r[:maxQuoted]) + "…"
		}
		return nil, fmt.Errorf("%q holds files by their ids: %s isn't one", f.Name, bad)
	}
	if len(refs) > maxFiles {
		return nil, fmt.Errorf("%q holds at most %d files", f.Name, maxFiles)
	}
	if len(refs) == 0 {
		return refs, nil
	}
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
		if r.Name == "" {
			refs[i].Name = "File"
		}
	}
	if in.Files == nil {
		return nil, fmt.Errorf("%q holds files added to this table", f.Name)
	}
	ours, err := in.Files(ids)
	if err != nil {
		return nil, fmt.Errorf("couldn't check %q's files", f.Name)
	}
	for _, r := range refs {
		if !ours[r.ID] {
			return nil, fmt.Errorf("%q holds files added to this table: %s isn't one", f.Name, r.ID)
		}
	}
	return refs, nil
}
