package business

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	placesBusiness "github.com/akashc777/OneCamp/business/Places"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A board is a place (miro-46): its resolver names only the boards the person
// may read now, by CanRead, with their titles and their page, keyed by the id
// asked for; a private board not shared with them, a deleted one, or one
// that isn't there is left out, so a star on it drops out of every list.
func TestBoardPlacesNameOnlyTheBoardsThePersonMayRead(t *testing.T) {
	yes, no := true, false
	deleted := time.Now().Add(-time.Hour)
	var asked []string
	var askedFor string
	old := readBoardsAccess
	t.Cleanup(func() { readBoardsAccess = old })
	readBoardsAccess = func(_ context.Context, ids []string, userUUID string) ([]*dgraphStruct.DgraphBoard, error) {
		asked, askedFor = ids, userUUID
		return []*dgraphStruct.DgraphBoard{
			{Uuid: "aaaa-open", Title: "Roadmap", IsPrivate: &no},
			{Uuid: "aaaa-mine", Title: "Offsite", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "me"}},
			{Uuid: "aaaa-shared", Title: "Retro", IsPrivate: &yes, HasCommentAccess: 1},
			{Uuid: "aaaa-private", Title: "Layoffs", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "them"}},
			{Uuid: "aaaa-binned", Title: "Old plan", IsPrivate: &no, DeletedAt: &deleted},
			nil,
		}, nil
	}
	ids := []string{"AAAA-OPEN", "aaaa-mine", "aaaa-shared", "aaaa-private", "aaaa-binned", "aaaa-gone"}
	got, err := boardPlaces{}.Resolve(context.Background(), placesBusiness.Person{UUID: "me", UID: "0x1"}, ids)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]placesBusiness.Seen{
		"AAAA-OPEN":   {Title: "Roadmap", Href: "/app/board/aaaa-open"},
		"aaaa-mine":   {Title: "Offsite", Href: "/app/board/aaaa-mine"},
		"aaaa-shared": {Title: "Retro", Href: "/app/board/aaaa-shared"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolved %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(asked, ids) || askedFor != "me" {
		t.Errorf("asked the graph for %v as %q, want every id at once as the person", asked, askedFor)
	}
}

// The graph not answering is an error, never an empty list (which would read
// as "you can open none of them").
func TestBoardPlacesSayWhenTheGraphCantAnswer(t *testing.T) {
	old := readBoardsAccess
	t.Cleanup(func() { readBoardsAccess = old })
	readBoardsAccess = func(context.Context, []string, string) ([]*dgraphStruct.DgraphBoard, error) {
		return nil, errors.New("graph down")
	}
	if got, err := (boardPlaces{}).Resolve(context.Background(), placesBusiness.Person{UUID: "me"}, []string{"b1"}); err == nil {
		t.Errorf("resolved %+v with the graph down, want an error", got)
	}
}
