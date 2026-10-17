package position

import (
	"errors"
	"sort"
	"time"
)

// ErrNotSibling is a place to follow that isn't among the siblings.
var ErrNotSibling = errors.New("position: the one to follow isn't among the siblings")

// Sibling is a node as a level is ordered: by its position, and the ones
// without one (made before the tree existed) after those, oldest first.
type Sibling struct {
	ID        string
	Position  string
	CreatedAt time.Time
}

// Order sorts a level the way it's shown: by position; nodes with no usable
// position after the rest, oldest first; ties by id, so every reader agrees.
func Order(s []Sibling) {
	sort.SliceStable(s, func(i, j int) bool {
		a, b := s[i], s[j]
		va, vb := ValidPosition(a.Position), ValidPosition(b.Position)
		if va != vb {
			return va
		}
		if va && a.Position != b.Position {
			return a.Position < b.Position
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}

// Where a node goes among its new siblings: right after After, or with no
// After, First or last.
type Where struct {
	After string
	First bool
}

// Placement is what a move or a new node writes: its own key, and fresh keys
// for siblings whose keys couldn't order it (none, usually).
type Placement struct {
	Key      string
	Renumber map[string]string // sibling id -> its new key
}

// Place finds the key for id at w among siblings (in shown order; id itself
// is left out if present). When the neighbours' keys can't take a key between
// them (missing, repeated, out of order), the whole level is numbered afresh
// in the order it's shown, which is the order people see, so nothing moves.
func Place(siblings []Sibling, id string, w Where) (Placement, error) {
	list := make([]Sibling, 0, len(siblings))
	for _, s := range siblings {
		if s.ID != id {
			list = append(list, s)
		}
	}
	at := len(list) // index the node takes
	switch {
	case w.After != "":
		at = -1
		for i, s := range list {
			if s.ID == w.After {
				at = i + 1
				break
			}
		}
		if at < 0 {
			return Placement{}, ErrNotSibling
		}
	case w.First:
		at = 0
	}

	before, after := "", ""
	if at > 0 {
		before = list[at-1].Position
	}
	if at < len(list) {
		after = list[at].Position
	}
	if usable(list, at) {
		if k, err := Between(before, after); err == nil {
			return Placement{Key: k}, nil
		}
	}

	// Number the level afresh, the node included at its place.
	keys, err := Spread("", "", len(list)+1)
	if err != nil {
		return Placement{}, err
	}
	p := Placement{Key: keys[at], Renumber: map[string]string{}}
	for i, s := range list {
		k := keys[i]
		if i >= at {
			k = keys[i+1]
		}
		if s.Position != k {
			p.Renumber[s.ID] = k
		}
	}
	return p, nil
}

// usable says whether the keys around index at can order a new key: every
// key up to and including the next one is valid and strictly increasing.
// Checking only the two neighbours would miss a run of keyless siblings after
// them, which sort last and would land before the new key.
func usable(list []Sibling, at int) bool {
	prev := ""
	for i, s := range list {
		if !ValidPosition(s.Position) || (prev != "" && s.Position <= prev) {
			return false
		}
		prev = s.Position
		if i > at {
			break
		}
	}
	return true
}
