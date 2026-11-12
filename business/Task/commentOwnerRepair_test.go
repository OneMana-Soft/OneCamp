package business

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// ownersGraph is the graph as the comment-owner repair reads and writes it:
// task comments by uid, each with its writer, owner and task; and the
// events in order (each page read, the dry run's log line, each write).
type ownersGraph struct {
	comments map[string]*dgraphStruct.DgraphComment
	events   []string
	writes   int
	failOn   int // the write that fails (1-based), none when 0
}

func ownerUser(uid, id string) *dgraphStruct.DgraphUser {
	return &dgraphStruct.DgraphUser{Uid: uid, Uuid: id}
}

func (g *ownersGraph) add(uid, by, owner string, task *dgraphStruct.DgraphTask) {
	g.comments[uid] = &dgraphStruct.DgraphComment{Uid: uid, CommentBy: ownerUser(by, ""), ContentAddedBy: ownerUser(owner, ""), Task: task}
}

func (g *ownersGraph) page(_ context.Context, after string, first int) ([]*dgraphStruct.DgraphComment, error) {
	g.events = append(g.events, "read after "+after)
	var uids []string
	for uid := range g.comments {
		if after == "" || uid > after {
			uids = append(uids, uid)
		}
	}
	sort.Strings(uids)
	var out []*dgraphStruct.DgraphComment
	for _, uid := range uids {
		if len(out) == first {
			break
		}
		c := *g.comments[uid]
		out = append(out, &c)
	}
	return out, nil
}

func (g *ownersGraph) write(_ context.Context, owners map[string]string) error {
	g.writes++
	if g.writes == g.failOn {
		return errors.New("the power went off")
	}
	g.events = append(g.events, "write")
	for c, o := range owners {
		g.comments[c].ContentAddedBy = ownerUser(o, "")
	}
	return nil
}

// useOwnersGraph points the repair's seams at g, with progress kept in
// memory under *saved, pages of 2.
func useOwnersGraph(t *testing.T, g *ownersGraph, saved *commentOwnerProgress) {
	t.Helper()
	page0, write0, load0, save0, log0, size0 := commentOwnersPage, reownComments, loadCommentOwners, saveCommentOwners, commentOwnerLog, commentOwnerPage
	t.Cleanup(func() {
		commentOwnersPage, reownComments, loadCommentOwners, saveCommentOwners, commentOwnerLog, commentOwnerPage = page0, write0, load0, save0, log0, size0
	})
	commentOwnersPage, reownComments, commentOwnerPage = g.page, g.write, 2
	loadCommentOwners = func(string) (commentOwnerProgress, error) { return *saved, nil }
	saveCommentOwners = func(p commentOwnerProgress) error { *saved = p; return nil }
	commentOwnerLog = func(format string, args ...any) {
		if strings.Contains(format, "DRY RUN") {
			g.events = append(g.events, "dry run")
		}
	}
}

// ownersWorld: Olu is given task 1, task 2 is given to nobody and was made
// by Maya, task 3 is given to Ben. Ben's two comments on task 1 and Kay's on
// task 2 are owned by their writers (before 6 Nov); Ben's on task 3 is his
// own task's; Tom's on task 1 is owned by Olu already (after 6 Nov); Kay's
// last names no task the system can read.
func ownersWorld() *ownersGraph {
	g := &ownersGraph{comments: map[string]*dgraphStruct.DgraphComment{}}
	t1 := &dgraphStruct.DgraphTask{Assignee: ownerUser("0xa1", "olu"), CreatedBy: ownerUser("0xa2", "maya")}
	t2 := &dgraphStruct.DgraphTask{CreatedBy: ownerUser("0xa2", "maya")}
	t3 := &dgraphStruct.DgraphTask{Assignee: ownerUser("0xb1", "ben"), CreatedBy: ownerUser("0xa2", "maya")}
	g.add("0x101", "0xb1", "0xb1", t1)
	g.add("0x102", "0xb1", "0xb1", t1)
	g.add("0x103", "0xc1", "0xc1", t2)
	g.add("0x104", "0xb1", "0xb1", t3)
	g.add("0x105", "0xd1", "0xa1", t1)
	g.add("0x106", "0xc1", "0xc1", nil)
	return g
}

func ownedBy(g *ownersGraph, uid string) string { return g.comments[uid].ContentAddedBy.Uid }

// Each comment owned by its writer is re-owned to its task's Addressee, a
// page at a time in one write per page, after a dry run counted them and
// said so; the rest are left as they are.
func TestTheCommentOwnerRepairReownsToWhomACommentIsAddressed(t *testing.T) {
	g, saved := ownersWorld(), commentOwnerProgress{}
	useOwnersGraph(t, g, &saved)
	rep, err := repairCommentOwners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for uid, want := range map[string]string{"0x101": "0xa1", "0x102": "0xa1", "0x103": "0xa2", "0x104": "0xb1", "0x105": "0xa1", "0x106": "0xc1"} {
		if got := ownedBy(g, uid); got != want {
			t.Errorf("comment %s is owned by %s; want %s", uid, got, want)
		}
	}
	if rep.DryRun != 3 || rep.Reowned != 3 || rep.Read != 6 {
		t.Errorf("report %+v; want a dry run of 3, 6 read, 3 re-owned", rep)
	}
	// Pages of 2: {101,102} and {103,104} change, {105,106} doesn't. Two
	// writes for three comments: batched, never a write per comment.
	if rep.Writes != 2 || g.writes != 2 {
		t.Errorf("%d writes (report %d); want 2, one per page that changes", g.writes, rep.Writes)
	}
	first := -1
	for i, e := range g.events {
		if e == "write" {
			first = i
			break
		}
	}
	dry := strings.Join(g.events, "; ")
	if i := ownersIndex(g.events, "dry run"); i < 0 || first < 0 || i > first {
		t.Errorf("events %s; want the dry run's count logged before any write", dry)
	}
}

// A second pass over what the first left changes nothing: it counts none,
// and writes nothing.
func TestASecondCommentOwnerPassChangesNothing(t *testing.T) {
	g, saved := ownersWorld(), commentOwnerProgress{}
	useOwnersGraph(t, g, &saved)
	if _, err := repairCommentOwners(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.writes, saved = 0, commentOwnerProgress{}
	rep, err := repairCommentOwners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if g.writes != 0 || rep.DryRun != 0 || rep.Reowned != 0 || rep.Writes != 0 {
		t.Errorf("second pass: %d writes, report %+v; want nothing written or counted", g.writes, rep)
	}
}

// A run cut off by a power-off goes on from the last page it kept: the dry
// run isn't counted again, the pages before aren't read again, and the end
// is the same as an uncut run's.
func TestTheCommentOwnerRepairGoesOnAfterAPowerOff(t *testing.T) {
	g, saved := ownersWorld(), commentOwnerProgress{}
	g.failOn = 2
	useOwnersGraph(t, g, &saved)
	if _, err := repairCommentOwners(context.Background()); err == nil {
		t.Fatal("the cut-off run answered no error")
	}
	if !saved.Counted || saved.After != "0x102" || saved.Report.Reowned != 2 {
		t.Fatalf("kept %+v; want counted, after 0x102, 2 re-owned", saved)
	}
	g.events, g.failOn = nil, 0
	rep, err := repairCommentOwners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ownersIndex(g.events, "dry run") >= 0 || g.events[0] != "read after 0x102" {
		t.Errorf("events after the restart %v; want no second dry run and the first read after 0x102", g.events)
	}
	if ownedBy(g, "0x103") != "0xa2" || rep.Reowned != 3 || rep.DryRun != 3 {
		t.Errorf("after the restart: 0x103 owned by %s, report %+v; want 0xa2 and 3 re-owned of a dry run of 3", ownedBy(g, "0x103"), rep)
	}
}

func ownersIndex(events []string, e string) int {
	for i, x := range events {
		if x == e {
			return i
		}
	}
	return -1
}
