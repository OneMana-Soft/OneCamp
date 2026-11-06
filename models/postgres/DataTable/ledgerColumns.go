package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// A column's change in the row ledger (V-455): a column made, changed,
// deleted or brought back is a change set like a row's, so an undo pressed
// after it takes it back, never the change before it. Its Change is
// {Row: the field's id, Field: FieldColumn}, with the column's state before
// and after (ColumnState; nil before for one made). data_table_changes.row_id
// holds the field's id there: no row has it, so no row's history shows it.

// ColumnState is a column as a change keeps it: what an undo puts back.
type ColumnState struct {
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Config      json.RawMessage `json:"config"`
	Position    float64         `json:"position"`
	Description *string         `json:"description,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Default     json.RawMessage `json:"default_value,omitempty"`
	// Deleted is a column in the table's trash.
	Deleted bool `json:"deleted,omitempty"`
}

// StateOf is f's state as a change keeps it; nil for none.
func StateOf(f *Field) *ColumnState {
	if f == nil {
		return nil
	}
	cfg := strings.TrimSpace(f.Config)
	if cfg == "" {
		cfg = "{}"
	}
	s := &ColumnState{Name: f.Name, Type: f.Type, Config: json.RawMessage(cfg), Position: f.Position,
		Description: f.Description, Required: f.Required, Deleted: f.DeletedAt != nil}
	if len(f.Default) > 0 && string(f.Default) != "null" {
		s.Default = f.Default
	}
	return s
}

// ColumnOf is a column's state as a change stored it; nil for none (a
// column's Before when it was made). Pure.
func ColumnOf(raw json.RawMessage) *ColumnState {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s ColumnState
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return &s
}

// Same is whether two column states are the same (nil is no column). Pure.
func (s *ColumnState) Same(o *ColumnState) bool {
	if s == nil || o == nil {
		return s == nil && o == nil
	}
	return s.Name == o.Name && s.Type == o.Type && sameJSON(s.Config, o.Config) && s.Position == o.Position &&
		sameText(s.Description, o.Description) && s.Required == o.Required && sameJSON(s.Default, o.Default) &&
		s.Deleted == o.Deleted
}

func sameText(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Columns is a column's change's states before and after (ColumnOf); nil
// and nil for any other change. Pure.
func (c Change) Columns() (was, now *ColumnState) {
	if c.Field != FieldColumn {
		return nil, nil
	}
	return ColumnOf(c.Before), ColumnOf(c.After)
}

// ColumnChange is the change of a column from was to now (either nil for
// none), and whether there is one. Pure.
func ColumnChange(id uuid.UUID, was, now *ColumnState) (Change, bool) {
	if was.Same(now) {
		return Change{}, false
	}
	c := Change{Row: id, Field: FieldColumn}
	if was != nil {
		c.Before = asJSON(was)
	}
	if now != nil {
		c.After = asJSON(now)
	}
	return c, true
}

// columnsTx is table's fields ids as tx finds them, trashed ones too,
// locked for the rest of tx; a field there isn't is left out.
func columnsTx(ctx context.Context, tx *sql.Tx, table uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]*Field, error) {
	out := map[uuid.UUID]*Field{}
	if len(ids) == 0 {
		return out, nil
	}
	fs, err := pgexec.Query(pgexec.WithTx(ctx, tx), `SELECT `+fieldColumns+` FROM data_table_fields
		WHERE table_id = $1 AND id = ANY($2) ORDER BY id FOR UPDATE`, func(r pgexec.Row) (*Field, error) { return scanField(r) }, table, ids)
	for _, f := range fs {
		out[f.Id] = f
	}
	return out, err
}

// WriteColumns runs write, a change to the columns ids of cs's table, and
// stores what it changed of each as cs's changes (FieldColumn), with it, in
// one transaction: the one ctx carries (an API item's, pgexec.InTxFor) or
// one of its own. write's ctx carries that transaction, so the field writes
// it makes (CreateField, DeleteField, CreateFieldsWithIDs, DeleteFieldPair,
// RestoreFields...) join it. The columns are read, locked, before and after
// write; a write that changed none of them stores nothing. committing runs
// in the transaction once cs is stored (the ledger's observers).
func WriteColumns(ctx context.Context, ids []uuid.UUID, cs *ChangeSet, committing Committing, write func(ctx context.Context) error) error {
	return joinTx(ctx, postgresInit.DBConn.DBTimeout, func(ctx context.Context, tx *sql.Tx) error {
		ctx = pgexec.WithTx(ctx, tx)
		was, err := columnsTx(ctx, tx, cs.TableID, ids)
		if err != nil {
			return err
		}
		if err := write(ctx); err != nil {
			return err
		}
		now, err := columnsTx(ctx, tx, cs.TableID, ids)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if c, ok := ColumnChange(id, StateOf(was[id]), StateOf(now[id])); ok {
				cs.Changes = append(cs.Changes, c)
			}
		}
		if !cs.Stored() {
			return nil
		}
		if err := recordTx(ctx, tx, cs); err != nil {
			return err
		}
		if committing != nil {
			return committing(ctx, tx, cs)
		}
		return nil
	})
}

// reverseColumns puts columns back as changes found them (FieldColumn
// changes), newest first, each only while the column is as the change left
// it: one made goes to the trash, one deleted comes back with its cells and
// links (which never left), and one changed takes its name, type, config,
// place, description, requiredness and default back. A column gone for good
// (the trash purged) or changed since is left. The table's primary field,
// which names its rows, never goes to the trash this way. Each reversal is
// cs's change. It says how many it reversed.
func reverseColumns(ctx context.Context, tx *sql.Tx, table, by uuid.UUID, changes []Change, cs *ChangeSet) (int, error) {
	n := 0
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		found, err := columnsTx(ctx, tx, table, []uuid.UUID{c.Row})
		if err != nil {
			return n, err
		}
		cur := found[c.Row]
		if cur == nil || !StateOf(cur).Same(ColumnOf(c.After)) {
			continue
		}
		back := ColumnOf(c.Before)
		switch {
		case back == nil || back.Deleted:
			if cur.DeletedAt == nil {
				if cur.Primary {
					continue
				}
				if err := deleteField(ctx, tx, table, cur.Id, by); err != nil {
					return n, err
				}
			}
		default:
			if cur.DeletedAt != nil {
				if err := restoreField(ctx, tx, cur); err != nil {
					return n, err
				}
			}
			live := *cur
			live.DeletedAt = nil
			f := live
			f.Name, f.Type, f.Config, f.Position = back.Name, back.Type, string(back.Config), back.Position
			f.Description, f.Required, f.Default = back.Description, back.Required, back.Default
			if !StateOf(&f).Same(StateOf(&live)) {
				if err := updateField(ctx, tx, &f); err != nil {
					return n, err
				}
			}
		}
		after, err := columnsTx(ctx, tx, table, []uuid.UUID{c.Row})
		if err != nil {
			return n, err
		}
		if r, ok := ColumnChange(c.Row, StateOf(cur), StateOf(after[c.Row])); ok {
			cs.Changes = append(cs.Changes, r)
			n++
		}
	}
	return n, nil
}
