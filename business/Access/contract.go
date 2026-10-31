package business

// The Access Resolver: the one place that decides how much someone may do with a
// resource (docs programme, PROGRAM.md §4 #12 and invariant I9; A5 built it).
//
// Levels run none < view < comment < edit < full. The sources are people,
// teams, a resource's space (one hop), general access, guests (their links are
// decided by the link's grant) and expiry; the modifiers are deletion, the lock
// and admin recovery. For docs the rule itself is business/Doc's
// (docAccessDecide.go), so every doc check, from opening a doc to joining it
// live, decides alike; this package asks it in batches, explains changes, and
// makes them.
//
// Code that needs a decision asks Rules:
//
//	d, err := accessBusiness.Rules.Resolve(ctx, who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: id})
//
// and a test that needs a different answer sets Rules to a fake and puts it back.

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
)

// Level is how much someone may do (business/Access/level).
type Level = level.Level

const (
	LevelNone    = level.None
	LevelView    = level.View
	LevelComment = level.Comment
	LevelEdit    = level.Edit
	LevelFull    = level.Full
)

// KindDoc is a doc.
const KindDoc = "doc"

// Resource is what a decision is about.
type Resource struct {
	Kind string `json:"kind"`
	UUID string `json:"uuid"`
}

// Subject is who a decision is for. Attribution comes from server context, never
// from the client (I11): when an agent acts, the decision is the person who asked
// it, and AgentUUID records which agent that was.
type Subject struct {
	UserUID   string // the person's graph uid
	UserUUID  string
	AgentUUID string
	// Admin is whether the person is a workspace admin (RequestSubject sets it
	// from the signed-in account). It allows four things no grant does, and
	// only to an admin acting themselves (ActingAdmin): recovering a resource,
	// bringing it back from the trash or deleting it for good (OpenDeleted),
	// removing a comment, and putting pages into a project or team they aren't
	// in. Nothing more comes of being one.
	Admin bool
}

// ActingAdmin is a workspace admin acting themselves, never through an agent:
// the one test for what being an admin allows.
func (s Subject) ActingAdmin() bool {
	return s.Admin && s.AgentUUID == ""
}

// Decision is someone's level on one resource, and why.
type Decision struct {
	// Level is what they may do now: none on a deleted resource, and held at
	// comment by a hard lock unless it is full.
	Level Level `json:"level"`
	// Owner is whether they own it, which a deleted doc keeps: the owner is who
	// restores it.
	Owner bool `json:"owner"`
	// Deleted is whether it is in the trash or archived. Deletion is part of
	// every decision (I9): a deleted resource answers LevelNone outside the
	// trash, which decides by Was.
	Deleted  bool `json:"deleted"`
	Archived bool `json:"archived,omitempty"`
	// Was is their level before it was deleted (Level for a live one): what the
	// trash and the archive show and let them restore.
	Was Level `json:"was"`
	// Locked is a hard lock (D-12), which holds everyone below full at comment.
	Locked bool `json:"locked"`
	// Lock is the lock, soft or hard ("" for none). A soft lock is the
	// editor's guard and changes no level.
	Lock string `json:"lock,omitempty"`
	// Unlocked is the level a hard lock holds them below (Level when it doesn't).
	Unlocked Level `json:"unlocked"`
	// Reasons say where the level comes from, as codes the web app words
	// (level.Reason: "owner", "team_edit", "space"). Never given for none.
	Reasons []string `json:"reasons,omitempty"`
	// Hosted is a doc that belongs to something else, a table row's page
	// (docs D-18): its level is its host's on a path that opens hosted docs
	// (business/Doc WithHostedOpen), and none on any other.
	Hosted bool `json:"hosted,omitempty"`
	// NodeKind is what a doc is when it isn't a page: "folder", "file" or
	// "link" (docs F1), read with the decision. Only a page opens in the
	// editor, so the live session refuses the rest.
	NodeKind string `json:"node_kind,omitempty"`
}

// PrincipalKind is who a grant is to.
type PrincipalKind string

const (
	PrincipalUser  PrincipalKind = "user"
	PrincipalTeam  PrincipalKind = "team"
	PrincipalSpace PrincipalKind = "space"
)

// Grant gives a principal a level on a resource, until a time or for good.
type Grant struct {
	Kind  PrincipalKind `json:"kind"`
	UUID  string        `json:"uuid"`
	Level Level         `json:"level"`
	Until *time.Time    `json:"until,omitempty"`
}

// Change is a change to a resource's sharing.
type Change struct {
	Resource Resource
	Add      []Grant
	// Remove takes these principals' grants away; their Level is ignored.
	Remove []Grant
	// General is what the general audience gets: everyone in the workspace, or
	// with Private, the resource's space. LevelNone makes it private; nil leaves
	// it as it is.
	General *Level
	// Private says whether the general audience is the space only (true) or
	// the whole workspace; nil follows General (private at none, else not).
	Private *bool
	// Replace ends the resource with exactly the grants added (its owner keeps
	// it): a page that adopts its new parent's sharing.
	Replace bool
	// ApplyToSubpages applies the change to every page under the resource too,
	// the ones the person has full access to.
	ApplyToSubpages bool
}

// Effect is one person whose level a change would move.
type Effect struct {
	UserUUID string `json:"user_uuid,omitempty"`
	From     Level  `json:"from"`
	To       Level  `json:"to"`
}

// Explanation is what a change would do before it is made ("3 people lose
// access").
type Explanation struct {
	Effects []Effect `json:"effects"`
	// Everyone is what everyone in the workspace with no grant of their own
	// gets, before and after, when that moves (UserUUID empty).
	Everyone *Effect `json:"everyone,omitempty"`
	// Pages is how many pages the change applies to (more than one with
	// ApplyToSubpages).
	Pages int `json:"pages"`
}

// Resolver is the Access Resolver.
type Resolver interface {
	// Resolve is who's level on res.
	Resolve(ctx context.Context, who Subject, res Resource) (Decision, error)
	// Filter keeps, in order, the resources on which who has at least atLeast.
	Filter(ctx context.Context, who Subject, res []Resource, atLeast Level) ([]Resource, error)
	// ExplainChange says whose level a change would move, without making it.
	ExplainChange(ctx context.Context, who Subject, change Change) (Explanation, error)
	// CopyOnCreate gives new children the parent's sharing: a new subpage copies
	// its parent's (owner decision, PROGRAM.md §3.2), and so do a duplicate's
	// copies that go under it, all from one read of the parent and in one
	// write. who has just created the children, and the caller has already
	// checked that who may add pages under parent.
	CopyOnCreate(ctx context.Context, who Subject, parent Resource, children ...Resource) error
	// ApplyToSubtree makes a change to a resource and everything under it, in one
	// transaction.
	ApplyToSubtree(ctx context.Context, who Subject, change Change) error
	// Transfer makes toUserUUID the owner of res.
	Transfer(ctx context.Context, who Subject, res Resource, toUserUUID string) error
}

// Batch is a resolver that decides many resources in one read. Rules is one;
// ResolveAll asks it when it can.
type Batch interface {
	ResolveAll(ctx context.Context, who Subject, res []Resource) (map[Resource]Decision, error)
}

// ResolveAll is who's decision on each resource, in as few reads as the
// resolver can: a resource that isn't there is left out.
func ResolveAll(ctx context.Context, who Subject, res []Resource) (map[Resource]Decision, error) {
	if b, ok := Rules.(Batch); ok {
		return b.ResolveAll(ctx, who, res)
	}
	out := make(map[Resource]Decision, len(res))
	for _, r := range res {
		d, err := Rules.Resolve(ctx, who, r)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[r] = d
	}
	return out, nil
}

var (
	// ErrUnavailable is an operation a resolver doesn't have.
	ErrUnavailable = errors.New("access: not available yet")
	// ErrUnknownKind is a resource kind with no rules.
	ErrUnknownKind = errors.New("access: no rules for this kind of resource")
	// ErrForbidden is a change the person may not make.
	ErrForbidden = errors.New("access: you can't change who has access to that")
	// ErrNotOwner is a change only the owner may make.
	ErrNotOwner = errors.New("access: only the owner can do that")
	// ErrNoSuchPrincipal is a grant to a person or team that isn't there.
	ErrNoSuchPrincipal = errors.New("access: no such person or team")
)

// Rules is the resolver everything asks.
var Rules Resolver = rules{}

// Subtree lists a resource and everything under it, root first. The tree package
// sets it (business/Tree imports this package, so this one cannot import it);
// until then a resource has nothing under it.
var Subtree = func(_ context.Context, root Resource) ([]Resource, error) {
	return []Resource{root}, nil
}
