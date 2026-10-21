package business

import (
	"context"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/google/uuid"
)

// Admin › Apps names an app's places only for an admin who may open them, and
// says whether the app's own user may open each: an app reaches a place only
// when both its audience and its user allow it.

const placeKind = "app-places-test"

var (
	placeAdmin = uuid.MustParse("6f1e2d3c-4b5a-4968-8776-655443322110")
	placeBot   = uuid.MustParse("0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d")
	openPlace  = "11111111-1111-4111-8111-111111111111"
	shutPlace  = "22222222-2222-4222-8222-222222222222"
	hiddenOne  = "33333333-3333-4333-8333-333333333333"
)

// placeRules: the bot may open openPlace and not shutPlace; hiddenOne is there
// but nobody may open it.
type placeRules struct{}

func (placeRules) DecideMany(_ context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	for _, id := range ids {
		level := accessBusiness.LevelNone
		if id == openPlace && who.UserUUID == placeBot.String() {
			level = accessBusiness.LevelView
		}
		out[id] = accessBusiness.Decision{Level: level}
	}
	return out, nil
}

func init() {
	accessBusiness.RegisterKind(placeKind, placeRules{})
	// The admin may open the first two, never the third.
	accessBusiness.RegisterTitleReader(placeKind, func(_ context.Context, ids []string, reader accessBusiness.Subject) (map[string]string, error) {
		out := map[string]string{}
		if reader.UserUUID != placeAdmin.String() {
			return out, nil
		}
		for _, id := range ids {
			switch id {
			case openPlace:
				out[id] = "Launch notes"
			case shutPlace:
				out[id] = "Payroll"
			}
		}
		return out, nil
	})
	uidOf = func(context.Context, accessBusiness.Subject) (string, error) { return "0x1", nil }
}

func TestAnAppsPlacesAreNamedForTheAdminAndSayWhatItsUserOpens(t *testing.T) {
	ctx := context.Background()
	admin := accessBusiness.Subject{UserUUID: placeAdmin.String(), Admin: true}
	bot := placeBot
	app := &Identity{Name: "Deploys", BotUserID: &bot, Audience: Audience{Places: []Place{
		{Kind: placeKind, ID: openPlace},
		{Kind: placeKind, ID: shutPlace},
		{Kind: placeKind, ID: hiddenOne},
		{Kind: "table", ID: openPlace},
	}}}

	got, err := DescribePlaces(ctx, admin, app)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Places) != 4 {
		t.Fatalf("%d places described, not 4", len(got.Places))
	}
	opens := func(i int) string {
		if got.Places[i].AppOpens == nil {
			return "undecided"
		}
		if *got.Places[i].AppOpens {
			return "opens"
		}
		return "shut"
	}
	if got.Places[0].Title != "Launch notes" || opens(0) != "opens" {
		t.Errorf("the place its user opens: %+v, %s", got.Places[0], opens(0))
	}
	if got.Places[1].Title != "Payroll" || opens(1) != "shut" {
		t.Errorf("the place its user can't open: %+v, %s", got.Places[1], opens(1))
	}
	// One the admin can't open isn't named to them.
	if got.Places[2].Title != "" || opens(2) != "shut" {
		t.Errorf("the place the admin can't open: %+v, %s", got.Places[2], opens(2))
	}
	// Nothing decides tables yet: nothing is said of one.
	if got.Places[3].Title != "" || opens(3) != "undecided" {
		t.Errorf("a table: %+v, %s", got.Places[3], opens(3))
	}
}

func TestAnAppWithNoUserYetSaysNothingOfWhatItOpens(t *testing.T) {
	app := &Identity{Name: "Deploys", Audience: Audience{Places: []Place{{Kind: placeKind, ID: openPlace}}}}
	got, err := DescribePlaces(context.Background(), accessBusiness.Subject{UserUUID: placeAdmin.String()}, app)
	if err != nil {
		t.Fatal(err)
	}
	if got.Places[0].Title != "Launch notes" || got.Places[0].AppOpens != nil {
		t.Errorf("before its first token: %+v", got.Places[0])
	}
	empty, err := DescribePlaces(context.Background(), accessBusiness.Subject{UserUUID: placeAdmin.String()}, &Identity{})
	if err != nil || empty.Places == nil || len(empty.Places) != 0 {
		t.Errorf("no places: %+v, %v", empty, err)
	}
}
