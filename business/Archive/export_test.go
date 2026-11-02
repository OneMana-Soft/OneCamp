package business

// An archive held in memory, put behind every store the archive list and its
// restore read and write, so a test outside this package (one that needs the
// handlers: restoreRoundTrip_test.go) lists and restores through the archive's
// own code with no database. Only tests build this file.

import (
	"context"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	archiveModels "github.com/akashc777/OneCamp/models/postgres/Archive"
)

// FakeArchive is an archive held in memory: each kind's items, by the id the
// kind's list gives, archived since a time or live, and the ids each kind's
// restore write was given.
type FakeArchive struct {
	mu     sync.Mutex
	items  map[string]map[string]*time.Time // kind -> id -> archived since (nil: live)
	writes map[string][]string              // kind -> the ids its restore write got, in order
}

// UseFakeArchive puts a FakeArchive behind the archive's stores for the test's
// life: the rows, the runs (none under way), the docs and the recordings, and
// the graph, search and memory a restore tells (which do nothing).
func UseFakeArchive(t testing.TB) *FakeArchive {
	t.Helper()
	f := &FakeArchive{items: map[string]map[string]*time.Time{}, writes: map[string][]string{}}

	prevRunning, prevArchived, prevRestore, prevLive, prevKnown := runningArchiveJob, archivedRows, restoreRows, liveRows, knownRows
	prevRecList, prevRecKeys, prevRecNodes := recentlyArchivedRecordings, recordingKeysHere, restoreRecordingNodes
	prevDocList, prevDocRead, prevDocDeletion, prevDocNodes := recentlyDeletedDocs, docsCouldRead, docsDeletion, restoreDocNodes
	prevLinks, prevTell, prevDocSearch := turnOffDocLinks, tellOwnerLinksOff, docSearchRestore
	prevRegistry := entityRegistry
	t.Cleanup(func() {
		runningArchiveJob, archivedRows, restoreRows, liveRows, knownRows = prevRunning, prevArchived, prevRestore, prevLive, prevKnown
		recentlyArchivedRecordings, recordingKeysHere, restoreRecordingNodes = prevRecList, prevRecKeys, prevRecNodes
		recentlyDeletedDocs, docsCouldRead, docsDeletion, restoreDocNodes = prevDocList, prevDocRead, prevDocDeletion, prevDocNodes
		turnOffDocLinks, tellOwnerLinksOff, docSearchRestore = prevLinks, prevTell, prevDocSearch
		entityRegistry = prevRegistry
	})

	// The kinds as registered, with the graph, search and memory a restore
	// tells standing still.
	registry := make(map[string]entityOps, len(prevRegistry))
	for kind, ops := range prevRegistry {
		if ops.RestoreDgraphFn != nil {
			ops.RestoreDgraphFn = func(context.Context, []string) error { return nil }
		}
		ops.OpenSearchRestoreFn = func([]string) {}
		ops.MemorySourceType = ""
		registry[kind] = ops
	}
	entityRegistry = registry

	runningArchiveJob = func(context.Context, string) (bool, error) { return false, nil }

	// Posts, chats, tasks and attachments: rows of the table the kind names.
	archivedRows = func(_ context.Context, table string, since time.Time, limit, offset int) ([]archiveModels.Archived, int64, error) {
		var rows []archiveModels.Archived
		for _, id := range f.archivedSince(table, since) {
			rows = append(rows, archiveModels.Archived{ID: id, At: f.since(table, id)})
		}
		return pageOf(rows, limit, offset), int64(len(rows)), nil
	}
	restoreRows = func(_ context.Context, table string, ids []string) ([]string, error) {
		return f.restore(table, ids), nil
	}
	liveRows = func(_ context.Context, table string, ids []string) ([]string, error) {
		return f.live(table, ids), nil
	}
	knownRows = func(_ context.Context, table string, ids []string) ([]string, error) {
		return f.known(table, ids), nil
	}

	// Docs: nodes by their uuid, archived by the retention (so their guest
	// links stay as they are), and every one readable by the admin asking.
	recentlyDeletedDocs = func(_ context.Context, cutoff string, first, offset int) ([]*dgraphStruct.DgraphDoc, int64, error) {
		var docs []*dgraphStruct.DgraphDoc
		for _, id := range f.archivedSince("docs", parseCutoff(cutoff)) {
			docs = append(docs, f.doc(id))
		}
		return pageOf(docs, first, offset), int64(len(docs)), nil
	}
	docsCouldRead = func(_ context.Context, ids []string, _ string) (map[string]bool, error) {
		readable := make(map[string]bool, len(ids))
		for _, id := range ids {
			readable[id] = true
		}
		return readable, nil
	}
	docsDeletion = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphDoc, error) {
		var docs []*dgraphStruct.DgraphDoc
		for _, id := range f.known("docs", ids) {
			docs = append(docs, f.doc(id))
		}
		return docs, nil
	}
	restoreDocNodes = func(_ context.Context, ids []string, _ bool) ([]string, error) {
		return f.restore("docs", ids), nil
	}
	turnOffDocLinks = func(context.Context, string) (int64, error) { return 0, nil }
	tellOwnerLinksOff = func(context.Context, *dgraphStruct.DgraphDoc, int64, string) {}
	docSearchRestore = func([]string) {}

	// Recordings: nodes by their key, an egress id or a call's session key.
	recentlyArchivedRecordings = func(_ context.Context, cutoff string, first, offset int) ([]*dgraphStruct.DgraphRecording, int64, error) {
		var recs []*dgraphStruct.DgraphRecording
		for _, key := range f.archivedSince("recordings", parseCutoff(cutoff)) {
			at := f.since("recordings", key)
			recs = append(recs, &dgraphStruct.DgraphRecording{EgressId: key, DeletedAt: &at})
		}
		return pageOf(recs, first, offset), int64(len(recs)), nil
	}
	recordingKeysHere = func(_ context.Context, keys []string) ([]string, error) {
		return f.known("recordings", keys), nil
	}
	restoreRecordingNodes = func(_ context.Context, keys []string) error {
		f.restore("recordings", keys)
		return nil
	}
	return f
}

// Archive puts an item of kind in the archive, archived at at, by the id the
// kind's list gives.
func (f *FakeArchive) Archive(kind, id string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.items[kind] == nil {
		f.items[kind] = map[string]*time.Time{}
	}
	f.items[kind][id] = &at
}

// IsLive is whether the archive holds this item of kind, live.
func (f *FakeArchive) IsLive(kind, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.items[kind][id]
	return ok && at == nil
}

// Writes are the ids kind's restore write was given, in order.
func (f *FakeArchive) Writes(kind string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes[kind])
}

// restore is a restore write of kind: it takes ids, brings back those archived
// and answers which it brought back.
func (f *FakeArchive) restore(kind string, ids []string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes[kind] = append(f.writes[kind], ids...)
	var restored []string
	for _, id := range ids {
		if at, ok := f.items[kind][id]; ok && at != nil {
			f.items[kind][id] = nil
			restored = append(restored, id)
		}
	}
	return restored
}

// known is which of ids the archive holds an item of kind for, archived or
// live, in ids' order.
func (f *FakeArchive) known(kind string, ids []string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, id := range ids {
		if _, ok := f.items[kind][id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// live is which of ids the archive holds a live item of kind for.
func (f *FakeArchive) live(kind string, ids []string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, id := range ids {
		if at, ok := f.items[kind][id]; ok && at == nil {
			out = append(out, id)
		}
	}
	return out
}

// archivedSince is kind's items archived after since, newest first, as the
// lists order them.
func (f *FakeArchive) archivedSince(kind string, since time.Time) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id, at := range f.items[kind] {
		if at != nil && at.After(since) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := f.items[kind][ids[i]], f.items[kind][ids[j]]
		if !a.Equal(*b) {
			return a.After(*b)
		}
		return ids[i] < ids[j]
	})
	return ids
}

// since is when kind's item id was archived (the zero time for a live one).
func (f *FakeArchive) since(kind, id string) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if at := f.items[kind][id]; at != nil {
		return *at
	}
	return time.Time{}
}

// doc is the archive's doc id as the graph reads it: archived by the retention
// while it is in the archive, and whose it is.
func (f *FakeArchive) doc(id string) *dgraphStruct.DgraphDoc {
	d := &dgraphStruct.DgraphDoc{
		Uuid:      id,
		Title:     "Launch plan",
		CreatedBy: &dgraphStruct.DgraphUser{Uuid: "dana", UserName: "Dana Whitfield"},
	}
	if at := f.since("docs", id); !at.IsZero() {
		d.DeletedAt, d.ArchivedAt = &at, &at
	}
	return d
}

// parseCutoff reads a list's RFC 3339 cutoff.
func parseCutoff(cutoff string) time.Time {
	at, _ := time.Parse(time.RFC3339, cutoff)
	return at
}

// pageOf is the page of all that starts at offset and holds at most limit.
func pageOf[T any](all []T, limit, offset int) []T {
	if offset >= len(all) {
		return nil
	}
	return all[offset:min(offset+limit, len(all))]
}
