// Package business (Places) keeps a person's places: the things they starred or pinned,
// of any kind, in the order they put them; and what they opened lately.
//
// It knows nothing of docs, channels or calendars. Each kind of thing
// registers a Resolver that says, for a person, which of their places they may
// still open and what each is called (Register, from the kind's own package).
// A place on something the person can no longer open, or that's gone, is left
// out of every answer: a list never names what its reader can't open.
//
// Positions are business/Tree/position's fractional keys: moving a star rewrites that
// star alone.
package business

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/business/Tree/position"
	model "github.com/akashc777/OneCamp/models/postgres/Places"
	"github.com/google/uuid"
)

// Kinds of place.
const (
	Star = "star"
	Pin  = "pin"
)

// Person is whom places are read for: their uuid (places are theirs), and
// their graph uid, which a resolver asks the graph with.
type Person struct {
	UUID string
	UID  string
}

// Item is a place as a list shows it.
type Item struct {
	Kind         string `json:"kind"`
	ResourceKind string `json:"resource_kind"`
	ResourceUUID string `json:"resource_uuid"`
	Position     string `json:"position"`
	Title        string `json:"title"`
	Icon         string `json:"icon,omitempty"`
	Href         string `json:"href"`
}

// Seen is what a resolver says of one thing the person may open.
type Seen struct {
	Title string
	Icon  string
	Href  string
}

// Resolver says which of the named things a person may open now, and what each
// is called. Things it leaves out are not listed.
type Resolver interface {
	Resolve(ctx context.Context, who Person, uuids []string) (map[string]Seen, error)
}

var (
	resolversMu sync.RWMutex
	resolvers   = map[string]Resolver{}
)

// Register makes a kind of thing placeable. Called from the kind's package, at
// init: business/Doc registers "doc".
func Register(resourceKind string, r Resolver) {
	resolversMu.Lock()
	defer resolversMu.Unlock()
	resolvers[resourceKind] = r
}

func resolverFor(resourceKind string) (Resolver, bool) {
	resolversMu.RLock()
	defer resolversMu.RUnlock()
	r, ok := resolvers[resourceKind]
	return r, ok
}

// Errors a change answers with.
var (
	ErrKind      = errors.New("places: a place is a star or a pin")
	ErrResource  = errors.New("places: that kind of thing can't be placed")
	ErrCantOpen  = errors.New("places: you can't open that")
	ErrNotPlaced = errors.New("places: the place to follow isn't in your list")
)

func validKind(kind string) bool { return kind == Star || kind == Pin }

// Add places a thing the person may open, right after another of their places
// of the same kind (or last), or moves it there if it's placed already.
func Add(ctx context.Context, who Person, kind, resourceKind, resourceUUID, afterUUID string) (*Item, error) {
	if !validKind(kind) {
		return nil, ErrKind
	}
	r, ok := resolverFor(resourceKind)
	if !ok {
		return nil, ErrResource
	}
	userID, err := uuid.Parse(who.UUID)
	if err != nil {
		return nil, err
	}
	seen, err := r.Resolve(ctx, who, []string{resourceUUID})
	if err != nil {
		return nil, err
	}
	s, ok := seen[resourceUUID]
	if !ok {
		return nil, ErrCantOpen
	}
	list, err := model.List(ctx, userID, kind, "")
	if err != nil {
		return nil, err
	}
	sibs := make([]position.Sibling, 0, len(list))
	for _, p := range list {
		sibs = append(sibs, position.Sibling{ID: p.ResourceUUID, Position: p.Position, CreatedAt: p.CreatedAt})
	}
	position.Order(sibs)
	// No place to follow: last (Place leaves the thing itself out of the list).
	placed, err := position.Place(sibs, resourceUUID, position.Where{After: afterUUID})
	if errors.Is(err, position.ErrNotSibling) {
		return nil, ErrNotPlaced
	}
	if err != nil {
		return nil, err
	}
	for id, key := range placed.Renumber {
		if _, err := model.Put(ctx, &model.Place{UserUUID: userID, Kind: kind, ResourceKind: kindOf(list, id), ResourceUUID: id, Position: key}); err != nil {
			return nil, err
		}
	}
	p, err := model.Put(ctx, &model.Place{UserUUID: userID, Kind: kind, ResourceKind: resourceKind, ResourceUUID: resourceUUID, Position: placed.Key})
	if err != nil {
		return nil, err
	}
	return &Item{Kind: p.Kind, ResourceKind: p.ResourceKind, ResourceUUID: p.ResourceUUID, Position: p.Position, Title: s.Title, Icon: s.Icon, Href: s.Href}, nil
}

func kindOf(list []*model.Place, id string) string {
	for _, p := range list {
		if p.ResourceUUID == id {
			return p.ResourceKind
		}
	}
	return ""
}

// Remove takes a place away; one the person doesn't have is gone already.
func Remove(ctx context.Context, who Person, kind, resourceUUID string) error {
	if !validKind(kind) {
		return ErrKind
	}
	userID, err := uuid.Parse(who.UUID)
	if err != nil {
		return err
	}
	return model.Remove(ctx, userID, kind, resourceUUID)
}

// List is the person's places of one kind (resourceKind "" for every kind of
// thing, mixed, as the Starred list shows them), in their order, each named
// by its kind's resolver; places on things they can't open are left out.
func List(ctx context.Context, who Person, kind, resourceKind string) ([]Item, error) {
	if !validKind(kind) {
		return nil, ErrKind
	}
	userID, err := uuid.Parse(who.UUID)
	if err != nil {
		return nil, err
	}
	rows, err := model.List(ctx, userID, kind, resourceKind)
	if err != nil {
		return nil, err
	}
	sibs := make([]position.Sibling, 0, len(rows))
	byID := map[string]*model.Place{}
	byKind := map[string][]string{}
	for _, p := range rows {
		sibs = append(sibs, position.Sibling{ID: p.ResourceUUID, Position: p.Position, CreatedAt: p.CreatedAt})
		byID[p.ResourceUUID] = p
		byKind[p.ResourceKind] = append(byKind[p.ResourceKind], p.ResourceUUID)
	}
	position.Order(sibs)
	seen := map[string]Seen{}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		r, ok := resolverFor(k)
		if !ok {
			continue // a kind this build doesn't have (an edition without it)
		}
		got, err := r.Resolve(ctx, who, byKind[k])
		if err != nil {
			return nil, err
		}
		for id, s := range got {
			seen[id] = s
		}
	}
	out := make([]Item, 0, len(sibs))
	for _, s := range sibs {
		v, ok := seen[s.ID]
		if !ok {
			continue
		}
		p := byID[s.ID]
		out = append(out, Item{Kind: p.Kind, ResourceKind: p.ResourceKind, ResourceUUID: p.ResourceUUID, Position: p.Position, Title: v.Title, Icon: v.Icon, Href: v.Href})
	}
	return out, nil
}

// IDs is the ids of the person's places of one kind and kind of thing, in
// their order, unresolved: for a list that reads the things itself (a doc
// list's Starred scope) and filters them by its own rule.
func IDs(ctx context.Context, personUUID, kind, resourceKind string) ([]string, error) {
	if !validKind(kind) {
		return nil, ErrKind
	}
	userID, err := uuid.Parse(personUUID)
	if err != nil {
		return nil, err
	}
	rows, err := model.List(ctx, userID, kind, resourceKind)
	if err != nil {
		return nil, err
	}
	sibs := make([]position.Sibling, 0, len(rows))
	for _, p := range rows {
		sibs = append(sibs, position.Sibling{ID: p.ResourceUUID, Position: p.Position, CreatedAt: p.CreatedAt})
	}
	position.Order(sibs)
	out := make([]string, len(sibs))
	for i, s := range sibs {
		out[i] = s.ID
	}
	return out, nil
}

// Recent is what the person opened lately of a kind that keeps a view log
// (resource_views: "doc" and "board"), newest first: since a moment, at most
// limit. Unresolved, as IDs.
func Recent(ctx context.Context, personUUID, resourceKind string, since time.Time, limit int) ([]model.View, error) {
	return model.RecentViews(ctx, personUUID, resourceKind, since, limit)
}

// Forget takes everyone's places on things gone for good.
func Forget(ctx context.Context, resourceKind string, uuids []string) error {
	return model.RemoveEverywhere(ctx, resourceKind, uuids)
}
