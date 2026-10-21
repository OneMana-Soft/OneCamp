package businness

// The work graph's save hook (DG6): a doc's tasks and projects, as its body
// links them, are kept as its refers_to by business/Refs, through the outbox.
// A save that links what the doc's last save on this instance linked writes
// no event (most saves, made while someone types); the first save of a doc
// after a start writes one, so an edge a link removed meanwhile goes too.

import (
	"context"
	"strings"
	"sync"

	refsBusiness "github.com/akashc777/OneCamp/business/Refs"
	"github.com/akashc777/OneCamp/helpers/bodyrefs"
)

func init() {
	SaveHooks.Register("doc.refs", refsSaved)
}

// lastLinks is what each doc's last save linked to (tasks and projects), as
// this instance saw it. Bounded: past refsKept docs it starts over.
var lastLinks = struct {
	sync.Mutex
	seen map[string]string
}{seen: map[string]string{}}

// tellRefs is the work graph's event (seam for tests).
var tellRefs = refsBusiness.Changed

func refsSaved(ctx context.Context, c Change) {
	r := bodyrefs.Scan(c.Body)
	key := strings.Join(r.Of(bodyrefs.KindTask, true), ",") + "|" + strings.Join(r.Of(bodyrefs.KindProject, true), ",")
	lastLinks.Lock()
	prev, ok := lastLinks.seen[c.DocUUID]
	if ok && prev == key {
		lastLinks.Unlock()
		return
	}
	if len(lastLinks.seen) >= refsKept {
		lastLinks.seen = map[string]string{}
	}
	lastLinks.seen[c.DocUUID] = key
	lastLinks.Unlock()
	tellRefs(ctx, refsBusiness.Doc, c.DocUUID)
}
