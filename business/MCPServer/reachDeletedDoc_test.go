package business

import (
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A deleted doc is said to be deleted only to someone it was there for: who
// could read it, or for a change, change it, while it was live. The refusal
// came before any grant was looked at, so an MCP client, or an agent's asker,
// who could never read a private doc learned that it existed and had been
// deleted. To anyone else a deleted doc now answers exactly as it did live,
// and the grants themselves are unchanged.
//
// The decisions are the Access Resolver's rule (business/Doc DecideDoc), on a
// doc as a read of it for each person carries it: what docReach gets in
// production from Rules.Resolve.
func TestADeletedDocIsDeletedOnlyToThoseItWasThereFor(t *testing.T) {
	const (
		author   = "11111111-1111-1111-1111-111111111111"
		reader   = "22222222-2222-2222-2222-222222222222"
		editor   = "33333333-3333-3333-3333-333333333333"
		outsider = "44444444-4444-4444-4444-444444444444"
		uid      = "0x2a"
	)
	private, public := true, false
	// The doc as read for who: its privacy, its author, and their grant.
	decide := func(isPrivate *bool, who string, deleted bool) accessBusiness.Decision {
		doc := &dgraphStruct.DgraphDoc{IsPrivate: isPrivate, CreatedBy: &dgraphStruct.DgraphUser{Uuid: author}}
		switch who {
		case reader:
			doc.HasReadAccess = 1
		case editor:
			doc.HasEditAccess = 1
		}
		if deleted {
			at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			doc.DeletedAt = &at
		}
		d := docBusiness.DecideDoc(doc, docBusiness.Viewer{UUID: who})
		out := accessBusiness.Decision{Level: d.Level, Was: d.Was, Deleted: d.Deleted, Owner: d.Owner}
		for _, r := range d.Reasons {
			out.Reasons = append(out.Reasons, string(r))
		}
		return out
	}
	const (
		noGrant = "the originating person has no grant on this private doc"
		noEdit  = "the originating person may not edit this doc"
	)
	for _, c := range []struct {
		name       string
		isPrivate  *bool
		who        string
		read       bool
		liveAllows bool   // what they ask is theirs while the doc is live
		liveWhy    string // and why, or why not
	}{
		{"an outsider reading a private doc", &private, outsider, true, false, noGrant},
		// A doc whose privacy was never set reads as the workspace's, for
		// people and agents alike: one rule (opening it, commenting on it and
		// search read it so too).
		{"an outsider reading a doc whose privacy is unknown", nil, outsider, true, true, "workspace-visible doc"},
		{"its reader", &private, reader, true, true, "explicit grant on private doc"},
		{"its author", &private, author, true, true, "explicit grant on private doc"},
		{"anyone reading a workspace-visible doc", &public, outsider, true, true, "workspace-visible doc"},
		{"its reader changing it", &private, reader, false, false, noEdit},
		{"an outsider changing a workspace-visible doc", &public, outsider, false, false, noEdit},
		{"its editor changing it", &private, editor, false, true, "doc editor"},
	} {
		t.Run(c.name, func(t *testing.T) {
			live := docReach(decide(c.isPrivate, c.who, false), uid, c.read)
			if live.Allow != c.liveAllows || live.Reason != c.liveWhy || (live.Allow && live.PrincipalDgraphUID != uid) {
				t.Fatalf("the live doc: %+v, want allowed=%v because %q", live, c.liveAllows, c.liveWhy)
			}
			gone := docReach(decide(c.isPrivate, c.who, true), uid, c.read)
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
