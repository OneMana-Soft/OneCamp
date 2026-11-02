package business

// A step over a view's rows (G2, gsheets-grid-01 items 3 and 5; POST
// /tables/{id}/rows/transform): find, replace, trim spaces, remove duplicate
// rows, split a text column. The walk is the export's (viewRows): the view's
// rows in its order, each as the reader may see it. A step runs in two
// phases. The first walks the view and keeps only what the writes need (the
// rows the step changes; a dedupe's duplicates), and refuses a view past
// maxTransformRows before anything is written. The second writes in chunks
// of maxBatch rows through Batch, so every cell meets a paste's checks: each
// chunk's rows are read again, the step is worked out on them as they are
// now, and each is written with the version read. Every chunk is a change
// set, and one operation joins them, so one Undo takes the whole step back.
//
// Who may: a find and every dry run, whoever may open the table, over its
// rows and cells as they read them; the writes, whoever may write a paste
// (Batch, as the actor). A table the reader can't open, and one that isn't
// there, are not found, alike.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

const (
	// maxTransformRows is the most rows a step walks: a view of more is
	// refused before anything is written.
	maxTransformRows = 100_000
	// maxMatches is the most matches an answer lists, maxSkipped the most
	// refused cells.
	maxMatches = 1000
	maxSkipped = 100
	// maxSplitInto is the most fields a split writes, maxSeparator its
	// longest separator in bytes.
	maxSplitInto = 10
	maxSeparator = 100
)

var (
	errTransformStep   = errors.New("a step is find, replace, trim, dedupe or split")
	errTransformFields = errors.New("name the fields: each a field of the table, once")
	errTransformView   = errors.New("name a view's rows by its rules or by their ids, not both")
	errTransformRows   = fmt.Errorf("a step works through at most %d rows: filter the view to fewer", maxTransformRows)
)

// stepKinds is each writing step's change kind, as its history says it.
var stepKinds = map[string]rowLedger.ChangeKind{
	StepReplace: rowLedger.ChangeReplace,
	StepTrim:    rowLedger.ChangeTrim,
	StepDedupe:  rowLedger.ChangeDedupe,
	StepSplit:   rowLedger.ChangeSplit,
}

// TransformInput is a step over a view's rows: the rows as an export names
// them (Spec, or Rows when only the browser knows the view's rules; neither
// for every row in the table's order), the fields it searches, changes or
// compares, and the step's own words: what a find or a replace looks for and
// how, a replace's text, a split's separator and the fields its parts go to
// (Into, 1 to 10, the first part to the first). DryRun counts what the step
// would do and writes nothing. From is the place in the view where a find's
// list of matches starts, for a find bar stepping past the first page.
type TransformInput struct {
	Step      string      `json:"step"`
	Spec      *query.Spec `json:"spec,omitempty"`
	Rows      []uuid.UUID `json:"rows,omitempty"`
	Fields    []uuid.UUID `json:"fields"`
	Find      string      `json:"find,omitempty"`
	Replace   string      `json:"replace,omitempty"`
	MatchCase bool        `json:"match_case,omitempty"`
	WholeCell bool        `json:"whole_cell,omitempty"`
	Regex     bool        `json:"regex,omitempty"`
	Separator string      `json:"separator,omitempty"`
	Into      []uuid.UUID `json:"into,omitempty"`
	DryRun    bool        `json:"dry_run,omitempty"`
	From      int         `json:"from,omitempty"`
}

// TransformMatch is a cell a step found, or would change: its row, its field
// (none for a duplicate row), and the row's place in the view, from 0.
type TransformMatch struct {
	Row   uuid.UUID `json:"row"`
	Field string    `json:"field,omitempty"`
	At    int       `json:"at"`
}

// TransformResult is what a step found or did. Walked is the rows of the
// view; Rows and Cells, those a find found, or a write changed (would
// change, in a dry run); Deleted, a dedupe's rows deleted (Rows, its
// duplicates). Matches lists the first maxMatches from the input's From, in
// view order, for a find and a dry run (Capped: there were more). A write
// answers its change sets and the operation joining them (the one Undo), the
// cells the server refused (Skipped, the first maxSkipped; SkippedCount, all)
// and the rows written by someone else while it ran, left as they are
// (KeptChanged). A split's dry run says how many filled cells it would write
// over (Overwrites) and the most parts a cell splits into (MaxParts).
type TransformResult struct {
	Walked       int              `json:"walked"`
	Rows         int              `json:"rows"`
	Cells        int              `json:"cells"`
	Deleted      int              `json:"deleted"`
	Matches      []TransformMatch `json:"matches"`
	Capped       bool             `json:"capped"`
	Operation    *uuid.UUID       `json:"operation,omitempty"`
	ChangeSets   []uuid.UUID      `json:"change_sets"`
	Skipped      []BatchError     `json:"skipped"`
	SkippedCount int              `json:"skipped_count"`
	KeptChanged  int              `json:"kept_changed"`
	Overwrites   int              `json:"overwrites,omitempty"`
	MaxParts     int              `json:"max_parts,omitempty"`
}

// newTransformResult is an answer with nothing in it yet, its lists empty
// rather than none.
func newTransformResult() *TransformResult {
	return &TransformResult{Matches: []TransformMatch{}, ChangeSets: []uuid.UUID{}, Skipped: []BatchError{}}
}

// Seams for tests: where a step's second phase reads a chunk's rows again,
// and where it writes them.
var (
	transformRead  = rowsNow
	transformBatch = Batch
)

// Transform runs in's step over table's view for actor (see the file's
// comment for who may).
func Transform(ctx context.Context, table uuid.UUID, in TransformInput, actor Actor) (*TransformResult, error) {
	t, err := loadViewable(ctx, table, actor)
	if err != nil {
		return nil, unseen(err)
	}
	all, err := model.ListFields(ctx, t.Id)
	if err != nil {
		return nil, errRowsUnavailable
	}
	tr, err := prepareTransform(all, in)
	if err != nil {
		return nil, err
	}
	switch {
	case in.Spec != nil && in.Rows != nil:
		return nil, errTransformView
	case len(in.Rows) > maxTransformRows:
		return nil, errTransformRows
	}
	w, err := tr.walk(ctx, viewRows(t.Id, actor, in.Spec, in.Rows), max(0, in.From))
	if err != nil {
		// The walk's reads check again: access lost since is as above.
		return nil, unseen(err)
	}
	if tr.step == StepFind || in.DryRun {
		return w.res, nil
	}
	return w.write(ctx, t.Id, actor, tr)
}

// textKind is whether f holds a person's own text, which a replace, a trim
// and a split change: never a computed field, a list or a number.
func textKind(f *model.Field) bool {
	switch f.Type {
	case model.FieldText, model.FieldLongText, model.FieldURL, model.FieldEmail, model.FieldPhone:
		return true
	}
	return false
}

// prepareTransform is in made ready to run over all's fields, or why it
// can't run, in words.
func prepareTransform(all []*model.Field, in TransformInput) (*transform, error) {
	if _, writes := stepKinds[in.Step]; !writes && in.Step != StepFind {
		return nil, errTransformStep
	}
	fields, err := fieldsNamed(all, in.Fields)
	if err != nil {
		return nil, errTransformFields
	}
	t := &transform{step: in.Step, fields: fields, with: in.Replace}
	switch in.Step {
	case StepFind, StepReplace:
		if t.find, err = compileFind(in.Find, in.MatchCase, in.WholeCell, in.Regex); err != nil {
			return nil, err
		}
	case StepSplit:
		if len(fields) != 1 {
			return nil, errors.New("split one text field at a time")
		}
		if in.Separator == "" || len(in.Separator) > maxSeparator {
			return nil, fmt.Errorf("say what the text splits at, in at most %d characters", maxSeparator)
		}
		t.sep = in.Separator
		if len(in.Into) > 0 {
			if t.into, err = fieldsNamed(all, in.Into); err != nil {
				return nil, errors.New("name the fields the parts go to: each a field of the table, once")
			}
		}
		switch {
		case len(t.into) > maxSplitInto:
			return nil, fmt.Errorf("a split writes at most %d fields", maxSplitInto)
		case len(t.into) == 0 && !in.DryRun:
			return nil, errors.New("name the fields the parts go to")
		case slices.ContainsFunc(t.into, func(f *model.Field) bool { return f.Id == fields[0].Id }):
			return nil, fmt.Errorf("%q is the field being split: put its parts in other fields", fields[0].Name)
		}
	}
	if in.Step == StepReplace || in.Step == StepTrim || in.Step == StepSplit {
		for _, f := range append(slices.Clone(fields), t.into...) {
			if !textKind(f) {
				return nil, fmt.Errorf("%q isn't text: a %s works on text, long text, URL, email and phone fields", f.Name, in.Step)
			}
		}
	}
	return t, nil
}

// written is the fields a step writes: a split's targets, else its fields.
func (t *transform) written() []*model.Field {
	if t.step == StepSplit {
		return t.into
	}
	return t.fields
}

// walked is a step's first phase: its answer so far, the rows it changes in
// view order, and a dedupe's duplicates, each with the row it repeats.
type walked struct {
	res    *TransformResult
	change []uuid.UUID
	dups   []duplicate
}

// duplicate is a row a dedupe deletes, and the first row it repeats.
type duplicate struct{ row, of uuid.UUID }

// walk reads the view a window at a time and works the step out on each row
// as read: what a find finds, what a write would change, a dedupe's
// duplicates (a row equal to an earlier one on every compared field). Places
// count from 0 along the view, and matches are listed from place from, the
// first maxMatches of them. Past maxTransformRows rows it stops, refused.
func (t *transform) walk(ctx context.Context, next rowWindows, from int) (*walked, error) {
	w := &walked{res: newTransformResult()}
	res := w.res
	list := func(row uuid.UUID, field string, at int) {
		switch {
		case at < from:
		case len(res.Matches) < maxMatches:
			res.Matches = append(res.Matches, TransformMatch{Row: row, Field: field, At: at})
		default:
			res.Capped = true
		}
	}
	first := map[[32]byte]uuid.UUID{}
	for more := true; more; {
		rows, m, err := next(ctx)
		if err != nil {
			return nil, err
		}
		more = m
		for _, r := range rows {
			at := res.Walked
			if res.Walked++; res.Walked > maxTransformRows {
				return nil, errTransformRows
			}
			values := parseRowValues(r.Values)
			switch t.step {
			case StepDedupe:
				k := dedupeKey(values, t.fields)
				if of, seen := first[k]; seen {
					w.dups = append(w.dups, duplicate{row: r.Id, of: of})
					res.Rows++
					list(r.Id, "", at)
				} else {
					first[k] = r.Id
				}
			case StepFind:
				_, found := t.cells(values)
				if len(found) > 0 {
					res.Rows++
					res.Cells += len(found)
				}
				for _, f := range found {
					list(r.Id, f.Id.String(), at)
				}
			default:
				if t.step == StepSplit {
					s, _ := values[t.fields[0].Id.String()].(string)
					res.MaxParts = max(res.MaxParts, countParts(s, t.sep))
				}
				patch, _ := t.cells(values)
				if len(patch) == 0 {
					continue
				}
				w.change = append(w.change, r.Id)
				res.Rows++
				res.Cells += len(patch)
				for _, f := range t.written() {
					if _, changes := patch[f.Id.String()]; changes {
						list(r.Id, f.Id.String(), at)
						if t.step == StepSplit && !emptyCell(values[f.Id.String()]) {
							res.Overwrites++
						}
					}
				}
			}
		}
	}
	return w, nil
}

// write is the step's second phase, in chunks of maxBatch rows, each a
// change set of one operation. Its answer counts what was written. A chunk
// that fails after others were written stops the step, saying so: Undo
// takes back what it wrote.
func (w *walked) write(ctx context.Context, table uuid.UUID, actor Actor, t *transform) (*TransformResult, error) {
	res := w.res
	res.Matches, res.Capped, res.Rows, res.Cells = []TransformMatch{}, false, 0, 0
	sw := &stepWrite{table: table, actor: actor, t: t, op: uuid.New(), res: res}
	todo := len(w.change) + len(w.dups)
	for start := 0; start < todo; start += maxBatch {
		end := min(start+maxBatch, todo)
		var err error
		if t.step == StepDedupe {
			err = sw.deletes(ctx, w.dups[start:end])
		} else {
			err = sw.patches(ctx, w.change[start:end])
		}
		if err != nil && len(res.ChangeSets) > 0 {
			return nil, fmt.Errorf("this stopped part way, after changing %d rows (Undo takes them back): %v", res.Rows+res.Deleted, err)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(res.ChangeSets) > 0 {
		res.Operation = &sw.op
	}
	return res, nil
}

// stepWrite is a step's second phase: the table, its writer, the step, the
// operation joining its change sets, and the answer it counts into.
type stepWrite struct {
	table uuid.UUID
	actor Actor
	t     *transform
	op    uuid.UUID
	res   *TransformResult
}

// kind is the step's change kind, as its history says it.
func (w *stepWrite) kind() string { return string(stepKinds[w.t.step]) }

// worked is the step worked out on ids' rows as they are now: a patch for
// each row it changes, naming the version read. A row deleted since is left
// out.
func (w *stepWrite) worked(ctx context.Context, ids []uuid.UUID) ([]PatchInput, error) {
	rows, err := transformRead(ctx, w.table, w.actor, ids)
	if err != nil {
		return nil, err
	}
	var out []PatchInput
	for _, r := range rows {
		if patch, _ := w.t.cells(parseRowValues(r.Values)); len(patch) > 0 {
			out = append(out, PatchInput{Row: r.Id, Values: patch, Version: r.Version})
		}
	}
	return out, nil
}

// patches writes one chunk of a replace, a trim or a split: its rows read
// again, the step worked out on each as it is now, and each written with the
// version read. A row written again between that read and the write refuses
// the batch (RowConflict): the chunk is read and worked out once more, and a
// row that refuses it again is left as it is (KeptChanged). Rows whose cells
// the batch refuses (BatchErrors) are left as they are, and the rest written
// (Skipped).
func (w *stepWrite) patches(ctx context.Context, ids []uuid.UUID) error {
	patch, err := w.worked(ctx, ids)
	if err != nil {
		return err
	}
	reread := false
	for len(patch) > 0 {
		out, err := transformBatch(ctx, w.table, BatchInput{Kind: w.kind(), Patch: patch, Operation: w.op}, w.actor)
		if err == nil {
			w.res.ChangeSets = append(w.res.ChangeSets, out.ChangeSet)
			w.res.Rows += len(patch)
			for _, p := range patch {
				w.res.Cells += len(p.Values)
			}
			return nil
		}
		var conflict *RowConflict
		var refused BatchErrors
		n := len(patch)
		switch {
		case errors.As(err, &conflict) && !reread:
			reread = true
			if patch, err = w.worked(ctx, ids); err != nil {
				return err
			}
			continue
		case errors.As(err, &conflict) && conflict.Row != nil:
			patch = slices.DeleteFunc(patch, func(p PatchInput) bool { return p.Row == conflict.Row.Id })
			w.res.KeptChanged += n - len(patch)
		case errors.As(err, &refused):
			gone := w.skip(refused)
			patch = slices.DeleteFunc(patch, func(p PatchInput) bool { return gone[p.Row] })
		}
		if len(patch) == n {
			return err // nothing of it to leave out: refused as a whole
		}
	}
	return nil
}

// deletes deletes one chunk of a dedupe's duplicates: each row, and the row
// it repeats, read again, and deleted only while both are there and still
// equal on the compared fields; else it's left (KeptChanged). A row deleted
// since is left out, and so is one the batch refuses (Skipped).
func (w *stepWrite) deletes(ctx context.Context, dups []duplicate) error {
	ids := make([]uuid.UUID, 0, 2*len(dups))
	for _, d := range dups {
		ids = append(ids, d.row, d.of)
	}
	rows, err := transformRead(ctx, w.table, w.actor, ids)
	if err != nil {
		return err
	}
	keys := make(map[uuid.UUID][32]byte, len(rows))
	for _, r := range rows {
		keys[r.Id] = dedupeKey(parseRowValues(r.Values), w.t.fields)
	}
	var del []uuid.UUID
	for _, d := range dups {
		k, there := keys[d.row]
		of, kept := keys[d.of]
		switch {
		case !there:
		case !kept || k != of:
			w.res.KeptChanged++
		default:
			del = append(del, d.row)
		}
	}
	for len(del) > 0 {
		out, err := transformBatch(ctx, w.table, BatchInput{Kind: w.kind(), Delete: del, Operation: w.op}, w.actor)
		if err == nil {
			w.res.ChangeSets = append(w.res.ChangeSets, out.ChangeSet)
			w.res.Deleted += len(del)
			return nil
		}
		n := len(del)
		var refused BatchErrors
		if errors.As(err, &refused) {
			gone := w.skip(refused)
			del = slices.DeleteFunc(del, func(id uuid.UUID) bool { return gone[id] })
		}
		if len(del) == n {
			return err
		}
	}
	return nil
}

// skip counts the rows a batch refused into the answer (the first maxSkipped
// listed), and names them.
func (w *stepWrite) skip(refused BatchErrors) map[uuid.UUID]bool {
	gone := map[uuid.UUID]bool{}
	for _, e := range refused {
		if e.Row == uuid.Nil {
			continue
		}
		gone[e.Row] = true
		w.res.SkippedCount++
		if len(w.res.Skipped) < maxSkipped {
			w.res.Skipped = append(w.res.Skipped, e)
		}
	}
	return gone
}

// rowsNow is the rows of ids that are there, each as actor may see it, read
// a window at a time as a view naming them is (rowsNamed).
func rowsNow(ctx context.Context, table uuid.UUID, actor Actor, ids []uuid.UUID) ([]*model.Row, error) {
	var out []*model.Row
	next := rowsNamed(table, actor, ids)
	for more := len(ids) > 0; more; {
		rows, m, err := next(ctx)
		if err != nil {
			return nil, err
		}
		out, more = append(out, rows...), m
	}
	return out, nil
}
