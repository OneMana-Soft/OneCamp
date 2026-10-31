package businness

// The doc tree's nodes that aren't pages (docs F1, Docs & Files): a folder, a
// file or a link. domain/Doc docNodeKind.go says what each is and what follows
// from it; these are its answers for the packages above business/Doc.

import (
	"context"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The kinds a request may make through POST /doc/createDoc: a folder or a
// link. A file is made through POST /doc/files (AddFile).
const (
	NodeFolder = domain.NodeFolder
	NodeLink   = domain.NodeLink
)

// ErrNotAPage is something only a page does (the editor's session, a save, a
// live body) asked of a folder, a file or a link.
var ErrNotAPage = domain.ErrNotAPage

// IsPage is whether a doc is a page: it has no kind. doc must be read with
// doc_node_kind.
func IsPage(doc *dgraphStruct.DgraphDoc) bool {
	return domain.IsPage(doc)
}

// IsLink is whether a doc is a link. doc must be read with doc_node_kind.
func IsLink(doc *dgraphStruct.DgraphDoc) bool {
	return doc != nil && doc.NodeKind == domain.NodeLink
}

// CheckLinkURL is a link's address as it is kept, or domain.ErrBadLink: a
// public https address only (domain/Doc CheckLinkURL).
func CheckLinkURL(raw string) (string, error) {
	return domain.CheckLinkURL(raw)
}

// NameLink names a link after its preview (title), only while it is still
// called by its address: a rename someone made meanwhile is left as it is.
// A rename is told, and the link's search entry follows it.
func NameLink(ctx context.Context, docUUID, address, title string, by Actor) error {
	renamed, err := domain.RetitleLink(ctx, docUUID, address, title)
	if err != nil || !renamed {
		return err
	}
	syncDocSearch(docUUID, true)
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind: resourceEventBusiness.DocRenamed, ResourceKind: "doc", ResourceUUID: docUUID, Actor: by, At: time.Now(),
		Data: map[string]string{"title": title},
	})
	return nil
}

// IsFile is whether a doc is a file. doc must be read with doc_node_kind.
func IsFile(doc *dgraphStruct.DgraphDoc) bool {
	return doc != nil && doc.NodeKind == domain.NodeFile
}
