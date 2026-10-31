package business

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// The admin's list says who made each link: with many members, a column of
// "Personal room" rows told an admin nothing. One read for the grants and one
// for every maker's name; names that can't be read leave the list standing.
func TestTheAdminListSaysWhoMadeEachLink(t *testing.T) {
	oldList, oldNames := activeGrants, displayNames
	t.Cleanup(func() { activeGrants, displayNames = oldList, oldNames })
	maya, ben, gone := uuid.New(), uuid.New(), uuid.New()
	slug := "maya-chen"
	grants := []*guestModel.GuestGrant{
		{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, CreatedBy: maya, Slug: &slug},
		{Id: uuid.New(), ResourceType: guestModel.ResourceDoc, CreatedBy: ben},
		{Id: uuid.New(), ResourceType: guestModel.ResourceDoc, CreatedBy: maya},
		{Id: uuid.New(), ResourceType: guestModel.ResourceDoc, CreatedBy: gone},
	}
	activeGrants = func(context.Context) ([]*guestModel.GuestGrant, error) { return grants, nil }
	var asked [][]uuid.UUID
	displayNames = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
		asked = append(asked, ids)
		// A member who has gone is absent, as DisplayNamesByUUIDs leaves them.
		return map[uuid.UUID]string{maya: "Maya Chen", ben: "Ben Ito"}, nil
	}

	list, err := ListActiveGrants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Fatalf("the names were read %d times; once is one query for the page", len(asked))
	}
	for _, id := range []uuid.UUID{maya, ben, gone} {
		found := false
		for _, a := range asked[0] {
			found = found || a == id
		}
		if !found {
			t.Errorf("the names asked for %v, without %v", asked[0], id)
		}
	}
	want := []string{"Maya Chen", "Ben Ito", "Maya Chen", ""}
	if len(list) != len(want) {
		t.Fatalf("%d rows, want %d", len(list), len(want))
	}
	for i, g := range list {
		if g.GuestGrant != grants[i] || g.CreatedByName != want[i] {
			t.Errorf("row %d: %+v named %q, want %q", i, g.GuestGrant, g.CreatedByName, want[i])
		}
	}

	// On the wire: the grant's own fields as before, and the name beside them;
	// none for a member who has gone.
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0]["created_by_name"] != "Maya Chen" || rows[0]["slug"] != "maya-chen" || rows[0]["created_by"] != maya.String() || rows[0]["resource_type"] != "meeting" {
		t.Errorf("a room on the wire: %v", rows[0])
	}
	if _, named := rows[3]["created_by_name"]; named {
		t.Errorf("a gone member's link on the wire: %v", rows[3])
	}

	// Names that can't be read: the rows still come, so an admin can still revoke.
	displayNames = func(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) { return nil, errors.New("down") }
	list, err = ListActiveGrants(context.Background())
	if err != nil || len(list) != len(grants) || list[0].CreatedByName != "" {
		t.Errorf("without names: %+v, %v", list, err)
	}

	// Grants that can't be read are an error, and no names are asked for.
	asked = nil
	displayNames = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
		asked = append(asked, ids)
		return nil, nil
	}
	activeGrants = func(context.Context) ([]*guestModel.GuestGrant, error) { return nil, errors.New("down") }
	if _, err := ListActiveGrants(context.Background()); err == nil || len(asked) != 0 {
		t.Errorf("a failed read: %v, names asked %d times", err, len(asked))
	}
}
