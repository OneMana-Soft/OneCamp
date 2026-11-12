package business

// A board is a place (business/Places, miro-46): a person stars it, and it is
// among their recent boards once they open it (RecordBoardView writes the view
// log). Its resolver names only the boards the person may read now, by
// CanRead, with their titles and their page, so a star on a board that was
// deleted, or made private without them, drops out of every list.

import (
	"context"
	"strings"

	placesBusiness "github.com/akashc777/OneCamp/business/Places"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// boardPlaces resolves boards for places.
type boardPlaces struct{}

// Resolve answers, of the named boards, those the person may read, keyed by
// the id asked for: one graph query for the lot (readBoardsAccess). An error
// is the graph not answering, never "none of them".
func (boardPlaces) Resolve(ctx context.Context, who placesBusiness.Person, ids []string) (map[string]placesBusiness.Seen, error) {
	boards, err := readBoardsAccess(ctx, ids, who.UUID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*dgraphStruct.DgraphBoard, len(boards))
	for _, b := range boards {
		if b != nil && b.Uuid != "" {
			byID[strings.ToLower(b.Uuid)] = b
		}
	}
	out := make(map[string]placesBusiness.Seen, len(ids))
	for _, id := range ids {
		if b := byID[strings.ToLower(id)]; CanRead(b, who.UUID) {
			out[id] = placesBusiness.Seen{Title: b.Title, Href: "/app/board/" + b.Uuid}
		}
	}
	return out, nil
}

func init() {
	placesBusiness.Register(KindBoard, boardPlaces{})
}
