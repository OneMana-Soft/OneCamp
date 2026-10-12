package businness

import (
	"testing"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Doc"
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

// A deleted doc is read by nobody and changed by nobody, whoever it was shared
// with, and stays its owner's: the owner is who restores it. Before this, a
// doc in the trash could be opened, edited, joined live and read by the
// assistant by everyone it had been shared with (the rules asked nothing about
// deletion, and most doc queries didn't read it).
func TestADeletedDocIsReadAndChangedByNobody(t *testing.T) {
	yes, no := true, false
	deleted, live := time.Now().Add(-time.Minute), time.Time{}
	epoch := time.Unix(0, 0).UTC() // the graph's own zero for a datetime: not deleted
	me, myUUID := "0x2", "me-uuid"
	mine := &dgraphStruct.DgraphUser{Uid: me, Uuid: myUUID}
	someoneElse := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "owner-uuid"}

	for _, c := range []struct {
		name                 string
		doc                  *dgraphStruct.DgraphDoc
		couldRead, couldEdit bool
	}{
		{"their own deleted doc", &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: mine, DeletedAt: &deleted}, true, true},
		{"a deleted doc shared with them to edit", &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: someoneElse, HasEditAccess: 1, DeletedAt: &deleted}, true, true},
		{"a deleted doc shared with them to read", &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: someoneElse, HasReadAccess: 1, DeletedAt: &deleted}, true, false},
		{"a deleted doc that wasn't private", &dgraphStruct.DgraphDoc{IsPrivate: &no, CreatedBy: someoneElse, DeletedAt: &deleted}, true, false},
		{"a deleted private doc never shared with them", &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: someoneElse, DeletedAt: &deleted}, false, false},
	} {
		if !IsDeleted(c.doc) {
			t.Errorf("%s: IsDeleted = false", c.name)
		}
		if CanRead(c.doc, myUUID) {
			t.Errorf("%s: CanRead = true for a deleted doc", c.name)
		}
		if CanEdit(c.doc, me) {
			t.Errorf("%s: CanEdit = true for a deleted doc", c.name)
		}
		if CanComment(c.doc, myUUID) {
			t.Errorf("%s: CanComment = true for a deleted doc", c.name)
		}
		// Who it answers "it was deleted" (410) rather than "no such doc" (404).
		if got := CouldRead(c.doc, myUUID); got != c.couldRead {
			t.Errorf("%s: CouldRead = %v, want %v", c.name, got, c.couldRead)
		}
		if got := CouldEdit(c.doc, me); got != c.couldEdit {
			t.Errorf("%s: CouldEdit = %v, want %v", c.name, got, c.couldEdit)
		}
	}
	if !IsOwner(&dgraphStruct.DgraphDoc{CreatedBy: mine, DeletedAt: &deleted}, me) {
		t.Error("deleting a doc took it from its owner, who is who restores it")
	}

	// Live is the zero time, the graph's epoch, or no deletion time read at all.
	for _, at := range []*time.Time{&live, &epoch, nil} {
		doc := &dgraphStruct.DgraphDoc{IsPrivate: &yes, CreatedBy: mine, DeletedAt: at}
		if IsDeleted(doc) || !CanRead(doc, myUUID) || !CanEdit(doc, me) {
			t.Errorf("a live doc (deleted at %v) reads as deleted", at)
		}
	}
	if IsDeleted(nil) {
		t.Error("no doc is a deleted doc")
	}
}

// domain.ReadableBy decides from a doc's sharing lists what CanRead decides
// from the counts a doc query reads for one person: who else may be told
// about it (a new comment's notifications).
func TestReadableByAgreesWithCanRead(t *testing.T) {
	yes, no := true, false
	deleted := time.Now().Add(-time.Minute)
	owner := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "owner"}
	reader := &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "reader"}
	editor := &dgraphStruct.DgraphUser{Uid: "0x3", Uuid: "editor"}
	commenter := &dgraphStruct.DgraphUser{Uid: "0x4", Uuid: "commenter"}
	stranger := &dgraphStruct.DgraphUser{Uid: "0x5", Uuid: "stranger"}
	people := []*dgraphStruct.DgraphUser{owner, reader, editor, commenter, stranger}

	shared := func(private *bool, deletedAt *time.Time) *dgraphStruct.DgraphDoc {
		return &dgraphStruct.DgraphDoc{IsPrivate: private, CreatedBy: owner, DeletedAt: deletedAt,
			ReadingUser: []*dgraphStruct.DgraphUser{reader}, EditingUser: []*dgraphStruct.DgraphUser{owner, editor},
			CommentingUser: []*dgraphStruct.DgraphUser{commenter}}
	}
	// The same doc as a query reads it for one person: their own counts.
	readFor := func(d *dgraphStruct.DgraphDoc, p *dgraphStruct.DgraphUser) *dgraphStruct.DgraphDoc {
		cp := *d
		count := func(list []*dgraphStruct.DgraphUser) uint8 {
			for _, u := range list {
				if u.Uuid == p.Uuid {
					return 1
				}
			}
			return 0
		}
		cp.HasReadAccess, cp.HasEditAccess, cp.HasCommentAccess = count(d.ReadingUser), count(d.EditingUser), count(d.CommentingUser)
		return &cp
	}
	for _, d := range []*dgraphStruct.DgraphDoc{shared(&yes, nil), shared(&no, nil), shared(nil, nil), shared(&yes, &deleted), shared(&no, &deleted)} {
		for _, p := range people {
			if got, want := domain.ReadableBy(d, p.Uuid), CanRead(readFor(d, p), p.Uuid); got != want {
				t.Errorf("private=%v deleted=%v, %s: ReadableBy = %v, CanRead = %v", d.IsPrivate, d.DeletedAt != nil, p.Uuid, got, want)
			}
		}
	}
	if domain.ReadableBy(shared(&no, nil), "") {
		t.Error("nobody (an empty id) may be told about a doc")
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
