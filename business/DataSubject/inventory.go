package business

// Personal-data inventory: every place in Postgres that holds rows belonging to
// one person.
//
// WHY IT DISCOVERS RATHER THAN LISTS. There are already more than forty
// user-referencing columns across as many tables, and the set grows with every
// feature. A hand-maintained list is wrong the day after it is written, and it
// fails silently: the answer still looks complete, it is just missing whatever
// was added last. This asks the catalog instead, so a new table with a user
// column is included the moment it exists.
//
// WHAT IT IS FOR. Two obligations need to know where a person's data lives
// before anything else can happen: a subject access request, and an erasure
// request. This answers that question and nothing more.
//
// WHAT IT IS NOT. It is NOT a GDPR Article 15 response. That requires a copy of
// the personal data, and the CJEU has held that "copy" means a faithful and
// intelligible reproduction, not a list of categories or counts. This produces
// counts. It is the map the export and the erasure are built from, and calling
// it compliance would be wrong.
//
// READ-ONLY BY CONSTRUCTION. It issues SELECT COUNT(*) and nothing else. Erasure
// is a separate, deliberate act.

import (
	"context"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// maxScannedColumns bounds one inventory run. Forty-odd columns exist today;
// this leaves headroom while stopping a pathological schema from turning one
// admin request into thousands of counts.
const maxScannedColumns = 200

// userColumnNames are the column names taken to mean "this row belongs to that
// person". Listed explicitly rather than matched loosely, because a wildcard
// over every uuid column would sweep in unrelated foreign keys (channel_uuid,
// task_uuid) and report a person as present wherever they are merely referenced
// by something they touched.
//
// A new naming convention needs an entry here. That is deliberate: the set of
// columns is discovered, but what COUNTS as ownership is a judgement.
var userColumnNames = []string{
	"user_id",
	"user_uuid",
	"created_by",
	"owner_id",
	"owner_user_id",
	"author_id",
	"run_as_user_id",
	"bot_user_id",
	"updated_by",
}

// PersonalDataLocation is one place rows belonging to a person were found.
type PersonalDataLocation struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Rows   int64  `json:"rows"`
}

// Inventory returns every table and column holding rows for userID, with a count
// each. Locations with no rows are omitted, so the result is what actually
// exists rather than the schema's shape.
//
// Ordered by table then column so two runs are comparable, which is what makes
// it usable as evidence that an erasure did what it said.
func Inventory(ctx context.Context, userID uuid.UUID) ([]PersonalDataLocation, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("inventory: no user id")
	}

	columns, err := discoverUserColumns(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]PersonalDataLocation, 0, len(columns))
	for _, c := range columns {
		n, err := countRowsFor(ctx, c.table, c.column, userID)
		if err != nil {
			// One unreadable table must not hide the rest of the map. Report it
			// and continue: an incomplete answer that says so is useful, an
			// answer that silently drops a table is not.
			helpers.LogErrorWithContext(ctx,
				"DataSubject/Inventory: counting %s.%s failed: %v", c.table, c.column, err)
			continue
		}
		if n > 0 {
			out = append(out, PersonalDataLocation{Table: c.table, Column: c.column, Rows: n})
		}
	}
	return out, nil
}

// TotalRows is the sum across every location, for the one-line summary an
// operator reads first.
func TotalRows(locations []PersonalDataLocation) int64 {
	var total int64
	for _, l := range locations {
		total += l.Rows
	}
	return total
}

type userColumn struct{ table, column string }

// discoverUserColumns asks the catalog which columns in the current schema name
// a user. Restricted to uuid columns in BASE TABLEs: a view would double-count
// its underlying table, and a non-uuid column of the same name is something else
// wearing the same word.
func discoverUserColumns(ctx context.Context) ([]userColumn, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		SELECT c.table_name, c.column_name
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema = current_schema()
		  AND t.table_type = 'BASE TABLE'
		  AND c.data_type = 'uuid'
		  AND c.column_name = ANY($1)
		ORDER BY c.table_name, c.column_name
		LIMIT $2`

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q, pq.Array(userColumnNames), maxScannedColumns)
	if err != nil {
		return nil, fmt.Errorf("discover user columns: %w", err)
	}
	defer rows.Close()

	var out []userColumn
	for rows.Next() {
		var c userColumn
		if err := rows.Scan(&c.table, &c.column); err != nil {
			return nil, fmt.Errorf("scan user column: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user columns: %w", err)
	}
	return out, nil
}

// countRowsFor counts one table's rows for a user.
//
// The table and column are interpolated because SQL has no parameter form for an
// identifier. They come from the catalog rather than from a request, and they are
// quoted with pq.QuoteIdentifier regardless, following the same rule
// business/DataSource already applies: a name from the database is still a name
// being pasted into SQL, and correctness here should not depend on trusting its
// source. The VALUE is always a bound parameter.
func countRowsFor(ctx context.Context, table, column string, userID uuid.UUID) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = $1`,
		pq.QuoteIdentifier(table), pq.QuoteIdentifier(column))

	var n int64
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, userID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
