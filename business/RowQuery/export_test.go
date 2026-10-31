package business

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// FailSummaries has every summaries and groups read fail until restore is
// called, as a read past its budget or on a lost connection would.
func FailSummaries() (restore func()) {
	saved := summarize
	summarize = func(context.Context, uuid.UUID, string, string, string, string, []interface{}, time.Duration, func(pgexec.Row) error) error {
		return errors.New("the summaries read failed (FailSummaries)")
	}
	return func() { summarize = saved }
}

// AfterIndexBatch has fn run after each batch IndexTable commits, until
// restore is called: a write that lands while a table is walked.
func AfterIndexBatch(fn func(ctx context.Context, table uuid.UUID)) (restore func()) {
	saved := batchIndexed
	batchIndexed = fn
	return func() { batchIndexed = saved }
}

// AfterKeptWork has fn run in each batch that writes kept entries once its
// rows are worked out, before the entries are written, until restore is
// called: a pass or a write that lands between a batch's reads and its
// writes.
func AfterKeptWork(fn func(ctx context.Context, table uuid.UUID)) (restore func()) {
	saved := keptWorked
	keptWorked = fn
	return func() { keptWorked = saved }
}

// Oracle answers a spec over rows with the evaluator alone (evaluate, as
// runWorked does): the ids its windows show, in order, and the summaries and
// the groups that the SQL must give (the property test's other side). A
// formula's or a rollup's values are the rows' own, compared as what each
// gives (resultsOf), as runWorked compares what a read worked out.
func Oracle(spec query.Spec, fields []*model.Field, rows []*model.Row, me uuid.UUID, now time.Time, zone *time.Location) ([]string, query.Summaries, []query.GroupCount, error) {
	read := make([]*evalRow, len(rows))
	for i, r := range rows {
		read[i] = evalRowOf(r)
	}
	ev, err := evaluate(spec, columns(fields), kindsOf(resultsOf(fields)), read, asker{user: me, now: now, zone: zone}, true)
	if err != nil {
		return nil, nil, nil, err
	}
	ids := make([]string, len(ev.shown))
	for i, r := range ev.shown {
		ids[i] = r.id.String()
	}
	return ids, ev.summaries, ev.groups, nil
}

// resultsOf is what each formula and rollup among fields gives, by id, as a
// read sends it in the field's config ("result", business/DataTable
// presentComputedFields) and the browser reads it.
func resultsOf(fields []*model.Field) map[string]string {
	out := map[string]string{}
	for _, f := range fields {
		var cfg struct {
			Result string `json:"result"`
		}
		if (f.Type == model.FieldFormula || f.Type == model.FieldRollup) && json.Unmarshal([]byte(f.Config), &cfg) == nil && cfg.Result != "" {
			out[f.Id.String()] = cfg.Result
		}
	}
	return out
}

// ReadsComputed is whether spec filters, sorts, groups or adds up by a
// formula or a rollup among fields (computedRead): a view the Computed Store
// answers, or the reader's own read.
func ReadsComputed(spec query.Spec, fields []*model.Field) bool {
	return len(computedRead(spec, columns(fields))) > 0
}

// IndexTablesPass is one pass of the repair sweep, tables.cellindex.repair.
func IndexTablesPass(ctx context.Context) error { return indexTables(ctx) }

// RetryFirst is how long the repair sweep leaves a table after its first
// failure, before the jitter helpers.Backoff adds (up to a quarter more).
const RetryFirst = retryFirst
