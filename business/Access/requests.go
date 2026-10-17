package business

// Asking for access ("Request access"), and answering: the people a resource's
// owner or someone with full access let in, at the level they choose, for good
// or until a date.

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

var (
	// ErrAlreadyHas is a request for a level the person already has.
	ErrAlreadyHas = errors.New("access: you can already do that")
	// ErrNoRequest is a request that isn't there, or isn't open any more.
	ErrNoRequest = errors.New("access: no such request")
	// ErrBadLevel is a level that can't be asked for or given here.
	ErrBadLevel = errors.New("access: that level can't be asked for")
	// ErrNobodyAnswers is a request nobody could answer: the resource's owner
	// and the people with full access are only the asker, or only the demo's
	// own people (its cast, its bots and its shared visitor), who never read
	// one.
	ErrNobodyAnswers = errors.New("access: nobody can answer a request for this")
)

// maxNote is the longest note a request takes.
const maxNote = 500

// Ask records who's request for a level on a resource they can't do that
// with (view, comment or edit; full is given, never asked for), with a note
// for whoever answers. Asking again updates the open request. The resource's
// owner and the people with full access are told (A6's bus).
func Ask(ctx context.Context, who Subject, res Resource, want Level, note string) (*accessModels.Request, error) {
	if res.Kind != KindDoc {
		return nil, ErrUnknownKind
	}
	if want < LevelView || want > LevelEdit {
		return nil, ErrBadLevel
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxNote {
		note = string([]rune(note)[:maxNote])
	}
	d, err := Rules.Resolve(ctx, who, res)
	if err != nil {
		return nil, err
	}
	if d.Deleted {
		return nil, docBusiness.ErrDocDeleted
	}
	if d.Level >= want {
		return nil, ErrAlreadyHas
	}
	resID, err := uuid.Parse(res.UUID)
	if err != nil {
		return nil, ErrNotFound
	}
	me, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return nil, ErrForbidden
	}
	// A request goes only to someone who can answer it, and is refused when
	// nobody can (docs incident, 14 Oct 2026, D3): on the demo a doc's owner
	// is its cast or its shared visitor, and a request to them was never read.
	answerers, err := answerersOf(ctx, res, who.UserUUID)
	if err != nil {
		return nil, err
	}
	if len(answerers) == 0 {
		return nil, ErrNobodyAnswers
	}
	req, err := accessModels.AskFor(ctx, res.Kind, resID, me, want.String(), note, now())
	if err != nil {
		return nil, err
	}
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind: resourceEventBusiness.DocAccessRequested, ResourceKind: res.Kind, ResourceUUID: res.UUID,
		Actor: docBusiness.Actor{UserUUID: who.UserUUID}, At: now(), Subjects: answerers,
		Data: map[string]string{"request": req.ID.String(), "level": want.String()},
	})
	return req, nil
}

// CanAsk says whether a request from who for more access to res than d (their
// decision on it) gives would reach someone who can answer it: they can't edit
// it yet, it isn't deleted, and someone other than them who isn't one of the
// demo's own people manages it (answerersOf). The web offers Request access
// by it, and Ask refuses by the same rule.
func CanAsk(ctx context.Context, who Subject, res Resource, d Decision) (bool, error) {
	if d.Deleted || d.Level >= LevelEdit || who.UserUUID == "" || res.Kind != KindDoc {
		return false, nil
	}
	answerers, err := answerersOf(ctx, res, who.UserUUID)
	if err != nil {
		return false, err
	}
	return len(answerers) > 0, nil
}

// answerersOf is who can answer a request on a resource from asker: its
// managers (managersOf) but the asker, anyone whose account is gone, and the
// demo's own people (helpers.IsDemoPerson: its cast, its bots and its shared
// visitor, whom nobody reads a request as). A seam for tests.
var answerersOf = func(ctx context.Context, res Resource, asker string) ([]string, error) {
	managers, err := managersOf(ctx, res)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(managers))
	for _, m := range managers {
		if strings.EqualFold(m, asker) {
			continue
		}
		if id, err := uuid.Parse(m); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	people, err := userModels.GetActiveUsersByUUIDsForNotification(ids)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range ids {
		if p := people[id]; p != nil && !helpers.IsDemoPerson(p.EmailID) {
			out = append(out, id.String())
		}
	}
	return out, nil
}

// managersOf is who answers a resource's requests: its owner and the people
// with full access.
func managersOf(ctx context.Context, res Resource) ([]string, error) {
	s, err := docBusiness.ReadSharing(ctx, res.UUID)
	if err != nil {
		return nil, err
	}
	var out []string
	if s.Owner != nil && s.Owner.Uuid != "" {
		out = append(out, strings.ToLower(s.Owner.Uuid))
	}
	for id, l := range s.People {
		if l == LevelFull {
			out = append(out, id)
		}
	}
	return out, nil
}

// Answer approves (giving the level asked for, or grant when it is set, until
// a moment or for good) or declines an open request, for the resource's owner
// or someone with full access to it.
func Answer(ctx context.Context, who Subject, requestID uuid.UUID, approve bool, grant *Level, until *time.Time) (*accessModels.Request, error) {
	req, err := accessModels.GetRequest(ctx, requestID)
	if errors.Is(err, accessModels.ErrNotFound) {
		return nil, ErrNoRequest
	}
	if err != nil {
		return nil, err
	}
	res := Resource{Kind: req.ResourceKind, UUID: req.ResourceUUID.String()}
	d, err := Rules.Resolve(ctx, who, res)
	if err != nil {
		return nil, err
	}
	switch {
	case d.Deleted:
		return nil, docBusiness.ErrDocDeleted
	case d.Level == LevelNone:
		// Someone who can't open the resource doesn't learn of its requests.
		return nil, ErrNoRequest
	case d.Level < LevelFull:
		return nil, ErrForbidden
	}
	if req.Status != accessModels.StatusPending {
		return nil, ErrNoRequest
	}
	status := accessModels.StatusDeclined
	var granted *string
	// The level the answer is about: the one asked for, or the one given.
	told := req.LevelWanted
	if approve {
		lvl, ok := wantedOf(req)
		if grant != nil {
			lvl, ok = *grant, *grant >= LevelView && *grant <= LevelFull
		}
		if !ok {
			return nil, ErrBadLevel
		}
		g := Grant{Kind: PrincipalUser, UUID: req.RequesterUUID.String(), Level: lvl, Until: until}
		// The share is told to its asker by the answer below, once.
		if _, err := apply(ctx, who, Change{Resource: res, Add: []Grant{g}}, applyOpts{tags: map[string]string{"request": req.ID.String()}}); err != nil {
			return nil, err
		}
		status = accessModels.StatusApproved
		name := lvl.String()
		granted, told = &name, name
	}
	me, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return nil, ErrForbidden
	}
	out, err := accessModels.Decide(ctx, requestID, status, granted, me, now())
	if errors.Is(err, accessModels.ErrNotFound) {
		// Answered by someone else a moment ago.
		return nil, ErrNoRequest
	}
	if err != nil {
		return nil, err
	}
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind: resourceEventBusiness.DocAccessAnswered, ResourceKind: res.Kind, ResourceUUID: res.UUID,
		Actor: docBusiness.Actor{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID}, At: now(),
		Subjects: []string{req.RequesterUUID.String()},
		Data:     map[string]string{"request": req.ID.String(), "status": status, "level": told},
	})
	return out, nil
}

// wantedOf is the level a request asks for.
func wantedOf(req *accessModels.Request) (Level, bool) {
	var l Level
	if err := l.UnmarshalText([]byte(req.LevelWanted)); err != nil || l < LevelView {
		return LevelNone, false
	}
	return l, true
}

// Requests are the open requests who may answer (on one resource when res is
// given, else on every resource they have full access to, the newest 200
// looked at), and who's own requests, newest first.
func Requests(ctx context.Context, who Subject, res *Resource) (incoming, outgoing []*accessModels.Request, err error) {
	if me, perr := uuid.Parse(who.UserUUID); perr == nil {
		if outgoing, err = accessModels.ListRequests(ctx, accessModels.RequestQuery{RequesterUUID: &me, Limit: 50}); err != nil {
			return nil, nil, err
		}
	}
	q := accessModels.RequestQuery{OpenOnly: true, Limit: 200}
	if res != nil {
		id, perr := uuid.Parse(res.UUID)
		if perr != nil {
			return nil, nil, ErrNotFound
		}
		q.ResourceKind, q.ResourceUUID = res.Kind, &id
	}
	open, err := accessModels.ListRequests(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	var all []Resource
	seen := map[Resource]bool{}
	for _, r := range open {
		one := Resource{Kind: r.ResourceKind, UUID: r.ResourceUUID.String()}
		if one.Kind == KindDoc && !seen[one] {
			seen[one] = true
			all = append(all, one)
		}
	}
	decided, err := ResolveAll(ctx, who, all)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range open {
		if d, ok := decided[Resource{Kind: r.ResourceKind, UUID: r.ResourceUUID.String()}]; ok && d.Level == LevelFull {
			incoming = append(incoming, r)
		}
	}
	return incoming, outgoing, nil
}
