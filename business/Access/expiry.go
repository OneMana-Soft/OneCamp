package business

// Grants that end: a person or a team given a level "until" a moment loses it
// when the moment passes. A registered sweep (business/Sweep) takes the grant
// away, once a minute, and does what any change does after it: the indexes are
// written again, the change is told, and the sockets of the people it moved
// are closed. Harmless when two processes run it at once: taking a grant away
// twice is taking it away.

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
)

// expiryBatch bounds the grants one run takes away.
const expiryBatch = 200

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "access.grant.expiry", Every: time.Minute, Run: EndDueGrants})
}

// EndDueGrants takes away every grant whose moment has passed (up to a batch
// per run; the next run takes the rest). It reports the first failure, after
// trying them all.
func EndDueGrants(ctx context.Context) error {
	due, err := accessModels.DueExpiry(ctx, now(), expiryBatch)
	if err != nil {
		return err
	}
	var first error
	for _, e := range due {
		if err := endGrant(ctx, e); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Access: %s %s's grant on %s %s didn't end: %+v",
				e.PrincipalKind, e.PrincipalUUID, e.ResourceKind, e.ResourceUUID, err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// endGrant takes one ended grant away, then forgets its end.
func endGrant(ctx context.Context, e *accessModels.Expiry) error {
	if e.ResourceKind != KindDoc {
		return accessModels.Ended(ctx, *e)
	}
	doc := e.ResourceUUID.String()
	before, err := docBusiness.ReadSharing(ctx, doc)
	if errors.Is(err, docBusiness.ErrNotFound) {
		// Deleted for good: nothing left to take away.
		return accessModels.Ended(ctx, *e)
	}
	if err != nil {
		return err
	}
	c := docBusiness.SharingChange{}
	principal := e.PrincipalUUID.String()
	if e.PrincipalKind == string(PrincipalTeam) {
		c.Teams = map[string]level.Level{principal: LevelNone}
	} else {
		c.People = map[string]level.Level{principal: LevelNone}
	}
	after := before.With(c)
	// A doc in the trash loses the grant too, so restoring it brings back
	// only what was meant to last.
	if err := docBusiness.ApplySharing(ctx, []string{doc}, c); err != nil && !errors.Is(err, docBusiness.ErrNoSuchPrincipal) {
		return err
	}
	if err := accessModels.Ended(ctx, *e); err != nil {
		return err
	}
	docBusiness.AccessChanged(ctx, []string{doc}, docBusiness.Actor{System: true}, resourceEventBusiness.DocAccessChanged,
		docBusiness.Explain(before, after), map[string]string{"ended": e.PrincipalKind + ":" + principal})
	return nil
}
