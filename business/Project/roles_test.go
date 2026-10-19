package business

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// fakeStores records each write to the rows and the graph, in order; a write
// whose name is in fail fails.
func fakeStores(t *testing.T, fail ...string) *[]string {
	t.Helper()
	sr, kr, ae, de, pp, im, ro := setRow, keepRows, addAdminEdge, dropAdminEdge, projectsPage, invalidateMemberships, rowsOf
	t.Cleanup(func() {
		setRow, keepRows, addAdminEdge, dropAdminEdge, projectsPage, invalidateMemberships, rowsOf = sr, kr, ae, de, pp, im, ro
	})
	var ops []string
	failing := func(op string) error {
		ops = append(ops, op)
		for _, f := range fail {
			if strings.HasPrefix(op, f) {
				return errors.New(op + " failed")
			}
		}
		return nil
	}
	setRow = func(_ context.Context, _, _ uuid.UUID, role string, _ uuid.UUID) error { return failing("row " + role) }
	addAdminEdge = func(context.Context, uuid.UUID, string) error { return failing("admin edge on") }
	dropAdminEdge = func(context.Context, string, string) error { return failing("admin edge off") }
	invalidateMemberships = func(context.Context, string) { ops = append(ops, "profile") }
	return &ops
}

// A change of role writes the row first, then the admin edge, so a write that
// fails halfway leaves the person with less, never more (Work Access takes
// the lower of the two).
func TestSetRoleWritesTheLowerFirst(t *testing.T) {
	person := &dgraphStruct.DgraphUser{Uid: "0xben", Uuid: uuid.NewString()}
	for _, c := range []struct {
		name      string
		adminEdge bool
		to        workAccess.Role
		fail      []string
		want      []string
		err       bool
	}{
		{"a member made a viewer", false, workAccess.RoleViewer, nil, []string{"row viewer"}, false},
		{"an admin made a member", true, workAccess.RoleMember, nil, []string{"row member", "admin edge off", "profile"}, false},
		{"an admin made a commenter", true, workAccess.RoleCommenter, nil, []string{"row commenter", "admin edge off", "profile"}, false},
		{"a member made an admin", false, workAccess.RoleAdmin, nil, []string{"row admin", "admin edge on", "profile"}, false},
		{"an admin kept one", true, workAccess.RoleAdmin, nil, []string{"row admin"}, false},
		{"the row can't be written", true, workAccess.RoleMember, []string{"row"}, []string{"row member"}, true},
		{"the edge can't be taken off", true, workAccess.RoleMember, []string{"admin edge off"}, []string{"row member", "admin edge off"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ops := fakeStores(t, c.fail...)
			err := SetRole(context.Background(), uuid.New(), uuid.New(), "0xproject", person, c.adminEdge, c.to)
			if (err != nil) != c.err || !reflect.DeepEqual(*ops, c.want) {
				t.Errorf("%v %v, want %v (error %v)", *ops, err, c.want, c.err)
			}
		})
	}
	ops := fakeStores(t)
	if err := SetRole(context.Background(), uuid.New(), uuid.New(), "0xproject", &dgraphStruct.DgraphUser{}, false, workAccess.RoleViewer); !errors.Is(err, ErrNotInProject) || len(*ops) != 0 {
		t.Errorf("someone with no graph node: %v %v", err, *ops)
	}
}

// The sweep settles each project toward the lower of its edges and its rows.
func TestTheParitySweepGoesTowardTheLower(t *testing.T) {
	ana, ben, cy, dee := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	user := func(id uuid.UUID, uid string) *dgraphStruct.DgraphUser {
		return &dgraphStruct.DgraphUser{Uid: uid, Uuid: id.String()}
	}
	p := &dgraphStruct.DgraphProject{
		Uid: "0xp", Uuid: uuid.NewString(),
		Members: []*dgraphStruct.DgraphUser{user(ana, "0xana"), user(ben, "0xben"), user(cy, "0xcy")},
		Admins:  []*dgraphStruct.DgraphUser{user(ana, "0xana"), user(cy, "0xcy")},
	}
	rows := map[uuid.UUID]string{
		ana: "admin",  // as the graph
		cy:  "viewer", // lower than its admin edge: the edge goes
		dee: "member", // off the project: the row goes
	}
	want, drop := settled(p, rows)
	if got := map[uuid.UUID]string{ana: "admin", ben: "member", cy: "viewer"}; !reflect.DeepEqual(want, got) {
		t.Errorf("rows: %v, want %v", want, got)
	}
	if !reflect.DeepEqual(drop, []string{"0xcy"}) {
		t.Errorf("admin edges taken off: %v", drop)
	}

	// The pass: every project of every page, and a project that fails is
	// skipped, not the pass.
	ops := fakeStores(t)
	broken := &dgraphStruct.DgraphProject{Uid: "0xq", Uuid: uuid.NewString()}
	projectsPage = func(_ context.Context, after string, _ int) ([]*dgraphStruct.DgraphProject, error) {
		if after != "" {
			t.Errorf("a second page was read after a short one (after %q)", after)
		}
		return []*dgraphStruct.DgraphProject{broken, p}, nil
	}
	rowsOf = func(_ context.Context, id uuid.UUID) (map[uuid.UUID]string, error) {
		if id.String() == broken.Uuid {
			return nil, errors.New("rows unreadable")
		}
		return rows, nil
	}
	var kept map[uuid.UUID]string
	keepRows = func(_ context.Context, id uuid.UUID, roles map[uuid.UUID]string) (int, error) {
		kept = roles
		*ops = append(*ops, fmt.Sprintf("keep %d", len(roles)))
		return 2, nil
	}
	changed, err := SettleRoles(context.Background())
	if err != nil || changed != 3 || !reflect.DeepEqual(*ops, []string{"admin edge off", "keep 3"}) || kept[cy] != "viewer" {
		t.Errorf("changed %d %v, ops %v, kept %v", changed, err, *ops, kept)
	}
}

// The member list shows each person's role as Work Access decides it.
func TestTheMemberListShowsTheLowerRole(t *testing.T) {
	ana, ben, cy := uuid.New(), uuid.New(), uuid.New()
	fakeStores(t)
	rowsOf = func(context.Context, uuid.UUID) (map[uuid.UUID]string, error) {
		return map[uuid.UUID]string{ana: "member", ben: "commenter", cy: "admin"}, nil
	}
	p := &dgraphStruct.DgraphProject{Uuid: uuid.NewString(), Members: []*dgraphStruct.DgraphUser{
		{Uuid: ana.String(), IsAdmin: true}, {Uuid: ben.String()}, {Uuid: cy.String()},
	}}
	if err := FillRoles(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range p.Members {
		got = append(got, m.ProjectRole)
	}
	// An admin edge with a member row is a member; a row above the edge is
	// the edge's.
	if want := []string{"member", "commenter", "member"}; !reflect.DeepEqual(got, want) {
		t.Errorf("roles %v, want %v", got, want)
	}
}
