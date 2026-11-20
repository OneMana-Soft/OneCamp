package business

import (
	"errors"
	"slices"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Board"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// roleBoard is a live board the owner made, as one person reads it: edit and
// coOwner are that person's counts, share the board's editors-may-share switch.
func roleBoard(edit, read, coOwner uint8, share *bool) *dgraphStruct.DgraphBoard {
	private := true
	return &dgraphStruct.DgraphBoard{Uuid: "b1", IsPrivate: &private, CreatedBy: &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "owner"},
		HasEditAccess: edit, HasReadAccess: read, HasOwnerAccess: coOwner, EditorsShare: share}
}

// Who may do what on a board (miro-32, the lead's rulings R2, R3 and R5): the
// owner everything; a co-owner shares, changes settings and protects, but
// never deletes; an editor draws, and shares only while the switch is on; a
// viewer reads. A deleted board is nobody's.
func TestBoardRolesMatrix(t *testing.T) {
	on, off := true, false
	gone := roleBoard(0, 0, 0, nil)
	deleted := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	gone.DeletedAt = &deleted
	type can struct{ read, edit, manage, share, del, protect bool }
	for _, c := range []struct {
		name  string
		board *dgraphStruct.DgraphBoard
		who   string
		want  can
	}{
		{"the owner", roleBoard(0, 0, 0, nil), "owner", can{true, true, true, true, true, true}},
		{"a co-owner", roleBoard(1, 0, 1, nil), "co", can{true, true, true, true, false, true}},
		{"a co-owner with the switch off", roleBoard(1, 0, 1, &off), "co", can{true, true, true, true, false, true}},
		{"an editor, the switch never set", roleBoard(1, 0, 0, nil), "ed", can{true, true, false, true, false, false}},
		{"an editor, the switch on", roleBoard(1, 0, 0, &on), "ed", can{true, true, false, true, false, false}},
		{"an editor, the switch off", roleBoard(1, 0, 0, &off), "ed", can{true, true, false, false, false, false}},
		{"a viewer", roleBoard(0, 1, 0, nil), "vi", can{true, false, false, false, false, false}},
		{"someone it isn't shared with", roleBoard(0, 0, 0, nil), "x", can{}},
		{"the owner of a deleted board", gone, "owner", can{}},
	} {
		got := can{CanRead(c.board, c.who), CanEdit(c.board, c.who), CanManage(c.board, c.who), MayShare(c.board, c.who),
			CanDelete(c.board, c.who), CanProtect(c.board, c.who, false)}
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// A change to who has which role, as each person may make it. Every refusal
// is a Refusal, which the route answers 403 with its words.
func TestBoardRoleChangeRefusals(t *testing.T) {
	off := false
	holders := map[string]string{"co": "full", "co2": "full", "ed": "edit", "vi": "view"}
	owner, co, ed, vi := roleBoard(0, 0, 0, nil), roleBoard(1, 0, 1, nil), roleBoard(1, 0, 0, nil), roleBoard(0, 1, 0, nil)
	edOff := roleBoard(1, 0, 0, &off)
	in := func(f func(*adapter.InputUpdateBoardPermissions)) adapter.InputUpdateBoardPermissions {
		x := adapter.InputUpdateBoardPermissions{BoardId: "b1"}
		f(&x)
		return x
	}
	for _, c := range []struct {
		name  string
		board *dgraphStruct.DgraphBoard
		who   string
		in    adapter.InputUpdateBoardPermissions
		want  error
	}{
		{"the owner makes a co-owner", owner, "owner", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddCoOwners = []string{"new"} }), nil},
		{"the owner takes a co-owner's place away", owner, "owner", in(func(x *adapter.InputUpdateBoardPermissions) { x.RemoveCoOwners = []string{"co"} }), nil},
		{"the owner makes a co-owner a viewer", owner, "owner", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddViewers = []string{"co"} }), nil},
		{"the owner names themselves", owner, "owner", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddViewers = []string{"owner"} }), ErrOwnersAccess},
		{"a co-owner adds an editor", co, "co", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddEditors = []string{"new"} }), nil},
		{"a co-owner removes a viewer", co, "co", in(func(x *adapter.InputUpdateBoardPermissions) { x.RemoveViewers = []string{"vi"} }), nil},
		{"a co-owner makes an editor a commenter", co, "co", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddCommenters = []string{"ed"} }), nil},
		{"a co-owner makes a co-owner", co, "co", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddCoOwners = []string{"new"} }), ErrCoOwnersOwnerOnly},
		{"a co-owner removes a co-owner", co, "co", in(func(x *adapter.InputUpdateBoardPermissions) { x.RemoveCoOwners = []string{"co2"} }), ErrCoOwnersOwnerOnly},
		{"a co-owner lowers a co-owner", co, "co", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddViewers = []string{"co2"} }), ErrCoOwnersOwnerOnly},
		{"a co-owner removes the owner", co, "co", in(func(x *adapter.InputUpdateBoardPermissions) { x.RemoveEditors = []string{"owner"} }), ErrOwnersAccess},
		{"a co-owner makes the owner a viewer", co, "co", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddViewers = []string{"OWNER"} }), ErrOwnersAccess},
		{"an editor invites a viewer", ed, "ed", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddViewers = []string{"new"} }), nil},
		{"an editor invites an editor", ed, "ed", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddEditors = []string{"new"} }), nil},
		{"an editor invites someone on the board", ed, "ed", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddEditors = []string{"vi"} }), ErrAlreadyOnBoard},
		{"an editor invites the owner", ed, "ed", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddViewers = []string{"owner"} }), ErrOwnersAccess},
		{"an editor removes a viewer", ed, "ed", in(func(x *adapter.InputUpdateBoardPermissions) { x.RemoveViewers = []string{"vi"} }), ErrInviteOnly},
		{"an editor makes a co-owner", ed, "ed", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddCoOwners = []string{"new"} }), ErrCoOwnersOwnerOnly},
		{"an editor with the switch off", edOff, "ed", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddViewers = []string{"new"} }), ErrEditorsMayNotShare},
		{"a viewer", vi, "vi", in(func(x *adapter.InputUpdateBoardPermissions) { x.AddViewers = []string{"new"} }), ErrNotManager},
	} {
		err := roleChangeRefusal(c.board, c.who, holders, c.in)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
			continue
		}
		var r Refusal
		if err != nil && !errors.As(err, &r) {
			t.Errorf("%s: %v isn't a Refusal, so it would answer 400", c.name, err)
		}
	}
}

// Only the owner and co-owners turn the editors-may-share switch or change
// the board's general access; anyone who may edit changes its title.
func TestBoardSettingsRefusals(t *testing.T) {
	on, private := true, false
	title := "Plan"
	for _, c := range []struct {
		name  string
		board *dgraphStruct.DgraphBoard
		who   string
		in    adapter.InputUpdateBoard
		want  error
	}{
		{"an editor renames it", roleBoard(1, 0, 0, nil), "ed", adapter.InputUpdateBoard{Title: &title}, nil},
		{"an editor opens it to the workspace", roleBoard(1, 0, 0, nil), "ed", adapter.InputUpdateBoard{IsPrivate: &private}, ErrNotManager},
		{"an editor turns the switch", roleBoard(1, 0, 0, nil), "ed", adapter.InputUpdateBoard{EditorsShare: &on}, ErrNotManager},
		{"a co-owner turns the switch", roleBoard(1, 0, 1, nil), "co", adapter.InputUpdateBoard{EditorsShare: &on}, nil},
		{"a co-owner opens it", roleBoard(1, 0, 1, nil), "co", adapter.InputUpdateBoard{IsPrivate: &private}, nil},
		{"a viewer renames it", roleBoard(0, 1, 0, nil), "vi", adapter.InputUpdateBoard{Title: &title}, ErrNotEditor},
	} {
		if err := SettingsRefusal(c.board, c.who, c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

// One role each (CR-270 F1): a co-owner taken off the board stays an editor
// only when the same change neither takes their edit away nor gives them
// another role, and no write both adds and removes someone on one role.
func TestBoardRolesOneEach(t *testing.T) {
	x := []string{"x"}
	type roles struct{ co, ed, vi, cm bool }
	type in = adapter.InputUpdateBoardPermissions
	has := func(l []string) bool { return slices.Contains(l, "x") }
	for _, c := range []struct {
		name string
		held roles
		in   in
		want roles
	}{
		{"a co-owner taken off stays an editor", roles{co: true, ed: true}, in{RemoveCoOwners: x}, roles{ed: true}},
		{"a co-owner taken off with their edit holds no role", roles{co: true, ed: true}, in{RemoveCoOwners: x, RemoveEditors: x}, roles{}},
		{"a co-owner taken off and made a viewer is a viewer only", roles{co: true, ed: true}, in{RemoveCoOwners: x, AddViewers: x}, roles{vi: true}},
		{"a co-owner taken off and made a commenter is a commenter only", roles{co: true, ed: true}, in{RemoveCoOwners: x, AddCommenters: x}, roles{cm: true}},
		{"a viewer made a co-owner edits too and stops viewing", roles{vi: true}, in{AddCoOwners: x}, roles{co: true, ed: true}},
		{"an editor given view is a viewer, not a co-owner", roles{ed: true}, in{AddViewers: x}, roles{vi: true}},
	} {
		w := oneRoleEach(c.in)
		for _, p := range [][2][]string{{w.AddCoOwners, w.RemoveCoOwners}, {w.AddEditors, w.RemoveEditors}, {w.AddViewers, w.RemoveViewers}, {w.AddCommenters, w.RemoveCommenters}} {
			if has(p[0]) && has(p[1]) {
				t.Errorf("%s: one write adds and removes them on the same role: %+v", c.name, w)
			}
		}
		keep := func(held bool, add, remove []string) bool { return has(add) || (held && !has(remove)) }
		got := roles{keep(c.held.co, w.AddCoOwners, w.RemoveCoOwners), keep(c.held.ed, w.AddEditors, w.RemoveEditors),
			keep(c.held.vi, w.AddViewers, w.RemoveViewers), keep(c.held.cm, w.AddCommenters, w.RemoveCommenters)}
		if got != c.want {
			t.Errorf("%s: %+v, want %+v (writes %+v)", c.name, got, c.want, w)
		}
	}
}
