package models

// An import's report (import_errors), written a batch at a time. Notes aren't
// capped: a table's importer notes each cell it leaves out, a tree of pages
// each part, and a statement a note ran thousands of them for a messy import.
// Now a worker holds its notes and writes them together, a thousand to a
// statement (LogImportErrors); LogImportError is one note of it.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ImportError is one entry of an import's report: an error, a warning or a
// fatal about something it read (EntityType, by the source's SourceID), from
// a chunk or from the import as a whole (ChunkID nil).
type ImportError struct {
	ChunkID              *uuid.UUID
	EntityType, SourceID string
	Severity, Code       string
	Message              string
	Context              json.RawMessage
}

// importErrorsPerStatement is how many entries one statement writes.
const importErrorsPerStatement = 1000

// noteTimeout bounds one statement of notes: a worker never waits long on
// its report.
const noteTimeout = 2 * time.Second

// LogImportErrors writes entries to the import's report, a statement for
// each thousand, in their order: within a statement each is a microsecond
// after the one before, so the report (newest first) reads them as written.
// Best effort, as a report is: a statement that fails is logged, and never
// stops the worker. A message's and a context's URLs lose their queries
// (helpers.WithoutURLQueries), and text Postgres can't keep (a NUL, bytes
// that aren't UTF-8, a context that isn't JSON) is cleaned rather than
// refusing the statement, and with it every other entry in it.
func LogImportErrors(ctx context.Context, importId uuid.UUID, entries []ImportError) {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return // not connected (a provider's unit test): nowhere to keep them
	}
	for len(entries) > 0 {
		n := min(len(entries), importErrorsPerStatement)
		cols := make([][]string, 7)
		for i := range cols {
			cols[i] = make([]string, n)
		}
		for i, e := range entries[:n] {
			if e.ChunkID != nil {
				cols[0][i] = e.ChunkID.String()
			}
			cols[1][i], cols[2][i] = storable(e.EntityType), storable(e.SourceID)
			cols[3][i], cols[4][i] = storable(e.Severity), storable(e.Code)
			cols[5][i] = storable(helpers.WithoutURLQueries(e.Message))
			cols[6][i] = storableJSON(contextWithoutURLQueries(e.Context))
		}
		args := []any{importId}
		for _, c := range cols {
			args = append(args, pq.Array(c))
		}
		c, cancel := context.WithTimeout(ctx, noteTimeout)
		_, err := pgexec.Exec(c, `
			INSERT INTO import_errors
			    (import_id, chunk_id, entity_type, source_id, severity, code, message, context, created_at)
			SELECT $1, NULLIF(e.chunk, '')::uuid, NULLIF(e.entity, ''), NULLIF(e.source, ''), e.severity,
			       NULLIF(e.code, ''), e.message, NULLIF(e.context, '')::jsonb, NOW() + e.n * interval '1 microsecond'
			FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[])
			     WITH ORDINALITY AS e(chunk, entity, source, severity, code, message, context, n)`, args...)
		cancel()
		if err != nil {
			helpers.LogErrorWithContext(ctx, "models/Import.LogImportErrors swallowed err (%d entries): %+v", n, err)
		}
		entries = entries[n:]
	}
}

// storable is s as a text column keeps it: Postgres refuses a NUL and bytes
// that aren't UTF-8. Pure.
func storable(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
}

// storableJSON is raw as the context column keeps it, or "" (none) for what
// jsonb can't hold: text that isn't JSON, or a NUL escaped in it. Pure.
func storableJSON(raw json.RawMessage) string {
	if len(raw) == 0 || !json.Valid(raw) || bytes.Contains(raw, []byte(`\u0000`)) {
		return ""
	}
	return string(raw)
}
