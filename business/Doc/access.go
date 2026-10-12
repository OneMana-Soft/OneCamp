package businness

import (
	"context"
	"errors"

	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// ErrDocDeleted is a doc in the trash: deleted by its owner, or archived by
// the workspace's retention. Every read of it and every change to it is
// refused (410 to someone it was shared with, 404 to anyone else) until its
// owner or an admin restores it.
var ErrDocDeleted = domain.ErrDocDeleted

// goneFor is what a deleted doc answers someone: ErrDocDeleted when it was
// there for them, so they're told it was deleted, and ErrNotFound when it
// never was, which tells them nothing.
func goneFor(couldSee bool) error {
	if couldSee {
		return ErrDocDeleted
	}
	return ErrNotFound
}

// CheckDocLive says whether a doc is there to be opened: nil for a live doc,
// ErrNotFound for none, ErrDocDeleted for one in the trash, and a read that
// failed as it is. For the paths that hold a doc's id without reading the
// doc for anyone, a guest's link.
func CheckDocLive(ctx context.Context, docUUID string) error {
	doc, err := domain.GetDocDeletion(ctx, docUUID)
	switch {
	case errors.Is(err, ErrNotFound) || (err == nil && (doc == nil || doc.Uuid == "")):
		return ErrNotFound
	case err != nil:
		return err
	case IsDeleted(doc):
		return ErrDocDeleted
	}
	return nil
}

// IsDeleted is whether a doc is in the trash. A live doc carries the zero time
// (0001-01-01) as its deletion time, so having one is not being deleted: a
// deletion time after 1970 is, as in every doc query's filter. doc must be read
// with doc_deleted_at, or a deleted doc reads as live.
func IsDeleted(doc *dgraphStruct.DgraphDoc) bool {
	return doc != nil && helpers.IsSoftDeleted(doc.DeletedAt)
}

// CanRead is who may read a doc: anyone signed in, for a doc that isn't
// private; for a private one, its creator and the people it's shared with;
// nobody, once it is deleted. Opening a doc, its comments, its files and its
// live updates all ask this.
func CanRead(doc *dgraphStruct.DgraphDoc, userUUID string) bool {
	return CouldRead(doc, userUUID) && !IsDeleted(doc)
}

// CouldRead is CanRead with the doc's deletion set aside: whether the doc is,
// or was before it was deleted, there for the person. A deleted doc tells them
// it was deleted (410), and anyone else what any missing doc does (404). The
// rule itself is domain/Doc's, which the archive's restore list asks too.
func CouldRead(doc *dgraphStruct.DgraphDoc, userUUID string) bool {
	return domain.CouldRead(doc, userUUID)
}

// CanComment is who may comment on a doc as a member of the workspace:
// someone who can read it (so not on a deleted doc), when the doc takes
// comments from everyone who can read it or they may edit it, comment on it
// or made it. doc must be read with its deletion time (GetBasicDgraphDocByUUID
// reads it). Comments through a guest link are decided by the link's grant.
func CanComment(doc *dgraphStruct.DgraphDoc, userUUID string) bool {
	if !CanRead(doc, userUUID) {
		return false
	}
	if doc.PublicComment != nil && *doc.PublicComment {
		return true
	}
	return doc.HasEditAccess > 0 || doc.HasCommentAccess > 0 ||
		(doc.CreatedBy != nil && doc.CreatedBy.Uuid == userUUID)
}

// CanEdit is who may change a doc (its body, its title, its history): its
// creator and the people it's shared with to edit, and nobody once it is
// deleted. doc is read for the person (its edit count is theirs); userUID is
// their graph uid, which every doc query is asked with.
func CanEdit(doc *dgraphStruct.DgraphDoc, userUID string) bool {
	return CouldEdit(doc, userUID) && !IsDeleted(doc)
}

// CouldEdit is CanEdit with the doc's deletion set aside.
func CouldEdit(doc *dgraphStruct.DgraphDoc, userUID string) bool {
	return doc != nil && (doc.HasEditAccess > 0 || IsOwner(doc, userUID))
}

// IsOwner is whether the person made the doc, and so decides who sees it,
// who may comment and who may share it, inside the workspace or out. Some doc
// queries return the creator only when it is the person asking; comparing the
// ids is right for both kinds. Deleting a doc leaves it its owner's: the owner
// is who restores it.
func IsOwner(doc *dgraphStruct.DgraphDoc, userUID string) bool {
	return doc != nil && userUID != "" && doc.CreatedBy != nil && doc.CreatedBy.Uid == userUID
}
