//go:build integration

package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/tests/integration"
)

// A board back from the trash (UX-TB-H2-BE, TABLES-BOARDS-13), as the graph
// writes it: one conditional upsert that writes only while the board is in
// the graph and in the trash. A board a retention rule removed is gone from
// the graph (PurgeBoardNodes), and an upsert keyed by its uuid alone would
// write it back as a new, bare, live node: a board nobody made, in everyone's
// list. So the restore never writes a board the graph doesn't hold in the
// trash; and a board restored before the rule's delete runs stays, since the
// delete asks again which are still in the trash.
//
// Run: go test -tags=integration ./domain/Board/ -run '^TestARestoreNeverWritesBackABoardARuleRemoved$' -v
func TestARestoreNeverWritesBackABoardARuleRemoved(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	now := time.Now().UTC()
	live := "0001-01-01T00:00:00Z"
	ids := map[string]string{"purged": uuid.NewString(), "trashed": uuid.NewString(), "live": uuid.NewString(), "restoredFirst": uuid.NewString()}
	board := func(key, title, deleted string) map[string]any {
		return map[string]any{"uid": "_:" + key, "dgraph.type": "Board", "board_uuid": ids[key], "board_title": title, "board_private": false,
			"board_created_at": now.AddDate(0, 0, -90).Format(time.RFC3339), "board_updated_at": now.AddDate(0, 0, -60).Format(time.RFC3339),
			"board_deleted_at": deleted, "board_created_by": map[string]any{"uid": "_:sam"}}
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "sam"},
		board("purged", "Old map", now.AddDate(0, 0, -40).Format(time.RFC3339)),
		board("trashed", "Retro", now.Add(-time.Hour).Format(time.RFC3339)),
		board("live", "Roadmap", live),
		board("restoredFirst", "Kickoff", now.AddDate(0, 0, -40).Format(time.RFC3339)),
	})
	nodes := func(id string) (n int, deleted string) {
		t.Helper()
		var got struct {
			B []struct {
				Deleted string `json:"board_deleted_at"`
			} `json:"b"`
		}
		if err := json.Unmarshal(dg.Query(t, `{ b(func: eq(board_uuid, "`+id+`")) { board_deleted_at } }`), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.B) > 0 {
			deleted = got.B[0].Deleted
		}
		return len(got.B), deleted
	}
	restore := func(key string) bool {
		t.Helper()
		ok, err := RestoreTrashedBoard(ctx, ids[key])
		if err != nil {
			t.Fatalf("restoring %s: %v", key, err)
		}
		return ok
	}

	// A 30-day rule removes the board in the trash for 40 days.
	cutoff := now.AddDate(0, 0, -30)
	if err := PurgeBoardNodes(ctx, []string{uids["purged"]}, cutoff, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := nodes(ids["purged"]); n != 0 {
		t.Fatalf("the rule left %d nodes of the board it removed", n)
	}
	if restore("purged") {
		t.Error("a board a retention rule removed was restored")
	}
	if n, _ := nodes(ids["purged"]); n != 0 {
		t.Errorf("restoring a board a rule removed wrote %d nodes back: a board nobody made, live in every list", n)
	}

	// A uuid the graph never held is written nowhere either.
	never := uuid.NewString()
	ids["never"] = never
	if restore("never") {
		t.Error("a board that was never there was restored")
	}
	if n, _ := nodes(never); n != 0 {
		t.Errorf("restoring a board that was never there wrote %d nodes", n)
	}

	// A live board isn't in the trash: nothing to restore, nothing written.
	if restore("live") {
		t.Error("a live board was restored")
	}
	if n, deleted := nodes(ids["live"]); n != 1 || !isZeroTime(deleted) {
		t.Errorf("the live board after a restore: %d nodes, deleted at %q", n, deleted)
	}

	// A board in the trash comes back live, once.
	if !restore("trashed") {
		t.Fatal("a board in the trash wasn't restored")
	}
	if n, deleted := nodes(ids["trashed"]); n != 1 || !isZeroTime(deleted) {
		t.Errorf("the restored board: %d nodes, deleted at %q, want one live node", n, deleted)
	}
	if restore("trashed") {
		t.Error("a board restored already was restored again")
	}

	// Restored before the rule's delete runs, a board stays: the delete asks again which are still in the trash.
	if !restore("restoredFirst") {
		t.Fatal("a board in the trash for 40 days wasn't restored before the rule ran")
	}
	if err := PurgeBoardNodes(ctx, []string{uids["restoredFirst"]}, cutoff, nil); err != nil {
		t.Fatal(err)
	}
	if n, deleted := nodes(ids["restoredFirst"]); n != 1 || !isZeroTime(deleted) {
		t.Errorf("a board restored before the rule ran: %d nodes, deleted at %q, want it kept live", n, deleted)
	}
}

// A restore of a board that is live writes nothing and says it brought
// nothing back (CR-247 F3, mutant I3). The business layer answers 409 for a
// live board before it gets here, so this is the check that holds when two
// restores race: the second finds the board live and must not write it again.
// One live board carries a live deletion time, the other none at all; an
// upsert without the trash filter would answer both restored and write the
// second a deletion time it never had.
//
// Run: go test -tags=integration ./domain/Board/ -run '^TestARestoreOfALiveBoardWritesNothing$' -v
func TestARestoreOfALiveBoardWritesNothing(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	now := time.Now().UTC().Format(time.RFC3339)
	zero, bare := uuid.NewString(), uuid.NewString()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "sam"},
		{"uid": "_:zero", "dgraph.type": "Board", "board_uuid": zero, "board_title": "Roadmap", "board_private": false,
			"board_created_at": now, "board_updated_at": now, "board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:sam"}},
		{"uid": "_:bare", "dgraph.type": "Board", "board_uuid": bare, "board_title": "Plan", "board_private": false,
			"board_created_at": now, "board_updated_at": now, "board_created_by": map[string]any{"uid": "_:sam"}},
	})
	for _, c := range []struct {
		name, id string
		want     string // the board's deletion time after the restore ("": none)
	}{
		{"a live board", zero, "0001-01-01T00:00:00Z"},
		{"a live board with no deletion time", bare, ""},
	} {
		ok, err := RestoreTrashedBoard(ctx, c.id)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if ok {
			t.Errorf("%s was restored: a restore that raced another one would answer that it brought it back", c.name)
		}
		var got struct {
			B []struct {
				Deleted string `json:"board_deleted_at"`
			} `json:"b"`
		}
		// board_uuid too: a node holding none of the asked predicates isn't answered at all.
		if err := json.Unmarshal(dg.Query(t, `{ b(func: eq(board_uuid, "`+c.id+`")) { board_uuid board_deleted_at } }`), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.B) != 1 {
			t.Fatalf("%s: %d nodes after the restore, want 1", c.name, len(got.B))
		}
		at := got.B[0].Deleted
		if p, err := time.Parse(time.RFC3339, at); err == nil {
			at = p.UTC().Format(time.RFC3339)
		}
		if at != c.want {
			t.Errorf("%s: deletion time %q after the restore, want %q (nothing written)", c.name, at, c.want)
		}
	}
}

// A board an import made and took away again (scratch: it was never the
// importer's to keep) is deleted like any board but never in anyone's trash:
// only boards a person deleted are listed (CR-247 F2, V-865).
//
// Run: go test -tags=integration ./domain/Board/ -run '^TestTheTrashLeavesOutScratchBoards$' -v
func TestTheTrashLeavesOutScratchBoards(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	now := time.Now().UTC()
	gone := now.Add(-time.Hour).Format(time.RFC3339)
	kept, scratch := uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "sam"},
		{"uid": "_:kept", "dgraph.type": "Board", "board_uuid": kept, "board_title": "Retro", "board_private": false,
			"board_created_at": now.Format(time.RFC3339), "board_deleted_at": gone, "board_created_by": map[string]any{"uid": "_:sam"}},
		{"uid": "_:scratch", "dgraph.type": "Board", "board_uuid": scratch, "board_title": "Kickoff", "board_private": false, "board_scratch": true,
			"board_created_at": now.Format(time.RFC3339), "board_deleted_at": gone, "board_created_by": map[string]any{"uid": "_:sam"}},
	})
	for _, others := range []bool{false, true} {
		boards, err := GetBoardTrash(ctx, uids["sam"], others, 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		var listed []string
		for _, b := range boards {
			listed = append(listed, b.Uuid)
		}
		if fmt.Sprint(listed) != fmt.Sprint([]string{kept}) {
			t.Errorf("the trash (an admin's: %v) lists %v, want only the board sam deleted (%s), never the scratch one (%s)", others, listed, kept, scratch)
		}
	}
	if ok, err := RestoreTrashedBoard(ctx, scratch); err != nil || ok {
		t.Errorf("a scratch board was restored (%v, %v): it was never anyone's to bring back", ok, err)
	}
}

// isZeroTime is whether a deletion time the graph answers is a live board's.
func isZeroTime(s string) bool {
	at, err := time.Parse(time.RFC3339, s)
	return err == nil && !at.After(time.Unix(0, 0))
}
