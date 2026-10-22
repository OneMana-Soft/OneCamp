//go:build integration
// +build integration

package integration_test

// The board's safety net (the whiteboard's W-0b) against real stores: two
// saves of a board, the second after an undo took every element back (W-0's
// bug: Sam's undo emptied Ann's board for everyone), leave a "Before large
// deletion" version in Postgres and MinIO that still holds Ann's rectangle
// and note, to restore. The size of the state barely moves on such an undo,
// so the size alone kept nothing; what a person sees drops from 4 to 0.
//
// Run: go test -tags=integration ./tests/integration/ -run TestABoardEmptiedByAnUndo -v

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"

	boardAdapter "github.com/akashc777/OneCamp/adapter/Board"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

// boardState is one of the two saves (business/Board/testdata): Ann's note, its
// words, her rectangle and Sam's, live; then all four taken back.
func boardState(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../business/Board/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestABoardEmptiedByAnUndoKeepsAVersionThatStillHoldsIt(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	startMinio(t)

	board := uuid.NewString()
	dg.Mutate(t, []map[string]any{{"uid": "_:b", "dgraph.type": "Board", "board_uuid": board, "board_title": "Launch retro", "board_private": false, "board_deleted_at": "0001-01-01T00:00:00Z"}})
	before, after := boardState(t, "undo-before.b64"), boardState(t, "undo-after.b64")

	save := func(state, snippet string) {
		t.Helper()
		if err := boardBusiness.UpdateBoardFromCollab(ctx, &boardAdapter.InputBoardCollabUpdate{BoardUuid: board, State: state, Snippet: snippet}); err != nil {
			t.Fatal(err)
		}
	}
	snapshots := func(reason string) []string {
		t.Helper()
		rows, err := env.PG.Query(`SELECT object_key FROM board_snapshots WHERE board_uuid = $1 AND reason = $2 ORDER BY created_at`, board, reason)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var keys []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, k)
		}
		return keys
	}
	wait := func(reason string) []string {
		t.Helper()
		// Snapshots are taken off the save's path (helpers.GoSafeNamed).
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if keys := snapshots(reason); len(keys) > 0 {
				return keys
			}
		}
		return nil
	}

	// The board as Ann left it, with the collaboration service's count of what a person sees.
	save(before, "4 elements")
	if wait("interval") == nil {
		t.Fatal("the board's first save kept no version at all")
	}
	// Sam's undo takes everything back: every element a tombstone, the state no smaller.
	save(after, "0 elements")
	keys := wait("mass_delete")
	if len(keys) != 1 {
		t.Fatalf("an undo that took every element back kept %d versions from before it, want 1", len(keys))
	}

	obj, err := minioInit.MinioClient.GetObject(ctx, helpers.UserUploadBucket(), keys[0], minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()
	zipped, err := io.ReadAll(obj)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		t.Fatal(err)
	}
	kept, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != before {
		t.Fatalf("the version kept isn't the board as Ann left it (%d bytes, want the %d of the save before the undo)", len(kept), len(before))
	}
	var count int
	if err := env.PG.QueryRow(`SELECT element_count FROM board_snapshots WHERE object_key = $1`, keys[0]).Scan(&count); err != nil || count != 4 {
		t.Fatalf("the version says it holds %d elements (%v), want 4", count, err)
	}
}
