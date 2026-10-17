// Package business (Follow) is who hears about what (docs programme A6, D-13):
// follows, the rules that turn a Resource Event into the people to tell, and
// the bus's other sinks (the audit, MQTT meta events for open tabs, webhooks)
// with the inbox's reading of notices. Everything is registered from init()
// (sinks.go), so linking this package is what switches it on.
//
// FOLLOW MODES. A person may choose, for one thing:
//
//	all       every comment and proposal on it they may read;
//	mentions  "replies and mentions": comments on what they take part in, and
//	          @mentions of them;
//	none      only an @mention of them.
//
// With no choice, someone who takes part (the owner, an editor, anyone who has
// commented) follows "replies and mentions", and everyone else "none", which is
// exactly the participants rule a comment has always followed. A mention, and
// being shared with, reach a reader whatever they chose.
package business

import (
	"context"
	"errors"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	"github.com/google/uuid"
)

// The modes, as the API spells them.
const (
	ModeAll      = followModels.ModeAll
	ModeMentions = followModels.ModeMentions
	ModeNone     = followModels.ModeNone
)

var (
	// ErrNotFound is a thing that isn't there, or that the person can't open:
	// the two answer alike, so following tells nobody what exists.
	ErrNotFound = errors.New("follow: there's nothing like that to follow")
	// ErrUnknownKind is a kind of thing that can't be followed yet.
	ErrUnknownKind = errors.New("follow: that kind of thing can't be followed")
	// ErrBadMode is a mode other than all, mentions and none.
	ErrBadMode = followModels.ErrBadMode
)

// State is someone's follow on one thing, as the app shows it.
type State struct {
	ResourceKind string `json:"resource_kind"`
	ResourceUUID string `json:"resource_uuid"`
	// Mode is what they hear now.
	Mode string `json:"mode"`
	// Chosen is whether they chose it; otherwise Mode is their default.
	Chosen bool `json:"chosen"`
	// Default is what they'd hear with no choice made.
	Default string `json:"default"`
}

// Seams for tests: the Access Resolver, the doc's people, and the store.
var (
	resolve = func(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
		return accessBusiness.Rules.Resolve(ctx, who, res)
	}
	docPeople   = docDomain.GetDocCommentParticipants
	getFollow   = followModels.GetFollow
	setFollow   = followModels.SetFollow
	clearFollow = followModels.ClearFollow
)

// Get is the person's follow on the thing.
func Get(ctx context.Context, who accessBusiness.Subject, kind, resourceUUID string) (State, error) {
	me, def, err := defaultFor(ctx, who, kind, resourceUUID)
	if err != nil {
		return State{}, err
	}
	st := State{ResourceKind: kind, ResourceUUID: resourceUUID, Mode: def, Default: def}
	f, err := getFollow(ctx, me, kind, resourceUUID)
	switch {
	case errors.Is(err, followModels.ErrNotFound):
		return st, nil
	case err != nil:
		return State{}, err
	}
	st.Mode, st.Chosen = f.Mode, true
	return st, nil
}

// Set records the person's choice.
func Set(ctx context.Context, who accessBusiness.Subject, kind, resourceUUID, mode string) (State, error) {
	if !followModels.ValidMode(mode) {
		return State{}, ErrBadMode
	}
	me, def, err := defaultFor(ctx, who, kind, resourceUUID)
	if err != nil {
		return State{}, err
	}
	if err := setFollow(ctx, me, kind, resourceUUID, mode); err != nil {
		return State{}, err
	}
	return State{ResourceKind: kind, ResourceUUID: resourceUUID, Mode: mode, Chosen: true, Default: def}, nil
}

// Clear forgets the person's choice: their default applies again.
func Clear(ctx context.Context, who accessBusiness.Subject, kind, resourceUUID string) (State, error) {
	me, def, err := defaultFor(ctx, who, kind, resourceUUID)
	if err != nil {
		return State{}, err
	}
	if err := clearFollow(ctx, me, kind, resourceUUID); err != nil {
		return State{}, err
	}
	return State{ResourceKind: kind, ResourceUUID: resourceUUID, Mode: def, Default: def}, nil
}

// defaultFor decides whether the person may follow the thing at all (they must
// be able to open it, through the Access Resolver) and what they follow with no
// choice made.
func defaultFor(ctx context.Context, who accessBusiness.Subject, kind, resourceUUID string) (uuid.UUID, string, error) {
	me, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return uuid.Nil, "", ErrNotFound
	}
	if kind != accessBusiness.KindDoc {
		return uuid.Nil, "", ErrUnknownKind
	}
	if _, err := uuid.Parse(resourceUUID); err != nil {
		return uuid.Nil, "", ErrNotFound
	}
	d, err := resolve(ctx, who, accessBusiness.Resource{Kind: kind, UUID: resourceUUID})
	switch {
	case errors.Is(err, docBusiness.ErrNotFound) || errors.Is(err, docBusiness.ErrDocDeleted):
		return uuid.Nil, "", ErrNotFound
	case err != nil:
		return uuid.Nil, "", err
	case d.Deleted || d.Level < accessBusiness.LevelView:
		return uuid.Nil, "", ErrNotFound
	case d.Owner || d.Level >= accessBusiness.LevelEdit:
		return me, ModeMentions, nil
	}
	doc, err := docPeople(ctx, resourceUUID)
	if err != nil {
		return uuid.Nil, "", err
	}
	if tookPart(doc, me.String()) {
		return me, ModeMentions, nil
	}
	return me, ModeNone, nil
}

// tookPart is whether someone takes part in a doc: made it, may edit it, or has
// commented on it. doc is read as GetDocCommentParticipants reads it. Pure.
func tookPart(doc *dgraphStruct.DgraphDoc, userUUID string) bool {
	if doc == nil || userUUID == "" {
		return false
	}
	if doc.CreatedBy != nil && doc.CreatedBy.Uuid == userUUID {
		return true
	}
	for _, u := range doc.EditingUser {
		if u != nil && u.Uuid == userUUID {
			return true
		}
	}
	for _, c := range doc.Comments {
		if c != nil && c.CommentBy != nil && c.CommentBy.Uuid == userUUID {
			return true
		}
	}
	return false
}
