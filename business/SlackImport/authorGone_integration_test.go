//go:build integration

package business

// V-1191: a Slack person the import mapped to someone who is no longer here
// (no row) didn't come across: their message is still brought across, as the
// importer's, as it always was. A read that fails still fails the message
// (TestAFailedAuthorReadFailsTheMessage); a person who isn't here doesn't.
//
// Run: PKGS=./business/SlackImport/ integ_rel.sh <wt> <log> 'TestAMessageFromSomeoneGoneIsStillBroughtAcross'

import (
	"testing"

	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
)

func TestAMessageFromSomeoneGoneIsStillBroughtAcross(t *testing.T) {
	w := newImportWorld(t)
	const fromGone = `{"type":"message","user":"U3","text":"see you all Thursday","ts":"1792000000.000700"}`
	job := w.newJob(t, "")
	w.exec(t, `INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid) VALUES ($1, $2, 'U3', $3)`,
		job, importModels.EntityUser, uuid.New())
	c := w.chunk(t, job, importModels.ChunkChannelMessages, "C1")
	posts := func() int { return w.count(t, `SELECT count(*) FROM posts WHERE post_channel = $1`, w.general) }
	before := posts()
	w.run(t, c, "", fromGone)
	if n := posts(); n != before+1 {
		t.Errorf("%d posts after the run, want %d: the message of someone no longer here wasn't brought across", n, before+1)
	}
	w.logged(t, job, map[string]int{"MESSAGE_IMPORT_FAILED": 0})
}
