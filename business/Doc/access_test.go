package businness

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The doc rules everything asks (opening a doc, its files, its comments, its
// history, the collaboration service, the assistant's read_doc).
func TestDocAccessRules(t *testing.T) {
	yes, no := true, false
	owner := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "owner-uuid"}
	me, myUUID := "0x2", "me-uuid"

	// The person asking is me (uid) / myUUID (uuid) throughout.
	for _, c := range []struct {
		name               string
		doc                *dgraphStruct.DgraphDoc
		read, edit, ownsIt bool
	}{
		{name: "no doc", doc: nil},
		{name: "a doc that isn't private, made by someone else",
			doc: &dgraphStruct.DgraphDoc{IsPrivate: &no, CreatedBy: owner}, read: true},
		{name: "a doc read without its privacy (as public)",
			doc: &dgraphStruct.DgraphDoc{CreatedBy: owner}, read: true},
		{name: "a private doc, made by someone else",
			doc: &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: owner}},
		{name: "a private doc shared with them to read",
			doc: &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: owner, HasReadAccess: 1}, read: true},
		{name: "a private doc shared with them to comment",
			doc: &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: owner, HasCommentAccess: 1}, read: true},
		{name: "a private doc shared with them to edit",
			doc: &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: owner, HasEditAccess: 1}, read: true, edit: true},
		{name: "their own private doc",
			doc:  &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uid: me, Uuid: myUUID}},
			read: true, edit: true, ownsIt: true},
		// The edit-access query returns the creator only to the creator.
		{name: "a private doc read for its editing access by someone else: no creator returned",
			doc: &dgraphStruct.DgraphDoc{IsPrivate: &yes}},
		{name: "read for its editing access by its creator",
			doc:  &dgraphStruct.DgraphDoc{CreatedBy: &dgraphStruct.DgraphUser{Uid: me, Uuid: myUUID}},
			read: true, edit: true, ownsIt: true},
		{name: "a creator with no id",
			doc: &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{}}},
	} {
		if got := CanRead(c.doc, myUUID); got != c.read {
			t.Errorf("%s: CanRead = %v, want %v", c.name, got, c.read)
		}
		if got := CanEdit(c.doc, me); got != c.edit {
			t.Errorf("%s: CanEdit = %v, want %v", c.name, got, c.edit)
		}
		if got := IsOwner(c.doc, me); got != c.ownsIt {
			t.Errorf("%s: IsOwner = %v, want %v", c.name, got, c.ownsIt)
		}
	}

	// Nobody is the owner of a doc whose creator has no id by having none.
	if IsOwner(&dgraphStruct.DgraphDoc{CreatedBy: &dgraphStruct.DgraphUser{}}, "") {
		t.Error("an empty id owns a doc whose creator has no id")
	}
}

// Who may comment on a doc as a member: someone who can read it, on a doc
// that isn't deleted, where it takes everyone's comments or they hold a grant.
func TestCanComment(t *testing.T) {
	yes, no := true, false
	live, deleted := time.Time{}, time.Now().Add(-time.Hour)
	owner := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "owner-uuid"}
	me := "me-uuid"
	doc := func(private, publicComment bool, deletedAt *time.Time) *dgraphStruct.DgraphDoc {
		p, c := no, no
		if private {
			p = yes
		}
		if publicComment {
			c = yes
		}
		return &dgraphStruct.DgraphDoc{IsPrivate: &p, PublicComment: &c, CreatedBy: owner, DeletedAt: deletedAt}
	}
	withGrant := func(d *dgraphStruct.DgraphDoc, read, comment, edit uint8) *dgraphStruct.DgraphDoc {
		d.HasReadAccess, d.HasCommentAccess, d.HasEditAccess = read, comment, edit
		return d
	}
	for _, c := range []struct {
		name string
		doc  *dgraphStruct.DgraphDoc
		want bool
	}{
		{"no doc", nil, false},
		{"a private doc open to comments, not shared with them", doc(true, true, &live), false},
		{"a private doc open to comments, shared with them to read", withGrant(doc(true, true, &live), 1, 0, 0), true},
		{"a private doc closed to comments, shared with them to read", withGrant(doc(true, false, &live), 1, 0, 0), false},
		{"a private doc shared with them to comment", withGrant(doc(true, false, &live), 0, 1, 0), true},
		{"a private doc shared with them to edit", withGrant(doc(true, false, &live), 0, 0, 1), true},
		{"a public doc open to comments", doc(false, true, &live), true},
		{"a public doc closed to comments", doc(false, false, &live), false},
		{"a public doc closed to comments, shared with them to comment", withGrant(doc(false, false, &live), 0, 1, 0), true},
		{"a public doc open to comments, read without its deletion time", doc(false, true, nil), true},
		{"a deleted doc open to comments", doc(false, true, &deleted), false},
		{"a deleted doc shared with them to edit", withGrant(doc(true, false, &deleted), 0, 0, 1), false},
		{"their own private doc", &dgraphStruct.DgraphDoc{IsPrivate: &yes, PublicComment: &no, DeletedAt: &live,
			CreatedBy: &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: me}}, true},
		{"a doc read without its comment setting", &dgraphStruct.DgraphDoc{IsPrivate: &no, CreatedBy: owner, DeletedAt: &live}, false},
	} {
		if got := CanComment(c.doc, me); got != c.want {
			t.Errorf("%s: CanComment = %v, want %v", c.name, got, c.want)
		}
	}
}
