package businness

// "Viewed by" for documents. Recording requires the viewer to be able to access
// the doc; listing viewers is restricted to the owner and editors (viewers and
// commenters cannot see who else has viewed). Delegates to the shared
// ResourceView business once the doc-specific permission check passes.

import (
	"context"
	"errors"

	resourceViewBusiness "github.com/akashc777/OneCamp/business/ResourceView"
	domain "github.com/akashc777/OneCamp/domain/Doc"
)

// RecordDocView records (deduplicated, throttled) that a user opened the doc.
func RecordDocView(ctx context.Context, docUUID, userUID, userUUID string) error {
	doc, err := domain.GetBasicDgraphDocByUUID(ctx, docUUID, userUID)
	if err != nil {
		return err
	}
	if doc == nil {
		return errors.New("doc not found")
	}
	if !CanRead(doc, userUUID) {
		return errors.New("unauthorized")
	}
	return resourceViewBusiness.RecordView(ctx, resourceViewBusiness.ResourceDoc, docUUID, userUUID)
}

// ListDocViewers returns one page of the doc's distinct viewers (most-recent
// first). Restricted to the owner and editors.
func ListDocViewers(ctx context.Context, docUUID, userUID string, limit, offset int) (*resourceViewBusiness.ViewersPage, error) {
	doc, err := domain.GetBasicDgraphDocByUUID(ctx, docUUID, userUID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, errors.New("doc not found")
	}
	if !CanEdit(doc, userUID) {
		return nil, errors.New("unauthorized")
	}
	return resourceViewBusiness.ListViewersResolved(ctx, resourceViewBusiness.ResourceDoc, docUUID, limit, offset)
}
