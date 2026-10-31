package business

// Each person's stray graph nodes merged into their own node, once per install
// (domain/User strayNodes.go says what a stray is and where they came from).
//
// Since 26 Oct every write names a person by their own node, so a stray only
// keeps a stale copy of its person and the edges written through it. The
// merge moves those edges onto the person's own node and deletes the strays,
// with main's safeguards: every node and edge it changes is backed up in
// person_merge_backups before the change, and RestoreStrayMerge puts a run
// back; a person whose nodes hold different emails is left and counted (one
// whose names differ renamed themselves, and is merged and counted); a
// dry run, the default, only says what the merge would do; it runs once per
// install through the Repair runner, a batch of strays per graph transaction;
// and the audit log says what it did, in counts alone. A server job with no
// route: it changes no one's access, since every edge keeps its meaning on
// the person's own node, which every check by uuid already reached.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	adminAuditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	repairBusiness "github.com/akashc777/OneCamp/business/Repair"
	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const (
	strayMergeKey       = "person_nodes_merged_v1"
	strayMergeDryRunKey = "person_nodes_merge_dry_run_v1"
	// strayMergePage is how many nodes holding a user_uuid one read pages
	// through.
	strayMergePage = 1000
	// strayMergeBatch is how many strays one graph transaction merges.
	strayMergeBatch = 200
	// strayMergeTries bounds the retries in one process; the next start tries
	// again.
	strayMergeTries = 12
	// strayMergeMode is what a start does: "dry-run" (the default) says what
	// the merge would do, "run" merges, "off" does neither.
	strayMergeMode = "PERSON_NODE_MERGE"
	// strayMergeRestore is a run id a start puts back, in place of the above.
	strayMergeRestore = "PERSON_NODE_MERGE_RESTORE"
)

// StrayMerge is what a merge did, or what a dry run says it would do.
type StrayMerge struct {
	Run     uuid.UUID // the run its backups are under; none on a dry run
	People  int       // people whose strays were merged
	Strays  int       // strays merged
	Moved   int       // edges into strays moved onto their person's own node
	Added   int       // the strays' own edges their person lacked, given to them
	Kept    int       // edges the person already had, dropped with the stray
	Skipped int       // people whose nodes hold different emails, left
	Renamed int       // of People, those whose strays hold another name: merged, their own node's name kept
}

func (r StrayMerge) String() string {
	did := "merged into their own (backups: run " + r.Run.String() + ")"
	if r.Run == uuid.Nil {
		did = "would be merged into their own (a dry run)"
	}
	return fmt.Sprintf("%d people's %d stray graph nodes %s: %d edges into them moved, %d of their own added, %d the person had; %d of the people renamed since, their own node's name kept; %d people left, their nodes holding different emails",
		r.People, r.Strays, did, r.Moved, r.Added, r.Kept, r.Renamed, r.Skipped)
}

// StrayRestore is what a restore put back.
type StrayRestore struct {
	Strays int // strays written back
	Edges  int // edges into them pointed at them again
}

var (
	strayMergeDryRun = repairBusiness.Once[StrayMerge]{
		Key: strayMergeDryRunKey, Name: "user.stray-node-merge-dry-run", Tries: strayMergeTries,
		Repair: func(ctx context.Context) (StrayMerge, error) { return MergeStrayPeople(ctx, true) },
	}
	strayMergeRun = repairBusiness.Once[StrayMerge]{
		Key: strayMergeKey, Name: "user.stray-node-merge", Tries: strayMergeTries,
		Repair: func(ctx context.Context) (StrayMerge, error) { return MergeStrayPeople(ctx, false) },
	}
)

// StartStrayMerge starts, in the background, what PERSON_NODE_MERGE asks for:
// the dry run unless it says "run" or "off", each once per install. With
// PERSON_NODE_MERGE_RESTORE set to a run's id, it puts that run back instead.
func StartStrayMerge(ctx context.Context) {
	if v := strings.TrimSpace(os.Getenv(strayMergeRestore)); v != "" {
		run, err := uuid.Parse(v)
		if err != nil {
			helpers.MessageLogs.ErrorLog.Printf("%s=%q: not a run id, so nothing is restored", strayMergeRestore, v)
			return
		}
		helpers.RetryInBackground(ctx, "user.stray-node-merge-restore",
			helpers.Retries{First: 30 * time.Second, Cap: 30 * time.Minute, Tries: strayMergeTries},
			func(ctx context.Context) error {
				back, err := RestoreStrayMerge(ctx, run)
				if err == nil {
					helpers.MessageLogs.InfoLog.Printf("user.stray-node-merge-restore: run %s: %d strays and %d edges into them put back as they were at the run (the same edges written since are reverted too)", run, back.Strays, back.Edges)
				}
				return err
			})
		return
	}
	switch strings.TrimSpace(os.Getenv(strayMergeMode)) {
	case "off":
	case "run":
		strayMergeRun.Start(ctx)
	default:
		strayMergeDryRun.Start(ctx)
	}
}

// MergeStrayPeople merges every person's strays into their own node, a batch
// at a time, each batch backed up first; with dryRun it only reads, and says
// what it would do. A real run first settles the batches a crash left
// half-recorded.
func MergeStrayPeople(ctx context.Context, dryRun bool) (rep StrayMerge, err error) {
	if !dryRun {
		if err := settleMergeBatches(ctx); err != nil {
			return rep, err
		}
		rep.Run = uuid.New()
	}
	preds, err := domain.LivePredicates(ctx)
	if err != nil {
		return rep, err
	}
	var batch []domain.StrayGroup
	size := 0
	merge := func() error {
		if size == 0 {
			return nil
		}
		err := mergeStrayBatch(ctx, preds, batch, dryRun, &rep)
		batch, size = nil, 0
		return err
	}
	// A dry run leaves every stray where it is, and the person a run leaves
	// keeps theirs, so a later page meets their uuid again.
	seen := map[string]bool{}
	for after := ""; ; {
		groups, next, err := domain.PeopleWithStrays(ctx, after, strayMergePage)
		if err != nil {
			return rep, err
		}
		for _, g := range groups {
			if seen[g.UserUUID] {
				continue
			}
			seen[g.UserUUID] = true
			if g.Disagree {
				rep.Skipped++
				continue
			}
			rep.People++
			if g.Renamed {
				// Main's ruling (26 Oct): a name alone differing is a rename,
				// counted and logged by uuid, never by name.
				rep.Renamed++
				helpers.MessageLogs.InfoLog.Printf("user.stray-node-merge: %s's strays hold another name than their own node; merged as one person, their own node's name kept", g.UserUUID)
			}
			for len(g.Strays) > 0 {
				part := g
				part.Strays = g.Strays[:min(len(g.Strays), strayMergeBatch-size)]
				g.Strays = g.Strays[len(part.Strays):]
				batch = append(batch, part)
				if size += len(part.Strays); size == strayMergeBatch {
					if err := merge(); err != nil {
						return rep, err
					}
				}
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	if err := merge(); err != nil {
		return rep, err
	}
	if !dryRun && (rep.Strays > 0 || rep.Skipped > 0) {
		recordStrayMerge(ctx, rep)
	}
	return rep, nil
}

// mergeStrayBatch merges one batch of strays in one graph transaction, its
// backup written first and marked applied once the transaction has landed.
// A concurrent write to an edge it moves (a role taken away meanwhile) makes
// Dgraph abort it, and it is read and planned again.
func mergeStrayBatch(ctx context.Context, preds []domain.GraphPredicate, groups []domain.StrayGroup, dryRun bool, rep *StrayMerge) error {
	batch := uuid.New()
	var plan *domain.StrayPlan
	err := dgexec.InTxn(ctx, func(tx *dgexec.Txn) error {
		var err error
		if plan, err = domain.PlanStrayMerge(tx, preds, groups); err != nil || dryRun {
			return err
		}
		rows, err := mergeBackups(plan)
		if err != nil {
			return err
		}
		if err := userModels.BackUpMergeBatch(ctx, rep.Run, batch, rows); err != nil {
			return err
		}
		_, err = tx.Mutate("", nil, plan.Mutation())
		return err
	})
	if err != nil {
		return err
	}
	if !dryRun {
		if err := userModels.MarkMergeBatch(ctx, rep.Run, batch, "pending", "applied"); err != nil {
			return err
		}
	}
	rep.Strays += len(plan.Nodes)
	for _, e := range plan.In {
		if e.Added {
			rep.Moved++
		} else {
			rep.Kept++
		}
	}
	for _, e := range plan.Out {
		if e.Added {
			rep.Added++
		} else {
			rep.Kept++
		}
	}
	return nil
}

// mergeBackups is the plan as backup rows.
func mergeBackups(p *domain.StrayPlan) ([]userModels.MergeBackup, error) {
	var rows []userModels.MergeBackup
	for _, n := range p.Nodes {
		node, err := json.Marshal(n.Fields)
		if err != nil {
			return nil, err
		}
		rows = append(rows, userModels.MergeBackup{Kind: "node", UserUUID: n.UserUUID, PersonUID: n.Person, StrayUID: n.Stray, Node: node})
	}
	for _, part := range []struct {
		kind  string
		edges []domain.StrayEdge
	}{{"in_edge", p.In}, {"out_edge", p.Out}} {
		for _, e := range part.edges {
			var facets json.RawMessage
			if len(e.Facets) > 0 {
				var err error
				if facets, err = json.Marshal(e.Facets); err != nil {
					return nil, err
				}
			}
			rows = append(rows, userModels.MergeBackup{Kind: part.kind, UserUUID: e.UserUUID, PersonUID: e.Person, StrayUID: e.Stray,
				Predicate: e.Predicate, OtherUID: e.Other, List: e.List, Added: e.Added, Facets: facets})
		}
	}
	return rows, nil
}

// settleMergeBatches settles the batches a crash left pending. The graph
// transaction is all or nothing, so a batch with a stray gone landed and its
// backup is kept, and one whose strays are all there never did and its rows
// go.
func settleMergeBatches(ctx context.Context) error {
	pending, err := userModels.PendingMergeBatches(ctx)
	if err != nil || len(pending) == 0 {
		return err
	}
	var strays []string
	for _, b := range pending {
		strays = append(strays, b.Strays...)
	}
	left, err := domain.StraysLeft(ctx, strays)
	if err != nil {
		return err
	}
	var landed, never []uuid.UUID
	for _, b := range pending {
		if slices.ContainsFunc(b.Strays, func(s string) bool { return !left[s] }) {
			landed = append(landed, b.Batch)
		} else {
			never = append(never, b.Batch)
		}
	}
	return userModels.SettleMergeBatches(ctx, landed, never)
}

// RestoreStrayMerge puts a merge run back, a batch per graph transaction: each
// stray with every predicate it held, each edge into one pointed at it again,
// and each edge the merge gave the person taken away. It writes them as they
// were at the run, so the same edges written since are reverted too: a
// single-valued edge changed since points at the stray again, and an edge the
// merge gave the person goes even if it was given again since. A batch put
// back is marked restored, so running it again changes nothing.
func RestoreStrayMerge(ctx context.Context, run uuid.UUID) (back StrayRestore, err error) {
	batches, rows, err := userModels.AppliedMergeBackups(ctx, run)
	if err != nil {
		return back, err
	}
	for _, batch := range batches {
		var nodes []domain.StrayNode
		var in, out []domain.StrayEdge
		for _, r := range rows[batch] {
			if r.Kind == "node" {
				n := domain.StrayNode{UserUUID: r.UserUUID, Person: r.PersonUID, Stray: r.StrayUID}
				if err := json.Unmarshal(r.Node, &n.Fields); err != nil {
					return back, err
				}
				nodes = append(nodes, n)
				continue
			}
			e := domain.StrayEdge{UserUUID: r.UserUUID, Person: r.PersonUID, Stray: r.StrayUID,
				Predicate: r.Predicate, Other: r.OtherUID, List: r.List, Added: r.Added}
			if len(r.Facets) > 0 {
				if err := json.Unmarshal(r.Facets, &e.Facets); err != nil {
					return back, err
				}
			}
			if r.Kind == "in_edge" {
				in = append(in, e)
			} else {
				out = append(out, e)
			}
		}
		m, err := domain.StrayRestoreMutation(nodes, in, out)
		if err != nil {
			return back, err
		}
		if _, err := dgexec.Mutate(ctx, "", nil, m); err != nil {
			return back, err
		}
		if err := userModels.MarkMergeBatch(ctx, run, batch, "applied", "restored"); err != nil {
			return back, err
		}
		back.Strays += len(nodes)
		back.Edges += len(in)
	}
	if back.Strays > 0 {
		recordStrayRestore(ctx, run, back)
	}
	return back, nil
}

// recordStrayMerge tells the admins, in the audit log, what the merge did:
// counts alone.
func recordStrayMerge(ctx context.Context, r StrayMerge) {
	err := adminAuditBusiness.RecordForPrincipal(ctx, nil, "", adminAuditBusiness.ActorSystem,
		"people.stray_nodes_merged", adminAuditBusiness.CategorySecurity, r.String(),
		map[string]interface{}{"run": r.Run.String(), "people": r.People, "strays": r.Strays, "moved": r.Moved,
			"added": r.Added, "kept": r.Kept, "renamed": r.Renamed, "skipped": r.Skipped})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "user/stray merge: the audit record: %+v", err)
	}
}

// recordStrayRestore tells the admins what a restore put back.
func recordStrayRestore(ctx context.Context, run uuid.UUID, back StrayRestore) {
	err := adminAuditBusiness.RecordForPrincipal(ctx, nil, "", adminAuditBusiness.ActorSystem,
		"people.stray_nodes_restored", adminAuditBusiness.CategorySecurity,
		fmt.Sprintf("Put back %d stray graph nodes and %d edges into them, from merge run %s, as they were at the run (the same edges written since are reverted too)", back.Strays, back.Edges, run),
		map[string]interface{}{"run": run.String(), "strays": back.Strays, "edges": back.Edges})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "user/stray merge: the restore's audit record: %+v", err)
	}
}
