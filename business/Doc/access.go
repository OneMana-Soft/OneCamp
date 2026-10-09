package businness

import (
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// CanRead is who may read a doc: anyone signed in, for a doc that isn't
// private; for a private one, its creator and the people it's shared with.
// Opening a doc, its comments and its live updates all ask this.
func CanRead(doc *dgraphStruct.DgraphDoc, userUUID string) bool {
	if doc == nil {
		return false
	}
	if doc.IsPrivate == nil || !*doc.IsPrivate {
		return true
	}
	return doc.HasEditAccess > 0 || doc.HasReadAccess > 0 || doc.HasCommentAccess > 0 ||
		(doc.CreatedBy != nil && doc.CreatedBy.Uuid == userUUID)
}

// CanComment is who may comment on a doc as a member of the workspace:
// someone who can read it, on a doc that isn't deleted, when the doc takes
// comments from everyone who can read it or they may edit it, comment on it
// or made it. doc must be read with its deletion time (GetBasicDgraphDocByUUID
// reads it). Comments through a guest link are decided by the link's grant.
func CanComment(doc *dgraphStruct.DgraphDoc, userUUID string) bool {
	if !CanRead(doc, userUUID) || helpers.IsSoftDeleted(doc.DeletedAt) {
		return false
	}
	if doc.PublicComment != nil && *doc.PublicComment {
		return true
	}
	return doc.HasEditAccess > 0 || doc.HasCommentAccess > 0 ||
		(doc.CreatedBy != nil && doc.CreatedBy.Uuid == userUUID)
}

// CanEdit is who may change a doc (its body, its title, its history): its
// creator and the people it's shared with to edit. doc is read for the person
// (its edit count is theirs); userUID is their graph uid, which every doc
// query is asked with.
func CanEdit(doc *dgraphStruct.DgraphDoc, userUID string) bool {
	return doc != nil && (doc.HasEditAccess > 0 || IsOwner(doc, userUID))
}

// IsOwner is whether the person made the doc, and so decides who sees it and
// who may comment. Some doc queries return the creator only when it is the
// person asking; comparing the ids is right for both kinds.
func IsOwner(doc *dgraphStruct.DgraphDoc, userUID string) bool {
	return doc != nil && userUID != "" && doc.CreatedBy != nil && doc.CreatedBy.Uid == userUID
}
