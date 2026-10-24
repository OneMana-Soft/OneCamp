package business

import (
	"context"
	"fmt"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// keepOwnFiles is the files of list the writer uploaded to the place (its kind
// and id), in order, each once, with the key its upload stored: the rule every
// write of a request's files keeps (business/Attachment OwnFiles), since a
// file is served through its place (business/FileAccess) and any other entry
// names a file the message's people may not have. A failed lookup is a fault,
// so the send is tried again rather than written without its files.
func keepOwnFiles(ctx context.Context, user *userModels.UserInfo, kind, place string, list []*dgraphStruct.DgraphAttachment) ([]*dgraphStruct.DgraphAttachment, error) {
	kept, err := attachmentBusiness.OwnFiles(ctx, user.UserPostgresInfo.Id.String(), kind, place, list)
	if err != nil {
		return nil, fmt.Errorf("reading which files are the writer's: %w", err)
	}
	return kept, nil
}
