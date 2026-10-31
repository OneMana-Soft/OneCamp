package business

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
)

// A board's read says, for each status with a rule on who moves tasks there,
// whether its reader may, as the gate answers a move: the project's admins by
// the reader's standing, named people by their id (an admin too), anyone
// always. A custom status's own rule is listed beside its category's, for the
// board to take the custom one first.
func TestMoveIntoSaysWhereItsReaderMayMoveTasks(t *testing.T) {
	old := listRules
	t.Cleanup(func() { listRules = old })
	ben := uuid.NewString()
	rules := []*model.StatusRule{
		{Status: "done", Who: "admins"},
		{Status: "inReview", Who: "people", People: []string{ben}},
		{Status: "canceled", Who: "people", People: []string{uuid.NewString()}},
		{Status: "c-shipped", Who: "anyone"},
	}
	var readErr error
	listRules = func(context.Context, uuid.UUID) ([]*model.StatusRule, error) { return rules, readErr }
	board := func(manage bool) map[string]bool {
		p := &dgraphStruct.DgraphProject{Uuid: uuid.NewString(), Access: &dgraphStruct.ProjectAccess{Manage: manage}}
		MoveInto(context.Background(), ben, p)
		return p.Access.MoveInto
	}

	if got, want := board(false), map[string]bool{"done": false, "inReview": true, "canceled": false, "c-shipped": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("ben, a member: %v, want %v", got, want)
	}
	if got := board(true); !got["done"] || got["canceled"] {
		t.Errorf("an admin: %v", got)
	}
	// No rules, or a read that fails: nothing said, and the server still
	// refuses such a move in words.
	rules = nil
	if got := board(false); got != nil {
		t.Errorf("a project without rules: %v", got)
	}
	rules, readErr = []*model.StatusRule{{Status: "done", Who: "admins"}}, errors.New("down")
	if got := board(false); got != nil {
		t.Errorf("a failed read: %v", got)
	}
}
