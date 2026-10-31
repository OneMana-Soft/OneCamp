//go:build integration
// +build integration

package integration_test

// A doc tree file's versions in Postgres 12 (docs F1,
// migrations/*_doc_file_versions.up.sql, models/postgres/DocFile): the
// migration goes up, down and up again; versions are numbered in turn however
// many arrive at once, each with its own attachments row at the file's place;
// a level's files read their newest version in one statement; a copy takes
// the newest version on the same stored object; and a purge takes a file's
// versions and archives their rows, so the attachments purge takes the bytes.
//
// Run: integ_rel.sh <wt> <log> '^(TestDocFileVersionsMigrationUpDownUp|TestDocFileVersionsFollowTheirFiles|TestAFilesVersionsAreNumberedInTurn)$'

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	docFileModels "github.com/akashc777/OneCamp/models/postgres/DocFile"
	"github.com/akashc777/OneCamp/tests/integration"
)

// docFileVersionsMigration is the migration's number, and the one before it,
// read from the files: the integrator may renumber it at landing.
func docFileVersionsMigration(t *testing.T) (dir string, mine, before uint) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	dir = filepath.Join(filepath.Dir(here), "..", "..", "migrations")
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
		if strings.HasSuffix(name, "_doc_file_versions.up.sql") {
			mine = uint(n)
		}
	}
	if mine == 0 {
		t.Fatal("no *_doc_file_versions.up.sql among the migrations")
	}
	for _, n := range all {
		if n < mine && n > before {
			before = n
		}
	}
	return dir, mine, before
}

func TestDocFileVersionsMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	dir, mine, before := docFileVersionsMigration(t)
	m, err := migrate.New("file://"+dir, env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if v, _, _ := m.Version(); v < mine {
		t.Fatalf("migrations stop at %d, before the file versions one (%d)", v, mine)
	}
	if err := m.Migrate(before); err != nil {
		t.Fatalf("down past %d: %v", mine, err)
	}
	var exists bool
	if err := env.PG.QueryRowContext(ctx, `SELECT to_regclass('doc_file_versions') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("down, the table is still there: %v %v", exists, err)
	}
	if err := m.Migrate(mine); err != nil {
		t.Fatalf("up to %d: %v", mine, err)
	}
	doc, by := uuid.New(), uuid.New()
	insert := func(n int, att uuid.UUID, name, note string) error {
		var notes any
		if note != "" {
			notes = note
		}
		_, err := env.PG.ExecContext(ctx, `INSERT INTO doc_file_versions (doc_uuid, n, attachment_id, name, size_bytes, mime, note, created_by)
			VALUES ($1, $2, $3, $4, 10, 'application/pdf', $5, $6)`, doc, n, att, name, notes, by)
		return err
	}
	if err := insert(1, uuid.New(), "msa.pdf", "First draft"); err != nil {
		t.Fatalf("a version: %v", err)
	}
	for what, err := range map[string]error{
		"the same n twice":      insert(1, uuid.New(), "msa.pdf", ""),
		"one attachment twice":  func() error { a := uuid.New(); _ = insert(2, a, "a", ""); return insert(3, a, "b", "") }(),
		"a version 0":           insert(0, uuid.New(), "msa.pdf", ""),
		"no name":               insert(4, uuid.New(), "", ""),
		"a note past 500 chars": insert(5, uuid.New(), "msa.pdf", strings.Repeat("x", 501)),
	} {
		if err == nil {
			t.Errorf("%s was accepted", what)
		}
	}
}

// seedFileVersion writes a file's version n as a recorded one is written:
// its attachments row at the file's place, and its version row.
func seedFileVersion(t *testing.T, db *sql.DB, doc uuid.UUID, n int, name string, by uuid.UUID) uuid.UUID {
	t.Helper()
	att := uuid.New()
	if _, err := db.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, 'doc', $3, $4)`,
		att, by.String()+"/"+att.String()+"_"+name, doc.String(), by); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO doc_file_versions (doc_uuid, n, attachment_id, name, size_bytes, mime, created_by)
		VALUES ($1, $2, $3, $4, 1200, 'application/pdf', $5)`, doc, n, att, name, by); err != nil {
		t.Fatal(err)
	}
	return att
}

func TestDocFileVersionsFollowTheirFiles(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	by := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'ana', 'Ana', NOW(), NOW())`, by, by.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	a, b, page := uuid.New(), uuid.New(), uuid.New()
	seedFileVersion(t, env.PG, a, 1, "msa.pdf", by)
	seedFileVersion(t, env.PG, a, 2, "msa-v2.pdf", by)
	for n := 1; n <= 6; n++ {
		seedFileVersion(t, env.PG, b, n, "plan.pdf", by)
	}

	current, err := docFileModels.CurrentOf(ctx, []uuid.UUID{a, b, page})
	if err != nil {
		t.Fatal(err)
	}
	if c := current[a]; c.N != 2 || c.Versions != 2 || c.Name != "msa-v2.pdf" || c.SizeBytes != 1200 || c.MIME != "application/pdf" || c.CreatedBy != by {
		t.Errorf("the first file's newest: %+v", c)
	}
	if c := current[b]; c.N != 6 || c.Versions != 6 {
		t.Errorf("the second file's newest: %+v", c)
	}
	if _, ok := current[page]; ok || len(current) != 2 {
		t.Errorf("a doc with no versions read as a file: %v", current)
	}

	// A copy takes the newest version on the same stored object.
	copyOf, copyAtt := uuid.New(), uuid.New()
	if n, err := docFileModels.CopyCurrent(ctx, []docFileModels.Copy{{From: a, To: copyOf, AttachmentID: copyAtt}}, by); err != nil || n != 1 {
		t.Fatalf("a copy: %d %v", n, err)
	}
	var key, srcValue, sourceKey string
	if err := env.PG.QueryRow(`SELECT obj_key, src_value FROM attachments WHERE id = $1`, copyAtt).Scan(&key, &srcValue); err != nil {
		t.Fatal(err)
	}
	if err := env.PG.QueryRow(`SELECT a.obj_key FROM attachments a JOIN doc_file_versions v ON v.attachment_id = a.id WHERE v.doc_uuid = $1 AND v.n = 2`, a).Scan(&sourceKey); err != nil {
		t.Fatal(err)
	}
	if key != sourceKey || srcValue != copyOf.String() {
		t.Errorf("the copy's row names %q at %q; want %q at the copy", key, srcValue, sourceKey)
	}
	if c, err := docFileModels.CurrentOf(ctx, []uuid.UUID{copyOf}); err != nil || c[copyOf].N != 1 || c[copyOf].Name != "msa-v2.pdf" {
		t.Errorf("the copy's version: %+v %v", c, err)
	}

	// A purge takes the first file's versions and archives their rows; the
	// copy's row, on the same object, stays live.
	at := time.Now()
	if n, err := docFileModels.ForgetFiles(ctx, []uuid.UUID{a, page}, at); err != nil || n != 2 {
		t.Fatalf("a purge: %d %v", n, err)
	}
	var archived, live, kept int
	if err := env.PG.QueryRow(`SELECT count(*) FILTER (WHERE deleted_at IS NOT NULL), count(*) FILTER (WHERE deleted_at IS NULL) FROM attachments WHERE obj_key LIKE $1 AND src_value IN ($2, $3)`,
		by.String()+"/%", a.String(), copyOf.String()).Scan(&archived, &live); err != nil {
		t.Fatal(err)
	}
	if archived != 2 || live != 1 {
		t.Errorf("after the purge, %d rows archived and %d live; want the file's 2 archived and the copy's 1 live", archived, live)
	}
	if err := env.PG.QueryRow(`SELECT count(*) FROM doc_file_versions WHERE doc_uuid = $1`, a).Scan(&kept); err != nil || kept != 0 {
		t.Errorf("the purged file still has %d versions (%v)", kept, err)
	}

	// A file whose node was never made leaves nothing.
	if n, err := docFileModels.DropUnseen(ctx, []uuid.UUID{copyOf}); err != nil || n != 1 {
		t.Fatalf("dropping the unseen copy: %d %v", n, err)
	}
	var rows int
	if err := env.PG.QueryRow(`SELECT (SELECT count(*) FROM attachments WHERE id = $1) + (SELECT count(*) FROM doc_file_versions WHERE doc_uuid = $2)`, copyAtt, copyOf).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("an unseen file left %d rows (%v)", rows, err)
	}
}

// A file's versions are numbered in turn however many arrive at once, each
// with its own attachments row at the file's place, and listed newest first
// with their notes.
func TestAFilesVersionsAreNumberedInTurn(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	by := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'ana', 'Ana', NOW(), NOW())`, by, by.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	file := uuid.New()
	record := func(name, note string) (docFileModels.Version, error) {
		att := uuid.New()
		return docFileModels.RecordVersion(ctx, docFileModels.NewVersion{DocUUID: file, AttachmentID: att,
			ObjKey: by.String() + "/" + att.String() + "_" + name, Name: name, SizeBytes: 1200, MIME: "application/pdf", Note: note, CreatedBy: by})
	}
	first, err := record("msa.pdf", "")
	if err != nil || first.N != 1 || first.CreatedAt.IsZero() {
		t.Fatalf("a file's first version: %+v %v", first, err)
	}
	var wg sync.WaitGroup
	ns := make([]int, 6)
	errs := make([]error, 6)
	for i := range ns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := record("msa.pdf", "")
			ns[i], errs[i] = v.N, err
		}(i)
	}
	wg.Wait()
	sort.Ints(ns)
	for i, n := range ns {
		if errs[i] != nil || n != i+2 {
			t.Fatalf("six at once numbered %v (%v)", ns, errs)
		}
	}
	if v, err := record("msa-signed.pdf", "Signed by both"); err != nil || v.N != 8 || v.Note != "Signed by both" {
		t.Fatalf("an eighth with a note: %+v %v", v, err)
	}
	list, err := docFileModels.Versions(ctx, file)
	if err != nil || len(list) != 8 || list[0].N != 8 || list[0].Note != "Signed by both" || list[7].N != 1 || list[7].Note != "" {
		t.Errorf("the versions, newest first: %d %v", len(list), err)
	}
	var placed int
	if err := env.PG.QueryRow(`SELECT count(*) FROM attachments WHERE src_key = 'doc' AND src_value = $1 AND deleted_at IS NULL`, file.String()).Scan(&placed); err != nil || placed != 8 {
		t.Errorf("the file's versions are %d live attachments at its place (%v)", placed, err)
	}
}
