//go:build integration
// +build integration

package integration

import (
	"context"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	snapshotModels "github.com/akashc777/OneCamp/models/postgres/DocSnapshot"
	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
)

// docVersionsMigration is the doc versions migration's number, found by its
// file's name (migrations are numbered in merge order: it was reserved as 206,
// which A3's took), and the number of the migration before it.
func docVersionsMigration(t *testing.T, dir string) (mine, before uint) {
	t.Helper()
	ups, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var all []uint
	for _, f := range ups {
		name := filepath.Base(f)
		n, err := strconv.ParseUint(strings.SplitN(name, "_", 2)[0], 10, 32)
		if err != nil {
			continue
		}
		all = append(all, uint(n))
		if strings.HasSuffix(name, "_doc_versions.up.sql") {
			mine = uint(n)
		}
	}
	if mine == 0 {
		t.Fatal("no *_doc_versions.up.sql among the migrations")
	}
	for _, n := range all {
		if n < mine && n > before {
			before = n
		}
	}
	return mine, before
}

// The doc versions migration goes up, down and up again on Postgres 12, and a
// version kept before it reads back after it.
func TestDocVersionsMigrationUpDownUp(t *testing.T) {
	env := SetupEnv(t)
	ctx := context.Background()
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(here), "..", "..", "migrations")
	mine, before := docVersionsMigration(t, dir)
	m, err := migrate.New("file://"+dir, env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	v, _, _ := m.Version()
	if v < mine {
		t.Fatalf("migrations stop at %d, before the doc versions one (%d)", v, mine)
	}
	if err := m.Migrate(before); err != nil {
		t.Fatalf("down past %d: %v", mine, err)
	}
	// A version as migration 88 kept it, and one for a new reason refused.
	old := uuid.New()
	if _, err := env.PG.ExecContext(ctx, `INSERT INTO doc_snapshots (id, doc_uuid, object_key, body_bytes, reason, contributor_uuids)
		VALUES ($1, 'doc-1', 'k', 10, 'interval', '{ana}')`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.ExecContext(ctx, `INSERT INTO doc_snapshots (id, doc_uuid, object_key, reason) VALUES ($1, 'doc-1', 'k2', 'idle')`, uuid.New()); err == nil {
		t.Fatalf("before %d an idle version was accepted", mine)
	}
	if err := m.Migrate(mine); err != nil {
		t.Fatalf("up to %d: %v", mine, err)
	}
	var kind, lineage, meta string
	var pinned bool
	if err := env.PG.QueryRowContext(ctx, `SELECT kind, lineage, contributor_meta::text, pinned FROM doc_snapshots WHERE id = $1`, old).Scan(&kind, &lineage, &meta, &pinned); err != nil {
		t.Fatal(err)
	}
	if kind != "doc" || lineage != "" || meta != "[]" || pinned {
		t.Errorf("an old version reads kind %q lineage %q meta %s pinned %v", kind, lineage, meta, pinned)
	}
	if _, err := env.PG.ExecContext(ctx, `INSERT INTO doc_snapshots (id, doc_uuid, object_key, reason, kind, run_id, name, pinned)
		VALUES ($1, 'doc-1', 'k3', 'before_write', 'agent_run', 'run-1', 'Before the agent', true)`, uuid.New()); err != nil {
		t.Fatalf("a version of the new kind: %v", err)
	}
	if _, err := env.PG.ExecContext(ctx, `INSERT INTO doc_snapshots (id, doc_uuid, object_key, reason, kind) VALUES ($1, 'doc-1', 'k4', 'interval', 'nonsense')`, uuid.New()); err == nil {
		t.Error("an unknown kind was accepted")
	}
	// Down keeps the rows under the old reasons; up again.
	if err := m.Migrate(before); err != nil {
		t.Fatalf("down again: %v", err)
	}
	var reason string
	if err := env.PG.QueryRowContext(ctx, `SELECT reason FROM doc_snapshots WHERE object_key = 'k3'`).Scan(&reason); err != nil || reason != "manual" {
		t.Errorf("a before_write version after down: %q %v", reason, err)
	}
	if err := m.Migrate(mine); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// The model keeps, names and prunes versions as the Snapshot Service asks: ids
// made in Go, named and pinned versions never deleted, the kept bytes summed.
func TestDocVersionRowsKeepNamedOnes(t *testing.T) {
	env := SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	doc := uuid.NewString()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var ids []uuid.UUID
	for i := 0; i < 5; i++ {
		row := &snapshotModels.DocSnapshot{
			DocUUID: doc, ObjectKey: "k" + uuid.NewString(), BodyBytes: 100, StoredBytes: 40, Reason: snapshotModels.ReasonInterval,
			Contributors:     []snapshotModels.Contributor{{UserUUID: "ana"}, {AgentUUID: "captain", AskerUUID: "sam"}},
			ContributorUUIDs: []string{"ana", "sam"}, Lineage: "L", CreatedAt: base.Add(time.Duration(i) * time.Hour),
		}
		id, err := snapshotModels.CreateSnapshot(ctx, row)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	name := "Launch draft"
	if ok, err := snapshotModels.SetSnapshotName(ctx, ids[0], doc, &name, true); err != nil || !ok {
		t.Fatalf("name: %v %v", ok, err)
	}
	got, err := snapshotModels.GetSnapshotByID(ctx, ids[0], doc)
	if err != nil || got.Name == nil || *got.Name != name || !got.Pinned || len(got.Contributors) != 2 || got.Contributors[1].AskerUUID != "sam" || got.Lineage != "L" {
		t.Fatalf("read back %+v %v", got, err)
	}
	if n, err := snapshotModels.KeptBytes(ctx, doc); err != nil || n != 40 {
		t.Errorf("kept bytes %d %v", n, err)
	}
	keys, err := snapshotModels.DeleteUnkeptByIDs(ctx, ids)
	if err != nil || len(keys) != 4 {
		t.Fatalf("deleted %d %v", len(keys), err)
	}
	left, _ := snapshotModels.ListSnapshots(ctx, doc, time.Time{}, 10)
	if len(left) != 1 || left[0].ID != ids[0] {
		t.Errorf("left %d", len(left))
	}
	docs, err := snapshotModels.DocsWithVersionsBefore(ctx, time.Now(), "", 10)
	if err != nil || len(docs) != 0 {
		t.Errorf("a doc whose only version is kept is listed for pruning: %v %v", docs, err)
	}
	if rows, err := snapshotModels.ListForRetention(ctx, doc); err != nil || len(rows) != 1 || !rows[0].Kept {
		t.Errorf("retention rows %+v %v", rows, err)
	}
}
