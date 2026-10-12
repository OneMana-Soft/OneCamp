package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A deleted doc is said to be deleted only to someone it was there for: who
// could read it, or for a change, change it, while it was live. The refusal
// came before any grant was looked at, so an MCP client, or an agent's asker,
// who could never read a private doc learned that it existed and had been
// deleted. To anyone else a deleted doc now answers exactly as it did live,
// and the grants themselves are unchanged.
func TestADeletedDocIsDeletedOnlyToThoseItWasThereFor(t *testing.T) {
	const (
		author   = "11111111-1111-1111-1111-111111111111"
		reader   = "22222222-2222-2222-2222-222222222222"
		editor   = "33333333-3333-3333-3333-333333333333"
		outsider = "44444444-4444-4444-4444-444444444444"
		uid      = "0x2a"
	)
	private, public := true, false
	doc := func(isPrivate *bool) *dgraphStruct.DgraphDoc {
		return &dgraphStruct.DgraphDoc{IsPrivate: isPrivate, CreatedBy: user(author),
			ReadingUser: []*dgraphStruct.DgraphUser{user(reader)},
			EditingUser: []*dgraphStruct.DgraphUser{user(editor)}}
	}
	const (
		noGrant = "the originating person has no grant on this private doc"
		noEdit  = "the originating person may not edit this doc"
	)

	for _, c := range []struct {
		name       string
		doc        *dgraphStruct.DgraphDoc
		who        string
		read       bool
		liveAllows bool   // what they ask is theirs while the doc is live
		liveWhy    string // and why, or why not
	}{
		{"an outsider reading a private doc", doc(&private), outsider, true, false, noGrant},
		{"an outsider reading a doc whose privacy is unknown", doc(nil), outsider, true, false, noGrant},
		{"its reader", doc(&private), reader, true, true, "explicit grant on private doc"},
		{"its author", doc(&private), author, true, true, "explicit grant on private doc"},
		{"anyone reading a workspace-visible doc", doc(&public), outsider, true, true, "workspace-visible doc"},
		{"its reader changing it", doc(&private), reader, false, false, noEdit},
		{"an outsider changing a workspace-visible doc", doc(&public), outsider, false, false, noEdit},
		{"its editor changing it", doc(&private), editor, false, true, "doc editor"},
	} {
		t.Run(c.name, func(t *testing.T) {
			live := docReach(c.doc, c.who, uid, c.read, false)
			if live.Allow != c.liveAllows || live.Reason != c.liveWhy || (live.Allow && live.PrincipalDgraphUID != uid) {
				t.Fatalf("the live doc: %+v, want allowed=%v because %q", live, c.liveAllows, c.liveWhy)
			}
			gone := docReach(c.doc, c.who, uid, c.read, true)
			if gone.Allow {
				t.Fatalf("a deleted doc let them through: %+v", gone)
			}
			if c.liveAllows && gone.Reason != ReasonDocDeleted {
				t.Errorf("someone the doc was there for was told %q, want %q", gone.Reason, ReasonDocDeleted)
			}
			if !c.liveAllows && gone != live {
				t.Errorf("someone it wasn't there for was told %q, want what the live doc tells them, %q", gone.Reason, live.Reason)
			}
		})
	}
}
