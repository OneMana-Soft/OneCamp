package businness

import (
	"context"
	"errors"

	"github.com/akashc777/OneCamp/business/Access/level"
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

// CheckPageLive says whether a page is there to be opened live: nil for a
// live page, ErrNotFound for none, ErrDocDeleted for one in the trash,
// ErrNotAPage for a folder, a file or a link (docs F1: only a page opens in
// the editor), and a read that failed as it is. For the paths that hold a
// doc's id without reading the doc for anyone: a guest's live connection.
func CheckPageLive(ctx context.Context, docUUID string) error {
	doc, err := domain.GetDocDeletion(ctx, docUUID)
	switch {
	case errors.Is(err, ErrNotFound) || (err == nil && (doc == nil || doc.Uuid == "")):
		return ErrNotFound
	case err != nil:
		return err
	case IsDeleted(doc):
		return ErrDocDeleted
	case !IsPage(doc):
		return ErrNotAPage
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

// CanRead is who may read a doc: anyone it is open to by the access rule
// (docAccessDecide.go), nobody once it is deleted. Opening a doc, its
// comments, its files and its live updates all ask this. doc must be read for
// the person with the access facts (domain/Doc docAccess.go).
func CanRead(doc *dgraphStruct.DgraphDoc, userUUID string) bool {
	return DecideDoc(doc, Viewer{UUID: userUUID}).Level >= level.View
}

// CouldRead is CanRead with the doc's deletion set aside: whether the doc is,
// or was before it was deleted, there for the person. A deleted doc tells them
// it was deleted (410), and anyone else what any missing doc does (404). The
// rule itself is domain/Doc's, which the archive's restore list asks too.
func CouldRead(doc *dgraphStruct.DgraphDoc, userUUID string) bool {
	return DecideDoc(doc, Viewer{UUID: userUUID}).Was >= level.View
}

// CanComment is who may comment on a doc as a member of the workspace: the
// people the access rule gives comment or more (so not on a deleted doc). doc
// must be read with the access facts. Comments through a guest link are
// decided by the link's grant.
func CanComment(doc *dgraphStruct.DgraphDoc, userUUID string) bool {
	return DecideDoc(doc, Viewer{UUID: userUUID}).Level >= level.Comment
}

// CanEdit is who may change a doc (its body, its title, its history): the
// people the access rule gives edit or more, which a hard lock holds below
// full, and nobody once it is deleted. doc is read for the person (its access
// facts are theirs); userUID is their graph uid, which every doc query is
// asked with.
func CanEdit(doc *dgraphStruct.DgraphDoc, userUID string) bool {
	return DecideDoc(doc, Viewer{UID: userUID}).Level >= level.Edit
}

// CouldEdit is CanEdit with the doc's deletion set aside.
func CouldEdit(doc *dgraphStruct.DgraphDoc, userUID string) bool {
	return DecideDoc(doc, Viewer{UID: userUID}).Was >= level.Edit
}

// IsOwner is whether the person owns the doc, and so may share it outside the
// workspace, hand it on, and delete it for good. Its owner is whoever it was
// handed to last (doc_owner), or for a doc never handed on, its maker.
// Deleting a doc leaves it its owner's: the owner is who restores it.
func IsOwner(doc *dgraphStruct.DgraphDoc, userUID string) bool {
	return doc != nil && userUID != "" && DecideDoc(doc, Viewer{UID: userUID}).Owner
}

// What bringing a doc back from the trash or the archive, and deleting it for
// good, take of the level someone had before it was deleted (Was). CanRestore,
// CanPurge and the trash's handlers (accessBusiness.OpenDeleted) all ask
// these, so the doc page's 410, the trash list and the trash's answers agree.
const (
	RestoreNeeds = level.Edit
	PurgeNeeds   = level.Full
)

// CanRestore is who may bring a doc back from the trash or the archive: the
// people who could edit it before it was deleted (its owner, full access, and
// the people, teams and space it gave edit, by the rule's level before the
// deletion), and a workspace admin. The trash's restore and the doc page's
// 410 (can_restore) both ask this, so the page offers Restore to exactly the
// people the restore lets in. doc is read for the person with the access facts.
func CanRestore(doc *dgraphStruct.DgraphDoc, v Viewer, isAdmin bool) bool {
	return isAdmin || DecideDoc(doc, v).Was >= RestoreNeeds
}

// CanPurge is who may delete a doc for good: its owner, the people with full
// access to it, and a workspace admin.
func CanPurge(doc *dgraphStruct.DgraphDoc, v Viewer, isAdmin bool) bool {
	return isAdmin || DecideDoc(doc, v).Was >= PurgeNeeds
}
