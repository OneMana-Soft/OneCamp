package domain

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Each person is read as their own node, the lowest uid holding their uuid,
// whatever order the copies an import made come back in; a node with no uuid
// or no uid is no one.
func TestEachPersonIsTheirOwnNodeNotAnImportsCopy(t *testing.T) {
	got := ownNodes([]*dgraphStruct.DgraphUser{
		{Uid: "0x1f", Uuid: "bo"},
		{Uid: "0x9", Uuid: "bo", UserName: "Bo"},
		{Uid: "0x10", Uuid: "bo"},
		{Uid: "0x2", Uuid: "ana", UserName: "Ana"},
		{Uid: "0x3"}, {Uuid: "cy"}, nil,
	})
	if len(got) != 2 || got["bo"].Uid != "0x9" || got["bo"].UserName != "Bo" || got["ana"].Uid != "0x2" {
		t.Fatalf("got %d people: bo %+v, ana %+v; want Bo's 0x9 and Ana's 0x2", len(got), got["bo"], got["ana"])
	}
}
