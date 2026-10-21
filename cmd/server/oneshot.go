package main

// One-shot subcommands (helpers.Subcommands): each runs once against this
// server's own stores and exits. main connects every store's client, then
// hands a one-shot to runOneShot BEFORE anything that makes the process a
// server starts: the Redis expiry worker, the AI reconcile, the seeds and
// registries, the loops, the port. So a one-shot run beside the live
// instance, even a dry run, never sends a mail, claims a job, runs an agent's
// task or counts a device that the server is handling already
// (helpers.RoleOneShot; helpers/oneShotGuard_test.go holds main to it).
//
// A new one-shot is a name in helpers.Subcommands and a runner here; the
// guard fails until both are there.

import (
	"context"
	"fmt"
	"os"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	commandBusiness "github.com/akashc777/OneCamp/business/Command"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	refsBusiness "github.com/akashc777/OneCamp/business/Refs"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	demoseed "github.com/akashc777/OneCamp/services/DemoSeed"
)

// oneShots runs each one-shot subcommand: its own arguments in, an exit code out.
var oneShots = map[string]func(ctx context.Context, args []string) int{
	"demoseed":      runDemoSeed,
	"repairdocs":    runRepairDocs,
	"calendarmove":  runCalendarMove,
	"workgraphrefs": runWorkGraphRefs,
}

// oneShotSettle is how long work a one-shot handed to the background (a
// search entry, a notice) gets to finish before the process exits.
const oneShotSettle = 15 * time.Second

// runOneShot is the one way a one-shot runs. Beyond the stores' clients it
// sets up only what isn't a loop: the command framework's context, which
// its deferred paths read. Work the command handed to the background
// finishes before it exits, and its broker connection closes cleanly.
func runOneShot(ctx context.Context, name string, args []string) int {
	run, ok := oneShots[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "%s: this build has no such command\n", name)
		return 2
	}
	commandBusiness.SetAppContext(ctx)
	code := run(ctx, args)
	if busy := helpers.SettleBackground(oneShotSettle); len(busy) > 0 {
		fmt.Fprintf(os.Stderr, "%s: still running when it exited, and cut off: %v\n", name, busy)
	}
	if mqttInit.MqttClient != nil {
		mqttInit.MqttClient.Disconnect(250)
	}
	return code
}

// hasFlag says whether args name flag.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// runDemoSeed is `go-one-camp demoseed <admin-email> [--refresh]`: it curates
// the public demo workspace. It writes through the business layer, so it
// needs Postgres, the graph and the object store connected, and it refuses
// unless ONECAMP_DEMO_HOST is set: every customer runs this binary, and a
// command that writes invented conversations into a workspace is one
// mistyped word away from putting fake content into somebody's real company.
func runDemoSeed(ctx context.Context, args []string) int {
	adminEmail := ""
	for _, a := range args {
		if a != "--refresh" && adminEmail == "" {
			adminEmail = a
		}
	}
	return demoseed.Main(ctx, adminEmail, hasFlag(args, "--refresh"))
}

// runRepairDocs is `go-one-camp repairdocs [--apply]`: it finds docs whose
// body a collaboration service before v2.70.0 replaced with "<p>Error
// generating HTML</p>" (or left far shorter than its saved Yjs state) and,
// with --apply, restores them from that state or from their version history.
// Without --apply it only lists what it would do. Idempotent;
// business/Doc/docRepair.go.
func runRepairDocs(ctx context.Context, args []string) int {
	rep, err := docBusiness.RepairDocs(ctx, hasFlag(args, "--apply"), os.Stdout)
	// A restored doc's search entry is written in the background: exiting at
	// once cut the last of those writes off, and those docs kept the
	// placeholder's text in search.
	if pending := docBusiness.WaitForSearchWrites(ctx, 10*time.Minute); pending > 0 {
		fmt.Fprintf(os.Stderr, "repairdocs: stopped waiting with %d docs' search entries still being written; each is written again when its doc is next saved\n", pending)
	}
	if err != nil || len(rep.Failed) > 0 {
		if err != nil {
			fmt.Fprintln(os.Stderr, "repairdocs:", err)
		}
		return 1
	}
	return 0
}

// runCalendarMove is `go-one-camp calendarmove [--apply]`: it moves the
// calendar's events from the two old stores (the graph's Event nodes and
// calendar_events) into its one store, and checks that what it wrote reads
// back as meant. Without --apply it only counts what it would do.
// Idempotent, and safe whichever store is in use: nothing reads the one store
// until it is on, and once on, the server moves by itself the first time it
// is asked. business/Calendar/move.go. Prints counts and ids, never what an
// event says.
func runCalendarMove(ctx context.Context, args []string) int {
	rep, err := calendarBusiness.MoveToPostgres(ctx, hasFlag(args, "--apply"))
	fmt.Fprintln(os.Stdout, "calendarmove:", rep)
	if err != nil || rep.Different > 0 {
		if err != nil {
			fmt.Fprintln(os.Stderr, "calendarmove:", err)
		}
		return 1
	}
	return 0
}

// runWorkGraphRefs is `go-one-camp workgraphrefs [--apply]`: it re-derives
// the work graph's links (refers_to: what each message, direct message,
// comment and doc links to, and the tasks each doc holds a to-do for) from the
// bodies, every one from the beginning, and prints the drift per kind. With
// --apply it fixes it: after a restore from backup, or to check the nightly
// reconcile. Idempotent; prints counts, never what a body says.
// business/Refs/reconcile.go.
func runWorkGraphRefs(ctx context.Context, args []string) int {
	reports, err := refsBusiness.ReconcileAll(ctx, refsBusiness.Options{Apply: hasFlag(args, "--apply"), Mirror: -1})
	for _, r := range reports {
		fmt.Fprintln(os.Stdout, "workgraphrefs:", r)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "workgraphrefs:", err)
		return 1
	}
	return 0
}
