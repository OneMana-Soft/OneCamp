package business

// One resource decided for many people at once (the A5×A6 join): what each of
// them may do with it, in one read of its sharing (business/Doc DecidePeople,
// the access rule from the doc's side). The notices, mentions and follows
// (business/Follow) ask this, so they reach exactly the people who can read
// the doc: those it is shared with by name, through a team or through its
// space, and its whole workspace when it isn't private; never someone whose
// grant has ended, even before the expiry sweep has taken it away.

import (
	"context"
	"errors"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
)

// People is a resolver that decides one resource for many people in one read.
// Rules is one; ResolvePeople asks it when it can.
type People interface {
	ResolvePeople(ctx context.Context, res Resource, userUUIDs []string) (map[string]Decision, error)
}

// ResolvePeople is each person's decision on res, for people named by uuid and
// keyed as given (an empty id is left out), in as few reads as the resolver
// can: one for Rules, one a person for a resolver that isn't a People (a
// test's), where a person it finds nothing for has none. ErrNotFound for a
// resource that isn't there; any error says nothing about anyone.
func ResolvePeople(ctx context.Context, res Resource, userUUIDs []string) (map[string]Decision, error) {
	if p, ok := Rules.(People); ok {
		return p.ResolvePeople(ctx, res, userUUIDs)
	}
	out := make(map[string]Decision, len(userUUIDs))
	for _, id := range userUUIDs {
		if id == "" {
			continue
		}
		d, err := Rules.Resolve(ctx, Subject{UserUUID: id}, res)
		switch {
		case errors.Is(err, ErrNotFound):
			d = Decision{}
		case err != nil:
			return nil, err
		}
		out[id] = d
	}
	return out, nil
}

func (rules) ResolvePeople(ctx context.Context, res Resource, userUUIDs []string) (map[string]Decision, error) {
	if res.Kind != KindDoc {
		return nil, ErrUnknownKind
	}
	got, err := docBusiness.DecidePeople(ctx, res.UUID, userUUIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Decision, len(got))
	for id, d := range got {
		out[id] = decisionOf(d)
	}
	return out, nil
}
