package taskrank

import (
	"errors"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// ErrNoSuchCard is a column page asked for after a card that isn't in the
// column any more (moved, deleted, filtered out): the reader re-reads the
// board rather than skip or repeat cards.
var ErrNoSuchCard = errors.New("taskrank: the card the page starts after is not in the column")

// ErrClosedColumn is a column page asked of done or cancelled, which page by
// newest first through the board's closedLimit instead.
var ErrClosedColumn = errors.New("taskrank: a closed column pages through the board's closedLimit")

// ErrUnknownColumn is a status that names no column of the board.
var ErrUnknownColumn = errors.New("taskrank: no such column")

// ColumnPage picks one open column of a board read as places: the column a
// board shows for status (a built-in status, or one of the project's custom
// statuses as partOf names them), in board order, the cards after the one with
// uuid after ("" for the first), at most limit. It returns the picks (still
// places), how many cards the column has in all, and whether more follow.
// Pure: the board's first answer (Pick) and this agree on order, because both
// Sort the same places.
func ColumnPage(columns map[string]*dgraphStruct.BoardPlaces, status string, partOf func(*dgraphStruct.DgraphTask) string, after string, limit int) (picks []*dgraphStruct.DgraphTask, total int, more bool, err error) {
	var column []*dgraphStruct.DgraphTask
	found := false
	for _, c := range dgraphStruct.BoardColumns {
		if c.Status == status && c.Closed {
			return nil, 0, false, ErrClosedColumn
		}
		col := columns[c.Key]
		if c.Closed || col == nil {
			if c.Status == status {
				found = true
			}
			continue
		}
		Sort(col.Places)
		for _, t := range col.Places {
			part := partOf(t)
			if part == status || (part == "" && c.Status == status) {
				column = append(column, t)
			}
		}
		if c.Status == status {
			found = true
		}
	}
	if !found && len(column) == 0 && !customStatus(status) {
		return nil, 0, false, ErrUnknownColumn
	}
	start := 0
	if after != "" {
		start = -1
		for i, t := range column {
			if t.Uuid == after {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return nil, len(column), false, ErrNoSuchCard
		}
	}
	end := len(column)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	return column[start:end], len(column), end < len(column), nil
}

// customStatus is whether status could be a custom status's id (a uuid's
// length and shape is enough here: a known one has already matched cards, and
// an unknown one simply has none).
func customStatus(status string) bool {
	if len(status) != 36 {
		return false
	}
	for i, r := range status {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
				return false
			}
		}
	}
	return true
}
