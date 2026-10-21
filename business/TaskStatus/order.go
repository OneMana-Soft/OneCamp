package business

// A project's column order (design/projects-vs-asana-jira/BOARD_COLUMNS_PLAN.md):
// its statuses in the order its admins dragged them, each a built-in key or a
// custom status's id. Board columns, the list's groups and every picker
// follow it. Open statuses come first in any order among themselves; closed
// ones (done and cancelled, and those that count as them) always come after,
// in any order among themselves: closed columns load on their own page and
// fold, and a board read left to right ends where work ends.

import (
	"context"
	"fmt"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/TaskStatus"
	"github.com/google/uuid"
)

// ErrClosedFirst is an order that puts a closed status before an open one.
var ErrClosedFirst = fmt.Errorf("%w: Done and Cancelled, and statuses that count as them, stay after the open ones", ErrInvalid)

// Seams for tests.
var (
	savedOrder = model.Order
	saveOrder  = model.SetOrder
	customsOf  = model.List
)

// DefaultOrder is a project's order before its admins set one: each built-in
// status followed by its own statuses, in the order they were given. Pure.
func DefaultOrder(customs []*model.TaskStatus) []string {
	sorted := append([]*model.TaskStatus(nil), customs...)
	sortByCategory(sorted)
	out := make([]string, 0, len(BuiltIns)+len(sorted))
	for _, b := range BuiltIns {
		out = append(out, b.Key)
		for _, c := range sorted {
			if c.Category == b.Key {
				out = append(out, c.ID.String())
			}
		}
	}
	return out
}

// EffectiveOrder is the order a project's statuses show in: saved as it was
// saved, less what no longer exists; a status it doesn't name (new since)
// goes after the one before it in the default order (so a new status of In
// progress goes after In progress, or after the last of its own already
// there); and closed statuses after open ones, each keeping its place among
// its own. Pure.
func EffectiveOrder(saved []string, customs []*model.TaskStatus) []string {
	def := DefaultOrder(customs)
	if len(saved) == 0 {
		return def
	}
	known := make(map[string]bool, len(def))
	for _, v := range def {
		known[v] = true
	}
	out := make([]string, 0, len(def))
	in := make(map[string]bool, len(def))
	for _, v := range saved {
		v = canonical(v)
		if known[v] && !in[v] {
			out = append(out, v)
			in[v] = true
		}
	}
	prev := ""
	for _, v := range def {
		if !in[v] {
			at := 0
			if prev != "" {
				at = indexOf(out, prev) + 1
			}
			out = append(out[:at], append([]string{v}, out[at:]...)...)
			in[v] = true
		}
		prev = v
	}
	return closedLast(out, categories(customs))
}

// SetOrder saves a project's column order, by whom: every one of its
// statuses once, closed ones after open ones. The order saved is the one
// returned.
func SetOrder(ctx context.Context, projectID uuid.UUID, order []string, by uuid.UUID) ([]string, error) {
	customs, err := customsOf(ctx, projectID)
	if err != nil {
		return nil, err
	}
	norm, err := CheckOrder(order, customs)
	if err != nil {
		return nil, err
	}
	if err := saveOrder(ctx, projectID, norm, by); err != nil {
		return nil, err
	}
	return norm, nil
}

// CheckOrder is order as it is kept (custom ids in their usual form) when it
// names each of a project's statuses once and puts no closed status before an
// open one, else why not. Pure.
func CheckOrder(order []string, customs []*model.TaskStatus) ([]string, error) {
	def := DefaultOrder(customs)
	known := make(map[string]bool, len(def))
	for _, v := range def {
		known[v] = true
	}
	seen := make(map[string]bool, len(order))
	norm := make([]string, 0, len(order))
	for _, raw := range order {
		v := canonical(raw)
		switch {
		case !known[v]:
			return nil, fmt.Errorf("%w: %q isn't one of the project's statuses", ErrInvalid, raw)
		case seen[v]:
			return nil, fmt.Errorf("%w: %q is named twice", ErrInvalid, raw)
		}
		seen[v] = true
		norm = append(norm, v)
	}
	if len(norm) != len(def) {
		return nil, fmt.Errorf("%w: name each of the project's %d statuses once", ErrInvalid, len(def))
	}
	cats := categories(customs)
	closedSeen := false
	for _, v := range norm {
		closed := IsClosed(categoryIn(v, cats))
		if !closed && closedSeen {
			return nil, ErrClosedFirst
		}
		closedSeen = closedSeen || closed
	}
	return norm, nil
}

// orderOf is a project's effective order, read.
func orderOf(ctx context.Context, projectID uuid.UUID, customs []*model.TaskStatus) ([]string, error) {
	saved, err := savedOrder(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return EffectiveOrder(saved, customs), nil
}

// canonical is a status value as kept: a built-in key as it is, a custom
// status's id in its usual lower-case form.
func canonical(v string) string {
	v = strings.TrimSpace(v)
	if IsBuiltIn(v) {
		return v
	}
	if id, err := uuid.Parse(v); err == nil {
		return id.String()
	}
	return v
}

// categories is each custom status's category, by id.
func categories(customs []*model.TaskStatus) map[string]string {
	out := make(map[string]string, len(customs))
	for _, c := range customs {
		out[c.ID.String()] = c.Category
	}
	return out
}

// categoryIn is what a value counts as: a built-in itself, a custom status
// its category.
func categoryIn(v string, cats map[string]string) string {
	if IsBuiltIn(v) {
		return v
	}
	return cats[v]
}

// closedLast moves closed statuses after open ones, each keeping its order.
func closedLast(order []string, cats map[string]string) []string {
	open := make([]string, 0, len(order))
	var closed []string
	for _, v := range order {
		if IsClosed(categoryIn(v, cats)) {
			closed = append(closed, v)
		} else {
			open = append(open, v)
		}
	}
	return append(open, closed...)
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}
