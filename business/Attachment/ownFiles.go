package business

import (
	"context"
	"strings"

	domain "github.com/akashc777/OneCamp/domain/Attachment"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// ownUploadsIn is where OwnFiles asks which files are whose: a seam.
var ownUploadsIn = domain.OwnUploadsIn

// OwnFiles is the files of list that the writer (their account id) uploaded
// to the place srcKey/srcValue, in list's order, each once and with the
// storage key its upload stored; any other is left out. A file is served
// through its place (business/FileAccess), so an entry naming someone else's
// file, the writer's file of another place, or an id nobody uploaded opened
// for no one: it is not written. One query for the lot; nothing to ask about
// asks nothing. An error is the lookup's: the write should not go ahead.
func OwnFiles(ctx context.Context, writer, srcKey, srcValue string, list []*dgraphStruct.DgraphAttachment) ([]*dgraphStruct.DgraphAttachment, error) {
	if len(list) == 0 {
		return list, nil
	}
	who, err := uuid.Parse(writer)
	if err != nil {
		return nil, nil
	}
	ids := make([]uuid.UUID, 0, len(list))
	for _, f := range list {
		if f == nil {
			continue
		}
		if id, err := uuid.Parse(f.Uuid); err == nil {
			ids = append(ids, id)
		}
	}
	keys, err := ownUploadsIn(ctx, ids, who, srcKey, strings.ToLower(srcValue))
	if err != nil {
		return nil, err
	}
	out := make([]*dgraphStruct.DgraphAttachment, 0, len(keys))
	seen := map[uuid.UUID]bool{}
	for _, f := range list {
		if f == nil {
			continue
		}
		id, err := uuid.Parse(f.Uuid)
		if err != nil || seen[id] {
			continue
		}
		key, ok := keys[id]
		if !ok {
			continue
		}
		seen[id] = true
		f.Uuid, f.ObjectKey = id.String(), key
		out = append(out, f)
	}
	return out, nil
}
