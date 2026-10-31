package business

import (
	"context"
	"strings"

	domain "github.com/akashc777/OneCamp/domain/Attachment"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// ownUploads is where OwnUploads asks which files are whose: a seam.
var ownUploads = domain.OwnUploads

// Uploads are, by id, the files of a list its writer uploaded, each with the
// place its upload went to and the key it stored (OwnUploads).
type Uploads map[uuid.UUID]domain.Upload

// OwnUploads is, of list, the files the writer (their account id) uploaded,
// with where each went, in one query: a write that sends one list to several
// places (a forward) reads it once and keeps each place's with In. Nothing to
// ask about asks nothing. An error is the lookup's: the write should not go
// ahead.
func OwnUploads(ctx context.Context, writer string, list []*dgraphStruct.DgraphAttachment) (Uploads, error) {
	who, err := uuid.Parse(writer)
	if err != nil {
		return Uploads{}, nil
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
	if len(ids) == 0 {
		return Uploads{}, nil
	}
	return ownUploads(ctx, ids, who)
}

// In is the files of list uploaded to the place srcKey/srcValue, in list's
// order, each once and with the storage key its upload stored; any other is
// left out.
func (u Uploads) In(srcKey, srcValue string, list []*dgraphStruct.DgraphAttachment) []*dgraphStruct.DgraphAttachment {
	srcValue = strings.ToLower(srcValue)
	return KeepStored(list, func(id uuid.UUID) (string, bool) {
		up, ok := u[id]
		return up.ObjKey, ok && up.SrcKey == srcKey && up.SrcValue == srcValue
	})
}

// KeepStored is, of list, the files stored says a row holds, in list's order
// and each once, each on the storage key stored gives: its row's. A message
// lists its files as its writer sent them; a write keeps only what a row,
// written at upload, says, on that row's object. Any other is left out.
func KeepStored(list []*dgraphStruct.DgraphAttachment, stored func(id uuid.UUID) (objKey string, ok bool)) []*dgraphStruct.DgraphAttachment {
	out := make([]*dgraphStruct.DgraphAttachment, 0, len(list))
	seen := map[uuid.UUID]bool{}
	for _, f := range list {
		if f == nil {
			continue
		}
		id, err := uuid.Parse(f.Uuid)
		if err != nil || seen[id] {
			continue
		}
		key, ok := stored(id)
		if !ok {
			continue
		}
		seen[id] = true
		f.Uuid, f.ObjectKey = id.String(), key
		out = append(out, f)
	}
	return out
}

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
	own, err := OwnUploads(ctx, writer, list)
	if err != nil {
		return nil, err
	}
	return own.In(srcKey, srcValue, list), nil
}
