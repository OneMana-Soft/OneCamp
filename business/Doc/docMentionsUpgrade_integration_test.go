//go:build integration

package businness

// The first save after an upgrade, against a real Postgres with every
// migration. resource_mentions (migration 210) starts empty, so a doc made
// before it that already mentions three people must tell none of them on its
// first save; a fourth person, added after, is told alone. A doc made since is
// told from its first save, and a save holding nobody's edit (the
// collaboration service converting a doc as it loads it) tells nobody, while
// what it found is kept.
//
// Run: go test -tags=integration ./business/Doc/ -run TestAnUpgradesFirstSaveTellsNobodyADocAlreadyMentioned -v

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnUpgradesFirstSaveTellsNobodyADocAlreadyMentioned(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	ev := withEmitted(t)
	oldR, oldW := readDocLinks, writeDocLinks
	t.Cleanup(func() { readDocLinks, writeDocLinks = oldR, oldW })
	made := map[string]time.Time{}
	readDocLinks = func(_ context.Context, id string) (*dgraphStruct.DgraphDoc, error) {
		at := made[id]
		return &dgraphStruct.DgraphDoc{Uid: "0x1", Uuid: id, CreatedAt: &at}, nil
	}
	writeDocLinks = func(context.Context, *dgraphStruct.DgraphDoc, []string) (bool, error) { return false, nil }

	chip := func(id string) string { return `<span data-type="mention" data-id="` + id + `@0x9">@someone</span>` }
	var people []string
	for range 4 {
		people = append(people, uuid.NewString())
	}
	editor := uuid.NewString()
	save := func(doc, body string, by ...string) {
		mentionsSaved(ctx, Change{DocUUID: doc, Body: body, Contributors: by, SavedAt: time.Now()})
	}
	told := func() []string {
		var out []string
		for _, e := range ev.events {
			if e.Kind == resourceEventBusiness.DocMentioned {
				out = append(out, e.Subjects...)
			}
		}
		ev.events = nil
		return out
	}

	// A doc made a month before, mentioning three people, saved once after
	// the upgrade.
	existing := uuid.NewString()
	made[existing] = time.Now().Add(-30 * 24 * time.Hour)
	three := "<p>" + chip(people[0]) + " " + chip(people[1]) + " " + chip(people[2]) + "</p>"
	save(existing, three, editor)
	if got := told(); len(got) != 0 {
		t.Fatalf("the upgrade's first save of a doc told %v, who it already mentioned", got)
	}
	save(existing, three+"<p>and "+chip(people[3])+"</p>", editor)
	if got := told(); !slices.Equal(got, []string{people[3]}) {
		t.Fatalf("a fourth mention after the upgrade told %v, want only them", got)
	}

	// A doc made since: its first save tells whom it mentions.
	fresh := uuid.NewString()
	made[fresh] = time.Now()
	save(fresh, "<p>"+chip(people[0])+"</p>", editor)
	if got := told(); !slices.Equal(got, []string{people[0]}) {
		t.Fatalf("a new doc's first save told %v, want %v", got, people[:1])
	}

	// A conversion holds nobody's edit and tells nobody; what it found is kept,
	// so the next edit tells only whom it adds.
	converted := uuid.NewString()
	made[converted] = time.Now()
	save(converted, "<p>"+chip(people[1])+"</p>")
	if got := told(); len(got) != 0 {
		t.Fatalf("a save holding nobody's edit told %v", got)
	}
	save(converted, "<p>"+chip(people[1])+" "+chip(people[2])+"</p>", editor)
	if got := told(); !slices.Equal(got, []string{people[2]}) {
		t.Fatalf("the edit after a conversion told %v, want %v", got, people[2:3])
	}
}
