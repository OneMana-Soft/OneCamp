//go:build integration

package business

// A message a Slack import brought across is never made twice because a read
// of its map failed, and an edit or a deletion that couldn't read its message
// says so (audit package 36). The channel and DM workers read what this
// import, or an earlier import of the workspace, had made for each message,
// and dropped those reads' errors: a failed read went by as "not made yet", so
// a retried chunk, or the workspace's next import, made the message again; a
// DM reply was skipped as having no parent; and an edit or a deletion went by
// without a word. Here a map can't be read for messages but can for the rest,
// and written (importDB.unreadable).
//
// Run: PKGS=./business/SlackImport/ integ_rel.sh <wt> <log> 'TestAFailedMessageMapReadMakesNoSecondPost'

import (
	"testing"

	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
)

func TestAFailedMessageMapReadMakesNoSecondPost(t *testing.T) {
	w := newImportWorld(t)
	posts := func(t *testing.T) int {
		return w.count(t, `SELECT count(*) FROM posts WHERE post_channel = $1`, w.general)
	}
	dms := func(t *testing.T) int {
		return w.count(t, `SELECT count(*) FROM chats WHERE created_by = $1`, w.admin.UserPostgresInfo.Id)
	}

	// A message, a reply in its thread, and the message edited, then deleted.
	const (
		launch  = `{"type":"message","user":"U1","text":"The launch moved to Thursday","ts":"1792000000.000100"}`
		reply   = `{"type":"message","user":"U2","text":"Thursday works","ts":"1792000000.000200","thread_ts":"1792000000.000100"}`
		edited  = `{"type":"message","subtype":"message_changed","ts":"1792000000.000300","message":{"type":"message","user":"U1","text":"The launch moved to Friday","ts":"1792000000.000100"}}`
		deleted = `{"type":"message","subtype":"message_deleted","ts":"1792000000.000400","previous_message":{"ts":"1792000000.000100","text":"The launch moved to Friday"}}`
	)

	t.Run("a channel's chunk again: this import's map", func(t *testing.T) {
		job := w.newJob(t, "")
		c := w.chunk(t, job, importModels.ChunkChannelMessages, "C1")
		none := posts(t)
		w.run(t, c, "", launch)
		once := posts(t)
		if once != none+1 {
			t.Fatalf("the first run left %d posts, want %d", once, none+1)
		}
		restore := w.unreadable(t, "import_id_map", importModels.EntityMessage)
		defer restore()
		w.run(t, c, "", launch, edited, deleted)
		if n := posts(t); n != once {
			t.Errorf("%d posts after the run that couldn't read its map, want %d: the message was posted again", n, once)
		}
		w.logged(t, job, map[string]int{"MESSAGE_IMPORT_FAILED": 1, "MESSAGE_EDIT_FAILED": 1, "MESSAGE_DELETE_FAILED": 1})
	})

	t.Run("the workspace's next import: the workspace's map", func(t *testing.T) {
		first := w.newJob(t, "Acme")
		w.run(t, w.chunk(t, first, importModels.ChunkChannelMessages, "C1"), "Acme", launch)
		once := posts(t)
		next := w.newJob(t, "Acme")
		restore := w.unreadable(t, "import_workspace_id_map", importModels.EntityMessage)
		defer restore()
		w.run(t, w.chunk(t, next, importModels.ChunkChannelMessages, "C1"), "Acme", launch, edited)
		if n := posts(t); n != once {
			t.Errorf("%d posts after the import that couldn't read the workspace's map, want %d: the message was posted again", n, once)
		}
		w.logged(t, next, map[string]int{"MESSAGE_IMPORT_FAILED": 1, "MESSAGE_EDIT_FAILED": 1})
	})

	t.Run("a DM's chunk again: this import's map", func(t *testing.T) {
		job := w.newJob(t, "")
		if _, err := resolveDMs(w.ctx, job, "", []SlackChannel{{ID: "D1", Members: []string{"U1", "U2"}}}, nil, w.admin); err != nil {
			t.Fatal(err)
		}
		c := w.chunk(t, job, importModels.ChunkDMMessages, "D1")
		none := dms(t)
		w.run(t, c, "", launch)
		once := dms(t)
		if once != none+1 {
			t.Fatalf("the first run left %d DM messages, want %d", once, none+1)
		}
		restore := w.unreadable(t, "import_id_map", importModels.EntityMessage)
		defer restore()
		w.run(t, c, "", launch, reply, edited, deleted)
		if n := dms(t); n != once {
			t.Errorf("%d DM messages after the run that couldn't read its map, want %d: the message was sent again", n, once)
		}
		// The reply's parent couldn't be read: the reply failed, and isn't put
		// down to a parent that never came across.
		w.logged(t, job, map[string]int{"DM_MESSAGE_FAILED": 1, "DM_COMMENT_FAILED": 1, "DM_EDIT_FAILED": 1, "DM_DELETE_FAILED": 1,
			"THREAD_PARENT_NOT_FOUND": 0})
	})
}
