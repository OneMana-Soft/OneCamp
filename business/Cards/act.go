package business

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"sync"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// Who acts: a member, by uuid (from the session, never the client).
type Who struct {
	UserUUID string
}

// Host is where cards live (§3.5): it says whether someone may act on one of
// its cards. A host that posts cards (chat's messages, a task's comments)
// brings Post and Update with it.
type Host interface {
	Kind() string
	MayAct(ctx context.Context, who Who, ref CardRef) bool
}

// Action is one press on a card, checked and on its way to its owner.
type Action struct {
	Who      Who
	Ref      CardRef
	Owner    Owner
	ActionID string
	Value    string
	// TriggerID lets the owner open a modal for this press, once, within
	// TriggerLife.
	TriggerID string
}

// Reply is what an owner answers a press with. It is the answer a command
// gives (the card to show instead, a note, a client action), so the web
// shows a press's answer exactly as it shows a command's. An owner that
// answers later (an app, over MQTT) replies nil.
type Reply = commandAdapter.CommandResponse

// Route takes a press to an owner of one kind: an app's address, a built-in
// handler, a workflow step.
type Route func(ctx context.Context, a Action) (*Reply, error)

// TriggerLife is how long a press's trigger can open a modal (§3.5).
const TriggerLife = 3 * time.Second

var (
	mu     sync.RWMutex
	hosts  = map[string]Host{}
	owners = map[string]Route{}
)

// RegisterHost adds a host of cards, from its package's init.
func RegisterHost(h Host) {
	mu.Lock()
	defer mu.Unlock()
	hosts[h.Kind()] = h
}

// RegisterOwner adds how presses reach one kind of owner, from its package's
// init: business/Command registers "app" and "builtin".
func RegisterOwner(kind string, r Route) {
	mu.Lock()
	defer mu.Unlock()
	owners[kind] = r
}

// Act checks a press on the server and takes it to the card's owner: the
// card is ours (sealed, not expired), the action is one it has, its host lets
// who act on it, and who is one of the card's actors when it names any.
func Act(ctx context.Context, who Who, token, actionID, value string) (*Reply, error) {
	s, err := open(token)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(s.Actions, actionID) {
		return nil, ErrNoAction
	}
	mu.RLock()
	host, hostOK := hosts[s.Ref.Host]
	route, routeOK := owners[s.Owner.Kind]
	mu.RUnlock()
	if who.UserUUID == "" || !hostOK || !host.MayAct(ctx, who, s.Ref) {
		return nil, ErrNotYours
	}
	if len(s.Actors) > 0 && !slices.Contains(s.Actors, who.UserUUID) {
		return nil, ErrNotYours
	}
	if !routeOK {
		return nil, ErrNoOwner
	}
	a := Action{Who: who, Ref: s.Ref, Owner: s.Owner, ActionID: actionID, Value: value, TriggerID: mintTrigger(ctx, who, s)}
	return route(ctx, a)
}

// mintTrigger is a press's one-time trigger: GETDEL'd by whoever opens a
// modal with it, within TriggerLife. Without Redis a press has none, and an
// owner can answer it but not open a modal.
func mintTrigger(ctx context.Context, who Who, s sealed) string {
	if !redisStore.IsAvailable() {
		return ""
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	id := hex.EncodeToString(b)
	if err := redisStore.SetStringWithTTL(ctx, registry.CardTrigger, []string{id}, who.UserUUID+" "+s.Owner.Kind+":"+s.Owner.ID, TriggerLife); err != nil {
		return ""
	}
	return id
}

// ephemeral is the host of a card shown to one person (a command's answer):
// its actors say who, so the host itself allows any member to be checked.
type ephemeral struct{}

func (ephemeral) Kind() string                                      { return HostEphemeral }
func (ephemeral) MayAct(_ context.Context, who Who, _ CardRef) bool { return who.UserUUID != "" }

// HostEphemeral is the host of a card one person sees, sealed with them as its
// only actor.
const HostEphemeral = "ephemeral"

func init() { RegisterHost(ephemeral{}) }
