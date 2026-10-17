package entityLinkBusiness

import (
	"context"
	"errors"
	"fmt"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// MaxMentionChecks is how many people one check takes: the "@" list shows
// eight at a time.
const MaxMentionChecks = 10

// ErrNoDoc is a doc the person can't edit, answered as one that isn't there:
// nobody mentions anyone in a doc they can't write in.
var ErrNoDoc = errors.New("no such doc")

// ErrTooManyPeople is a check of more than MaxMentionChecks people.
var ErrTooManyPeople = fmt.Errorf("check at most %d people at once", MaxMentionChecks)

// MentionAccess is what the "@" list needs to know about the people it
// offers, in a doc being written: who of them can't open it, and whether the
// writer may share it with them. Sharing with someone just mentioned is an
// invite (POST /doc/access/invite, accessBusiness.Invite), so it is offered by
// the invite's rule, accessBusiness.MayInvite: an editor may, at their own
// level or below, as from the share dialog.
type MentionAccess struct {
	CanShare bool     `json:"can_share"`
	CantOpen []string `json:"cant_open"`
}

// MentionAccessFor decides, through the Access Resolver, which of people
// (user uuids) can't open a doc that who is writing in: one read of the doc's
// sharing for all of them (ResolvePeople).
func MentionAccessFor(ctx context.Context, who accessBusiness.Subject, docUUID string, people []string) (MentionAccess, error) {
	if len(people) > MaxMentionChecks {
		return MentionAccess{}, ErrTooManyPeople
	}
	doc := accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID}
	d, err := accessBusiness.Rules.Resolve(ctx, who, doc)
	if errors.Is(err, accessBusiness.ErrNotFound) || (err == nil && d.Level < accessBusiness.LevelEdit) {
		return MentionAccess{}, ErrNoDoc
	}
	if err != nil {
		return MentionAccess{}, err
	}
	decided, err := accessBusiness.ResolvePeople(ctx, doc, people)
	if errors.Is(err, accessBusiness.ErrNotFound) {
		return MentionAccess{}, ErrNoDoc
	}
	if err != nil {
		return MentionAccess{}, err
	}
	out := MentionAccess{CanShare: accessBusiness.MayInvite(d), CantOpen: []string{}}
	seen := map[string]bool{}
	for _, id := range people {
		if !seen[id] && decided[id].Level < accessBusiness.LevelView {
			out.CantOpen = append(out.CantOpen, id)
		}
		seen[id] = true
	}
	return out, nil
}
