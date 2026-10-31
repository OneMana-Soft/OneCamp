//go:build integration

package business

// What a Slack import reads of its maps, it reads, or the step fails (audit
// package 36). The people step and the conversations step dropped their reads'
// errors: a failed read went by as "not brought across yet". The people step
// set out to make the person again, and what it said was "could not create
// external user" (the address was taken). The conversations step recorded the
// DM or group again. Here a map can't be read but can still be written
// (importDB.unreadable).
//
// Run: PKGS=./business/SlackImport/ integ_rel.sh <wt> <log> 'TestAFailedMapReadFailsItsStep'

import (
	"testing"

	"github.com/google/uuid"

	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
)

func TestAFailedMapReadFailsItsStep(t *testing.T) {
	db := newImportDB(t)

	for _, c := range []struct {
		name, workspace, table string
		person                 SlackUser
	}{
		{name: "people: this import's own map", table: "import_id_map", person: SlackUser{ID: "U1", Name: "maya", RealName: "Maya Chen"}},
		{name: "people: the workspace's map", workspace: "Acme", table: "import_workspace_id_map", person: SlackUser{ID: "U2", Name: "ada", RealName: "Ada Okafor"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			first := db.newJob(t, c.workspace)
			if _, err := resolveUsers(db.ctx, first, c.workspace, []SlackUser{c.person}, uuid.Nil); err != nil {
				t.Fatal(err)
			}
			// The run again: the same import retried, or the workspace's next import.
			again := first
			if c.workspace != "" {
				again = db.newJob(t, c.workspace)
			}
			before := db.count(t, `SELECT count(*) FROM users`)
			restore := db.unreadable(t, c.table, "")
			_, err := resolveUsers(db.ctx, again, c.workspace, []SlackUser{c.person}, uuid.Nil)
			restore()
			if err == nil {
				t.Error("a failed read went by as nothing brought across yet; want the step to fail")
			}
			if n := db.count(t, `SELECT count(*) FROM users`); n != before {
				t.Errorf("%d people after the run that couldn't read, want %d", n, before)
			}
		})
	}

	// A DM's or group's own map is never the read that fails first: the step
	// reads its members' people from that map before anything else, and stops
	// there. The workspace's map is read for the conversation alone.
	for _, c := range []struct {
		name, kind string
		people     int
	}{
		{name: "a DM: the workspace's map", kind: importModels.EntityDM, people: 2},
		{name: "a group: the workspace's map", kind: importModels.EntityMPIM, people: 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			job := db.newJob(t, "Acme")
			members := []string{"U3", "U4", "U5"}[:c.people]
			for _, id := range members {
				db.exec(t, `INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid)
					VALUES ($1, $2, $3, $4)`, job, importModels.EntityUser, id, uuid.New())
			}
			var dms, groups []SlackChannel
			if c.kind == importModels.EntityDM {
				dms = []SlackChannel{{ID: "D1", Members: members}}
			} else {
				groups = []SlackChannel{{ID: "G1", Members: members}}
			}
			restore := db.unreadable(t, "import_workspace_id_map", "")
			_, err := resolveDMs(db.ctx, job, "Acme", dms, groups, nil)
			restore()
			if err == nil {
				t.Error("a failed read went by as nothing brought across yet; want the step to fail")
			}
			if n := db.count(t, `SELECT count(*) FROM import_id_map WHERE import_id = $1 AND entity_type = $2`, job, c.kind); n != 0 {
				t.Errorf("the run that couldn't read recorded the %s %d time(s), want none", c.kind, n)
			}
		})
	}
}
