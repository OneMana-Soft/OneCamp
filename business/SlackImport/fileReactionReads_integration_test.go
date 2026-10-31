//go:build integration

package business

// A file or a reaction a Slack import brings across is never made twice, put
// in the wrong place, or skipped for good because a read failed (audit package
// 36). The file worker reads whether this import made the file already, which
// conversation and message it belongs to, and who wrote that message; the
// reaction pass reads what each conversation, message and person became, and
// the emoji map. Both dropped those reads' errors: a failed read went by as
// "not there", so a retried file was fetched and made again (or noted as not
// read), a file was filed on its channel and not its message, and reactions
// were skipped as if their message or person never came across. Now a failed
// read is the chunk's error, and the chunk is tried again. Here a map can't be
// read for one kind of row (importDB.unreadable).
//
// Run: PKGS=./business/SlackImport/ integ_rel.sh <wt> <log> 'TestAFailedFileOrReactionReadIsTriedAgain'

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
)

func TestAFailedFileOrReactionReadIsTriedAgain(t *testing.T) {
	w := newImportWorld(t)
	// readFailed is whether err is the fault's failed read, not a step's own
	// answer ("could not resolve parent conversation") or none at all.
	readFailed := func(err error) bool {
		var pg *pgconn.PgError
		return errors.As(err, &pg)
	}

	file := SlackFile{ID: "F1", Name: "notes.txt", Size: 5, URLPrivateDownload: "https://files.slack.com/files-pri/T1-F1/notes.txt"}
	fileChunk := func(t *testing.T, job uuid.UUID, conversation string) (*importModels.Chunk, *fileChunkMeta) {
		t.Helper()
		c := buildFileChunk(job, conversation, "1792000000.000100", file)
		if err := importModels.CreateChunks(w.ctx, []*importModels.Chunk{c}); err != nil {
			t.Fatal(err)
		}
		meta, err := parseFileChunkMeta(*c.ObjectKey)
		if err != nil {
			t.Fatal(err)
		}
		return c, meta
	}

	t.Run("a file this import made: its map", func(t *testing.T) {
		job := w.newJob(t, "")
		c, _ := fileChunk(t, job, "C1")
		w.exec(t, `INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid) VALUES ($1, $2, $3, $4)`,
			job, importModels.EntityFile, file.ID, uuid.New())
		restore := w.unreadable(t, "import_id_map", importModels.EntityFile)
		defer restore()
		// With no uploads and links closed, a file the worker went on to read
		// is noted as not read (FILE_NOT_READ), as a new one would be.
		if err := processFileChunk(w.quiet, fileSource{}, c, w.admin, 0); !readFailed(err) {
			t.Errorf("the file's chunk answered %v; want its map's failed read, so the chunk is tried again", err)
		}
		w.logged(t, job, map[string]int{"FILE_NOT_READ": 0})
	})

	for _, c := range []struct {
		name, conversation, kind string
	}{
		{name: "a file's message: its map", conversation: "C1", kind: importModels.EntityMessage},
		{name: "a file's DM: its map", conversation: "D1", kind: importModels.EntityDM},
	} {
		t.Run(c.name, func(t *testing.T) {
			chunk, meta := fileChunk(t, w.newJob(t, ""), c.conversation)
			restore := w.unreadable(t, "import_id_map", c.kind)
			defer restore()
			if parent, err := resolveFileParent(w.ctx, chunk, meta); !readFailed(err) {
				t.Errorf("the file's place came to %+v, %v; want its map's failed read", parent, err)
			}
		})
	}

	// ada's 👍 and that of someone the graph has no node for (U3), on a
	// message in #general.
	reacted := `{"type":"message","user":"U1","text":"Shipping Friday","ts":"1792000000.000500","reactions":[{"name":"thumbsup","users":["U2","U3"],"count":2}]}`
	export := &ParsedExport{Channels: []SlackChannel{{ID: "C1", Name: "general"}}, MessageFiles: map[string][]string{"C1": {dayFile("C1")}}}
	tally := func(t *testing.T, job uuid.UUID, field string) int {
		return w.count(t, `SELECT COALESCE((progress->'reactions'->>$2)::int, 0) FROM import_jobs WHERE id = $1`, job, field)
	}
	for _, c := range []struct {
		name, table, kind string
	}{
		{name: "reactions: their message's map", table: "import_id_map", kind: importModels.EntityMessage},
		{name: "reactions: their people's map", table: "import_id_map", kind: importModels.EntityUser},
		{name: "reactions: the emoji map", table: "slack_emoji_map"},
	} {
		t.Run(c.name, func(t *testing.T) {
			job := w.newJob(t, "")
			w.exec(t, `INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid) VALUES ($1, $2, 'U3', $3)`,
				job, importModels.EntityUser, uuid.New())
			w.run(t, w.chunk(t, job, importModels.ChunkChannelMessages, "C1"), "", reacted)
			pass := w.chunk(t, job, importModels.ChunkReactionPass, "C1")
			restore := w.unreadable(t, c.table, c.kind)
			err := processReactionPass(w.quiet, exportWith(t, "C1", reacted), pass, export)
			restore()
			if !readFailed(err) {
				t.Errorf("the reaction pass answered %v; want the failed read, so the pass is tried again", err)
			}
			// Tried again with its reads back, the pass brings ada's reaction
			// across, and skips the person the graph has no node for.
			if err := processReactionPass(w.quiet, exportWith(t, "C1", reacted), pass, export); err != nil {
				t.Fatal(err)
			}
			if n := tally(t, job, "applied"); n != 1 {
				t.Errorf("%d reactions brought across, want ada's", n)
			}
			if n := tally(t, job, "skipped_user"); n != 1 {
				t.Errorf("%d reactions skipped for their person, want U3's", n)
			}
		})
	}
}
