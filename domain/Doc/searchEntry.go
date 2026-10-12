package domain

// A doc's entries in global search, written from the doc.
//
// Global search finds a doc by its entry in the docs index, and a doc's
// comments and files by theirs, which carry the doc's title, privacy and
// sharing. Those entries were patched with what each change carried, through a
// shape that leaves out false and empty values. So no public doc ever said it
// was public (nobody but the people it was shared with found one: on the demo,
// every one of the 14 entries lacked it), a doc made public again stayed
// private, a person a doc stopped being shared with kept finding its title and
// text, any save brought a deleted doc's entry back to life, and an entry the
// index never had (OpenSearch down at the time, a restore) was never made.
//
// Now an entry is written whole, from what the graph says the doc is at that
// moment: its text, who made it, who it is shared with (empty lists written as
// empty), and whether it is deleted. A change asks for that write; a write
// that finds newer changes waiting reads the doc again and writes again, so
// the last write is always of the doc as it last was. Search also checks every
// doc hit against the graph before showing it (business/GlobalSearch), so an
// entry that is still behind can cost a result, never a private doc.
//
// Here rather than in business/Doc because the workspace's retention archive
// (business/Archive) moves docs too, and business/Doc already depends on it.

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	openSearchGlobalSearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
)

// searchSnippetRunes bounds the preview an entry is given when the doc has no
// stored one (business/Doc stores one of the same length on every save).
const searchSnippetRunes = 1000

// DocSearchEntryOf is a doc's whole entry in the docs index, from the doc
// read as GetDocSearchEntry reads it. Pure.
func DocSearchEntryOf(doc *dgraphStruct.DgraphDoc) *openSearchStruct.OpenSearchDocEntry {
	uuids := func(users []*dgraphStruct.DgraphUser) []string {
		out := []string{}
		for _, u := range users {
			if u != nil && u.Uuid != "" {
				out = append(out, u.Uuid)
			}
		}
		return out
	}
	unix := func(t *time.Time) int64 {
		if t == nil || !t.After(time.Unix(0, 0)) {
			return 0
		}
		return t.Unix()
	}
	e := &openSearchStruct.OpenSearchDocEntry{
		Uuid:     doc.Uuid,
		DocTitle: doc.Title,
		// The body as plain text, never markup: it is a field to match, not to
		// return, and an image's bytes live in an attribute (business/Doc
		// UpdateDoc's note).
		DocBody:            helpers.HTMLToPlainText(doc.Body),
		DocSnippet:         doc.Snippet,
		DocPrivate:         doc.IsPrivate != nil && *doc.IsPrivate,
		DocReadingUsers:    uuids(doc.ReadingUser),
		DocEditingUsers:    uuids(doc.EditingUser),
		DocCommentingUsers: uuids(doc.CommentingUser),
		DocCreatedAt:       unix(doc.CreatedAt),
		DocUpdatedAt:       unix(doc.UpdatedAt),
	}
	switch {
	case e.DocSnippet == "" && doc.Body != "":
		e.DocSnippet = helpers.TruncateRunes(e.DocBody, searchSnippetRunes)
	case strings.HasPrefix(strings.TrimSpace(e.DocSnippet), "<"):
		// A preview stored before previews were made from plain text is
		// markup ("<p xmlns=…>"), which a result would show as such.
		e.DocSnippet = helpers.TruncateRunes(helpers.HTMLToPlainText(e.DocSnippet), searchSnippetRunes)
	}
	if e.DocUpdatedAt == 0 {
		e.DocUpdatedAt = e.DocCreatedAt
	}
	if doc.CreatedBy != nil {
		e.DocCreatedByUserUuid = doc.CreatedBy.Uuid
		e.DocCreatedByFullName = doc.CreatedBy.DisplayName()
		e.DocCreatedByProfile = doc.CreatedBy.ProfileKey
	}
	// Deleted is a deletion time after 1970: a live doc carries the zero time.
	if helpers.IsSoftDeleted(doc.DeletedAt) {
		at := doc.DeletedAt.Unix()
		e.DocDeletedAt = &at
	}
	return e
}

// docSearchSync is what is waiting to be written for one doc: a write is
// running, and another is wanted after it (again), with its comments' and
// files' entries too (children).
type docSearchSync struct {
	again, children bool
}

const (
	// docSearchWriters is how many docs' entries are written at once; the
	// rest wait their turn. Each write reads the doc's whole body from the
	// graph and sends it to search, and the retention archive and restore
	// ask for one per doc they move, up to 5000 a run: all at once, that was
	// thousands of bodies in memory together, more than a 4 GB install has.
	docSearchWriters = 8
	// docSearchWriteTimeout bounds one write. Neither the graph's client nor
	// search's gives up on its own, so a request that hung kept its doc
	// marked as being written for good, and its entry was never written
	// again.
	docSearchWriteTimeout = 2 * time.Minute
)

var (
	docSearchSyncMu sync.Mutex
	docSearchSyncs  = map[string]*docSearchSync{}
	// docSearchSlots holds a token for each write running.
	docSearchSlots = make(chan struct{}, docSearchWriters)
)

// SyncDocSearch has a doc's search entry written from the doc, in the
// background, and, with children, its comments' and files' entries too (what
// they carry of it: its title, privacy and sharing, and who made it). Asked
// while a write for the doc is running or waiting its turn, it waits for that
// one and writes once more after it, however many times it was asked
// meanwhile.
func SyncDocSearch(docUUID string, children bool) {
	if docUUID == "" {
		return
	}
	docSearchSyncMu.Lock()
	if s, running := docSearchSyncs[docUUID]; running {
		s.again = true
		s.children = s.children || children
		docSearchSyncMu.Unlock()
		return
	}
	s := &docSearchSync{children: children}
	docSearchSyncs[docUUID] = s
	docSearchSyncMu.Unlock()
	goDocSearchSync(docUUID, s)
}

// goDocSearchSync writes a doc's entry in the background, again while more
// writes are asked for, and then lets go of the doc.
func goDocSearchSync(docUUID string, s *docSearchSync) {
	helpers.GoSafeNamed("doc.search-entry", func() {
		withChildren := false
		// A write that panicked must not leave the doc marked as being
		// written: every later ask only marked it again, and its entry was
		// not written until a restart. Asks that came during it are written
		// by a fresh run, the doc marked throughout; with none, it is let go.
		defer func() {
			docSearchSyncMu.Lock()
			defer docSearchSyncMu.Unlock()
			if docSearchSyncs[docUUID] != s {
				return // let go of as usual
			}
			if !s.again {
				delete(docSearchSyncs, docUUID)
				return
			}
			s.children = s.children || withChildren
			goDocSearchSync(docUUID, s)
		}()
		for {
			docSearchSyncMu.Lock()
			withChildren = s.children
			s.again, s.children = false, false
			docSearchSyncMu.Unlock()

			if err := writeDocSearchInTurn(docUUID, withChildren); err != nil {
				helpers.MessageLogs.ErrorLog.Printf("search: could not write doc %s's entry: %v", docUUID, err)
			}

			docSearchSyncMu.Lock()
			if !s.again {
				delete(docSearchSyncs, docUUID)
				docSearchSyncMu.Unlock()
				return
			}
			docSearchSyncMu.Unlock()
		}
	})
}

// writeDocSearchInTurn runs one write once fewer than docSearchWriters are
// running, and gives it docSearchWriteTimeout.
func writeDocSearchInTurn(docUUID string, children bool) error {
	docSearchSlots <- struct{}{}
	defer func() { <-docSearchSlots }()
	ctx, cancel := context.WithTimeout(context.Background(), docSearchWriteTimeout)
	defer cancel()
	return writeDocSearch(ctx, docUUID, children)
}

// WaitDocSearchWrites waits until no doc's search entry is being written or
// waiting its turn, for at most timeout or until ctx ends, and returns how
// many docs' still are (0: all written). For a command that changed docs and
// is about to exit, which would cut those writes off (repairdocs).
func WaitDocSearchWrites(ctx context.Context, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for {
		docSearchSyncMu.Lock()
		pending := len(docSearchSyncs)
		docSearchSyncMu.Unlock()
		if pending == 0 || ctx.Err() != nil || !time.Now().Before(deadline) {
			return pending
		}
		select {
		case <-ctx.Done():
		case <-time.After(min(25*time.Millisecond, time.Until(deadline))):
		}
	}
}

// writeDocSearch reads the doc and writes its entry whole, and with children
// its comments' and files' entries. A seam for tests.
var writeDocSearch = func(ctx context.Context, docUUID string, children bool) error {
	doc, err := GetDocSearchEntry(ctx, docUUID)
	if err != nil {
		return err
	}
	if doc == nil || doc.Uuid == "" {
		return ErrNotFound
	}
	if err := IndexDocEntry(ctx, DocSearchEntryOf(doc)); err != nil {
		return err
	}
	if !children {
		return nil
	}
	e := DocSearchEntryOf(doc)
	return openSearchGlobalSearchModels.SyncDocMetadataInOpenSearch(ctx, doc.Uuid, &e.DocTitle, &e.DocPrivate,
		e.DocReadingUsers, e.DocEditingUsers, e.DocCommentingUsers, e.DocCreatedByUserUuid)
}
