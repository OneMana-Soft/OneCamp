package businness

// A doc's search entries are written from the doc (domain/Doc searchEntry.go
// says why and how); global search asks the doc about every doc hit
// (ReadableDocsFor, below), and so does the activity feed about every comment
// on a doc it shows.

import (
	"context"
	"strings"
	"time"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	domain "github.com/akashc777/OneCamp/domain/Doc"
)

func init() {
	// The feed can't import this package, which imports it, so it is handed
	// the doc's read check here (business/Activity readable.go).
	activityBusiness.RegisterDocReadCheck(ReadableDocsFor)
}

// syncDocSearch has the doc's search entry written whole from the doc, and
// with children its comments' and files' entries' fields for it.
func syncDocSearch(docUUID string, children bool) {
	domain.SyncDocSearch(docUUID, children)
}

// WaitForSearchWrites waits until no doc's search entry is being written or
// waiting its turn, for at most timeout or until ctx ends, and returns how
// many docs' still are. Those writes run in the background, so a command
// that changed docs waits on this before it exits (repairdocs).
func WaitForSearchWrites(ctx context.Context, timeout time.Duration) int {
	return domain.WaitDocSearchWrites(ctx, timeout)
}

// ReadableDocsFor answers, of the named docs, which the person (by uuid) may
// read now, by CanRead, keyed by lower-case uuid: a doc that is private and
// not theirs to see, deleted, or not there at all is false (or absent). One
// graph query for the lot. An error is the graph not answering, and says
// nothing about any doc.
func ReadableDocsFor(ctx context.Context, docUUIDs []string, userUUID string) (map[string]bool, error) {
	out := make(map[string]bool, len(docUUIDs))
	if len(docUUIDs) == 0 {
		return out, nil
	}
	docs, err := domain.GetDocsReadAccessFor(ctx, docUUIDs, userUUID)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if d != nil && d.Uuid != "" {
			out[strings.ToLower(d.Uuid)] = CanRead(d, userUUID)
		}
	}
	return out, nil
}
