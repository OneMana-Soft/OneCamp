package business

// Retention rules and holds (the governance trail's H2): ONE registry for every
// kind of item a workspace keeps or removes on a schedule. The calendar's
// events are the first kind (business/Calendar/retention.go); chat's messages
// join the same way, with no second copy of any of this.
//
// A kind registers what only it knows: how many items a rule would remove, how
// to remove them, and how to find and export a held person's. This package
// does the rest, the same way for every kind:
//
//   - the rule is a policy in the Policy Registry (an owned one: this panel
//     sets it, and shows what it would remove before it can be saved); 0, the
//     default, keeps everything;
//   - holds: admins put people on hold, and no rule removes their items,
//     deleted ones included, until the hold is released (models/postgres/
//     Retention.HeldPeople, which each kind's removal reads inside its own
//     statement);
//   - one sweep a day removes what the rules allow, and writes ONE audit row
//     with its counts;
//   - every hold change, every rule change, and every search or export of a
//     hold's items is a row in the audit log, or it doesn't happen.
//
// Only workspace admins reach any of it (the router's admin group). Holds are
// silent: nothing tells the people on one, and their own views don't change.

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// Kind is one kind of item under retention.
type Kind struct {
	// Name is its key in routes and records ("calendar").
	Name string
	// Label is its items as a count reads ("calendar events").
	Label string
	// Policy is its rule: remove items that ended this many days ago.
	Policy policyBusiness.IntPolicy
	// Count is how many items a rule with cutoff would remove now.
	Count func(ctx context.Context, cutoff time.Time) (int64, error)
	// Purge removes for good at most limit of the items Count counts.
	Purge func(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// Search is people's items matching q, deleted ones included, at most limit.
	Search func(ctx context.Context, people []uuid.UUID, q Query, limit int) ([]Item, error)
	// Export writes ONE part of people's items matching q in format: at most
	// part.Size, in the kind's own order, the first after part.After (its own
	// cursor; empty for the first part). It answers how many it wrote and,
	// when more match past them, Next, the cursor the next part starts after.
	// A file that isn't the whole export says so in itself (PartNote).
	Export func(ctx context.Context, w io.Writer, people []uuid.UUID, q Query, format string, part Part) (Written, error)
	// Formats are what Export writes, first the default ("ics", "json").
	Formats []string
}

// Query narrows a hold's items: words, and a stretch of time (zero: open).
type Query struct {
	Text     string
	From, To time.Time
}

// Part is which part of a hold's export to write: Number from 1, at most Size
// items, starting after After (the kind's cursor; empty for part 1).
type Part struct {
	Number int
	Size   int
	After  string
}

// Written is what one part of an export holds: N items and, when more match
// past them, Next, the kind's cursor the next part starts after.
type Written struct {
	N    int
	Next string
}

// Partial is whether a part is less than the whole export: more come after
// it, or parts came before it.
func (p Part) Partial(w Written) bool { return p.Number > 1 || w.Next != "" }

// PartNote is what a part of an export says about itself, in the file and in
// the audit log, so nobody takes part of a hold's items for all of them:
// empty for a whole export. label is the kind's items as a count reads
// ("calendar events"). Pure.
func PartNote(label string, p Part, w Written) string {
	if !p.Partial(w) {
		return ""
	}
	n := helpers.Thousands(int64(w.N))
	if w.Next == "" {
		more := n + " more " + label
		if w.N == 1 {
			more = "1 more of the " + label
		}
		return fmt.Sprintf("Part %d of this hold's export, the last: %s that match. Parts 1 to %d hold all of them.", p.Number, more, p.Number)
	}
	which := "the first"
	if p.Number > 1 {
		which = "the next"
	}
	return fmt.Sprintf("Part %d of this hold's export: %s %s %s that match, not all of them. More remain: export part %d from the hold's search for the rest.", p.Number, which, n, label, p.Number+1)
}

// Item is one held item as a search lists it.
type Item struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Detail    string     `json:"detail,omitempty"`
	Start     *time.Time `json:"start,omitempty"`
	End       *time.Time `json:"end,omitempty"`
	StartDate string     `json:"start_date,omitempty"`
	EndDate   string     `json:"end_date,omitempty"`
	Repeats   bool       `json:"repeats"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

var (
	kindsMu sync.RWMutex
	kinds   = map[string]Kind{}
)

// Register adds a kind; call it from the kind's package init. A kind missing
// any part is refused, so nothing half-registered ever sweeps.
func Register(k Kind) {
	if k.Name == "" || k.Label == "" || k.Policy.Key == "" || k.Count == nil || k.Purge == nil ||
		k.Search == nil || k.Export == nil || len(k.Formats) == 0 {
		return
	}
	kindsMu.Lock()
	defer kindsMu.Unlock()
	kinds[k.Name] = k
}

// Kinds is every registered kind, by name.
func Kinds() []Kind {
	kindsMu.RLock()
	out := make([]Kind, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k)
	}
	kindsMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// kindNamed is the kind called name.
func kindNamed(name string) (Kind, bool) {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	k, ok := kinds[name]
	return k, ok
}
