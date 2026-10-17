package businness

// The AI edition's part of a change to who may open a doc: its AI entries
// (embeddings of the doc and its comments) carry its sharing, written again
// whole after every change (I13). AI only: without-ai drops this file, and
// RepairAccessIndexes then writes the search entries alone.

import (
	"context"
	"sort"

	"github.com/akashc777/OneCamp/business/Access/level"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func init() {
	aiAccessEntries = func(ctx context.Context, s *Sharing) error {
		_, err := ai.SetDocAccess(ctx, []ai.DocAccess{aiAccessOf(s)})
		return err
	}
}

// aiAccessOf is a doc's sharing as the AI index carries it: its owner, and the
// people it is shared with by name (full access with the editors). People who
// reach it through a team or its space don't find it in AI search until that
// index carries teams and spaces; search fails closed for them.
func aiAccessOf(s *Sharing) ai.DocAccess {
	a := ai.DocAccess{DocUUID: s.DocUUID, Private: s.General.Private, Reading: []string{}, Editing: []string{}, Commenting: []string{}}
	if s.Owner != nil {
		a.CreatedBy = s.Owner.Uuid
	}
	ids := make([]string, 0, len(s.People))
	for id := range s.People {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		switch s.People[id] {
		case level.Full, level.Edit:
			a.Editing = append(a.Editing, id)
		case level.Comment:
			a.Commenting = append(a.Commenting, id)
		case level.View:
			a.Reading = append(a.Reading, id)
		}
	}
	return a
}
