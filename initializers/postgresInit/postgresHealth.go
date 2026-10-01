package postgresInit

// Is the relational store answering?
//
// The app refuses to boot when the schema is behind the build, so a running
// server has already proved the schema matched AT BOOT. It has not proved the
// database is still there: a Postgres that dies afterwards leaves a process that
// keeps serving, failing every write, with the boot log still saying it
// connected.

import (
	"context"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
)

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "database",
		Kind: helpers.CheckKindDependency,
		Describe: "Postgres answers right now. The schema was already checked at boot, which is why the " +
			"server is running at all; this proves the database has not gone away since.",
		Probe: func(ctx context.Context) error {
			if DBConn == nil || DBConn.SqlDB == nil {
				return fmt.Errorf("no Postgres connection: the database was never initialised")
			}
			dbCtx, cancel := context.WithTimeout(ctx, DBConn.DBTimeout)
			defer cancel()
			if err := DBConn.SqlDB.PingContext(dbCtx); err != nil {
				return fmt.Errorf("Postgres did not answer: %w", err)
			}
			return nil
		},
	})
}
