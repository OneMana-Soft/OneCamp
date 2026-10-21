//go:build integration

package integration_test

// PR3 (#34): the roles sweep reads a page's rows in one statement and writes
// only where they differ: it fills rows the graph implies, and a second pass
// over projects already in step writes nothing.
// Run: integ_rel.sh <wt> <log> 'TestPR3'

import (
	"context"
	"testing"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
)

func TestPR3TheRolesSweepSettlesAndThenLeavesProjectsAlone(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	w.exec(`DELETE FROM project_roles WHERE project_id = $1`, w.project)
	if _, err := projectBusiness.SettleRoles(ctx); err != nil {
		t.Fatal(err)
	}
	role := func(who string) string {
		var r string
		_ = w.env.PG.QueryRow(`SELECT role FROM project_roles WHERE project_id = $1 AND user_id = $2`, w.project, w.people[who]).Scan(&r)
		return r
	}
	if role("ana") != "admin" || role("ben") != "member" {
		t.Fatalf("ana %q, ben %q", role("ana"), role("ben"))
	}
	changed, err := projectBusiness.SettleRoles(ctx)
	if err != nil || changed != 0 {
		t.Fatalf("a second pass over a project in step changed %d: %v", changed, err)
	}
}
