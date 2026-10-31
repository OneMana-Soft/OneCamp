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
// the codec's (business/ICS): a provider's event read as iCalendar reads it,
// so whatever a provider sends comes in through the calendar's one import
// path (business/Calendar importICS.go), as a file does.
package business

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	ICS "github.com/akashc777/OneCamp/business/ICS"
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
	// Primary is the link's own calendar (Google's named by the address).
	Primary bool
	// Zone is the zone its events are in when they name none.
	Zone string
	// Reminders are the minutes before an event it reminds by default.
	Reminders []int
	// Source is where a subscription is read from (calendar B8): the address
	// a calendar publishes, for the "ics" adapter. A secret where it is a
	// private link: never logged.
	Source string
	// ReadOnly is a calendar the link may read but not change (shared with
	// it to see only): nothing is pushed to it.
	ReadOnly bool
}

// ErrFullPull is a cursor the provider no longer knows: everything is
// pulled again, from "".
var ErrFullPull = errors.New("calendar sync: the provider asks for a full pull")

// What an adapter's errors say, so sync can tell a change that won't be taken
// and a link that's gone from a provider that is only busy or down for now,
// which is tried again later. An adapter wraps its own error in one of them.
var (
	// ErrRefused is a change the provider won't take as it is: sent again
	// unchanged, it would be refused again.
	ErrRefused = errors.New("calendar sync: the provider refused the change")
	// ErrReadOnly is a change to a calendar the link may only read. It is a
	// refusal too.
	ErrReadOnly = fmt.Errorf("%w: the calendar can only be read", ErrRefused)
	// ErrRevoked is a link the provider no longer answers for: its access
	// was revoked or the account disconnected. Nothing syncs until the
	// person connects it again.
	ErrRevoked = errors.New("calendar sync: the provider no longer answers for this account")
)

// Change is one event as the provider has it now.
type Change struct {
	// ExternalID and ETag are the provider's id and version for it.
	ExternalID string
	ETag       string
	// Deleted is an event the provider no longer has.
	Deleted bool
	// SeriesID, on one instance of a series changed on its own, is the
	// provider's id of its series.
	SeriesID string
	Event    ICS.Event
}

// Op is one change of OneCamp's to make at the provider.
type Op struct {
	Kind       string // "create", "update" or "delete"
	Remote     Remote
	EventUUID  uuid.UUID
	ExternalID string // the provider's id, for an update or a deletion
	Event      ICS.Event
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
