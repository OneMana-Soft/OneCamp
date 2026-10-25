package models

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	roleBoard = "3f1c2a9e-8b7d-4c6e-9a5f-1d2e3c4b5a69"
	roleEd    = "7a8b9c0d-1e2f-4a3b-8c5d-6e7f8a9b0c1d"
	roleVi    = "0b1c2d3e-4f5a-4b6c-8d7e-9f0a1b2c3d4e"
)

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A role change's ids reach Dgraph only as variables: the query's text holds
// none of them. Each person is named once, as uuids are stored, and their
// edges go in one mutation that runs only when the node and they exist.
func TestARoleChangeNamesItsPeopleByVariablesOnly(t *testing.T) {
	q, vars, muts, err := roleUpsert("board_uuid", roleBoard, []RoleChange{
		{Predicate: "board_editing_users", Add: []string{roleEd, "", strings.ToUpper(roleEd)}},
		{Predicate: "board_reading_users", Remove: []string{roleEd, roleVi}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{roleBoard, roleEd, roleVi} {
		if strings.Contains(strings.ToLower(q), id) {
			t.Errorf("the query's text holds %s:\n%s", id, q)
		}
	}
	if len(vars) != 3 || vars["$node"] != roleBoard || vars["$p0"] != roleEd || vars["$p1"] != roleVi {
		t.Errorf("vars %v: want the board and each person once, in lower case", vars)
	}
	if len(muts) != 2 {
		t.Fatalf("%d mutations, want one for each person", len(muts))
	}
	ed, vi := muts[0], muts[1]
	if ed.Cond != "@if(eq(len(node), 1) AND gt(len(p0), 0))" || vi.Cond != "@if(eq(len(node), 1) AND gt(len(p1), 0))" {
		t.Errorf("conditions %q and %q: each person's edges need the node and that person", ed.Cond, vi.Cond)
	}
	if got := asJSON(t, ed.Set); got != `{"board_editing_users":[{"uid":"uid(p0)"}],"uid":"uid(node)"}` {
		t.Errorf("Ed's edit: %s", got)
	}
	if got := asJSON(t, ed.Delete); got != `{"board_reading_users":[{"uid":"uid(p0)"}],"uid":"uid(node)"}` {
		t.Errorf("Ed off reading: %s", got)
	}
	if vi.Set != nil {
		t.Errorf("Vi is only taken off, but is given %s", asJSON(t, vi.Set))
	}
}

// Anything that isn't a uuid is refused before anything is built.
func TestARoleChangeRefusesWhatIsNotAUUID(t *testing.T) {
	for name, c := range map[string]struct {
		node    string
		changes []RoleChange
	}{
		"a node that isn't a uuid": {"board-1", nil},
		"a person who isn't":       {roleBoard, []RoleChange{{Predicate: "board_editing_users", Add: []string{"not-a-uuid"}}}},
		"one among people who are": {roleBoard, []RoleChange{{Predicate: "board_reading_users", Remove: []string{roleEd, "12345"}}}},
	} {
		if _, _, _, err := roleUpsert("board_uuid", c.node, c.changes); !errors.Is(err, ErrRoleIDs) {
			t.Errorf("%s: %v, want ErrRoleIDs", name, err)
		}
	}
}

// A change that names nobody writes nothing and never reaches the graph
// (there is no graph client in this test).
func TestARoleChangeWithNobodyInItTouchesNothing(t *testing.T) {
	if err := ChangeRoles(context.Background(), "board_uuid", roleBoard, []RoleChange{{Predicate: "board_editing_users", Add: []string{""}}}); err != nil {
		t.Fatal(err)
	}
}
