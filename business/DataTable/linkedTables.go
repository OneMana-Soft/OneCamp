package business

import (
	"context"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// linkedTables is the other tables a save of fields checks against: what a
// relation links to, and what a formula or a rollup reads there (a rollup's
// chain, a level at a time). Each is read once, however many of a batch's
// items ask: a batch reads the tables its items can name at once, by ids
// (preload), and any other the first time a check asks for it (tableAt).
type linkedTables struct {
	opened map[uuid.UUID]*linkedTable
	// fetch reads one table and its fields; swapped by tests.
	fetch func(ctx context.Context, id uuid.UUID) *linkedTable
}

// linkedTable is a table as a save's checks read it: none when it isn't
// there (or is deleted), its live fields, or why they couldn't be read.
type linkedTable struct {
	table  *model.DataTable
	fields []*model.Field
	err    error
}

func newLinked() *linkedTables {
	return &linkedTables{opened: map[uuid.UUID]*linkedTable{}, fetch: fetchLinked}
}

// fetchLinked reads table id and its fields, as a save's checks read one.
func fetchLinked(ctx context.Context, id uuid.UUID) *linkedTable {
	t, err := model.GetTableByID(ctx, id)
	if err != nil || t == nil {
		return &linkedTable{}
	}
	fields, err := model.ListFields(ctx, id)
	return &linkedTable{table: t, fields: fields, err: err}
}

// tableAt is table id as the checks read it, read the first time it's asked
// for.
func (l *linkedTables) tableAt(ctx context.Context, id uuid.UUID) *linkedTable {
	if o, ok := l.opened[id]; ok {
		return o
	}
	o := l.fetch(ctx, id)
	l.opened[id] = o
	return o
}

// fieldsFor is linkedFieldsFor over l: a table's live fields as actor may
// read them, none for one they can't open.
func (l *linkedTables) fieldsFor(ctx context.Context, actor Actor) func(uuid.UUID) []*model.Field {
	return func(id uuid.UUID) []*model.Field {
		o := l.tableAt(ctx, id)
		if o.err != nil || !opens(ctx, o.table, actor) {
			return nil
		}
		return liveOnly(o.fields)
	}
}

// preload reads the tables among ids that l hasn't, with their fields, at
// once: two reads however many there are.
func (l *linkedTables) preload(ctx context.Context, ids []uuid.UUID) error {
	var want []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if _, ok := l.opened[id]; !ok && !seen[id] {
			seen[id] = true
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return nil
	}
	tables, err := model.TablesByIDs(ctx, want)
	if err != nil {
		return storeFailed("failed to load table")
	}
	fields, err := model.FieldsOfTables(ctx, want)
	if err != nil {
		return storeFailed("failed to load fields")
	}
	for _, id := range want {
		l.opened[id] = &linkedTable{}
	}
	for _, t := range tables {
		if t.DeletedAt == nil {
			l.opened[t.Id] = &linkedTable{table: t, fields: fields[t.Id]}
		}
	}
	return nil
}
