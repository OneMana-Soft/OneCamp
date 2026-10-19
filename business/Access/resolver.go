package business

// The resolver for docs: decisions in batches from business/Doc's rule, changes
// explained before they are made and made in one transaction, a new subpage's
// copy of its parent's sharing, and ownership handed on.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
	"github.com/google/uuid"
)

// ErrNotFound is a resource that isn't there.
var ErrNotFound = docBusiness.ErrNotFound

// batchSize bounds the docs one read decides.
const batchSize = 500

type rules struct{}

func viewerOf(who Subject) docBusiness.Viewer {
	return docBusiness.Viewer{UID: who.UserUID, UUID: who.UserUUID}
}

// decisionOf is business/Doc's decision as the resolver gives it.
func decisionOf(d docBusiness.DocDecision) Decision {
	out := Decision{
		Level: d.Level, Owner: d.Owner, Deleted: d.Deleted, Archived: d.Archived, Was: d.Was,
		Locked: d.Locked(), Lock: d.Lock, Unlocked: d.Unlocked, Hosted: d.Hosted,
	}
	for _, r := range d.Reasons {
		out.Reasons = append(out.Reasons, string(r))
	}
	return out
}

// ResolveAll decides every resource: docs a batch of reads at a time, and
// any other kind by the rule its package registered. A read that fails is
// ErrUndecided, so no caller takes it for a no.
func (rules) ResolveAll(ctx context.Context, who Subject, res []Resource) (map[Resource]Decision, error) {
	out := make(map[Resource]Decision, len(res))
	docs := make([]Resource, 0, len(res))
	var others []Resource
	for _, r := range res {
		if r.Kind == KindDoc {
			docs = append(docs, r)
		} else {
			others = append(others, r)
		}
	}
	// Kinds other than docs go to the rule their package registered (kinds.go).
	if len(others) > 0 {
		if err := resolveOthers(ctx, who, others, out); err != nil {
			return nil, undecided(err)
		}
	}
	// A hosted doc's host is decided for who, as asked (hosts.go).
	docCtx := withSubject(ctx, who)
	for from := 0; from < len(docs); from += batchSize {
		chunk := docs[from:min(from+batchSize, len(docs))]
		ids := make([]string, 0, len(chunk))
		for _, r := range chunk {
			ids = append(ids, r.UUID)
		}
		got, err := docBusiness.DecideDocs(docCtx, viewerOf(who), ids)
		if err != nil {
			return nil, undecided(err)
		}
		for _, r := range chunk {
			if d, ok := got[strings.ToLower(r.UUID)]; ok {
				out[r] = decisionOf(d)
			}
		}
	}
	return out, nil
}

func (r rules) Resolve(ctx context.Context, who Subject, res Resource) (Decision, error) {
	got, err := r.ResolveAll(ctx, who, []Resource{res})
	if err != nil {
		return Decision{}, err
	}
	d, ok := got[res]
	if !ok {
		return Decision{}, ErrNotFound
	}
	return d, nil
}

// Filter decides every resource in batches of one read each (A3's lists and
// tree once made one query per doc).
func (r rules) Filter(ctx context.Context, who Subject, res []Resource, atLeast Level) ([]Resource, error) {
	got, err := r.ResolveAll(ctx, who, res)
	if err != nil {
		return nil, err
	}
	var kept []Resource
	for _, one := range res {
		if d, ok := got[one]; ok && d.Level >= atLeast {
			kept = append(kept, one)
		}
	}
	return kept, nil
}

// sharingChangeOf is a change as business/Doc makes it, against the sharing it
// changes (whose general access a change of privacy alone keeps the level of).
func sharingChangeOf(before *docBusiness.Sharing, c Change) (docBusiness.SharingChange, error) {
	sc := docBusiness.SharingChange{People: map[string]level.Level{}, Teams: map[string]level.Level{}, Replace: c.Replace}
	for _, g := range c.Add {
		if _, err := uuid.Parse(g.UUID); err != nil {
			return sc, ErrNoSuchPrincipal
		}
		id := strings.ToLower(g.UUID)
		switch g.Kind {
		case PrincipalUser, "":
			sc.People[id] = g.Level
		case PrincipalTeam:
			sc.Teams[id] = min(g.Level, LevelEdit)
		default:
			// A space's members reach a doc through the doc's own space.
			return sc, ErrUnknownKind
		}
	}
	for _, g := range c.Remove {
		id := strings.ToLower(g.UUID)
		switch g.Kind {
		case PrincipalUser, "":
			sc.People[id] = LevelNone
		case PrincipalTeam:
			sc.Teams[id] = LevelNone
		default:
			return sc, ErrUnknownKind
		}
	}
	if c.General != nil || c.Private != nil {
		g := before.General
		if c.General != nil {
			if *c.General == LevelNone {
				g = docBusiness.GeneralAccess{Private: true, Level: LevelView}
			} else {
				g = docBusiness.GeneralAccess{Private: false, Level: *c.General}
			}
		}
		if c.Private != nil {
			g.Private = *c.Private
		}
		g.Level = min(max(g.Level, LevelView), LevelEdit)
		sc.General = &g
	}
	return sc, nil
}

// explanationOf is business/Doc's explanation as the resolver gives it.
func explanationOf(ex docBusiness.Explained, pages int) Explanation {
	out := Explanation{Effects: []Effect{}, Pages: pages}
	for _, s := range ex.People {
		out.Effects = append(out.Effects, Effect{UserUUID: s.UserUUID, From: s.From, To: s.To})
	}
	if ex.Everyone != nil {
		out.Everyone = &Effect{From: ex.Everyone.From, To: ex.Everyone.To}
	}
	return out
}

func (r rules) ExplainChange(ctx context.Context, who Subject, c Change) (Explanation, error) {
	if c.Resource.Kind != KindDoc {
		return Explanation{}, ErrUnknownKind
	}
	before, err := docBusiness.ReadSharing(ctx, c.Resource.UUID)
	if err != nil {
		return Explanation{}, err
	}
	sc, err := sharingChangeOf(before, c)
	if err != nil {
		return Explanation{}, err
	}
	after, err := before.After(ctx, sc)
	if errors.Is(err, docBusiness.ErrNoSuchPrincipal) {
		return Explanation{}, ErrNoSuchPrincipal
	}
	if err != nil {
		return Explanation{}, err
	}
	pages := 1
	if c.ApplyToSubpages {
		ids, _, err := r.fullUnder(ctx, who, c.Resource)
		if err != nil {
			return Explanation{}, err
		}
		pages = len(ids)
	}
	return explanationOf(docBusiness.Explain(before, after), pages), nil
}

// fullUnder is the live pages from res down that who has full access to, res
// first, and how many pages under it they don't.
func (r rules) fullUnder(ctx context.Context, who Subject, res Resource) ([]string, int, error) {
	all, err := Subtree(ctx, res)
	if err != nil {
		return nil, 0, err
	}
	got, err := r.ResolveAll(ctx, who, all)
	if err != nil {
		return nil, 0, err
	}
	var ids []string
	skipped := 0
	for _, p := range all {
		if d, ok := got[p]; ok && d.Level == LevelFull {
			ids = append(ids, p.UUID)
		} else if p != res {
			skipped++
		}
	}
	return ids, skipped, nil
}

// Apply makes a change to a resource (and with ApplyToSubpages, to every page
// under it that who has full access to), in one transaction, then does what
// follows it: the expiry of grants that end, the indexes, the events and the
// live sockets. who must have full access to the resource; what it would do is
// returned.
func Apply(ctx context.Context, who Subject, c Change) (Explanation, error) {
	return apply(ctx, who, c, applyOpts{})
}

// applyOpts is what sets one way of changing sharing apart from another; the
// rest (the read, the change, the expiry, the indexes, the events and the
// sockets) is apply's, once.
type applyOpts struct {
	// allow says whether who, deciding d on the resource, may make change c.
	// nil is Apply's rule: full access. A doc in the trash, or one who can't
	// open, is refused before allow is asked, and allow is asked before the
	// sharing is read, so a refusal costs no read of the graph.
	allow func(d Decision, c Change) error
	// against, when set, checks change c against the sharing before it, once
	// that is read (an invite never names someone the doc names already).
	against func(c Change, before *docBusiness.Sharing) error
	// tags go on the change's events beside their own data: a request's id
	// (its asker is told by the answer), an invite's invite=true.
	tags map[string]string
}

// fullAccess is Apply's rule: only full access changes sharing.
func fullAccess(d Decision, _ Change) error {
	if d.Level < LevelFull {
		return ErrForbidden
	}
	return nil
}

// apply is the one path every change to sharing takes: Apply (full access),
// an editor's invite (B6's Invite) and a request's approval (Answer). It
// decides through Rules, as every other access check does.
func apply(ctx context.Context, who Subject, c Change, o applyOpts) (Explanation, error) {
	r := rules{}
	if c.Resource.Kind != KindDoc {
		return Explanation{}, ErrUnknownKind
	}
	d, err := Rules.Resolve(ctx, who, c.Resource)
	if err != nil {
		return Explanation{}, err
	}
	switch {
	case d.Deleted:
		return Explanation{}, docBusiness.ErrDocDeleted
	case d.Level == LevelNone:
		return Explanation{}, ErrNotFound
	}
	allow := o.allow
	if allow == nil {
		allow = fullAccess
	}
	if err := allow(d, c); err != nil {
		return Explanation{}, err
	}
	before, err := docBusiness.ReadSharing(ctx, c.Resource.UUID)
	if err != nil {
		return Explanation{}, err
	}
	if o.against != nil {
		if err := o.against(c, before); err != nil {
			return Explanation{}, err
		}
	}
	sc, err := sharingChangeOf(before, c)
	if err != nil {
		return Explanation{}, err
	}
	after, err := before.After(ctx, sc)
	if errors.Is(err, docBusiness.ErrNoSuchPrincipal) {
		return Explanation{}, ErrNoSuchPrincipal
	}
	if err != nil {
		return Explanation{}, err
	}
	ids := []string{c.Resource.UUID}
	if c.ApplyToSubpages {
		if ids, _, err = r.fullUnder(ctx, who, c.Resource); err != nil {
			return Explanation{}, err
		}
	}
	if err := docBusiness.ApplySharing(ctx, ids, sc); err != nil {
		if errors.Is(err, docBusiness.ErrNoSuchPrincipal) {
			return Explanation{}, ErrNoSuchPrincipal
		}
		return Explanation{}, err
	}
	recordExpiry(ctx, who, ids, c)
	ex := docBusiness.Explain(before, after)
	actor := docBusiness.Actor{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID}
	data := map[string]string{}
	for k, v := range o.tags {
		data[k] = v
	}
	if len(ids) > 1 {
		data["subpages"] = "true"
		// A subpage's people and levels differ from the page's: its sockets
		// are all asked again.
		docBusiness.AccessChanged(ctx, ids[:1], actor, resourceEventBusiness.DocAccessChanged, ex, data)
		docBusiness.AccessChanged(ctx, ids[1:], actor, resourceEventBusiness.DocAccessChanged, docBusiness.Explained{Everyone: &docBusiness.Shift{}}, data)
	} else {
		docBusiness.AccessChanged(ctx, ids, actor, resourceEventBusiness.DocAccessChanged, ex, data)
	}
	return explanationOf(ex, len(ids)), nil
}

// recordExpiry records, for every page a change was made to, when each grant
// it added ends, and forgets the end of every grant it made for good or took
// away. A failure is logged: the grant then lasts until it is taken away.
func recordExpiry(ctx context.Context, who Subject, docIDs []string, c Change) {
	resources := make([]uuid.UUID, 0, len(docIDs))
	for _, id := range docIDs {
		if u, err := uuid.Parse(id); err == nil {
			resources = append(resources, u)
		}
	}
	var by *uuid.UUID
	if u, err := uuid.Parse(who.UserUUID); err == nil {
		by = &u
	}
	if c.Replace {
		// The grants before are gone, and with them when they ended.
		_ = accessModels.ClearAllExpiry(ctx, c.Resource.Kind, resources)
	}
	end := func(g Grant) {
		p, err := uuid.Parse(g.UUID)
		if err != nil {
			return
		}
		kind := string(PrincipalUser)
		if g.Kind == PrincipalTeam {
			kind = string(PrincipalTeam)
		}
		if g.Until == nil || g.Level == LevelNone {
			_ = accessModels.ClearExpiry(ctx, c.Resource.Kind, resources, kind, p)
			return
		}
		lvl := g.Level
		if g.Kind == PrincipalTeam {
			lvl = min(lvl, LevelEdit)
		}
		for _, res := range resources {
			_ = accessModels.SetExpiry(ctx, accessModels.Expiry{
				ResourceKind: c.Resource.Kind, ResourceUUID: res, PrincipalKind: kind, PrincipalUUID: p,
				Level: lvl.String(), ExpiresAt: g.Until.UTC(), CreatedBy: by,
			})
		}
	}
	for _, g := range c.Add {
		end(g)
	}
	for _, g := range c.Remove {
		g.Level, g.Until = LevelNone, nil
		end(g)
	}
}

func (rules) CopyOnCreate(ctx context.Context, who Subject, parent Resource, children ...Resource) error {
	if parent.Kind != KindDoc {
		return ErrUnknownKind
	}
	ids := make([]string, 0, len(children))
	for _, c := range children {
		if c.Kind != KindDoc {
			return ErrUnknownKind
		}
		ids = append(ids, c.UUID)
	}
	if len(ids) == 0 {
		return nil
	}
	p, err := docBusiness.ReadSharing(ctx, parent.UUID)
	if err != nil {
		return err
	}
	me := strings.ToLower(who.UserUUID)
	people, teams := inheritedFrom(p, me)
	if err := docBusiness.ApplySharing(ctx, ids, docBusiness.SharingChange{People: people, Teams: teams, General: &p.General}); err != nil {
		return err
	}
	err = endsWithParent(ctx, parent, ids, me)
	for _, id := range ids {
		docBusiness.RepairAccessIndexes(ctx, id)
	}
	return err
}

// endsWithParent gives children the parent's grants that end, ending at the
// same moment: everyone's but owner's (the children's owner, lower case).
// Each one not written is logged, and the first is answered: a grant meant
// to end that doesn't is never silent.
func endsWithParent(ctx context.Context, parent Resource, children []string, owner string) error {
	pu, err := uuid.Parse(parent.UUID)
	if err != nil {
		return ErrNotFound
	}
	ends, err := accessModels.ListExpiry(ctx, parent.Kind, pu)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Access CopyOnCreate under %s: the parent's ending grants weren't read: %+v", parent.UUID, err)
		return err
	}
	var first error
	for _, id := range children {
		cu, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		for _, e := range ends {
			if e.PrincipalKind == string(PrincipalUser) && strings.EqualFold(e.PrincipalUUID.String(), owner) {
				continue
			}
			row := *e
			row.ResourceUUID = cu
			if err := accessModels.SetExpiry(ctx, row); err != nil {
				helpers.LogErrorWithContext(ctx, "business/Access CopyOnCreate %s: a grant that ends on its parent wasn't given its end: %+v", id, err)
				if first == nil {
					first = err
				}
			}
		}
	}
	return first
}

func (r rules) ApplyToSubtree(ctx context.Context, who Subject, c Change) error {
	c.ApplyToSubpages = true
	_, err := Apply(ctx, who, c)
	return err
}

func (r rules) Transfer(ctx context.Context, who Subject, res Resource, toUserUUID string) error {
	d, err := r.Resolve(ctx, who, res)
	if err != nil {
		return err
	}
	switch {
	case d.Deleted:
		return docBusiness.ErrDocDeleted
	case d.Level == LevelNone:
		return ErrNotFound
	case !d.Owner:
		return ErrNotOwner
	}
	_, err = handOn(ctx, res, toUserUUID, who, resourceEventBusiness.DocTransferred, nil)
	return err
}

// handOn makes toUserUUID the owner of a doc; the owner before keeps full
// access. Returns that owner's uuid ("" for none).
func handOn(ctx context.Context, res Resource, toUserUUID string, by Subject, kind resourceEventBusiness.Kind, data map[string]string) (string, error) {
	if _, err := uuid.Parse(toUserUUID); err != nil {
		return "", ErrNoSuchPrincipal
	}
	to := strings.ToLower(toUserUUID)
	before, err := docBusiness.ReadSharing(ctx, res.UUID)
	if err != nil {
		return "", err
	}
	from := ""
	if before.Owner != nil {
		from = strings.ToLower(before.Owner.Uuid)
	}
	if from == to {
		return from, nil
	}
	c := docBusiness.SharingChange{People: map[string]level.Level{to: LevelNone}}
	if from != "" {
		c.People[from] = LevelFull
	}
	if err := docBusiness.HandOverSharing(ctx, res.UUID, to, c); err != nil {
		if errors.Is(err, docBusiness.ErrNoSuchPrincipal) {
			return "", ErrNoSuchPrincipal
		}
		return "", err
	}
	after, err := docBusiness.ReadSharing(ctx, res.UUID)
	if err != nil {
		after = before
	}
	if data == nil {
		data = map[string]string{}
	}
	data["from"], data["to"] = from, to
	docBusiness.AccessChanged(ctx, []string{res.UUID}, docBusiness.Actor{UserUUID: by.UserUUID, AgentUUID: by.AgentUUID}, kind, docBusiness.Explain(before, after), data)
	return from, nil
}

// now is the clock, for tests.
var now = time.Now
