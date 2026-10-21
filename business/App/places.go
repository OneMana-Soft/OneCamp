package business

import (
	"context"
	"errors"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// PlaceView is a place an app was added to, as Admin › Apps shows it to one
// admin. Its title is there only when that admin may open it (a list never
// names what its reader can't), and AppOpens says whether the app's own user
// may open it now: an app reaches a place only when both its audience and its
// user allow it, so a place its user can't open does nothing yet.
type PlaceView struct {
	Place
	Title string `json:"title,omitempty"`
	// AppOpens is absent where nothing decides the kind yet (no rule is
	// registered with business/Access for it), or before the app has a user.
	AppOpens *bool `json:"app_opens,omitempty"`
}

// uidOf names a person by their graph uid, as the resolver counts memberships
// (a seam for tests, which have no graph).
var uidOf = accessBusiness.UIDOf

// AudienceView is an app's audience as Admin › Apps shows it.
type AudienceView struct {
	Places []PlaceView `json:"places"`
	Public bool        `json:"public"`
}

// DescribePlaces names an app's places for viewer (business/Access
// ReadableTitles, as viewer acting themselves) and says whether the app's own
// user may open each (ResolveAll, as that user: never an admin). It reads in
// one batch per kind.
func DescribePlaces(ctx context.Context, viewer accessBusiness.Subject, app *Identity) (AudienceView, error) {
	out := AudienceView{Places: make([]PlaceView, 0, len(app.Audience.Places)), Public: app.Audience.Public}
	if len(app.Audience.Places) == 0 {
		return out, nil
	}
	refs := make([]accessBusiness.ItemRef, 0, len(app.Audience.Places))
	byKind := map[string][]accessBusiness.Resource{}
	var kinds []string
	for _, p := range app.Audience.Places {
		refs = append(refs, accessBusiness.ItemRef{Kind: p.Kind, ID: p.ID})
		if _, seen := byKind[p.Kind]; !seen {
			kinds = append(kinds, p.Kind)
		}
		byKind[p.Kind] = append(byKind[p.Kind], accessBusiness.Resource{Kind: p.Kind, UUID: p.ID})
	}
	titles, err := accessBusiness.ReadableTitles(ctx, []accessBusiness.Subject{viewer}, refs)
	if err != nil {
		return AudienceView{}, err
	}
	opens := map[accessBusiness.Resource]bool{}
	decided := map[string]bool{}
	if app.BotUserID != nil {
		// The app's own user, as its tokens act: no admin powers (R1).
		bot := accessBusiness.Subject{UserUUID: app.BotUserID.String()}
		if uid, err := uidOf(ctx, bot); err == nil {
			bot.UserUID = uid
		}
		for _, kind := range kinds {
			got, err := accessBusiness.ResolveAll(ctx, bot, byKind[kind])
			if errors.Is(err, accessBusiness.ErrUnknownKind) {
				continue // nothing decides this kind yet: say nothing of it
			}
			if err != nil {
				return AudienceView{}, err
			}
			decided[kind] = true
			for r, d := range got {
				opens[r] = d.Level >= accessBusiness.LevelView && !d.Deleted
			}
		}
	}
	for _, p := range app.Audience.Places {
		v := PlaceView{Place: p, Title: titles[accessBusiness.ItemRef{Kind: p.Kind, ID: p.ID}]}
		if decided[p.Kind] {
			o := opens[accessBusiness.Resource{Kind: p.Kind, UUID: p.ID}]
			v.AppOpens = &o
		}
		out.Places = append(out.Places, v)
	}
	return out, nil
}
