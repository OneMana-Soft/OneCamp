package businness

// Every doc's entries in global search, rewritten from the docs, once.
//
// Until this release a doc's entry was patched with what each change carried,
// through a shape that leaves out false and empty values, and a new comment on
// a doc was written as deleted and without its doc's sharing (searchEntry.go
// says what that cost). Writes now keep entries right, and search checks each
// doc hit against the graph; this rewrites what the index already holds:
//
//   - every doc's own entry, whole, from the doc: who may find it (public docs
//     said nothing, so nobody but their editors found them), who it is shared
//     with now, and whether it is deleted; a doc the index never had (written
//     while search was down, or before a restore) gets one;
//   - its comments' and files' entries, with its title, privacy, sharing and
//     maker; and a comment on a live doc written as deleted (-1) is findable.
//
// It runs in the background at startup until it has succeeded once, which the
// system_configs key below records, so later starts skip it. Until then it
// retries with a growing wait (search or the graph may still be starting).
// Running it twice is harmless: it writes what the docs say, and the second
// time finds nothing to change. A startup job rather than an admin command, so
// that every install is fixed by upgrading, without anyone having to know to
// run it. No migration: the docs are in the graph and the entries in search.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	openSearchGlobalSearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

const (
	docSearchRepairKey  = "docs_search_entries_v2"
	docSearchRepairPage = 100
	// docSearchRepairTries bounds the retries in one process: about three and
	// a half hours of waits, then the next start tries again.
	docSearchRepairTries = 12
)

// The graph as the repair reads it and search as it writes it. Seams for
// tests.
var (
	docSearchEntryPage  = domain.GetDocSearchEntryPage
	docUUIDPage         = domain.GetDocUUIDPage
	bulkIndexDocEntries = domain.BulkIndexDocEntries
	repairDocChildren   = openSearchGlobalSearchModels.RepairDocChildren
)

// DocSearchRepair is what one rewrite of every doc's entries did.
type DocSearchRepair struct {
	Docs     int // entries written, one per doc
	Live     int // of which for live docs
	Deleted  int // and for deleted (or archived) ones
	Children int // comment and file entries that carried something wrong of their doc
	Revived  int // comments on live docs that were written as deleted, findable now
	Skipped  int // docs too large to read, their entries left as they were
}

func (r DocSearchRepair) String() string {
	return fmt.Sprintf("%d doc entries written (%d live, %d deleted); %d comment and file entries corrected, %d comments findable again; %d docs too large to read, skipped",
		r.Docs, r.Live, r.Deleted, r.Children, r.Revived, r.Skipped)
}

// StartDocSearchRepair runs RunDocSearchRepair in the background until it
// succeeds, ctx ends, or the tries run out.
func StartDocSearchRepair(ctx context.Context) {
	helpers.GoSafeNamed("doc.search-repair", func() {
		wait := 30 * time.Second
		for try := 0; try < docSearchRepairTries; try++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			rep, ran, err := RunDocSearchRepair(ctx)
			if err == nil {
				if ran {
					helpers.MessageLogs.InfoLog.Printf("Docs search repair: %s", rep)
				}
				return
			}
			helpers.MessageLogs.ErrorLog.Printf("Docs search repair failed (will retry): %v (so far: %s)", err, rep)
			if wait < 30*time.Minute {
				wait *= 2
			}
		}
	})
}

// RunDocSearchRepair rewrites every doc's search entries, unless that has
// been done, and then records that it has, with what it did. ran is whether
// it ran this time.
func RunDocSearchRepair(ctx context.Context) (rep DocSearchRepair, ran bool, err error) {
	if done, derr := docSearchRepairDone(); derr != nil || done {
		return rep, false, derr
	}
	rep, err = RepairDocSearch(ctx)
	if err != nil {
		return rep, true, err
	}
	return rep, true, configModel.UpsertConfig(docSearchRepairKey, "done: "+rep.String())
}

// RepairDocSearch rewrites every doc's search entries from the docs, a page at
// a time: each doc's own entry whole, and its comments' and files' entries'
// fields for it. A doc too large to read is skipped and counted. Idempotent.
func RepairDocSearch(ctx context.Context) (rep DocSearchRepair, err error) {
	for offset := 0; ; offset += docSearchRepairPage {
		end, err := repairDocSearchPage(ctx, &rep, offset, docSearchRepairPage)
		if err != nil || end {
			return rep, err
		}
	}
}

// repairDocSearchPage rewrites the entries of the n docs from offset, read as
// one page, and says whether the docs ran out there. The graph's client takes
// at most 64 MB an answer, and images pasted into a doc are in its body, so a
// page can be too large to send (ResourceExhausted): that failed the whole
// repair, which started over at every boot and never got past the page. Such
// a page is read again in pages a tenth its size, down to single docs, and a
// doc too large to read alone is skipped: counted, and named in the log. Any
// other error ends the repair, to be tried again later.
func repairDocSearchPage(ctx context.Context, rep *DocSearchRepair, offset, n int) (end bool, err error) {
	docs, err := docSearchEntryPage(ctx, offset, n)
	if err == nil {
		if err := writeDocSearchRepairPage(ctx, rep, docs); err != nil {
			return false, err
		}
		return len(docs) < n, nil
	}
	if status.Code(err) != codes.ResourceExhausted {
		return false, err
	}
	if n <= 1 {
		// Named by its uuid alone, a small read. A doc is skipped only when
		// there is one: with none at the offset the docs ran out, so a
		// ResourceExhausted that is not about size cannot skip on for ever.
		ids, uerr := docUUIDPage(ctx, offset, 1)
		if uerr != nil {
			return false, uerr
		}
		if len(ids) == 0 {
			return true, nil
		}
		rep.Skipped++
		helpers.LogErrorWithContext(ctx, "Docs search repair: skipped doc %s, too large to read (%v); its search entries are as they were", ids[0], err)
		return false, nil
	}
	step := max(n/10, 1)
	for from := offset; from < offset+n; from += step {
		end, err := repairDocSearchPage(ctx, rep, from, min(step, offset+n-from))
		if err != nil || end {
			return end, err
		}
	}
	return false, nil
}

// writeDocSearchRepairPage writes a page of docs' own entries whole, and their
// comments' and files' entries' fields for them.
func writeDocSearchRepairPage(ctx context.Context, rep *DocSearchRepair, docs []*dgraphStruct.DgraphDoc) error {
	entries := make([]*openSearchStruct.OpenSearchDocEntry, 0, len(docs))
	children := make(map[string]openSearchGlobalSearchModels.DocChildFields, len(docs))
	for _, d := range docs {
		if d == nil || strings.TrimSpace(d.Uuid) == "" {
			continue
		}
		e := domain.DocSearchEntryOf(d)
		entries = append(entries, e)
		if e.DocDeletedAt != nil {
			rep.Deleted++
		} else {
			rep.Live++
		}
		children[d.Uuid] = openSearchGlobalSearchModels.DocChildFields{
			Title: e.DocTitle, Private: e.DocPrivate, Reading: e.DocReadingUsers, Editing: e.DocEditingUsers,
			Commenting: e.DocCommentingUsers, CreatedBy: e.DocCreatedByUserUuid, Live: e.DocDeletedAt == nil,
		}
	}
	written, err := bulkIndexDocEntries(ctx, entries)
	rep.Docs += written
	if err != nil {
		return err
	}
	changed, revived, err := repairDocChildren(ctx, children)
	rep.Children += changed
	rep.Revived += revived
	return err
}

func docSearchRepairDone() (bool, error) {
	cfg, err := configModel.GetConfigByKey(docSearchRepairKey)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cfg != nil && strings.HasPrefix(cfg.Value, "done"), nil
}
