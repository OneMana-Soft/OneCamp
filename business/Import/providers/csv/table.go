// Package csv imports a CSV of tasks as a project: OneCamp's own export of a
// task list, or a rival's (Asana, Jira, Linear, ClickUp, Notion). The admin
// uploads the file through the generic presign and finalize (source
// backup_csv). Its columns are read by name, ours first and the rivals' too;
// every other column becomes a field of the kind its values suggest, which
// the plan shows and the admin can change. A row whose Parent names another
// row (by key, else by title) becomes that row's sub-task.
package csv

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// MaxRows is the most tasks a CSV brings: as many as a list's export writes
// (business/Project ExportMax).
const MaxRows = 50_000

// maxBytes is the most of a file read. A full export with long descriptions
// is well under it; past it, the file is refused before it fills memory.
const maxBytes = 128 << 20

var (
	errEmpty   = errors.New("The file is empty. A CSV of tasks starts with a row of column names, then a row for each task.")
	errTooBig  = fmt.Errorf("The file is larger than %d MB. Split it and import each part.", maxBytes>>20)
	errNoTitle = errors.New("No column holds the tasks' titles. Name it Title (or Name, Task or Summary) and upload the file again.")
	errNoRows  = errors.New("The file has column names but no tasks under them.")
)

// errTooMany is a file with more rows than an import brings.
var errTooMany = fmt.Errorf("The file has more than %d tasks, the most an import brings. Split it and import each part.", MaxRows)

// table is a CSV as it was read: its columns and its rows.
type table struct {
	id   string // the file's own: its SHA-256's first 12 hex digits
	cols []*column
	rows []row
}

// row is a row's cells as they were typed (helpers.CSVValue) and the line of
// the file it starts on.
type row struct {
	line  int
	cells []string
}

// column is the cells under one header. A header written more than once
// (Jira repeats Labels for each label) is one column.
type column struct {
	name string // the header as first written
	idx  []int
	prop property // the task's own property it fills, or "" for a field
	// email is the column of these people's addresses ("Assignee Email"
	// beside "Assignee"), and emailOf the people column an address column
	// stands beside.
	email, emailOf *column
}

// readTable reads a CSV of at most maxLen bytes: the header row, then each
// row with anything in it.
func readTable(r io.Reader, maxLen int64) (*table, error) {
	sum := sha256.New()
	limit := &io.LimitedReader{R: r, N: maxLen + 1}
	cr := helpers.NewCSVReader(io.TeeReader(limit, sum))
	head, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, errEmpty
	}
	if err != nil {
		return nil, unreadable(err)
	}
	t := &table{cols: columnsOf(head)}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, unreadable(err)
		}
		if limit.N <= 0 {
			return nil, errTooBig
		}
		if blank(rec) {
			continue
		}
		if len(t.rows) == MaxRows {
			return nil, errTooMany
		}
		line, _ := cr.FieldPos(0)
		for i := range rec {
			rec[i] = helpers.CSVValue(strings.TrimSpace(rec[i]))
		}
		t.rows = append(t.rows, row{line: line, cells: rec})
	}
	if limit.N <= 0 {
		return nil, errTooBig
	}
	t.id = hex.EncodeToString(sum.Sum(nil))[:12]
	return t, nil
}

func unreadable(err error) error {
	return fmt.Errorf("The file couldn't be read as a CSV: %v", err)
}

func blank(rec []string) bool {
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// columnsOf names each column from the header row: the property its name
// says (ours first, then the rivals'), else a field. A property two headers
// name (Title and Name) is the first one's; the other is a field.
func columnsOf(head []string) []*column {
	var cols []*column
	byKey := map[string]*column{}
	taken := map[property]bool{}
	for i, h := range head {
		name := strings.TrimSpace(h)
		if name == "" {
			name = fmt.Sprintf("Column %d", i+1)
		}
		key := headerKey(name)
		if c := byKey[key]; c != nil {
			c.idx = append(c.idx, i)
			continue
		}
		c := &column{name: name, idx: []int{i}}
		if p := aliases[key]; p != "" && !taken[p] {
			c.prop, taken[p] = p, true
		}
		byKey[key] = c
		cols = append(cols, c)
	}
	for _, c := range cols {
		key := headerKey(c.name)
		for _, suffix := range []string{" email", " e-mail", " email address"} {
			base, ok := strings.CutSuffix(key, suffix)
			if p := byKey[base]; ok && p != nil && p.email == nil && c.prop == "" {
				p.email, c.emailOf = c, p
				break
			}
		}
	}
	return cols
}

// headerKey is a header as headers are matched: lower-cased, its spaces
// collapsed.
func headerKey(h string) string {
	return strings.ToLower(strings.Join(strings.Fields(h), " "))
}

// cell is a row's value in a column: its first cell with anything in it.
func (r row) cell(c *column) string {
	if c == nil {
		return ""
	}
	for _, i := range c.idx {
		if i < len(r.cells) && r.cells[i] != "" {
			return r.cells[i]
		}
	}
	return ""
}

// all is each of a row's cells in a column with anything in it, for a
// header written more than once.
func (r row) all(c *column) []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, i := range c.idx {
		if i < len(r.cells) && r.cells[i] != "" {
			out = append(out, r.cells[i])
		}
	}
	return out
}
