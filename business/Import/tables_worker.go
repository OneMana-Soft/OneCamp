package business

// The tables stage (CapTables): a provider's bases of tables become OneCamp
// tables, a base at a time, through the one importer of every source's
// tables (dataTableBusiness.ImportBaseAs): their fields of OneCamp's kinds,
// the links between them made once from both sides, rollups and formulas,
// every row through the Row Ledger, each row's links, and views. A base's
// tables arrive private to the admin importing, as a tree of pages does;
// they share them from the table's own menu.
//
// A base is one chunk, and is tried again whole: the importer takes away
// what it made when it stops part way, and a base's tables are recorded in
// the id map (EntityTable, by base and table) once it is made, so a base
// whose tables are there is done. What the importer couldn't bring (a cell
// no kind takes, a formula it can't read) is logged against the job as a
// warning, by table, field and row: a base's notes together (chunkNotes).

import (
	"context"
	"fmt"
	"sync/atomic"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	dataTableModels "github.com/akashc777/OneCamp/models/postgres/DataTable"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// tableWorkers is how many bases are read at once: a provider's rate limit
// is per base, so two bases go side by side.
var tableWorkers = envIntDefault("IMPORT_TABLE_WORKERS", 2)

// importBase is the importer, a seam for tests.
var importBase = dataTableBusiness.ImportBaseAs

// importActor is the person an import is for, as its tables' actor, their
// admin flag with them (dataTableBusiness.ImportActor). A seam for tests.
var importActor = dataTableBusiness.ImportActor

// baseTableKey is a table's key in the id map: unique across the bases of
// one import.
func baseTableKey(base, table string) string { return base + "/" + table }

func processTableChunk(ctx context.Context, prov importProvider.Provider, job *importModels.Job,
	opts importProvider.JobOptions, chunk *importModels.Chunk, importingUser *userModels.UserInfo) error {

	src, ok := prov.(importProvider.TableSource)
	if !ok {
		return fmt.Errorf("%s brings no tables", prov.Name())
	}
	if chunk.ParentSourceId == nil || *chunk.ParentSourceId == "" {
		return fmt.Errorf("table chunk missing its base")
	}
	if job.TriggeredBy == nil {
		return fmt.Errorf("table chunk has no one importing")
	}
	base := *chunk.ParentSourceId
	if made, err := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityTable, base); err != nil {
		return err
	} else if made != uuid.Nil {
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}

	var read atomic.Int64
	stop := keepClaimed(ctx, chunk.Id, func() int { return int(read.Load()) })
	defer stop()
	b, err := src.BaseOf(ctx, job, opts, base, func(n int) { read.Store(int64(n)) })
	if err == nil {
		// What the base can't bring goes to the report together: as the base
		// is noted (recordBase), or when the importer stops part way, before
		// a rate limit's wait (and on a panic, as the chunk ends).
		notes := newChunkNotes(job.Id, &chunk.Id)
		defer notes.flush(ctx)
		b.Private = true
		b.Files = keepTableFile(ctx, prov, job, opts, notes)
		unmatched := matchPeople(ctx, &b)
		var made *dataTableBusiness.ImportedBase
		var actor dataTableBusiness.Actor
		if actor, err = importActor(ctx, *job.TriggeredBy); err == nil {
			made, err = importBase(ctx, actor, b)
		}
		if n := unmatched(); n > 0 {
			notes.add(importModels.EntityTable, base, importModels.SeverityWarning, "PEOPLE_NOT_MEMBERS",
				fmt.Sprintf("%d people named in the base's cells aren't members of this workspace by their email, so those cells leave them out; invite them, then set the cells.", n))
		}
		if err == nil {
			return recordBase(ctx, job, chunk, base, b, made, notes)
		}
		notes.flush(ctx)
	}
	if d, limited := importProvider.IsRateLimited(err); limited {
		waitOutRateLimit(ctx, job, err, d)
		_ = importModels.ResetChunkForRetry(ctx, chunk.Id, "provider rate-limited")
		return errReaperOnly
	}
	_ = importingUser
	return err
}

// keepTableFile keeps a files cell's file as a file of its table (storeFile,
// as the attachments stage keeps a task's): fetched through the provider,
// scanned, at most the import's cap. One not kept is said in the chunk's
// notes and left out of its cell; a rate limit waits it out and tries once
// more.
func keepTableFile(ctx context.Context, prov importProvider.Provider, job *importModels.Job, opts importProvider.JobOptions, notes *chunkNotes) func(uuid.UUID, dataTableBusiness.ImportFile) (fieldTypes.FileRef, bool) {
	maxBytes := defaultMaxAttachmentBytes
	if v, _ := opts["max_file_bytes"].(float64); v > 0 {
		maxBytes = int64(v)
	}
	return func(table uuid.UUID, f dataTableBusiness.ImportFile) (fieldTypes.FileRef, bool) {
		att, ok := f.From.(importProvider.SourceAttachment)
		if !ok {
			return fieldTypes.FileRef{}, false
		}
		if att.Size > maxBytes {
			notes.add(importModels.EntityFile, att.SourceID, importModels.SeverityWarning, "FILE_TOO_LARGE",
				fmt.Sprintf("%s is larger than the import's cap, so it isn't brought.", f.Name))
			return fieldTypes.FileRef{}, false
		}
		var stored storedFile
		var err error
		for try := 0; try < 2; try++ {
			stored, err = storeFile(ctx, prov, job, opts, att, postgresStruct.ATTACHMENT_SRC_TABLE, table.String(), *job.TriggeredBy, maxBytes)
			d, limited := importProvider.IsRateLimited(err)
			if !limited {
				break
			}
			waitOutRateLimit(ctx, job, err, d)
		}
		if err != nil || stored.Outcome != "" {
			why := stored.Outcome
			if err != nil {
				why = err.Error()
			}
			notes.add(importModels.EntityFile, att.SourceID, importModels.SeverityWarning, "FILE_NOT_KEPT",
				fmt.Sprintf("%s couldn't be brought (%s).", f.Name, why))
			return fieldTypes.FileRef{}, false
		}
		return fieldTypes.FileRef{ID: stored.ID.String(), Name: f.Name, Size: stored.Size, Mime: stored.ContentType}, true
	}
}

// memberByEmail is a member's id by their email (uuid.Nil for none), a seam
// for tests.
var memberByEmail = userDomain.GetUserIDByEmail

// matchPeople has every people cell of base name members by their ids: a
// source names people by email (importProvider.PersonRef), and one who isn't
// a member is left out. It returns how many distinct people weren't, once
// the rows have been read.
func matchPeople(ctx context.Context, b *dataTableBusiness.ImportBase) func() int {
	known := map[string]uuid.UUID{}
	missing := map[string]bool{}
	match := func(v any) any {
		list, ok := v.([]any)
		if !ok {
			return v
		}
		var out []any
		for _, x := range list {
			p, ok := x.(importProvider.PersonRef)
			if !ok {
				out = append(out, x)
				continue
			}
			id, seen := known[p.Email]
			if !seen {
				id, _ = memberByEmail(ctx, p.Email)
				known[p.Email] = id
			}
			if id == uuid.Nil {
				missing[p.Email] = true
				continue
			}
			out = append(out, id.String())
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	for i := range b.Tables {
		t := &b.Tables[i]
		people := map[string]bool{}
		for _, f := range t.Fields {
			if f.Type == dataTableModels.FieldPeople {
				people[f.Key] = true
			}
		}
		if len(people) == 0 || t.Rows == nil {
			continue
		}
		rows := t.Rows
		t.Rows = func(page func([]dataTableBusiness.ImportRow) error) error {
			return rows(func(rs []dataTableBusiness.ImportRow) error {
				for _, r := range rs {
					for k := range people {
						if v, ok := r.Cells[k]; ok {
							if m := match(v); m == nil {
								delete(r.Cells, k)
							} else {
								r.Cells[k] = m
							}
						}
					}
				}
				return page(rs)
			})
		}
	}
	return func() int { return len(missing) }
}

// recordBase notes a made base's tables in the id map, so a roll back takes
// them away and a retry finds them; the base itself is noted by its first
// table, last, so a base half noted is tried again. What the base couldn't
// bring goes to the report with the chunk's other notes, together, before
// the base is noted done.
func recordBase(ctx context.Context, job *importModels.Job, chunk *importModels.Chunk, base string, b dataTableBusiness.ImportBase, made *dataTableBusiness.ImportedBase, notes *chunkNotes) error {
	var first uuid.UUID
	for _, t := range b.Tables {
		id, ok := made.Tables[t.Key]
		if !ok {
			continue
		}
		if first == uuid.Nil {
			first = id
		}
		if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityTable, baseTableKey(base, t.Key), id, strPtr(base), nil, true); err != nil {
			notes.add(importModels.EntityTable, t.Key, importModels.SeverityError,
				"TABLE_NOT_RECORDED", fmt.Sprintf("The table %q came across, but the import couldn't note it, so a roll back leaves it in place.", t.Name))
		}
	}
	for _, n := range made.Notes {
		what := n.Table
		if n.Row != "" {
			what += "/" + n.Row
		} else if n.Field != "" {
			what += "/" + n.Field
		}
		notes.add(importModels.EntityTable, what, importModels.SeverityWarning, n.Code, n.Msg)
	}
	notes.flush(ctx)
	if first != uuid.Nil {
		if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityTable, base, first, nil,
			mustMarshal(map[string]any{"tables": len(made.Tables), "rows": made.RowsMade}), false); err != nil {
			return err
		}
	}
	return importModels.FinishChunk(ctx, chunk.Id, made.RowsMade, nil)
}
