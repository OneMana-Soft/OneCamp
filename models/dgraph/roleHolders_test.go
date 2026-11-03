package models

import (
	"reflect"
	"testing"
)

// A person on two lists holds the higher role, and ids read as the role
// writer stores them, in lower case.
func TestRoleHoldersKeepsEachPersonsHighestRole(t *testing.T) {
	got := RoleHolders(
		[]*DgraphUser{{Uuid: "KIM"}, nil, {Uuid: ""}},
		[]*DgraphUser{{Uuid: "kim"}, {Uuid: "sam"}},
		[]*DgraphUser{{Uuid: "sam"}, {Uuid: "lee"}},
	)
	want := map[string]string{"kim": "edit", "sam": "comment", "lee": "view"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RoleHolders = %v, want %v", got, want)
	}
}
