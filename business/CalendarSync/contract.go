// Package business (CalendarSync) is calendar sync
// (design/calendar-vs-gcal/PROGRAM.md §4.2 #42 and CAL7; A4 builds the ledger
// and the Syncer, B6 to B8 the adapters): other calendars are copied in and
// kept in step, never read live, and OneCamp's changes reach them through the
// durable outbox.
//
// DAY 0. The adapter contract, with no adapter registered: today Google is
// read live on every page (business/Integration) and nothing syncs. An adapter
// package registers itself from its init:
//
//	func init() { calendarSyncBusiness.Adapters.Register(googleAdapter{}) }
//
// and A4's Syncer asks Adapters.For(provider). The event a change carries is
// today's event input (adapter/Calendar), which A1's model replaces. Nothing
// registers yet, so nothing changes by this existing.
package business

import (
	"context"
	"fmt"
	"sync"
	"time"

	calendarAdapter "github.com/akashc777/OneCamp/adapter/Calendar"
	"github.com/google/uuid"
)

// Link is one person's account at a provider.
type Link struct {
	ID         uuid.UUID
	PersonUUID string
	Provider   string
	// Account is how the provider names it (an email address).
	Account string
}

// Remote is one calendar at the provider, through a link.
type Remote struct {
	Link Link
	// ID is the provider's id for it.
	ID   string
	Name string
}

// Change is one event as the provider has it now.
type Change struct {
	// ExternalID and ETag are the provider's id and version for it.
	ExternalID string
	ETag       string
	// Deleted is an event the provider no longer has.
	Deleted bool
	Event   calendarAdapter.CreateOrUpdateEventInput
}

// Op is one change of OneCamp's to make at the provider.
type Op struct {
	Kind       string // "create", "update" or "delete"
	Remote     Remote
	EventUUID  uuid.UUID
	ExternalID string // the provider's id, for an update or a deletion
	Event      calendarAdapter.CreateOrUpdateEventInput
}

// Ref is where a change landed at the provider.
type Ref struct {
	ExternalID string
	ETag       string
}

// Watch is the provider telling OneCamp of changes to a calendar, until it
// expires or stops.
type Watch struct {
	ID      string
	Remote  Remote
	Expires time.Time
}

// Adapter is one provider: Google (B6), Microsoft Graph (B7), CalDAV and ICS
// feeds (B8).
type Adapter interface {
	// Provider names it: "google", "microsoft", "caldav" or "ics".
	Provider() string
	// Calendars are the link's calendars at the provider.
	Calendars(ctx context.Context, link Link) ([]Remote, error)
	// Pull is what changed in a calendar since cursor ("" for everything),
	// and the cursor to ask from next.
	Pull(ctx context.Context, cal Remote, cursor string) ([]Change, string, error)
	// Push makes one change at the provider.
	Push(ctx context.Context, op Op) (Ref, error)
	// Watch asks the provider to say when a calendar changes; Stop ends it.
	Watch(ctx context.Context, cal Remote) (Watch, error)
	Stop(ctx context.Context, w Watch) error
}

// Registry holds the adapters, one per provider.
type Registry interface {
	// Register adds an adapter, from its package's init. A second one for a
	// provider is a mistake two packages made in parallel, and panics there.
	Register(a Adapter)
	// For is the provider's adapter, if one is registered.
	For(provider string) (Adapter, bool)
}

// Adapters is the registry everything asks.
var Adapters Registry = &registry{byProvider: map[string]Adapter{}}

type registry struct {
	mu         sync.RWMutex
	byProvider map[string]Adapter
}

func (r *registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a == nil || a.Provider() == "" {
		panic("calendar sync: an adapter needs a provider")
	}
	if _, taken := r.byProvider[a.Provider()]; taken {
		panic(fmt.Sprintf("calendar sync: the provider %q is registered twice", a.Provider()))
	}
	r.byProvider[a.Provider()] = a
}

func (r *registry) For(provider string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byProvider[provider]
	return a, ok
}
