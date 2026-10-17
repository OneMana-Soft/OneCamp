//go:build integration
// +build integration

package integration_test

// Edit proposals on a real Postgres 12 (migration 205, business/Proposal):
// the migration goes up, down and up again; proposals are stored and read back
// whole; two reviewers accepting at once apply one proposal once; a proposal a
// stopped API left preparing is swept.
//
// Run: go test -tags=integration ./tests/integration/ -run TestEditProposals -v

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	proposalBusiness "github.com/akashc777/OneCamp/business/Proposal"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	proposalModels "github.com/akashc777/OneCamp/models/postgres/Proposal"
	"github.com/akashc777/OneCamp/tests/integration"
)

// The migration's own files, found by name so a renumbering at merge keeps the
// test.
func proposalMigration(t *testing.T, dir string) (string, string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_edit_proposals."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one edit_proposals %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return matches[0], string(b)
}

func TestEditProposalsMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	upName, up := proposalMigration(t, "up")
	_, down := proposalMigration(t, "down")
	// Ids are made in Go: nothing in the migration's SQL makes one.
	var sqlOnly []string
	for _, line := range strings.Split(up, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			sqlOnly = append(sqlOnly, line)
		}
	}
	for _, banned := range []string{"gen_random_uuid", "uuid_generate"} {
		if strings.Contains(strings.Join(sqlOnly, "\n"), banned) {
			t.Fatalf("%s uses %s; Postgres 12 has neither by default, make ids in Go", upName, banned)
		}
	}
	exists := func() bool {
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'edit_proposals'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if !exists() {
		t.Fatal("the migrations did not make edit_proposals")
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{down, false}, {up, true}, {down, false}, {up, true}} {
		if _, err := env.PG.Exec(step.sql); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if exists() != step.want {
			t.Fatalf("step %d: table exists = %v", i, !step.want)
		}
	}
	// The checks hold: an unknown status is refused.
	_, err := env.PG.Exec(`INSERT INTO edit_proposals (id, resource_kind, resource_uuid, proposed, author_kind, author_uuid, status)
		VALUES ($1, 'doc', $2, '[]', 'person', $3, 'maybe')`, uuid.New(), uuid.New(), uuid.New())
	if err == nil {
		t.Fatal("a status outside the list was stored")
	}
}

// openKind lets everyone edit and runs steps through `run`.
type openKind struct {
	run func(p proposalBusiness.Proposal) error
}

func (openKind) Access(context.Context, accessBusiness.Subject, string) (accessBusiness.Decision, error) {
	return accessBusiness.Decision{Level: accessBusiness.LevelFull}, nil
}

func (k openKind) Run(_ context.Context, _ accessBusiness.Subject, p proposalBusiness.Proposal) error {
	if k.run != nil {
		return k.run(p)
	}
	return nil
}

func TestEditProposalsOnPostgres(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	var applies atomic.Int32
	kind := "it-" + uuid.NewString()[:8]
	proposalBusiness.Kinds.Register(kind, openKind{run: func(p proposalBusiness.Proposal) error {
		applies.Add(1)
		time.Sleep(150 * time.Millisecond) // a step takes a while: the second reviewer arrives meanwhile
		return nil
	}})
	who := accessBusiness.Subject{UserUID: "0x1", UserUUID: uuid.NewString()}
	doc := uuid.NewString()
	in := proposalBusiness.Proposal{
		ResourceKind: kind, ResourceUUID: doc, BaseHash: "abc", Note: "Friday, not Thursday",
		Anchor: threadBusiness.Anchor{Kind: threadBusiness.AnchorYRange, Start: []byte{1, 2, 3}, End: []byte{4, 5}, Lineage: "L1", Quote: "We ship on Thursday."},
		Base:   json.RawMessage(`[{"type":"paragraph"}]`), Proposed: json.RawMessage(`[{"type":"paragraph"}]`),
		Hunks: []proposalBusiness.Hunk{{ID: "h1", Op: "replace", Level: "inline", Path: []int{0}, From: 11, To: 19,
			Before: json.RawMessage(`[{"type":"text","text":"Thursday"}]`), After: json.RawMessage(`[{"type":"text","text":"Friday"}]`), Start: "AQ==", End: "Ag=="}},
	}
	p, err := proposalBusiness.Proposals.Create(ctx, who, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := proposalBusiness.Proposals.Get(ctx, who, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Anchor.Start) != string([]byte{1, 2, 3}) || got.Anchor.Lineage != "L1" || got.Hunks[0].Start != "AQ==" ||
		got.Note != "Friday, not Thursday" || got.AuthorUUID != who.UserUUID || got.Status != proposalBusiness.StatusOpen {
		t.Fatalf("read back: %+v", got)
	}

	// Two reviewers accept at once: one applies it, the other is told.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = proposalBusiness.Proposals.Decide(ctx, who, p.ID, proposalBusiness.Decision{Accept: true})
		}(i)
	}
	wg.Wait()
	if applies.Load() != 1 {
		t.Fatalf("applied %d times", applies.Load())
	}
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, proposalBusiness.ErrBusy), errors.Is(err, proposalBusiness.ErrDecided):
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d reviewers succeeded", ok)
	}
	list, err := proposalBusiness.Proposals.List(ctx, who, accessBusiness.Resource{Kind: kind, UUID: doc})
	if err != nil || len(list) != 1 || list[0].Status != proposalBusiness.StatusAccepted || list[0].Hunks[0].DecidedBy != who.UserUUID {
		t.Fatalf("%v %+v", err, list)
	}

	// A proposal left preparing by a stopped API is swept; a fresh one is not.
	store := proposalModels.Store{}
	old := time.Now().Add(-time.Hour)
	for _, at := range []time.Time{old, time.Now()} {
		if err := store.Insert(ctx, &proposalModels.Row{ID: uuid.New(), ResourceKind: kind, ResourceUUID: uuid.MustParse(doc), AnchorKind: "none",
			Proposed: json.RawMessage(`[]`), Hunks: json.RawMessage(`[]`), AuthorKind: "agent", AuthorUUID: uuid.New(), Status: "preparing", CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := store.Sweep(ctx, 10*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("swept %d: %v", n, err)
	}
}
