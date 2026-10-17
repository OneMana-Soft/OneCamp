package business

// Kinds other than docs. A doc is decided here (resolver.go, through
// business/Doc's rule); every other kind of resource brings its own rule and
// registers it once, from its package's init:
//
//	func init() { accessBusiness.RegisterKind("channel", channelRules{}) }
//
// so Rules.Resolve, ResolveAll and Filter answer for it like a doc, and this
// package imports none of them (the chat kinds live in business/Conversation,
// the calendar's in business/Calendar).

import (
	"context"
	"fmt"
	"sync"
)

// KindRules decides one kind of resource for one person, many at a time: a
// decision per id that is there, keyed as given; an id that isn't there is
// left out.
type KindRules interface {
	DecideMany(ctx context.Context, who Subject, ids []string) (map[string]Decision, error)
}

var (
	kindsMu sync.RWMutex
	kinds   = map[string]KindRules{}
)

// RegisterKind adds the rule for a kind of resource. A second rule for one
// kind, or one for docs, is a mistake two packages made in parallel, and panics
// where it happened.
func RegisterKind(kind string, r KindRules) {
	if kind == "" || kind == KindDoc || r == nil {
		panic(fmt.Sprintf("access: no rule can be registered for the kind %q", kind))
	}
	kindsMu.Lock()
	defer kindsMu.Unlock()
	if _, taken := kinds[kind]; taken {
		panic(fmt.Sprintf("access: the kind %q is registered twice", kind))
	}
	kinds[kind] = r
}

// rulesFor is a registered kind's rule.
func rulesFor(kind string) (KindRules, bool) {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	r, ok := kinds[kind]
	return r, ok
}

// resolveOthers decides the resources that aren't docs into out, one read per
// kind as its rule allows; ErrUnknownKind for a kind nothing registered.
func resolveOthers(ctx context.Context, who Subject, res []Resource, out map[Resource]Decision) error {
	byKind := map[string][]string{}
	var order []string
	for _, r := range res {
		if _, seen := byKind[r.Kind]; !seen {
			order = append(order, r.Kind)
		}
		byKind[r.Kind] = append(byKind[r.Kind], r.UUID)
	}
	for _, kind := range order {
		rule, ok := rulesFor(kind)
		if !ok {
			return ErrUnknownKind
		}
		got, err := rule.DecideMany(ctx, who, byKind[kind])
		if err != nil {
			return err
		}
		for id, d := range got {
			out[Resource{Kind: kind, UUID: id}] = d
		}
	}
	return nil
}
