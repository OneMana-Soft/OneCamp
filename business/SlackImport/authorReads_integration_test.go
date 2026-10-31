//go:build integration

package business

// A message a Slack import brings across is its author's, or it fails: never
// the importer's because the read of who its author came across as failed
// (audit package 36). importedPerson, the lookup a channel's and a DM's
// messages share, dropped its reads' errors (the import's map, the person's
// row, their graph node): a failed read went by as "didn't come across", and
// the message was posted or sent as the importing admin. Here the import's map
// can't be read for people (importDB.unreadable).
//
// Run: PKGS=./business/SlackImport/ integ_rel.sh <wt> <log> 'TestAFailedAuthorReadFailsTheMessage'

import (
	"testing"

	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
)

func TestAFailedAuthorReadFailsTheMessage(t *testing.T) {
	w := newImportWorld(t)
	const fromAda = `{"type":"message","user":"U2","text":"Thursday works for me","ts":"1792000000.000600"}`

	t.Run("a channel's message", func(t *testing.T) {
		job := w.newJob(t, "")
		c := w.chunk(t, job, importModels.ChunkChannelMessages, "C1")
		posts := func() int { return w.count(t, `SELECT count(*) FROM posts WHERE post_channel = $1`, w.general) }
		before := posts()
		restore := w.unreadable(t, "import_id_map", importModels.EntityUser)
		defer restore()
		w.run(t, c, "", fromAda)
		if n := posts(); n != before {
			t.Errorf("%d posts after the run that couldn't read its author, want %d: ada's message was posted as the importer", n, before)
		}
		w.logged(t, job, map[string]int{"MESSAGE_IMPORT_FAILED": 1})
	})

	t.Run("a DM's message", func(t *testing.T) {
		job := w.newJob(t, "")
		if _, err := resolveDMs(w.ctx, job, "", []SlackChannel{{ID: "D1", Members: []string{"U1", "U2"}}}, nil, w.admin); err != nil {
			t.Fatal(err)
		}
		c := w.chunk(t, job, importModels.ChunkDMMessages, "D1")
		chats := func() int { return w.count(t, `SELECT count(*) FROM chats`) }
		before := chats()
		restore := w.unreadable(t, "import_id_map", importModels.EntityUser)
		defer restore()
		w.run(t, c, "", fromAda)
		if n := chats(); n != before {
			t.Errorf("%d DM messages after the run that couldn't read its author, want %d: ada's message was sent as the importer", n, before)
		}
		w.logged(t, job, map[string]int{"DM_MESSAGE_FAILED": 1})
	})
}
